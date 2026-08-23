# Liftr Engineering Guidance

This file defines the architectural guardrails for contributors and coding agents working in this repository.

## Architectural Principles

1. Liftr is resource-first, not stack-first.
2. Public APIs are provisioner-neutral.
3. Pulumi, Terraform, Crossplane, Git, CI/CD, and cloud implementation details must not leak into developer-facing Resource contracts.
4. Backstage is a client, not part of Liftr core.
5. Git is optional.
6. Kubernetes is optional.
7. Cloud providers are implementation details.
8. Desired state and observed state are separate concepts.
9. Lifecycle orchestration belongs to Liftr.
10. Provisioner adapters execute infrastructure capabilities but do not own business policy.
11. Long-running infrastructure operations are asynchronous.
12. Lifecycle mutations must eventually produce auditable Operations and Events.
13. Core domain logic must remain independent from HTTP, storage, and provisioner implementations.
14. Major architectural changes require an ADR.
15. Avoid premature abstractions.

## Current Scope

The repository contains the core domain model, pure lifecycle semantics, provider-neutral provisioning contract, application orchestration, durable PostgreSQL execution, a Pulumi Automation API adapter foundation, a versioned public HTTP Resource API with ResourceType discovery backed by JSON Schema (draft 2020-12) contracts, and the official `liftr` CLI as a pure client of that public API (`cmd/liftr` -> `internal/cli` -> `internal/client`, ADR-0013). It has no production Pulumi programs, cloud-specific ResourceTypes, or continuously running worker process. Add capabilities incrementally, with tests, and avoid introducing implementation technologies before a concrete milestone requires them.

## Development Expectations

- Prefer small, reviewable changes.
- Prefer the Go standard library when it is sufficient.
- Keep package boundaries driven by actual domain needs.
- Run `make verify` before considering a change complete.
- Update documentation when behavior or architectural intent changes.
