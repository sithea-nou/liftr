# Liftr Operational Runbook

This runbook assumes M17-M20 signals: structured JSON logs, the Prometheus
endpoint on `LIFTR_METRICS_ADDR`, `/healthz` (liveness), `/readyz`
(control-plane core readiness), and the optional separate operator listener.
Metric names follow the pinned OTel SDK version; see the metric help strings
for cluster-global vs per-process semantics.

## Safety rules — read first

1. **Never casually mutate database state.** Durable rows are the only source
   of lifecycle truth; outbox terminal rows are immutable by trigger.
2. **Never delete Operation, Event, execution, or outbox rows, and never revive
   a `Dead` row.** M20 recovery is narrowly safe: it creates one fresh work row
   from current locked durable state through the admin API. It is not a general
   redrive and never replays a Dead payload or creates a replacement Dispatch.
3. **Never force-conclude ambiguous infrastructure work.** A Dispatch whose
   outcome reached the provider may be genuinely applied even if Liftr never
   learned so. Recovery always routes through Observe.
4. Restarts are safe by lease design: abandoned leases expire and recover
   through the existing Unknown → Observe machinery. Prefer restart over any
   manual intervention.
5. **Never bypass an OpenTofu state lock.** Do not use `force-unlock`,
   `-lock=false`, delete lock artifacts, or run plan/refresh as a lock probe.

## 1. API unhealthy

**Signals:** `healthz` failing (process dead), 5xx share of
`http_server_request_duration_seconds`, `liftr_http_panics_total`.

- Liveness failing on one replica: check its container logs for
  `error_class="panic"` or startup errors; replace the instance.
- Elevated 5xx with `code=INTERNAL`: pull request_id from the access log,
  follow it into admission/worker logs.

## 2. PostgreSQL unavailable

**Signals:** `/readyz` = 503 `PERSISTENCE_UNAVAILABLE`,
`liftr_persistence_transactions_total{result="error"|"retryable"}` rising,
connection-refused errors in logs.

- Transient loss: requests answer existing 503 Problems; workers retry with
  bounded backoff; nothing needs manual repair once the DB returns.
- Pool exhaustion: watch per-process `liftr_persistence_pool_acquired/_idle`;
  long saturation means a slow query or undersized pool — investigate slow
  queries, do not raise limits blindly.
- Deadlocks/serialization (`result="retryable"`): occasional counts are
  normal under contention; sustained growth indicates a pathological
  workload — capture the period and open an issue.

## 3. Outbox backlog

**Signals:** `liftr_outbox_pending_depth`, `liftr_outbox_pending_oldest_age_seconds`
(cluster-global; aggregate replicas with max),
`liftr_worker_work_total{outcome="retry"}` elevated.

- Depth growing while worker success rate is nonzero: capacity problem —
  scale worker processes (safe by lease design).
- Oldest age growing but depth flat: a poison loop — look for repeated
  `outcome="failed"` (quarantined to Dead;
  `liftr_outbox_dead_total_count`) and the corresponding WARN log with
  `error_class`.
- Expired leases rising (`liftr_outbox_expired_leases`): recovery lag or
  crashed claimants; after a crash this drains automatically.

## 4. Stuck candidates (long-running / reconciliation-silent)

**Signals:** `liftr_operations_long_running{capability,severity}`,
`liftr_operations_reconciliation_silent{capability}`, sampler freshness
(`liftr_observability_sampler_last_success_unix_seconds`).

These are DIAGNOSTIC labels, not lifecycle states. A continuously-observed
never-converging backend raises long-running while reconciliation-silent
stays zero; silence rising means observations/work stopped.

Runbook actions, in order:
1. Identify the Operation via sampler WARN logs (operation_id, resource_id,
   capability, age).
2. Read its public history (`GET /v1/resources/{id}/operations`) and Events.
3. Check the relevant provisioner section below.
4. If the developer wants a clean slate for a FAILED operation, explicit
   retry (`POST /v1/operations/{id}/retry`) remains the only sanctioned
   developer retry path. Operator observation and dead-work recovery do not
   create a lifecycle retry.
5. Never mark anything Failed by hand; never delete the Operation.

## 5. Repeated ambiguous submissions

**Signals:** `liftr_provisioner_submissions_total{outcome="ambiguous"}`,
worker `liftr_worker_work_total{kind="dispatch",outcome="ambiguous"}` versus
`liftr_worker_work_total{kind="dispatch",outcome="lease_lost"}`.

These are DIFFERENT diagnoses:

- `ambiguous` — the provider submission outcome is uncertain while this
  worker still owned its fenced lease. Recovery routes the attempt through
  Unknown → Observe; nothing was double-executed.
- `lease_lost` — the worker provably lost fencing ownership (heartbeat
  renewal failed or another claimant moved the durable state). Look for
  competing worker processes, clock problems, or database stalls during the
  loss window.

Ambiguity is safe by design (attempt → Unknown → Observe recovery), but a
rising rate means the provisioner cannot complete a submission round trip.
For Pulumi check workspace/backend availability; for Crossplane check API
reachability. For a composed OpenTofu registration, check the HTTPS HTTP state
backend and the adapter-private evidence/quarantine boundary described below.
The attempt's Unknown state guarantees no double-execution — do not "help" by
re-submitting anything manually.

## 6. Auth / JWKS errors

**Signals:** `liftr_authentication_total` failure reasons (typed enum),
`liftr_jwks_refresh_total{result="failure"}`,
`liftr_jwks_forced_refresh_limited_total`.

- Cached keys keep verifying through IdP outages; readiness is unaffected by
  design. Sustained `jwks_unavailable` plus `refresh_rate_limited` spikes
  usually mean either an IdP outage or a forgery flood probing unknown kids —
  compare against request volume.
- Reason labels are operator-only diagnostics; the public API still answers
  one indistinguishable 401. Never expose reason detail to callers.

## 7. Pulumi failures

**Signals:** `liftr_provisioner_submissions_total{liftr_provisioner_kind="pulumi",...}`,
curated reasons in structured logs (e.g., WorkspaceUnavailable,
ExecutionEnvironmentUnavailable, HistoryUnavailable).

All Pulumi detail stays in curated logs — never in labels. Check workspace
root disk space, program binary digests, and credential env forwarding.
Stack names and backend URLs are diagnostic identifiers in logs only.

## 8. Crossplane reconciliation stalls

**Signals:** long-running Operations with `liftr.provisioner.kind="crossplane"`,
Observe outcomes stuck at running/unknown.

Crossplane reconciles declaratively; nonterminal loops persist until operator
intervention by design (ADR-0015). Inspect platform XR conditions on the
Kubernetes side; raw condition messages intentionally never cross into Liftr.
Destruction completes only when Observe proves genuine absence — an accepted
DELETE whose kind disappeared stays alive with a curated failure until fixed.

## 9. Output reconciliation failures

**Signals:** Operations terminal-Failed with output-postcondition reasons,
`scheduler`-side Observe outcomes cycling, WARN logs with
`error_class="output_reconciliation"`.

Output recovery retries are observe-only and never re-execute the backend.
If extraction keeps failing (OutputsInvalid), the contract mapping disagrees
with the backend reality; fix the binding/mapping via a new versioned
ResourceType contract — never edit persisted mappings.

## 10. Safe restart / shutdown expectations

Sequence on SIGTERM: public and admin readiness flip 503, with admin reporting
`ADMIN_DRAINING` → public and admin HTTP drain (10s) → metrics listener stops
→ worker canceled and awaited (10s) → telemetry flushed → DB closed. In-flight
Submits that get canceled remain durably ambiguous and recover via lease expiry
+ Observe after restart — expect (and ignore) transient `lease_lost`/Unknown
entries around the restart window. Telemetry flush failures never affect
lifecycle outcomes. PostgreSQL must remain available until both HTTP listeners
and the worker have stopped.

## 11. Platform policy and quota admission

`LIFTR_POLICY_FILE` is read exactly once at startup. Invalid syntax, unknown
fields or ResourceTypes, duplicate members/rules, and invalid limits fail boot.
Every healthy instance logs `platform admission policy loaded` with
`policy_revision`; compare that field across replicas before considering a
rollout complete. Never rely on a long mixed-revision window: old instances
continue enforcing their immutable old snapshot by design.

Safe rollout:

1. Validate the document in staging against the exact registered ResourceType
   catalog and exercise representative authorized creates/updates.
2. Roll all replicas with the same file contents and verify one identical
   revision in startup logs.
3. Watch `liftr_policy_admissions_total` by bounded mutation/outcome and public
   `POLICY_DENIED`, `QUOTA_EXCEEDED`, and `PERSISTENCE_UNAVAILABLE` rates.
4. Roll back by restoring the previous file and restarting all replicas. There
   is no hot reload and no database policy state to repair.

`QUOTA_EXCEEDED` is an expected admission result and persists no Operation,
Event, outbox item, or idempotency outcome. A same-key retry can therefore be
admitted after policy changes. Successful earlier requests remain replayable
under a stricter current revision.

For an unexpected quota denial, use read-only diagnostics grouped by the exact
owner and remember that every retained state except `Deleted` counts. First
check for corrupt retained rows with no durable status; Liftr fails those
owners closed as `INTERNAL` rather than undercounting:

```sql
SELECT r.id
FROM resources r
LEFT JOIN resource_statuses s ON s.resource_id = r.id
WHERE r.owner_kind = $1 AND r.owner_id = $2 AND s.resource_id IS NULL;
```

Never edit statuses or delete tombstones to free quota. Correct policy through
a coordinated rollout, or use normal lifecycle deletion so the retained
Resource reaches `Deleted`. Event `data.admission.policyRevision` identifies
which private revision admitted a successful create/update; rule IDs and
counts intentionally never appear in public Problems or metric labels.

## 12. OpenTofu HTTP state, locks, scratch, and quarantine

The OpenTofu package, PostgreSQL evidence store, per-call scratch/quarantine
handling, and Unix process-tree cancellation are optionally wired into
production server composition only when `LIFTR_OPENTOFU_CONFIG_FILE` names the
strict operator-supplied registration set. Its `registrations` array retains
every immutable ref still bound to a Resource; its `routes` array selects the
ref used only for new Resources of each ResourceType. An empty variable leaves
the Pulumi default unchanged. Liftr ships no production cloud program or
backend registration.

The only approved engine target is a real official OpenTofu 1.12.6 binary.
Production also requires an operator-supplied conformant HTTPS HTTP state
backend. Qualification is complete against a real official OpenTofu 1.12.6
binary and the conformant test HTTP backend, but each operator must separately
qualify its production HTTPS backend. The local backend and insecure HTTP are
development/test-only and cannot be selected by production composition. No
Terraform version is selected or supported.

Treat each provisioner ref, identity, exact executable, source digest, program
ref, and backend ref as immutable while any Resource uses that registration. To
roll forward, add a new registration, move the matching current route, and keep
the old registration and artifacts available. Existing Resources resolve their
persisted ref exactly and do not follow the current type route or default. The
config file must not be group- or world-writable. Program environments cannot
set OpenTofu control variables; backend environments allow only the explicit
HTTP credential/TLS variables. Their values must never be logged.

### Backend and lock handling

- OpenTofu, not Liftr, owns HTTP backend GET, state update, LOCK, and UNLOCK and
  propagates the backend lock ID. Never proxy these through an improvised Liftr
  lock or treat a scratch-workdir file lock as the state lock.
- For a busy state lock, identify the OpenTofu process and owning worker lease,
  then wait for normal completion. On Unix, cancellation of owned internal work
  interrupts the process group and uses bounded forced termination if needed;
  backend lock release and Observe still determine recovery. Public Operation
  cancellation and Windows adapter support are not available.
- Never use `force-unlock`, `-lock=false`, lock deletion, or a sacrificial
  plan/refresh probe. The normal saved plan and saved-plan apply acquire their
  own engine locks.

### Attempt and state diagnosis

- `ApplyMayStart` means no `tofu apply` process may have started before that
  phase became durable. Init, normal planning, and plan inspection may already
  have read state, initialized providers, evaluated data sources, or made
  provider reads; they did not authorize Apply mutation.
- Unknown operational init/plan/inspection failures before `ApplyMayStart` may
  redeliver the same attempt with capped backoff. Deterministic registration,
  source, input, supply-chain, or plan-closure errors are terminal. The exact
  1.12.6 implementation recognizes only its closed, tested machine-UI semantic
  summary allowlist; all other nonzero outcomes remain unavailable. Never infer
  lock or backend conclusions from diagnostic text.
- A missing, malformed, wrong-lineage, regressed, or digest-mismatched backend
  state does not prove infrastructure absence. Stop execution for the affected
  registration and compare the private attempt journal/state binding with
  PostgreSQL history read-only. Do not import, push/edit state, rewrite a
  SHA-256 digest, run OpenTofu manually, or resubmit.
- Delete is a normal saved-plan apply with `desiredPresent=false`. Workload
  addresses are removed while the private control marker and stable backend
  state remain. Never substitute whole-state `destroy`, backend DELETE,
  `state rm`, or garbage collection.

### Scratch and quarantine

- Every call uses a private scratch workdir; normal conclusive success removes
  it. Scratch is not the stable state location and is not reused as a
  workspace.
- Ambiguous/errored workdirs, including any containing `errored.tfstate`, move
  to a separate same-filesystem quarantine. Startup scans only adapter-owned
  orphan names and quarantines only confirmed inactive owned workdirs; it does
  not adopt or delete unrelated directories.
- Quarantine may contain state fragments, plans, provider data, command output,
  and secrets. Keep it non-executable, encrypted, access-restricted, and
  retained until an approved diagnostic/retention action. Never restore it into
  the work namespace or delete it solely by age. Quarantine is diagnostic
  evidence, not authoritative backend state.

### Backup and capacity

- Pause workers before backup or restore. Coordinate PostgreSQL and the
  production HTTP state backend as one recovery set; restoring only one side
  can invalidate attempt/state bindings. Back up quarantine separately under
  its diagnostic retention and access policy.
- Under pressure, add backend or quarantine capacity. Do not delete backend
  state, the retained delete marker, journals, or quarantine to make space.
- External-provider registrations require their immutable dependency lock and
  verified offline provider packages. The built-in-only path has no provider
  lock entry or provider/registry network access; even after it is run, it is
  not external-provider or cloud acceptance evidence.

### Output confidentiality

Output collection privately retrieves bounded metadata for all root outputs so
the mapped envelope can be required to have `sensitive=false`. Unmapped values
are discarded immediately. Never log or copy output names, output values, raw
state, plans, stdout, or stderr into tickets, Events, metrics, or public API
fields.

## 13. Operator diagnostics and safe recovery

The M20 operator API is an optional, separate control plane. It is disabled
unless `LIFTR_ADMIN_ADDR` is set, requires `LIFTR_DATABASE_URL` and the full
durable runtime, and is never composed in health-only mode. Its router contains
only `/admin/v1` routes plus unauthenticated `/healthz` and `/readyz`; it does
not appear on the public listener, and public `/v1` routes do not appear on the
admin listener. Restrict the admin address at the network layer in addition to
using bearer authentication.

### Secured configuration

An enabled secured listener requires a distinct audience and a strict static
grants file:

```sh
LIFTR_ADMIN_ADDR=127.0.0.1:8082
LIFTR_ADMIN_AUTH_AUDIENCE=https://liftr.example/operator
LIFTR_ADMIN_AUTH_GRANTS_FILE=/etc/liftr/operator-grants.json
```

`LIFTR_ADMIN_AUTH_ISSUER` defaults to `LIFTR_AUTH_ISSUER`.
`LIFTR_ADMIN_AUTH_ALGORITHMS` and `LIFTR_ADMIN_AUTH_KIND_CLAIM` likewise
default to their public-listener settings. The admin audience must differ from
`LIFTR_AUTH_AUDIENCE`. Audience checks use membership semantics: an explicitly
issued dual-audience token may authenticate on both listeners, but public owner
authorization and operator grants are still independent.

The grants file is deny-by-default and is read strictly at startup:

```json
{
  "subjects": {
    "on-call-subject": [
      "operator:diagnostics:read",
      "operator:observation:trigger",
      "operator:work:recover"
    ]
  }
}
```

Unknown fields or permissions, duplicate permissions, empty grants, and
non-canonical subjects fail startup. M20 grants subjects, not groups. Protect
the file as security policy and roll processes to change it; do not assume a
hot reload. `LIFTR_AUTH_MODE=insecure` is development-only and makes both the
public and admin listeners insecure and allow-all. There is no independently
insecure admin switch and no secure fallback from incomplete configuration.

Probe the admin listener itself during rollout. `/healthz` proves listener
liveness only. `/readyz` also checks durable operator state and returns
`PERSISTENCE_UNAVAILABLE` when PostgreSQL cannot be reached or
`ADMIN_DRAINING` during shutdown.

### Finding and diagnosing a candidate

Candidate listing is deliberately deferred. There is no admin list, search,
or recovery-candidate endpoint in M20. Obtain IDs from bounded sampler WARN
logs and existing metrics, then correlate Resource and Operation IDs through
public history when authorized. Metrics identify a class of candidate; they do
not contain IDs. This is a scope limitation, not an undocumented listing API.

Use the target-specific diagnostic:

```sh
curl -i \
  -H "Authorization: Bearer $LIFTR_OPERATOR_TOKEN" \
  "$LIFTR_ADMIN_URL/admin/v1/operations/$OPERATION_ID/diagnostics"

curl -i \
  -H "Authorization: Bearer $LIFTR_OPERATOR_TOKEN" \
  "$LIFTR_ADMIN_URL/admin/v1/resources/$RESOURCE_ID/diagnostics"

curl -i \
  -H "Authorization: Bearer $LIFTR_OPERATOR_TOKEN" \
  "$LIFTR_ADMIN_URL/admin/v1/work/$WORK_ID/diagnostics"
```

Diagnostics require `operator:diagnostics:read`, carry
`Cache-Control: no-store`, and return a quoted strong `ETag`. Inspect the
bounded `recovery.state`, `recovery.reasons`, and `recovery.allowedActions`
along with current lifecycle/work state and registration availability. A
Resource response may contain the desired-state SHA-256 and, for OpenTofu, only
a bounded private state digest prefix. Treat provisioner refs, state keys, and
other private identifiers as restricted operator data.

The response never includes raw ResourceSpec, output or secret values, provider
diagnostics, outbox payload or last-error text, handles, state bytes, state
lineage values, full private state digests, credentials, tokens, or idempotency
keys. Do not seek those values in logs or copy private state/quarantine into a
ticket.

### Requesting an allowed action

Every mutation requires an empty body, a non-blank `Idempotency-Key` of at most
200 bytes, and the corresponding permission. Supplying the diagnostic ETag as
`If-Match` is recommended:

```sh
curl -i -X POST \
  -H "Authorization: Bearer $LIFTR_OPERATOR_TOKEN" \
  -H "Idempotency-Key: incident-2026-08-25-observe-1" \
  -H 'If-Match: "diag_v1_..."' \
  "$LIFTR_ADMIN_URL/admin/v1/operations/$OPERATION_ID/observe"
```

Use `/admin/v1/resources/{id}/observe` for a Resource PassiveObserve and
`/admin/v1/work/{id}/recover` for Dead work. A first acceptance returns `202`
with `result: applied`, one `operatorActionId`, and one `createdWorkId`. A
same-principal retry with the same key returns the original IDs, reports
`result: replayed`, and adds `Idempotency-Replayed: true`; it creates no audit
or work. A rejected request does not bind its key. Never put credentials,
tokens, specs, or incident secrets in an idempotency key.

An ETag is stale-decision assistance, not authorization. Liftr authorizes
before lookup and again in the transaction, locks and reloads current durable
state, and reruns the pure RecoveryPlanner even without `If-Match`. On
`DIAGNOSTIC_STALE`, read diagnostics again and reassess; do not simply remove
the precondition. On `OPERATOR_FORBIDDEN`, correct the grants rollout rather
than borrowing a developer token or editing data.

The safe mappings are deliberately narrow:

| Request | Accepted only when | New work |
| --- | --- | --- |
| Operation observe | Active observable execution, no active Observe, registration available | `Observe` |
| Resource observe | Current Resource is not Deleted, no active Operation/equivalent work, registration available | `PassiveObserve` |
| Recover Dead Dispatch | Current execution is safely observable and no equivalent chain is active | `Observe`, never `Dispatch` |
| Recover Dead Observe | Current execution remains observable and no Observe is active | `Observe` |
| Recover Dead PassiveObserve | Current Resource is not Deleted and no equivalent work is active | `PassiveObserve` |
| Recover Dead Drive | Current Operation is active and no Drive is active | `Drive` rebuilt from current state with `{}` payload |

Terminal/superseded targets, equivalent active work, absent execution or
registration, pre-submit Dead Dispatch without observable evidence, and unsafe
ambiguity are refused. `ACTION_NOT_APPLICABLE` means the action has no safe
meaning for current state; `RECOVERY_ALREADY_ACTIVE` means existing work owns
it; `RECOVERY_UNSAFE` requires investigation rather than force. The API has no
force, generic retry, cancel, terminal override, state edit, or provider call
inside its admission transaction.

Never mutate PostgreSQL to make an assessment pass, set a Dead row back to
Pending, edit/delete its payload, invent a lease, or manually create Dispatch.
The immutable Dead row is evidence. Accepted recovery writes a new row and an
immutable `operator_actions` audit row; `source_work_id` exists only for
dead-work recovery and `created_work_id` references the one new outbox row.

### Operator signals

- `liftr_operator_requests_total` uses only bounded action and result labels.
- `liftr_operator_recoveries_total` uses only bounded source recovery kind and
  result labels.

Neither metric carries IDs, principals, provisioner refs, diagnostic revisions,
or free-form errors. Use the accepted-action log and immutable audit for
provenance; logs never contain the raw idempotency key. An increase in
`unsafe`, `not_applicable`, `conflict`, or `stale` is a prompt to reread current
diagnostics, not to bypass the planner.

## 14. Resource dependencies and blocked dependents (M21)

Resources may declare provider-neutral references to other Liftr Resources of
the same owner (ADR-0022). A dependent's create/update waits until every hard
dependency is READY; a conclusively Failed dependency with no active recovery
fails the dependent PRE-SUBMISSION with reasons `DependencyFailed` /
`DependencyInvalid`. There is no force-submit and no dependency override:
progress comes only from target-state wakes driving fresh gate evaluations.

Symptoms and actions:

- **Dependent stuck with `DependenciesReady=False/WaitingForDependencies`:**
  expected while a dependency converges. Inspect the referenced Resource via
  the source's public `references`; fix or retry the dependency. The planner
  reports `DEPENDENCY_BLOCKED` / `no_action_needed` — Observe triggers are
  intentionally refused because no provider execution exists.
- **`DependencyFailed` on the dependent:** the dependency terminalized Failed
  with nothing in flight. Retry the dependency first; then M13-retry or update
  the dependent. Retrying the dependent alone re-fails pre-submission by
  design.
- **`RESOURCE_IN_USE` on delete:** another live Resource still desires or may
  physically depend on this one. Delete or retarget the dependent; release
  order is user-sequenced (no cascade).
- **`DEPENDENCY_WAKE_DEAD`:** a Dead wake row is recoverable — recovery mints
  ONE fresh current-version wake from current state and never replays waiters.
- **Reference-invariant INTERNAL failures on delete:** protective rows owned
  by a Deleted source indicate corruption; investigate durable state before
  any manual repair. Deletion stays blocked (fail closed).

Metrics: `liftr.dependency.gate.total{result=ready|waiting|failed|invalid}` and
`liftr.dependency.wake.total{result}`. Neither carries IDs, owners, slots,
types, or graph depth. A sustained `waiting` share with no wake traffic can
indicate lost-wake machinery damage — check Dead work sampled as
`WakeDependents` first.

## Alerting cookbook (examples, not product)

- 5xx ratio: `sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])) / sum(rate(http_server_request_duration_seconds_count[5m])) > 0.02`
- Readiness: `max(readyz_probe_failures)` from your probe system.
- Backlog age: `max(liftr_outbox_pending_oldest_age_seconds) > 3600`
- Long-running critical: `max(liftr_operations_long_running{liftr_severity="critical"}) > 0`
- Silence: `max(liftr_operations_reconciliation_silent) > 0`
- Sampler staleness: `time() - max(liftr_observability_sampler_last_success_unix_seconds) > 120`
- JWKS refresh failures: `increase(liftr_jwks_refresh_total{result="failure"}[15m]) > 5`

Remember: sampled gauges duplicate across replicas — aggregate with `max`,
not `sum`.
