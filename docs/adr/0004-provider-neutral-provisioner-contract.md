# ADR-0004: Provider-Neutral Provisioner Contract

- Status: Accepted
- Date: 2026-08-16

## Context

Liftr's domain model and lifecycle engine are intentionally independent from infrastructure implementation systems. Liftr must eventually integrate with imperative systems, declarative controllers, GitOps workflows, native cloud APIs, on-premises infrastructure, and custom provisioning systems.

These systems do not share a universal execution workflow. Some expose planning and applying, while others accept desired state and converge asynchronously. A contract based on provider-native methods would either exclude valid implementations or leak implementation concepts into Liftr.

The contract also needs to represent a ready existing resource when there is no current backend execution. An absent execution is different from an execution whose state cannot be determined, and neither fact says whether the backend recognizes a particular submission request. Presence, readiness, and drift are Liftr-owned normalized facts rather than provider-owned resource status.

## Decision

Liftr defines a provider-neutral provisioning contract with three responsibilities:

- Advertise supported ResourceType and domain Capability combinations.
- Submit provider-neutral lifecycle intent.
- Observe backend execution and resource facts.

The contract does not expose Plan, Preview, Apply, Destroy, Reconcile, or Cancel methods. `Submit` means that a provisioner receives lifecycle intent; it does not imply synchronous completion. `Observe` is the common convergence mechanism for synchronous, asynchronous, declarative, and GitOps implementations.

The minimum interface is conceptually:

```text
Capabilities() []ProvisionerCapability
Submit(ExecutionRequest) (Submission, error)
Observe(ObservationRequest) (ExecutionObservation, error)
```

ExecutionRequest contains only the Resource identity, ResourceTypeRef, opaque ResourceSpec, domain Capability, TargetGeneration, and OperationID used for correlation and submission idempotency. A provisioner does not receive or control the complete Liftr Operation, OperationState, OperationPhase, ResourceStatus, or Event.

ExecutionObservation contains RequestCorrelation, an optional Execution object, and domain-owned ObservedFacts exposed through the contract as ResourceObservation. RequestCorrelation has three explicit values:

- Found means the backend conclusively recognizes the request identified by the OperationID.
- NotFound means the backend conclusively does not recognize that request.
- Unknown means the backend cannot determine whether it recognizes the request.

RequestCorrelation is independent from Execution. A nil Execution means no current execution is visible; it does not mean that the request was NotFound. A non-nil Execution with state Unknown means an execution exists but its state is unknown. These meanings must not be inferred from one another.

Execution observations for one accepted submission attempt are monotonic. Succeeded and Failed are stable terminal execution states; a provisioner must not later report Running or the opposite terminal state for the same execution. A correlated observed Failed state is a terminal execution outcome even when its normalized cause is Timeout or Unavailable. In contrast, a Submit or Observe call error, RequestCorrelation Unknown, and an ambiguous Submit response do not prove an execution outcome and remain Unknown.

Only an explicit RequestCorrelation NotFound can authorize another submission attempt after an ambiguous submission. Liftr creates a new attempt with the same OperationID and immutable intent; it never rewinds or reuses the old attempt. Found prevents resubmission even when Execution is nil. Unknown requires later observation and cannot authorize resubmission.

ExecutionHandle is an opaque backend reference used by the provisioner to correlate subsequent observations with submitted intent. It may identify an execution, remote run, resource or reconciliation target, or a Git change. Liftr must never inspect or branch on its contents. Handles do not belong in ResourceSpec or ResourceStatus and will be persisted later at the application or provisioning-attempt boundary.

Execution failures are normalized into provider-neutral kinds such as Unsupported, Unavailable, Timeout, NotFound, ExecutionFailed, and Unknown. Liftr owns the policy that maps these observations to Operation transitions, Conditions, retries, and Events. Provisioners do not construct ResourceStatus or own ObservedGeneration.

Planning remains an internal Liftr OperationPhase. It does not require a corresponding provider method. An adapter may perform planning internally, skip it, or use a different workflow before returning a submission result.

## Consequences

- Pulumi and Terraform/OpenTofu can perform provider-native planning internally without making plan a contract requirement.
- Crossplane can accept desired state and return Accepted while Liftr observes readiness later.
- GitOps can publish desired intent and return an opaque change reference or no handle, then converge through Observe.
- Synchronous providers can return Succeeded directly from Submit.
- Liftr can distinguish request correlation, absence of a current execution, and unknown execution state.
- Ambiguous submission outcomes retain the stable OperationID and require observation before any same-ID resubmission can be considered. A timeout, transport error, nil Execution, or Unknown correlation does not prove that submission failed.
- An explicit NotFound correlation creates a separate auditable submission attempt under the original OperationID; previous attempts remain unchanged.
- Provider adapters translate backend-specific facts and failures but do not own Liftr lifecycle policy, generation semantics, authorization, approval policy, or Event creation.
- The application boundary records execution intent, handles, and idempotency metadata. Durable uniqueness, crash recovery, and at-least-once dispatch require Milestone 5 transactional persistence and an outbox or equivalent dispatcher.
- The initial contract does not standardize cancellation because cancellation is not uniformly available across backend models.

## Alternatives Considered

### Plan, Apply, and Destroy Methods

These methods model an imperative workflow and do not fit declarative controllers or GitOps systems. They were rejected.

### Single Execute Method

A single synchronous-looking method cannot represent accepted desired state, long-running convergence, or ambiguous submission safely. It was rejected.

### Provider-Owned Reconciliation

Allowing adapters to own reconciliation policy would duplicate Liftr lifecycle semantics and weaken generation and audit guarantees. It was rejected.

### Complete Domain Operation as Input

Provisioners do not need Liftr transition state and must not mutate or own it. Only the immutable execution context required by the backend is provided.

### Backend Configuration in ResourceSpec

Provider configuration such as stacks, workspaces, repositories, branches, clusters, and cloud settings would leak implementation details into developer intent. It was rejected.

### Mandatory Cancellation

Cancellation is not uniformly meaningful for declarative and externally managed systems. It is deferred until a concrete cross-backend requirement exists.
