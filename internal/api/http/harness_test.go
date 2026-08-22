// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

const (
	testResourceType    = "FakeResource"
	testResourceVersion = "v1"
)

type fixture struct {
	handler  http.Handler
	service  *application.Service
	store    *applicationfake.Store
	resolver *applicationfake.Resolver
	ref      application.ProvisionerRef
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := applicationfake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Fake resource for transport tests",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}
	ref, err := application.NewProvisionerRef("transport-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	selector := &applicationfake.Selector{Ref: ref}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)}}
	service, err := application.NewService(catalog, selector, resolver, store)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		handler:  apihttp.NewHandler(apihttp.Deps{Service: service}),
		service:  service,
		store:    store,
		resolver: resolver,
		ref:      ref,
	}
}

// request performs one API call against the handler.
func (f *fixture) request(t *testing.T, method, path string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func decodeBody(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("response body is not a JSON object: %v (%s)", err, payload)
	}
	return document
}

func rawBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func header(response *http.Response, name string) string { return response.Header.Get(name) }

func expectStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d (body=%s)", response.StatusCode, want, rawBody(t, response))
	}
}

func expectProblem(t *testing.T, response *http.Response, status int, code string) map[string]any {
	t.Helper()
	expectStatus(t, response, status)
	if got := header(response, "Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	document := decodeBody(t, response)
	if document["code"] != code {
		t.Fatalf("problem code = %v, want %v", document["code"], code)
	}
	for _, field := range []string{"type", "title", "status", "code", "requestId"} {
		if _, ok := document[field]; !ok {
			t.Fatalf("problem missing required field %q: %v", field, document)
		}
	}
	if status, ok := document["status"].(float64); !ok || int(status) != response.StatusCode {
		t.Fatalf("problem.status = %v does not match HTTP %d", document["status"], response.StatusCode)
	}
	return document
}

// createResource drives the happy-path create and returns the recorded body.
func (f *fixture) createResource(t *testing.T, id string, spec map[string]any) *http.Response {
	t.Helper()
	response := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "create-key-" + id}, map[string]any{
		"id":   id,
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": spec,
	})
	expectStatus(t, response, http.StatusCreated)
	return response
}

// drainWorker executes all durable outbox work so admitted operations reach
// their terminal state, mirroring production's asynchronous completion.
func (f *fixture) drainWorker(t *testing.T) {
	t.Helper()
	instance, err := worker.New(f.store, f.resolver)
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = 0
	for range 64 {
		worked, err := instance.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("worker did not drain")
}

// seedDeletedRecord installs a retained Deleted tombstone directly in the
// store so tombstone semantics are observable without driving the worker.
func (f *fixture) seedDeletedRecord(t *testing.T, id string) {
	t.Helper()
	requestedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource(domain.ResourceID(id), provisioningfake.ResourceType(),
		domain.OwnerRef{Kind: "team", ID: "platform"}, mustSpec(t, map[string]any{"size": int64(5)}), requestedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.UpdateSpec(mustSpec(t, map[string]any{"size": int64(6)}), requestedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 2, domain.ResourceStateDeleted,
		[]domain.Condition{mustCondition(t, "Deleted", domain.ConditionStatusTrue, "DeleteSucceeded", 2, requestedAt.Add(2*time.Minute))},
		requestedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	operation, err := domain.NewOperation("op-tombstone", resource.ID(), domain.CapabilityDelete, 2, requestedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := operation.Start(requestedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseDestroying, requestedAt.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := operation.Succeed(requestedAt.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		if err := tx.Resources().CreateResource(context.Background(), application.ResourceRecord{
			Resource: resource, Status: status,
			ProvisionerRef: mustProvisionerRef(t), Version: 3,
		}); err != nil {
			return err
		}
		return tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: operation, Version: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustSpec(t *testing.T, values map[string]any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func mustCondition(t *testing.T, typeName string, status domain.ConditionStatus, reason string, generation uint64, at time.Time) domain.Condition {
	t.Helper()
	condition, err := domain.NewCondition(typeName, status, reason, "", generation, at)
	if err != nil {
		t.Fatal(err)
	}
	return condition
}

func mustProvisionerRef(t *testing.T) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef("transport-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
