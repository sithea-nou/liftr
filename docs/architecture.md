# Target Architecture

## Status

This document describes Liftr's intended high-level architecture. It is a direction for future development, not a description of components currently implemented. Today, Liftr consists only of a minimal HTTP server and health endpoint.

## Product Boundary

Liftr is intended to be a vendor-neutral resource lifecycle control plane. Developers interact with stable Resource contracts. Platform teams determine how those resources are implemented without exposing implementation-specific concepts to resource consumers.

## Target Components

The target architecture is expected to include these conceptual areas as the product evolves:

- **Resource API:** Accepts provisioner-neutral desired state and exposes resource identity, lifecycle state, and status.
- **Core domain:** Defines resource lifecycle rules, separates desired state from observed state, and remains independent from transport, persistence, and provisioner technologies.
- **Lifecycle orchestration:** Coordinates asynchronous lifecycle transitions and records their progress.
- **Operations and events:** Provide an auditable account of requested mutations, execution progress, and outcomes.
- **Provisioner adapters:** Translate Liftr's infrastructure capabilities into calls to external implementation systems. Adapters execute capabilities but do not define business policy.
- **Persistence boundary:** Stores resources, operations, events, and observed state through interfaces defined by domain needs.
- **Clients:** Developer portals, command-line tools, automation, and other systems consume Liftr through its public API. Backstage may be one such client, but is not part of Liftr core.

## Architectural Boundaries

Public Resource contracts must not depend on a specific provisioner, source-control workflow, CI/CD system, orchestrator, or cloud provider. Git and Kubernetes may participate in an implementation, but neither is required by Liftr's architecture.

Long-running infrastructure work will be modeled asynchronously. A lifecycle request should create an auditable Operation, with Events describing meaningful progress and outcomes. Detailed schemas and execution semantics are intentionally deferred until a later milestone.

## Current Implementation

Only the process and HTTP bootstrap exists:

- `cmd/liftr-server` starts the HTTP server, emits structured logs, and performs graceful shutdown.
- `internal/server` exposes the `GET /healthz` route.

The remaining components in this document are targets and have not been implemented.
