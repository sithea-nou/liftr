// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sithea-nou/liftr/internal/identity"
)

// OperatorGrants is the strict M20 subject-to-permission policy. The admin
// authenticator trusts exactly one configured issuer, so subject keys are
// issuer-qualified by that composition boundary. This policy is independent
// from developer owner memberships: neither can grant the other's actions.
// A later milestone may add operator group mapping without changing the
// application-owned OperatorAuthorizer port.
type OperatorGrants struct {
	Subjects map[string][]identity.Action `json:"subjects"`
}

// LoadOperatorGrants loads one required, bounded static operator grants file.
// Unknown fields and unknown actions fail startup rather than granting
// accidentally.
func LoadOperatorGrants(path string) (OperatorGrants, error) {
	if strings.TrimSpace(path) == "" {
		return OperatorGrants{}, fmt.Errorf("operator grants file is required when the admin listener is enabled")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return OperatorGrants{}, fmt.Errorf("read operator grants file: %w", err)
	}
	if len(body) > 1<<20 {
		return OperatorGrants{}, fmt.Errorf("operator grants file exceeds 1 MiB")
	}
	var grants OperatorGrants
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grants); err != nil {
		return OperatorGrants{}, fmt.Errorf("parse operator grants file %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return OperatorGrants{}, fmt.Errorf("parse operator grants file %s: trailing JSON content", path)
	}
	if len(grants.Subjects) == 0 {
		return OperatorGrants{}, fmt.Errorf("operator grants file %s declares no subject grants", path)
	}
	for subject, actions := range grants.Subjects {
		if strings.TrimSpace(subject) == "" || subject != strings.TrimSpace(subject) {
			return OperatorGrants{}, fmt.Errorf("operator grants file %s has a non-canonical subject", path)
		}
		if len(actions) == 0 {
			return OperatorGrants{}, fmt.Errorf("operator grants file %s subject %q declares no actions", path, subject)
		}
		seen := map[identity.Action]struct{}{}
		for _, action := range actions {
			if !identity.ValidOperatorAction(action) {
				return OperatorGrants{}, fmt.Errorf("operator grants file %s subject %q declares unknown action %q", path, subject, action)
			}
			if _, duplicate := seen[action]; duplicate {
				return OperatorGrants{}, fmt.Errorf("operator grants file %s subject %q repeats action %q", path, subject, action)
			}
			seen[action] = struct{}{}
		}
	}
	return grants, nil
}

// StaticOperatorAuthorizer is the deny-by-default M20 operator policy.
type StaticOperatorAuthorizer struct{ Grants OperatorGrants }

func (a StaticOperatorAuthorizer) AuthorizeOperator(_ context.Context, principal identity.Principal, action identity.Action, _ identity.OperatorTarget) error {
	if principal.ID == "" || !identity.ValidOperatorAction(action) {
		return fmt.Errorf("%w: invalid operator decision", ErrDenied)
	}
	for _, granted := range a.Grants.Subjects[principal.Subject] {
		if granted == action {
			return nil
		}
	}
	return fmt.Errorf("%w: operator action %q is not granted", ErrDenied, action)
}
