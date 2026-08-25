// SPDX-License-Identifier: Apache-2.0

// Package opentofu contains adapter-private OpenTofu contracts.
package opentofu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

var (
	ErrEvidenceNotFound = errors.New("OpenTofu evidence not found")
	ErrEvidenceConflict = errors.New("OpenTofu evidence conflict")
	ErrFenceRejected    = errors.New("OpenTofu evidence lease fence rejected")
	ErrInvalidEvidence  = errors.New("invalid OpenTofu evidence")
)

// AttemptPhase records how far an OpenTofu invocation is known to have
// progressed. ApplyMayStart is durable ambiguity evidence: once recorded it
// can never be removed or moved backward.
type AttemptPhase string

const (
	AttemptPrepared            AttemptPhase = "Prepared"
	AttemptApplyMayStart       AttemptPhase = "ApplyMayStart"
	AttemptApplyExited         AttemptPhase = "ApplyExited"
	AttemptApplyOutcomeUnknown AttemptPhase = "ApplyOutcomeUnknown"
	AttemptObservedConverged   AttemptPhase = "ObservedConverged"
)

func (p AttemptPhase) Valid() bool {
	switch p {
	case AttemptPrepared, AttemptApplyMayStart, AttemptApplyExited, AttemptApplyOutcomeUnknown, AttemptObservedConverged:
		return true
	default:
		return false
	}
}

// CanAdvanceTo reports the only legal durable phase edges.
func (p AttemptPhase) CanAdvanceTo(next AttemptPhase) bool {
	switch p {
	case AttemptPrepared:
		return next == AttemptApplyMayStart
	case AttemptApplyMayStart:
		return next == AttemptApplyExited || next == AttemptApplyOutcomeUnknown
	case AttemptApplyExited, AttemptApplyOutcomeUnknown:
		return next == AttemptObservedConverged
	default:
		return false
	}
}

// AttemptKey includes both lifecycle identity and provisioner registration
// identity so evidence from a different registration cannot be reused.
type AttemptKey struct {
	ResourceID     domain.ResourceID
	OperationID    domain.OperationID
	AttemptNumber  uint64
	ProvisionerRef string
}

func (k AttemptKey) Validate() error {
	if strings.TrimSpace(string(k.ResourceID)) == "" {
		return fmt.Errorf("%w: resource ID is required", ErrInvalidEvidence)
	}
	if strings.TrimSpace(string(k.OperationID)) == "" {
		return fmt.Errorf("%w: operation ID is required", ErrInvalidEvidence)
	}
	if k.AttemptNumber == 0 {
		return fmt.Errorf("%w: attempt number must be greater than zero", ErrInvalidEvidence)
	}
	if strings.TrimSpace(k.ProvisionerRef) == "" {
		return fmt.Errorf("%w: provisioner registration reference is required", ErrInvalidEvidence)
	}
	return nil
}

// LeaseFence identifies the currently leased durable work item authorizing a
// write. Persistence must validate its expiry using database server time.
type LeaseFence struct {
	MessageID string
	Token     string
}

func (f LeaseFence) Validate() error {
	if strings.TrimSpace(f.MessageID) == "" || strings.TrimSpace(f.Token) == "" {
		return fmt.Errorf("%w: message ID and lease token are required", ErrInvalidEvidence)
	}
	return nil
}

type AttemptEvidence struct {
	Key       AttemptKey
	Phase     AttemptPhase
	Version   uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateBindingIdentity is immutable for the lifetime of a Resource. StateKey
// is the backend object key, not OpenTofu state content.
type StateBindingIdentity struct {
	ResourceID     domain.ResourceID
	ProvisionerRef string
	Engine         string
	Program        string
	Backend        string
	StateKey       string
}

func (i StateBindingIdentity) Validate() error {
	if strings.TrimSpace(string(i.ResourceID)) == "" {
		return fmt.Errorf("%w: resource ID is required", ErrInvalidEvidence)
	}
	for name, value := range map[string]string{
		"provisioner registration reference": i.ProvisionerRef,
		"engine":                             i.Engine,
		"program":                            i.Program,
		"backend":                            i.Backend,
		"state key":                          i.StateKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidEvidence, name)
		}
	}
	return nil
}

// StateDigest is exactly a SHA-256 digest. It is deliberately not an HMAC and
// carries no raw state bytes.
type StateDigest [32]byte

type StateEvidence struct {
	Lineage string
	Serial  uint64
	Digest  StateDigest
}

func (e StateEvidence) Validate() error {
	if strings.TrimSpace(e.Lineage) == "" {
		return fmt.Errorf("%w: state lineage is required", ErrInvalidEvidence)
	}
	return nil
}

type StateBinding struct {
	Identity  StateBindingIdentity
	State     *StateEvidence
	Version   uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EvidenceStore is the complete persistence surface available to the
// OpenTofu adapter. Every mutation is fenced; reads are restart-safe.
type EvidenceStore interface {
	PrepareAttempt(context.Context, AttemptKey, LeaseFence) (AttemptEvidence, error)
	LoadAttempt(context.Context, AttemptKey) (AttemptEvidence, error)
	AdvanceAttempt(context.Context, AttemptKey, LeaseFence, AttemptPhase, uint64, AttemptPhase) (AttemptEvidence, error)
	CreateStateBinding(context.Context, AttemptKey, LeaseFence, StateBindingIdentity) (StateBinding, error)
	LoadStateBinding(context.Context, domain.ResourceID) (StateBinding, error)
	UpdateState(context.Context, AttemptKey, LeaseFence, uint64, StateEvidence) (StateBinding, error)
}
