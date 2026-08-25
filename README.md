# Liftr

Liftr is a vendor-neutral resource lifecycle control plane.

> Developers manage resources. Platform teams manage how those resources are implemented.

## What Liftr Aims to Become

Liftr aims to expose a stable, developer-facing Resource API while allowing platform teams to choose and evolve the infrastructure systems that implement those resources. Infrastructure technologies will integrate through adapters rather than appear in public Resource contracts.

The target architecture is described in [docs/architecture.md](docs/architecture.md). Architectural decisions are recorded in [docs/adr](docs/adr).

## Current Implementation

Liftr is in early development. The repository currently implements:

- A Go HTTP server built with the standard library.
- A `GET /healthz` endpoint.
- Graceful shutdown on interrupt and termination signals.
- Structured JSON logging with `log/slog`.
- A provisioner-neutral core domain model.
- A deterministic, pure lifecycle engine for create, update, and delete semantics.
- A provider-neutral provisioning contract with a deterministic fake.
- An application/orchestration layer with persistence ports and stable private provisioner bindings.
- A pgx PostgreSQL persistence adapter with explicit checksummed migrations.
- A transactional outbox and provisioner-neutral `RunOnce` worker with fenced leases and ambiguous-dispatch recovery.
- A Pulumi Automation API provisioner foundation using isolated local Go programs, deterministic retained stacks, and a filesystem state backend.
- A non-provisioning PostgreSQLDatabase example ResourceType whose developer contract is discoverable and schema-validated.
- ResourceType discovery: `GET /v1/resource-types` and `GET /v1/resource-types/{name}/{version}` publish developer contracts with self-contained JSON Schema (draft 2020-12) spec schemas; admission validates specs against those contracts before any durable effect.
- Contract-owned update-transition validation: PostgreSQLDatabase/v1 declares engine version immutable, storage grow-only, and high availability freely toggleable; illegal transitions are rejected synchronously as structured 422 problems with zero durable effects.
- First real ResourceType execution: PostgreSQLDatabase/v1 executes through the Pulumi Automation API adapter. The execution architecture — envelope encoding, platform-scoped naming, stack identity, history correlation, create/update/delete orchestration, recovery, and honest observation facts — is validated by credential-free CI tests that drive a clearly named deterministic test program through the real CLI against the file backend. A private reference implementation targeting Azure Database for PostgreSQL Flexible Server is provided behind opt-in, cost-bearing acceptance tests; **it is not yet validated** and becomes trusted only after that suite has run successfully against a real subscription.
- Platform-scoped infrastructure naming, a registration-scoped environment allowlist for provider credentials, generated-and-encrypted administrator secrets that never reach any public surface, and honest normalized observations (`Ready` means the latest desired-generation execution succeeded; drift is not yet detected).
- Resource outputs: `PostgreSQLDatabase/v2` declares required non-secret realized values (`hostname`, `port`) as part of its immutable contract. Validated output snapshots persist per generation, are embedded in Resource representations with an explicit `outputs.observedGeneration`, and survive restarts. `PostgreSQLDatabase/v1` remains unchanged and spec-only.
- An initial runtime composition: `liftr-server` wires durable persistence, the contract registry, the Pulumi adapter, and a ticker-driven outbox worker loop with context-driven shutdown. API serving and worker execution remain independently deployable.
- The official `liftr` CLI ([ADR-0013](docs/adr/0013-cli-as-a-public-api-client.md)): a pure client of the public HTTP API with ResourceType discovery, generation-safe create/update/delete, Operation inspection, `--wait` polling of the authoritative monitor Operation, automatic idempotency keys with explicit replay override, JSON/text output that preserves server number representations verbatim, and stable exit codes. It consumes externally issued bearer tokens (`--token-file`, `LIFTR_TOKEN_FILE`, or `LIFTR_TOKEN`), requires HTTPS everywhere except loopback plaintext HTTP, treats server-supplied links as untrusted same-origin navigation only, and never implements login flows or persists credentials.
- Resource-scoped Operation history and explicit failed-Operation retry ([ADR-0014](docs/adr/0014-operation-history-and-explicit-retry.md)): `GET /v1/resources/{id}/operations` provides stable cursor pagination with no global list, and `POST /v1/operations/{id}/retry` admits a new exact-generation child Operation under independent `resource:retry` authorization. Retry provenance is public as optional `retryOf`; insertion sequence, Events, attempts, mappings, and phases remain private. Output-only repair observes an explicitly compatible mapping against the source attempt without resubmitting infrastructure.
- Production observability ([ADR-0018](docs/adr/0018-production-observability-and-operational-diagnostics.md)): structured request/admission/worker logs correlated by server-minted request IDs and sanitized caller correlation IDs; OpenTelemetry metrics with standard HTTP semantic conventions plus a Liftr-namespaced lifecycle/worker/outbox/provisioner surface, exported via an opt-in Prometheus listener (`LIFTR_METRICS_ADDR`) and optional OTLP/gRPC push through the standard `OTEL_*` variables (M17 supports OTLP/gRPC only; other protocols fail startup clearly); minimal boundary tracing that stays inert unless OTLP is configured; typed authentication failure diagnostics behind one unchanged public 401; low-cardinality labels enforced by architecture tests; an operational sampler exposing cluster-global backlog, active-Operation age, and honest long-running / reconciliation-silence stuck-candidate gauges (diagnostic only — never lifecycle state); commitment-aware panic recovery for HTTP and per-work recovery for workers with lease-preserving ambiguity; readiness gated only on the control-plane core (PostgreSQL usable, schema verified at boot, not draining); and an ordered graceful shutdown that preserves M6/M14 ambiguity safety. Telemetry never participates in lifecycle decisions and exporter failures never affect requests. Operational guidance lives in [docs/runbook.md](docs/runbook.md).
- Platform policy and transactional admission quotas ([ADR-0019](docs/adr/0019-platform-policy-and-transactional-admission-quotas.md)): `LIFTR_POLICY_FILE` strictly compiles one immutable startup revision supporting restrictive create/update capability denials and per-owner retained-Resource limits. Quota-bearing creates serialize by owner in PostgreSQL before Resource/Operation locks, count every non-Deleted state, fail closed on corrupt missing status, and persist the admitting revision as private typed Event provenance. Authorization remains before replay; successful replays bypass changed policy. Public clients receive only stable `POLICY_DENIED` or `QUOTA_EXCEEDED` Problems.
- A stateful OpenTofu CLI adapter package and PostgreSQL evidence foundation
  ([ADR-0020](docs/adr/0020-stateful-opentofu-cli-provisioning.md)), scoped
  exactly to OpenTofu 1.12.6. It implements fenced attempt/state correlation,
  one stable backend state key, normal saved-plan apply for
  create/update/delete, and bounded private all-root-output metadata handling
  that enforces `sensitive=false`, immediately discards unmapped values, and
  never logs output data. It also implements Unix process-tree cancellation,
  disposable per-call scratch workdirs, and quarantine. Production requires an
  operator-supplied conformant HTTPS HTTP
  state backend where OpenTofu owns GET/update/LOCK/UNLOCK and lock-ID
  propagation; the local backend and test HTTP backend are
  development/test-only. Production server composition is optional through one
  strict, immutable operator-supplied `LIFTR_OPENTOFU_CONFIG_FILE`. That file
  retains immutable historical registrations separately from the current routes
  used for new Resources; no production cloud program or backend registration is
  shipped. Qualification is complete
  against a real official OpenTofu 1.12.6 binary and the conformant test HTTP
  backend, while qualification of each production HTTPS HTTP backend remains
  the operator's responsibility. The public API is unchanged. Terraform has no
  selected version or support claim.
- Initial tests and continuous integration.

## Future Direction

Liftr implements one reference implementation, not generic multi-cloud PostgreSQL support. Secret references (opaque, non-bearer, externally backed) are defined in ADR-0011 but not yet implemented: administrator credentials are generated privately inside the Pulumi secret dataflow and have no retrieval or resolution path. Drift detection, independent readiness verification, credential retrieval and rotation, multi-cloud implementations, shipped production OpenTofu cloud registrations, conditional Terraform compatibility, multi-issuer federation, and secret resolution remain future work. The API requires RFC 9068 JWT access-token authentication with owner-based authorization (ADR-0012): the full runtime refuses to start without issuer configuration, and `LIFTR_AUTH_MODE=insecure` is an explicit, loudly-warned development opt-in only. The target architecture is described in [docs/architecture.md](docs/architecture.md), and the decisions that shape future work are recorded in [docs/adr](docs/adr).

## Getting Started

Requirements:

- Go 1.25.11 or newer.
- PostgreSQL 17 for persistence integration tests.
- Pulumi CLI 3.257.0 for Pulumi adapter integration tests.

OpenTofu is not required unless `LIFTR_OPENTOFU_CONFIG_FILE` is set. M19
qualification has run with a real official OpenTofu CLI at exactly 1.12.6 and
the conformant test HTTP backend. That is not qualification evidence for an
operator's production HTTPS HTTP state backend; operators must qualify their
own backend and keep every bound provisioner ref plus executable, source,
program, and backend identity present and immutable while existing Resources use it.

Run the server:

```sh
go run ./cmd/liftr-server
```

The server listens on `:8080` by default. Set `LIFTR_ADDR` to use another address:

```sh
LIFTR_ADDR=:9090 go run ./cmd/liftr-server
```

Check its health:

```sh
curl http://localhost:8080/healthz
```

Use the CLI against it. Start the server in explicit development mode, then:

```sh
go run ./cmd/liftr version
LIFTR_TOKEN=dev go run ./cmd/liftr resource-type list
LIFTR_TOKEN=dev go run ./cmd/liftr resource-type get PostgreSQLDatabase v2
LIFTR_TOKEN=dev go run ./cmd/liftr resource create \
    --id orders-db --type PostgreSQLDatabase --version v2 \
    --owner team=payments --spec spec.json
LIFTR_TOKEN=dev go run ./cmd/liftr resource get orders-db
LIFTR_TOKEN=dev go run ./cmd/liftr resource list --owner team=payments
LIFTR_TOKEN=dev go run ./cmd/liftr operation get op-xxxx
LIFTR_TOKEN=dev go run ./cmd/liftr operation list --resource orders-db
LIFTR_TOKEN=dev go run ./cmd/liftr operation retry op-failed --wait
LIFTR_TOKEN=dev go run ./cmd/liftr resource update orders-db --spec new-spec.json --wait
LIFTR_TOKEN=dev go run ./cmd/liftr resource delete orders-db
```

The CLI defaults to `http://localhost:8080` (override with `--server` or `LIFTR_SERVER`; non-loopback servers require HTTPS). Tokens are read from files or the environment and never persisted; see [ADR-0013](docs/adr/0013-cli-as-a-public-api-client.md) for the credential-handling and exit-code contract.

Run all checks:

```sh
make verify
```

Backstage integration checks live under Node and never run inside `make verify`:

```sh
make verify-backstage   # requires Node >= 20; see integrations/backstage/README.md
```

CI runs the Go and Backstage jobs independently. See
[ADR-0017](docs/adr/0017-backstage-as-a-delegated-public-api-client.md) for the
integration architecture.

PostgreSQL-backed tests run when `LIFTR_TEST_DATABASE_URL` is set. Start the local database and run all checks with:

```sh
docker compose up -d postgres
LIFTR_TEST_DATABASE_URL='postgres://liftr:liftr@localhost:55432/liftr?sslmode=disable' make verify
```

Pulumi integration tests run when `LIFTR_TEST_PULUMI_ROOT` identifies a preinstalled Pulumi layout containing `bin/pulumi`:

```sh
LIFTR_TEST_PULUMI_ROOT="$HOME/.pulumi" go test ./internal/provisioning/pulumi ./internal/server -count=1
```

### Azure acceptance tests

The reference implementation against real Azure Database for PostgreSQL Flexible Server is an **opt-in, cost-bearing** external test. It is never part of `go test ./...` or `make verify`, and it never runs without explicit configuration:

> **Validation status:** this acceptance suite has not been executed yet. Until it passes against a live subscription, the Azure reference implementation is provided as architecture only and must be treated as unvalidated.

```sh
export LIFTR_ACCEPTANCE_AZURE=1
export LIFTR_ACCEPTANCE_LOCATION='eastus'          # private platform choice
export LIFTR_ACCEPTANCE_SKU_NAME='Standard_B1ms'
export LIFTR_ACCEPTANCE_SKU_TIER='Burstable'
export LIFTR_ACCEPTANCE_HA_MODE='SameZone'         # mode used when highAvailability=true
export ARM_SUBSCRIPTION_ID=... ARM_TENANT_ID=... ARM_CLIENT_ID=... ARM_CLIENT_SECRET=...
export PULUMI_CONFIG_PASSPHRASE=...                # encrypts generated credentials in state
LIFTR_TEST_PULUMI_ROOT="$HOME/.pulumi" make test-acceptance-azure
```

Never commit generated credentials or cloud state.

Apply migrations explicitly with:

```sh
LIFTR_DATABASE_URL='postgres://liftr:liftr@localhost:55432/liftr?sslmode=disable' go run ./cmd/liftr-migrate
```

Production server startup does not apply migrations automatically.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidance and [AGENTS.md](AGENTS.md) for the architectural constraints that govern changes.

## License

Liftr is licensed under the [Apache License 2.0](LICENSE).
