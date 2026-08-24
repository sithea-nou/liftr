// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const retrySourceFixture = `{"id":"op-source","resourceId":"orders-db","capability":"update","state":"Failed","targetGeneration":4,"requestedAt":"2026-08-23T09:00:00Z","completedAt":"2026-08-23T09:01:00Z"}`

const retryChildFixture = `{"id":"op-child","resourceId":"orders-db","retryOf":"op-source","capability":"update","state":"%s","targetGeneration":7,"requestedAt":"2026-08-23T10:00:00Z"}`

func TestOperationListPaginationAndOutput(t *testing.T) {
	page := `{"items":[{"id":"op-child","resourceId":"orders-db","retryOf":"op-source","capability":"update","state":"Succeeded","targetGeneration":7,"requestedAt":"2026-08-23T10:00:00Z","completedAt":"2026-08-23T10:01:00Z"}],"nextCursor":"c1_next+/="}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/resources/orders-db/operations" || r.URL.RawQuery != "cursor=c1_old%2B%2F%3D&limit=13" {
			t.Errorf("request URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		jsonHeaders(w)
		fmt.Fprint(w, page)
	}))
	defer server.Close()
	env := map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken}

	result := runCLI(t, nil, env, "-o", "json", "operation", "list", "--resource", "orders-db", "--limit", "13", "--cursor", "c1_old+/=")
	if result.code != ExitOK || result.stdout != page+"\n" {
		t.Fatalf("JSON list = code %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "--cursor c1_next+/=") {
		t.Fatalf("continuation guidance missing: %q", result.stderr)
	}
	if !json.Valid([]byte(result.stdout)) {
		t.Fatalf("stdout is not one JSON document: %q", result.stdout)
	}

	text := runCLI(t, nil, env, "operation", "list", "--resource", "orders-db", "--limit", "13", "--cursor", "c1_old+/=")
	for _, want := range []string{"ID", "TARGET GENERATION", "RETRY OF", "op-child", "op-source", "2026-08-23T10:01:00Z"} {
		if !strings.Contains(text.stdout, want) {
			t.Fatalf("text list missing %q:\n%s", want, text.stdout)
		}
	}
}

func TestOperationListRejectsInvalidLimitLocally(t *testing.T) {
	requests := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	for _, limit := range []string{"0", "101"} {
		result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL}, "operation", "list", "--resource", "orders-db", "--limit", limit)
		if result.code != ExitUsage {
			t.Fatalf("limit %s exit = %d", limit, result.code)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid limits sent %d requests", requests.Load())
	}
}

func TestOperationRetryGenerationPreReadAndExplicitBypass(t *testing.T) {
	for _, tc := range []struct {
		name             string
		args             []string
		wantSourceReads  int32
		wantResourceRead int32
	}{
		{name: "pre-reads source and owning Resource", wantSourceReads: 1, wantResourceRead: 1},
		{name: "explicit generation bypasses both reads", args: []string{"--generation", "9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sourceReads, resourceReads, retries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op-source":
					sourceReads.Add(1)
					jsonHeaders(w)
					fmt.Fprint(w, retrySourceFixture)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/resources/orders-db":
					resourceReads.Add(1)
					jsonHeaders(w)
					fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 9, `{}`, "Ready", 9))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/operations/op-source/retry":
					retries.Add(1)
					if r.Header.Get("If-Liftr-Generation") != "9" {
						t.Errorf("generation header = %q", r.Header.Get("If-Liftr-Generation"))
					}
					if key := r.Header.Get("Idempotency-Key"); !strings.HasPrefix(key, "cli-") {
						t.Errorf("generated key = %q", key)
					}
					raw, _ := io.ReadAll(r.Body)
					if len(raw) != 0 {
						t.Errorf("retry body = %q", raw)
					}
					jsonHeaders(w)
					w.Header().Set("Link", `</v1/operations/op-child>; rel="monitor"`)
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprintf(w, retryChildFixture, "Pending")
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			args := []string{"-o", "json", "operation", "retry", "op-source"}
			args = append(args, tc.args...)
			result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken}, args...)
			if result.code != ExitOK || !strings.Contains(result.stdout, `"id":"op-child"`) || !strings.Contains(result.stdout, `"retryOf":"op-source"`) {
				t.Fatalf("result = code %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
			}
			if sourceReads.Load() != tc.wantSourceReads || resourceReads.Load() != tc.wantResourceRead || retries.Load() != 1 {
				t.Fatalf("requests: source=%d resource=%d retry=%d", sourceReads.Load(), resourceReads.Load(), retries.Load())
			}
		})
	}
}

func TestOperationRetryRejectsExplicitZeroGenerationLocally(t *testing.T) {
	requests := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
		"operation", "retry", "op-source", "--generation", "0")
	if result.code != ExitUsage || requests.Load() != 0 || !strings.Contains(result.stderr, "--generation must be greater than zero") {
		t.Fatalf("result code=%d requests=%d stderr=%q", result.code, requests.Load(), result.stderr)
	}
}

func TestOperationRetryRejectsMonitorOperationMismatchWithoutPollingOrGuidance(t *testing.T) {
	var posts, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-other>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, retryChildFixture, "Pending")
			return
		}
		polls.Add(1)
	}))
	defer server.Close()
	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
		"operation", "retry", "op-source", "--generation", "7", "--wait")
	if result.code != ExitFailure || posts.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("result code=%d posts=%d polls=%d stderr=%q", result.code, posts.Load(), polls.Load(), result.stderr)
	}
	if !strings.Contains(result.stderr, "protocol failure") || strings.Contains(result.stderr, "monitor with:") || strings.Contains(result.stderr, "waiting for operation") {
		t.Fatalf("unexpected mismatch guidance: %q", result.stderr)
	}
}

func TestOperationRetryWithoutWaitEmitsChildDespiteUnusableMonitorMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string
	}{
		{name: "missing"},
		{name: "malformed", link: `<not-an-operation>; rel="monitor"`},
		{name: "mismatched", link: `</v1/operations/op-other>; rel="monitor"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				jsonHeaders(w)
				if tc.link != "" {
					w.Header().Set("Link", tc.link)
				}
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprintf(w, retryChildFixture, "Pending")
			}))
			defer server.Close()

			result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
				"-o", "json", "operation", "retry", "op-source", "--generation", "7")
			if result.code != ExitOK || !strings.Contains(result.stdout, `"id":"op-child"`) {
				t.Fatalf("result code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
			}
			if !strings.Contains(result.stderr, "warning: retry admission carries unusable monitor metadata") {
				t.Fatalf("missing warning: %q", result.stderr)
			}
		})
	}
}

func TestOperationRetryWaitOutputsOnlyFinalChildOperation(t *testing.T) {
	var childPolls, resourceReads, sourcePolls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operations/op-source/retry":
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-child>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, retryChildFixture, "Pending")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op-child":
			childPolls.Add(1)
			jsonHeaders(w)
			fmt.Fprintf(w, retryChildFixture, "Succeeded")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op-source":
			sourcePolls.Add(1)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/resources/"):
			resourceReads.Add(1)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "operation", "retry", "op-source", "--generation", "7", "--wait")
	if result.code != ExitOK || childPolls.Load() != 1 || sourcePolls.Load() != 0 || resourceReads.Load() != 0 {
		t.Fatalf("result code=%d child=%d source=%d resource=%d stderr=%q", result.code, childPolls.Load(), sourcePolls.Load(), resourceReads.Load(), result.stderr)
	}
	if strings.Count(strings.TrimSpace(result.stdout), "\n") != 0 || !strings.Contains(result.stdout, `"state":"Succeeded"`) || strings.Contains(result.stdout, `"type":`) {
		t.Fatalf("stdout is not exactly final child Operation: %q", result.stdout)
	}
}

func TestOperationRetryWaitFailureEmitsOperationAndExitsFive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonHeaders(w)
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v1/operations/op-child")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, retryChildFixture, "Pending")
			return
		}
		fmt.Fprint(w, `{"id":"op-child","resourceId":"orders-db","retryOf":"op-source","capability":"update","state":"Failed","targetGeneration":7,"requestedAt":"2026-08-23T10:00:00Z","completedAt":"2026-08-23T10:01:00Z","failure":{"reason":"ProvisionFailed","message":"backend rejected retry"}}`)
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
		"-o", "json", "operation", "retry", "op-source", "--generation", "7", "--wait")
	if result.code != ExitOperationFailed || !strings.Contains(result.stdout, `"state":"Failed"`) || !strings.Contains(result.stderr, "ProvisionFailed") {
		t.Fatalf("result = code %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
	}
}

func TestOperationRetryGenerationConflictIsNotRetried(t *testing.T) {
	attempts := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"type":"https://liftr.dev/problems/generation-conflict","title":"Generation conflict","status":409,"code":"GENERATION_CONFLICT","requestId":"req-conflict","currentGeneration":8}`)
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
		"operation", "retry", "op-source", "--generation", "7", "--idempotency-key", "fixed-key")
	if result.code != ExitRejected || attempts.Load() != 1 || !strings.Contains(result.stderr, "Current generation: 8") {
		t.Fatalf("result code=%d attempts=%d stderr=%q", result.code, attempts.Load(), result.stderr)
	}
}

func TestOperationDetailShowsRetryOf(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonHeaders(w)
		fmt.Fprintf(w, retryChildFixture, "Succeeded")
	}))
	defer server.Close()
	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL}, "operation", "get", "op-child")
	if result.code != ExitOK || !strings.Contains(result.stdout, "Retry of:") || !strings.Contains(result.stdout, "op-source") {
		t.Fatalf("detail output = %q stderr=%q", result.stdout, result.stderr)
	}
}
