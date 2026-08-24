# Authentication: Backstage ↔ Liftr

## Token classes (never conflate them)

| Class | Issuer/audience | Valid at Liftr? |
| --- | --- | --- |
| Backstage user token (`vnd.backstage.user`) | Backstage core services | **No** |
| Backstage plugin token (`vnd.backstage.plugin`, `sub=<src>`, `aud=<dst>`) | plugin-to-plugin only | **No** |
| Upstream delegation assertion (IdP access/id token) | org IdP | Only as exchange input |
| Liftr access token (RFC 9068, `typ=at+jwt`, Liftr audience) | configured Liftr issuer | **Yes** |

The BFF strips inbound Authorization and cookies; the Liftr Authorization
header is generated exclusively by the credential provider.

## Bound-user mode (reference, `mode: delegated`)

Two separate trust inputs are bound before any Liftr call:

```
authenticated Backstage principal (userEntityRef)
        +
verified delegation assertion issuer+subject
        ↓  configured binding (claim or static)
same user OR fail closed
```

* `claim` strategy — the trusted IdP embeds the Backstage entity ref in a
  dedicated assertion claim. Binding rests on trusted sign-in resolution;
  never on email/display-name matching.
* `static` strategy — explicit operator map of `backstageRef → {issuer,
  subject}`.
* After exchange, the issued token's `iss`+`sub` are re-verified against the
  bound identity; mismatches fail closed even if the STS misbehaves.
* `strategy: none` is invalid in delegated mode.

Fail-closed matrix: Backstage A + delegation B ⇒ rejected pre-exchange;
malformed assertion ⇒ rejected; unknown ref ⇒ rejected. Rejections happen
before any Liftr request, so no mutation can occur under another identity.

RFC 8693 terminology: OAuth client authentication (clientId/clientSecret) is
**separate** from `actor_token`. The BFF authenticates as a confidential
client and sends `subject_token` = user assertion plus `audience`/`resource`
per STS support. `actor_token` is sent only when a deployment configures a
real actor-token provider; a client secret is never synthesized into one.

Deployment obligations: exchanged tokens must carry RFC 9068 typing, allowed
algorithms, exact Liftr issuer/audience, non-empty `client_id`/`jti`, and
membership claims mapped for `LIFTR_AUTH_GROUP_*` — M11 validation is
unchanged and non-negotiable.

## Passthrough mode (`mode: passthrough`)

For IdPs that issue Liftr-audience RFC 9068 tokens directly to the SPA. The
BFF performs hygiene checks only and forwards; **Liftr remains the sole
validator**. This mode does NOT verify that the Backstage principal equals the
Liftr principal; the token is authoritative. Logs and audit narratives must
not claim bound-user equivalence in this mode.

## Authentication-failure policy (asymmetric by design)

* Reads/polling: on Liftr 401 the BFF may reacquire once and retry once;
  authorization is re-evaluated server-side every request, so revocation takes
  effect immediately.
* Mutations: NO automatic replay after any HTTP response, including 401. The
  failure surfaces to the UI; the user re-establishes auth and deliberately
  retries. Idempotency namespaces are `(PrincipalID, key)` — a refreshed
  credential must never silently change them mid-action.
* Connection-level ambiguity on mutations yields `outcomeUnknown`; the UI may
  replay the SAME key with the SAME bytes by explicit choice.

## Storage discipline

Delegated tokens live in handler-scope memory only: no database, catalog,
localStorage/sessionStorage, URLs, task parameters, logs, errors, or Resource
fields. Log events carry outcome categories and correlation/request IDs —
never credential material.
