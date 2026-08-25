// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/policy"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func TestPostgresOwnerQuotaSerializesConcurrentCreates(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	compiled, err := policy.Parse([]byte(`{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"one-per-owner","kind":"resource-count-quota","limit":1}]
	}`), []domain.ResourceTypeRef{provisioningfake.ResourceType()})
	if err != nil {
		t.Fatal(err)
	}
	service.AdmissionPolicy = compiled

	commands := []application.CreateResourceCommand{
		postgresCreateCommand(t, "quota-concurrent-a", "quota-concurrent-operation-a", map[string]any{"n": int64(1)}),
		postgresCreateCommand(t, "quota-concurrent-b", "quota-concurrent-operation-b", map[string]any{"n": int64(2)}),
	}
	commands[0].IdempotencyKey = "quota-concurrent-key-a"
	commands[1].IdempotencyKey = "quota-concurrent-key-b"
	results := make(chan error, len(commands))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, command := range commands {
		command := command
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, admitErr := service.AdmitCreateResource(context.Background(), command)
			results <- admitErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var admitted, denied int
	for result := range results {
		switch {
		case result == nil:
			admitted++
		case errors.Is(result, application.ErrQuotaExceeded):
			denied++
		default:
			t.Fatalf("unexpected concurrent result: %v", result)
		}
	}
	if admitted != 1 || denied != 1 {
		t.Fatalf("admitted=%d denied=%d", admitted, denied)
	}
	var resources, idempotency, provenance int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM resources`).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_records`).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE data->'admission'->>'policyRevision'=$1`, compiled.Revision()).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if resources != 1 || idempotency != 1 || provenance != 1 {
		t.Fatalf("resources=%d idempotency=%d provenance=%d", resources, idempotency, provenance)
	}
}

func TestPostgresQuotaFactsFailClosedForMissingStatus(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO resources
		(id,type_name,type_version,owner_kind,owner_id,generation,spec_codec_version,spec,record_version,created_at_ns,updated_at_ns)
		VALUES ('quota-corrupt','FakeResource','v1','team','platform',1,1,'{}',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	compiled, err := policy.Parse([]byte(`{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"bounded-owner","kind":"resource-count-quota","limit":100}]
	}`), []domain.ResourceTypeRef{provisioningfake.ResourceType()})
	if err != nil {
		t.Fatal(err)
	}
	service.AdmissionPolicy = compiled
	command := postgresCreateCommand(t, "quota-after-corruption", "quota-after-corruption-operation", map[string]any{"n": int64(1)})
	command.IdempotencyKey = "quota-after-corruption-key"
	if _, err := service.AdmitCreateResource(ctx, command); !errors.Is(err, application.ErrQuotaInvariant) {
		t.Fatalf("corrupt quota admission error = %v", err)
	}
	var resources, idempotency int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM resources`).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_records`).Scan(&idempotency); err != nil {
		t.Fatal(err)
	}
	if resources != 1 || idempotency != 0 {
		t.Fatalf("resources=%d idempotency=%d", resources, idempotency)
	}
}
