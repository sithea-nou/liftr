# Liftr Engineering Guidance

This file defines the architectural guardrails for contributors and coding agents working in this repository.

## Architectural Principles

1. Liftr is resource-first, not stack-first.
2. Public APIs are provisioner-neutral.
3. Pulumi, OpenTofu, Terraform, Crossplane, Git, CI/CD, and cloud implementation details must not leak into developer-facing Resource contracts.
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

The repository contains the core domain model, pure lifecycle semantics,
provider-neutral provisioning contract, application orchestration, durable
PostgreSQL execution, Pulumi and Crossplane adapter foundations, and the M19
stateful OpenTofu CLI package and private PostgreSQL evidence/quarantine
foundation. The Crossplane adapter operates on platform-owned XRs (ADR-0015),
with all Kubernetes knowledge confined to its adapter subtree. The OpenTofu
adapter is scoped exactly to OpenTofu 1.12.6 and implements one stable backend
state key, fenced evidence, saved-plan apply semantics, bounded private output
handling, Unix process-tree cancellation, disposable per-call scratch, and
quarantine. Its production profile is an operator-supplied conformant HTTPS
HTTP state backend; local and test HTTP backends are development/test-only.

Production server composition is optional through one strict, immutable
operator-supplied `LIFTR_OPENTOFU_CONFIG_FILE`; absent that file, Pulumi remains
the unchanged default. Liftr ships no production OpenTofu cloud program or
backend registration. Qualification is complete against a real official
OpenTofu 1.12.6 binary and the conformant test HTTP backend; qualification of
each production HTTPS HTTP backend remains the operator's responsibility.
The file can retain multiple immutable registrations and selects current
ResourceType routes separately, so old refs remain resolvable for bound
Resources. Operators must retain each exact provisioner ref and executable,
source, program, and backend identity rather than changing configuration beneath
a ref. Terraform has no selected version or support claim. The public API
remains unchanged.

The repository also contains a versioned public HTTP Resource API with JSON
Schema draft 2020-12 ResourceType discovery, ownership-scoped inventory with
`resource:list` authorization and visibility-bound keyset pagination
(ADR-0016), and the official `liftr` CLI as a pure public-API client
(`cmd/liftr` -> `internal/cli` -> `internal/client`, ADR-0013). The official
Backstage integration under `integrations/backstage` consumes only `/v1` with
user-delegated credentials (ADR-0017). Production observability follows
ADR-0018, and immutable platform admission policy with transactionally
serialized per-owner quotas follows ADR-0019.

`liftr-server` has a ticker-driven continuously running outbox worker loop when
durable runtime composition is enabled; API and worker execution remain
independently deployable, but there is no separate worker executable yet.
Telemetry stays out of lifecycle packages and never participates in durable
decisions. Policy remains a restrictive application overlay and never enters
ResourceType or provisioner contracts. There are no production cloud provider
compositions or cloud-specific public ResourceTypes beyond private reference
bindings, and core Go verification has no Node dependency. Add capabilities
incrementally, with tests, and avoid introducing implementation technologies
before a concrete milestone requires them.

## Development Expectations

- Prefer small, reviewable changes.
- Prefer the Go standard library when it is sufficient.
- Keep package boundaries driven by actual domain needs.
- Run `make verify` before considering a change complete.
- Update documentation when behavior or architectural intent changes.
