// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestRequiredEnvironmentReportsEveryMissingVariable(t *testing.T) {
	t.Setenv("LIFTR_TEST_PRESENT", "value")
	t.Setenv("LIFTR_TEST_MISSING_ONE", "")
	t.Setenv("LIFTR_TEST_MISSING_TWO", "")

	values, err := requiredEnvironment(
		"LIFTR_TEST_PRESENT",
		"LIFTR_TEST_MISSING_ONE",
		"LIFTR_TEST_MISSING_TWO",
	)
	if err == nil {
		t.Fatal("expected missing environment error")
	}
	if values != nil {
		t.Fatalf("values = %#v, want nil on invalid configuration", values)
	}
	for _, name := range []string{"LIFTR_TEST_MISSING_ONE", "LIFTR_TEST_MISSING_TWO"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name %s", err, name)
		}
	}
}

func TestRequiredEnvironmentReturnsValues(t *testing.T) {
	t.Setenv("LIFTR_TEST_ONE", "one")
	t.Setenv("LIFTR_TEST_TWO", "two")

	values, err := requiredEnvironment("LIFTR_TEST_ONE", "LIFTR_TEST_TWO")
	if err != nil {
		t.Fatalf("requiredEnvironment: %v", err)
	}
	if values["LIFTR_TEST_ONE"] != "one" || values["LIFTR_TEST_TWO"] != "two" {
		t.Fatalf("values = %#v", values)
	}
}
