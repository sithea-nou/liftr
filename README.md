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
- Initial tests and continuous integration.

## Future Direction

Liftr implements one reference implementation, not generic multi-cloud PostgreSQL support. Secret references (opaque, non-bearer, externally backed) are defined in ADR-0011 but not yet implemented: administrator credentials are generated privately inside the Pulumi secret dataflow and have no retrieval or resolution path. Drift detection, independent readiness verification, credential retrieval and rotation, multi-cloud implementations, Terraform/OpenTofu and Crossplane adapters, CLI tooling, multi-issuer federation, and secret resolution remain future work. The API requires RFC 9068 JWT access-token authentication with owner-based authorization (ADR-0012): the full runtime refuses to start without issuer configuration, and `LIFTR_AUTH_MODE=insecure` is an explicit, loudly-warned development opt-in only. The target architecture is described in [docs/architecture.md](docs/architecture.md), and the decisions that shape future work are recorded in [docs/adr](docs/adr).

## Getting Started

Requirements:

- Go 1.25.11 or newer.
- PostgreSQL 17 for persistence integration tests.
- Pulumi CLI 3.257.0 for Pulumi adapter integration tests.

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
LIFTR_TOKEN=dev go run ./cmd/liftr operation get op-xxxx
LIFTR_TOKEN=dev go run ./cmd/liftr resource update orders-db --spec new-spec.json --wait
LIFTR_TOKEN=dev go run ./cmd/liftr resource delete orders-db
```

The CLI defaults to `http://localhost:8080` (override with `--server` or `LIFTR_SERVER`; non-loopback servers require HTTPS). Tokens are read from files or the environment and never persisted; see [ADR-0013](docs/adr/0013-cli-as-a-public-api-client.md) for the credential-handling and exit-code contract.

Run all checks:

```sh
make verify
```

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
