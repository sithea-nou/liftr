// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"

	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// A Kubernetes 404 on a managed object is ambiguous by itself: it can mean
// the deterministic object is genuinely absent, or that the API kind itself
// is no longer served (CRD removed). Only the first may ever establish
// managed absence, because conclusive absence drives resubmission
// authorization and delete completion. Every absence-concluding 404 in this
// adapter therefore verifies, live and in the same decision cycle, that the
// target API resource is currently served. Liftr deliberately keeps no
// discovery cache: the freshness bound for an absence conclusion is zero.
// Discovery answers only "is this resource API currently served" — it never
// contributes ownership or correlation context, and discovery failures are
// uncertainty, never absence.
type absenceResolution int

const (
	// absenceProven means the GVR is definitively served right now, so the
	// already-observed 404 establishes genuine object absence.
	absenceProven absenceResolution = iota
	// kindNotServed means discovery definitively refutes the target API
	// resource. This is a control-plane failure, not managed absence.
	kindNotServed
	// absenceUncertain means discovery could not answer (transport loss,
	// server fault, authorization denial). Absence is unproven.
	absenceUncertain
)

func (a absenceResolution) failure() failurePair {
	switch a {
	case kindNotServed:
		return failurePair{provisioning.FailureUnsupported, reasonTargetKindUnregistered}
	default:
		return failurePair{provisioning.FailureUnavailable, reasonControlPlaneUnavailable}
	}
}

// resolveAbsence performs the live served-GVR verification required behind
// every absence-concluding 404.
func (p *Provisioner) resolveAbsence(ctx context.Context, binding *resolvedBinding) absenceResolution {
	switch p.client.ServedResource(ctx, binding.gvr) {
	case kube.ServedConfirmed:
		return absenceProven
	case kube.ServedRefuted:
		return kindNotServed
	default:
		return absenceUncertain
	}
}
