# ADR-0013: The CLI as a Public API Client

- Status: Accepted
- Date: 2026-08-23
- Refines: ADR-0001 (provisioner-neutral resource API), ADR-0008 (HTTP Resource API contract v1), ADR-0009 (ResourceType contracts and schema discovery), ADR-0012 (authentication, authorization, and actor identity)

## Context

Through Milestone 12's predecessor milestones Liftr gained a complete public surface: asynchronous Resource mutations with mandatory idempotency keys and concrete generation preconditions (ADR-0008), ResourceType discovery with self-contained JSON Schema contracts (ADR-0009), generation-bound non-secret outputs (ADR-0011), and RFC 9068 bearer-token authentication with owner-scoped authorization (ADR-0012). Developers can do everything over HTTP, but only by constructing requests by hand. The architecture positions CLIs and portals as clients of exactly this public API; M12 delivers the first official developer client without letting it become a second control plane.

## Decisions

### The CLI is a pure client of the public HTTP API

The dependency law is `cmd/liftr -> internal/cli -> internal/client -> public HTTP /v1`. The client package speaks the contract documented in `docs/openapi/v1/openapi.yaml` and imports nothing from `internal/application`, `internal/domain`, `internal/api`, or any other server implementation package — so the CLI structurally cannot bypass authentication, authorization, idempotency, concurrency semantics, or public representation boundaries. Architecture tests pin every forbidden import. The client maintains its own typed representations mirroring the published schemas; duplication is deliberate and pinned against the OpenAPI document by a drift test. Command parsing, rendering, waiting, and exit codes live in the separate command layer so both layers stay independently testable.

### Credentials are externally supplied; there is no login

The v1 CLI implements no OAuth/OIDC flows, browser handoffs, token minting, refresh, persistence, or profiles. It consumes an already-issued bearer access token from `--token-file`, `LIFTR_TOKEN_FILE`, then `LIFTR_TOKEN`, in that strict precedence. There is deliberately no `--token`: command lines leak through shell history and process listings. Token files must be regular files with trimmed non-empty contents within the server's 8 KiB credential ceiling; broad Unix permissions produce a warning. An invocation with no credential sends no Authorization header at all and lets the server decide, keeping explicitly insecure development composition usable without ceremony. Tokens stay memory-only for one invocation: never printed, logged, persisted, written to configuration, or included in errors — the client additionally redacts its own configured credential from rendered diagnostics, so even a hostile server echoing it inside Problem fields cannot make the terminal reprint it.

### HTTPS everywhere except syntactic loopback plaintext

A configured server address is an origin — scheme, host, effective port — and nothing else: paths, queries, fragments, and userinfo are refused, and reverse-proxy path-prefix hosting is deferred until Liftr defines an explicit base-path contract. Plaintext HTTP is accepted only for syntactic loopback hosts (`localhost`, 127.0.0.0/8, `[::1]`) regardless of whether a token is configured, so behavior never changes with credential presence; hostnames are never resolved to infer loopback membership. Redirects are refused outright, which removes both cross-origin credential forwarding and ambiguous base relocation in one rule. Response bodies are size-bounded; request documents mirror the server's 1 MiB bound locally.

### Server-supplied navigation is same-origin only

`Link rel="monitor"` and `Location` are untrusted input. Waiting resolves them against the configured origin and requires the effective scheme/host/port to match exactly and the path to identify one v1 Operation; anything else is refused before any authenticated request is issued, and an attacker-origin reference therefore produces zero outbound requests. Preference is the monitor Link, with Location consulted only when no monitor entry exists at all; a present-but-invalid entry never falls back to Location. There is no fallback to `latestOperation` — on an idempotency replay it may belong to a newer request — so missing or malformed monitor metadata under `--wait` is an explicit protocol failure.

### Generic, ResourceType-neutral commands with JSON-only input

Commands address Resources generically (`resource-type list|get`, `resource create|get|update|delete`, `operation get`, `version`); no per-ResourceType flags exist anywhere. Create input is either the exact create document (`-f FILE|-`) or assembly flags with a spec file; update input is the complete replacement spec object, because v1 performs full replacement and the CLI does not pretend PATCH exists. Input is JSON only: numeric literal fidelity is part of admission identity (`20` and `20.0` fingerprint distinctly per ADR-0008/ADR-0009), and YAML's implicit typing could silently rewrite intent. Specs are spliced verbatim into envelopes, successful responses keep bounded raw bytes alongside typed views, `-o json` emits those bytes untouched, and no local schema validation authority competes with the server. YAML support requires a future explicit semantic decision.

### Idempotency and concurrency ergonomics that preserve server semantics

Every mutation invocation mints exactly one cryptographically random key unless `--idempotency-key` is given; internal transport retries resend byte-identical bodies under the same key and never mint replacements. Keys are identifiers, not credentials: none are persisted, and when a mutation ends in an ambiguous transport failure the key is printed on stderr with explicit replay instructions. Updates default to read-then-precondition using the fetched generation, with `--generation` skipping the pre-read for automation; GENERATION_CONFLICT is surfaced with `currentGeneration` and never auto-resolved by refetching, because silently advancing the precondition would overwrite another actor's intent. Deletes always display the target first: interactive sessions type the exact Resource ID to confirm, non-interactive use requires `--yes`, and stdin being unavailable never becomes silent consent.

### Authoritative Operation waiting with honest outcomes

Without `--wait`, mutations print the admitted Resource snapshot and return. With `--wait`, the CLI polls the authoritative monitor Operation until `Succeeded`, `Failed`, `Canceled`, or timeout, with modest fixed-interval polling, bounded consecutive-failure tolerance, cancellation, and authentication failures ending the wait immediately. Terminal semantics are pinned: Operation success followed by a failing final Resource read is a command/read failure (exit 1, or 3 for authentication) — never "operation failed" — and no stale snapshot is emitted as final; `Failed`/`Canceled`/timeout exit distinctly; an unexpectedly absent Operation is a protocol failure. JSON mode writes exactly one verbatim document to stdout with all diagnostics on stderr.

### Stable exit codes instead of per-Problem proliferation

Documented and ordered: 0 success, 1 generic client/network/protocol failure or user abort, 2 usage/configuration/input, 3 authentication, 4 API-rejected request, 5 admitted Operation failed/canceled/timed out, 130 interrupted. Hidden not-found Problems render exactly as served; request IDs accompany every API error for support correlation; hostile strings are sanitized before terminal rendering.

## Consequences

- Developers get first-class tooling while every guarantee continues to live in exactly one place: the server.
- The CLI and the reusable client package give future clients (Backstage plugins, scripts) a hardened reference implementation of credential handling, same-origin navigation, replay recovery, and JSON fidelity.
- Some friction is accepted deliberately: no path-prefixed servers, no YAML, no wildcard generations, no silent conflict resolution, confirmation typing on interactive deletes.
- The drift test makes the duplicated representations safe; adding or changing a documented field without updating the client fails CI.

## Scope Exclusions

OAuth login flows, browser handoffs, refresh tokens, credential persistence, config files/profiles/contexts, telemetry, TUI/color, shell-completion support promises, YAML input, local PATCH/merge semantics, Resource/Operation lists, retry endpoints, secret resolution, Backstage integration, and server-side watch/streaming remain out of scope.

## Alternatives Considered

### Reusing Server Transport Types in the Client

Importing `internal/api/http` DTOs would drag application and domain packages across the boundary and couple the client to implementation evolution. Rejected; duplication plus a drift test keeps the OpenAPI document authoritative.

### Warning Instead of Refusing Non-Loopback Plaintext HTTP

A warning still transmits the credential after one miskeyed flag. Rejected in favor of refusing outright, independent of credential presence.

### Trusting Server Links After Scheme/Host Checks Alone

Effective-port normalization matters: `https://host` and `https://host:443` are one origin, other ports are not. Origin comparison includes normalized ports, and monitor references must additionally name a v1 Operation path.

### Falling Back to latestOperation for Waiting

Replay responses legitimately carry newer operations in snapshots; treating them as the mutation's result would report the wrong execution. Rejected; absence of the authoritative reference is a failure.

### Per-Problem Exit Codes

One code per approved Problem code would churn with every vocabulary addition and provide scripting value only marginally above four coarse classes. Rejected.

### Local Spec Validation Before Admission

Duplicating JSON Schema semantics client-side risks diverging on exactly the numeric-literal distinctions the fingerprint depends on. Rejected; discovery commands exist precisely so external tools can build on machine-readable contracts.

### YAML Input

Implicit typing, float normalization, and timestamp coercion would rewrite developer intent whose literal form is significant. Deferred to an explicit future decision.
