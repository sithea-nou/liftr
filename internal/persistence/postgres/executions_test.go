// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"strings"
	"testing"
)

func TestSaveExecutionQueryBindsOutputMappingOnceAndRejectsMismatch(t *testing.T) {
	want := []string{
		"output_mapping_ref=CASE WHEN output_mapping_ref='' THEN $19 ELSE output_mapping_ref END",
		"AND (output_mapping_ref='' OR output_mapping_ref=$19)",
	}
	for _, clause := range want {
		if !strings.Contains(saveExecutionQuery, clause) {
			t.Fatalf("SaveExecution query does not enforce %q", clause)
		}
	}
}
