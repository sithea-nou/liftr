// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/sithea-nou/liftr/internal/cli"
	"github.com/sithea-nou/liftr/internal/client"
)

func mustParseReferences(t *testing.T, values ...string) []client.ReferenceBinding {
	t.Helper()
	bindings, err := cli.ParseReferenceFlagsForTest(values)
	if err != nil {
		t.Fatal(err)
	}
	return bindings
}

func TestParseReferenceFlagsSingleAndMulti(t *testing.T) {
	bindings := mustParseReferences(t, "database=res_a", "cache=res_b,res_c")
	if len(bindings) != 2 {
		t.Fatalf("bindings = %+v", bindings)
	}
	bySlot := map[string][]string{}
	for _, binding := range bindings {
		bySlot[binding.Slot] = binding.Targets
	}
	if len(bySlot["cache"]) != 2 || len(bySlot["database"]) != 1 {
		t.Fatalf("bindings = %+v", bindings)
	}
}

func TestParseReferenceFlagsRepeatsAppendAndRejectDuplicates(t *testing.T) {
	bindings := mustParseReferences(t, "dep=res_a", "dep=res_b")
	if len(bindings[0].Targets) != 2 {
		t.Fatalf("repeat did not append: %+v", bindings)
	}
	if _, err := cli.ParseReferenceFlagsForTest([]string{"dep=res_a", "dep=res_a"}); err == nil {
		t.Fatal("duplicate target accepted")
	}
	if _, err := cli.ParseReferenceFlagsForTest([]string{"nonsense"}); err == nil {
		t.Fatal("malformed flag accepted")
	}
}

func TestBuildUpdateBodyPreservesByDefault(t *testing.T) {
	body, err := cli.BuildUpdateBodyForTest([]byte(`{"size":9}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["references"]; present {
		t.Fatalf("default update body must omit references entirely: %s", body)
	}
	if _, present := decoded["spec"]; !present {
		t.Fatalf("update body lost spec: %s", body)
	}
}

func TestBuildUpdateBodyExplicitReplacementAndClear(t *testing.T) {
	body, err := cli.BuildUpdateBodyForTest([]byte(`{"size":9}`), []string{"dependency=res_x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Spec       json.RawMessage     `json:"spec"`
		References map[string][]string `json:"references"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.References["dependency"]) != 1 || decoded.References["dependency"][0] != "res_x" {
		t.Fatalf("replacement references missing: %s", body)
	}

	cleared, err := cli.BuildUpdateBodyForTest([]byte(`{"size":9}`), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var clearedDecoded map[string]json.RawMessage
	if err := json.Unmarshal(cleared, &clearedDecoded); err != nil {
		t.Fatal(err)
	}
	raw := string(clearedDecoded["references"])
	if raw != "{}" {
		t.Fatalf("--clear-references must send explicit {}, got %s", cleared)
	}

	if _, err := cli.BuildUpdateBodyForTest([]byte(`{}`), []string{"a=b"}, true); err == nil && !errors.Is(err, err) {
		t.Fatal("unreachable")
	} else if err == nil {
		t.Fatal("--reference and --clear-references combined must fail")
	}
}
