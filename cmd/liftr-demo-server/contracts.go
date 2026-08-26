// SPDX-License-Identifier: Apache-2.0

package main

// Demo-only ResourceType contracts. These types exist exclusively for the
// local demonstration composition in this directory; they are never registered
// by cmd/liftr-server and carry no real backend semantics. The deterministic
// behaviors are documented on each schema below.

import (
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

func demoTypeRef(name string) domain.ResourceTypeRef {
	return domain.ResourceTypeRef{Name: name, Version: "v1"}
}

// databaseSchema describes DemoDatabase/v1: a stand-in dependency Resource.
// While spec.hold is true the deterministic provisioner reports the resource
// NotReady; setting hold to false through an ordinary update releases it.
const databaseSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:DemoDatabase:v1:spec",
  "title": "DemoDatabase/v1 ResourceSpec (non-production demo type)",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine", "sizeGB", "hold"],
  "properties": {
    "engine": {
      "type": "string",
      "minLength": 1,
      "description": "Free-form engine label; the demo provisioner ignores it."
    },
    "sizeGB": {
      "type": "integer",
      "minimum": 1,
      "description": "Free-form size label; the demo provisioner ignores it."
    },
    "hold": {
      "type": "boolean",
      "description": "Deterministic demo gate: while true the provisioner keeps reporting NotReady."
    }
  }
}`

// appSchema describes DemoApp/v1: a dependent Resource whose contract declares
// one required hard-dependency slot named "database" targeting DemoDatabase/v1.
const appSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:DemoApp:v1:spec",
  "title": "DemoApp/v1 ResourceSpec (non-production demo type)",
  "type": "object",
  "additionalProperties": false,
  "required": ["image", "hold"],
  "properties": {
    "image": {
      "type": "string",
      "minLength": 1,
      "description": "Free-form image label; the demo provisioner ignores it."
    },
    "hold": {
      "type": "boolean",
      "description": "Deterministic demo gate: while true the provisioner keeps reporting NotReady after submission."
    },
    "holdDelete": {
      "type": "boolean",
      "description": "Deterministic demo teardown gate: while true deletion remains in progress until the demo control plane releases it."
    }
  }
}`

// faultSchema describes DemoFault/v1: scripted provider behavior for operator
// diagnostics demos. scenario values:
//
//   - clean: every execution succeeds immediately.
//   - failure: create/update executions fail conclusively and deterministically
//     forever (delete still succeeds so cleanup stays simple).
//   - ambiguous: the FIRST submission attempt of each operation reports an
//     unknown outcome; Observe then resolves the truth, so Liftr converges
//     without ever resubmitting infrastructure.
const faultSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:DemoFault:v1:spec",
  "title": "DemoFault/v1 ResourceSpec (non-production demo type)",
  "type": "object",
  "additionalProperties": false,
  "required": ["scenario"],
  "properties": {
    "scenario": {
      "type": "string",
      "enum": ["clean", "failure", "ambiguous"],
      "description": "Scripted deterministic provider behavior for this Resource."
    }
  }
}`

// freeTransitions allows every spec replacement; demo contracts carry no
// migration-style constraints.
func freeTransitions(_, _ map[string]any) []resourcetypes.TransitionViolation {
	return nil
}

func buildContracts() ([]resourcetypes.Contract, error) {
	databaseType, err := domain.NewResourceType(demoTypeRef("DemoDatabase"),
		"Non-production deterministic dependency resource used only by the local Liftr demo server.",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		return nil, err
	}
	appType, err := domain.NewResourceType(demoTypeRef("DemoApp"),
		"Non-production deterministic dependent resource used only by the local Liftr demo server.",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		return nil, err
	}
	faultType, err := domain.NewResourceType(demoTypeRef("DemoFault"),
		"Non-production scripted-failure resource used only by the local Liftr demo server.",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		return nil, err
	}
	databaseContract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        databaseType,
		DisplayName: "Demo Database (local demo only)",
		SpecSchema:  []byte(databaseSchema),
		Transitions: freeTransitions,
	})
	if err != nil {
		return nil, err
	}
	appContract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        appType,
		DisplayName: "Demo App (local demo only)",
		SpecSchema:  []byte(appSchema),
		Transitions: freeTransitions,
		References:  []resourcecontract.ReferenceSlot{{Name: "database", AllowedTargetTypes: []domain.ResourceTypeRef{demoTypeRef("DemoDatabase")}, MinItems: 1, MaxItems: 1}},
	})
	if err != nil {
		return nil, err
	}
	faultContract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        faultType,
		DisplayName: "Demo Fault (local demo only)",
		SpecSchema:  []byte(faultSchema),
		Transitions: freeTransitions,
	})
	if err != nil {
		return nil, err
	}
	return []resourcetypes.Contract{appContract, databaseContract, faultContract}, nil
}
