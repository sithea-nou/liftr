# Contributing to Liftr

Thank you for your interest in Liftr. The project is at an early stage, so keeping changes focused and architectural intent explicit is especially important.

## Development Setup

Install Go 1.24 or newer, then run:

```sh
make verify
```

The verification target checks formatting, runs `go vet`, and executes all tests.

## Making Changes

- Keep pull requests small and focused.
- Prefer the Go standard library unless an external dependency has a clear, documented benefit.
- Add or update tests for behavioral changes.
- Preserve provisioner-neutral public contracts and keep core domain logic independent of transport and infrastructure details.
- Read [AGENTS.md](AGENTS.md) before making architectural changes.
- Add an ADR under `docs/adr` for major architectural decisions.

## Pull Requests

Before opening a pull request:

```sh
make verify
```

Describe the problem, the chosen approach, relevant tradeoffs, and any follow-up work. Do not combine unrelated cleanup with functional changes.

## License

Liftr is licensed under the [Apache License 2.0](LICENSE). Unless explicitly stated otherwise, contributions submitted for inclusion in Liftr are provided under the same license.

## Reporting Issues

Include a concise description, reproduction steps when applicable, expected behavior, actual behavior, and relevant environment details. Security reporting guidance will be added before Liftr handles sensitive workloads.
