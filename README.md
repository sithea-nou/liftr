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
- A non-provisioning PostgreSQLDatabase example ResourceType.
- Initial tests and continuous integration.

## Future Direction

No cloud-specific ResourceTypes, production Pulumi programs, authentication, authorization, or public Resource endpoints have been implemented yet. The target architecture is described in [docs/architecture.md](docs/architecture.md), and the decisions that shape future work are recorded in [docs/adr](docs/adr).

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
LIFTR_TEST_PULUMI_ROOT="$HOME/.pulumi" go test ./internal/provisioning/pulumi -count=1
```

Apply migrations explicitly with:

```sh
LIFTR_DATABASE_URL='postgres://liftr:liftr@localhost:55432/liftr?sslmode=disable' go run ./cmd/liftr-migrate
```

Production server startup does not apply migrations automatically.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidance and [AGENTS.md](AGENTS.md) for the architectural constraints that govern changes.

## License

Liftr is licensed under the [Apache License 2.0](LICENSE).
