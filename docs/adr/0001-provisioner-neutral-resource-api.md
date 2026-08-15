# ADR-0001: Provisioner-Neutral Resource API

- Status: Accepted
- Date: 2026-08-15

## Context

Liftr is intended to give developers a stable way to manage resources while allowing platform teams to choose how those resources are implemented. Infrastructure systems have different state models, workflows, terminology, and operational constraints. If those details appear in developer-facing contracts, consumers become coupled to a particular implementation and platform teams cannot evolve implementations without causing API churn.

Liftr also needs a clear ownership boundary: the control plane owns resource lifecycle policy and orchestration, while infrastructure technologies execute the capabilities assigned to them.

## Decision

Liftr exposes a provisioner-neutral Resource API and treats infrastructure technologies as implementation adapters.

Developer-facing Resource contracts describe resource intent and lifecycle concepts in Liftr's domain language. They must not expose provisioner-specific stacks, plans, workspaces, compositions, repositories, pipelines, state files, or cloud-provider details.

Provisioner adapters translate between Liftr's internal capability requests and an implementation technology. Adapters execute infrastructure operations and report observations and progress, but they do not own Liftr's business policy or public contracts.

## Consequences

- Developers receive stable Resource contracts that are independent of infrastructure implementation choices.
- Platform teams can select or replace implementation technologies without requiring equivalent changes from API consumers.
- Liftr must define its own resource identity, desired state, observed state, operation, event, and lifecycle semantics.
- Adapter boundaries require deliberate capability modeling and translation between Liftr and implementation-specific concepts.
- Some features unique to an implementation may not be exposed unless they can be represented as a coherent provisioner-neutral capability.
- Liftr carries responsibility for lifecycle orchestration and consistent behavior across adapters.

## Alternatives Considered

### Expose Provisioner-Native APIs

Liftr could proxy each infrastructure technology's native concepts. This would reduce initial translation work but would couple developers to implementation details, fragment the API, and make implementation changes disruptive. This alternative was rejected.

### Standardize on One Provisioner

Liftr could choose a single infrastructure technology as part of its core contract. This would simplify the first implementation but conflict with the project's vendor-neutral goal and constrain platform teams. This alternative was rejected.

### Make Git or CI/CD the Public Contract

Liftr could require resources to be represented as repositories, files, pull requests, or pipelines. This would make workflow tooling part of the product model and exclude valid API-driven implementations. Git and CI/CD may be used by adapters or clients, but remain optional implementation details. This alternative was rejected.
