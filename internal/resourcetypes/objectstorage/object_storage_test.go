// SPDX-License-Identifier: Apache-2.0

package objectstorage_test

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcetypes/objectstorage"
)

func TestResourceTypeCapabilities(t *testing.T) {
	resourceType, err := objectstorage.NewResourceType()
	if err != nil {
		t.Fatal(err)
	}
	if resourceType.Ref() != objectstorage.TypeRef() {
		t.Fatalf("Ref() = %#v, want %#v", resourceType.Ref(), objectstorage.TypeRef())
	}
	for _, capability := range []domain.Capability{
		domain.CapabilityCreate,
		domain.CapabilityUpdate,
		domain.CapabilityDelete,
		domain.CapabilityObserve,
	} {
		if !resourceType.Supports(capability) {
			t.Fatalf("Supports(%q) = false", capability)
		}
	}
}

func TestNewSpec(t *testing.T) {
	for _, test := range []struct {
		name      string
		frequency string
		wantErr   bool
	}{
		{name: "frequent", frequency: "frequent"},
		{name: "infrequent", frequency: "infrequent"},
		{name: "empty", frequency: "", wantErr: true},
		{name: "provider tier", frequency: "Hot", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, err := objectstorage.NewSpec(test.frequency, false)
			if (err != nil) != test.wantErr {
				t.Fatalf("NewSpec() error = %v, wantErr %t", err, test.wantErr)
			}
			if !test.wantErr && spec.Values()["accessFrequency"] != test.frequency {
				t.Fatalf("accessFrequency = %v", spec.Values()["accessFrequency"])
			}
		})
	}
}

func TestOutputContractRequiresEndpoint(t *testing.T) {
	contract, err := objectstorage.Contract()
	if err != nil {
		t.Fatal(err)
	}
	outputs := contract.OutputContract()
	if outputs == nil {
		t.Fatal("endpoint output contract is missing")
	}
	fields := outputs.Fields()
	if len(fields) != 1 || fields[0].Name != "endpoint" || !fields[0].RequiredWhenReady {
		t.Fatalf("unexpected output contract: %+v", fields)
	}
}

func TestPublicSchemaDoesNotLeakImplementationVocabulary(t *testing.T) {
	schema := strings.ToLower(string(objectstorage.SpecSchemaDocument()))
	for _, forbidden := range []string{"azure", "pulumi", "opentofu", "terraform", "resource_group", "replication_type", "account_tier"} {
		if strings.Contains(schema, forbidden) {
			t.Fatalf("public schema contains implementation term %q", forbidden)
		}
	}
}
