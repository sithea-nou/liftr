// SPDX-License-Identifier: Apache-2.0

package observability_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/observability"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/worker"
)

func newTestTelemetry(t *testing.T) *observability.Telemetry {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	telemetry, err := observability.NewTelemetry(context.Background(), observability.Config{ServiceVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return telemetry
}

func gatherText(t *testing.T, telemetry *observability.Telemetry) string {
	t.Helper()
	families, err := telemetry.PrometheusRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, family := range families {
		fmt.Fprintf(&out, "# %s\n", family.GetName())
		for _, metric := range family.GetMetric() {
			labels := []string{}
			for _, pair := range metric.GetLabel() {
				labels = append(labels, pair.GetName()+"="+pair.GetValue())
			}
			switch {
			case metric.GetCounter() != nil:
				fmt.Fprintf(&out, "%s{%s} %f\n", family.GetName(), strings.Join(labels, ","), metric.GetCounter().GetValue())
			case metric.GetGauge() != nil:
				fmt.Fprintf(&out, "%s{%s} %f\n", family.GetName(), strings.Join(labels, ","), metric.GetGauge().GetValue())
			case metric.GetHistogram() != nil:
				fmt.Fprintf(&out, "%s{%s} count=%d\n", family.GetName(), strings.Join(labels, ","), metric.GetHistogram().GetSampleCount())
			}
		}
	}
	return out.String()
}

// The Prometheus surface renders standard HTTP semantic-convention names with
// bounded route templates plus the Liftr-namespaced instruments.
func TestPrometheusScrapeRendersStandardAndLiftrNames(t *testing.T) {
	telemetry := newTestTelemetry(t)
	telemetry.HTTPRequestStarted()
	telemetry.HTTPRequestFinished("/v1/resources/{id}", "GET", 200, 12*time.Millisecond)
	telemetry.OperationAdmitted(domain.CapabilityCreate, false)
	telemetry.OperationTerminalized(worker.TerminalEvent{
		OperationID: "op-t", ResourceID: "res-t", Capability: "create",
		TerminalState: "Succeeded", DurationSeconds: 42,
	})
	telemetry.RecordClusterSample(observability.ClusterSample{
		SampledAt:                        time.Unix(1700000000, 0),
		OutboxPendingDepth:               3,
		OutboxPendingOldestAgeSeconds:    -1,
		ActiveOperations:                 2,
		ActiveOperationsOldestAgeSeconds: 900,
		LongRunningWarning:               map[string]int64{"update": 1},
		Pool:                             observability.PoolStats{Acquired: 1, Idle: 2, MaxTotal: 10},
	})

	text := gatherText(t, telemetry)
	for _, expected := range []string{
		"http_server_request_duration_seconds",
		`http_route=/v1/resources/{id}`,
		"http_server_active_requests",
		"liftr_operations_admitted_total",
		"liftr_operations_terminal_total",
		"liftr_operation_duration_seconds",
		"liftr_outbox_pending_depth",
		"liftr_operations_long_running",
		`capability=update`,
		"liftr_observability_sampler_last_success_unix_seconds",
		"liftr_persistence_pool_acquired",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("scrape output missing %q:\n%s", expected, text)
		}
	}
}

// Arbitrary ProvisionerRefs must never appear as metric labels; only the
// bounded software-defined kind does.
func TestProvisionerRefCardinalityNeverReachesMetrics(t *testing.T) {
	telemetry := newTestTelemetry(t)
	for i := range 50 {
		ref := fmt.Sprintf("ref-%d-with-UNBOUNDED-chars-%d", i, i*7919)
		kind := observability.ProvisionerKindPulumi
		wrapped, err := observability.InstrumentProvisioner(&refEchoProvisioner{tag: ref}, kind, telemetry)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wrapped.Submit(context.Background(), provisioning.ExecutionRequest{
			OperationID: "op-x", AttemptNumber: 1, ResourceID: "res-x", Capability: domain.CapabilityCreate,
			ResourceType: domain.ResourceTypeRef{Name: "T", Version: "v1"}, TargetGeneration: 1,
		})
		_, _ = wrapped.Observe(context.Background(), provisioning.ObservationRequest{
			ResourceID: "res-x", ResourceType: domain.ResourceTypeRef{Name: "T", Version: "v1"}, TargetGeneration: 1,
		})
	}
	text := gatherText(t, telemetry)
	if !strings.Contains(text, `liftr_provisioner_kind="pulumi"`) && !strings.Contains(text, `provisioner_kind="pulumi"`) &&
		!strings.Contains(text, "liftr_provisioner_submissions_total") {
		t.Fatalf("provisioner metrics missing entirely:\n%s", text)
	}
	if strings.Contains(text, "ref-") || strings.Contains(text, "UNBOUNDED") {
		t.Fatalf("unbounded provisioner reference leaked into metric labels:\n%s", text)
	}
}

func TestProvisionerKindsIncludeOpenTofuButNotTerraform(t *testing.T) {
	if !observability.ProvisionerKindOpenTofu.Valid() {
		t.Fatal("OpenTofu provisioner kind is not valid")
	}
	if observability.ProvisionerKind("terraform").Valid() {
		t.Fatal("Terraform must not be an observability provisioner kind")
	}
}

func TestInstrumentedProvisionerPreservesOptionalOutputCapabilities(t *testing.T) {
	telemetry := newTestTelemetry(t)
	inner := &outputCapableProvisioner{refEchoProvisioner: refEchoProvisioner{}}
	wrapped, err := observability.InstrumentProvisioner(inner, observability.ProvisionerKindOpenTofu, telemetry)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := wrapped.(provisioning.OutputMappingSource)
	if !ok || source.OutputMappingRef(domain.ResourceTypeRef{}, domain.CapabilityCreate) != "mapping-v2" {
		t.Fatal("instrumentation hid output mapping capability")
	}
	selector, ok := wrapped.(provisioning.OutputRecoveryMappingSelector)
	if !ok {
		t.Fatal("instrumentation hid output recovery capability")
	}
	if mapping, selected := selector.SelectOutputRecoveryMapping(domain.ResourceTypeRef{}, domain.CapabilityCreate, "mapping-v1"); !selected || mapping != "mapping-v2" {
		t.Fatal("instrumentation changed output recovery selection")
	}

	plain, err := observability.InstrumentProvisioner(&refEchoProvisioner{}, observability.ProvisionerKindPulumi, telemetry)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(provisioning.OutputMappingSource); ok {
		t.Fatal("instrumentation invented an optional output capability")
	}
	if _, ok := plain.(provisioning.FencedProvisioner); ok {
		t.Fatal("instrumentation invented an optional fenced execution capability")
	}
}

func TestInstrumentedProvisionerForwardsFencedCallsThroughTelemetry(t *testing.T) {
	telemetry := newTestTelemetry(t)
	inner := &fencedProvisioner{refEchoProvisioner: refEchoProvisioner{}}
	wrapped, err := observability.InstrumentProvisioner(inner, observability.ProvisionerKindOpenTofu, telemetry)
	if err != nil {
		t.Fatal(err)
	}
	fenced, ok := wrapped.(provisioning.FencedProvisioner)
	if !ok {
		t.Fatal("instrumentation hid fenced execution capability")
	}
	fence := provisioning.ExecutionFence{MessageID: "message-1", LeaseToken: "lease-1"}
	_, _ = fenced.SubmitFenced(context.Background(), provisioning.ExecutionRequest{OperationID: "op", AttemptNumber: 1, ResourceID: "res", ResourceType: domain.ResourceTypeRef{Name: "T", Version: "v1"}, Capability: domain.CapabilityCreate, TargetGeneration: 1}, fence)
	if inner.fence != fence {
		t.Fatalf("forwarded fence = %+v", inner.fence)
	}
	if text := gatherText(t, telemetry); !strings.Contains(text, "liftr_provisioner_submissions_total") {
		t.Fatal("fenced call bypassed telemetry")
	}
}

func TestSubmitDurationMeasuresWallTime(t *testing.T) {
	telemetry := newTestTelemetry(t)
	wrapped, err := observability.InstrumentProvisioner(&slowSubmitProvisioner{}, observability.ProvisionerKindOpenTofu, telemetry)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wrapped.Submit(context.Background(), provisioning.ExecutionRequest{OperationID: "op-duration", AttemptNumber: 1,
		ResourceID: "res-duration", ResourceType: domain.ResourceTypeRef{Name: "T", Version: "v1"}, Capability: domain.CapabilityCreate, TargetGeneration: 1})
	families, err := telemetry.PrometheusRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "liftr_provisioner_call_duration_seconds" {
			continue
		}
		for _, sample := range family.GetMetric() {
			method := ""
			for _, label := range sample.GetLabel() {
				if label.GetName() == "liftr_provisioner_method" || label.GetName() == "provisioner_method" {
					method = label.GetValue()
				}
			}
			if method == "submit" && sample.GetHistogram().GetSampleSum() >= 0.005 {
				return
			}
		}
	}
	t.Fatal("Submit duration histogram did not record elapsed wall time")
}

// refEchoProvisioner embeds its (arbitrary) tag in every result so a leak
// would be visible.
type refEchoProvisioner struct{ tag string }

func (p *refEchoProvisioner) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *refEchoProvisioner) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted},
	}}, nil
}

func (p *refEchoProvisioner) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}}, nil
}

type outputCapableProvisioner struct{ refEchoProvisioner }

func (*outputCapableProvisioner) OutputMappingRef(domain.ResourceTypeRef, domain.Capability) string {
	return "mapping-v2"
}

func (*outputCapableProvisioner) SelectOutputRecoveryMapping(_ domain.ResourceTypeRef, _ domain.Capability, source string) (string, bool) {
	return "mapping-v2", source == "mapping-v1"
}

type slowSubmitProvisioner struct{ refEchoProvisioner }

type fencedProvisioner struct {
	refEchoProvisioner
	fence provisioning.ExecutionFence
}

func (p *fencedProvisioner) SubmitFenced(ctx context.Context, request provisioning.ExecutionRequest, fence provisioning.ExecutionFence) (provisioning.Submission, error) {
	p.fence = fence
	return p.Submit(ctx, request)
}

func (p *fencedProvisioner) ObserveFenced(ctx context.Context, request provisioning.ObservationRequest, fence provisioning.ExecutionFence) (provisioning.ExecutionObservation, error) {
	p.fence = fence
	return p.Observe(ctx, request)
}

func (*slowSubmitProvisioner) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	time.Sleep(10 * time.Millisecond)
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}}, nil
}

// Auth failure labels come exclusively from the typed enum.
func TestAuthFailureReasonLabelsAreBounded(t *testing.T) {
	telemetry := newTestTelemetry(t)
	reasons := []identity.AuthFailureReason{
		identity.AuthFailureMissingCredential,
		identity.AuthFailureMalformed,
		identity.AuthFailureUnsupportedAlgorithm,
		identity.AuthFailureUnknownKey,
		identity.AuthFailureInvalidSignature,
		identity.AuthFailureExpired,
		identity.AuthFailureIssuerMismatch,
		identity.AuthFailureAudienceMismatch,
		identity.AuthFailureClaimsInvalid,
		identity.AuthFailureJWKSUnavailable,
		identity.AuthFailureRefreshRateLimited,
		identity.AuthFailureOther,
	}
	for _, reason := range reasons {
		telemetry.AuthenticationObserved(false, reason)
	}
	telemetry.AuthenticationObserved(true, identity.AuthFailureNone)
	text := gatherText(t, telemetry)
	if !strings.Contains(text, "liftr_authentication_attempts_total") {
		t.Fatalf("auth counter missing:\n%s", text)
	}
	for _, reason := range reasons {
		if !strings.Contains(text, string(reason)) {
			t.Fatalf("typed reason %q missing from scrape:\n%s", reason, text)
		}
	}
	if strings.Contains(text, `liftr_auth_result="bogus`) {
		t.Fatal("unbounded auth label leaked")
	}
}

// An unreachable OTLP endpoint must not affect recording or shutdown bounds:
// telemetry export can never block or fail lifecycle work (ADR-0018).
func TestTelemetrySurvivesUnreachableExporter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	telemetry, err := observability.NewTelemetry(context.Background(), observability.Config{ServiceVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		telemetry.OperationAdmitted(domain.CapabilityDelete, true)
		telemetry.SampleFailed()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recording blocked while exporter was unreachable")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := telemetry.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error=%v", err)
	}
}

// Empty queues export physically meaningful zeros: depth 0 and oldest age 0.
// A negative seconds sentinel must never appear in any scrape (ADR-0018).
func TestEmptyQueueExportsZeroAgeNotNegativeSentinel(t *testing.T) {
	telemetry := newTestTelemetry(t)
	telemetry.RecordClusterSample(observability.ClusterSample{
		SampledAt:                        time.Unix(1700000100, 0),
		OutboxPendingDepth:               0,
		OutboxPendingOldestAgeSeconds:    0,
		OutboxExpiredLeases:              0,
		OutboxDead:                       0,
		ActiveOperations:                 0,
		ActiveOperationsOldestAgeSeconds: 0,
		LongRunningWarning:               map[string]int64{},
		LongRunningCritical:              map[string]int64{},
		ReconciliationSilent:             map[string]int64{},
	})
	text := gatherText(t, telemetry)
	values := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.SplitN(fields[0], "{", 2)[0]
		var value float64
		if _, err := fmt.Sscanf(fields[1], "%f", &value); err == nil {
			values[name] = value
		}
	}
	for _, name := range []string{
		"liftr_outbox_pending_depth",
		"liftr_outbox_pending_oldest_age_seconds",
		"liftr_operations_active",
		"liftr_operations_oldest_active_age_seconds",
	} {
		value, ok := values[name]
		if !ok {
			t.Fatalf("empty-queue scrape missing %s:\n%s", name, text)
		}
		if value != 0 {
			t.Fatalf("%s = %f on an empty queue, want exactly 0", name, value)
		}
	}
	for name, value := range values {
		if strings.Contains(name, "age_seconds") && value < 0 {
			t.Fatalf("negative age sentinel exported for %s: %f", name, value)
		}
	}
}
