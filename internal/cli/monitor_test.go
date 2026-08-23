// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMutationWithoutMonitorMetadataWarnsButSucceeds pins the adversarial
// case "missing monitor metadata": the admission itself still succeeds, a
// visible warning names the unusable reference, and --wait turns it into an
// explicit protocol failure instead of guessing via latestOperation.
func TestMutationWithoutMonitorMetadata(t *testing.T) {
	newServer := func(withMonitor bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPut:
				jsonHeaders(w)
				if withMonitor {
					w.Header().Set("Link", `</v1/operations/op-m>; rel="monitor"`)
				}
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Pending", 4))
			case http.MethodGet:
				jsonHeaders(w)
				fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 5, `{}`, "Ready", 5))
			}
		}))
	}

	t.Run("admission succeeds with warning", func(t *testing.T) {
		server := newServer(false)
		defer server.Close()
		specFile := filepath.Join(t.TempDir(), "spec.json")
		os.WriteFile(specFile, []byte(`{}`), 0o600)

		result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
			"resource", "update", "orders-db", "--spec", specFile, "--generation", "5")
		if result.code != ExitOK {
			t.Fatalf("exit %d: %s", result.code, result.stderr)
		}
		if !strings.Contains(result.stderr, "warning: the admission carries no usable monitor reference") {
			t.Fatalf("warning missing: %s", result.stderr)
		}
	})

	t.Run("--wait fails instead of guessing", func(t *testing.T) {
		server := newServer(false)
		defer server.Close()
		specFile := filepath.Join(t.TempDir(), "spec.json")
		os.WriteFile(specFile, []byte(`{}`), 0o600)

		result := runCLI(t, nil, map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
			"resource", "update", "orders-db", "--spec", specFile, "--generation", "5", "--wait")
		if result.code != ExitFailure {
			t.Fatalf("expected protocol failure, got %d", result.code)
		}
		if !strings.Contains(result.stderr, "cannot determine the admitted Operation") {
			t.Fatalf("protocol failure message missing: %s", result.stderr)
		}
	})
}

// TestCreateIdempotencyKeyGeneratedOncePerInvocation pins that one invocation
// sends exactly one generated key even when internal retries occur.
func TestCreateIdempotencyKeyGeneratedOncePerInvocation(t *testing.T) {
	var attempts atomic.Int32
	keys := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonHeaders(w)
		w.Header().Set("Link", `</v1/operations/op-c>; rel="monitor"`)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, fmt.Sprintf(resourceFixtureTemplate, 1, `{}`, "Pending", 0))
	}))
	defer server.Close()

	result := runCLI(t, strings.NewReader(`{"id":"orders-db","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}`),
		map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"-o", "json", "resource", "create", "-f", "-")
	if result.code != ExitOK {
		t.Fatalf("exit %d: %s", result.code, result.stderr)
	}
	close(keys)
	seen := map[string]int{}
	for key := range keys {
		seen[key]++
	}
	if len(seen) != 1 || attempts.Load() != 2 {
		t.Fatalf("retry minted or diverged keys: %v attempts=%d", seen, attempts.Load())
	}
	for key := range seen {
		if !strings.HasPrefix(key, "cli-") {
			t.Fatalf("generated key format unexpected: %q", key)
		}
	}
}

// TestAmbiguousMutationFailurePrintsReplayGuidance pins that a final
// transport failure surfaces the idempotency key with replay instructions.
func TestAmbiguousMutationFailurePrintsReplayGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	result := runCLI(t, strings.NewReader(`{"id":"orders-db","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}`),
		map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken},
		"resource", "create", "-f", "-", "--idempotency-key", "cli-explicit-replay-me")
	if result.code != ExitFailure {
		t.Fatalf("exit %d", result.code)
	}
	for _, want := range []string{
		"could not be determined",
		"--idempotency-key cli-explicit-replay-me",
	} {
		if !strings.Contains(result.stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, result.stderr)
		}
	}
}
