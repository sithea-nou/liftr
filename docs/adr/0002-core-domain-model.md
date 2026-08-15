# ADR-0002: Initial Core Domain Model

- Status: Accepted
- Date: 2026-08-15

## Context

ADR-0001 establishes that Liftr exposes a provisioner-neutral Resource API and treats infrastructure technologies as implementation adapters. Liftr now needs an initial domain model that represents developer intent, normalized observations, asynchronous work, and auditable history without introducing transport, storage, or provisioner dependencies.

The model must support different ResourceTypes while preventing their implementation details from entering the core. It also needs enough concurrency information to correlate asynchronous work and history with changing desired state.

## Decision

Liftr's initial core domain consists of Resource, ResourceType, ResourceSpec, ResourceStatus, Condition, Capability, Operation, and Event.

Resource contains developer intent. It identifies a versioned ResourceType, has a provisioner-neutral OwnerRef, contains a ResourceSpec, and carries a monotonically increasing Generation. ResourceStatus is a separate normalized observation with an ObservedGeneration. Desired and observed state are not represented by the same object.

ResourceSpec is an opaque wrapper around portable object values. It validates its structural values, owns its mutable map state, and returns only defensive copies. ResourceType-specific schema validation is deferred until discovery and API requirements justify choosing a schema mechanism.

ResourceStatus uses a deliberately small top-level state vocabulary: Unknown, Pending, Ready, Deleting, and Failed. More specific facts such as Healthy, Drifted, Reconciling, and Compliant are represented as Conditions.

Capability is one extensible developer-facing action type. Create, update, and delete are lifecycle capabilities. Observe and future actions that do not change desired lifecycle state are operational capabilities. Both categories use the same Capability type until concrete behavior requires a stronger distinction.

Operations represent asynchronous capability invocations. Every Operation targets a specific Resource and TargetGeneration so work can be correlated with the desired-state revision that initiated it.

Events represent significant lifecycle history. Every Event records a Resource generation and may reference an Operation. Events are append-only audit and history records. Liftr is not using event sourcing: Resources, ResourceStatus, and Operations remain authoritative domain records and are not reconstructed from Events.

PostgreSQLDatabase is the first example ResourceType, but its name, specification fields, and validation remain outside the core domain package.

## Consequences

- Core domain code remains independent from HTTP, storage, provisioners, identity providers, and infrastructure technologies.
- Developer intent cannot be mutated indirectly through a ResourceSpec map reference.
- Generation and TargetGeneration make stale asynchronous work detectable by future orchestration logic.
- Liftr owns a small normalized state vocabulary while Conditions remain extensible.
- OwnerRef supports ownership without selecting an identity system or portal.
- ResourceType schema publication and full schema validation remain unresolved and will require a later decision when concrete API requirements exist.
- Persistence implementations must preserve append-only Event semantics but do not need to implement an event store as the system of record.

## Alternatives Considered

### Combine Desired and Observed State

A single mutable Resource representation would make it unclear which fields are developer intent and which are Liftr observations. This was rejected because it weakens ownership boundaries and concurrency semantics.

### Expose ResourceSpec Maps Directly

Exposing the underlying map would allow callers to mutate desired state without validation or a Generation change. This was rejected in favor of an opaque value with defensive copies.

### Use Detailed Top-Level Resource States

Adding health, drift, compliance, and reconciliation states to ResourceState would mix independent dimensions and create ambiguous combinations. This was rejected in favor of a small state plus Conditions.

### Use Events as the Source of Truth

Reconstructing all domain state from Events would introduce event sourcing before there is a demonstrated requirement. This was rejected. Events provide append-only audit history only.

### Put PostgreSQL Types in the Core Domain

Embedding the first example's fields or behavior in the core would make the domain resource-type-specific. This was rejected; example ResourceTypes depend on the core, never the reverse.
