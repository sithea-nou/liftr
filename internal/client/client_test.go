// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sithea-nou/liftr/internal/client"
)

const testToken = "test-bearer-credential-do-not-leak"

func newTestClient(t *testing.T, origin string) *client.Client {
	t.Helper()
	parsed, err := client.ParseOrigin(origin)
	if err != nil {
		t.Fatalf("ParseOrigin(%q): %v", origin, err)
	}
	c, err := client.New(client.Options{
		Origin:        parsed,
		Token:         testToken,
		UserAgent:     "liftr/test-version",
		CorrelationID: "correlation-fixed",
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func resourceBody(id string, specValue string) string {
	return fmt.Sprintf(`{"id":%q,"type":{"name":"PostgreSQLDatabase","version":"v2"},"owner":{"kind":"team","id":"payments"},`+
		`"generation":4,"spec":{"storageGB":%s},"status":{"state":"Ready","observedGeneration":4,"updatedAt":"2026-08-23T10:00:00Z"},`+
		`"latestOperation":{"id":"op-1","capability":"update","state":"Succeeded","targetGeneration":4,"href":"/v1/operations/op-1"},`+
		`"outputs":{"observedGeneration":3,"values":{"port":5432.0}},`+
		`"createdAt":"2026-08-20T10:00:00Z","updatedAt":"2026-08-23T10:00:00Z"}`, id, specValue)
}

func TestClientSendsBearerTokenAndHeaders(t *testing.T) {
	var gotAuthorization, gotUA, gotCorrelation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotCorrelation = r.Header.Get("X-Correlation-ID")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resourceBody("orders-db", "20"))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	if _, err := c.GetResource(context.Background(), "orders-db"); err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if gotAuthorization != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", gotAuthorization)
	}
	if gotUA != "liftr/test-version" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if gotCorrelation != "correlation-fixed" {
		t.Fatalf("X-Correlation-ID = %q", gotCorrelation)
	}
}

func TestClientWithoutTokenSendsNoAuthorizationHeader(t *testing.T) {
	var sawAuthorization bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()

	parsed, err := client.ParseOrigin(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Options{Origin: parsed, UserAgent: "liftr/test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListResourceTypes(context.Background()); err != nil {
		t.Fatalf("ListResourceTypes: %v", err)
	}
	if sawAuthorization {
		t.Fatal("Authorization header sent without a configured token")
	}
}

func TestClientRefusesRedirectsAndNeverForwardsCredentials(t *testing.T) {
	var attackerHits atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
	}))
	defer attacker.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.GetResource(context.Background(), "orders-db")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected redirect refusal, got %v", err)
	}
	if hits := attackerHits.Load(); hits != 0 {
		t.Fatalf("attacker received %d requests", hits)
	}
}

// TestMonitorReferencesNeverLeaveTheOrigin pins Correction 2: Link and
// Location are untrusted server input; cross-origin references are refused
// and the attacker receives zero requests.
func TestMonitorReferencesNeverLeaveTheOrigin(t *testing.T) {
	var attackerHits atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
	}))
	defer attacker.Close()

	mutate := func(linkHeader, locationHeader string) *client.MutationResult {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if linkHeader != "" {
				w.Header().Set("Link", linkHeader)
			}
			if locationHeader != "" {
				w.Header().Set("Location", locationHeader)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, resourceBody("orders-db", "20"))
		}))
		defer server.Close()

		c := newTestClient(t, server.URL)
		result, err := c.UpdateResource(context.Background(), "orders-db",
			[]byte(`{"spec":{}}`), "key-1", 4)
		if err != nil {
			t.Fatalf("UpdateResource: %v", err)
		}
		return result
	}

	t.Run("cross-origin monitor link refused", func(t *testing.T) {
		result := mutate(fmt.Sprintf(`<%s/v1/operations/op-1>; rel="monitor"`, attacker.URL), "")
		c := newTestClient(t, "https://liftr.example.com")
		if _, err := c.MonitorOperationID(result); err == nil {
			t.Fatal("cross-origin monitor link accepted")
		}
	})

	t.Run("cross-origin location refused when no link exists", func(t *testing.T) {
		result := mutate("", fmt.Sprintf("%s/v1/operations/op-1", attacker.URL))
		c := newTestClient(t, "https://liftr.example.com")
		if _, err := c.MonitorOperationID(result); err == nil {
			t.Fatal("cross-origin Location accepted")
		}
	})

	t.Run("malformed link never falls back to location", func(t *testing.T) {
		result := mutate(fmt.Sprintf("<%s/x>; rel=\"monitor\"", attacker.URL), "/v1/operations/op-good")
		c := newTestClient(t, "https://liftr.example.com")
		if _, err := c.MonitorOperationID(result); err == nil {
			t.Fatal("malformed monitor entry fell back to Location")
		}
	})

	t.Run("same-origin absolute and relative links accepted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", `</v1/operations/op-rel>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, resourceBody("orders-db", "20"))
		}))
		defer server.Close()
		c := newTestClient(t, server.URL)
		result, err := c.UpdateResource(context.Background(), "orders-db", []byte(`{"spec":{}}`), "k", 1)
		if err != nil {
			t.Fatalf("UpdateResource: %v", err)
		}
		got, err := c.MonitorOperationID(result)
		if err != nil || got != "op-rel" {
			t.Fatalf("relative monitor = %q, err %v", got, err)
		}

		absolute := server.URL + "/v1/operations/op-abs"
		result.MonitorRef = absolute
		got, err = c.MonitorOperationID(result)
		if err != nil || got != "op-abs" {
			t.Fatalf("absolute same-origin monitor = %q, err %v", got, err)
		}
	})

	t.Run("no latestOperation fallback", func(t *testing.T) {
		result := mutate("", "/v1/resources/orders-db")
		c := newTestClient(t, "https://liftr.example.com")
		if _, err := c.MonitorOperationID(result); err == nil {
			t.Fatal("Location pointing at the Resource was used as an Operation monitor")
		}
		result.HasMonitorEntry = false
		result.LocationRef = ""
		if _, err := c.MonitorOperationID(result); err == nil {
			t.Fatal("missing monitor metadata did not fail")
		}
	})

	t.Run("default port equivalence", func(t *testing.T) {
		origin, err := client.ParseOrigin("https://liftr.example.com")
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(client.Options{Origin: origin})
		if err != nil {
			t.Fatal(err)
		}
		result := &client.MutationResult{
			MonitorRef:      "https://liftr.example.com:443/v1/operations/op-1",
			HasMonitorEntry: true,
		}
		if got, err := c.MonitorOperationID(result); err != nil || got != "op-1" {
			t.Fatalf("explicit default port refused: %q %v", got, err)
		}
		bad := &client.MutationResult{
			MonitorRef:      "https://liftr.example.com:9443/v1/operations/op-1",
			HasMonitorEntry: true,
		}
		if _, err := c.MonitorOperationID(bad); err == nil {
			t.Fatal("different effective port accepted as same origin")
		}
	})
}

// TestRetriesReuseIdenticalBytes pins that internal retries never change the
// body bytes or mint a replacement idempotency key.
func TestRetriesReuseIdenticalBytes(t *testing.T) {
	var requests atomic.Int32
	type seen struct {
		key    string
		body   string
		gen    string
		method string
	}
	observed := make(chan seen, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil && len(body) == 0 {
			t.Errorf("reading request body: %v", err)
		}
		observed <- seen{
			key:    r.Header.Get("Idempotency-Key"),
			body:   string(body),
			gen:    r.Header.Get("If-Liftr-Generation"),
			method: r.Method,
		}
		n := requests.Add(1)
		if n < 3 {
			// First two attempts die at transport level before any response.
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, resourceBody("orders-db", "20"))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	result, err := c.UpdateResource(context.Background(), "orders-db", []byte(`{"spec":{"storageGB":20}}`), "cli-key", 4)
	if err != nil {
		t.Fatalf("UpdateResource after retries: %v", err)
	}
	if result.Resource == nil {
		t.Fatal("mutation returned no Resource snapshot")
	}
	close(observed)
	count := 0
	for each := range observed {
		count++
		if each.key != "cli-key" || each.body != `{"spec":{"storageGB":20}}` || each.gen != "4" || each.method != http.MethodPut {
			t.Fatalf("retry diverged: %+v", each)
		}
	}
	if count != 3 {
		t.Fatalf("expected exactly 3 attempts, saw %d", count)
	}
}

// TestMutationsDoNotRetrySemanticErrors proves a 409 problem is surfaced
// with its extensions and never retried.
func TestMutationsDoNotRetrySemanticErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"type":"https://liftr.dev/problems/generation-conflict","title":"Generation conflict",`+
			`"status":409,"code":"GENERATION_CONFLICT","requestId":"req-1","currentGeneration":7}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.DeleteResource(context.Background(), "orders-db", "cli-key", 4)
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	if !apiErr.HasCode(client.CodeGenerationConflict) {
		t.Fatalf("code = %q", apiErr.Problem.Code)
	}
	if apiErr.Problem.CurrentGeneration == nil || *apiErr.Problem.CurrentGeneration != 7 {
		t.Fatalf("currentGeneration = %v", apiErr.Problem.CurrentGeneration)
	}
	if attempts.Load() != 1 {
		t.Fatalf("semantic 409 retried %d times", attempts.Load())
	}
}

func TestReadsRetryTransientFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resourceBody("orders-db", "20"))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resource, err := c.GetResource(context.Background(), "orders-db")
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if resource.Generation != 4 {
		t.Fatalf("generation = %d", resource.Generation)
	}
}

// TestJSONOutputPreservesNumericLiterals pins Correction 4: the client keeps
// the exact server bytes, so 20 and 20.0 remain distinct in -o json output.
func TestJSONOutputPreservesNumericLiterals(t *testing.T) {
	for _, tc := range []struct {
		specLiteral string
	}{
		{"20"},
		{"20.0"},
	} {
		t.Run(tc.specLiteral, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, resourceBody("orders-db", tc.specLiteral))
			}))
			defer server.Close()

			c := newTestClient(t, server.URL)
			resource, err := c.GetResource(context.Background(), "orders-db")
			if err != nil {
				t.Fatalf("GetResource: %v", err)
			}
			if !strings.Contains(string(resource.Raw), `"storageGB":`+tc.specLiteral) {
				t.Fatalf("raw representation lost literal %s: %s", tc.specLiteral, resource.Raw)
			}
			var probe struct {
				Spec json.RawMessage `json:"spec"`
			}
			if err := json.Unmarshal(resource.Raw, &probe); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(probe.Spec), tc.specLiteral) {
				t.Fatalf("typed Spec lost literal %s: %s", tc.specLiteral, probe.Spec)
			}
			if !strings.Contains(string(resource.Raw), `"port":5432.0`) {
				t.Fatalf("outputs number normalized in raw bytes: %s", resource.Raw)
			}
		})
	}
}

func TestProblemDecoding(t *testing.T) {
	body := `{"type":"https://liftr.dev/problems/resource-spec-invalid","title":"Invalid resource spec",` +
		`"status":422,"detail":"the submitted spec does not satisfy the PostgreSQLDatabase/v2 contract",` +
		`"instance":"/v1/resources","code":"RESOURCE_SPEC_INVALID","requestId":"req-abc",` +
		`"violations":[{"path":"/engineVersion","keyword":"const","message":"value is immutable"}],` +
		`"truncated":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.GetResource(context.Background(), "orders-db")
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	if apiErr.Problem.Code != "RESOURCE_SPEC_INVALID" || len(apiErr.Problem.Violations) != 1 ||
		!apiErr.Problem.Truncated || apiErr.Problem.RequestID != "req-abc" {
		t.Fatalf("problem decoded incorrectly: %+v", apiErr.Problem)
	}
}

func TestNonProblemErrorBodiesStayOpaque(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Request-ID", "req-header-id")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "<html>boom</html>")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.GetResource(context.Background(), "orders-db")
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.Problem.Title != "request failed" || apiErr.RequestID != "req-header-id" {
		t.Fatalf("opaque fallback wrong: %+v / header id %q", apiErr.Problem, apiErr.RequestID)
	}
}

func TestResponseSizeLimitIsEnforced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(make([]byte, 5<<20)) // exceeds maxResponseBytes
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.GetResource(context.Background(), "orders-db")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestListResourceOperationsEncodesQueryAndPreservesRawPage(t *testing.T) {
	page := "  {\n" +
		`"items":[{"id":"op-2","resourceId":"orders/db","retryOf":"op-1","capability":"update","state":"Succeeded","targetGeneration":7,"requestedAt":"2026-08-23T09:00:00Z"}],` +
		`"nextCursor":"c1_a+b/c=="}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/resources/orders%2Fdb/operations" {
			t.Errorf("escaped path = %q", r.URL.EscapedPath())
		}
		if r.URL.RawQuery != "cursor=before%2B%2F%3D&limit=37" {
			t.Errorf("raw query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, page)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	list, err := c.ListResourceOperations(context.Background(), "orders/db", 37, "before+/=")
	if err != nil {
		t.Fatalf("ListResourceOperations: %v", err)
	}
	if string(list.Raw) != page {
		t.Fatalf("raw page changed:\n got %q\nwant %q", list.Raw, page)
	}
	if len(list.Items) != 1 || list.Items[0].RetryOf != "op-1" || list.Items[0].TargetGeneration != 7 {
		t.Fatalf("typed items = %+v", list.Items)
	}
	if list.NextCursor != "c1_a+b/c==" {
		t.Fatalf("next cursor = %q", list.NextCursor)
	}
	if !strings.Contains(string(list.Items[0].Raw), `"retryOf":"op-1"`) {
		t.Fatalf("raw item missing retryOf: %s", list.Items[0].Raw)
	}
}

func TestRetryOperationHasNoBodyAndRetainsAdmissionMetadata(t *testing.T) {
	operationBody := `{"id":"op-child","resourceId":"orders-db","retryOf":"op-source","capability":"update",` +
		`"state":"Pending","targetGeneration":7,"requestedAt":"2026-08-23T09:00:00Z"}`
	var attempts atomic.Int32
	type requestShape struct {
		key, generation, contentType, body string
		contentLength                      int64
	}
	seen := make(chan requestShape, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		seen <- requestShape{
			key:           r.Header.Get("Idempotency-Key"),
			generation:    r.Header.Get("If-Liftr-Generation"),
			contentType:   r.Header.Get("Content-Type"),
			body:          string(raw),
			contentLength: r.ContentLength,
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/operations/source%2Fid/retry" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `</v1/operations/op-child>; rel="monitor"`)
		w.Header().Set("Location", "/v1/operations/op-location")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, operationBody)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	result, err := c.RetryOperation(context.Background(), "source/id", "retry-key", 7)
	if err != nil {
		t.Fatalf("RetryOperation: %v", err)
	}
	close(seen)
	for request := range seen {
		if request.key != "retry-key" || request.generation != "7" || request.body != "" || request.contentLength != 0 || request.contentType != "" {
			t.Fatalf("retry request changed: %+v", request)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	if result.Status != http.StatusAccepted || !result.Replay || result.Resource != nil || result.Operation == nil {
		t.Fatalf("result metadata = %+v", result)
	}
	if string(result.Operation.Raw) != operationBody || result.Operation.RetryOf != "op-source" {
		t.Fatalf("operation = %+v raw=%q", result.Operation, result.Operation.Raw)
	}
	if result.MonitorRef != "/v1/operations/op-child" || !result.HasMonitorEntry || result.LocationRef != "/v1/operations/op-location" {
		t.Fatalf("monitor metadata = %+v", result)
	}
	monitorID, err := c.MonitorOperationID(result)
	if err != nil || monitorID != "op-child" {
		t.Fatalf("authoritative monitor = %q, %v", monitorID, err)
	}
}
