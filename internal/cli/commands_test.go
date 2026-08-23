// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const secretTestToken = "liftr-secret-credential-9f2b7"

func init() {
	pollInterval = 20 * time.Millisecond
}

type cliResult struct {
	stdout string
	stderr string
	code   int
}

func (r cliResult) combined() string { return r.stdout + "\n" + r.stderr }

func runCLI(t *testing.T, stdin io.Reader, env map[string]string, args ...string) cliResult {
	t.Helper()
	return runCLIWithTTY(t, false, stdin, env, args...)
}

// runCLIWithTTY simulates interactive and non-interactive terminals.
func runCLIWithTTY(t *testing.T, tty bool, stdin io.Reader, env map[string]string, args ...string) cliResult {
	t.Helper()
	previousOverride := stdinIsTTYOverride
	stdinIsTTYOverride = func() bool { return tty }
	defer func() { stdinIsTTYOverride = previousOverride }()

	for key, value := range env {
		t.Setenv(key, value)
	}
	var stdout, stderr bytes.Buffer
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := Execute(context.Background(), args, &stdout, &stderr, stdin, "test-version")
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

const resourceFixtureTemplate = `{"id":"orders-db","type":{"name":"PostgreSQLDatabase","version":"v2"},` +
	`"owner":{"kind":"team","id":"payments"},"generation":%d,"spec":%s,` +
	`"status":{"state":%q,"observedGeneration":%d,"updatedAt":"2026-08-23T10:00:00Z"},` +
	`"latestOperation":{"id":"op-latest","capability":"update","state":"Succeeded","targetGeneration":4,"href":"/v1/operations/op-latest"},` +
	`"outputs":{"observedGeneration":3,"values":{"port":5432.0,"hostname":"pg.example"}},` +
	`"createdAt":"2026-08-20T10:00:00Z","updatedAt":"2026-08-23T10:00:00Z"}`

const operationFixture = `{"id":"op-monitor","resourceId":"orders-db","capability":"create",` +
	`"state":%q,"targetGeneration":1,"requestedAt":"2026-08-23T09:00:00Z",` +
	`"startedAt":"2026-08-23T09:00:01Z","completedAt":"2026-08-23T09:01:00Z"}`

func jsonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", "req-server-id")
}

func TestVersionCommand(t *testing.T) {
	result := runCLI(t, nil, nil, "version")
	if result.code != ExitOK || !strings.Contains(result.stdout, "liftr version test-version") {
		t.Fatalf("version output wrong: %d %q %q", result.code, result.stdout, result.stderr)
	}
}

func TestResourceTypeListAndJSONPurity(t *testing.T) {
	listBody := `{"items":[{"name":"PostgreSQLDatabase","version":"v2","displayName":"PostgreSQL Database",` +
		`"description":"A managed PostgreSQL database.","capabilities":["create","delete","observe","update"],` +
		`"href":"/v1/resource-types/PostgreSQLDatabase/v2"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/resource-types" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		jsonHeaders(w)
		fmt.Fprint(w, listBody)
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource-type", "list")
	if result.code != ExitOK {
		t.Fatalf("exit %d stderr %q", result.code, result.stderr)
	}
	for _, want := range []string{"PostgreSQLDatabase", "v2", "PostgreSQL Database", "create"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("text-mode list missing %q:\n%s", want, result.stdout)
		}
	}

	jsonResult := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "resource-type", "list")
	if jsonResult.code != ExitOK || jsonResult.stderr != "" {
		t.Fatalf("clean json run must succeed silently: exit %d stderr %q", jsonResult.code, jsonResult.stderr)
	}
	lines := strings.Split(strings.TrimRight(jsonResult.stdout, "\n"), "\n")
	if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
		t.Fatalf("json mode must emit exactly one valid JSON document, got %d lines", len(lines))
	}
	if !strings.Contains(jsonResult.stdout, `"displayName":"PostgreSQL Database"`) {
		t.Fatalf("json mode lost representation fidelity: %s", jsonResult.stdout)
	}
}

func TestResourceTypeGetPreservesSchemaVerbatim(t *testing.T) {
	detail := `{"name":"PostgreSQLDatabase","version":"v2","displayName":"PostgreSQL Database",` +
		`"description":"","capabilities":["create"],"href":"/v1/resource-types/PostgreSQLDatabase/v2",` +
		`"specSchema":{"$id":"urn:liftr:resource-type:PostgreSQLDatabase:v2:spec","type":"object","properties":{"storageGB":{"type":"integer"}}},` +
		`"outputContract":{"fields":[{"name":"hostname","jsonType":"string","requiredWhenReady":true}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/resource-types/PostgreSQLDatabase/v2" {
			http.NotFound(w, r)
			return
		}
		jsonHeaders(w)
		fmt.Fprint(w, detail)
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "resource-type", "get", "PostgreSQLDatabase", "v2")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if !strings.Contains(result.stdout, `"storageGB":{"type":"integer"}`) ||
		!strings.Contains(result.stdout, `"jsonType":"string"`) {
		t.Fatalf("spec schema or output contract normalized away: %s", result.stdout)
	}
}

func TestCreateDocumentModeFromStdinPreservesBytesAndHeaders(t *testing.T) {
	document := `{"id":"orders-db","type":{"name":"PostgreSQLDatabase","version":"v2"},` +
		`"owner":{"kind":"team","id":"payments"},"spec":{"storageGB":20.0,"labels":{"a":1}}}`
	var gotBody string
	var gotAuth, gotKey, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("Idempotency-Key")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `</v1/operations/op-create>; rel="monitor"`)
		w.Header().Set("Location", "/v1/resources/orders-db")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 1, `{"storageGB":20}`, "Pending", 0))
	}))
	defer server.Close()

	result := runCLI(t, strings.NewReader(document),
		map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "resource", "create", "-f", "-")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if gotBody != document {
		t.Fatalf("request bytes were re-encoded:\n got %s\nwant %s", gotBody, document)
	}
	if gotAuth != "Bearer "+secretTestToken {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotKey, "cli-") || len(gotKey) < 12 {
		t.Fatalf("generated idempotency key looks wrong: %q", gotKey)
	}
	if gotUA != "liftr/test-version" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if !strings.Contains(result.stdout, `"storageGB":20`) || strings.Contains(result.stdout, `"storageGB":20.0`) {
		t.Fatalf("JSON output did not preserve the served literal: %s", result.stdout)
	}
}

func TestCreateAssemblyModeBuildsEnvelope(t *testing.T) {
	var envelope map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Errorf("bad envelope: %v", err)
		}
		jsonHeaders(w)
		w.Header().Set("Link", `</v1/operations/op-1>; rel="monitor"`)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 1, `{}`, "Pending", 0))
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{"storageGB":20}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "create",
		"--id", "orders-db", "--type", "PostgreSQLDatabase", "--version", "v2",
		"--owner", "team=payments", "--spec", specFile)
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if envelope["id"] != "orders-db" {
		t.Fatalf("envelope id wrong: %v", envelope["id"])
	}
	owner, _ := envelope["owner"].(map[string]any)
	if owner == nil || owner["kind"] != "team" || owner["id"] != "payments" {
		t.Fatalf("owner wrong: %v", envelope["owner"])
	}
	spec, _ := envelope["spec"].(map[string]any)
	if spec == nil || spec["storageGB"] == nil {
		t.Fatalf("spec missing: %v", envelope)
	}
}

func TestCreateInputValidation(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"mixed modes", []string{"resource", "create", "-f", "-", "--id", "x"}, `{}`},
		{"empty stdin", []string{"resource", "create", "-f", "-"}, ""},
		{"yaml input", []string{"resource", "create", "-f", "-"}, "id: orders-db\ntype:\n  name: PostgreSQLDatabase\n"},
		{"non-object", []string{"resource", "create", "-f", "-"}, `[1,2,3]`},
		{"two documents", []string{"resource", "create", "-f", "-"}, "{} {}"},
		{"assembly missing spec", []string{"resource", "create", "--id", "x", "--type", "T", "--version", "v1", "--owner", "k=i"}, ""},
		{"bad owner", []string{"resource", "create", "--id", "x", "--type", "T", "--version", "v1", "--owner", "team-payments", "--spec", "-"}, `{}`},
		{"bad resource id", []string{"resource", "create", "--id", "has space", "--type", "T", "--version", "v1", "--owner", "k=i", "--spec", "-"}, `{}`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be sent for invalid input, saw %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	env := map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLI(t, strings.NewReader(tc.stdin), env, tc.args...)
			if result.code != ExitUsage {
				t.Fatalf("expected usage exit 2, got %d (stderr %q)", result.code, result.stderr)
			}
		})
	}
}

func TestCreateOversizedInputRejectedLocally(t *testing.T) {
	requestSeen := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen.Store(true)
	}))
	defer server.Close()

	huge := `{"pad":"` + strings.Repeat("x", MaxInputBytes+10) + `"}`
	result := runCLI(t, strings.NewReader(huge),
		map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "create", "-f", "-")
	if result.code != ExitUsage {
		t.Fatalf("expected usage exit, got %d", result.code)
	}
	if requestSeen.Load() {
		t.Fatal("oversized input was transmitted")
	}
}

func TestExplicitIdempotencyKeyIsRespected(t *testing.T) {
	var seenKey atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey.Store(r.Header.Get("Idempotency-Key"))
		jsonHeaders(w)
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Link", `</v1/operations/op-original>; rel="monitor"`)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 1, `{}`, "Running", 0))
	}))
	defer server.Close()

	result := runCLI(t, strings.NewReader(`{"id":"orders-db","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}`),
		map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "create", "-f", "-", "--idempotency-key", "my-explicit-key")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if seenKey.Load() != "my-explicit-key" {
		t.Fatalf("explicit key not honored: %v", seenKey.Load())
	}
	if !strings.Contains(result.stderr, "replays an earlier admission") {
		t.Fatalf("replay notice missing on stderr: %q", result.stderr)
	}
}

func TestResourceGetRendersOutputFreshness(t *testing.T) {
	stale := fmt.Sprintf(resourceFixtureTemplate, 4, `{"storageGB":20}`, "Ready", 3)
	fresh := fmt.Sprintf(`{"id":"orders-db","type":{"name":"PostgreSQLDatabase","version":"v2"},` +
		`"owner":{"kind":"team","id":"payments"},"generation":4,"spec":{"storageGB":20},` +
		`"status":{"state":"Ready","observedGeneration":4,"updatedAt":"2026-08-23T10:00:00Z"},` +
		`"outputs":{"observedGeneration":4,"values":{"port":5432}},` +
		`"createdAt":"2026-08-20T10:00:00Z","updatedAt":"2026-08-23T10:00:00Z"}`)

	for _, tc := range []struct {
		name     string
		body     string
		contains string
		absent   string
	}{
		{"stale outputs labeled", stale, "STALE — outputs describe generation 3, desired generation is 4", "current"},
		{"fresh outputs labeled", fresh, "Outputs (generation 4): current", "STALE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonHeaders(w)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
				"resource", "get", "orders-db")
			if result.code != ExitOK {
				t.Fatalf("exit %d: %s", result.code, result.stderr)
			}
			if !strings.Contains(result.stdout, tc.contains) {
				t.Fatalf("output missing %q:\n%s", tc.contains, result.stdout)
			}
			if strings.Contains(result.stdout, tc.absent+"\n") && tc.name != "fresh outputs labeled" {
				t.Fatalf("output unexpectedly contains %q", tc.absent)
			}
		})
	}
}

func TestUpdatePreReadsGenerationAndSurfacesConflict(t *testing.T) {
	var puts atomic.Int32
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			jsonHeaders(w)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Ready", 5))
		case http.MethodPut:
			puts.Add(1)
			if r.Header.Get("If-Liftr-Generation") != "5" {
				t.Errorf("precondition = %q, want 5", r.Header.Get("If-Liftr-Generation"))
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"type":"https://liftr.dev/problems/generation-conflict","title":"Generation conflict",`+
				`"status":409,"code":"GENERATION_CONFLICT","requestId":"req-x","currentGeneration":9}`)
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{"storageGB":40}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile)
	if result.code != ExitRejected {
		t.Fatalf("expected exit 4, got %d (%s)", result.code, result.stderr)
	}
	if gets.Load() != 1 || puts.Load() != 1 {
		t.Fatalf("conflict handling diverged: gets=%d puts=%d", gets.Load(), puts.Load())
	}
	for _, want := range []string{"GENERATION_CONFLICT", "Current generation: 9", "Request ID: req-x"} {
		if !strings.Contains(result.stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, result.stderr)
		}
	}
}

func TestUpdateExplicitGenerationSkipsPreRead(t *testing.T) {
	var gets, puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
		case http.MethodPut:
			puts.Add(1)
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-u>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 6, `{}`, "Pending", 5))
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile, "--generation", "5")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if gets.Load() != 0 || puts.Load() != 1 {
		t.Fatalf("--generation should skip the pre-read: gets=%d puts=%d", gets.Load(), puts.Load())
	}
}

func TestDeleteSafetyGates(t *testing.T) {
	newServer := func(deleteSeen *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				jsonHeaders(w)
				fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 4, `{}`, "Ready", 4))
			case http.MethodDelete:
				deleteSeen.Add(1)
				jsonHeaders(w)
				w.Header().Set("Link", `</v1/operations/op-d>; rel="monitor"`)
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 4, `{}`, "Deleting", 4))
			}
		}))
	}

	t.Run("non-TTY without --yes refuses and sends nothing", func(t *testing.T) {
		deleteSeen := &atomic.Int32{}
		server := newServer(deleteSeen)
		defer server.Close()
		result := runCLI(t, strings.NewReader(""), map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
			"resource", "delete", "orders-db")
		if result.code != ExitUsage {
			t.Fatalf("expected exit 2, got %d (%s)", result.code, result.stderr)
		}
		if deleteSeen.Load() != 0 {
			t.Fatal("DELETE was admitted without confirmation")
		}
	})

	t.Run("--yes proceeds non-interactively", func(t *testing.T) {
		deleteSeen := &atomic.Int32{}
		server := newServer(deleteSeen)
		defer server.Close()
		result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
			"resource", "delete", "orders-db", "--yes")
		if result.code != ExitOK {
			t.Fatalf("exit %d: %s", result.code, result.stderr)
		}
		if deleteSeen.Load() == 0 {
			t.Fatal("DELETE never sent despite --yes")
		}
	})

	t.Run("interactive exact-ID confirmation", func(t *testing.T) {
		deleteSeen := &atomic.Int32{}
		server := newServer(deleteSeen)
		defer server.Close()
		env := map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken}

		mismatch := runCLIWithTTY(t, true, strings.NewReader("wrong-id\n"), env, "resource", "delete", "orders-db")
		if mismatch.code != ExitFailure {
			t.Fatalf("mismatched confirmation exit = %d", mismatch.code)
		}
		if deleteSeen.Load() != 0 {
			t.Fatal("DELETE admitted after failed confirmation")
		}

		confirmed := runCLIWithTTY(t, true, strings.NewReader("orders-db\n"), env, "resource", "delete", "orders-db")
		if confirmed.code != ExitOK || deleteSeen.Load() == 0 {
			t.Fatalf("confirmed deletion failed: %d %s", confirmed.code, confirmed.stderr)
		}
	})
}

func TestWaitFollowsMonitorOperationToSuccess(t *testing.T) {
	opState := atomic.Value{}
	opState.Store("Pending")
	polls := atomic.Int32{}
	finalRead := atomic.Bool{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-monitor>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-monitor":
			polls.Add(1)
			if polls.Load() >= 2 {
				opState.Store("Succeeded")
			}
			jsonHeaders(w)
			fmt.Fprintf(w, operationFixture, opState.Load())
		case r.URL.Path == "/v1/resources/orders-db":
			finalRead.Store(true)
			jsonHeaders(w)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Ready", 5))
		default:
			t.Errorf("unexpected poll path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait", "--timeout", "30s")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	if !finalRead.Load() {
		t.Fatal("final Resource read never happened")
	}
	if !strings.Contains(result.stdout, `"state":"Ready"`) {
		t.Fatalf("final Resource snapshot not emitted in JSON mode: %s", result.stdout)
	}
	if polls.Load() > 6 {
		t.Fatalf("polling too aggressive: %d polls", polls.Load())
	}
}

func TestWaitOperationFailureExitFive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-monitor>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-monitor":
			jsonHeaders(w)
			fmt.Fprint(w, `{"id":"op-monitor","resourceId":"orders-db","capability":"update",`+
				`"state":"Failed","targetGeneration":5,"requestedAt":"2026-08-23T09:00:00Z",`+
				`"completedAt":"2026-08-23T09:01:00Z",`+
				`"failure":{"reason":"ProvisionFailed","message":"the backend rejected the change"}}`)
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait")
	if result.code != ExitOperationFailed {
		t.Fatalf("expected exit 5, got %d", result.code)
	}
	if !strings.Contains(result.stderr, "ProvisionFailed") {
		t.Fatalf("failure reason not surfaced: %s", result.stderr)
	}
}

func TestWaitTimeoutExitFive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-monitor>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-monitor":
			jsonHeaders(w)
			fmt.Fprint(w, fmt.Sprintf(operationFixture, "Running"))
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	start := time.Now()
	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait", "--timeout", "50ms")
	if result.code != ExitOperationFailed {
		t.Fatalf("expected exit 5, got %d", result.code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout waited too long: %s", elapsed)
	}
	if !strings.Contains(result.stderr, "timed out") || !strings.Contains(result.stderr, "op-monitor") {
		t.Fatalf("timeout message missing details: %s", result.stderr)
	}
}

func TestWaitAuthFailureMidPollExitsThree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-monitor>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-monitor":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"https://liftr.dev/problems/unauthenticated","title":"Unauthenticated",`+
				`"status":401,"code":"UNAUTHENTICATED","requestId":"req-auth"}`)
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait")
	if result.code != ExitAuth {
		t.Fatalf("expected exit 3, got %d (%s)", result.code, result.stderr)
	}
}

func TestWaitOperationVanishesIsProtocolFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-gone>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-gone":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"type":"https://liftr.dev/problems/operation-not-found","title":"Operation not found",`+
				`"status":404,"code":"OPERATION_NOT_FOUND","requestId":"req-404"}`)
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait")
	if result.code != ExitFailure {
		t.Fatalf("expected protocol failure exit 1, got %d", result.code)
	}
}

// TestWaitSuccessWithFailingFinalRead pins the correction: when the Operation
// succeeded but the final Resource GET fails, the Operation did NOT fail —
// the command reports a read failure (exit 1, or 3 for authentication) and
// never emits the stale admission snapshot as a final one.
func TestWaitSuccessWithFailingFinalRead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		finalStatus  int
		expectedCode int
	}{
		{"generic failure exits one", http.StatusInternalServerError, ExitFailure},
		{"authentication failure exits three", http.StatusUnauthorized, ExitAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			polls := atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut:
					jsonHeaders(w)
					w.Header().Set("Link", `</v1/operations/op-final>; rel="monitor"`)
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
				case r.URL.Path == "/v1/operations/op-final":
					if polls.Add(1) >= 2 {
						// keep Succeeded stable across repeated observations
					}
					jsonHeaders(w)
					fmt.Fprint(w, fmt.Sprintf(operationFixture, "Succeeded"))
				case r.URL.Path == "/v1/resources/orders-db":
					w.WriteHeader(tc.finalStatus)
				}
			}))
			defer server.Close()

			specFile := filepath.Join(t.TempDir(), "spec.json")
			os.WriteFile(specFile, []byte(`{}`), 0o600)

			result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
				"-o", "json", "resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait")
			if result.code != tc.expectedCode {
				t.Fatalf("exit = %d, want %d (stderr %s)", result.code, tc.expectedCode, result.stderr)
			}
			if !strings.Contains(result.stderr, "op-final succeeded, but the final Resource could not be retrieved") {
				t.Fatalf("explanatory message missing: %s", result.stderr)
			}
			if result.stdout != "" {
				t.Fatalf("stale admission snapshot leaked to JSON stdout: %s", result.stdout)
			}
		})
	}
}

func TestInterruptDuringWaitExitsPromptly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			jsonHeaders(w)
			w.Header().Set("Link", `</v1/operations/op-int>; rel="monitor"`)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
		case r.URL.Path == "/v1/operations/op-int":
			jsonHeaders(w)
			fmt.Fprint(w, fmt.Sprintf(operationFixture, "Running"))
		}
	}))
	defer server.Close()

	specFile := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(specFile, []byte(`{}`), 0o600)
	t.Setenv("LIFTR_SERVER", server.URL)
	t.Setenv("LIFTR_TOKEN", secretTestToken)

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		code int
	}
	done := make(chan outcome, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Execute(ctx, []string{"resource", "update", "orders-db", "--spec", specFile,
			"--generation", "5", "--wait"},
			&stdout, &stderr, strings.NewReader(""), "test-version")
		done <- outcome{code: code}
	}()
	time.Sleep(60 * time.Millisecond) // let at least one poll happen
	cancel()
	select {
	case o := <-done:
		if o.code != ExitInterrupted {
			t.Fatalf("expected exit 130, got %d", o.code)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("interrupt did not propagate promptly")
	}
}

func TestPlainHTTPRemoteRefusedBeforeAnyRequest(t *testing.T) {
	result := runCLI(t, nil,
		map[string]string{"LIFTR_SERVER": "http://192.0.2.1:9999", "LIFTR_TOKEN": secretTestToken},
		"resource", "get", "orders-db")
	if result.code != ExitUsage {
		t.Fatalf("expected usage/config rejection, got %d", result.code)
	}
	if !strings.Contains(result.combined(), "loopback") {
		t.Fatalf("rejection does not explain the loopback rule: %s", result.stderr)
	}
}

func TestPathPrefixServerConfigurationRefused(t *testing.T) {
	result := runCLI(t, nil,
		map[string]string{"LIFTR_SERVER": "https://liftr.example.com/liftr-api", "LIFTR_TOKEN": secretTestToken},
		"resource", "get", "orders-db")
	if result.code != ExitUsage {
		t.Fatalf("path-prefix origin accepted, exit %d", result.code)
	}
	if !strings.Contains(result.combined(), "origin") {
		t.Fatalf("message should explain origin-only servers: %s", result.stderr)
	}
}

func TestHostileProblemDetailIsSanitizedInTextMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, "{\"type\":\"https://liftr.dev/problems/resource-spec-invalid\",\"title\":\"Invalid\\u001b[31mESC\",\"status\":422,"+
			"\"detail\":\"line one\\nline two\\u001b]0;owned\\u0007\",\"code\":\"RESOURCE_SPEC_INVALID\",\"requestId\":\"req-h\"}")
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "get", "orders-db")
	if result.code != ExitRejected {
		t.Fatalf("expected exit 4, got %d", result.code)
	}
	if strings.ContainsAny(result.combined(), "\x1b\x07") {
		t.Fatalf("terminal control sequences survived sanitization: %q", result.stderr)
	}
	if !strings.Contains(result.stderr, "Request ID: req-h") {
		t.Fatalf("request ID not rendered: %s", result.stderr)
	}
}

func TestTokenNeverAppearsInAnyOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or misconfigured server echoes the credential everywhere.
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"type":"https://liftr.dev/problems/unauthenticated","title":"Unauthenticated %s",`+
			`"status":401,"detail":"rejected credential %s","code":"UNAUTHENTICATED","requestId":"req-t"}`,
			secretTestToken, secretTestToken)
	}))
	defer server.Close()

	result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "get", "orders-db")
	if result.code != ExitAuth {
		t.Fatalf("expected exit 3, got %d", result.code)
	}
	if strings.Contains(result.combined(), secretTestToken) {
		t.Fatalf("credential leaked into output:\n%s", result.combined())
	}
}

func TestTokenFileWarningAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	broadFile := filepath.Join(dir, "token-broad")
	narrowFile := filepath.Join(dir, "token-narrow")
	os.WriteFile(broadFile, []byte("file-token-value\n"), 0o644)
	os.WriteFile(narrowFile, []byte(secretTestToken), 0o600)

	var seenAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		jsonHeaders(w)
		fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 1, `{}`, "Ready", 1))
	}))
	defer server.Close()

	t.Run("flag file beats LIFTR_TOKEN", func(t *testing.T) {
		result := runCLI(t, nil, map[string]string{
			"LIFTR_SERVER": server.URL,
			"LIFTR_TOKEN":  "env-inline-token",
		}, "--token-file", narrowFile, "resource", "get", "orders-db")
		if result.code != ExitOK {
			t.Fatalf("exit %d: %s", result.code, result.stderr)
		}
		if seenAuth.Load() != "Bearer "+secretTestToken {
			t.Fatalf("token precedence wrong: %v", seenAuth.Load())
		}
		if strings.Contains(result.combined(), "readable beyond its owner") {
			t.Fatalf("permission warning shown for 0600 file: %s", result.stderr)
		}
	})

	t.Run("broad permissions warn without leaking contents", func(t *testing.T) {
		result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL},
			"--token-file", broadFile, "resource", "get", "orders-db")
		if result.code != ExitOK {
			t.Fatalf("exit %d: %s", result.code, result.stderr)
		}
		if !strings.Contains(result.stderr, "warning: token file") {
			t.Fatalf("permission warning missing: %s", result.stderr)
		}
	})
}
