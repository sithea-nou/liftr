# ADR-0022: Resource Relationships and Dependency-Aware Lifecycle

Date: 2026-08-26
Status: Accepted

- Refines: ADR-0002 (core domain model), ADR-0003 (deterministic lifecycle
  engine), ADR-0005 (application orchestration), ADR-0006 (PostgreSQL and
  transactional outbox), ADR-0008 (HTTP contract v1), ADR-0009 (ResourceType
  contracts), ADR-0011 (outputs; convergence postconditions), ADR-0014
  (explicit retry), ADR-0019 (platform policy and transactional quotas),
  ADR-0021 (operator diagnostics and safe recovery)

## Context

A Liftr Resource may need another Liftr Resource to exist and be ready before
its own infrastructure can converge: an application depends on a database, a
database depends on a network. Until M21 nothing in Liftr expressed this. The
dependency graph belongs to Liftr, not to any provisioner: a developer names
Liftr ResourceIDs — never Azure IDs, Terraform addresses, Pulumi URNs, or
Kubernetes object references — and relationship semantics stay identical
whether either side is implemented by Pulumi, Crossplane, OpenTofu, or a
future adapter.

## Decision

### First-class provider-neutral references

References are a first-class Resource concept, separate from the opaque
`ResourceSpec`. Spec stays schema-blind and untouched; reference extraction is
structural, not JSON scraping. Canonical wire form:

    "references": { "<slot>": ["res_id", ...] }

Slot order and ID order are irrelevant: admission canonicalizes (slots sorted,
targets deduplicated and sorted byte-wise), so reordering alone is
fingerprint-neutral. Duplicates are rejected. Bounds: per-slot `maxItems`
(contract-declared, hard cap 16), total 32 bound targets per Resource.

### Immutable contract-owned ReferenceContract

The ResourceType contract owns an immutable `ReferenceContract`: named slots,
exact allowed `ResourceTypeRef` targets, and `minItems`(0|1)/`maxItems`.
M21 admits no wildcards, selectors, expressions, provider constraints,
external URLs, Kubernetes refs, or cloud identifiers, and exactly one
reference mode: HARD. Optionality exists only as `minItems == 0`. Adding slots
to a released version requires a new ResourceType version (ADR-0009/0011
precedent). Discovery exposes it as optional `referenceContract`.

### Owner boundary and authorization

References are SAME durable `OwnerRef` only; cross-owner relationships are
rejected with the same generic outcome as every other failure. At admission of
NEWLY ADDED edges only, the acting Principal must hold `resource:read` on each
target; missing, unreadable, cross-owner, wrong-typed, Deleting, and Deleted
targets all render ONE identical refusal (`REFERENCE_INVALID`) so references
never become existence oracles. Preserved durable edges are trusted intent:
updates that keep an edge never reauthorize it, and removed edges require no
permission on their former target. Workers remain authorization-blind.

Target eligibility at HTTP admission includes `Unknown`, `Pending`, `Ready`,
and `Failed` — chains must be creatable and Failed Resources may recover.
Readiness is enforced later by the execution gate.

### Desired and applied reference sets

One set is insufficient during reconciliation. Liftr persists:

    resource_desired_references(source, slot, target, generation)
    resource_applied_references(source, slot, target, generation)

Desired is rewritten atomically with each generation-bumping update. Applied
advances ONLY inside the terminal-success transaction that proves create/update
convergence INCLUDING required output postconditions — the same commit as
status, Operation, Event, execution, output publication, and outbox settlement.
ObservedGeneration is deliberately NOT consulted: repository inspection proved
it advances at Request admission, so it proves evaluation, not convergence.
Output-postcondition rejection does not advance applied sets; observe-only
output recovery advances them exactly once through the normal success path.

Protective evidence = desired ∪ applied. Deletion protection uses the union:
during a B→C retarget both stay protected regardless of which physical
infrastructure actually references what (conservative over-protection).

### Owner structural admission lock

The M18 owner quota advisory lock doubles as the owner-scoped STRUCTURAL
admission lock for graph-mutating paths (relationship-bearing creates,
reference-bearing updates, target-delete protection). Key derivation, SQL, and
namespace are byte-identical; no second owner lock exists. Canonical order on
every affected path:

    idempotency advisory lock → owner admission lock →
    Resource rows (source first, then targets in ascending ResourceID order) →
    Operation rows → execution/attempt rows → waits/outbox

No path acquires the owner lock after a Resource row. Worker gating NEVER
acquires the owner lock, and M13 retry never takes it merely because a
Resource has references.

### Cycle detection

Edge source→target means "source depends on target". A proposed S→T closes a
cycle iff T already reaches S through OUTGOING desired edges. Detection
therefore traverses outgoing edges from each candidate target under the owner
lock (so no concurrent writer can mutate the graph mid-proof). Traversal is
bounded by depth (32) and visited-node budget (4096); reaching either bound
while work remains FAILS CONSERVATIVELY with `ErrReferenceGraphLimit` — a
truncated search never proves safety.

### Pre-Submit dependency gate

Before any Up-like Submit, Drive evaluates the source's CURRENT desired set.
Classification is closed and provider-neutral, derived from durable domain
state only:

- READY — state Ready ∧ generation-matched `Reconciled=True` ∧ no active
  Operation ∧ required outputs published for the current generation.
- WAIT — Pending/Unknown, active update, or Failed with an active
  retry/output-recovery Operation.
- TERMINAL_DEPENDENCY_FAILURE — Failed with no active recovery.
- INVALID — missing, Deleting, Deleted, or invariant violation.

All READY ⇒ dispatch proceeds and obsolete waits are deleted. Any WAIT ⇒ exact
durable waits are registered, `DependenciesReady=False/WaitingForDependencies`
is recorded, and NO attempt is created. Terminal failure or INVALID fails the
dependent PRE-SUBMISSION with curated Liftr-owned reasons (`DependencyFailed`/
`DependencyInvalid`): no Submission, no ExecutionHandle, no provider metrics;
M13 retry re-evaluates live dependency truth with the same-generation desired
set. DELETE bypasses readiness entirely — cleanup must succeed even with
Failed dependencies. Readiness is point-in-time: passing the gate guarantees
eligibility at Submit admission, not throughout provider execution; later
dependency changes never cancel an admitted Submit.

### Lost-wake prevention

Gate transactions lock every desired target row (ascending ID) BEFORE reading
readiness facts and registering waits. Every gate-relevant target transition
(status/state convergence, terminalization, retry/recovery transitions,
required-output publication/failure, passive observation fact changes) locks
the same row and enqueues wake work atomically with its mutation. Therefore
either the gate observed the new state or its wait commits before the
transition — no window remains. No provider call occurs while cross-resource
rows are held. Gate-irrelevant internal phase changes enqueue nothing, and
Resources without waiters produce zero wake rows.

### Versioned WakeDependents

New resource-targeted outbox kind `WakeDependents`:

- Dedupe identity is VERSIONED: `wake-dependents:{resource}:{recordVersion}`;
  unversioned keys could never be enqueued again after first completion.
- At most one ACTIVE wake per target (partial unique index). Concurrent
  transitions coalesce silently behind the active row.
- Finalizer handshake THROUGH THE TARGET ROW LOCK: fan out waiters in bounded
  keyset batches (LIMIT + wait_seq, renewed lease, repeatable batches because
  Drive enqueue deduplicates), then lock the target row, read its current
  version, TERMINALIZE this wake, and — if the target advanced — insert one
  fresh current-version follow-up in the same transaction (old row first so
  the one-active constraint admits it). No version change can be lost.
- Wake means ONLY "something relevant changed; re-evaluate": the worker
  validates waiter identity/version/nonterminality, schedules fresh canonical
  Drives, deletes obsolete wait rows, and NEVER decides readiness, never
  Dispatches, never Submits. Drive remains the single gate decision point.

Wait rows bind exact OperationID + record version; stale registrations are
deleted rather than honored. Waits are removed when the gate passes, the
Operation terminalizes, or the source reaches Deleted.

### Deletion semantics

- Target delete blocked while ANY inbound desired/applied row stands —
  regardless of dependent state — answering bare `409 RESOURCE_IN_USE` (no
  dependent IDs, no counts). Expected release order is user-orchestrated;
  there is no cascade in either direction.
- Source delete bypasses readiness, retains outgoing desired+applied rows
  while Deleting, and removes BOTH sets atomically with the Deleted
  transition.
- Protective rows owned by a Deleted source are invariant corruption: the
  fail-closed rule refuses the target delete with
  `ErrReferenceInvariant` instead of silently filtering
  `state <> 'Deleted'`.

### Public surface

Resource representations expose canonical DESIRED references only; applied
sets are internal protective evidence. Create accepts optional `references`;
update treats an ABSENT field as PRESERVE (pre-M21 CLI/Backstage bodies that
send only spec can never destroy relationships) and an EXPLICITLY present
field — including `{}` — as full replacement; JSON null is rejected as
ambiguous. Fingerprints cover the effective/submitted canonical form: same key
plus reordered arrays replays, different keys preserve existing PUT semantics
(a reordered resubmission under a fresh key admits a new generation; M21 adds
no generic content-no-op PUT). New Problems: `422 REFERENCE_INVALID`,
`409 RESOURCE_IN_USE`, `409 DEPENDENCY_CYCLE`; conservative traversal-bound
failures render as `REFERENCE_INVALID`. Discovery detail gains
`referenceContract`. Inventory summaries are unchanged. The CLI gains
repeatable `--reference SLOT=ID[,ID...]` (create: additive binding; update:
explicit replacement), explicit `--clear-references`, and displays desired
references on get. Backstage forwards bodies verbatim; absent-field
preservation keeps it safe without BFF changes.

### Conditions and states

No `ResourceStateBlocked`. Waiting Resources keep existing states;
`DependenciesReady` is a Liftr-owned Condition (`WaitingForDependencies`,
`DependencyFailed`, `DependencyInvalid`, `DependenciesSatisfied`) written
exclusively by the application gate through the lifecycle engine's sanctioned
setter. Messages carry counts at most — never unbounded ID lists; slot names
and target IDs are public on the source itself.

### Policy, quota, observability, operator plane

M18 policy stays graph-blind; quota counts the source exactly as before and
replays skip policy/quota/reference validation unchanged. M17 adds bounded
counters only (`liftr.dependency.gate.total{result}`, 
`liftr.dependency.wake.total{result}`); IDs, owners, slots, types, and depth
are forbidden labels. M20 extends the closed vocabularies: planner reasons
`DEPENDENCY_BLOCKED`, `DEPENDENCY_FAILED`, `DEPENDENCY_INVALID`,
`DEPENDENCY_WAKE_DEAD`; a dependency-blocked Operation plans
`no_action_needed` and Observe triggers refuse; Dead `WakeDependents` recovers
by minting ONE fresh CURRENT-version wake under a fresh action-owned identity
(never replaying waiter lists, never Dispatching); the old Dead row stays
immutable. Operator recovery never overrides dependency truth; no force-submit
exists.

### Provisioner neutrality

The Provisioner contract is unchanged: no resolver, no dependency methods, no
new fields. Gating precedes Submit; adapters receive no other Resource's data.
Zero-reference ResourceTypes execute byte-compatibly across Pulumi,
Crossplane, and OpenTofu.

### Eager test composition cannot weaken dependency safety

The synchronous eager composition (`EnableEagerExecutionForTesting`) is a
test optimization and MUST NOT become an alternate execution path that ignores
relationship semantics. Rule: before any inline phase advancement, eager mode
classifies the pre-Submit gate; zero-reference Resources keep today's eager
behavior byte-for-byte, an all-READY reference set permits the inline path to
proceed (point-in-time readiness, identical to the durable gate guarantee), and
any WAIT / TERMINAL_DEPENDENCY_FAILURE / INVALID classification REFUSES the
inline Submit with `ErrEagerExecutionBlockedByDependencies` while leaving the
admitted Operation and its original canonical Drive untouched — so the durable
worker later owns the full wait/wake/pre-submission-failure semantics through
the standard gate with no special-casing. Eager mode implements no polling, no
private wait system, and never terminalizes an Operation on the gate's behalf.

## Consequences

- Developers express dependencies with Liftr identity alone; graphs are
  auditable, enforceable, and provisioner-independent.
- Convergence ordering becomes automatic for create/update; deletion becomes
  safe against accidental orphaning, at the cost of explicit user-sequenced
  teardown of dependency chains.
- Per-owner mutation serialization widens slightly (reference-bearing
  mutations and all deletes take the shared owner lock).
- Update admission re-stamps desired-row generations even for preserved sets
  so `desired.generation == Resource.Generation` holds universally.
- Wake latency remains ticker-bound like all orchestration latency.

## Rejected Alternatives

- Embedded-in-spec references: opaque-spec violation, unsafe scraping.
- Reverse-graph ("who depends on T") cycle proof: misses the canonical
  A→B/B→A case; outgoing traversal is mandatory.
- Unversioned wake dedupe keys: globally unique history would permanently
  suppress future wakes.
- Owner advisory lock in worker paths: violates the canonical lock ladder.
- Filtering protective rows by `source.state <> 'Deleted'`: silently
  undercounts corrupted state; fail closed instead.
- Generic content-no-op PUT refinement: unrelated API change; deferred.
- Clear-on-absent update semantics: destroys pre-M21 client relationships.
- New Blocked lifecycle states: Conditions suffice; vocabulary stays small.
- Polling-only waiting and unbounded in-transaction fan-out.
- Provisioner-visible references or adapter-side resolution.

## Implementation evidence

The merge gate required seeded large-graph plan proof on real PostgreSQL
(≥100k desired edges, a 10,000-dependent star, 12,000 waits, and an acyclic
path longer than `MaxDependencyDepth`). All nine hot queries are proven
index-backed (`EXPLAIN ANALYZE`): outgoing desired/applied by source ride the
primary keys, protective inbound union rides both inbound indexes (~8 ms at
10k dependents), waiter keyset paging rides `(target_id, wait_seq)` — swept at
10,000 waiters in 40 bounded batches — and cycle traversal rides the primary
key. Two findings were fixed rather than papered over: the redundant standalone
unique index on `wait_seq` was REMOVED because it misled the planner into a
degenerate fanout plan whenever one target's waiters are sparse across the
global sequence space (identity already guarantees uniqueness), and the
wake-EXISTS hot path is pinned against a SPARSE target, which is the
production-critical distribution; at extreme single-target skew a sequential
scan is legitimately optimal and is covered functionally instead.

A large fanout execution test on real PostgreSQL deterministically registers
250 blocked dependents against one Pending target, releases the anchor's own
outbox work, and proves: the anchor's success transaction enqueues exactly ONE
WakeDependents; a mid-fanout worker restart (fresh lease identity) is harmless;
every dependent converges with exactly one Provisioner.Submit; and zero wait
rows survive full convergence.

## Scope Exclusions

Output-to-input wiring, secret propagation, expressions/selectors, cross-owner
sharing and ACLs, cascades, aggregates/stacks, graph visualization or developer
traversal APIs, automatic migration orchestration, provider-specific or
external references, Kubernetes ownerReferences, Crossplane selectors,
Terraform remote state, Pulumi StackReference, dependency-based policy/quota,
soft/runtime-health dependencies, and automatic propagation when dependency
outputs change remain out of scope.
