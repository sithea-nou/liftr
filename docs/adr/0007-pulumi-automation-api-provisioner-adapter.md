# ADR-0007: Pulumi Automation API Provisioner Adapter

- Status: Accepted
- Date: 2026-08-16

## Context

ADRs 0004 through 0006 define a provisioner-neutral Submit/Observe contract, stable private provisioner bindings, immutable submission attempts, and PostgreSQL-backed outbox recovery. Liftr now needs a first real provisioner adapter without exposing Pulumi concepts through ResourceSpec, ResourceStatus, lifecycle policy, or the public Resource contract.

Pulumi's Automation API embeds orchestration in the Liftr worker but still invokes the Pulumi CLI. A synchronous CLI invocation can outlive an outbox lease, and a process or persistence failure after invocation can make the result ambiguous. The adapter therefore must preserve Liftr's Found, NotFound, and Unknown correlation semantics rather than treating a missing response as permission to invoke Pulumi again.

Pulumi also exposes raw configuration, state, outputs, command diagnostics, environment values, and backend-specific resource details. The first adapter must use only the minimum data needed to execute and correlate lifecycle intent because Liftr does not yet have secret-aware ResourceSpec handling or provider-specific observation policy.

## Decision

Liftr adds a Pulumi provisioner adapter using the Go Automation API and `LocalWorkspace`. Pulumi remains an implementation detail behind the provider-neutral provisioning contract.

### Versions and Backend

The adapter pins both components to the same release:

- Go module `github.com/pulumi/pulumi/sdk/v3` at `v3.257.0`.
- Pulumi CLI at `3.257.0`.

The version was verified against the official [Pulumi GitHub release for v3.257.0](https://github.com/pulumi/pulumi/releases/tag/v3.257.0) and the [Go module proxy record](https://proxy.golang.org/github.com/pulumi/pulumi/sdk/v3/@v/v3.257.0.info). Both identify Pulumi commit `5a7ae5c7b7970a44cc1f9eeba314cbdf11bc03a5`. The adapter checks the CLI version and fails preflight on a missing or different binary; it never downloads a CLI at runtime.

The SDK module requires Go 1.25.11, so Liftr pins that Go language version as part of the compatibility change. CI downloads the matching official CLI archive before tests, verifies its published SHA-256 digest, and then runs the adapter with that preinstalled binary. Application runtime code has no CLI download path.

Adapter v0.1 supports only a platform-owned `file://` backend. It does not log in to, depend on, or add tags in Pulumi Cloud. The Pulumi v3.257.0 `PULUMI_DIY_BACKEND_IGNORE_DEPRECATION_ERROR` compatibility opt-in and legacy-warning suppression are confined to the constructed child-process environment; they do not select the legacy layout. Backend storage is durable platform infrastructure and is not developer-configurable.

Runtime images contain the pinned CLI, Go dependencies, and every permitted plugin in advance. Runtime module downloads, toolchain downloads, and automatic plugin acquisition or installation are disabled. Missing dependencies or plugins fail preflight rather than causing network access.

### Private Configuration and Trusted Source

Pulumi adapter registrations are private, immutable configuration keyed by `ProvisionerRef`. A registration contains the trusted source identity and digest, Pulumi project identity, backend root, deterministic stack-naming version, supported ResourceType and Capability pairs, and runtime requirements. Changing any of these values requires a new `ProvisionerRef`; existing Resources remain resolvable through their original binding. Neither the reference nor its configuration enters ResourceSpec or a developer-facing Resource representation.

Only trusted, platform-managed local Go Pulumi source is supported. Each adapter call copies the registered source into a fresh isolated working directory and uses that copy with `LocalWorkspace`; it does not execute from the registered source directory. The copy is verified against the immutable registration and removed after the call. An advisory lock protects every live workspace from startup cleanup; abandoned unlocked workspaces are removed only after the configured age. User-provided source, shared mutable work directories, inline programs, and source fetched at runtime are not supported.

ResourceSpec has no secret annotations or secret-safe traversal contract. Every v0.1 registration therefore declares secret-bearing inputs unsupported. A ResourceType that requires secret-bearing input cannot be registered with this adapter. The adapter does not infer that an arbitrary value is safe, does not mark Pulumi configuration as secret, and does not claim to protect secrets accidentally placed in ResourceSpec. Secret-aware ResourceSpec and secret-store design require a later decision.

### Stack Identity and Retention

Each Liftr Resource has exactly one deterministic Pulumi stack under its immutable `ProvisionerRef`. A private, versioned naming function derives the project and collision-resistant stack name from the registration and ResourceID. OperationID, generation, capability, and attempt number are not part of stack identity. Create, update, retry, observation, and delete therefore address the same stack.

Stacks and their update history are retained. Successful delete invokes `Destroy` but does not remove the stack. Liftr does not call `RemoveStack` or otherwise prune the history needed for correlation. Retention also permits a later create for a newer Resource generation to reuse the deterministic stack after the Deleted tombstone semantics defined by ADR-0003.

Pulumi update history uses one canonical, non-secret message for each Liftr submission attempt:

```text
liftr/v1/<base64url-without-padding(UTF-8 OperationID)>/<base-10 AttemptNumber>
```

AttemptNumber is greater than zero and has no leading zeroes. `Up`, `Destroy`, and observation use the same encoder, and observation correlates only an exact message match. The message distinguishes immutable provisioning attempts under one OperationID without creating a stack per attempt.

### Provider-Neutral Contract Refinement

The provisioning contract gains a positive, immutable `AttemptNumber` on execution and observation requests. Observation requests also carry the domain `Capability`. These are provider-neutral concepts required by any adapter that must distinguish immutable submission attempts and interpret the action being observed; they do not expose Pulumi operations. An Operation retry remains a new OperationID as required by ADR-0003, while a resubmission after conclusive NotFound remains a new attempt under the same OperationID as required by ADR-0006.

Create and update capabilities map internally to synchronous Automation API `Up`; delete maps to synchronous `Destroy`. The Capability on observation identifies the expected history operation. Unsupported capabilities fail before CLI invocation. The public Provisioner interface remains Submit/Observe and does not gain Up, Destroy, or Pulumi types.

Request correlation is strict:

| Evidence | Correlation and execution |
| --- | --- |
| Deterministic validation or unsupported-input failure before Pulumi can be invoked | `NotFound` with terminal `Failed` |
| Exact canonical history message with a conclusive success | `Found` with `Succeeded` |
| Exact canonical history message with a conclusive failure | `Found` with `Failed` |
| Exact canonical history message for work that is still active | `Found` with `Running` |
| Pulumi may have been invoked, but neither the call result nor exact history proves an outcome | `Unknown`, with no execution or an `Unknown` execution |

The preflight `NotFound` plus `Failed` combination means the request was conclusively rejected before backend invocation; the Failed execution represents that terminal submission-attempt outcome, not a Pulumi update. This is the only adapter-produced terminal failure with NotFound. Once invocation may have occurred, an absent stack, absent history entry, timeout, canceled process, transport error, or unclassifiable CLI result is Unknown. Absence after possible invocation is never converted to NotFound, even though this may require observation or operator intervention indefinitely. A nil execution does not imply NotFound.

This refines ADR-0004 without weakening its retry rule: only conclusive NotFound can authorize another immutable attempt. Found prevents resubmission, and Unknown requires observation. Raw Pulumi errors are normalized into provider-neutral failures; Pulumi-specific error values and diagnostics do not cross the adapter boundary.

### Dispatch, Leases, and Recovery

The worker performs a submission in this order:

1. Claim the existing Dispatch outbox message with a fenced lease.
2. Commit the provisioning attempt transition from Pending to Dispatching.
3. Invoke synchronous `Up` or `Destroy` outside any PostgreSQL transaction.
4. Commit the normalized result only if the worker still owns the lease and fencing token.

While the synchronous call runs, the worker renews its lease in short, independent PostgreSQL transactions. Claim, renewal, expiry, and result fencing use PostgreSQL server time as required by ADR-0006; host time is not lease authority. No database transaction is held open around Pulumi, and the adapter itself does not access PostgreSQL.

Recovery depends on the committed attempt state. If a Dispatch lease expires while the attempt is still Pending, Pulumi was not authorized to run, so recovery makes the same Dispatch message available again with the same OperationID, attempt number, and dedupe identity. If the lease expires after Dispatching was committed, Pulumi may have run; recovery changes the attempt to Unknown and enqueues Observe. It does not invoke Submit again.

There is no blind submission retry in the worker or adapter. Redelivery checks persisted state, and an ambiguous `Up` or `Destroy` is observed through the deterministic stack and canonical history message. A new submission attempt can be created only under the ADR-0006 conclusive-NotFound rule.

### Observation and Data Boundary

Observation reads retained stack update history. It does not run `Refresh`, preview, import, export, or another modifying command. `Up` and `Destroy` explicitly run without Refresh. A successful Destroy does not remove the stack.

Adapter v0.1 performs no provider-specific interpretation. Every response reports the generic resource facts as:

```text
Presence  = Unknown
Readiness = Unknown
Drift     = Unknown
```

Pulumi success or failure is execution evidence, not proof of readiness, presence, or drift. Liftr lifecycle policy continues to interpret the normalized execution outcome.

The adapter invokes the pinned CLI through Automation API's `PulumiCommand` boundary rather than the high-level `Stack.Up` helper, because that helper retrieves plaintext stack outputs after a successful update. The adapter does not run an output command or read or return stack outputs. Except for Pulumi's required state and history in the platform-owned file backend, Liftr does not persist raw Pulumi outputs, rendered configuration, state, environment, CLI stdout or stderr, Automation API results, or workspace files. These values are not logged, placed in Events or failure messages, returned through the provisioning contract, or copied into ResourceStatus. The child process receives a constructed allowlist rather than inheriting Liftr's ambient process environment. Ephemeral process environment and workspace configuration are used only for the call and deleted with the isolated workspace. The private lossless ResourceSpec intent snapshot required by ADR-0006 remains unchanged and is not a Pulumi artifact.

## Consequences

- Liftr gains a concrete Pulumi execution path while its public and core contracts remain provisioner-neutral.
- One retained stack per Resource provides deterministic convergence and durable attempt correlation without stack proliferation.
- Exact version pinning, trusted copied source, offline execution, and an isolated workspace make the runtime boundary reviewable and reproducible.
- Synchronous Pulumi calls remain safe with long-running work because PostgreSQL leases are renewed and fenced without holding a transaction open.
- Ambiguity favors safety over automatic progress: missing evidence after possible invocation remains Unknown and cannot trigger duplicate infrastructure work.
- The file backend is operationally simple and avoids a Pulumi Cloud dependency, but the platform must operate and protect its durable files and retained history.
- Returning only execution evidence and Unknown generic facts limits current status detail but prevents provider-specific state and potentially sensitive artifacts from leaking into Liftr.
- ResourceTypes requiring secrets cannot use adapter v0.1.

## Scope Exclusions

Adapter v0.1 does not add Git checkout or GitOps workflows, `RemoteWorkspace`, Pulumi Deployments, Pulumi Cloud backends or Cloud-specific features, stack tags, provider or cloud resource types, runtime provider installation, secret-store or encryption design, secret-aware ResourceSpec support, readiness evaluation, drift detection, or provider-specific resource-presence inference. It also does not add Refresh, raw Pulumi artifact APIs, stack removal, cancellation, or automated resolution of indefinitely Unknown attempts.

## Alternatives Considered

### Pulumi Cloud or Pulumi Deployments

These services provide remote state and execution features but would make the first adapter depend on Pulumi Cloud-specific authentication and concepts. A local `file://` backend and `LocalWorkspace` were selected for v0.1.

### Remote or Git-Backed Source

Fetching source adds credentials, mutable references, network failure modes, and source-trust policy. Trusted local source copied into an isolated workspace was selected; Git and `RemoteWorkspace` are deferred.

### One Stack per Operation or Attempt

Per-operation stacks would fragment the desired state of one Resource, complicate update and delete, and make retries create parallel infrastructure identities. One deterministic retained stack per Resource was selected.

### Remove the Stack After Destroy

Removing a stack would discard the history used to prove whether a delete attempt ran and would break deterministic reuse for newer intent. Destroy without stack removal was selected.

### Refresh During Observation

Refresh is a provider operation that can mutate Pulumi state and expose provider-specific resource data. History-only observation was selected until readiness, drift, and sensitive-state handling have explicit designs.

### Retry an Expired Dispatch

Repeating Submit after Dispatching may duplicate effects because lease expiry does not prove that Pulumi was not invoked. Expired Dispatching work becomes Unknown and is observed; only an expired lease that left the attempt Pending can requeue the same Dispatch.

### Expose Pulumi Outputs as Resource Facts

Outputs do not have universal presence, readiness, or drift semantics and may contain secrets. They are discarded rather than returned or persisted by Liftr.
