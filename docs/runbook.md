# Liftr Operational Runbook

This runbook assumes M17/M18 signals: structured JSON logs, the Prometheus
endpoint on `LIFTR_METRICS_ADDR`, `/healthz` (liveness) and `/readyz`
(control-plane core readiness). Metric names follow the pinned OTel SDK
version; see the metric help strings for cluster-global vs per-process
semantics.

## Safety rules — read first

1. **Never casually mutate database state.** Durable rows are the only source
   of lifecycle truth; outbox terminal rows are immutable by trigger.
2. **Never delete Operation, Event, execution, or outbox rows.** There is no
   redrive in M17; quarantined (`Dead`) work stays until an approved redrive
   milestone exists.
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
   retry (`POST /v1/operations/{id}/retry`) remains the only sanctioned path.
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

Sequence on SIGTERM: readiness flips 503 → HTTP drains (10s) → metrics
listener stops → worker canceled and awaited (10s) → telemetry flushed → DB
closed. In-flight Submits that get canceled remain durably ambiguous and
recover via lease expiry + Observe after restart — expect (and ignore)
transient `lease_lost`/Unknown entries around the restart window. Telemetry
flush failures never affect lifecycle outcomes.

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
