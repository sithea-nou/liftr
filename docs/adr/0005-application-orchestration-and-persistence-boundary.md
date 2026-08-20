# ADR-0005: Application Orchestration and Persistence Boundary

- Status: Accepted
- Date: 2026-08-16

## Context

Liftr now has separate domain, lifecycle, and provider-neutral provisioning contracts. A coordinating layer is required to load domain snapshots, select stable provisioner bindings, invoke lifecycle transitions, submit and observe provisioning work, and persist the resulting state without becoming a second domain model.

The application layer must also prepare for durable concurrency, idempotency, execution-handle recovery, and asynchronous provider work. Those guarantees cannot be implemented by a stateless application service alone.

## Decision

Milestone 4 uses one `internal/application` package for application use cases and orchestration. The package depends on `domain`, `lifecycle`, and `provisioning`. Domain, lifecycle, and provisioning packages do not depend on application.

The application owns these coordination responsibilities:

- Create, update, delete, retry, advance, and observe use cases.
- ResourceType lookup and provisioner selection for new Resources.
- Stable private ProvisionerRef binding on Resource records.
- ProvisionerRef-based resolution for existing Resources.
- Submission and observation coordination.
- Association of OperationID, ProvisionerRef, ExecutionHandle, submission state, and observations.
- Application error categorization and repository coordination.

Provisioner selection is performed only for a new Resource. Existing Resource records use their stored ProvisionerRef for update, delete, and observation. Changing platform defaults cannot implicitly move an existing Resource between providers. Migration is a future explicit workflow.

Application idempotency replays the same logical Resource and Operation identities. It does not promise an immutable copy of the original response: replay may return the current state of those records. Create replay checks idempotency before ResourceType lookup or provisioner selection, so changed or unavailable defaults do not affect an existing logical request.

ObservedFacts are Liftr-owned normalized domain facts containing Presence, Readiness, and Drift. Provisioners translate backend-specific state into these facts. Lifecycle interprets them and owns ResourceStatus and Condition policy. `lifecycle.Engine.ApplyObservation` supports passive observation without an active Operation and does not create a synthetic Operation or imply Operation success from readiness alone. Passive facts update conditions but do not replace stable Deleted or Failed lifecycle states.

Application tests use the real deterministic lifecycle Engine. Repositories, transaction runners, provisioner selectors, provisioner resolvers, and provisioners are fakes.

Repository interfaces are application ports. A future persistence implementation must atomically coordinate Resource/Status, Operation, Event, idempotency records, and provisioning execution records. Provider calls occur outside database transactions. A future outbox or equivalent dispatcher is expected for reliable asynchronous submission.

Each provisioning execution record retains the private submitted intent snapshot, stable ProvisionerRef, confirmed-acceptance evidence, and latest effective observation timestamp. Dispatch atomically claims a Pending attempt as Dispatching before calling the provider; ordinary dispatch and observation do not observe or resubmit a Dispatching attempt. Milestone 4 does not expose recovery for Dispatching claims because deciding that a claim owner is dead requires Milestone 5 durable ownership or lease semantics. Pending and Unknown attempts have explicit paths: Dispatch handles Pending, while Recover observes Unknown using a caller-supplied receipt timestamp before any future same-OperationID resubmission can be considered. If a fresh, monotonic observation confirms that no execution exists, the attempt may return to Pending and later be submitted with the same OperationID; it never creates a replacement lifecycle Operation. Acceptance learned from Submit or Observe prevents a later nil execution observation from authorizing resubmission. Generic timeouts and transport failures remain Unknown because they do not prove non-acceptance; only conclusive InvalidRequest or Unsupported outcomes may fail immediately without observation.

Provisioner `ObservedAt` is an optional backend source timestamp. A nonzero value is authoritative and is rejected if stale. When a backend has no source timestamp, the application uses the caller-supplied observation receipt time; synchronous submission falls back to the current lifecycle cursor supplied by the application.

Provider evidence must advance the persisted observation timeline. The latest effective observation timestamp is monotonic: evidence whose timestamp does not strictly advance the persisted `LastObservedAt` is stale. Stale submission evidence is never applied — it cannot confirm acceptance, reject an attempt, or transition an Operation — and is recorded as an ambiguous `StaleSubmissionEvidence` outcome so the observe path can recover the truth from the backend. Stale observation evidence likewise never transitions an Operation; the durable worker settles the observation message and schedules a follow-up observation with a bounded retry delay so an active execution is never stranded. Timestamps derived from application receipt time (fallback timestamps) never trigger staleness because they do not claim a backend evidence time.

Observed facts are validated before interpretation. Malformed facts on terminal evidence are sanitized away so a real terminal outcome is never stranded by corrupted condition data. Malformed facts on nonterminal submission evidence produce an ambiguous `MalformedObservedFacts` outcome, and on nonterminal observation evidence a retryable error; neither quarantines the Operation nor poisons the worker message.

Internal phase and terminal Events use a reserved, deterministic hash-based ID namespace derived from OperationID and transition. Caller-provided request Event IDs cannot use that namespace, preventing request IDs from colliding with application-generated audit Events. Every component that drives a lifecycle transition renders the transition label with the same canonical function, so eager and durable execution produce identical Event IDs for identical transitions.

## Consequences

- Lifecycle policy is not duplicated in application services.
- Existing Resources remain bound to their original ProvisionerRef even when platform defaults change.
- Crash recovery can resolve the adapter that owns an opaque ExecutionHandle.
- Passive observations can update normalized status without inventing lifecycle history.
- Milestone 4 defines persistence ports but does not implement storage, transactions, outbox delivery, workers, or queues.
- Milestone 4's in-memory fake demonstrates orchestration semantics but does not provide durable crash recovery. At-least-once dispatch, process-crash recovery, and durable claim ownership require Milestone 5 persistence and outbox work.
- Stateless application coordination cannot prevent races from stale callers; durable optimistic concurrency or transactional uniqueness is required later.
- Provider submission and observation failures remain outside domain error types until application mapping invokes lifecycle policy.

## Alternatives Considered

### Resolve Provisioner on Every Operation

Recomputing provider selection from ResourceType and Capability would allow platform default changes to move existing Resources between provisioning technologies. This was rejected.

### Put ProvisionerRef on domain.Resource

ProvisionerRef is private platform implementation metadata, not developer intent. Adding it to the domain Resource would expose implementation concerns in the core contract. This was rejected in favor of an application/persistence ResourceRecord.

### Let Application Mutate ResourceStatus Directly

This would duplicate lifecycle policy and create inconsistent failure, generation, and operation semantics. This was rejected; application delegates lifecycle transitions to `internal/lifecycle`.

### Call Providers Inside Persistence Transactions

Provider calls are slow, external, and may remain ambiguous after a timeout. Holding a transaction during a provider call cannot provide atomic cross-system behavior. This was rejected.

### Implement Outbox and Workers in Milestone 4

Reliable dispatch requires persistence and execution infrastructure that are outside the approved milestone. The ports and recovery states are defined now; implementation is deferred.
