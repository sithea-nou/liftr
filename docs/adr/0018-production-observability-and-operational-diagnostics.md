# ADR-0018: Production Observability and Operational Diagnostics

Date: 2026-08-25
Status: Accepted

## Context

Liftr now runs as a real control plane: a public `/v1` API, durable PostgreSQL
state with a transactional outbox, leased workers driving Pulumi and
Crossplane provisioning, delegated Backstage clients, and no operational
signals beyond nine startup log lines and two health endpoints. An operator
cannot answer whether the API serves normally, whether workers progress,
whether the outbox drains, or why one developer's request disappeared —
without database access.

M17 adds production operability without changing any developer-facing
lifecycle contract.

## Decision

### Observability describes Liftr; it is never a second source of truth

Durable Resource / Operation / Event state remains authoritative. Telemetry
never participates in lifecycle decisions, is never required for correctness,
and can always be disabled by simply not configuring it. There is no strict-
telemetry mode.

### Signal families

- **Structured logs** (stdlib `log/slog`, JSON by default): the primary
  diagnostic surface and the only signal carrying diagnostic identifiers.
  Level (`LIFTR_LOG_LEVEL`) and format (`LIFTR_LOG_FORMAT`) are configurable;
  DEBUG carries high-volume internal evidence such as steady-state observation
  polling so INFO stays human-scale.
- **Metrics**: OpenTelemetry instruments exported through a Prometheus-
  compatible pull endpoint plus optional OTLP push. Standard semantic names
  (`http.server.request.duration` with `http.route`,
  `http.request.method`, `http.response.status_code`) are used where
  conventions exist; Liftr concepts live under the `liftr.` namespace.
- **Traces**: minimal boundary tracing only — HTTP root spans, provisioner
  Submit/Observe spans, and independent worker work-item roots joined to
  admissions through `operation_id` and structured logs. **Trace context is
  never persisted in durable outbox state.** Tracing is inert unless an OTLP
  endpoint is configured; sampling follows the standard
  `OTEL_TRACES_SAMPLER(_ARG)` variables and never affects execution.

### Activity is not progress

Observation receipt is activity, not progress: a backend may be polled every
few seconds yet never converge forever. Diagnostics therefore separate:

1. **Long-running Operation** (diagnostic): an active Operation
   (Pending | Running) whose total runtime (`now - requestedAt`) exceeds a
   configured threshold (`LIFTR_OBSERVABILITY_LONG_RUNNING_WARN_AFTER` /
   `_CRIT_AFTER`). It means only that the Operation has been active unusually
   long.
2. **Reconciliation silence** (diagnostic): an active Operation with no
   reconciliation ACTIVITY within a configured window
   (`LIFTR_OBSERVABILITY_RECONCILIATION_SILENT_AFTER`), measured from durable
   activity timestamps (`executions.last_observed_at_ns`,
   `operations.phase_changed_at_ns`, `started_at_ns`). It diagnoses stopped
   workers, stranded outbox items, or observations not occurring — not lack of
   backend progress.

Both are exposed as sampled cluster-global gauges ("stuck candidates" in the
runbook). Neither ever mutates Operation or Resource state, marks anything
Failed, or affects readiness. Liftr continues to own no timeout policy
(ADR-0015).

### Metric event counters count real committed transitions

- `liftr.operations.admitted` counts NEW Operations only. Idempotent replays
  that return the original Operation are structurally distinguished by
  `application.Result.Replay` at the owning boundary and are never counted;
  the distinction is never inferred from HTTP status.
- Terminal counters increment exactly when the current transaction performs a
  Pending/Running → terminal transition (captured after `SaveOperation`
  succeeds, flushed only when that transaction commits). Stale outbox items
  re-observing an already-terminal Operation settle as stale and never count.

Worker disposition telemetry distinguishes two diagnoses that must never be
conflated: **ambiguous** (the external submission outcome is uncertain while
this worker provably still owns the fenced lease) and **lease_lost**
(fencing ownership was lost through heartbeat renewal failure or another
claimant moving durable state). Provisioner submit metrics independently
record `outcome="ambiguous"`. No raw error text ever becomes a label.

Counters remain non-authoritative: a crash between DB commit and counter
increment may lose one metric event. That is acceptable and documented here;
what must never happen — systematic double-counting through idempotent replay
or stale outbox settlement — is prevented structurally and pinned by tests.

### Cardinality and privacy policy

Metric labels come exclusively from bounded enums or bounded route templates:

- Allowed dimensions: method, route template, status code/class, capability,
  operation state, execution/outcome enums, worker kind/outcome,
  authentication result and typed reason, persistence result, resource state,
  severity, provisioner kind.
- **ProvisionerRef is diagnostic, not a dimension**: private refs are
  deployment-controlled strings whose length bound says nothing about their
  cardinality. Metrics carry the closed software-defined
  `liftr.provisioner.kind` enum (`pulumi`, `crossplane`; new backends add one
  code-defined value). Refs remain available in structured logs and sampled
  traces. Composition refuses to start instrumented provisioners whose ref
  lacks a valid kind.
- Forbidden as labels everywhere, without exception: ResourceID,
  OperationID, PrincipalID, owner identity, ResourceType name/version,
  request/correlation IDs, Idempotency-Key, arbitrary error text.
- Diagnostic identifiers (request_id, correlation_id, operation_id,
  resource_id, attempt number) are allowed in operator-controlled logs and
  sampled traces. PrincipalID appears only where it materially aids mutation/
  audit diagnostics — not in generic read access logs; Audit Events remain the
  authoritative actor record.

Sensitive material — tokens, Authorization headers, JWT claims, kubeconfig
material, Pulumi secrets/config/output, ResourceSpec/Outputs wholesale, raw
provider diagnostics — is never passed to a logger at all. That structural
avoidance is the primary defense; redaction-style sanitization exists only as
defense-in-depth for free-text fields (bounded length, control characters
flattened), pinned by canary tests. Auth failure reporting uses a typed
reason enum owned by the identity/authentication vocabulary; the public 401
response remains fully indistinguishable across reasons.

Client-supplied `X-Correlation-ID` is sanitized before echo (trimmed, ≤128
bytes, printable ASCII); hostile values are dropped and counted, never
echoed or logged.

### Dependency boundaries

Only `internal/observability` and `cmd/liftr-server` import telemetry
libraries. Core packages (`domain`, `lifecycle`, `resourcecontract`,
`identity`) and every lifecycle-executing package (`application`, `worker`,
`provisioning`, `persistence/postgres`, `auth`) stay telemetry-free and expose
consumer-side ports that composition wires onto instrumentation. Architecture
tests enforce this continuously.

### Configuration

Standard OTel environment variables govern standard OTel behavior
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_SAMPLER`,
`OTEL_TRACES_SAMPLER_ARG`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`).
**M17 deliberately supports OTLP/gRPC only** (`OTEL_EXPORTER_OTLP_PROTOCOL=
grpc`, the default): any other protocol value fails startup as a clear
configuration error and never silently falls back or drops telemetry.
OTLP/HTTP remains an incremental future enhancement.
The OTel resource carries `service.name` and build-stamped `service.version`.
Liftr-specific knobs live under `LIFTR_`: `LIFTR_METRICS_ADDR`,
`LIFTR_LOG_LEVEL`, `LIFTR_LOG_FORMAT`, `LIFTR_OBSERVABILITY_SAMPLE_INTERVAL`,
and the three diagnostic thresholds above. Invalid configuration fails
startup with one structured error and no secret material.

The exact set of exported metric names, histogram buckets, and the Prometheus
representation are NOT frozen by this ADR; they follow the pinned OTel SDK /
exporter versions and live in operational documentation. `/metrics` is an
operator interface governed by the pinned observability dependency, not by
Liftr API versioning.

### Metrics exposure model

`/metrics` is served on a dedicated listener bound to an explicitly
configured address (`LIFTR_METRICS_ADDR`). It is disabled by default; there
is no default public exposure and no authenticated-metrics path on the main
listener. Metrics can reveal rates, volumes, and backend kinds, so exposure
is a deliberate operator decision handled at the network boundary.

### Readiness, liveness, and shutdown

- `healthz`: process liveness only, unchanged.
- `readyz`: control-plane CORE readiness — PostgreSQL usable, schema verified
  at startup (via the previously unused `VerifySchema`; mismatch fails boot),
  and the process not draining. It deliberately excludes provisioners and
  authentication infrastructure: Resource reads and durable control-plane
  behavior survive backend and IdP outages, which surface through metrics and
  logs instead.
- Graceful shutdown order: readiness false → HTTP drains → metrics listener
  stops → worker canceled and awaited boundedly (leases stay intact; canceled
  or panicked Submits convert to durable Unknown/Observe recovery, never to
  definitive failure — M6/M14 ambiguity safety is inviolable) → telemetry
  flushed boundedly (flush failure never alters persisted outcomes) →
  PostgreSQL closes. Leases are never eagerly released just because the
  process is exiting.

### Panic boundaries

HTTP panics respect response commitment: before commit, one generic
sanitized INTERNAL problem with requestId; after commit, nothing further is
written — a Problem document is never appended to a partially committed
response, and no stack trace or panic value ever reaches a client. Worker
panics are recovered at the per-work boundary: the panic is logged
sanitized and counted, the item's lease stays intact for existing expiry
recovery, and the loop continues; a panic never marks work successful or
failed. Startup composition failures remain fail-fast.

### Cluster-global vs per-process signals

Operational-sampler gauges query cluster-global PostgreSQL truth, so every
replica exposes identical values: dashboards must aggregate them with `max`
(or last), never `sum`. Per-process signals (pool stats, panics, in-flight
requests) aggregate per replica. Metric help strings record this distinction;
no leader election is introduced for metrics in M17.

### Sampling cost discipline

The operational sampler runs periodically in its own goroutine under a strict
context budget — never synchronously inside a scrape or request. Its
aggregates ride existing partial indexes plus one new partial Dead index
(000008), chosen from measured plan evidence because terminal outbox rows are
immutable and retained forever. Transient sample failures retain previous
gauge values, count once, warn boundedly, and expose a last-success
freshness timestamp; they never crash the process or monopolize the database.

### No administrative surface

M17 adds no admin redrive, force-retry, cancellation, timeout, repair APIs,
or diagnostics endpoints that disclose topology. Dead-letter redrive remains
a separate future milestone requiring its own work-identity decision.

## Consequences

- Operators gain correlated logs (request → admission → operation → worker →
  provisioner), low-cardinality metrics for every plane, honest stuck-candidate
  diagnostics, and safe shutdown — with zero change to the public `/v1`
  contract, lifecycle semantics, or ambiguity invariants.
- Telemetry is structurally incapable of affecting durable outcomes; exporter
  outages degrade signal freshness, never availability.
- Future provisioners must register a code-defined kind; future verifier
  failure paths must map onto the typed reason enum or collapse into `Other`.
