# Target Architecture

## Status

This document describes Liftr's intended high-level architecture and marks the
boundary between implemented components and future composition. It does not
claim that every accepted adapter or production integration is wired.

The repository currently includes the provisioner-neutral domain and lifecycle
model, durable PostgreSQL orchestration, a ticker-driven fenced outbox worker,
the versioned public HTTP Resource API, ResourceType discovery and validation,
the official CLI and Backstage clients, production observability, platform
admission policy and quotas, and Pulumi and Crossplane adapter foundations.
PostgreSQLDatabase/v1 has a real Pulumi execution path; its private Azure
reference program still awaits its first successful opt-in acceptance run.

M19 also implements the stateful OpenTofu package and private PostgreSQL
evidence/quarantine foundation described by
[ADR-0020](adr/0020-stateful-opentofu-cli-provisioning.md). It is scoped only to
OpenTofu 1.12.6 and does not change the public API. Production server
composition is optional through one strict operator-supplied configuration
file; no production cloud program or backend registration is shipped.
Qualification is complete against a real official OpenTofu 1.12.6 binary and
the conformant test HTTP backend. Qualification of each production HTTPS HTTP
backend remains the operator's responsibility. Terraform remains unsupported
and has no selected version.

## Product Boundary

Liftr is a vendor-neutral resource lifecycle control plane. Developers interact
with stable Resource contracts. Platform teams determine how those Resources
are implemented without exposing provisioner, backend, source-control,
orchestration, or cloud-provider details to consumers.

## Conceptual Architecture

```text
Backstage / liftr CLI / direct API clients / other portals
                            |
                            v
                          Liftr
                            |
             +--------------+--------------+
             |              |              |
         Resource       Lifecycle      Operations
          Model           Engine        and Events
             |              |              |
             +--------------+--------------+
                            |
                       Adapter Layer
                            |
             +--------------+--------------+
             |              |              |
          Pulumi         OpenTofu       Crossplane
             |              |              |
             +--------------+--------------+
                            |
              Cloud / Kubernetes / on-premises
```

The named clients, adapters, and targets are illustrative, not mandatory.
Backstage is one API client. Git and Kubernetes are optional. Cloud providers
and all provisioner technologies remain private implementation choices.

## Architectural Boundaries

- **Resource model:** Defines provisioner-neutral identity, desired state,
  observed state, lifecycle state, and status.
- **Lifecycle engine:** Owns deterministic create, update, delete, retry, and
  asynchronous reconciliation policy. Adapters do not own business policy.
- **Operations and Events:** Provide the auditable account of requested
  mutations, progress, and outcomes. Public Operation history is
  Resource-scoped; attempts, phases, mappings, and insertion sequence remain
  private.
- **Adapter layer:** Translates Liftr capabilities into implementation-system
  actions. Adapter concepts never become ResourceSpec fields or public API
  methods.
- **Persistence boundary:** Stores Resources, Operations, Events, outputs,
  private bindings, execution evidence, and outbox work through ports defined
  by application and domain needs.
- **Clients:** The CLI, Backstage, portals, and automation consume only the
  public `/v1` API. They do not import server or provisioner implementation
  packages.

Desired state and normalized observed state are separate. Operations target a
specific desired-state generation. Events are append-only audit history, not an
event-sourcing mechanism. Long-running infrastructure work is asynchronous and
must not hold database, Resource, Operation, quota, or idempotency locks while a
provider executes.

## Current Implementation

### Core and public API

- `cmd/liftr-server` composes the HTTP server and, when
  `LIFTR_DATABASE_URL` is set, PostgreSQL persistence and a ticker-driven
  outbox worker loop. API and worker roles remain independently deployable even
  though the repository does not yet ship a separate worker executable.
- `internal/domain` and `internal/lifecycle` define the neutral Resource model
  and pure lifecycle semantics without importing HTTP, storage, telemetry, or
  provisioner implementations.
- `internal/application` owns admission, authorization ports, orchestration,
  stable private provisioner bindings, passive observation, evidence
  monotonicity, explicit retry, output reconciliation, and schema/transition
  validation before durable effects.
- `internal/api/http` implements the versioned `/v1` Resource, ResourceType,
  and Resource-scoped Operation API with asynchronous mutations, concrete
  generation preconditions, idempotency, keyset pagination, and RFC 9457
  Problems.
- `internal/resourcetypes` publishes self-contained JSON Schema draft 2020-12
  contracts and declared non-secret output fields without exposing platform
  registrations. PostgreSQLDatabase/v1 and v2 preserve their versioned public
  contracts.
- Authentication verifies RFC 9068 access tokens from the configured issuer;
  application authorization uses normalized owner membership. Inventory uses
  independent `resource:list` authorization and visibility-bound pagination.
- `internal/persistence/postgres` provides checksummed migrations, durable
  lifecycle records, immutable attempts, outputs, private evidence, and the
  transactionally claimed and fenced outbox.

### Provisioning adapters

- The provider-neutral boundary exposes capabilities plus asynchronous
  Submit/Observe behavior. Provisioners receive private, stable bindings from
  composition; public Resource contracts contain no Pulumi, OpenTofu,
  Crossplane, backend, provider, or cloud fields.
- The Pulumi Automation API foundation uses isolated local Go programs and
  deterministic retained stacks. PostgreSQLDatabase/v1 executes through this
  path. The credential-free CLI/file-backend path is distinct from the
  unvalidated, opt-in, cost-bearing Azure reference acceptance suite.
- The Crossplane adapter drives platform-owned composite resources with
  identity-safe conditional writes, UID-preconditioned deletion, and
  freshness-aware readiness. All Kubernetes knowledge stays in its adapter
  subtree ([ADR-0015](adr/0015-crossplane-provisioner-and-declarative-reconciliation.md)).
- `internal/provisioning/opentofu` implements the OpenTofu 1.12.6 adapter
  package. Its production profile is an operator-supplied conformant HTTPS HTTP
  state backend. OpenTofu owns GET/state-update/LOCK/UNLOCK and lock-ID
  propagation; the local backend and any test HTTP backend are
  development/test-only.

The OpenTofu adapter retains one stable backend state key per Resource while
using private, disposable per-call scratch workdirs. Normal success removes
scratch. Ambiguous/errored workdirs, including those containing
`errored.tfstate`, move to a separate quarantine; startup scans only owned
orphans. Private PostgreSQL attempt and state evidence is fenced and monotonic.
On Unix, context cancellation interrupts and then forcibly terminates the owned
process group after a bound; public Operation cancellation and Windows adapter
support remain deferred.

For OpenTofu delete, Liftr applies a normal saved plan with
`desiredPresent=false`: registered workload addresses disappear, but the
private control marker and backend state remain. It never uses whole-state
destroy, backend DELETE, `state rm`, or garbage collection. Output extraction
uses bounded private all-root-output metadata to enforce `sensitive=false`,
immediately discards unmapped values, and never logs output names or values.

Production OpenTofu composition is optional through
`LIFTR_OPENTOFU_CONFIG_FILE`. The private type route affects only new Resources;
existing Resources resolve their persisted provisioner ref exactly. Operators
must retain all historical registrations referenced by Resources in the one
file while routing new Resources separately. Each provisioner ref and its
executable/source/program/backend identities remain immutable; operators must
not hide a configuration change beneath an existing ref. No production cloud program or
backend registration is shipped. Real OpenTofu 1.12.6 qualification is complete
against the conformant test HTTP backend; each operator remains responsible for
qualifying its production HTTPS HTTP backend. Remote execution products such as
HCP Terraform or Terraform Enterprise are deferred; that does not defer the
selected HTTP state backend.

### Clients and integrations

- `cmd/liftr` → `internal/cli` → `internal/client` implements the official
  CLI as a pure public-API client ([ADR-0013](adr/0013-cli-as-a-public-api-client.md)).
  It uses externally issued bearer tokens, rejects non-loopback plaintext HTTP
  and redirects, keeps server links same-origin, preserves JSON number
  representations, and supports generation-safe create/update/delete, inventory,
  Operation history, retry, and authoritative `--wait` polling.
- `integrations/backstage` contains the official frontend plugin, constrained
  user-delegating BFF, and shared TypeScript public-API contracts
  ([ADR-0017](adr/0017-backstage-as-a-delegated-public-api-client.md)). The
  Software Catalog is not Liftr inventory.

### Policy and observability

- Immutable platform admission policy is a restrictive application overlay
  after authorization and before persistence. Quota-bearing creates serialize
  by owner in PostgreSQL and count every retained non-Deleted Resource
  ([ADR-0019](adr/0019-platform-policy-and-transactional-admission-quotas.md)).
  Policy never enters ResourceType or provisioner contracts.
- Observability is composition-owned and cannot participate in lifecycle
  decisions. Structured logs, bounded OpenTelemetry metrics, optional
  Prometheus and OTLP export, boundary tracing, panic recovery, operational
  sampling, readiness, and ordered shutdown are implemented under
  [ADR-0018](adr/0018-production-observability-and-operational-diagnostics.md).
  Identifiers never become metric labels, and exporter failures never affect
  requests or durable outcomes.

## Still Future

Independent readiness verification, drift detection, secret reference
resolution, production cloud compositions beyond private reference bindings,
shipped OpenTofu cloud registrations, production HTTPS backend qualification,
remote execution integrations, multi-region execution, and conditional
exact-version Terraform support remain future work unless another ADR records
their implementation.
