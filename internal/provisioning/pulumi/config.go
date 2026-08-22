// SPDX-License-Identifier: Apache-2.0

// Package pulumi implements Liftr's provider-neutral Provisioner contract with
// the Pulumi Automation API.
package pulumi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	pulumiworkspace "github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

const (
	CLIVersion       = "3.257.0"
	inputEnvironment = "LIFTR_INPUT_FILE"
)

// StackNamingVersion names the algorithm that derives deterministic stack
// identity. It is immutable adapter configuration; only v1 exists.
type StackNamingVersion string

const (
	StackNamingVersionV1 StackNamingVersion = "v1"
)

type EnvironmentProvider func(context.Context) (map[string]string, error)

type Input struct {
	OperationID      domain.OperationID
	AttemptNumber    uint64
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Capability       domain.Capability
	Spec             domain.ResourceSpec
	TargetGeneration uint64
}

type InputEncoder func(Input) ([]byte, error)

type Program struct {
	ResourceType domain.ResourceTypeRef
	Capabilities []domain.Capability
	ProjectName  string
	SourceDir    string
	SourceDigest string
	EncodeInput  InputEncoder
	// RequiredEnvironment declares the names of the additional child-process
	// environment variables this program needs from the platform callback.
	// Only names are declared here; values are supplied at execution time and
	// never outlive the isolated workspace. Declared names that the platform
	// does not supply cause a conclusive preflight rejection.
	RequiredEnvironment     []string
	SecretInputsUnsupported bool
}

type Config struct {
	Identity            string
	StackNamingVersion  StackNamingVersion
	PulumiRoot          string
	GoExecutable        string
	BackendURL          string
	StackNamespace      string
	WorkspaceRoot       string
	Programs            []Program
	Environment         EnvironmentProvider
	HistoryPageSize     int
	HistoryMaximumPages int
	StaleWorkspaceAge   time.Duration
}

func (c Config) validate() (map[domain.ResourceTypeRef]Program, error) {
	if strings.TrimSpace(c.Identity) == "" || strings.TrimSpace(c.StackNamespace) == "" {
		return nil, fmt.Errorf("configuration identity and stack namespace are required")
	}
	if strings.TrimSpace(string(c.StackNamingVersion)) == "" {
		return nil, fmt.Errorf("stack naming version is required")
	}
	if c.StackNamingVersion != StackNamingVersionV1 {
		return nil, fmt.Errorf("unsupported stack naming version %q", c.StackNamingVersion)
	}
	if !filepath.IsAbs(c.PulumiRoot) || !filepath.IsAbs(c.GoExecutable) || !filepath.IsAbs(c.WorkspaceRoot) {
		return nil, fmt.Errorf("Pulumi, Go, and workspace paths must be absolute")
	}
	goInfo, err := os.Stat(c.GoExecutable)
	if err != nil || !goInfo.Mode().IsRegular() || !isExecutable(goInfo.Mode()) {
		return nil, fmt.Errorf("preinstalled Go runtime is unavailable or not executable")
	}
	backend, err := url.Parse(c.BackendURL)
	if err != nil || backend.Scheme != "file" || backend.Host != "" || !filepath.IsAbs(backend.Path) || backend.User != nil || backend.RawQuery != "" || backend.Fragment != "" {
		return nil, fmt.Errorf("v0.1 requires a private file backend URL")
	}
	if c.HistoryPageSize <= 0 || c.HistoryMaximumPages <= 0 {
		return nil, fmt.Errorf("positive history scan bounds are required")
	}
	if c.StaleWorkspaceAge <= 0 {
		return nil, fmt.Errorf("positive stale workspace age is required")
	}
	if len(c.Programs) == 0 {
		return nil, fmt.Errorf("at least one Pulumi program is required")
	}
	programs := make(map[domain.ResourceTypeRef]Program)
	for _, program := range c.Programs {
		if strings.TrimSpace(program.ResourceType.Name) == "" || strings.TrimSpace(program.ResourceType.Version) == "" ||
			strings.TrimSpace(program.ProjectName) == "" || !filepath.IsAbs(program.SourceDir) || program.EncodeInput == nil {
			return nil, fmt.Errorf("Pulumi program registration is incomplete")
		}
		if !program.SecretInputsUnsupported {
			return nil, fmt.Errorf("v0.1 programs must reject secret-bearing input")
		}
		sourceInfo, err := os.Lstat(program.SourceDir)
		if err != nil || !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("Pulumi source must be a real directory")
		}
		project, err := pulumiworkspace.LoadProject(filepath.Join(program.SourceDir, "Pulumi.yaml"))
		if err != nil || string(project.Name) != program.ProjectName || project.Runtime.Name() != "go" || project.Backend != nil {
			return nil, fmt.Errorf("v0.1 requires a matching Go Pulumi project without a source-defined backend")
		}
		binary, ok := project.Runtime.Options()["binary"].(string)
		if !ok || binary == "" || filepath.IsAbs(binary) || strings.HasPrefix(filepath.Clean(binary), ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("v0.1 requires a source-relative prebuilt Go program")
		}
		binaryInfo, err := os.Stat(filepath.Join(program.SourceDir, binary))
		if err != nil || !binaryInfo.Mode().IsRegular() || !isExecutable(binaryInfo.Mode()) {
			return nil, fmt.Errorf("prebuilt Go program is unavailable or not executable")
		}
		digest, err := SourceDigest(program.SourceDir)
		if err != nil {
			return nil, fmt.Errorf("validate Pulumi source: %w", err)
		}
		if !strings.EqualFold(digest, program.SourceDigest) {
			return nil, fmt.Errorf("Pulumi source digest does not match its registration")
		}
		if len(program.Capabilities) == 0 {
			return nil, fmt.Errorf("Pulumi program capabilities are required")
		}
		if err := validateRequiredEnvironment(program.RequiredEnvironment); err != nil {
			return nil, err
		}
		seen := make(map[domain.Capability]struct{}, len(program.Capabilities))
		for _, capability := range program.Capabilities {
			if capability != domain.CapabilityCreate && capability != domain.CapabilityUpdate && capability != domain.CapabilityDelete {
				return nil, fmt.Errorf("unsupported Pulumi program capability")
			}
			if _, exists := seen[capability]; exists {
				return nil, fmt.Errorf("Pulumi program capability is duplicated")
			}
			seen[capability] = struct{}{}
		}
		if _, exists := programs[program.ResourceType]; exists {
			return nil, fmt.Errorf("duplicate Pulumi program registration for resource type %s/%s", program.ResourceType.Name, program.ResourceType.Version)
		}
		programs[program.ResourceType] = program
	}
	return programs, nil
}

func isExecutable(mode os.FileMode) bool {
	return runtime.GOOS == "windows" || mode.Perm()&0o111 != 0
}

func SourceDigest(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return fmt.Errorf("source contains unsupported file type")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if forbiddenSourcePath(relative) {
			return fmt.Errorf("source contains mutable Pulumi workspace data")
		}
		_, _ = hash.Write([]byte(relative))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte((info.Mode().Perm() & 0o700).String()))
		_, _ = hash.Write([]byte{0})
		if info.Mode().IsRegular() {
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, _ = hash.Write(contents)
		}
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func forbiddenSourcePath(relative string) bool {
	base := filepath.Base(relative)
	return relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) ||
		relative == ".pulumi" || strings.HasPrefix(relative, ".pulumi"+string(filepath.Separator)) ||
		(strings.HasPrefix(base, "Pulumi.") && base != "Pulumi.yaml" && base != "Pulumi.yml" && base != "Pulumi.json")
}

func unknownFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}
}

func sortCapabilities(capabilities []provisioning.ProvisionerCapability) {
	sort.Slice(capabilities, func(i, j int) bool {
		left := capabilities[i].ResourceType.Name + "\x00" + capabilities[i].ResourceType.Version + "\x00" + string(capabilities[i].Capability)
		right := capabilities[j].ResourceType.Name + "\x00" + capabilities[j].ResourceType.Version + "\x00" + string(capabilities[j].Capability)
		return left < right
	})
}
