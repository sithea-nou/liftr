# ADR-0021: Operator Diagnostics and Safe Recovery Control Plane

Date: 2026-08-25
Status: Accepted

- Refines: ADR-0005 (application orchestration), ADR-0006 (PostgreSQL and
  transactional outbox), ADR-0012 (authentication and authorization), ADR-0014
  (explicit developer retry), ADR-0018 (bounded observability), ADR-0020
  (private OpenTofu evidence)

## Context

Liftr already detects long-running Operations, reconciliation silence, expired
leases, and Dead outbox work. Those signals deliberately contain bounded
labels, so they can identify a class of failure without exposing enough durable
state to decide whether intervention is safe. The public Resource API is also
the wrong place for platform-wide diagnostics and recovery authority.

Operators need a narrow way to inspect curated durable facts, request a fresh
observation, and recover a limited set of Dead work. That facility must not
become a general database editor, provisioner console, retry bypass, or force
control. In particular, it cannot replay an old Dispatch payload or infer that
an uncertain provider mutation did not happen.

M20 adds a separate, authenticated operator control plane. It retains Liftr's
resource-first public contract, conservative ambiguity rules, transactional
outbox, and independent developer authorization model.

## Decision

### A separate, optional listener

`LIFTR_ADMIN_ADDR` enables a separate admin HTTP listener. It is disabled when
the variable is empty, requires the full durable PostgreSQL runtime, and cannot
be enabled in health-only mode. The public listener registers `/v1` and its own
health probes; the admin listener registers only `/admin/v1` plus its own
unauthenticated `/healthz` and `/readyz`. The routers are intentionally
disjoint. Enabling the admin listener does not add an admin route to the public
address.

Admin liveness means the listener process can answer. Admin readiness also
requires reachable durable state and returns `ADMIN_DRAINING` after shutdown
starts. The listener must be placed on a restricted operator network even
though its API routes are authenticated.

Secured composition requires:

- `LIFTR_ADMIN_AUTH_AUDIENCE`, which must differ from
  `LIFTR_AUTH_AUDIENCE`;
- `LIFTR_ADMIN_AUTH_GRANTS_FILE`, a strict static grants document;
- optional `LIFTR_ADMIN_AUTH_ISSUER`, defaulting to `LIFTR_AUTH_ISSUER`;
- optional `LIFTR_ADMIN_AUTH_ALGORITHMS`, defaulting to the public algorithms;
- optional `LIFTR_ADMIN_AUTH_KIND_CLAIM`, defaulting to the public kind claim.

Required-audience validation uses JWT audience membership semantics. A token
whose audience claim contains both configured audiences can authenticate on
both listeners. This is deliberate and does not join authorization: public
owner grants and operator grants remain separate decisions with disjoint action
vocabularies. An API-only or admin-only token fails on the opposite listener.

`LIFTR_AUTH_MODE=insecure` is one explicit development-only switch for both
listeners. It composes fixed insecure authentication and allow-all
authorization on both; there is no independently insecure admin mode and no
fallback from incomplete secured configuration.

### Deny-by-default operator authorization

The closed operator permission vocabulary is:

- `operator:diagnostics:read`
- `operator:observation:trigger`
- `operator:work:recover`

The required grants file has this shape:

```json
{
  "subjects": {
    "operator-subject": [
      "operator:diagnostics:read",
      "operator:observation:trigger",
      "operator:work:recover"
    ]
  }
}
```

It is parsed strictly at startup. Missing or empty grants, unknown fields,
unknown permissions, duplicate permissions, and non-canonical subjects fail
composition. Unlisted subjects and actions are denied. M20 grants subjects
only; operator group mapping is not implied.

Authentication and authorization occur before target or idempotency lookup.
Mutations repeat authorization inside the transaction before making a durable
decision. A diagnostic recommendation, ETag, target ID, or previously accepted
idempotency key is never authorization.

### Curated diagnostics

The admin contract defines:

- `GET /admin/v1/resources/{id}/diagnostics`
- `GET /admin/v1/operations/{id}/diagnostics`
- `GET /admin/v1/work/{id}/diagnostics`

Responses carry `Cache-Control: no-store` and a stable quoted `ETag`. They
project IDs, lifecycle and work state, timestamps and ages, bounded failure or
terminal-reason classes, registration availability, and the pure planner's
current recovery assessment. Resource diagnostics may include the SHA-256 of
the exact stored desired-state bytes. When a private OpenTofu state identity
exists, the response may include its identifiers, a lineage-presence boolean,
serial, evidence version, and only the first eight digest bytes as a 16-character
hex prefix.

The projection never contains a raw ResourceSpec, output or secret value, raw
provider diagnostic, outbox payload or last-error text, execution handle,
backend state bytes, state lineage value, full private state digest, plan,
environment, credential, bearer token, or idempotency key. `handlePresent` and
`LineagePresent` are booleans, not the underlying values. Private provisioner,
program, backend, and state-key identifiers are operator diagnostics and must
not be copied into public APIs or metric labels.

An ETag is diagnostic concurrency assistance, not a capability or proof of
continued safety. Read-only traffic leaves it stable, while meaningful durable
version changes alter it. Mutations always lock and reload current durable
state, rebuild the planner input, reauthorize, and decide again. `If-Match` is
optional; when supplied it must be the latest strong diagnostic ETag or the
request fails with `412 DIAGNOSTIC_STALE`.

### Narrow asynchronous mutations

The mutation endpoints are:

- `POST /admin/v1/operations/{id}/observe`
- `POST /admin/v1/resources/{id}/observe`
- `POST /admin/v1/work/{id}/recover`

Requests have an empty body. `Idempotency-Key` is required and is limited to
200 bytes. `If-Match` may carry the ETag from the corresponding latest
diagnostic. Accepted and replayed requests return `202` with:

```json
{
  "result": "applied",
  "action": "trigger_observe",
  "targetKind": "operation",
  "targetId": "op-123",
  "operatorActionId": "opact_...",
  "createdWorkId": "observe:op-123:4"
}
```

`result` is `applied` for first acceptance and `replayed` for idempotent replay.
A replay also returns `Idempotency-Replayed: true`. All actions schedule
exactly one new canonical outbox row and return immediately; workers perform
any later provider interaction.

Observation trigger and dead-work recovery do not force, retry, cancel, mark
terminal, change desired state or generation, edit state, resolve ambiguity by
declaration, or call a provider in the admission transaction.

### Pure recovery planning from current truth

The application-owned `RecoveryPlanner` is a pure function over a bounded
snapshot. It has no repository, HTTP, authorization, clock, policy, or
provisioner dependency. Diagnostics show its assessment, but mutation paths
rebuild that snapshot from current locked durable rows and rerun it. The client
cannot submit a decision.

An Operation observation trigger is allowed only when the Operation is active,
its current execution is in an observable state, no active equivalent Observe
exists, and the historical provisioner registration resolves. A Resource
passive observation is allowed only for a non-Deleted current Resource with no
active Operation, no active equivalent PassiveObserve, and an available
registration.

Dead rows are immutable evidence. Recovery never revives, edits, deletes, or
clones one and never reuses its lease, fence, or payload. It creates one fresh
work identity from current aggregate state:

| Dead kind | Safe M20 result |
| --- | --- |
| `Dispatch` | Never another Dispatch. Create Observe only when the current execution is safely observable. |
| `Observe` | Create a fresh Observe only while the current execution remains observable. |
| `PassiveObserve` | Create a fresh PassiveObserve for the current non-Deleted Resource. |
| `Drive` | Create a fresh Drive from current Operation state with the decision-free `{}` payload. |

Terminal or superseded targets, active equivalent work, a missing execution,
an unavailable historical registration, pre-submit dead Dispatch with no
observable execution, and any unsupported or unsafe ambiguity are refused.
Ordinary expired leases continue to use normal worker recovery; M20 is not a
replacement for fencing or lease expiry.

### Transactional audit and idempotency

An accepted mutation transaction atomically creates exactly one outbox row,
one immutable `operator_actions` row, and one principal-scoped
`operator_idempotency` binding. `created_work_id` is a required typed foreign
key to the new outbox row. `source_work_id` is present only for dead-work
recovery and points to its immutable source row. One audit row means the action
was accepted and applied; rejected attempts are not audit rows.

The idempotency fingerprint binds action, target kind, target ID, and request
representation version. A same-principal, same-key replay returns the original
action and work IDs, changes no durable state, and creates no work or audit.
Using the key for another request returns
`OPERATOR_IDEMPOTENCY_CONFLICT`. Different principals may use the same raw key.
A rejected action does not bind the key and can be retried after conditions or
authorization change.

The audit stores a SHA-256 binding of the scoped key, never the raw key. Logs
and telemetry also exclude raw keys and bearer credentials. Audit rows are
append-only and cannot be used as a second mutable work state.

### Problems and bounded telemetry

The admin transport uses RFC 9457 Problem Details and the following stable
codes:

| HTTP | Code |
| --- | --- |
| 400 | `INVALID_ARGUMENT` |
| 401 | `UNAUTHENTICATED` |
| 403 | `OPERATOR_FORBIDDEN` |
| 404 | `RESOURCE_NOT_FOUND`, `OPERATION_NOT_FOUND`, `WORK_NOT_FOUND` |
| 409 | `OPERATOR_IDEMPOTENCY_CONFLICT`, `RECOVERY_ALREADY_ACTIVE`, `ACTION_NOT_APPLICABLE`, `RECOVERY_UNSAFE` |
| 412 | `DIAGNOSTIC_STALE` |
| 428 | `PRECONDITION_REQUIRED` |
| 500 | `INTERNAL` |
| 503 | `ADMIN_DRAINING`, `PERSISTENCE_UNAVAILABLE` |

Prometheus export exposes `liftr_operator_requests_total`. Its bounded actions
are `diagnostics_read`, `trigger_observe`, `trigger_passive_observe`, and
`recover_dead_work`; results are `read`, `applied`, `replayed`, `stale`,
`not_applicable`, `unsafe`, `conflict`, `denied`, and `error` (with unknown
values collapsed). `liftr_operator_recoveries_total` uses the bounded source
recovery kinds `Dispatch`, `Observe`, `PassiveObserve`, and `Drive` plus a
bounded result. IDs, principals, provisioner refs, diagnostic revisions, and
free-form errors are forbidden labels. Telemetry is not used for authorization,
planning, idempotency, audit, or recovery.

### Candidate discovery is deferred

M20 deliberately has no Resource, Operation, work, or recovery-candidate list
endpoint on the admin listener. Operators discover candidate IDs through the
existing bounded sampler WARN logs and metrics, then correlate Resource and
Operation IDs through public history where authorized. Metrics identify a
class of candidate, not every ID; sampler logs carry the bounded IDs needed for
follow-up. This is a conscious scope limitation, not an implied listing API.

### Shutdown order

Shutdown first flips readiness to draining, then drains both public and admin
HTTP listeners before stopping metrics, canceling and awaiting workers,
flushing telemetry, and closing PostgreSQL. New admin mutations return
`ADMIN_DRAINING` once draining starts. Keeping PostgreSQL available until both
listeners and workers have stopped preserves transactional admissions and
in-flight request completion.

## Consequences

- Operators can inspect enough durable metadata to choose a sanctioned action
  without exposing raw specs, state, output values, or provider diagnostics.
- Recovery is conservative. Some Dead Dispatches and ambiguous executions
  intentionally remain manual investigations because safe automation cannot
  prove what happened.
- The separate audience, grants, listener, router, and metrics vocabulary keep
  platform authority out of the developer API, but operators must secure and
  monitor another network endpoint and static policy file.
- Immutable audit plus principal-scoped idempotency gives one durable accepted
  action and one work item despite client retries.
- ETags reduce stale operator decisions but do not replace transactional
  revalidation or authorization.

## Deferred Work

- Candidate listing, pagination, filtering, bulk diagnostics, and bulk recovery
  are deferred.
- Force, arbitrary retry, cancellation, terminal override, state mutation,
  Dispatch recreation, and manual ambiguity resolution are not M20 features.
- Operator group grants, dynamic policy administration, grants hot reload, and
  an audit-read HTTP API are deferred.
- Operation diagnostics are bounded by construction: they return the latest
  attempt, the structurally small active work set, and honest total counts —
  never complete attempt or work history arrays. Repository queries enforce
  LIMIT and aggregate counts at SQL level; no endpoint loads a full collection
  and truncates in Go. Paginated historical investigation endpoints remain
  deferred follow-up work.
- General OpenTofu state repair, import, push, force-unlock, quarantine restore,
  and backend-state manipulation remain prohibited by ADR-0020.

## Rejected Alternatives

- Adding admin routes to the public router weakens network and audience
  separation and risks exposing platform implementation details to developer
  clients.
- Reusing owner authorization makes Resource membership imply platform control;
  the two authorities are intentionally independent.
- Replaying a Dead row or resetting it to Pending reuses stale payload,
  expected-version, lease, and ambiguity assumptions.
- Recreating Dispatch can duplicate an already-applied provider mutation.
- Treating ETag possession or an idempotency replay as authorization permits
  stale credentials or leaked values to retain authority.
- Calling a provisioner while holding the recovery transaction cannot make the
  provider effect atomic with PostgreSQL and would expand lock duration.
- Persisting rejected operator actions confuses attempted access with accepted
  mutation and would make audit-row existence cease to have one meaning.
