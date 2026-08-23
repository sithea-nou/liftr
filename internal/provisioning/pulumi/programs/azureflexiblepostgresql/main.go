// SPDX-License-Identifier: Apache-2.0

// Command azureflexiblepostgresql is Liftr's private reference Pulumi
// program for the PostgreSQLDatabase developer contracts. It implements the
// developer contract on Azure Database for PostgreSQL Flexible Server:
//
//	version          -> engine major version
//	storageGB        -> allocated storage in GB (grow-only per contract)
//	highAvailability -> platform-configured HA mode or Disabled
//
// Validation status: this program has not been executed against live Azure
// infrastructure yet. It becomes validated only after the opt-in acceptance
// suite (TestAzureFlexibleServerLifecycle) passes against a real
// subscription.
//
// The administrative password is generated inside this program as a Pulumi
// secret and is stable across updates: the RandomPassword resource keeps its
// URN and inputs unchanged between create and update invocations, so no
// rotation can occur. The password is never exported; Liftr reads exactly one
// allowlisted non-secret output envelope (hostname and port) and nothing else.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	pf "github.com/pulumi/pulumi-azure-native-sdk/dbforpostgresql/v2"
	"github.com/pulumi/pulumi-azure-native-sdk/resources/v2"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	envelopeVersion    = 1
	outputMappingRef   = "liftr-azure-pg-outputs-v1"
	outputEnvelopeName = "liftrOutputs"
)

type envelope struct {
	InputVersion     int    `json:"inputVersion"`
	Capability       string `json:"capability"`
	ResourceID       string `json:"resourceId"`
	InfraName        string `json:"infraName"`
	TargetGeneration uint64 `json:"targetGeneration"`
	Platform         struct {
		Location             string `json:"location"`
		SkuName              string `json:"skuName"`
		SkuTier              string `json:"skuTier"`
		HighAvailabilityMode string `json:"highAvailabilityMode"`
		AdministratorLogin   string `json:"administratorLogin"`
	} `json:"platform"`
	Spec struct {
		Version          string      `json:"version"`
		StorageGB        json.Number `json:"storageGB"`
		HighAvailability bool        `json:"highAvailability"`
	} `json:"spec"`
}

type decoded struct {
	envelope
	storageValue int64
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		input, err := readEnvelope()
		if err != nil {
			return err
		}
		ctx.Log.Info(fmt.Sprintf("PostgreSQLDatabase capability=%q infra=%q version=%s storage=%d ha=%t",
			input.Capability, input.InfraName, input.Spec.Version, input.storageValue, input.Spec.HighAvailability), nil)

		password, err := random.NewRandomPassword(ctx, input.InfraName+"-admin-password", &random.RandomPasswordArgs{
			Length:  pulumi.Int(24),
			Special: pulumi.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("generate administrator password: %w", err)
		}

		resourceGroup, err := resources.NewResourceGroup(ctx, input.InfraName+"-rg", &resources.ResourceGroupArgs{
			Location: pulumi.String(input.Platform.Location),
		})
		if err != nil {
			return fmt.Errorf("create resource group: %w", err)
		}

		highAvailabilityMode := "Disabled"
		if input.Spec.HighAvailability {
			highAvailabilityMode = input.Platform.HighAvailabilityMode
		}

		serverOutput, err := pf.NewServer(ctx, input.InfraName, &pf.ServerArgs{
			ServerName:                 pulumi.String(input.InfraName),
			ResourceGroupName:          resourceGroup.Name,
			Location:                   pulumi.String(input.Platform.Location),
			Version:                    pulumi.String(input.Spec.Version),
			Sku:                        &pf.SkuArgs{Name: pulumi.String(input.Platform.SkuName), Tier: pulumi.String(input.Platform.SkuTier)},
			Storage:                    &pf.StorageArgs{StorageSizeGB: pulumi.Int(int(input.storageValue))},
			AdministratorLogin:         pulumi.String(input.Platform.AdministratorLogin),
			AdministratorLoginPassword: password.Result,
			HighAvailability:           &pf.HighAvailabilityArgs{Mode: pulumi.String(highAvailabilityMode)},
			Backup:                     &pf.BackupTypeArgs{BackupRetentionDays: pulumi.Int(7)},
		}, pulumi.IgnoreChanges([]string{"administratorLoginPassword"}))
		if err != nil {
			return fmt.Errorf("create flexible server: %w", err)
		}

		if input.Capability == "delete" {
			return nil
		}
		// The single allowlisted non-secret export. The generated password is
		// never exported; hostname is the server's endpoint and port is the
		// PostgreSQL wire-protocol constant derived by this mapping. The
		// envelope echoes the persisted mapping identity and execution
		// identity so the control plane can verify provenance before use.
		ctx.Export(outputEnvelopeName, pulumi.Map{
			"version":          pulumi.Int(1),
			"mapping":          pulumi.String(outputMappingRef),
			"resourceId":       pulumi.String(input.ResourceID),
			"targetGeneration": pulumi.Int(int(input.TargetGeneration)),
			"values": pulumi.Map{
				"hostname": serverOutput.FullyQualifiedDomainName,
				"port":     pulumi.Int(5432),
			},
		})
		return nil
	})
}

func readEnvelope() (*decoded, error) {
	path := os.Getenv("LIFTR_INPUT_FILE")
	if path == "" {
		return nil, fmt.Errorf("LIFTR_INPUT_FILE is required")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read program input: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.UseNumber()
	var value envelope
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode program input: %w", err)
	}
	if value.InputVersion != envelopeVersion {
		return nil, fmt.Errorf("unsupported input envelope version %d", value.InputVersion)
	}
	switch value.Capability {
	case "create", "update", "delete":
	default:
		return nil, fmt.Errorf("unsupported capability %q", value.Capability)
	}
	if strings.TrimSpace(value.InfraName) == "" {
		return nil, fmt.Errorf("infrastructure name is required")
	}
	if strings.TrimSpace(value.Spec.Version) == "" {
		return nil, fmt.Errorf("spec property %q must be a non-empty string", "version")
	}
	integral, ok := integralNumber(value.Spec.StorageGB)
	if !ok {
		return nil, fmt.Errorf("spec property %q must carry a canonical integral number, got %s", "storageGB", value.Spec.StorageGB.String())
	}
	return &decoded{envelope: value, storageValue: integral}, nil
}

// integralNumber mirrors the control plane's numeric contract: only exactly
// representable integral values are accepted; fractional, non-finite, and
// out-of-range numbers are rejected rather than rounded.
func integralNumber(value json.Number) (int64, bool) {
	asFloat, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return 0, false
	}
	const maxExact = 9007199254740992.0
	if math.IsNaN(asFloat) || math.IsInf(asFloat, 0) || asFloat != math.Trunc(asFloat) || math.Abs(asFloat) > maxExact {
		return 0, false
	}
	return int64(asFloat), true
}
