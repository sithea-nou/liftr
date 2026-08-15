# Target Architecture

## Status

This document describes Liftr's intended high-level architecture. It is a direction for future development, not a description of components currently implemented. Today, Liftr consists only of a minimal HTTP server and health endpoint.

## Product Boundary

Liftr is intended to be a vendor-neutral resource lifecycle control plane. Developers interact with stable Resource contracts. Platform teams determine how those resources are implemented without exposing implementation-specific concepts to resource consumers.

## Conceptual Architecture

The long-term architecture is organized around the following flow:

```text
Backstage / CLI / API / other portals
                  |
                  v
                Liftr
                  |
       +----------+----------+
       |          |          |
   Resource   Lifecycle   Operations
    Model       Engine
       |          |          |
       +----------+----------+
                  |
             Adapter Layer
                  |
       +----------+----------+
       |          |          |
    Pulumi    Terraform   Crossplane
       |          |          |
       +----------+----------+
                  |
      Cloud / Kubernetes / on-premises
```

The named clients, adapters, and infrastructure targets are illustrative rather than required. Backstage is one possible client. Pulumi, Terraform, and Crossplane are possible adapter implementations. Git, Kubernetes, and any particular cloud provider remain optional implementation choices.

## Target Components

The target architecture is expected to include these conceptual areas as the product evolves:

- **Clients:** Backstage, command-line tools, direct API consumers, developer portals, and automation consume Liftr through its public API.
- **Resource Model:** Defines provisioner-neutral resource identity, desired state, observed state, lifecycle state, and status.
- **Lifecycle Engine:** Owns lifecycle policy and coordinates asynchronous transitions between desired and observed state.
- **Operations and Events:** Provide an auditable account of requested mutations, execution progress, and outcomes.
- **Adapter Layer:** Translates Liftr-defined infrastructure capabilities into calls to external implementation systems. Adapters execute capabilities but do not define business policy.
- **Infrastructure targets:** Cloud platforms, Kubernetes environments, and on-premises systems may be managed through adapters without becoming part of public Resource contracts.
- **Persistence boundary:** Stores resources, operations, events, and observed state through interfaces defined by domain needs.

## Architectural Boundaries

Public Resource contracts must not depend on a specific provisioner, source-control workflow, CI/CD system, orchestrator, or cloud provider. Git and Kubernetes may participate in an implementation, but neither is required by Liftr's architecture.

Dependencies point toward Liftr's domain concepts and capability contracts. Adapter implementations depend on those contracts; the Resource Model and Lifecycle Engine do not depend on adapter or infrastructure-technology types.

Long-running infrastructure work will be modeled asynchronously. A lifecycle request should create an auditable Operation, with Events describing meaningful progress and outcomes. Detailed schemas and execution semantics are intentionally deferred until a later milestone.

## Current Implementation

Only the process and HTTP bootstrap exists:

- `cmd/liftr-server` starts the HTTP server, emits structured logs, and performs graceful shutdown.
- `internal/server` exposes the `GET /healthz` route.

The remaining components in this document are targets and have not been implemented.
