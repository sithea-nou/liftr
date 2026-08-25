# ADR-0019: Platform Policy and Transactional Admission Quotas

Date: 2026-08-25
Status: Accepted

- Refines: ADR-0005 (application orchestration), ADR-0006 (PostgreSQL transactions), ADR-0008 (HTTP admission and idempotency), ADR-0012 (authorization before replay), ADR-0018 (bounded observability)

## Context

Liftr authorizes who may mutate a Resource and validates what each
ResourceType accepts, but platform operators cannot disable a capability for a
type or bound retained Resource counts per owner. Provisioners cannot own this
business policy, and an in-process count followed by a later insert would
over-admit when replicas create concurrently.

M18 adds a restrictive platform-policy overlay and transactional quota
admission without changing public Resource contracts or worker execution.

## Decision

### One immutable startup policy

`LIFTR_POLICY_FILE` optionally names one bounded JSON policy document. Startup
strictly parses and validates it against the registered ResourceType catalog;
unknown fields, duplicate JSON members, unknown ResourceTypes, malformed or
duplicate semantic rules, trailing documents, and bounds violations fail
startup. An omitted path compiles an empty no-restrictions document. There is
no hot reload: each process logs one derived `pol_v1_<sha256>` revision and
uses it for its lifetime.

The v1 grammar has exactly two rule kinds:

- `capability-deny` requires an exact `resourceType`, optionally an exact
  `owner`, and denies only `create` and/or `update`.
- `resource-count-quota` has a positive bounded `limit`; exact `owner` and
  exact `resourceType` selectors are independently optional. Omitted owner
  always means the actual authorized owner, never a global count. Omitted type
  bounds all retained Resources for that owner; a present type bounds that
  owner's exact type/version.

Matching selectors compose by AND. Matching restrictions never override each
other; the smallest applicable limit for each owner or owner/type dimension
wins deterministically. Rule order has no meaning and normalized equivalent
documents derive the same revision.

### Pure evaluation and durable facts

The application owns `AdmissionPolicy` and `QuotaRepository` ports. The policy
compiler/evaluator in `internal/policy` receives only an `AdmissionIntent` and
typed `ResourceCountFacts`: no Principal, repository, clock, HTTP,
provisioner, environment, or remote dependency can enter evaluation. The
PostgreSQL adapter owns count acquisition; the policy owns the decision.

Create evaluates capability denial and applicable count quotas. Update
evaluates capability denial only. Delete, explicit retry, provisioner workers,
and observations are policy-blind: admitted work remains executable after a
policy revision changes.

Every retained Resource state except `Deleted` consumes quota. The count query
uses a LEFT JOIN from Resources to durable statuses and fails closed if any
retained Resource for the owner lacks status, rather than silently
undercounting corrupted state.

### Transactional admission and lock order

PostgreSQL admission transactions explicitly use `READ COMMITTED`. A fresh
quota-bearing create takes these locks in this order:

1. Principal-scoped idempotency advisory lock.
2. Transaction-scoped owner quota advisory lock.
3. Resource row/identity lock.
4. Operation and execution locks.

No path may acquire the owner quota lock after a Resource or Operation lock.
The owner key is a SHA-256 digest over length-framed namespace, owner kind,
and owner ID, mapped to PostgreSQL's two-`int32` advisory-lock namespace. The
lock statement and subsequent aggregate query are separate statements. This
serializes all quota dimensions for one actual owner while allowing unrelated
owners to admit concurrently. It deliberately favors one simple lock over
premature per-rule lock graphs or counter tables.

After the lock, Liftr counts, decides, and inserts Resource, status, binding,
Operation, Event, execution, outbox, and idempotency outcome in the same
transaction. A denial rolls everything back and does not claim the
Idempotency-Key.

### Replay, authorization, and audit

Authorization remains before replay as required by ADR-0012. After current
authorization and fingerprint equality, a successful idempotent replay
returns its original result without consulting the current policy or quota;
otherwise a configuration rollout could invalidate an already-completed API
contract.

Newly admitted create/update Events carry typed private admission provenance
with the policy revision. Rule IDs and count details remain private and never
enter public Resource or Operation representations. Public Problems add only
stable opaque outcomes: `403 POLICY_DENIED` and `409 QUOTA_EXCEEDED`.
Persistence inability while acquiring quota facts is
`503 PERSISTENCE_UNAVAILABLE`; malformed policy evaluation or quota-state
corruption is an opaque `500 INTERNAL`.

### Observability

`liftr.policy.admissions` counts pure policy decisions by the closed mutation
(`create`, `update`) and outcome (`allowed`, `policy_denied`,
`quota_exceeded`, `error`) vocabularies. Owner, ResourceType, rule ID, policy
revision, and counts are forbidden metric labels. Durable Events remain the
audit source of truth; telemetry never participates in admission.

## Consequences

- Concurrent replicas cannot exceed a configured per-owner limit through a
  count/insert race.
- Stricter rollout is process configuration, not an API mutation. Operators
  must coordinate restarts because mixed revisions intentionally enforce
  their own immutable snapshots.
- Counts are computed from durable Resources rather than cached counters,
  avoiding a second state model at the cost of one indexed aggregate per
  quota-bearing create.
- M18 intentionally adds no allow/override rules, expressions, global quotas,
  rate limits, policy administration API, hot reload, counter table, or policy
  checks in workers.

## Rejected Alternatives

- Provisioner-owned policy leaks business policy into implementation adapters.
- Preflight counts outside the insertion transaction race across replicas.
- `SERIALIZABLE` alone obscures the owner contention boundary and requires
  broad retry semantics; the explicit owner lock makes the invariant direct.
- Global or per-rule locks either destroy unrelated-owner concurrency or
  create unnecessary lock-order complexity.
- Re-evaluating successful replays breaks idempotency when configuration
  changes.
