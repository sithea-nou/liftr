// SPDX-License-Identifier: Apache-2.0

package opentofu_test

import (
	"errors"
	"testing"

	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
)

func TestAttemptPhaseEdges(t *testing.T) {
	allowed := map[opentofu.AttemptPhase][]opentofu.AttemptPhase{
		opentofu.AttemptPrepared:            {opentofu.AttemptApplyMayStart},
		opentofu.AttemptApplyMayStart:       {opentofu.AttemptApplyExited, opentofu.AttemptApplyOutcomeUnknown},
		opentofu.AttemptApplyExited:         {opentofu.AttemptObservedConverged},
		opentofu.AttemptApplyOutcomeUnknown: {opentofu.AttemptObservedConverged},
	}
	phases := []opentofu.AttemptPhase{
		opentofu.AttemptPrepared,
		opentofu.AttemptApplyMayStart,
		opentofu.AttemptApplyExited,
		opentofu.AttemptApplyOutcomeUnknown,
		opentofu.AttemptObservedConverged,
	}
	for _, from := range phases {
		for _, to := range phases {
			want := false
			for _, candidate := range allowed[from] {
				want = want || candidate == to
			}
			if got := from.CanAdvanceTo(to); got != want {
				t.Errorf("%s.CanAdvanceTo(%s) = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestEvidenceIdentityValidation(t *testing.T) {
	if err := (opentofu.AttemptKey{}).Validate(); !errors.Is(err, opentofu.ErrInvalidEvidence) {
		t.Fatalf("empty attempt key error = %v", err)
	}
	if err := (opentofu.LeaseFence{}).Validate(); !errors.Is(err, opentofu.ErrInvalidEvidence) {
		t.Fatalf("empty fence error = %v", err)
	}
	if err := (opentofu.StateEvidence{}).Validate(); !errors.Is(err, opentofu.ErrInvalidEvidence) {
		t.Fatalf("empty state evidence error = %v", err)
	}
}
