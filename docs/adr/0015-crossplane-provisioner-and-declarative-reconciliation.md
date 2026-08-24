# ADR-0015: Crossplane Provisioner and Declarative Reconciliation

- Status: Accepted
- Date: 2026-08-24
- Refines: ADR-0004 (provider-neutral provisioner contract), ADR-0006 (PostgreSQL persistence and transactional outbox), ADR-0010 (implementation binding and transition semantics), ADR-0011 (resource outputs and secret references), ADR-0013 (CLI as a public API client), ADR-0014 (operation history and explicit retry)

## Context

ADR-0007 delivered the first real provisioner adapter: Pulumi, an imperative engine whose `Submit` launches an execution that eventually terminates and whose history provides durable correlation. Milestone 14 proves the opposite execution model. Crossplane accepts desired state, persists it as a Kubernetes object, and reconciles it asynchronously through controllers; there is no terminating execution to correlate, only a live desired-state document whose conditions report progress.

The core question was whether Liftr's unchanged Submit/Observe contract can model both engines without leaking Kubernetes semantics — apiVersion, kind, namespace, UID, generation, resourceVersion, conditions — into ResourceSpec, ResourceStatus, discovery, Operations, or the HTTP API.

Three review corrections shape this decision. First, resource presence, Liftr ownership, and request correlation are three distinct dimensions that must never be collapsed into one boolean, because conclusive managed absence is already a resubmission and deletion-success signal elsewhere in Liftr. Second, GET-then-act identity races are unacceptable: a verified object must never be mutated or deleted by name after being replaced under the same name. Third, readiness requires two freshness dimensions at once: Crossplane's own condition freshness and Liftr's target-generation correlation.

## Decisions

### The Provisioner contract is unchanged

`Provisioner` keeps exactly `Capabilities`, `Submit`, and `Observe`. No Plan, Apply, Destroy, Reconcile, or Kubernetes-specific method exists. Compile-time interface conformance is pinned. The contract already anticipated declarative backends: `ObservationRequest.Handle` is optional for backends that observe by resource identity, `ExecutionStateAccepted` distinguishes persisted desired state from convergence, and the passive observation path issues handle-less requests.

### Integration model: platform-owned composite resources

Liftr creates platform-owned Crossplane composite resources (XRs), never provider ManagedResources directly and never Claims:

```text
ResourceType -> private XR binding -> Crossplane Composition -> provider ManagedResources
```

This is the structural analogue of ADR-0010's program registration and preserves the same principle: developer contract ≠ implementation technology. The adapter understands one platform CRD contract per binding, not every cloud provider CRD. Claims were rejected because Liftr itself is the claim layer; direct ManagedResources would couple Liftr to each provider schema and forfeit composition; a generic arbitrary-object adapter has no input-contract enforcement and an unbounded authorization surface.

### Stable logical identity versus physical UID

One XR exists per Liftr Resource. The logical object name digests platform identity, XR namespace, ResourceTypeRef, and ResourceID — never OperationID, attempt number, or generation — so create, update, retry, observe, and delete address the same object across restarts. The Kubernetes UID is runtime physical identity, not naming identity: the opaque `ExecutionHandle` may retain the UID once an observation confirms it, and a UID mismatch during an execution is an identity conflict (`TargetIdentityChanged`) — never a silent adoption. Handles remain opaque; only the adapter decodes them.

### Presence, ownership, and request correlation are separate dimensions

Private metadata on every owned XR separates the concerns. Ownership labels/annotations (platform digest, resource ID, resource type) are stable across updates and retries. Execution annotations (OperationID, target Liftr generation) change with each admitted Operation. No user-editable ResourceSpec field participates. Labels carry only label-legal digest values; full opaque identifiers ride in annotations.

### Object absence requires positive served-GVR evidence

A Kubernetes 404 on a managed object is ambiguous by itself: it can mean the deterministic object is genuinely absent, or that the API kind is no longer served because its CRD was removed. Because managed absence drives resubmission authorization and deletion completion in Liftr, `Presence=NotFound` — and every conclusive-absence outcome built on it, including the delete-preflight proof — requires two facts established together:

1. structured API discovery currently answers that the target group/version/resource is served; and
2. the exact deterministic object is absent.

Discovery uses the standard group-version resource list and is interpreted exclusively through structured payloads and status codes: a listed resource confirms serving; a missing group-version endpoint or an omitted entry refutes it; transport loss, server faults, throttling, authorization denials, and malformed payloads all collapse to uncertainty that fails closed as `Unknown` or curated unavailability (`TargetKindUnregistered` when refuted, `ControlPlaneUnavailable` when uncertain). Raw discovery and error text never crosses into Liftr. Discovery answers only "is this resource API currently served": it contributes no ownership or correlation context, needs no elevated RBAC beyond ordinary discovery reads, and ownership verification remains a separate object-metadata check.

Liftr deliberately keeps no discovery cache. Every absence-concluding 404 verifies served-ness live in the same decision cycle, so the freshness bound for an absence conclusion is zero: an API kind removed after earlier successful traffic cannot be misread as a missing object, and discovery outages can never be promoted to absence. Successful GETs need no discovery request at all.

Consequently: an accepted DELETE whose API kind then disappears cannot complete destruction on the object's 404 — the operation stays alive with a curated failure until an operator restores the kind or intervenes; an ambiguous create or update followed by a kind-level 404 stays unresolved and authorizes no resubmission; and update preflight against a vanished target distinguishes conclusively-absent objects (genuine `ManagedTargetAbsent`) from unserved kinds (curated control-plane failure).

Normalized reporting follows strictly:

| Physical XR | Ownership | Request correlation | Report |
|---|---|---|---|
| absent | — | — | Presence=NotFound; execution observations report `RequestCorrelation NotFound` with no Execution |
| present | ours, current Operation | correlated | normal execution evaluation |
| present | ours, older Operation | uncorrelated | **Presence=Present**, correlation NotFound, no Execution — existing safe resubmission rules may act |
| present | foreign | none | **Presence=Present** + Failed with curated `TargetIdentityConflict`; never adopted, never deleted, never resubmitted onto |

Conclusive managed absence exists only when the deterministic target is genuinely absent. A stale operation annotation, a lost write, or a foreign collision is never encoded as absence.

### Identity-safe mutation and UID-preconditioned deletion

Every mutation is conditioned on the exact physical version Liftr just verified:

- Create posts the manifest by deterministic name; `AlreadyExists` triggers re-read and re-evaluation, and only an owned object may be reasserted onto.
- Update performs a conditional full-object write carrying the verified `metadata.resourceVersion`. The precondition converts the write into an atomic optimistic-concurrency compare-and-swap: a replacement object under the same name can never be overwritten or adopted because its resource version cannot match. The desired spec is replaced wholesale — Liftr always intends complete desired state — while the preceding GET-modify-write cycle naturally preserves server-owned fields such as finalizers installed by webhooks. On conflict the adapter re-reads, re-verifies ownership and UID, and retries from fresh evidence within a small bound; persistent churn fails closed rather than forcing a write.
- Delete sends `DeleteOptions` with a UID precondition. A precondition conflict is never retried blindly by name: the adapter re-reads and re-evaluates ownership, failing closed on any replacement regardless of its metadata.

Kubernetes DELETE acceptance yields Accepted/Running evidence, never Succeeded. Deletion completes exclusively when Observe proves genuine absence. Objects with a deletionTimestamp are Present and NotReady while finalizers run; they are never Deleted. Finalizer force-removal is out of scope.

### Asynchronous submission semantics

Submit validates, encodes the spec through the private binding encoder, and persists desired state. A confirmed persistence returns Found + Accepted with Present facts — never Succeeded, because stored intent is not reconciliation. Write transport loss returns Unknown with the ambiguous-submission error so lease recovery moves the attempt to Unknown and schedules Observe. Observe then resolves the ambiguity through deterministic identity: an object carrying this Operation's correlation confirms acceptance without resubmission; conclusive absence reports plain NotFound with no Execution, which is the only signal authorizing another attempt under the unchanged ADR-0004/0006 rule. Unlike Pulumi, Kubernetes makes post-transmission-loss absence genuinely conclusive here because submission is a single atomic desired-state write with no side effects before persistence; the contract-level rule is unchanged, and no Kubernetes-specific retry shortcut exists.

Update on a genuinely absent target fails conclusively as preflight rejection: external deletion of the XR is operator territory, reachable again through explicit M13 retry semantics.

M13 retry creates a new OperationID against the same logical identity; the first observation of the child finds the previous Operation's annotation and follows exactly the uncorrelated-request path above, reasserting the same XR. Retry therefore never duplicates infrastructure. Output-recovery children are observe-only by construction and issue zero mutating calls.

### Condition mapping and dual freshness

Readiness derives only from structurally registered condition bindings; the default platform XR rule requires `Ready=True`, `Synced=True`, and both conditions' `observedGeneration` equal to `metadata.generation`. Missing freshness fields count as unfresh. Independently, the stamped Liftr target generation must equal the requesting generation before anything may report Ready — for execution observation and passive observation alike. Stale-generation health evidence therefore reads Unknown and never advances readiness. Active progress on the current generation reads NotReady; termination reads NotReady. Terminal reconciliation failures come only from a private, curated allowlist of structural condition reasons mapped to curated failure kinds; raw condition messages, controller text, and provider identifiers never cross into failures, events, logs, or status.

Drift is always Unknown in M14. `Synced=True` is Crossplane's belief about past reconciliation, not independent provider truth, and Liftr will not manufacture dashboard evidence.

### Outputs reuse M10/M11/M13 unchanged

Bindings declare immutable output mappings reading exactly one registered status path holding a strictly bounded envelope that echoes mapping identity, resource ID, and target generation; values are flat non-secret scalars validated by the developer contract before publication. The frozen `OutputMappingRef` machinery, defer/publish/reject resolution, durable provenance, and explicit compatibility-declared repair mappings are reused verbatim. Delete executions never carry outputs. Arbitrary XR status is never exposed. No new durable manifest-mapping identity was introduced: input encoding follows the accepted ADR-0010 precedent where deliberate implementation upgrades apply between attempts, while output decoding retains frozen-provenance durability because that is where silent reinterpretation would corrupt persisted meaning.

### Selection, deployment, and credentials

Selection stays creation-time-only with an immutable ProvisionerRef; there is no migration and no public selector vocabulary. The neutrality proof is two deployment compositions over one identical `PostgreSQLDatabase/v2` contract: Deployment A defaults to Pulumi, Deployment B to Crossplane, with zero public difference. One control-plane cluster serves one provisioner registration; multi-cluster routing is deferred. Credentials use standard kubeconfig files (a supported subset without exec/auth-provider plugins) or in-cluster service accounts, stay in adapter configuration, and never appear in API payloads, logs, or persistence.

### Polling, not watches

M14 observes through scheduled polling consistent with the existing worker; watches and informers are deferred optimizations. Passive observation remains enabled and now produces genuine normalized facts — ownership and Liftr-generation verified, OperationID skipped, Drift Unknown — which is the first time ongoing presence/readiness evidence reflects backend truth rather than permanent Unknown.

## Consequences

- Liftr demonstrably drives a fundamentally different execution model through an unchanged contract, with compile-time interface conformance and import boundaries pinning both directions.
- Developers gain nothing Kubernetes-shaped: no GVK, namespace, composition, ProviderConfig, annotation, or UID appears anywhere public, enforced by JSON-scan tests and import-boundary tests.
- Replacement-object attacks fail closed on every mutation path; UID preconditions and resourceVersion preconditions remove the accepted GET-then-act risk from the design review.
- CRD removal can no longer masquerade as managed absence: deletion completion, resubmission authorization, and conclusive-absence proofs all demand live served-GVR discovery evidence, so a vanished API kind surfaces as a control-plane failure instead of a false Deleted or false resubmission.
- Stale-generation and stale-condition evidence can never mark a resource Ready, including through passive observation.
- Permanently nonterminal reconciliation loops through bounded-delay observation until an operator intervenes; Liftr still owns no timeout policy.
- The adapter subtree contains all Kubernetes knowledge behind a thin REST boundary, keeping the dependency footprint unchanged and every test deterministic.

## Scope Exclusions

Terraform/OpenTofu, GitOps adapters, multi-cluster routing, provisioner migration, direct ManagedResource APIs, provider-specific public schemas, watches/informers, finalizer force-removal, Crossplane or ProviderConfig installation management, cloud credential APIs, secret resolution, cancellation, and automatic lifecycle timeouts are excluded from M14.

## Alternatives Considered

### Server-Side Apply for Mutations

SSA was evaluated first because its field-manager model mirrors Liftr's declarative intent. Verification against a real API server rejected it for this adapter: objects created through POST are owned under an Update-operation manager entry, while applies own fields under a distinct Apply-operation entry, so every post-create apply by the same platform conflicts with its own creation history and can only win by `force=true` — which would also override genuinely foreign managers and violate the fail-closed rule. Creation through apply itself was refused by the API server (415), and apply-to-absent creation would silently merge onto any object that races into the deterministic name between preflight and write, weakening Correction-2's zero-adoption guarantee. Conditional full-object updates deliver identical compare-and-swap identity safety without any ownership machinery.

### Direct ManagedResources or Claims

Would couple Liftr to provider CRDs or add a self-service indirection Liftr does not need. Rejected in favor of platform-owned XRs.

### Collapsing Presence With Correlation

Reporting NotFound for stale-operation or foreign objects would have authorized destructive resubmission and fabricated deletion success. Rejected explicitly by design review.

### Unconditional Writes By Name

GET-then-apply and GET-then-delete leave replacement-object races open. Rejected in favor of resourceVersion-preconditioned applies and UID-preconditioned deletes with mandatory re-evaluation on conflict.

### Single Freshness Signal

Accepting `Ready=True` alone, trusting `status.observedGeneration`, or skipping Liftr generation checks under passive observation would each let stale evidence mark current generations Ready. Rejected; dual structural freshness is required everywhere.

### New Manifest-Mapping Identities

Two additional versioned refs would proliferate provenance without solving a recovery problem the frozen output mapping does not already solve. Rejected; encoding upgrades follow ADR-0010 precedent.

### Watches And Informers

Lower-latency convergence detection at the cost of new machinery and a semantic departure from the Pulumi path. Deferred.
