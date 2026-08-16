# ADR-0004: Provider-Neutral Provisioner Contract

- Status: Accepted
- Date: 2026-08-16

## Context

Liftr's domain model and lifecycle engine are intentionally independent from infrastructure implementation systems. Liftr must eventually integrate with imperative systems, declarative controllers, GitOps workflows, native cloud APIs, on-premises infrastructure, and custom provisioning systems.

These systems do not share a universal execution workflow. Some expose planning and applying, while others accept desired state and converge asynchronously. A contract based on provider-native methods would either exclude valid implementations or leak implementation concepts into Liftr.

The contract also needs to represent a ready existing resource when there is no current backend execution. An absent execution is different from an execution whose state cannot be determined.

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

ExecutionObservation contains an optional Execution object alongside ResourceObservation facts. A nil Execution means no current execution exists. A non-nil Execution with state Unknown means an execution exists but its state is unknown. These meanings must not be conflated.

ExecutionHandle is an opaque backend reference used by the provisioner to correlate subsequent observations with submitted intent. It may identify an execution, remote run, resource or reconciliation target, or a Git change. Liftr must never inspect or branch on its contents. Handles do not belong in ResourceSpec or ResourceStatus and will be persisted later at the application or provisioning-attempt boundary.

Execution failures are normalized into provider-neutral kinds such as Unsupported, Unavailable, Timeout, NotFound, ExecutionFailed, and Unknown. Liftr owns the policy that maps these observations to Operation transitions, Conditions, retries, and Events.

Planning remains an internal Liftr OperationPhase. It does not require a corresponding provider method. An adapter may perform planning internally, skip it, or use a different workflow before returning a submission result.

## Consequences

- Pulumi and Terraform/OpenTofu can perform provider-native planning internally without making plan a contract requirement.
- Crossplane can accept desired state and return Accepted while Liftr observes readiness later.
- GitOps can publish desired intent and return an opaque change reference or no handle, then converge through Observe.
- Synchronous providers can return Succeeded directly from Submit.
- Liftr can distinguish no active execution from unknown execution state.
- Ambiguous submission outcomes require stable OperationID-based retry or observation rather than blindly creating another Operation.
- Provider adapters translate backend-specific facts and failures but do not own Liftr lifecycle policy, generation semantics, authorization, approval policy, or Event creation.
- Durable uniqueness, atomic one-active-operation enforcement, handle persistence, and idempotency remain future application/persistence responsibilities.
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
