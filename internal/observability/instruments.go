// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Bounded label vocabularies. Every metric attribute value in Liftr comes
// from one of these enums or a small closed set of route templates; unbounded
// identifiers (ResourceID, OperationID, PrincipalID, owner, ResourceType,
// ProvisionerRef, request/correlation IDs, Idempotency-Key) and arbitrary
// error text are forbidden as labels (ADR-0018).
const (
	attrCapability     = "liftr.capability"
	attrRetry          = "liftr.retry"
	attrOperationStat  = "liftr.operation.state"
	attrWorkerKind     = "liftr.worker.kind"
	attrWorkOutcome    = "liftr.worker.outcome"
	attrProvKind       = "liftr.provisioner.kind"
	attrProvOutcome    = "liftr.provisioner.outcome"
	attrProvMethod     = "liftr.provisioner.method"
	attrAuthResult     = "liftr.auth.result"
	attrAuthReason     = "liftr.auth.failure_reason"
	attrJWKSResult     = "liftr.jwks.result"
	attrPersistResult  = "liftr.persistence.result"
	attrSeverity       = "liftr.severity"
	attrPanicPhase     = "liftr.panic.phase"
	attrPolicyMutation = "liftr.policy.mutation"
	attrPolicyOutcome  = "liftr.policy.outcome"
)

// Panic phases for HTTP request handling.
const (
	PanicBeforeResponseCommit = "before_response_commit"
	PanicAfterResponseCommit  = "after_response_commit"
)

// Work outcomes are the bounded disposition of one worker work item. They
// mirror the worker package's outbox disposition vocabulary.
const (
	WorkOutcomeSuccess  = "success"
	WorkOutcomeRetry    = "retry"
	WorkOutcomeStale    = "stale"
	WorkOutcomeFailed   = "failed"
	WorkOutcomeLeaseLos = "lease_lost"
	WorkOutcomePanic    = "panic"
)

type instruments struct {
	httpDuration        metric.Float64Histogram
	activeRequests      metric.Int64UpDownCounter
	httpPanics          metric.Int64Counter
	correlationDropped  metric.Int64Counter
	authAttempts        metric.Int64Counter
	jwksRefreshes       metric.Int64Counter
	jwksRefreshDuration metric.Float64Histogram
	jwksLimited         metric.Int64Counter
	opsAdmitted         metric.Int64Counter
	opsTerminal         metric.Int64Counter
	opDuration          metric.Float64Histogram
	workerWork          metric.Int64Counter
	workerDuration      metric.Float64Histogram
	workerActive        metric.Int64UpDownCounter
	workerPanics        metric.Int64Counter
	provSubmissions     metric.Int64Counter
	provObservations    metric.Int64Counter
	provCallDuration    metric.Float64Histogram
	persistenceTxs      metric.Int64Counter
	persistenceTxMillis metric.Float64Histogram
	policyAdmissions    metric.Int64Counter

	poolAcquired   metric.Int64Gauge
	poolIdle       metric.Int64Gauge
	poolConnecting metric.Int64Gauge
	poolLimit      metric.Int64Gauge

	outboxPendingDepth  metric.Int64Gauge
	outboxPendingOldest metric.Int64Gauge
	outboxExpiredLeases metric.Int64Gauge
	outboxDead          metric.Int64Gauge
	opsActive           metric.Int64Gauge
	opsOldestActiveAge  metric.Int64Gauge
	opsLongRunning      metric.Int64Gauge
	opsSilent           metric.Int64Gauge
	samplerFreshness    metric.Int64Gauge
	samplerFailures     metric.Int64Counter
}

func newInstruments(meter metric.Meter) (*instruments, error) {
	var err error
	i := &instruments{}
	if i.httpDuration, err = meter.Float64Histogram("http.server.request.duration",
		metric.WithDescription("Duration of Liftr HTTP request handling."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.activeRequests, err = meter.Int64UpDownCounter("http.server.active_requests",
		metric.WithDescription("Requests currently being served by Liftr (per process)."),
		metric.WithUnit("{request}")); err != nil {
		return nil, err
	}
	if i.httpPanics, err = meter.Int64Counter("liftr.http.panics",
		metric.WithDescription("Recovered panics while serving HTTP requests."),
		metric.WithUnit("{panic}")); err != nil {
		return nil, err
	}
	if i.correlationDropped, err = meter.Int64Counter("liftr.http.correlation_ids_dropped",
		metric.WithDescription("Client-supplied X-Correlation-ID values rejected by sanitization."),
		metric.WithUnit("{id}")); err != nil {
		return nil, err
	}
	if i.authAttempts, err = meter.Int64Counter("liftr.authentication.attempts",
		metric.WithDescription("Authentication attempts classified by bounded result and typed failure reason."),
		metric.WithUnit("{attempt}")); err != nil {
		return nil, err
	}
	if i.jwksRefreshes, err = meter.Int64Counter("liftr.jwks.refreshes",
		metric.WithDescription("JWKS key-set refresh attempts."),
		metric.WithUnit("{refresh}")); err != nil {
		return nil, err
	}
	if i.jwksRefreshDuration, err = meter.Float64Histogram("liftr.jwks.refresh.duration",
		metric.WithDescription("Duration of JWKS key-set fetches."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.jwksLimited, err = meter.Int64Counter("liftr.jwks.forced_refresh_limited",
		metric.WithDescription("Forced JWKS refetches suppressed by the unknown-kid rate window."),
		metric.WithUnit("{refresh}")); err != nil {
		return nil, err
	}
	if i.opsAdmitted, err = meter.Int64Counter("liftr.operations.admitted",
		metric.WithDescription("Newly admitted Operations; idempotent replays are not counted."),
		metric.WithUnit("{operation}")); err != nil {
		return nil, err
	}
	if i.opsTerminal, err = meter.Int64Counter("liftr.operations.terminal",
		metric.WithDescription("Operations that reached a terminal state through an actual durable transition in this process; stale or replayed observations of already-terminal Operations are not counted."),
		metric.WithUnit("{operation}")); err != nil {
		return nil, err
	}
	if i.opDuration, err = meter.Float64Histogram("liftr.operation.duration",
		metric.WithDescription("End-to-end Operation latency from requestedAt to completedAt on real terminal transitions."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.workerWork, err = meter.Int64Counter("liftr.worker.work",
		metric.WithDescription("Outbox work items processed, by kind and outcome."),
		metric.WithUnit("{item}")); err != nil {
		return nil, err
	}
	if i.workerDuration, err = meter.Float64Histogram("liftr.worker.processing.duration",
		metric.WithDescription("Wall time to process one outbox work item including provider calls."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.workerActive, err = meter.Int64UpDownCounter("liftr.worker.active",
		metric.WithDescription("Work items currently executing in this process."),
		metric.WithUnit("{item}")); err != nil {
		return nil, err
	}
	if i.workerPanics, err = meter.Int64Counter("liftr.worker.panics",
		metric.WithDescription("Panics recovered at the per-work execution boundary."),
		metric.WithUnit("{panic}")); err != nil {
		return nil, err
	}
	if i.provSubmissions, err = meter.Int64Counter("liftr.provisioner.submissions",
		metric.WithDescription("Provisioner Submit calls by bounded outcome."),
		metric.WithUnit("{call}")); err != nil {
		return nil, err
	}
	if i.provObservations, err = meter.Int64Counter("liftr.provisioner.observations",
		metric.WithDescription("Provisioner Observe calls by bounded outcome."),
		metric.WithUnit("{call}")); err != nil {
		return nil, err
	}
	if i.provCallDuration, err = meter.Float64Histogram("liftr.provisioner.call.duration",
		metric.WithDescription("Provisioner call wall time."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.persistenceTxs, err = meter.Int64Counter("liftr.persistence.transactions",
		metric.WithDescription("Persistence transactions by bounded result classification."),
		metric.WithUnit("{transaction}")); err != nil {
		return nil, err
	}
	if i.persistenceTxMillis, err = meter.Float64Histogram("liftr.persistence.transaction.duration",
		metric.WithDescription("Persistence transaction wall time."),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.policyAdmissions, err = meter.Int64Counter("liftr.policy.admissions",
		metric.WithDescription("Platform policy decisions by bounded mutation and outcome."),
		metric.WithUnit("{admission}")); err != nil {
		return nil, err
	}
	if i.poolAcquired, err = meter.Int64Gauge("liftr.persistence.pool.acquired",
		metric.WithDescription("Connections currently acquired from this process's PostgreSQL pool.")); err != nil {
		return nil, err
	}
	if i.poolIdle, err = meter.Int64Gauge("liftr.persistence.pool.idle",
		metric.WithDescription("Idle connections in this process's PostgreSQL pool.")); err != nil {
		return nil, err
	}
	if i.poolConnecting, err = meter.Int64Gauge("liftr.persistence.pool.connecting",
		metric.WithDescription("Connections currently being established by this process's PostgreSQL pool.")); err != nil {
		return nil, err
	}
	if i.poolLimit, err = meter.Int64Gauge("liftr.persistence.pool.max",
		metric.WithDescription("Maximum connections configured for this process's PostgreSQL pool.")); err != nil {
		return nil, err
	}
	if i.outboxPendingDepth, err = meter.Int64Gauge("liftr.outbox.pending_depth",
		metric.WithDescription("Cluster-global count of Pending outbox messages; aggregate across replicas with max, not sum.")); err != nil {
		return nil, err
	}
	if i.outboxPendingOldest, err = meter.Int64Gauge("liftr.outbox.pending_oldest_age_seconds",
		metric.WithDescription("Cluster-global age in seconds of the oldest Pending outbox message by creation time; aggregate with max. Zero when the queue is empty: depth determines whether an oldest item exists.")); err != nil {
		return nil, err
	}
	if i.outboxExpiredLeases, err = meter.Int64Gauge("liftr.outbox.expired_leases",
		metric.WithDescription("Cluster-global count of Leased outbox messages whose lease has already expired (recovery lag signal); aggregate with max.")); err != nil {
		return nil, err
	}
	if i.outboxDead, err = meter.Int64Gauge("liftr.outbox.dead_total_count",
		metric.WithDescription("Cluster-global count of Dead (quarantined) outbox messages; aggregate with max.")); err != nil {
		return nil, err
	}
	if i.opsActive, err = meter.Int64Gauge("liftr.operations.active",
		metric.WithDescription("Cluster-global count of active Operations; aggregate with max, not sum.")); err != nil {
		return nil, err
	}
	if i.opsOldestActiveAge, err = meter.Int64Gauge("liftr.operations.oldest_active_age_seconds",
		metric.WithDescription("Cluster-global age in seconds of the oldest active Operation since requestedAt; aggregate with max. Zero when no active Operations exist.")); err != nil {
		return nil, err
	}
	if i.opsLongRunning, err = meter.Int64Gauge("liftr.operations.long_running",
		metric.WithDescription("Cluster-global count of long-running stuck candidates: active Operations whose total runtime exceeds the configured threshold. Diagnostic only; never a lifecycle state.")); err != nil {
		return nil, err
	}
	if i.opsSilent, err = meter.Int64Gauge("liftr.operations.reconciliation_silent",
		metric.WithDescription("Cluster-global count of reconciliation-silent stuck candidates: active Operations with no observation or phase activity beyond the configured window. Diagnostic only; never a lifecycle state.")); err != nil {
		return nil, err
	}
	if i.samplerFreshness, err = meter.Int64Gauge("liftr.observability.sampler.last_success_unix_seconds",
		metric.WithDescription("Unix timestamp of the last successful operational sample; staleness marker for every sampled gauge.")); err != nil {
		return nil, err
	}
	if i.samplerFailures, err = meter.Int64Counter("liftr.observability.sampler.failures",
		metric.WithDescription("Failed operational sampling cycles.")); err != nil {
		return nil, err
	}
	return i, nil
}

// --- HTTP transport sink ---------------------------------------------------

// HTTPRequestStarted increments the in-flight gauge.
func (t *Telemetry) HTTPRequestStarted() {
	if !t.ready() {
		return
	}
	t.instruments.activeRequests.Add(context.Background(), 1)
}

// HTTPRequestFinished records one served request using standard semantic
// conventions: http.server.request.duration with http.request.method,
// http.route (bounded template), and http.response.status_code.
func (t *Telemetry) HTTPRequestFinished(route, method string, status int, duration time.Duration) {
	if !t.ready() {
		return
	}
	ctx := context.Background()
	t.instruments.activeRequests.Add(ctx, -1)
	t.instruments.httpDuration.Record(ctx, duration.Seconds(),
		metric.WithAttributes(
			attributeString("http.request.method", method),
			attributeString("http.route", route),
			attributeInt("http.response.status_code", status),
		))
}

// HTTPPanicRecovered counts one recovered handler panic.
func (t *Telemetry) HTTPPanicRecovered(beforeCommit bool) {
	if !t.ready() {
		return
	}
	phase := PanicAfterResponseCommit
	if beforeCommit {
		phase = PanicBeforeResponseCommit
	}
	t.instruments.httpPanics.Add(context.Background(), 1,
		metric.WithAttributes(attributeString(attrPanicPhase, phase)))
}

// CorrelationIDDropped counts one rejected client correlation ID.
func (t *Telemetry) CorrelationIDDropped() {
	if !t.ready() {
		return
	}
	t.instruments.correlationDropped.Add(context.Background(), 1)
}

// AuthenticationObserved records one authentication attempt with the typed
// reason rendered as a bounded label.
func (t *Telemetry) AuthenticationObserved(success bool, reason identity.AuthFailureReason) {
	if !t.ready() {
		return
	}
	attrs := []attribute.KeyValue{attributeString(attrAuthResult, authResultLabel(success))}
	if !success {
		attrs = append(attrs, attributeString(attrAuthReason, reason.String()))
	}
	t.instruments.authAttempts.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// JWKSRefreshed records one background key-set fetch.
func (t *Telemetry) JWKSRefreshed(success bool, duration time.Duration) {
	if !t.ready() {
		return
	}
	ctx := context.Background()
	result := "failure"
	if success {
		result = "success"
	}
	t.instruments.jwksRefreshes.Add(ctx, 1, metric.WithAttributes(attributeString(attrJWKSResult, result)))
	t.instruments.jwksRefreshDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attributeString(attrJWKSResult, result)))
}

// ForcedRefreshLimited records a rate-window-suppressed JWKS refetch.
func (t *Telemetry) ForcedRefreshLimited() {
	if !t.ready() {
		return
	}
	t.instruments.jwksLimited.Add(context.Background(), 1)
}

func authResultLabel(success bool) string {
	if success {
		return "success"
	}
	return "failure"
}

// --- Lifecycle -------------------------------------------------------------

// OperationAdmitted counts exactly one NEW durable admission. Idempotent
// replays must not be reported by callers (ADR-0018).
func (t *Telemetry) OperationAdmitted(capability domain.Capability, retry bool) {
	if !t.ready() {
		return
	}
	retryLabel := "false"
	if retry {
		retryLabel = "true"
	}
	t.instruments.opsAdmitted.Add(context.Background(), 1, metric.WithAttributes(
		attributeString(attrCapability, string(capability)),
		attributeString(attrRetry, retryLabel),
	))
}

// --- Persistence -----------------------------------------------------------

// TransactionFinished classifies one completed persistence transaction.
func (t *Telemetry) TransactionFinished(result string, duration time.Duration) {
	if !t.ready() {
		return
	}
	ctx := context.Background()
	t.instruments.persistenceTxs.Add(ctx, 1, metric.WithAttributes(attributeString(attrPersistResult, result)))
	t.instruments.persistenceTxMillis.Record(ctx, duration.Seconds(),
		metric.WithAttributes(attributeString(attrPersistResult, result)))
}

// PoolStats is the per-process pgx pool snapshot recorded by the sampler.
type PoolStats struct {
	Acquired   int64
	Idle       int64
	Connecting int64
	MaxTotal   int64
}

func (t *Telemetry) recordPoolStats(stats PoolStats) {
	ctx := context.Background()
	t.instruments.poolAcquired.Record(ctx, stats.Acquired)
	t.instruments.poolIdle.Record(ctx, stats.Idle)
	t.instruments.poolConnecting.Record(ctx, stats.Connecting)
	t.instruments.poolLimit.Record(ctx, stats.MaxTotal)
}
func attributeString(key, value string) attribute.KeyValue { return attribute.String(key, value) }

func attributeInt(key string, value int) attribute.KeyValue { return attribute.Int(key, value) }

// ready reports whether instruments exist. Nil *Telemetry (or a zero-value
// instance) is always safe: telemetry must never be able to affect behavior.
func (t *Telemetry) ready() bool {
	return t != nil && t.instruments != nil
}
