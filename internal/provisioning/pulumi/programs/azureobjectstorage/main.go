// SPDX-License-Identifier: Apache-2.0

// Command azureobjectstorage is the private Pulumi implementation used by the
// opt-in ObjectStorage/v1 Azure acceptance scenario. It is not registered by
// the production server composition.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/storage/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	envelopeVersion    = 1
	outputMappingRef   = "liftr-azure-object-storage-outputs-v1"
	outputEnvelopeName = "liftrOutputs"
)

type envelope struct {
	InputVersion        int    `json:"inputVersion"`
	Capability          string `json:"capability"`
	ResourceID          string `json:"resourceId"`
	ResourceTypeName    string `json:"resourceTypeName"`
	ResourceTypeVersion string `json:"resourceTypeVersion"`
	InfraName           string `json:"infraName"`
	TargetGeneration    uint64 `json:"targetGeneration"`
	Platform            struct {
		Location string `json:"location"`
		SkuName  string `json:"skuName"`
	} `json:"platform"`
	Spec struct {
		AccessFrequency       string `json:"accessFrequency"`
		PermitAnonymousAccess bool   `json:"permitAnonymousAccess"`
	} `json:"spec"`
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		input, err := readEnvelope()
		if err != nil {
			return err
		}
		ctx.Log.Info(fmt.Sprintf("ObjectStorage capability=%q infra=%q accessFrequency=%q anonymous=%t",
			input.Capability, input.InfraName, input.Spec.AccessFrequency, input.Spec.PermitAnonymousAccess), nil)

		resourceGroup, err := resources.NewResourceGroup(ctx, input.InfraName+"-rg", &resources.ResourceGroupArgs{
			Location: pulumi.String(input.Platform.Location),
			Tags: pulumi.StringMap{
				"liftr.io/managed": pulumi.String("true"),
			},
		})
		if err != nil {
			return fmt.Errorf("create resource group: %w", err)
		}

		accessTier := storage.AccessTierHot
		if input.Spec.AccessFrequency == "infrequent" {
			accessTier = storage.AccessTierCool
		}
		account, err := storage.NewStorageAccount(ctx, input.InfraName+"-storage", &storage.StorageAccountArgs{
			AccountName:                  pulumi.String(storageAccountName(input.InfraName)),
			ResourceGroupName:            resourceGroup.Name,
			Location:                     pulumi.String(input.Platform.Location),
			Kind:                         pulumi.String("StorageV2"),
			Sku:                          &storage.SkuArgs{Name: pulumi.String(input.Platform.SkuName)},
			AccessTier:                   accessTier,
			AllowBlobPublicAccess:        pulumi.Bool(input.Spec.PermitAnonymousAccess),
			AllowCrossTenantReplication:  pulumi.Bool(false),
			AllowSharedKeyAccess:         pulumi.Bool(false),
			DefaultToOAuthAuthentication: pulumi.Bool(true),
			EnableHttpsTrafficOnly:       pulumi.Bool(true),
			MinimumTlsVersion:            pulumi.String("TLS1_2"),
			PublicNetworkAccess:          pulumi.String("Enabled"),
			Tags: pulumi.StringMap{
				"liftr.io/managed": pulumi.String("true"),
			},
		})
		if err != nil {
			return fmt.Errorf("create storage account: %w", err)
		}

		if input.Capability == "delete" {
			return nil
		}
		ctx.Export(outputEnvelopeName, pulumi.Map{
			"version":          pulumi.Int(1),
			"mapping":          pulumi.String(outputMappingRef),
			"resourceId":       pulumi.String(input.ResourceID),
			"targetGeneration": pulumi.Int(int(input.TargetGeneration)),
			"values": pulumi.Map{
				"endpoint": account.PrimaryEndpoints.Blob(),
			},
		})
		return nil
	})
}

func readEnvelope() (envelope, error) {
	path := os.Getenv("LIFTR_INPUT_FILE")
	if path == "" {
		return envelope{}, fmt.Errorf("LIFTR_INPUT_FILE is required")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return envelope{}, fmt.Errorf("read program input: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var value envelope
	if err := decoder.Decode(&value); err != nil {
		return envelope{}, fmt.Errorf("decode program input: %w", err)
	}
	if value.InputVersion != envelopeVersion {
		return envelope{}, fmt.Errorf("unsupported input envelope version %d", value.InputVersion)
	}
	switch value.Capability {
	case "create", "update", "delete":
	default:
		return envelope{}, fmt.Errorf("unsupported capability %q", value.Capability)
	}
	if strings.TrimSpace(value.ResourceID) == "" || strings.TrimSpace(value.InfraName) == "" {
		return envelope{}, fmt.Errorf("resource and infrastructure identity are required")
	}
	if value.ResourceTypeName != "ObjectStorage" || value.ResourceTypeVersion != "v1" {
		return envelope{}, fmt.Errorf("unsupported resource type %s/%s", value.ResourceTypeName, value.ResourceTypeVersion)
	}
	if strings.TrimSpace(value.Platform.Location) == "" || strings.TrimSpace(value.Platform.SkuName) == "" {
		return envelope{}, fmt.Errorf("platform location and SKU are required")
	}
	if value.Spec.AccessFrequency != "frequent" && value.Spec.AccessFrequency != "infrequent" {
		return envelope{}, fmt.Errorf("unsupported access frequency %q", value.Spec.AccessFrequency)
	}
	return value, nil
}

// storageAccountName produces a stable, globally unique candidate that obeys
// Azure's 3-24 lowercase-alphanumeric naming constraint. The infrastructure
// identity already scopes the hash to this Liftr installation and resource.
func storageAccountName(infraName string) string {
	digest := sha256.Sum256([]byte(infraName))
	return "liftr" + hex.EncodeToString(digest[:])[:19]
}
