# Liftr

Liftr is a vendor-neutral resource lifecycle control plane.

> Developers manage resources. Platform teams manage how those resources are implemented.

## What Liftr Aims to Become

Liftr aims to expose a stable, developer-facing Resource API while allowing platform teams to choose and evolve the infrastructure systems that implement those resources. Infrastructure technologies will integrate through adapters rather than appear in public Resource contracts.

The target architecture is described in [docs/architecture.md](docs/architecture.md). Architectural decisions are recorded in [docs/adr](docs/adr).

## What Exists Today

This repository is an initial bootstrap. It currently contains only:

- A Go HTTP server built with the standard library.
- A `GET /healthz` endpoint.
- Graceful shutdown on interrupt and termination signals.
- Structured JSON logging with `log/slog`.
- An initial provisioner-neutral core domain model.
- A deterministic, pure lifecycle engine for create, update, and delete semantics.
- A provider-neutral provisioning contract with a deterministic fake.
- An application/orchestration layer with persistence ports and stable private provisioner bindings.
- A non-provisioning PostgreSQLDatabase example ResourceType.
- Initial tests and continuous integration.

No real provisioner adapters, persistence implementations, background execution, authentication, authorization, infrastructure provisioning, or public Resource endpoints have been implemented yet.

## Getting Started

Requirements:

- Go 1.24 or newer.

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

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidance and [AGENTS.md](AGENTS.md) for the architectural constraints that govern changes.

## License

Liftr is licensed under the [Apache License 2.0](LICENSE).
