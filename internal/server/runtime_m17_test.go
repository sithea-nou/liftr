// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/observability"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/server"
)

// ---- helpers ---------------------------------------------------------------

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newM17Telemetry(t *testing.T) *observability.Telemetry {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	telemetry, err := observability.NewTelemetry(context.Background(), observability.Config{ServiceVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return telemetry
}

// m17Fixture composes a full Runtime over fakes with telemetry enabled.
func composeInstrumented(t *testing.T) (*server.Runtime, *applicationfake.Store, application.ProvisionerRef) {
	t.Helper()
	store := applicationfake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Fake resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{
		provisioningfake.ResourceType(): typeValue,
	}}
	ref, refErr := application.NewProvisionerRef("m17-provider")
	if refErr != nil {
		t.Fatal(refErr)
	}
	runtime, composeErr := server.Compose(server.Config{
		Transactions: store,
		Catalog:      catalog,
		Provisioners: map[application.ProvisionerRef]provisioning.Provisioner{
			ref: provisioningfake.New(provisioningfake.ModeSynchronous),
		},
		DefaultProvisionerRef: ref,
		Logger:                testLogger(t),
		Telemetry:             newM17Telemetry(t),
		ProvisionerKinds: map[application.ProvisionerRef]observability.ProvisionerKind{
			ref: observability.ProvisionerKindPulumi,
		},
		Sampler:      &m17SamplerStub{},
		InsecureAuth: true,
	})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	return runtime, store, ref
}

// Compose must refuse arbitrary provisioner refs without a code-defined kind:
// unbounded refs can never become metric dimensions (ADR-0018).
func TestComposeRequiresCodeDefinedProvisionerKinds(t *testing.T) {
	store := applicationfake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Fake resource",
		[]domain.Capability{domain.CapabilityCreate})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{
		provisioningfake.ResourceType(): typeValue,
	}}
	ref, refErr := application.NewProvisionerRef("anything-goes")
	if refErr != nil {
		t.Fatal(refErr)
	}
	telemetry := newM17Telemetry(t)
	if _, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)},
		DefaultProvisionerRef: ref,
		Telemetry:             telemetry,
	}); err == nil {
		t.Fatal("compose accepted telemetry without provisioner kinds")
	}
	if _, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)},
		DefaultProvisionerRef: ref,
		Telemetry:             telemetry,
		ProvisionerKinds:      map[application.ProvisionerRef]observability.ProvisionerKind{ref: "mystery-backend"},
	}); err == nil {
		t.Fatal("compose accepted an unknown provisioner kind enum value")
	}
}

// Readiness flips false before HTTP draining starts while liveness stays up.
func TestReadinessFlipsFalseWhenDraining(t *testing.T) {
	runtime, _, _ := composeInstrumented(t)
	handler := runtime.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("readyz before draining = %d", response.Code)
	}

	runtime.SetDraining()
	drained := httptest.NewRecorder()
	handler.ServeHTTP(drained, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if drained.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", drained.Code)
	}
	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("healthz must stay alive during drain, got %d", live.Code)
	}
}

// The operational sampler records cluster truth on success and retains prior
// gauges with a failure counter on transient errors; it never crashes and
// never blocks the process.
func TestOperationalSamplerRetainsValuesAndCountsFailures(t *testing.T) {
	sampler := &m17SamplerStub{err: errors.New("synthetic sampling outage")}
	telemetry := newM17Telemetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go samplerLoopForTest(ctx, sampler, telemetry)

	waitFor(t, 2*time.Second, func() bool { return scrapeCounter(t, telemetry, "liftr_observability_sampler_failures_total") > 0 })

	sampler.mu.Lock()
	sampler.err = nil
	sampler.next = observability.ClusterSample{
		SampledAt:          time.Unix(1700000000, 0),
		OutboxPendingDepth: 7,
		Pool:               observability.PoolStats{Acquired: 3, MaxTotal: 9},
	}
	sampler.mu.Unlock()

	waitFor(t, 2*time.Second, func() bool { return scrapeGauge(t, telemetry, "liftr_outbox_pending_depth") == 7 })
	if got := scrapeGauge(t, telemetry, "liftr_outbox_pending_depth"); got != 7 {
		t.Fatalf("pending depth gauge = %f, want 7", got)
	}
	if got := scrapeGauge(t, telemetry, "liftr_observability_sampler_last_success_unix_seconds"); got != 1700000000 {
		t.Fatalf("freshness gauge = %f, want sample timestamp", got)
	}
}

func samplerLoopForTest(ctx context.Context, sampler *m17SamplerStub, telemetry *observability.Telemetry) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample, err := sampler.SnapshotOperationalState(ctx, server.DiagnosticThresholds{})
			if err != nil {
				telemetry.SampleFailed()
				continue
			}
			telemetry.RecordClusterSample(sample)
		}
	}
}

func waitFor(t *testing.T, budget time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met within the budget")
}

// scrape helpers read the Prometheus registry text.
func scrapeText(t *testing.T, telemetry *observability.Telemetry) string {
	t.Helper()
	families, err := telemetry.PrometheusRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			switch {
			case metric.GetCounter() != nil:
				fmt.Fprintf(&out, "%s %f\n", family.GetName(), metric.GetCounter().GetValue())
			case metric.GetGauge() != nil:
				fmt.Fprintf(&out, "%s %f\n", family.GetName(), metric.GetGauge().GetValue())
			}
		}
	}
	return out.String()
}

func scrapeCounter(t *testing.T, telemetry *observability.Telemetry, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(scrapeText(t, telemetry), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			var value float64
			fmt.Sscanf(fields[1], "%f", &value)
			return value
		}
	}
	return -1
}

func scrapeGauge(t *testing.T, telemetry *observability.Telemetry, name string) float64 {
	t.Helper()
	return scrapeCounter(t, telemetry, name)
}

type m17SamplerStub struct {
	mu   sync.Mutex
	err  error
	next observability.ClusterSample
}

func (s *m17SamplerStub) SnapshotOperationalState(context.Context, server.DiagnosticThresholds) (observability.ClusterSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next, s.err
}
