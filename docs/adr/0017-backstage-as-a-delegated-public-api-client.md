# ADR-0017: Backstage as a Delegated Public API Client

- Status: Accepted
- Date: 2026-08-24
- Refines: ADR-0001 (provisioner-neutral resource API), ADR-0008 (HTTP Resource API contract v1), ADR-0012 (authentication, authorization, and actor identity), ADR-0013 (CLI as a public API client), ADR-0016 (ownership-scoped resource inventory)

## Context

ADR-0012 anticipated portals as public-API clients, and the architecture names Backstage among them. Milestone 16 delivers that integration. The governing constraints are unchanged: Liftr remains the authoritative lifecycle and authorization system; provisioner and portal internals must not leak into Resource contracts; audit identity must record the acting developer.

## Decisions

### Topology: constrained BFF over the public API only

Backstage frontend → `liftr` backend plugin (BFF) → Liftr public `/v1` API. The BFF mirrors an explicit finite route set; it is not a general proxy, holds no Resource/Operation state, makes no authorization decisions, persists nothing. Direct browser-to-Liftr access was rejected for M16 (CORS support would be a server change; bearer tokens in portal JS widen XSS blast radius). Generic proxy plugins were rejected: they cannot mint per-user third-party credentials.

### Backstage tokens are never Liftr tokens

Backstage user/plugin JWTs (`vnd.backstage.*`) satisfy nothing in M11's RFC 9068 profile and are never forwarded upstream. Inbound Authorization/cookies are stripped before any upstream construction.

### User-delegated credentials with mandatory subject binding

Interactive flows carry user-delegated Liftr credentials acquired per request behind a deliberately narrow `LiftrCredentialProvider` (token exchange / passthrough / insecure development — no general OAuth framework). Reference mode performs RFC 8693 exchange at a configured STS: OAuth client authentication via confidential client credentials, `subject_token` = user assertion, `audience`/`resource` = Liftr, `actor_token` only when deployment supplies a real actor token (client secrets are never synthesized into actor tokens).

The delegation subject MUST be bound to the authenticated Backstage principal through trusted configuration (IdP-issued entity-ref claim or explicit static map) before use, and the exchanged token's issuer+subject are re-verified against the binding afterwards. Mismatches, malformed assertions, and unavailable bindings fail closed before any Liftr call. No email or display-name matching exists. Passthrough deployments may declare the Liftr token authoritative but must not claim verified sameness with the Backstage principal.

The resulting token must pass all of M11 unchanged — typing, algorithms, exact issuer, Liftr audience, bounded claims, and membership claims mapped by operator configuration. An IdP incapable of this is an unsupported deployment, not a reason to weaken validation.

### Subject vs actor

Liftr Principals continue to identify developers (`prn_v1_(issuer, sub)`); the BFF integration is identified by token `client_id`, which Liftr does not authorize against. `act` parsing is unnecessary in M16. Audit Events therefore stamp the developer PrincipalID even when mutations traverse Backstage — acceptance-tested by proving two developers cannot collide under one key.

### Typed authoritative monitor propagation

Because the BFF strips navigation headers from browser responses, every successful mutation response uses one stable envelope `{data, monitorOperationId}` shared by backend, common package, and frontend. `monitorOperationId` derives exclusively from the validated `Link rel="monitor"` reference (pinned origin, `/v1/operations/{id}` shape, redirects refused). Missing or hostile references are protocol failures — never guesses, never `latestOperation`, never unvalidated Location (create's Location identifies a Resource and is categorically excluded).

### Idempotency and concurrency preserved

Keys remain opaque identifiers validated exactly as Liftr does (non-empty after trimming) and forwarded byte-for-byte; UUIDs are a frontend convention only, never a contract. One key per logical action across internal transport retries; ambiguous transport outcomes surface to users who may replay the same key deliberately; new actions mint new keys; keys appear in no URL or durable store. Mutations are never automatically replayed after authentication refresh — idempotency namespaces are `(PrincipalID, key)` and refreshed credentials must not silently switch them mid-action. Reads/polling may reacquire once on 401 because Liftr re-authorizes every request.

### Numeric fidelity

Spec editing is JSON-editor-first. Specs travel as text spliced verbatim into envelopes; payloads parse through a pinned lossless-number layer so `20` and `20.0` keep their distinct admission identities end to end. Schema-generated numeric forms are deferred until representation-safe editing exists. Server-side 422 violations remain authoritative and render with pointers.

### Catalog relationship

The Software Catalog is not Resource inventory: no synchronization, no shadow entities, `GET /v1/resources` stays authoritative. Catalog ownership informs presentation only — display labels and optional links via operator mapping. Requests are byte-identical regardless of mapping or catalog contents; catalog claims never widen Liftr visibility. Owner suggestions in create flows are UX conveniences whose denial surfaces Liftr's honest 403.

### Scope boundaries recorded

Scaffolder actions, service-account automation, secret resolution, watch/SSE APIs, owner-discovery endpoints, admin inventory, and permission-framework mirroring of Liftr policy are out of scope; several have sketched future boundaries here so they arrive as deliberate decisions rather than drift. Development composition requires explicit insecure flags plus literal-loopback targets and can never auto-fallback.

## Consequences

- Developers manage Liftr resources through Backstage while every guarantee — authorization, lifecycle policy, idempotency, concurrency, audit — lives in exactly one place: Liftr.
- Zero changes to Liftr core, public OpenAPI, or authentication semantics were required.
- The TS client duplicates the published contract deliberately; a drift test parses the OpenAPI document itself and fails CI when shapes diverge.
- Node tooling enters the repository behind `make verify-backstage`; `make verify` remains Go-only, and CI runs both as independent jobs.
- Delegation quality depends on deployment IdP capability (RFC 8693 conformance, group-claim preservation, subject stability across clients). These are documented obligations; failures manifest as honest denials, not weakened checks.

## Alternatives Considered

### Trusting Backstage-issued JWTs at Liftr

Would couple the control plane to portal internals, break profile pinning, and make portal token format changes a Liftr compatibility burden. Rejected without an extraordinary justification none exists today.

### Shared privileged service account

Destroys audit identity (Events would stamp "backstage"), collapses per-developer idempotency namespaces, and turns the portal into the authorizer. Rejected as a user-flow; machine automation remains a future milestone with its own design.

### Browser-to-Liftr direct access

Requires CORS (a Liftr change), keeps delegated bearers in portal JS memory, and duplicates hardening client-side. Deferred unless a future CORS+PKCE decision revisits it explicitly.

### Catalog synchronization / Scaffolder now

Both add second-control-plane pressure before the interactive plugin proves the delegation model. Recorded as future work requiring their own decisions.

## Scope Exclusions

UI component-library choices, styling, and editor widget selection are implementation details intentionally absent from this record.
