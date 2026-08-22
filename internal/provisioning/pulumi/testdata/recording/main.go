// SPDX-License-Identifier: Apache-2.0

// Command recording is Liftr's deterministic CI Pulumi program. It exercises
// real Pulumi invocation, the file backend, program input plumbing, stack
// identity, history correlation, and create/update/delete orchestration —
// without pretending to be a PostgreSQL database or requiring any cloud
// credentials. It validates the private input envelope strictly and fails
// deterministically on malformed input so failure-normalization paths stay
// testable.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const envelopeVersion = 1

var (
	infraNamePattern  = regexp.MustCompile(`^liftr-[0-9a-f]{20}$`)
	validCapabilities = map[string]struct{}{
		"create": {}, "update": {}, "delete": {},
	}
)

type envelope struct {
	InputVersion        int    `json:"inputVersion"`
	Capability          string `json:"capability"`
	ResourceID          string `json:"resourceId"`
	ResourceTypeName    string `json:"resourceTypeName"`
	ResourceTypeVersion string `json:"resourceTypeVersion"`
	TargetGeneration    uint64 `json:"targetGeneration"`
	InfraName           string `json:"infraName"`
	Spec                struct {
		Version          string      `json:"version"`
		StorageGB        json.Number `json:"storageGB"`
		HighAvailability bool        `json:"highAvailability"`
	} `json:"spec"`
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		path := os.Getenv("LIFTR_INPUT_FILE")
		if path == "" {
			return fmt.Errorf("LIFTR_INPUT_FILE is required")
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read program input: %w", err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(contents)))
		decoder.UseNumber()
		var input envelope
		if err := decoder.Decode(&input); err != nil {
			return fmt.Errorf("decode program input: %w", err)
		}
		if input.InputVersion != envelopeVersion {
			return fmt.Errorf("unsupported input envelope version %d", input.InputVersion)
		}
		if _, ok := validCapabilities[input.Capability]; !ok {
			return fmt.Errorf("unsupported capability %q", input.Capability)
		}
		if strings.TrimSpace(input.ResourceID) == "" {
			return fmt.Errorf("resource ID is required")
		}
		if !infraNamePattern.MatchString(input.InfraName) {
			return fmt.Errorf("infrastructure name %q does not satisfy the platform-scoped format", input.InfraName)
		}
		if strings.TrimSpace(input.Spec.Version) == "" {
			return fmt.Errorf("spec property %q must be a non-empty string", "version")
		}
		if _, err := strconv.ParseInt(input.Spec.StorageGB.String(), 10, 64); err != nil {
			return fmt.Errorf("spec property %q must be a canonical integral number, got %s", "storageGB", input.Spec.StorageGB.String())
		}
		storage, _ := strconv.ParseInt(input.Spec.StorageGB.String(), 10, 64)
		if storage < 1 {
			return fmt.Errorf("spec property %q must be at least 1", "storageGB")
		}
		ctx.Log.Info(fmt.Sprintf("recording program accepted capability=%q resource=%q infra=%q storage=%d",
			input.Capability, input.ResourceID, input.InfraName, storage), nil)
		return nil
	})
}
