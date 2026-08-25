# ADR-0020: Stateful OpenTofu CLI Provisioning

- Status: Accepted
- Date: 2026-08-25
- Refines: ADR-0004 (provider-neutral provisioner contract), ADR-0006
  (PostgreSQL persistence and transactional outbox), ADR-0010 (private
  implementation binding), ADR-0011 (resource outputs), ADR-0014 (explicit
  retry), ADR-0018 (operational diagnostics)

## Context

Liftr needs an imperative provisioner that can execute existing HCL provider
ecosystems without making workspaces, state, plans, providers, backends, or
engine commands part of a Resource contract. Unlike a remote execution
service, an OpenTofu CLI process has no retained service-side execution
history. Its backend state is necessary control data and still an incomplete
record of what a provider may already have done. A process can therefore crash
after a provider side effect but before state or the Liftr submission result is
durable.

M19 defines a stateful CLI adapter while preserving ADR-0004's conservative
ambiguity rule. The design must correlate an immutable Liftr attempt with a
stable state identity, survive crashes without reusing uncertain work, and
leave OpenTofu in charge of state locking. It must also distinguish provider
dependency locking from runtime state locking: they solve different problems.

## Decision

### Public and application contracts remain unchanged

The adapter must implement the existing `Capabilities`, `Submit`, and `Observe`
contract. ResourceSpec, ResourceStatus, ResourceType discovery, Operations,
Events, the HTTP `/v1` API, and the official CLI gain no OpenTofu fields or
methods. Workspaces, HCL, modules, provider addresses, dependency locks, local
state, state lineage, serials, digests, journals, and engine diagnostics remain
private implementation details.

Selection remains creation-time startup composition through an immutable
`ProvisionerRef`. A private registration binds one ResourceTypeRef to trusted
platform source, supported capabilities, input and output mappings, a stable
state-naming version, private scratch/quarantine roots, the engine binary, the
HTTP backend profile, and approved provider packages. Existing Resources
remain on their original registration; M19 adds no provisioner migration.
One strict operator file may retain multiple immutable registrations and has a
separate current ResourceType-to-ref route set. Moving a route affects only new
Resources; deleting an old registration while a Resource remains bound is an
invalid rollout.

### Supported engine is OpenTofu 1.12.6 only

The M19 implementation target is the OpenTofu CLI at exactly `1.12.6`. The
adapter must reject a missing, differently versioned, or non-OpenTofu binary
before provisioning can be submitted, and it must never download an engine at
runtime.

No HashiCorp Terraform CLI version is selected, tested, or supported by this
decision. Terraform compatibility is conditional future work: it may be
claimed only after an exact version is chosen and the full state, locking,
diagnostic, crash-recovery, and provider suite proves equivalent behavior.
Similarity of commands or state syntax is not a compatibility claim.

### One durable state identity per Liftr Resource

One Liftr Resource has one deterministic state key under its immutable
ProvisionerRef. The versioned key derives from stable platform identity,
registration identity, ResourceTypeRef, and ResourceID. OperationID,
generation, capability, attempt number, and worker identity are not inputs, so
create, update, retry, observe, and delete converge on the same state.

Production state is platform control data in an operator-supplied, conformant
HTTPS OpenTofu HTTP backend. OpenTofu owns backend GET, state update, LOCK, and
UNLOCK requests and propagates the lock ID according to the HTTP backend
protocol. Liftr supplies configuration and credentials through the private
registration but does not implement or proxy those state operations. The
local backend exists only for development and tests. An in-process or fixture
HTTP backend used by tests is also not a production backend.

The implemented private PostgreSQL binding freezes ResourceID,
ProvisionerRef, engine, program, backend profile, and the one stable backend
state key for the Resource. Once state exists, the binding monotonically
records OpenTofu lineage, serial, and a 32-byte SHA-256 digest over the exact
state bytes accepted by the adapter; lineage cannot change, serial cannot
regress, and equal serials cannot acquire conflicting digests. Raw state is
never stored in this binding.

The digest is deliberately unkeyed: it detects accidental mismatch and binds
journal evidence to bytes, but is not an HMAC, signature, credential, or proof
against an actor who can write backend state. Backend authentication and
authorization, TLS, encryption, and backup policy provide the storage trust
boundary.

Backend-state absence has narrow meaning:

- A deterministic registration or request rejection before Apply is
  authorized can support a conclusive pre-submission failure; state absence is
  not what proves that result.
- Once Apply may have started, a missing, malformed, wrong-lineage, regressed,
  or digest-mismatched state is `ControlStateUnavailable` and correlation
  remains Unknown. It never proves that provider-managed infrastructure is
  absent.
- Delete completion requires the exact fenced attempt, compatible state
  evidence, and a successful verification plan. The retained private control
  marker means a successful delete state is not expected to be an empty or
  deleted state.

OpenTofu state is not independent readiness or drift evidence. Unless a future
observation design adds provider-backed facts, create/update observations keep
Presence, Readiness, and Drift Unknown. A conclusively correlated successful
delete may report Presence NotFound under the existing delete semantics.

### Fenced private attempt journal

The implemented foundation maintains the journal in private PostgreSQL tables,
not in the work root. It supplements rather than replaces Operations,
executions, submission attempts, Events, and outbox records. An immutable key
binds ResourceID, OperationID, attempt number, and ProvisionerRef; a
compare-and-swap record version and database triggers enforce these only legal
phase paths:

```text
Prepared -> ApplyMayStart -> ApplyExited -> ObservedConverged
                          \-> ApplyOutcomeUnknown -> ObservedConverged
```

`ApplyMayStart` is the durable Apply launch/ambiguity boundary for create,
update, and delete despite its engine-private name. No `tofu apply` process may
have started before that transition is durable. Initialization, creation of a
normal saved plan, and plan inspection occur while the attempt is `Prepared`;
they may read backend state, initialize providers, evaluate data sources, and
make provider reads, but they do not authorize the Apply mutation. The phase
cannot move backward or be deleted. `ObservedConverged` is immutable terminal
journal evidence. Journal and state-binding mutations validate the exact
leased outbox message and token, its unexpired lease using PostgreSQL server
time, the current attempt, and the ProvisionerRef. Record versions fence
concurrent mutation; an older worker cannot overwrite newer evidence. Observe
may advance convergence and state evidence under its own live fence.
PassiveObserve cannot mutate attempt journal or state-binding evidence.

Loss of fencing prevents the worker from publishing a terminal result even if
its child process later exits. An `ApplyMayStart` entry with no admissible
terminal binding is ambiguous and is recovered through Observe, never by
erasing the entry or creating a replacement workspace. Entries and state
bindings never appear in a public response or metric label.

The journal cannot make provider effects atomic with state. It records what
Liftr authorized, what state bytes were bound, and where ambiguity begins; it
must never manufacture success from an exit code, an empty state, or an absent
process when the required binding is missing.

### Provider calls stay outside PostgreSQL transactions

Dispatch follows ADR-0006:

1. Claim the outbox item with a fenced lease and commit.
2. Commit the attempt's Dispatching state, `Prepared` evidence, and stable
   state binding through short fenced transactions.
3. Outside every PostgreSQL transaction, initialize and create and inspect the
   normal saved plan. Only after those steps succeed, commit
   `ApplyMayStart`, then launch `tofu apply` for that plan.
4. Let OpenTofu acquire the HTTP backend lock and perform backend/provider work
   outside every PostgreSQL transaction.
5. Persist `ApplyExited` or `ApplyOutcomeUnknown`, updated state evidence, and
   normalized execution evidence through later fenced transactions. Observe
   alone may subsequently record `ObservedConverged`.

Lease heartbeats use short, independent PostgreSQL transactions. No database
transaction, Resource lock, Operation lock, quota lock, or idempotency lock is
held while waiting for the state lock or while OpenTofu or a provider runs.

Failures before Apply authorization are classified explicitly:

- Deterministic registration, source, input, executable, supply-chain, or plan
  closure rejection returns conclusive NotFound plus terminal Failed.
- An unknown operational initialization/planning/inspection availability or
  timeout failure while still durably `Prepared` returns the provider-neutral
  typed `SubmissionNotAttemptedError`; the worker redelivers the same durable
  attempt with capped backoff. It does not create a new attempt or schedule
  Observe. This classification means only that Apply cannot have started; the
  preceding commands may have performed reads.
- The real 1.12.6 runner recognizes only a closed, tested allowlist of exact
  machine-UI semantic diagnostic summaries as deterministic input/configuration
  rejection. Every other nonzero init/plan outcome remains operationally
  unavailable. Backend and lock diagnostics are never allowlisted and never
  establish safe lock or submission conclusions.
- Once `ApplyMayStart` is durable, cancellation, timeout, crash, lost
  heartbeat, uncertain CLI diagnostics, or state mismatch is Unknown and
  schedules Observe. It cannot become a not-attempted retry.

### OpenTofu owns state locking

The normal plan and saved-plan apply use the engine's normal state locking with
a bounded lock wait. Against the production HTTP profile, OpenTofu performs
GET/update/LOCK/UNLOCK and propagates the backend lock ID; Liftr does not
reimplement the protocol. Liftr never passes `-lock=false`, never calls or
automates `force-unlock`, never deletes a lock artifact, and never treats a
scratch-directory file lock as a substitute for the engine lock. That private
file lock protects only scratch assembly and quarantine.

There is no sacrificial lock probe. In particular, Liftr does not launch an
extra plan, refresh, apply, or another provider-capable command merely to test
whether a lock is available: such a probe can initialize providers, refresh
resources, evaluate data sources, or otherwise make calls. The required normal
plan and its saved-plan apply acquire their own real locks. Liftr does not
infer lock outcomes from free-form diagnostics.

Workers renew fenced outbox leases in short PostgreSQL transactions while
Submit, Observe, and PassiveObserve wait for an engine lock or child process.
A lost heartbeat cancels owned work where possible and discards its result.
Observe then resolves active-attempt correlation from the fenced journal and
state binding. PassiveObserve may refresh normalized facts but cannot complete
an Operation, authorize a resubmission, reinterpret an attempt, or update the
private attempt journal; if it loses its lease, its facts are discarded. A
fresh outbox heartbeat and `ApplyMayStart` evidence may report Found/Running
for the exact attempt. A stale heartbeat proves only loss of activity, not
failure or absence.

### Provider dependency locks are not state locks

Programs using external providers require a platform-owned
`.terraform.lock.hcl` with exact approved selections and checksums. It is
immutable for one registration and used read-only. Provider packages and all
modules are preinstalled from verified build artifacts in an offline
filesystem mirror; initialization cannot fall back to a registry or direct
download. These dependency locks provide package selection and integrity, not
runtime mutual exclusion. The filename is OpenTofu's dependency-lock format,
not a Terraform compatibility claim. Provider API traffic during an admitted
operation is still expected and is governed by the registration's credential
and network policy.

The deterministic credential-free qualification path for the engine boundary
must use OpenTofu's built-in-only resource support. A built-in provider has no
dependency-lock entry, no provider plugin package, and no provider or registry
network call. When actually run against the required real engine and backend,
this path qualifies CLI, state, journal, locking, output, and crash semantics
only; it does not validate an external provider or cloud implementation.

### Private scratch workdirs and quarantine

The adapter implements a private per-call scratch model. The production state
identity is the stable HTTP backend key, not a workspace directory:

- Every call creates a uniquely named, mode-restricted scratch workdir, copies
  and verifies trusted source into it, and writes private generated files.
- A normal conclusively handled call removes its scratch workdir. Workdirs are
  not retained as the primary state location or reused as a workspace.
- A workdir whose outcome is ambiguous, whose handling fails, or which contains
  `errored.tfstate` is atomically moved to a separate, same-filesystem,
  non-executable quarantine. Quarantine is diagnostic evidence, not the state
  backend and not a recovery source.
- Startup scans only adapter-owned orphan names with valid ownership metadata.
  It leaves active locked work alone, quarantines confirmed owned inactive
  orphans, and fails closed on malformed owned entries. It does not inspect,
  adopt, delete, or execute unrelated directories.
- Work, quarantine, source, generated variables, plans, command output, and
  state observations remain private and bounded. Quarantine is retained under
  restrictive permissions until an approved operator action; it is never
  silently restored or garbage-collected by age alone.

An abandoned child may continue holding the OpenTofu HTTP backend lock after a
worker lease expires. On Unix, the implemented command runner cancels the
owned process group with an interrupt followed by bounded forced termination;
regardless, recovery waits for backend lock release and observes the journal.
It does not break the lock. Scratch locks are local safety mechanisms and are
never treated as distributed state locks.

### Commands, retained control state, and outputs

Create, update, and delete all map privately to a normal saved plan followed by
noninteractive `tofu apply`. For delete, the generated private input sets
`desiredPresent=false`; the registered workload addresses must be absent from
the planned closure while the private Liftr control-marker resource remains.
Delete is never whole-state `destroy`, backend DELETE, `state rm`, or garbage
collection. M19 adds no public plan, preview, import, state edit, refresh-only,
replace, target, or arbitrary argument surface. Platform registrations cannot
inject command-line flags that weaken locking, change the backend, or fetch
source.

Successful delete retains backend state, lineage, serial, terminal state
binding, attempt journal, stable state key, and the private control marker.
Retention preserves delete correlation, supports the Resource tombstone, and
prevents a missing state object from being mistaken for proof of absence.
Per-call scratch is still removed after normal success.

Outputs reuse ADR-0011 and ADR-0014 unchanged. A frozen private
OutputMappingRef selects one explicitly registered output envelope for the
exact Resource and generation. The implementation invokes bounded private
all-root-output JSON metadata retrieval so it can enforce the selected root
output's `sensitive=false` metadata. It then immediately discards the unmapped
root-output values and validates only the registered bounded envelope.
Sensitive-marked, unknown, malformed, oversized, wrong-identity, or
wrong-generation values are rejected; output names and values are never
logged. Validated scalar values publish transactionally through
ResourceOutputs. Transient extraction remains observe-only and never reruns
apply. Delete publishes no outputs, and retained state is never exposed
through an API.

### Supply chain and data boundary

Production composition must supply the official OpenTofu `1.12.6` artifact and
pin trusted release checksum/signature evidence. CI installs the pinned release
artifact and verifies its published SHA-256. The implemented adapter requires
the configured executable SHA-256, verifies official build identity and exact
runtime version, and executes a digest-verified private copy so replacement of
the configured pathname after startup cannot change the engine. It also verifies
the registered source digest. External-provider registrations additionally pin module source,
dependency lock content, provider versions, platform/architecture packages,
and package checksums. Runtime engine, module, and provider downloads are
disabled; a missing artifact fails before submission.

Only trusted platform-managed source is supported. User HCL, user modules,
remote modules, Git checkout, arbitrary environment inheritance, arbitrary
CLI flags, and developer-selected providers are excluded. Child environments
are constructed from a reserved base plus an explicit registration allowlist.
Credentials may reach approved external providers only through that private
channel and never enter ResourceSpec, journal fields, logs, Events, failures,
metrics, or API representations.

OpenTofu state and provider diagnostics can contain secret or provider-specific
data even when ResourceSpec does not. The HTTP state backend, private scratch,
quarantine, and backups must therefore be encrypted and access-restricted. Raw
state, plans, stdout, stderr, provider diagnostics, dependency locks, and
journal bodies are not copied into PostgreSQL text fields or telemetry. Errors
crossing the adapter boundary are typed and curated.

## Consequences

- Liftr can add an HCL-based imperative engine without changing its public or
  provider-neutral contracts.
- Stable HTTP backend state and a fenced journal provide durable correlation, but
  cannot eliminate the fundamental provider/state commit gap; some crashes
  remain Unknown until operator intervention.
- OpenTofu remains the state-lock authority. This avoids unsafe lock breaking
  but can intentionally stall work behind a genuinely abandoned process.
- Production HTTP state makes backend durability, confidentiality, protocol
  conformance, locking, capacity, and coordinated backup platform
  responsibilities; private scratch and quarantine have a separate diagnostic
  retention boundary.
- Exact offline supply-chain pinning makes execution reviewable at the cost of
  requiring a new immutable registration for deliberate source, engine, or
  provider changes.
- The built-in-only path can validate control-plane mechanics without
  credentials or network access, but makes no claim about external providers.

## M19 Scope and Implementation Status

M19 is limited to the OpenTofu `1.12.6` CLI, an operator-supplied conformant
HTTPS HTTP state backend for production, trusted platform source,
create/update/delete through the existing provisioner contract, fenced
attempt/state correlation, retained control state and marker, mapped
non-secret outputs, offline dependencies, and a built-in-only deterministic
qualification path. The local backend and any test HTTP backend are
development/test-only. The public API is unchanged.

The `internal/provisioning/opentofu` package, private PostgreSQL evidence store
and migration, Unix process-tree cancellation, per-call scratch handling, and
quarantine implementation exist. Optional production composition loads one
strict operator registration set and leaves Pulumi as the default when it is
absent. Liftr provides no production ResourceType/program/backend registration.
Qualification has run against a real official OpenTofu `1.12.6` binary and the
conformant test HTTP backend. That evidence does not qualify any operator's
production HTTPS backend, external provider, or cloud program; each production
backend and registration still requires separate acceptance evidence.

No production cloud ResourceType or provider composition is approved by this
ADR. Any such binding needs its own private configuration, acceptance evidence,
credential review, and operational ownership.

## Deferred and Conditional Work

- Terraform compatibility is conditional on selecting and proving an exact
  version; no Terraform support exists under this ADR.
- Additional state backend types, state migration, multi-execution-domain
  routing, and cross-region execution are deferred. The conformant HTTPS HTTP
  state backend is the selected production profile and is not deferred.
- Remote execution products, including HCP Terraform/Terraform Enterprise
  execution integration, are deferred; they are distinct from the selected
  HTTP state protocol.
- Import, adoption, manual state repair/push, force unlock, cancellation,
  targeting/replacement flags, and an administrative ambiguity-resolution API
  are deferred; here cancellation means a public Operation cancellation
  contract. Internal Unix process-tree cancellation is implemented. Windows
  adapter support is deferred. Force unlock and lock bypass remain prohibited
  unless a later ADR replaces this decision.
- Provider-backed passive readiness, refresh, drift detection, planning APIs,
  cost estimation, and policy evaluation over plans are deferred.
- Secret-bearing ResourceSpec, secret outputs/references, credential rotation,
  and general secret retrieval remain deferred to their own contracts.
- Automatic quarantine deletion, state compaction, physical Resource
  tombstone deletion, and control-state archival are deferred.

## Risks and Required Operator Procedures

- **Database/backend split outcomes:** pause workers for coordinated backup or
  restore. Coordinate PostgreSQL with the production HTTP state backend as one
  recovery set. Never restore one side casually or rewrite a digest to make it
  match. Preserve and back up quarantine separately as sensitive diagnostic
  evidence; it is not authoritative backend state.
- **State locks:** identify the owning process and worker lease, then wait for
  normal completion or terminate the verified process through ordinary process
  control. Never use `force-unlock`, `-lock=false`, lock-file deletion, or a
  sacrificial plan/refresh probe.
- **Missing or corrupt state:** stop work for the affected registration,
  preserve backend and quarantine evidence, compare read-only journal/state
  bindings and PostgreSQL attempt history, and restore only an approved
  coordinated PostgreSQL/backend backup.
  Never infer cloud absence, run import, push edited state, or resubmit the
  attempt manually.
- **Quarantine:** treat quarantine as potentially sensitive evidence. Do not
  execute it or move it back into the active namespace. Record its identity,
  restrict access, and escalate for an approved recovery or retention action.
- **Capacity pressure:** expand backend or quarantine capacity without deleting
  backend state, journals, or quarantine. Automatic age-based cleanup is not
  safe.
- **Engine/provider rollout:** build and verify artifacts offline, create a new
  immutable registration where identity-affecting configuration changes, keep
  old registrations and binaries available for bound Resources, and exercise
  the built-in-only path before external-provider acceptance.
- **Ambiguous operations:** use Observe and ordinary lease-expiry recovery.
  Do not launch OpenTofu by hand or use the public explicit-retry endpoint to
  bypass an active/Unknown attempt; retry remains legal only under ADR-0014.

## Rejected Alternatives

### Per-Call State Identity

Giving every scratch workdir or attempt a different backend key loses lineage
and makes ambiguous submission indistinguishable from work that never started.
Rejected in favor of disposable per-call scratch around one retained backend
state identity per Resource.

### Provider Call Inside the Admission or Result Transaction

PostgreSQL cannot atomically commit an external provider effect. Holding a
transaction open adds contention without closing the ambiguity window.
Rejected; calls remain between fenced commits.

### Sacrificial Lock Probe or Liftr-Owned State Lock

A provider-capable probe can make calls, while an adapter lock cannot replace
backend locking. Rejected; the actual command acquires the OpenTofu state lock.

### Force Unlock or `-lock=false`

Either can permit concurrent writers and state corruption when the apparent
owner is merely slow or partitioned. Rejected, including as an automated
recovery mechanism.

### Missing or Empty State Means Infrastructure Is Absent

Providers may have completed side effects before state was written, and state
can be lost independently. Rejected; only fenced attempt evidence can support
correlation and ambiguity otherwise remains explicit.

### HMAC for State Digests

A keyed digest would add secret generation, distribution, rotation, and
recovery without protecting against the privileged runtime that must read and
write state. Rejected. SHA-256 is an equality/integrity binding only; storage
security protects the trust boundary.

### Whole-State Destroy or Delete State After Resource Delete

Whole-state destroy would remove the private control marker, while backend
DELETE, `state rm`, or garbage collection would destroy lineage and correlation
precisely when delete recovery needs them. Rejected; delete applies the normal
saved plan with `desiredPresent=false` and retains state and the marker.

### Treat OpenTofu as Drop-In Terraform Support

Command resemblance does not prove equal locking, diagnostics, state, or crash
behavior. Rejected; Terraform remains conditional future work with no selected
or claimed version.
