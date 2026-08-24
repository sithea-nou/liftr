# Target Architecture

## Status

This document describes Liftr's intended high-level architecture. It is a direction for future development, not a claim that all components are implemented. Today, Liftr has the provisioner-neutral domain and lifecycle model, durable PostgreSQL orchestration, a fenced outbox worker, a Pulumi Automation API adapter foundation, a versioned HTTP Resource API, ResourceType contracts with schema discovery and transition semantics, a first real execution path for PostgreSQLDatabase/v1 through the Pulumi adapter (a private Azure reference implementation is provided but awaits its first successful opt-in acceptance run — see [ADR-0010](adr/0010-resource-type-implementation-binding-and-transition-semantics.md)), generation-associated ResourceOutputs with selected non-secret extraction ([ADR-0011](adr/0011-resource-outputs-and-secret-references.md)), RFC 9068 access-token authentication and owner-membership authorization ([ADR-0012](adr/0012-authentication-authorization-and-actor-identity.md)), the official `liftr` CLI as a pure client of the public HTTP API ([ADR-0013](adr/0013-cli-as-a-public-api-client.md)), and Resource-scoped Operation history with explicit failed-Operation retry ([ADR-0014](adr/0014-operation-history-and-explicit-retry.md)).

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

Long-running infrastructure work is modeled asynchronously. A lifecycle request creates an auditable Operation, with Events describing meaningful progress and outcomes. Public Operation history is Resource-scoped, while Events, attempts, phases, mappings, and insertion sequence remain private execution detail.

The initial domain model keeps Resource developer intent separate from normalized ResourceStatus observations. Operations target a specific desired-state generation. Events carry that generation as append-only audit history; they are not an event-sourcing mechanism. These decisions are recorded in [ADR-0002](adr/0002-core-domain-model.md).

The lifecycle engine applies deterministic create, update, and delete rules using explicit Operation phases. Conditions report normalized facts such as Ready and Reconciled but do not control workflow transitions. Generation concurrency, capability-specific failure semantics, retry attempts, and the Deleted tombstone state are defined in [ADR-0003](adr/0003-deterministic-lifecycle-engine.md).

## Current Implementation

The process bootstrap, core domain model, pure lifecycle semantics, provider-neutral provisioning contract, and application orchestration ports exist:

- `cmd/liftr-server` starts the HTTP server, composes the durable runtime when `LIFTR_DATABASE_URL` is configured, emits structured logs, and performs graceful shutdown including the worker loop.
- Authentication and authorization follow [ADR-0012](adr/0012-authentication-authorization-and-actor-identity.md): the transport verifies RFC 9068 bearer tokens against one configured OIDC issuer; the application owns the Authorizer port and authorizes every exported business use case against stored or requested owners. Principal identity is issuer-qualified with a versioned derivation, memberships are typed owner references normalized at claim mapping, idempotency keys are scoped per principal, admission events record the normalized actor, and worker execution never re-authorizes admitted work.
- `internal/api/http` implements the versioned Resource and Operation HTTP contract ([ADR-0008](adr/0008-http-resource-api-contract-v1.md)): asynchronous create, read, update-admission, and delete-admission with concrete generation preconditions, mandatory idempotency keys, RFC 9457 problems, deterministic latest-operation reads, Resource-scoped paginated Operation history, explicit failed-Operation retry, and health endpoints. It adds ResourceType discovery ([ADR-0009](adr/0009-resource-type-contracts-and-schema-discovery.md)): `GET /v1/resource-types` and `GET /v1/resource-types/{name}/{version}` expose developer contracts with their embedded JSON Schema 2020-12 spec schemas; discovery carries no provisioner or platform state.
- `internal/domain` defines the provisioner-neutral Resource model, normalized status, asynchronous Operations, and audit Events.
- `internal/lifecycle` defines deterministic create, update, and delete orchestration rules without executing infrastructure.
- `internal/provisioning` defines the provider-neutral Submit/Observe boundary, a deterministic contract fake, an isolated Pulumi Automation API adapter ([ADR-0007](adr/0007-pulumi-automation-api-provisioner-adapter.md)), and a Crossplane declarative-reconciliation adapter ([ADR-0015](adr/0015-crossplane-provisioner-and-declarative-reconciliation.md)) that drives platform-owned composite resources with identity-safe conditional writes, UID-preconditioned deletion, dual Crossplane/Liftr freshness for readiness, and all Kubernetes knowledge confined to the adapter subtree.
- `internal/application` coordinates lifecycle, provisioning, stable private provisioner bindings, repository ports, passive observation, evidence monotonicity (stale provider evidence never applies), malformed-facts sanitization, and spec validation against the consumer-owned ResourceType contract before any durable effect. It owns exact-generation retry admission under independent `resource:retry` authorization and durable output resolution (`None/Pending/Published/Rejected`) so backend execution success and reconciliation completion stay separable monotonic dimensions. Explicitly compatible output-mapping repair observes the original successful attempt without resubmitting infrastructure; mappings and recovery provenance remain private.
- `internal/resourcetypes` defines the developer-facing ResourceType contract — identity, display metadata, capabilities, self-contained JSON Schema (draft 2020-12) documents, semantic validation hooks, declared non-secret output fields — and a deterministic in-memory registry satisfying the application catalog port through the neutral shared vocabulary in `internal/resourcecontract`, which both sides import without importing each other. It is the only package that depends on a JSON Schema implementation; validation performs no network schema resolution.
- `internal/persistence/postgres` implements durable state, immutable submission attempts, deterministic private Operation insertion sequence, retry provenance, checksummed migrations, and a transactional outbox.
- `internal/worker` drives lifecycle work, renews long-running Dispatch leases, fences ambiguous recovery, keeps the observe loop alive for active executions when backend evidence is stale or absent, and structurally prevents output-only repair Operations from dispatching provider work.
- `internal/resourcetypes/postgresqldatabase` defines the first executable ResourceType outside the core: its contract publishes the accepted developer intent (`version`, `storageGB`, `highAvailability`), validates specs, and owns the v1 update-transition semantics (version immutable, storage grow-only, availability freely toggleable) that admission enforces synchronously.
- `internal/provisioning/bindings` translates provider-neutral execution intent into private program input envelopes with representation-safe numeric consumption; `internal/provisioning/xrbinding` performs the equivalent translation onto the platform-owned XR contract; composition binds contracts to implementations and platform configuration to implementations without any public leakage. The same `PostgreSQLDatabase/v2` contract runs against either implementation technology depending on deployment composition.
- `internal/server` composes persistence, catalog, provisioner, HTTP handler, and a ticker-driven outbox worker loop for process startup.
- `cmd/liftr` with `internal/cli` and `internal/client` implements the official CLI ([ADR-0013](adr/0013-cli-as-a-public-api-client.md)): a pure client of the public `/v1` HTTP surface that imports no server implementation packages, consumes externally issued bearer tokens from files or the environment, refuses non-loopback plaintext HTTP and all redirects, treats server-supplied monitor references as same-origin-only navigation, preserves server JSON number representations verbatim, generates one idempotency key per mutation invocation with explicit replay override, enforces concrete generation preconditions without silent conflict resolution, gates destructive deletes on exact-ID confirmation or `--yes`, lists Resource-scoped Operation history, retries failed Operations, polls the authoritative monitor Operation under `--wait`, and exposes a documented exit-code contract. Retry `--wait` returns the terminal child Operation JSON rather than a Resource snapshot.

A first real execution path exists: PostgreSQLDatabase/v1 executes through the Pulumi adapter, validated end to end by deterministic credential-free CI programs that drive the real CLI against the file backend. A private reference implementation targeting Azure Database for PostgreSQL Flexible Server accompanies it behind an opt-in, cost-bearing acceptance suite that has **not yet been run**; the Azure program is architecture today and becomes validated only when that suite passes against a live subscription. `Ready` status means the latest desired-generation execution succeeded (execution-reconciliation semantics); drift detection and independent readiness verification are future work.
