# ADR-0012: Authentication, Authorization, and Actor Identity

- Status: Accepted
- Date: 2026-08-23
- Refines: ADR-0001 (provisioner-neutral public API), ADR-0005 (application boundary), ADR-0008 (HTTP contract v1), ADR-0011 (secret-reference independence)
- Amends: ADR-0009 admission ordering (authorization inserted before replay)

## Context

Through Milestone 10 every network-reachable caller could read Resources, non-secret ResourceOutputs, create, update, and delete infrastructure, and inspect Operations. Network isolation was the only access boundary, as ADR-0011 recorded. Liftr now needs provider-neutral authentication and authorization suitable for humans, service accounts, future CLI clients, Backstage integration, and the reserved SecretReference semantics — without coupling the domain to any identity provider.

## Decisions

### Explicit access-token profile (RFC 9068)

Liftr verifies bearer credentials against the RFC 9068 JWT Profile for OAuth 2.0 Access Tokens. OIDC does not define an access-token format, and audience membership cannot distinguish an access token from an ID Token, so token typing is mandatory:

- `typ` must be `at+jwt` or `application/at+jwt` (case-insensitive).
- Missing typing, `JWT`, ID-token typing, or arbitrary JWT types are refused even when the signature is valid and the audience matches. Cross-JWT confusion is defeated by explicit profile enforcement, not by inference.
- `alg: none` is always refused; HMAC algorithms are not implemented; accepted algorithms are a configured allowlist over {RS256, PS256, ES256} with RS256 default.
- Required claims per the selected profile: exact issuer match, Liftr API audience contained in `aud`, `exp`, `iat`, non-empty `sub`, non-empty `client_id`, and non-empty length-bounded `jti`; `nbf` is enforced when present. Bounded clock skew applies. `jti` is profile validation only — Liftr keeps no replay cache, persists nothing about it, and never carries it into principals, events, problems, or logs.
- The transport enforces an explicit conservative ceiling of 8 KiB on a single bearer credential before any token processing, so oversized credentials cost no parsing or JWKS work; the exact credential length never appears in errors or logs.
- An identity provider that cannot issue RFC 9068-compatible tokens is unsupported by M11; validation is never silently weakened for compatibility. Future vendor profiles require an explicit decision.
- Issuer, discovery, and JWKS URLs must be HTTPS in secured runtimes. Metadata fetching is bounded: short timeout, response-size caps, no redirects, capped key counts. JWKS keys cache with a TTL; an unknown `kid` triggers one rate-limited refetch so forged kid floods cannot loop.
- The JWT library is an implementation detail deliberately not recorded here, mirroring ADR-0009's posture toward the JSON Schema library.

### Principal identity

Authentication produces one normalized principal: opaque stable ID, kind (`user` | `serviceAccount` | `system`), issuer, subject, typed memberships, and method label. Raw claims never cross the authenticator boundary.

Identity is issuer-qualified: stable identity is issuer + subject. Email addresses are never authorization identities. Because PrincipalID scopes idempotency namespaces and carries durable actor attribution, its derivation is versioned:

```text
prn_v1_<hex(sha256(length-prefixed(issuer) || length-prefixed(subject)))>
```

The length-prefixed encoding makes delimiter ambiguity impossible. Any encoding or algorithm change requires a new `prn_vN_` prefix; v1 values are never reinterpreted.

### Typed owner memberships

Normalized memberships are `[]domain.OwnerRef`, never strings. Claim mapping accepts deployment conventions such as `liftr:team:payments` (configurable claim name, configurable prefix strip), but string encodings terminate at the mapper: authorization compares owner references structurally. Optional static grants map subjects or raw group values directly onto typed OwnerRefs for IdPs whose identifiers must stay private. Claim processing is bounded (entry count, entry length, total bytes, expected JSON array-of-string shape) and deterministic (sorted, deduplicated); malformed or unmapped entries grant nothing. Raw group claims are never persisted.

### HTTP authenticates; application authorizes

The transport extracts the bearer credential, verifies it through an Authenticator port, constructs the principal on the request context, and answers 401 — it makes no policy decisions. Every exported business use case authorizes through an application-owned `Authorizer` port (`Authorize(ctx, principal, action, target)`), so no other transport can bypass policy and workers can deliberately skip re-authorization. The neutral vocabulary lives in `internal/identity` (imports domain + stdlib only); concrete implementations in `internal/auth` satisfy application's port structurally; architecture tests pin all dependency directions.

Actions: `resource:create|read|update|delete`, `resourceType:read`. `secret:resolve` is reserved and independently authorized when SecretReferences arrive; it will never inherit resource-read permission. Operation reads authorize `resource:read` through the owning Resource — operations have no independent ACL. Discovery is authenticated-global: schemas reveal capabilities, not secrets, so every authenticated principal holds `resourceType:read`.

The initial policy is the owner-membership authorizer: resource actions require an exact structural membership under the target owner; discovery requires authentication only. Deny-by-default everywhere, including unknown actions.

### OwnerRef semantics

Create requests still carry client-proposed `owner`; automation acting on behalf of teams remains possible. Assignment is checked, not claimed: admission authorizes `resource:create` against the requested OwnerRef, and mutations authorize against the stored owner. Server-side derivation from caller identity alone was rejected as too restrictive.

### Authorization ordering

Create: authenticate → structural parsing → authorize requested type+owner → principal-scoped idempotency replay → catalog → contract validation → admission. Unauthorized creates answer 403 before any catalog or idempotency state is consulted, so mutation errors cannot probe the catalog or burn keys.

Update/delete: authenticate → structural parsing → load stored Resource → authorize stored owner → then Idempotency-Key and If-Liftr-Generation requirements → then replay, active-operation checks, lifecycle legality, and contract validation inside the admission transaction. Replay resolution precedes generation comparison per ADR-0008/ADR-0010 replay precedence; authorization precedes both. A denied caller therefore learns nothing about generation, operation activity, state, or outputs through conflict responses.

Reads: Resource reads load then authorize against the stored owner; Operation reads resolve the owning Resource and authorize identically — Operations have no independent ACL. ResourceType discovery is authenticated but globally readable: schemas reveal platform capability, never secrets or provider configuration.

### Hidden 404 versus honest 403

Forbidden Resource reads, updates, deletes, and Operation reads render byte-equivalent "not found" Problems to true absence: IDs are client-chosen and guessable, outputs carry hostnames, and conflict paths expose `currentGeneration`, so a 403 oracle would enable enumeration and probing. Forbidden creates answer 403 FORBIDDEN because nothing exists to hide; forbidden discovery answers 403. This asymmetry is deliberate and pinned by tests.

### Per-principal idempotency scope

Idempotency uniqueness moves from the global key to `(PrincipalID, Key)` using the persistence layer's existing `scope` dimension. Possession of an Idempotency-Key — like possession of a ResourceID, OperationID, or a future secret reference — is never authorization. Two principals sharing one key hold independent namespaces; one principal can never retrieve another's recorded result. Records persisted before M11 retain the legacy `control-plane` scope, which no PrincipalID can equal; they are retired in place rather than migrated. Fingerprints remain purely content-derived; scoping already partitions the keyspace.

### Admission-time authorization and worker continuity

Authorization is admission-time policy. Once a lifecycle request is durably admitted, worker execution drives, dispatches, observes, retries attempts, and completes without consulting the authorizer — regardless of later token expiry, membership revocation, or authorizer/IdP unavailability. Durable reconciliation must never become identity-provider dependent. Worker-facing execution paths are structurally excluded from the authorizing use cases, and tests pin that admission consults policy exactly once while worker loops never do.

### Actor audit

Admitted user mutations stamp their lifecycle Event with deliberately selected normalized fields: PrincipalID and principal kind. Issuer/subject remain derivable at authentication time and are not persisted in events, keeping PII minimal; raw subject was evaluated and deemed unnecessary for M11. Access tokens, Authorization headers, raw claims, memberships, and signing material are never persisted anywhere — Events, execution records, outbox payloads, logs, or Problems. Replays append no additional lifecycle Event; denials are structured transport/application log lines, not Events. Internal transitions stay unattributed system actions, distinguishable by their reserved internal event-ID namespace.

### Secure default and one insecure mode

The full runtime refuses to start without authentication configuration; missing configuration never silently degrades into open access. Exactly one development mode exists: explicit `LIFTR_AUTH_MODE=insecure` composes a fixed development principal with allow-all authorization and logs a prominent warning. No static-token production scheme ships merely for testing; tests inject fake authenticators and authorizers directly. Health and readiness remain unauthenticated, and readiness never depends on IdP availability after successful bootstrap — cached signing keys keep validating within the defined cache policy during temporary IdP outages.

## Consequences

- Developers and automation authenticate once per call with standards-conformant access tokens and act within owner-scoped permissions.
- The control plane can durably answer "who admitted this change?" and "may this caller touch this Resource?" without embedding any vendor's identity model.
- Operators must run an RFC 9068-capable issuer and map group claims into the `kind:id` convention or static grants.
- Legitimate-but-unauthorized callers receive indistinguishable 404s; troubleshooting relies on request-ID-correlated server audit logs.
- Pre-M11 anonymous-era idempotency records become unreachable.
- Future milestones inherit: versioned PrincipalID derivation, an open Action enum (with `secret:resolve` reserved and independently authorized), and an Authorizer port replaceable beyond owner-membership policy.

## Scope Exclusions

CLI, browser login flows, OAuth authorization-code flows, UI, multi-issuer federation, tenant management, group management, policy languages and external policy engines, API keys, mTLS, SCIM, token minting or refresh, vendor-specific token compatibility fallbacks, secret resolution, and admin consoles remain out of scope.

## Alternatives Considered

### Generic OIDC JWT Validation with Audience Matching Only

Audience membership cannot distinguish ID Tokens — which also carry `aud` — from access tokens, so a validly signed ID Token naming the Liftr audience would authenticate. Explicit RFC 9068 typing was selected instead.

### Transport-Only Middleware Authorization

Any non-HTTP driver of the application core would bypass policy entirely. Rejected; the application owns authorization, the transport owns protocol.

### Deriving Ownership From Caller Identity

Would forbid automation acting for teams. Rejected in favor of proposed-owner-plus-check.

### Global Idempotency Keys Retained

One principal could replay another's result by learning a key. Rejected; scoping is the security fix of this milestone.

### Kubernetes-Style 403 On Forbidden Reads

Preserves existence oracles over guessable IDs. Rejected in favor of hidden 404.

### Static Development Tokens As A Shipped Protocol

A second credential format invites production misuse and test-only surface area. Rejected; explicit insecure mode plus injected fakes cover development deterministically.

### Unversioned Principal Hash

Changing derivation would silently reinterpret existing scoped records. Rejected; `prn_v1_` pins the scheme forever.

### String Memberships With Delimiter Encoding

Delimiter collisions could authorize the wrong owner. Rejected; typed OwnerRefs compare structurally.

### Policy Engine Integration Now

No requirement exceeds owner-membership expressiveness. Deferred behind the Authorizer port.
