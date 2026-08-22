// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// InfraName derives the deterministic, platform-scoped private infrastructure
// name for one Liftr Resource under one implementation registration.
//
// A ResourceID is unique within one Liftr control plane, not globally, so the
// name digests stable platform identity together with resource identity:
//
//	platformIdentity | implementationNamespace | ResourceTypeRef | ResourceID
//
// The digest binds to the same immutable configuration family as Pulumi stack
// identity but is computed independently: infrastructure resource naming and
// stack naming are separate private identities. Properties pinned by tests:
//
//   - stable across process restarts, updates, retries, and deletes;
//   - distinct Liftr installations configured with distinct identities never
//     derive the same name for the same ResourceID;
//   - OperationID, generation, and attempt number are not inputs;
//   - the derived name satisfies common backend naming constraints
//     (lowercase alphanumeric and hyphens only, fixed length).
func InfraName(identity, namespace string, ref domain.ResourceTypeRef, id domain.ResourceID) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		identity, namespace, ref.Name, ref.Version, string(id),
	}, "\x00")))
	return "liftr-" + hex.EncodeToString(digest[:])[:20]
}

// reservedEnvironmentNames cannot be declared as required program environment
// variables. They are either constructed by the adapter itself, supplied by
// the global platform passphrase channel, or required for child process
// plumbing; declaring them per-program would create ambiguous ownership of
// the execution environment.
var reservedEnvironmentNames = map[string]struct{}{
	"PULUMI_BACKEND_URL":                          {},
	"PULUMI_CONFIG_PASSPHRASE":                    {},
	"PULUMI_CONFIG_PASSPHRASE_FILE":               {},
	"PULUMI_DISABLE_AUTOMATIC_PLUGIN_ACQUISITION": {},
	"PULUMI_DISABLE_REGISTRY_RESOLVE":             {},
	"PULUMI_DIY_BACKEND_IGNORE_DEPRECATION_ERROR": {},
	"PULUMI_DIY_BACKEND_NO_LEGACY_WARNING":        {},
	"PULUMI_IGNORE_AMBIENT_PLUGINS":               {},
	"PULUMI_SKIP_UPDATE_CHECK":                    {},
	"PULUMI_AUTOMATION_API_SKIP_VERSION_CHECK":    {},
	inputEnvironment:                              {},
	"PATH":                                        {},
	"HOME":                                        {},
}

func validateRequiredEnvironment(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("required environment variable names must not be empty")
		}
		if _, reserved := reservedEnvironmentNames[name]; reserved {
			return fmt.Errorf("required environment variable %q is reserved", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("required environment variable %q is duplicated", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// missingRequiredEnvironmentError reports that a program declared an
// environment variable that the platform did not supply. Detection happens
// before any Pulumi invocation, so it authorizes a conclusive preflight
// rejection without weakening post-launch ambiguity semantics.
type missingRequiredEnvironmentError struct{}

func (missingRequiredEnvironmentError) Error() string {
	return "a required execution environment variable was not supplied"
}

// resolveProgramEnvironment collects the platform-supplied values for one
// program execution. Passphrase variables flow through the existing global
// channel; every other supplied key must be declared by the program
// registration (names-only allowlist), and every declared name must be
// present and non-empty. Values never outlive the isolated workspace.
func resolveProgramEnvironment(ctx context.Context, provider EnvironmentProvider, required []string) (map[string]string, error) {
	supplied := map[string]string{}
	if provider != nil {
		values, err := provider(ctx)
		if err != nil {
			return nil, err
		}
		for key, value := range values {
			if strings.TrimSpace(value) == "" {
				continue
			}
			supplied[key] = value
		}
	}
	resolved := make(map[string]string, len(required)+2)
	declared := make(map[string]struct{}, len(required))
	for _, name := range required {
		declared[name] = struct{}{}
	}
	for key, value := range supplied {
		switch key {
		case "PULUMI_CONFIG_PASSPHRASE", "PULUMI_CONFIG_PASSPHRASE_FILE":
			resolved[key] = value
		default:
			if _, ok := declared[key]; !ok {
				return nil, fmt.Errorf("execution environment contains an undeclared variable")
			}
			resolved[key] = value
		}
	}
	for _, name := range required {
		if _, ok := resolved[name]; !ok {
			return nil, missingRequiredEnvironmentError{}
		}
	}
	return resolved, nil
}
