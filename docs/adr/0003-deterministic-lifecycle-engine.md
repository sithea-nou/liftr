# ADR-0003: Deterministic Lifecycle Engine

- Status: Accepted
- Date: 2026-08-15

## Context

ADR-0001 makes lifecycle policy Liftr's responsibility, and ADR-0002 separates desired state, normalized status, asynchronous Operations, and audit Events. Liftr now needs deterministic create, update, and delete orchestration semantics without introducing execution infrastructure or provisioner concepts.

Lifecycle execution has both a coarse outcome and detailed progress. ResourceState must remain a small normalized vocabulary, while transition legality must not depend on descriptive Conditions or historical Events. Asynchronous completion must also remain safe when desired state changes during an Operation.

## Decision

Liftr implements lifecycle policy as a pure, stateless engine. The caller supplies all current domain values, IDs, and timestamps. Each accepted transition returns an updated Operation, an updated ResourceStatus, and one Event. The engine does not perform I/O or generate nondeterministic values.

OperationState remains Pending, Running, Succeeded, Failed, or Canceled. OperationPhase is the authoritative detailed workflow cursor and initially supports Requested, Validating, Planning, Applying, and Destroying. Conditions report normalized facts but do not determine legal workflow transitions.

Create and update follow Requested to Validating to Planning to Applying before success. Delete follows Requested to Validating to Destroying before success. Operation state, phase, and capability determine legal transitions. Skipped, repeated, backward, capability-incompatible, and post-terminal transitions are rejected.

Only one nonterminal Operation may exist for a Resource. This is a lifecycle policy invariant even though durable enforcement will eventually require transactional persistence.

TargetGeneration is immutable. ResourceStatus.ObservedGeneration is the highest Resource generation whose desired state Liftr has evaluated as part of lifecycle processing. It does not imply successful infrastructure reconciliation. Generation-specific Reconciled and Ready Conditions communicate successful outcomes.

Completion of an Operation targeting generation N never marks a newer generation as reconciled. If the Resource advances while an Operation runs, the Operation and its Events remain associated with N, successful Conditions refer to N, and status indicates that a newer generation remains pending.

If a delete targeting generation N succeeds after desired state advances, Deleted records the successful destruction for N. A subsequent create Operation is legal for the newer generation so the tombstone cannot strand newer intent.

Failure status depends on the capability. Create failure may leave ResourceState Failed. Update failure preserves a previously usable Ready state while setting Reconciled=False and OperationFailed=True. Delete failure does not claim destruction and restores the previously known Ready existence state. The failed Operation and failure Event retain the error reason and message.

ResourceState adds Deleted. Deleted is a stable logical tombstone state after successful destruction, not an execution phase. Persistence, tombstone retention, and eventual physical record removal are deferred.

A retry creates a new Operation with a new identity and audit trail. The failed Operation remains terminal. The retry targets the Resource's current generation, which may differ from the failed Operation's TargetGeneration. Automated retry scheduling and backoff are deferred.

Events remain append-only audit and history records, not an event-sourcing mechanism. The pure domain rejects repeated state transitions without producing another Event. Durable idempotency, unique ID enforcement, command deduplication, and replay of prior results belong to a future application and persistence boundary.

## Consequences

- Lifecycle rules can be exhaustively unit tested without workers, queues, storage, or provisioners.
- Operation phase is the source of workflow truth; Conditions can evolve as normalized reporting without changing transition legality.
- Consumers must compare generation-specific Conditions with Resource.Generation rather than treating ObservedGeneration as proof of reconciliation.
- Ready and reconciliation are independent facts, so a previous generation may remain usable after an update failure.
- Callers must provide the latest Operation when requesting work so the engine can reject concurrency and recognize retries.
- Future persistence must atomically store Operation, ResourceStatus, and Event changes and enforce the one-active-Operation and idempotency invariants.
- Because the engine is stateless, it cannot detect that a caller supplied an old inactive snapshot after another caller accepted an Operation. Preventing that stale-snapshot race requires application or persistence-layer version checks and is intentionally deferred.

## Alternatives Considered

### Use ResourceState for Every Workflow Step

Adding Requested, Validating, Planning, Applying, and Destroying to ResourceState would mix execution progress with normalized resource state. This was rejected.

### Use Condition Reasons as the Workflow Cursor

Condition reasons are useful reporting details but are not an appropriate authority for legal transitions. This was rejected in favor of explicit OperationPhase.

### Mark Every Failure as ResourceState Failed

An update can fail while an older generation remains usable, and a failed delete does not prove that the resource no longer exists. A universal Failed state would discard known normalized facts. This was rejected in favor of capability-specific failure semantics.

### Restart a Failed Operation for Retry

Mutating a terminal Operation would weaken auditability and blur separate attempts. This was rejected; retries are new Operations.

### Provide Durable Idempotency in the Domain Engine

The pure engine has no durable command or Event history and cannot guarantee cross-request deduplication. This was rejected as a domain responsibility and deferred to future application and persistence boundaries.
