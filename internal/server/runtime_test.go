// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func runtimeCatalog(t *testing.T) *appfake.Catalog {
	t.Helper()
	resourceType, err := domain.NewResourceType(provisioningfake.ResourceType(), "runtime test type",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return &appfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): resourceType}}
}

// TestComposeDrivesWorkUntilTerminalThroughWorkerLoop pins the initial
// runtime composition decision: the ticker-driven worker loop settles durable
// work to a terminal lifecycle state without any external pumping.
func TestComposeDrivesWorkUntilTerminalThroughWorkerLoop(t *testing.T) {
	store := appfake.NewStore()
	ref := application.ProvisionerRef("runtime-test-provider")
	composed, err := Compose(Config{
		Transactions:          store,
		Catalog:               runtimeCatalog(t),
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)},
		DefaultProvisionerRef: ref,
		WorkerInterval:        2 * time.Millisecond,
		InsecureAuth:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	composed.StartWorker(ctx)

	spec, err := domain.NewResourceSpec(map[string]any{"value": true})
	if err != nil {
		t.Fatal(err)
	}
	command := application.CreateResourceCommand{
		Actor:          appfake.Principal("tester"),
		ID:             domain.ResourceID("rt-1"),
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec:           spec,
		OperationID:    domain.OperationID("op-rt-1"),
		EventID:        domain.EventID("evt-rt-1"),
		RequestedAt:    time.Now().UTC(),
		IdempotencyKey: "runtime-create",
	}
	if _, err := composed.Service().CreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var state domain.ResourceState
		if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
			record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID("rt-1"))
			if err != nil {
				return err
			}
			state = record.Status.State()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if state == domain.ResourceStateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource did not settle to Ready; state=%s", state)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Shutdown must stop the loop promptly and without goroutine leaks.
	cancel()
	select {
	case <-composed.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}
}

// TestComposeRejectsIncompleteConfiguration pins composition preflight rules:
// durable transactions, a catalog, and a resolvable default provisioner are
// mandatory before the process serves anything.
func TestComposeRejectsIncompleteConfiguration(t *testing.T) {
	ref := application.ProvisionerRef("p")
	base := Config{
		Catalog:               runtimeCatalog(t),
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)},
		DefaultProvisionerRef: ref,
		InsecureAuth:          true,
	}
	tests := []struct {
		name   string
		mutate func(Config) Config
	}{
		{name: "missing transactions", mutate: func(config Config) Config { return base }},
		{name: "no provisioners", mutate: func(config Config) Config {
			config.Transactions = appfake.NewStore()
			config.Provisioners = nil
			return config
		}},
		{name: "unregistered default reference", mutate: func(config Config) Config {
			config.Transactions = appfake.NewStore()
			config.DefaultProvisionerRef = "missing"
			return config
		}},
		{name: "missing default reference", mutate: func(config Config) Config {
			config.Transactions = appfake.NewStore()
			config.DefaultProvisionerRef = ""
			return config
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Compose(test.mutate(base)); err == nil {
				t.Fatal("incomplete composition was accepted")
			}
		})
	}
}

func TestComposeRoutesNewResourcesByTypeAndKeepsPersistedResolutionExact(t *testing.T) {
	store := appfake.NewStore()
	routedType := provisioningfake.ResourceType()
	fallbackType := domain.ResourceTypeRef{Name: "FallbackResource", Version: "v1"}
	fallbackDomain, err := domain.NewResourceType(fallbackType, "fallback runtime test type", []domain.Capability{domain.CapabilityCreate})
	if err != nil {
		t.Fatal(err)
	}
	catalog := runtimeCatalog(t)
	catalog.Types[fallbackType] = fallbackDomain
	defaultRef := application.ProvisionerRef("pulumi-default")
	routedRef := application.ProvisionerRef("opentofu-private")
	defaultProvider := &capabilityProvisioner{capabilities: []provisioning.ProvisionerCapability{{ResourceType: fallbackType, Capability: domain.CapabilityCreate}}}
	routedProvider := &capabilityProvisioner{capabilities: []provisioning.ProvisionerCapability{
		{ResourceType: routedType, Capability: domain.CapabilityCreate},
		{ResourceType: routedType, Capability: domain.CapabilityUpdate},
		{ResourceType: routedType, Capability: domain.CapabilityDelete},
	}}
	composed, err := Compose(Config{
		Transactions: store, Catalog: catalog,
		Provisioners: map[application.ProvisionerRef]provisioning.Provisioner{
			defaultRef: defaultProvider,
			routedRef:  routedProvider,
		},
		DefaultProvisionerRef: defaultRef,
		ResourceTypeProvisionerRefs: map[domain.ResourceTypeRef]application.ProvisionerRef{
			routedType: routedRef,
		},
		InsecureAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for index, resourceType := range []domain.ResourceTypeRef{routedType, fallbackType} {
		spec, specErr := domain.NewResourceSpec(map[string]any{"index": int64(index)})
		if specErr != nil {
			t.Fatal(specErr)
		}
		suffix := strconv.Itoa(index + 1)
		id := domain.ResourceID("routed-resource-" + suffix)
		_, createErr := composed.Service().CreateResource(context.Background(), application.CreateResourceCommand{
			Actor: appfake.Principal("tester"), ID: id, Type: resourceType,
			Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
			OperationID: domain.OperationID("operation-" + suffix),
			EventID:     domain.EventID("event-" + suffix), RequestedAt: time.Now().UTC(),
			IdempotencyKey: "routing-" + suffix,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		want := defaultRef
		if resourceType == routedType {
			want = routedRef
		}
		if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
			record, getErr := tx.Resources().GetResource(context.Background(), id)
			if getErr == nil && record.ProvisionerRef != want {
				t.Fatalf("resource %s provisioner = %q, want %q", id, record.ProvisionerRef, want)
			}
			return getErr
		}); err != nil {
			t.Fatal(err)
		}
	}

	resolver := staticResolver{providers: map[application.ProvisionerRef]provisioning.Provisioner{
		defaultRef: defaultProvider,
		routedRef:  routedProvider,
	}}
	resolved, err := resolver.Resolve(context.Background(), routedRef)
	if err != nil || resolved != routedProvider {
		t.Fatalf("persisted routed ref resolution = %T, %v", resolved, err)
	}
}

func TestComposeRejectsInvalidPrivateRoutes(t *testing.T) {
	resourceType := provisioningfake.ResourceType()
	defaultRef := application.ProvisionerRef("default")
	routedRef := application.ProvisionerRef("routed")
	base := Config{
		Transactions: appfake.NewStore(), Catalog: runtimeCatalog(t),
		Provisioners: map[application.ProvisionerRef]provisioning.Provisioner{
			defaultRef: provisioningfake.New(provisioningfake.ModeSynchronous),
			routedRef: &capabilityProvisioner{capabilities: []provisioning.ProvisionerCapability{
				{ResourceType: resourceType, Capability: domain.CapabilityCreate},
			}},
		},
		DefaultProvisionerRef:       defaultRef,
		ResourceTypeProvisionerRefs: map[domain.ResourceTypeRef]application.ProvisionerRef{resourceType: routedRef},
		InsecureAuth:                true,
	}
	tests := []struct {
		name   string
		mutate func(Config) Config
	}{
		{name: "unregistered ref", mutate: func(config Config) Config {
			config.ResourceTypeProvisionerRefs = map[domain.ResourceTypeRef]application.ProvisionerRef{resourceType: "missing"}
			return config
		}},
		{name: "unregistered resource type", mutate: func(config Config) Config {
			config.ResourceTypeProvisionerRefs = map[domain.ResourceTypeRef]application.ProvisionerRef{{Name: "Missing", Version: "v1"}: routedRef}
			return config
		}},
		{name: "missing create declaration", mutate: func(config Config) Config {
			config.Provisioners = cloneProvisioners(config.Provisioners)
			config.Provisioners[routedRef] = &capabilityProvisioner{capabilities: []provisioning.ProvisionerCapability{{ResourceType: resourceType, Capability: domain.CapabilityUpdate}}}
			return config
		}},
		{name: "capability outside contract", mutate: func(config Config) Config {
			config.Provisioners = cloneProvisioners(config.Provisioners)
			config.Provisioners[routedRef] = &capabilityProvisioner{capabilities: []provisioning.ProvisionerCapability{
				{ResourceType: resourceType, Capability: domain.CapabilityCreate},
				{ResourceType: resourceType, Capability: domain.Capability("rotate")},
			}}
			return config
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Compose(test.mutate(base)); err == nil {
				t.Fatal("invalid private route was accepted")
			}
		})
	}
}

type capabilityProvisioner struct {
	capabilities []provisioning.ProvisionerCapability
}

func (p *capabilityProvisioner) Capabilities() []provisioning.ProvisionerCapability {
	return append([]provisioning.ProvisionerCapability(nil), p.capabilities...)
}

func (*capabilityProvisioner) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{}, nil
}

func (*capabilityProvisioner) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, nil
}

func cloneProvisioners(source map[application.ProvisionerRef]provisioning.Provisioner) map[application.ProvisionerRef]provisioning.Provisioner {
	cloned := make(map[application.ProvisionerRef]provisioning.Provisioner, len(source))
	for ref, provider := range source {
		cloned[ref] = provider
	}
	return cloned
}
