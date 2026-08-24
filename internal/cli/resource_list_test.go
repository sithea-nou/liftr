// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const resourceListPageFixture = `{"items":[{"id":"orders-db","type":{"name":"PostgreSQLDatabase","version":"v2"},"owner":{"kind":"team","id":"payments"},"generation":3,"status":{"state":"Ready","observedGeneration":3,"updatedAt":"2026-08-24T09:30:00Z"},"latestOperation":{"id":"op-9","capability":"update","state":"Succeeded","targetGeneration":3,"href":"/v1/operations/op-9"},"createdAt":"2026-08-20T10:00:00Z","updatedAt":"2026-08-24T09:30:00Z"}],"nextCursor":"r1_cursor+/="}`

func TestResourceListPaginationJSONAndTextOutput(t *testing.T) {
	var seenQuery atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery.Store(r.URL.RawQuery)
		if r.URL.Path != "/v1/resources" {
			t.Errorf("request path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+secretTestToken {
			t.Errorf("Authorization = %q", got)
		}
		jsonHeaders(w)
		fmt.Fprint(w, resourceListPageFixture)
	}))
	defer server.Close()
	env := map[string]string{"LIFTR_SERVER": server.URL, "LIFTR_TOKEN": secretTestToken}

	jsonRun := runCLI(t, nil, env, "-o", "json", "resource", "list",
		"--owner", "team=payments", "--type", "PostgreSQLDatabase", "--version", "v2",
		"--state", "Ready", "--include-deleted", "--limit", "25", "--cursor", "r1_prev+/=")
	wantQuery := "cursor=r1_prev%2B%2F%3D&includeDeleted=true&limit=25&ownerId=payments&ownerKind=team&state=Ready&type=PostgreSQLDatabase&version=v2"
	if jsonRun.code != ExitOK || jsonRun.stdout != resourceListPageFixture+"\n" {
		t.Fatalf("JSON list = code %d stdout %q stderr %q", jsonRun.code, jsonRun.stdout, jsonRun.stderr)
	}
	if got := seenQuery.Load().(string); got != wantQuery {
		t.Fatalf("query = %q, want %q", got, wantQuery)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonRun.stdout), &decoded); err != nil {
		t.Fatalf("stdout is not one JSON document: %v", err)
	}
	if !strings.Contains(jsonRun.stderr, "--cursor r1_cursor+/=") {
		t.Fatalf("continuation guidance missing: %q", jsonRun.stderr)
	}

	textRun := runCLI(t, nil, env, "resource", "list")
	for _, want := range []string{
		"ID", "TYPE", "OWNER", "STATE", "GENERATION", "OBSERVED", "LATEST OPERATION",
		"orders-db", "PostgreSQLDatabase/v2", "team/payments", "Ready", "op-9/Succeeded",
	} {
		if !strings.Contains(textRun.stdout, want) {
			t.Fatalf("text list missing %q:\n%s", want, textRun.stdout)
		}
	}
	for _, banned := range []string{"spec", "outputs", "conditions"} {
		if strings.Contains(textRun.stdout, banned) {
			t.Fatalf("text list printed %q", banned)
		}
	}
}

func TestResourceListRejectsInvalidInputLocally(t *testing.T) {
	requests := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	env := map[string]string{"LIFTR_SERVER": server.URL}
	cases := [][]string{
		{"resource", "list", "--limit", "0"},
		{"resource", "list", "--limit", "101"},
		{"resource", "list", "--version", "v2"},
		{"resource", "list", "--state", "Exploded"},
		{"resource", "list", "--owner", "team-only"},
		{"resource", "list", "--extra"},
	}
	for _, args := range cases {
		result := runCLI(t, nil, env, args...)
		if result.code != ExitUsage {
			t.Fatalf("%v exit = %d, want usage error", args, result.code)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid invocations sent %d requests", requests.Load())
	}
}
