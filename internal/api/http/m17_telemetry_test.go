// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

// telemetryStub records transport telemetry calls for assertions.
type telemetryStub struct {
	mu          sync.Mutex
	started     int
	finished    []recordedRequest
	panics      []bool
	dropped     int
	authResults []recordedAuth
	admissions  []recordedAdmission
}

type recordedRequest struct {
	route  string
	method string
	status int
}

type recordedAuth struct {
	success bool
	reason  identity.AuthFailureReason
}

type recordedAdmission struct {
	capability domain.Capability
	retry      bool
}

func (s *telemetryStub) HTTPRequestStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started++
}

func (s *telemetryStub) HTTPRequestFinished(route string, method string, status int, _ time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, recordedRequest{route: route, method: method, status: status})
}

func (s *telemetryStub) HTTPPanicRecovered(beforeCommit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panics = append(s.panics, beforeCommit)
}

func (s *telemetryStub) CorrelationIDDropped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped++
}

func (s *telemetryStub) AuthenticationObserved(success bool, reason identity.AuthFailureReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authResults = append(s.authResults, recordedAuth{success: success, reason: reason})
}

func (s *telemetryStub) OperationAdmitted(capability domain.Capability, retry bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admissions = append(s.admissions, recordedAdmission{capability: capability, retry: retry})
}

func (s *telemetryStub) snapshot() (started, dropped int, finished []recordedRequest, admissions []recordedAdmission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.dropped, append([]recordedRequest(nil), s.finished...), append([]recordedAdmission(nil), s.admissions...)
}

// capturingLog collects slog records emitted by the transport.
type capturingLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturingLog) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record)
	return nil
}
func (c *capturingLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingLog) WithGroup(string) slog.Handler      { return c }

func (c *capturingLog) find(message string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, record := range c.records {
		if record.Message != message {
			continue
		}
		fields := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool {
			fields[attr.Key] = attr.Value.String()
			return true
		})
		return fields, true
	}
	return nil, false
}

// instrumentedFixture is the standard harness plus injected telemetry and log.
type instrumentedFixture struct {
	handler   http.Handler
	service   *application.Service
	store     *applicationfake.Store
	resolver  *applicationfake.Resolver
	ref       application.ProvisionerRef
	telemetry *telemetryStub
	log       *capturingLog
	pumper    *worker.Worker
}

func newInstrumentedFixture(t *testing.T) *instrumentedFixture {
	t.Helper()
	store := applicationfake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Fake resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}
	ref, refErr := application.NewProvisionerRef("transport-test-provider")
	if refErr != nil {
		t.Fatal(refErr)
	}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
		ref: provisioningfake.New(provisioningfake.ModeSynchronous),
	}}
	service, serviceErr := application.NewService(catalog, &applicationfake.Selector{Ref: ref}, resolver, store, applicationfake.AllowAll{})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	auth := newHeaderAuthenticator()
	stub := &telemetryStub{}
	logCapture := &capturingLog{}
	handler := apihttp.NewHandler(apihttp.Deps{
		Service:   service,
		Auth:      auth,
		Logger:    slog.New(logCapture),
		Telemetry: stub,
	})
	pumper, pumpErr := worker.New(store, resolver)
	if pumpErr != nil {
		t.Fatal(pumpErr)
	}
	return &instrumentedFixture{handler: handler, service: service, store: store, resolver: resolver, ref: ref,
		telemetry: stub, log: logCapture, pumper: pumper}
}

func (f *instrumentedFixture) do(t *testing.T, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, payload)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func TestCorrelationIDSanitizedBeforeEcho(t *testing.T) {
	stub := &telemetryStub{}
	handler := apihttp.NewHandler(apihttp.Deps{Telemetry: stub})

	valid := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Correlation-ID", "  deploy-42 ")
	handler.ServeHTTP(valid, request)
	if echoed := valid.Header().Get("X-Correlation-ID"); echoed != "deploy-42" {
		t.Fatalf("sanitized echo = %q, want trimmed value", echoed)
	}

	hostile := httptest.NewRecorder()
	hostileRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	hostileRequest.Header.Set("X-Correlation-ID", "evil\r\nX-Injected: 1")
	handler.ServeHTTP(hostile, hostileRequest)
	if echoed := hostile.Header().Get("X-Correlation-ID"); echoed != "" {
		t.Fatalf("hostile correlation ID was echoed: %q", echoed)
	}
	if _, dropped, _, _ := stub.snapshot(); dropped != 1 {
		t.Fatalf("dropped counter = %d, want 1", dropped)
	}
}

func TestRouteMetricLabelsUseTemplatesOnly(t *testing.T) {
	fixture := newInstrumentedFixture(t)
	fixture.do(t, http.MethodGet, "/v1/resources/orders-db", map[string]string{"Authorization": "Bearer tester"}, nil)
	fixture.do(t, http.MethodGet, "/v1/resources/orders-db/operations?limit=5", nil, nil)
	fixture.do(t, http.MethodGet, "/totally/unmatched/path", nil, nil)

	_, _, finished, _ := fixture.telemetry.snapshot()
	want := map[string]string{
		"GET /v1/resources/{id}":            "/v1/resources/{id}",
		"GET /v1/resources/{id}/operations": "/v1/resources/{id}/operations",
		"GET unmatched":                     "unmatched",
	}
	for key, expectedRoute := range want {
		parts := strings.SplitN(key, " ", 2)
		found := false
		for _, record := range finished {
			if record.method == parts[0] && record.route == expectedRoute {
				found = true
			}
		}
		if !found {
			t.Fatalf("no metric record for %q among %+v", key, finished)
		}
	}
	for _, record := range finished {
		if strings.Contains(record.route, "orders-db") {
			t.Fatalf("raw resource ID leaked into route label: %q", record.route)
		}
	}
}

func TestAdmissionCounterCountsNewOperationsAndReplaysStructurally(t *testing.T) {
	fixture := newInstrumentedFixture(t)
	headers := func(key string) map[string]string {
		return map[string]string{"Authorization": "Bearer tester", "Idempotency-Key": key,
			"Content-Type": "application/json"}
	}
	body := map[string]any{
		"id":    "admit-count-1",
		"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]any{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"size": int64(3)},
	}
	first := fixture.do(t, http.MethodPost, "/v1/resources", headers("admit-key"), body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	replay := fixture.do(t, http.MethodPost, "/v1/resources", headers("admit-key"), body)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d replayed=%q", replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	_, _, _, admissions := fixture.telemetry.snapshot()
	if len(admissions) != 1 {
		t.Fatalf("admissions=%d, want exactly 1 (replay must not count)", len(admissions))
	}
	if admissions[0].capability != domain.CapabilityCreate || admissions[0].retry {
		t.Fatalf("admission=%+v", admissions[0])
	}
}

func TestRetryAdmissionCountsNewChildOperationOnce(t *testing.T) {
	fixture := newInstrumentedFixture(t)
	createBody := map[string]any{
		"id":    "retry-count-1",
		"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]any{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"size": int64(1)},
	}
	if code := fixture.do(t, http.MethodPost, "/v1/resources", map[string]string{
		"Authorization": "Bearer tester", "Idempotency-Key": "retry-create",
		"Content-Type": "application/json"}, createBody).Code; code != http.StatusCreated {
		t.Fatalf("create status=%d", code)
	}
	fixture.drain(t)

	fixture.resolver.Providers[fixture.ref] = provisioningfake.New(provisioningfake.ModeFailure)
	failed := fixture.do(t, http.MethodPut, "/v1/resources/retry-count-1", map[string]string{
		"Authorization": "Bearer tester", "Idempotency-Key": "retry-fail",
		"If-Liftr-Generation": "1", "Content-Type": "application/json",
	}, map[string]any{"spec": map[string]any{"size": int64(9)}})
	if failed.Code != http.StatusAccepted {
		t.Fatalf("failed update status=%d", failed.Code)
	}
	fixture.drain(t)
	fixture.resolver.Providers[fixture.ref] = provisioningfake.New(provisioningfake.ModeSynchronous)

	resourceView := fixture.do(t, http.MethodGet, "/v1/resources/retry-count-1", map[string]string{"Authorization": "Bearer tester"}, nil)
	currentGeneration := resourceView.Header().Get("Liftr-Generation")
	if currentGeneration == "" {
		t.Fatalf("resource generation missing: %s", resourceView.Body.String())
	}
	operationPath := strings.TrimPrefix(strings.Split(failed.Header().Get("Link"), ";")[0], "<")
	operationPath = strings.TrimSuffix(operationPath, ">")
	operationID := strings.TrimPrefix(operationPath, "/v1/operations/")
	operationView := fixture.do(t, http.MethodGet, "/v1/operations/"+operationID, map[string]string{"Authorization": "Bearer tester"}, nil)
	var operationBody struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(operationView.Body.Bytes(), &operationBody)
	if operationBody.State != "Failed" {
		t.Fatalf("source operation state=%q body=%s", operationBody.State, operationView.Body.String())
	}
	retryHeaders := func(key string) map[string]string {
		return map[string]string{"Authorization": "Bearer tester", "Idempotency-Key": key,
			"If-Liftr-Generation": currentGeneration, "Content-Type": "application/json"}
	}
	first := fixture.do(t, http.MethodPost, "/v1/operations/"+operationID+"/retry", retryHeaders("retry-key"), nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first retry status=%d body=%s", first.Code, first.Body.String())
	}
	replay := fixture.do(t, http.MethodPost, "/v1/operations/"+operationID+"/retry", retryHeaders("retry-key"), nil)
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry replay status=%d", replay.Code)
	}
	_, _, _, admissions := fixture.telemetry.snapshot()
	retryAdmissions := 0
	for _, admission := range admissions {
		if admission.retry {
			retryAdmissions++
		}
	}
	if len(admissions) != 3 || retryAdmissions != 1 {
		t.Fatalf("admissions=%+v, want create + failed update + exactly one retry child", admissions)
	}
}

func (f *instrumentedFixture) drain(t *testing.T) {
	t.Helper()
	for range 64 {
		found, err := f.pumper.RunOnce(context.Background())
		if err != nil && !errors.Is(err, application.ErrConcurrencyConflict) {
			t.Fatalf("worker error=%v", err)
		}
		if !found {
			return
		}
	}
	t.Fatal("worker did not drain")
}

func mustU64(t *testing.T, value string) uint64 {
	t.Helper()
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestAccessLogsCarryRequestCorrelationAndMutationPrincipal(t *testing.T) {
	fixture := newInstrumentedFixture(t)
	fixture.do(t, http.MethodGet, "/v1/resource-types", map[string]string{"Authorization": "Bearer tester"}, nil)
	fixture.do(t, http.MethodPost, "/v1/resources", map[string]string{
		"Authorization": "Bearer tester", "Idempotency-Key": "log-key", "X-Correlation-ID": "corr-log",
		"Content-Type": "application/json",
	}, map[string]any{
		"id":    "access-log-1",
		"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]any{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"size": int64(1)},
	})
	postFields, ok := fixture.log.find("http_request")
	_ = postFields
	if !ok {
		t.Fatal("no access log record was emitted")
	}
	posts := 0
	fixture.log.mu.Lock()
	defer fixture.log.mu.Unlock()
	for _, record := range fixture.log.records {
		if record.Message != "http_request" {
			continue
		}
		fields := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool {
			fields[attr.Key] = attr.Value.String()
			return true
		})
		if fields["request_id"] == "" || fields["route"] == "" {
			t.Fatalf("access record missing correlation fields: %v", fields)
		}
		if fields["method"] == http.MethodPost {
			posts++
			if fields["principal_id"] == "" {
				t.Fatalf("mutation access record lacks principal_id: %v", fields)
			}
			if fields["correlation_id"] != "corr-log" {
				t.Fatalf("correlation_id = %q", fields["correlation_id"])
			}
		} else if fields["principal_id"] != "" {
			t.Fatalf("read access record carries principal_id: %v", fields)
		}
	}
	if posts == 0 {
		t.Fatal("no POST access record found")
	}
}
