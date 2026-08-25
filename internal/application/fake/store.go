// SPDX-License-Identifier: Apache-2.0

// Package fake provides deterministic application-layer test infrastructure.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// ErrNotFound reports a missing record from non-repository fakes (catalog,
// resolver). Repository fakes return the application sentinel errors that the
// PostgreSQL store surfaces.
var ErrNotFound = errors.New("fake record not found")

type Store struct {
	mu        sync.Mutex
	resources map[domain.ResourceID]application.ResourceRecord
	// resourceSequences mirrors PostgreSQL's private immutable insertion
	// sequence so fake and durable stores order inventory identically.
	resourceSequences     map[domain.ResourceID]uint64
	nextResourceSequence  uint64
	operations            map[domain.OperationID]application.OperationRecord
	nextOperationSequence uint64
	events                map[domain.EventID]domain.Event
	executions            map[domain.OperationID]application.ProvisioningExecutionRecord
	idempotency           map[string]application.IdempotencyRecord
	attempts              map[string]application.SubmissionAttemptRecord
	outbox                map[string]application.OutboxMessage
	outputs               map[domain.ResourceID]map[uint64]application.ResourceOutputRecord
}

func NewStore() *Store {
	return &Store{
		resources:         make(map[domain.ResourceID]application.ResourceRecord),
		resourceSequences: make(map[domain.ResourceID]uint64),
		operations:        make(map[domain.OperationID]application.OperationRecord),
		events:            make(map[domain.EventID]domain.Event),
		executions:        make(map[domain.OperationID]application.ProvisioningExecutionRecord),
		idempotency:       make(map[string]application.IdempotencyRecord),
		attempts:          make(map[string]application.SubmissionAttemptRecord),
		outbox:            make(map[string]application.OutboxMessage),
		outputs:           make(map[domain.ResourceID]map[uint64]application.ResourceOutputRecord),
	}
}

func (s *Store) Within(_ context.Context, fn func(application.UnitOfWork) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Store{
		resources:             cloneMap(s.resources),
		resourceSequences:     cloneMap(s.resourceSequences),
		nextResourceSequence:  s.nextResourceSequence,
		operations:            cloneMap(s.operations),
		nextOperationSequence: s.nextOperationSequence,
		events:                cloneEvents(s.events),
		executions:            cloneExecutions(s.executions),
		idempotency:           cloneMap(s.idempotency),
		attempts:              cloneMap(s.attempts),
		outbox:                cloneMap(s.outbox),
		outputs:               cloneOutputs(s.outputs),
	}
	if err := fn(tx); err != nil {
		// PostgreSQL identity sequences are non-transactional: allocated values
		// remain consumed when the surrounding transaction rolls back.
		s.nextOperationSequence = tx.nextOperationSequence
		s.nextResourceSequence = tx.nextResourceSequence
		return err
	}
	s.resources = tx.resources
	s.resourceSequences = tx.resourceSequences
	s.nextResourceSequence = tx.nextResourceSequence
	s.operations = tx.operations
	s.nextOperationSequence = tx.nextOperationSequence
	s.events = tx.events
	s.executions = tx.executions
	s.idempotency = tx.idempotency
	s.attempts = tx.attempts
	s.outbox = tx.outbox
	s.outputs = tx.outputs
	return nil
}

func cloneOutputs(source map[domain.ResourceID]map[uint64]application.ResourceOutputRecord) map[domain.ResourceID]map[uint64]application.ResourceOutputRecord {
	cloned := make(map[domain.ResourceID]map[uint64]application.ResourceOutputRecord, len(source))
	for resourceID, generations := range source {
		inner := make(map[uint64]application.ResourceOutputRecord, len(generations))
		for generation, record := range generations {
			inner[generation] = record
		}
		cloned[resourceID] = inner
	}
	return cloned
}

func cloneExecutions(source map[domain.OperationID]application.ProvisioningExecutionRecord) map[domain.OperationID]application.ProvisioningExecutionRecord {
	cloned := make(map[domain.OperationID]application.ProvisioningExecutionRecord, len(source))
	for key, value := range source {
		cloned[key] = cloneExecution(value)
	}
	return cloned
}

func cloneExecution(value application.ProvisioningExecutionRecord) application.ProvisioningExecutionRecord {
	if value.Handle != nil {
		handle := *value.Handle
		value.Handle = &handle
	}
	if value.Submission != nil {
		submission := cloneSubmission(*value.Submission)
		value.Submission = &submission
	}
	if value.LastObservation != nil {
		observation := cloneObservation(*value.LastObservation)
		value.LastObservation = &observation
	}
	if value.LastFailure != nil {
		failure := *value.LastFailure
		value.LastFailure = &failure
	}
	return value
}

func cloneSubmission(submission provisioning.Submission) provisioning.Submission {
	submission.Observation = cloneObservation(submission.Observation)
	return submission
}

func cloneObservation(observation provisioning.ExecutionObservation) provisioning.ExecutionObservation {
	if observation.Execution != nil {
		execution := *observation.Execution
		if execution.Handle != nil {
			handle := *execution.Handle
			execution.Handle = &handle
		}
		if execution.Failure != nil {
			failure := *execution.Failure
			execution.Failure = &failure
		}
		observation.Execution = &execution
	}
	return observation
}

// cloneEvents snapshots event maps with their actor payloads so transaction
// isolation matches PostgreSQL row semantics.
func cloneEvents(source map[domain.EventID]domain.Event) map[domain.EventID]domain.Event {
	cloned := make(map[domain.EventID]domain.Event, len(source))
	for key, event := range source {
		if actor, present := event.Actor(); present {
			event, _ = event.WithActor(actor)
		}
		if admission, present := event.Admission(); present {
			event, _ = event.WithAdmissionPolicyRevision(admission.PolicyRevision)
		}
		cloned[key] = event
	}
	return cloned
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	cloned := make(map[K]V, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// RecordCounts reports how many records of each kind are stored. It exists
// for tests that assert an admission produced no durable side effects.
type RecordCounts struct {
	Resources       int
	Operations      int
	Events          int
	Executions      int
	Idempotency     int
	Attempts        int
	Outbox          int
	OutputSnapshots int
}

func (s *Store) RecordCounts() RecordCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshots := 0
	for _, generations := range s.outputs {
		snapshots += len(generations)
	}
	return RecordCounts{
		Resources:       len(s.resources),
		Operations:      len(s.operations),
		Events:          len(s.events),
		Executions:      len(s.executions),
		Idempotency:     len(s.idempotency),
		Attempts:        len(s.attempts),
		Outbox:          len(s.outbox),
		OutputSnapshots: snapshots,
	}
}

func (s *Store) Resources() application.ResourceRepository                   { return s }
func (s *Store) Operations() application.OperationRepository                 { return s }
func (s *Store) Events() application.EventRepository                         { return s }
func (s *Store) Executions() application.ExecutionRepository                 { return s }
func (s *Store) Idempotency() application.IdempotencyRepository              { return s }
func (s *Store) SubmissionAttempts() application.SubmissionAttemptRepository { return s }
func (s *Store) Outbox() application.OutboxRepository                        { return s }
func (s *Store) Outputs() application.ResourceOutputRepository               { return s }
func (s *Store) Quotas() application.QuotaRepository                         { return s }

// SaveResourceOutputs is idempotent only for identical provenance and
// content; contradictory evidence for the same resource/generation pair
// fails closed, mirroring the PostgreSQL implementation.
func (s *Store) SaveResourceOutputs(_ context.Context, record application.ResourceOutputRecord) error {
	generations, ok := s.outputs[record.ResourceID]
	if !ok {
		generations = make(map[uint64]application.ResourceOutputRecord)
		s.outputs[record.ResourceID] = generations
	}
	existing, ok := generations[record.ObservedGeneration]
	if ok {
		if existing.OperationID != record.OperationID ||
			existing.Capability != record.Capability ||
			existing.OutputMappingRef != record.OutputMappingRef ||
			existing.OutputContractDigest != record.OutputContractDigest ||
			existing.ValuesDigest != record.ValuesDigest {
			return fmt.Errorf("%w: contradictory output evidence for generation %d", application.ErrInvalidApplicationCall, record.ObservedGeneration)
		}
		return nil
	}
	generations[record.ObservedGeneration] = record
	return nil
}

func (s *Store) LatestResourceOutputs(_ context.Context, id domain.ResourceID) (application.ResourceOutputRecord, bool, error) {
	generations, ok := s.outputs[id]
	if !ok || len(generations) == 0 {
		return application.ResourceOutputRecord{}, false, nil
	}
	latest := uint64(0)
	for generation := range generations {
		if generation > latest {
			latest = generation
		}
	}
	record := generations[latest]
	return record, true, nil
}

func (s *Store) GetResource(_ context.Context, id domain.ResourceID) (application.ResourceRecord, error) {
	record, ok := s.resources[id]
	if !ok {
		return application.ResourceRecord{}, application.ErrResourceNotFound
	}
	return record, nil
}

func (s *Store) LookupResource(ctx context.Context, id domain.ResourceID) (application.ResourceRecord, error) {
	return s.GetResource(ctx, id)
}

func (s *Store) LockResourceID(_ context.Context, id domain.ResourceID) (bool, error) {
	_, found := s.resources[id]
	return found, nil
}

func (s *Store) LockOwnerQuota(context.Context, domain.OwnerRef) error { return nil }

func (s *Store) ResourceCountFacts(_ context.Context, owner domain.OwnerRef, ref domain.ResourceTypeRef) (application.ResourceCountFacts, error) {
	facts := application.ResourceCountFacts{Available: true, Owner: owner, ResourceType: ref}
	for _, record := range s.resources {
		if record.Resource.Owner() != owner {
			continue
		}
		state := record.Status.State()
		if state == "" {
			return application.ResourceCountFacts{}, application.ErrQuotaInvariant
		}
		if state == domain.ResourceStateDeleted {
			continue
		}
		facts.OwnerNonDeleted++
		if record.Resource.Type() == ref {
			facts.TypeNonDeleted++
		}
	}
	return facts, nil
}

func (s *Store) CreateResource(_ context.Context, record application.ResourceRecord) error {
	if _, exists := s.resources[record.Resource.ID()]; exists {
		return fmt.Errorf("%w: resource already exists", application.ErrConcurrencyConflict)
	}
	if record.Version == 0 {
		record.Version = 1
	}
	// The private insertion sequence is allocated once at creation and never
	// changes afterwards, mirroring the durable store's identity column.
	s.nextResourceSequence++
	s.resourceSequences[record.Resource.ID()] = s.nextResourceSequence
	s.resources[record.Resource.ID()] = record
	return nil
}

// SaveResource mirrors the durable store's immutability triggers: ownership
// is fixed at creation and no application flow may change it (ADR-0016).
func (s *Store) SaveResource(_ context.Context, record application.ResourceRecord, expectedVersion uint64) error {
	current, ok := s.resources[record.Resource.ID()]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if current.Version != expectedVersion {
		return application.ErrConcurrencyConflict
	}
	if current.Resource.Owner() != record.Resource.Owner() {
		return fmt.Errorf("%w: resource owner is immutable", application.ErrInvalidApplicationCall)
	}
	record.Version = expectedVersion + 1
	s.resources[record.Resource.ID()] = record
	return nil
}

// ListResources executes the trusted query mechanically: owner-scope filter,
// narrowing filters, Deleted policy, and the same descending keyset order as
// the durable store, so both implementations produce identical pages.
func (s *Store) ListResources(_ context.Context, query application.ResourceListQuery) (application.ResourceInventoryPage, error) {
	if query.Limit <= 0 {
		return application.ResourceInventoryPage{}, fmt.Errorf("%w: resource page limit must be greater than zero", application.ErrInvalidApplicationCall)
	}
	if !query.Unrestricted && len(query.AllowedOwners) == 0 {
		return application.ResourceInventoryPage{Items: []application.ResourceInventoryItem{}}, nil
	}
	allowed := make(map[domain.OwnerRef]struct{}, len(query.AllowedOwners))
	for _, owner := range query.AllowedOwners {
		allowed[owner] = struct{}{}
	}
	type row struct {
		item     application.ResourceInventoryItem
		sequence uint64
	}
	matches := make([]row, 0)
	for id, record := range s.resources {
		resource := record.Resource
		owner := resource.Owner()
		if !query.Unrestricted {
			if _, ok := allowed[owner]; !ok {
				continue
			}
		}
		if query.OwnerFilter != nil && owner != *query.OwnerFilter {
			continue
		}
		if query.TypeName != "" && resource.Type().Name != query.TypeName {
			continue
		}
		if query.TypeVersion != "" && resource.Type().Version != query.TypeVersion {
			continue
		}
		state := record.Status.State()
		if state == domain.ResourceStateDeleted && !query.IncludeDeleted {
			continue
		}
		if query.StateFilter != nil && state != *query.StateFilter {
			continue
		}
		sequence := s.resourceSequences[id]
		if query.AfterSequence != 0 && sequence >= query.AfterSequence {
			continue
		}
		item := application.ResourceInventoryItem{
			ID:         resource.ID(),
			Type:       resource.Type(),
			Owner:      owner,
			Generation: resource.Generation(),
			CreatedAt:  resource.CreatedAt(),
			UpdatedAt:  resource.UpdatedAt(),
			Status: application.ResourceInventoryStatus{
				State:              state,
				ObservedGeneration: record.Status.ObservedGeneration(),
				UpdatedAt:          record.Status.UpdatedAt(),
			},
			Sequence: sequence,
		}
		if latest, found := s.latestOperationOf(id); found {
			capability := latest.Capability()
			state := latest.State()
			targetGeneration := latest.TargetGeneration()
			item.Latest = &application.ResourceInventoryLatestOperation{
				ID:               latest.ID(),
				Capability:       capability,
				State:            state,
				TargetGeneration: targetGeneration,
			}
		}
		matches = append(matches, row{item: item, sequence: sequence})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].sequence > matches[j].sequence })
	page := application.ResourceInventoryPage{Items: []application.ResourceInventoryItem{}}
	for _, candidate := range matches {
		if len(page.Items) == query.Limit {
			page.NextSequence = page.Items[len(page.Items)-1].Sequence
			break
		}
		page.Items = append(page.Items, candidate.item)
	}
	return page, nil
}

// latestOperationOf selects the newest inserted Operation for a Resource by
// insertion sequence, mirroring LatestForResource.
func (s *Store) latestOperationOf(id domain.ResourceID) (domain.Operation, bool) {
	var latest domain.Operation
	found := false
	for _, record := range s.operations {
		if record.Operation.ResourceID() != id {
			continue
		}
		if !found || record.Sequence > s.operationSequenceOf(latest) {
			latest = record.Operation
			found = true
		}
	}
	return latest, found
}

func (s *Store) operationSequenceOf(operation domain.Operation) uint64 {
	record, ok := s.operations[operation.ID()]
	if !ok {
		return 0
	}
	return record.Sequence
}

func (s *Store) GetOperation(_ context.Context, id domain.OperationID) (application.OperationRecord, error) {
	record, ok := s.operations[id]
	if !ok {
		return application.OperationRecord{}, application.ErrOperationNotFound
	}
	return record, nil
}

func (s *Store) LookupOperation(ctx context.Context, id domain.OperationID) (application.OperationRecord, error) {
	return s.GetOperation(ctx, id)
}

func (s *Store) ActiveForResource(_ context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	for _, record := range s.operations {
		if record.Operation.ResourceID() == id && !record.Operation.IsTerminal() {
			return record, true, nil
		}
	}
	return application.OperationRecord{}, false, nil
}

func (s *Store) LatestForResource(_ context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	var latest application.OperationRecord
	found := false
	for _, record := range s.operations {
		if record.Operation.ResourceID() != id {
			continue
		}
		if !found || record.Sequence > latest.Sequence {
			latest = record
			found = true
		}
	}
	return latest, found, nil
}

func (s *Store) PageForResource(_ context.Context, id domain.ResourceID, beforeSequence uint64, limit int) (application.OperationPage, error) {
	if limit <= 0 {
		return application.OperationPage{}, fmt.Errorf("%w: operation page limit must be greater than zero", application.ErrInvalidApplicationCall)
	}
	records := make([]application.OperationRecord, 0)
	for _, record := range s.operations {
		if record.Operation.ResourceID() == id && (beforeSequence == 0 || record.Sequence < beforeSequence) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence > records[j].Sequence })
	page := application.OperationPage{}
	if len(records) > limit {
		page.Records = records[:limit]
		page.NextSequence = page.Records[len(page.Records)-1].Sequence
	} else {
		page.Records = records
	}
	return page, nil
}

func (s *Store) CreateOperation(_ context.Context, record application.OperationRecord) error {
	s.nextOperationSequence++
	record.Sequence = s.nextOperationSequence
	if _, exists := s.operations[record.Operation.ID()]; exists {
		return fmt.Errorf("operation already exists")
	}
	if record.Version == 0 {
		record.Version = 1
	}
	if retryOf := record.Operation.RetryOfOperationID(); retryOf != "" {
		source, exists := s.operations[retryOf]
		if !exists || source.Operation.State() != domain.OperationStateFailed ||
			source.Operation.ResourceID() != record.Operation.ResourceID() ||
			source.Operation.Capability() != record.Operation.Capability() ||
			source.Operation.TargetGeneration() != record.Operation.TargetGeneration() {
			return fmt.Errorf("%w: retry source must be a failed operation with matching intent", application.ErrInvalidApplicationCall)
		}
	}
	s.operations[record.Operation.ID()] = record
	return nil
}

func (s *Store) SaveOperation(_ context.Context, record application.OperationRecord, expectedVersion uint64) error {
	current, ok := s.operations[record.Operation.ID()]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if current.Version != expectedVersion {
		return application.ErrConcurrencyConflict
	}
	if current.Operation.IsTerminal() {
		return fmt.Errorf("%w: terminal operation is immutable", application.ErrInvalidApplicationCall)
	}
	if current.Operation.ResourceID() != record.Operation.ResourceID() ||
		current.Operation.Capability() != record.Operation.Capability() ||
		current.Operation.TargetGeneration() != record.Operation.TargetGeneration() ||
		current.Operation.RequestedAt().UnixNano() != record.Operation.RequestedAt().UnixNano() ||
		current.Operation.RetryOfOperationID() != record.Operation.RetryOfOperationID() {
		return fmt.Errorf("%w: operation intent is immutable", application.ErrInvalidApplicationCall)
	}
	record.Sequence = current.Sequence
	record.Version = expectedVersion + 1
	s.operations[record.Operation.ID()] = record
	return nil
}

func (s *Store) Append(_ context.Context, event domain.Event) error {
	if _, exists := s.events[event.ID()]; exists {
		return fmt.Errorf("event already exists")
	}
	s.events[event.ID()] = event
	return nil
}

func (s *Store) GetEvent(_ context.Context, id domain.EventID) (domain.Event, error) {
	event, ok := s.events[id]
	if !ok {
		return domain.Event{}, ErrNotFound
	}
	return event, nil
}

func (s *Store) GetExecution(_ context.Context, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	record, ok := s.executions[id]
	if !ok {
		return application.ProvisioningExecutionRecord{}, application.ErrResourceNotFound
	}
	return cloneExecution(record), nil
}

func (s *Store) LookupExecution(ctx context.Context, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	return s.GetExecution(ctx, id)
}

func (s *Store) CreateExecution(_ context.Context, record application.ProvisioningExecutionRecord) error {
	if _, exists := s.executions[record.OperationID]; exists {
		return fmt.Errorf("execution already exists")
	}
	if record.Version == 0 {
		record.Version = 1
	}
	if (record.RecoverySourceOperationID == "") != (record.RecoverySourceAttempt == 0) || record.RecoverySourceOperationID == record.OperationID {
		return fmt.Errorf("%w: invalid output recovery provenance", application.ErrInvalidApplicationCall)
	}
	if record.IsOutputRecovery() {
		if _, exists := s.attempts[attemptKey(record.RecoverySourceOperationID, record.RecoverySourceAttempt)]; !exists {
			return fmt.Errorf("%w: output recovery source attempt does not exist", application.ErrInvalidApplicationCall)
		}
	}
	s.executions[record.OperationID] = cloneExecution(record)
	return nil
}

func (s *Store) SaveExecution(_ context.Context, record application.ProvisioningExecutionRecord, expectedVersion uint64) error {
	current, ok := s.executions[record.OperationID]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if current.Version != expectedVersion {
		return application.ErrConcurrencyConflict
	}
	for _, child := range s.executions {
		if child.RecoverySourceOperationID == record.OperationID {
			return fmt.Errorf("%w: referenced recovery source execution is immutable", application.ErrInvalidApplicationCall)
		}
	}
	if current.RecoverySourceOperationID != record.RecoverySourceOperationID || current.RecoverySourceAttempt != record.RecoverySourceAttempt {
		return fmt.Errorf("%w: output recovery provenance is immutable", application.ErrInvalidApplicationCall)
	}
	if current.OutputMappingRef != "" && current.OutputMappingRef != record.OutputMappingRef {
		return application.ErrConcurrencyConflict
	}
	if current.OutputMappingRef != "" {
		record.OutputMappingRef = current.OutputMappingRef
	}
	record.Version = expectedVersion + 1
	s.executions[record.OperationID] = cloneExecution(record)
	return nil
}

func (s *Store) GetIdempotency(_ context.Context, scope, key string) (application.IdempotencyRecord, error) {
	record, ok := s.idempotency[idempotencyMapKey(scope, key)]
	if !ok {
		return application.IdempotencyRecord{}, application.ErrIdempotencyNotFound
	}
	return cloneIdempotency(record), nil
}

func (s *Store) PutIdempotency(_ context.Context, record application.IdempotencyRecord) error {
	mapKey := idempotencyMapKey(record.Scope, record.Key)
	if _, exists := s.idempotency[mapKey]; exists {
		return fmt.Errorf("idempotency key already exists")
	}
	s.idempotency[mapKey] = cloneIdempotency(record)
	return nil
}

// idempotencyMapKey namespaces the key by its scope so distinct principals
// never share an idempotency namespace.
func idempotencyMapKey(scope, key string) string {
	return scope + "\x00" + key
}

func cloneIdempotency(record application.IdempotencyRecord) application.IdempotencyRecord {
	cloned := record
	return cloned
}

func attemptKey(operationID domain.OperationID, attempt uint64) string {
	return fmt.Sprintf("%s:%d", operationID, attempt)
}

func (s *Store) GetSubmissionAttempt(_ context.Context, operationID domain.OperationID, attempt uint64) (application.SubmissionAttemptRecord, error) {
	record, ok := s.attempts[attemptKey(operationID, attempt)]
	if !ok {
		return application.SubmissionAttemptRecord{}, application.ErrResourceNotFound
	}
	return record, nil
}

func (s *Store) CreateSubmissionAttempt(_ context.Context, record application.SubmissionAttemptRecord) error {
	key := attemptKey(record.OperationID, record.AttemptNumber)
	if _, exists := s.attempts[key]; exists {
		return fmt.Errorf("submission attempt already exists")
	}
	s.attempts[key] = record
	return nil
}

func (s *Store) SaveSubmissionAttempt(_ context.Context, record application.SubmissionAttemptRecord, expected application.SubmissionAttemptState) error {
	key := attemptKey(record.OperationID, record.AttemptNumber)
	current, exists := s.attempts[key]
	if !exists {
		return application.ErrConcurrencyConflict
	}
	if current.State != expected {
		return application.ErrConcurrencyConflict
	}
	for _, child := range s.executions {
		if child.RecoverySourceOperationID == record.OperationID && child.RecoverySourceAttempt == record.AttemptNumber {
			return fmt.Errorf("%w: referenced recovery source submission attempt is immutable", application.ErrInvalidApplicationCall)
		}
	}
	s.attempts[key] = record
	return nil
}

func (s *Store) Enqueue(_ context.Context, message application.OutboxMessage) error {
	if _, exists := s.outbox[message.DedupeKey]; exists {
		return nil
	}
	if message.State == "" {
		message.State = application.OutboxPending
	}
	if message.AvailableAt.IsZero() {
		message.AvailableAt = time.Now().Add(message.Delay)
	}
	s.outbox[message.ID] = message
	return nil
}

func (s *Store) GetOutbox(_ context.Context, id string) (application.OutboxMessage, error) {
	message, ok := s.outbox[id]
	if !ok {
		return application.OutboxMessage{}, application.ErrResourceNotFound
	}
	return message, nil
}

func (s *Store) ClaimOutbox(_ context.Context, token string, lease time.Duration) (application.OutboxMessage, bool, error) {
	now := time.Now()
	for id, message := range s.outbox {
		if message.State != application.OutboxPending || (!message.AvailableAt.IsZero() && message.AvailableAt.After(now)) {
			continue
		}
		message.State = application.OutboxLeased
		message.LeaseToken = token
		message.LeasedUntil = now.Add(lease)
		message.AttemptCount++
		s.outbox[id] = message
		return message, true, nil
	}
	return application.OutboxMessage{}, false, nil
}

func (s *Store) FindExpiredDispatch(_ context.Context) (application.OutboxMessage, bool, error) {
	now := time.Now()
	for _, message := range s.outbox {
		if message.Kind == application.OutboxDispatch && message.State == application.OutboxLeased && !message.LeasedUntil.After(now) {
			return message, true, nil
		}
	}
	return application.OutboxMessage{}, false, nil
}

func (s *Store) RenewOutbox(_ context.Context, id, token string, lease time.Duration) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	now := time.Now()
	if message.State != application.OutboxLeased || message.LeaseToken != token || !message.LeasedUntil.After(now) {
		return application.ErrConcurrencyConflict
	}
	message.LeasedUntil = now.Add(lease)
	s.outbox[id] = message
	return nil
}

func (s *Store) RequeueExpiredOutbox(_ context.Context, id, token string) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if message.State != application.OutboxLeased || message.LeaseToken != token || message.LeasedUntil.After(time.Now()) {
		return application.ErrConcurrencyConflict
	}
	message.State = application.OutboxPending
	message.LeaseToken = ""
	message.LeasedUntil = time.Time{}
	message.AvailableAt = time.Now()
	s.outbox[id] = message
	return nil
}

func (s *Store) CompleteOutbox(_ context.Context, id, token, reason string) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if message.State != application.OutboxLeased || message.LeaseToken != token || !message.LeasedUntil.After(time.Now()) {
		return application.ErrConcurrencyConflict
	}
	message.State = application.OutboxCompleted
	message.LeaseToken = ""
	message.LeasedUntil = time.Time{}
	message.TerminalReason = reason
	s.outbox[id] = message
	return nil
}

func (s *Store) CompleteExpiredOutbox(_ context.Context, id, token, reason string) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if message.State != application.OutboxLeased || message.LeaseToken != token || message.LeasedUntil.After(time.Now()) {
		return application.ErrConcurrencyConflict
	}
	message.State = application.OutboxCompleted
	message.LeaseToken = ""
	message.LeasedUntil = time.Time{}
	message.TerminalReason = reason
	s.outbox[id] = message
	return nil
}

func (s *Store) RetryOutbox(_ context.Context, id, token string, delay time.Duration, messageText string) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if message.State != application.OutboxLeased || message.LeaseToken != token || !message.LeasedUntil.After(time.Now()) {
		return application.ErrConcurrencyConflict
	}
	message.LeaseToken = ""
	message.LeasedUntil = time.Time{}
	message.LastError = messageText
	message.State = application.OutboxPending
	message.AvailableAt = time.Now().Add(delay)
	s.outbox[id] = message
	return nil
}

func (s *Store) DeadOutbox(_ context.Context, id, token, reason string) error {
	message, ok := s.outbox[id]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if message.State != application.OutboxLeased || message.LeaseToken != token || !message.LeasedUntil.After(time.Now()) {
		return application.ErrConcurrencyConflict
	}
	message.State = application.OutboxDead
	message.LeaseToken = ""
	message.LeasedUntil = time.Time{}
	message.TerminalReason = reason
	s.outbox[id] = message
	return nil
}

// Catalog is a deterministic in-memory ResourceTypeCatalog fake. Types holds
// bare domain types; they are adapted to application.ResourceContract with a
// permissive ValidateSpec so existing tests are unaffected by contract
// validation. Set ValidateFunc to make admission reject specific specs.
type Catalog struct {
	Types        map[domain.ResourceTypeRef]domain.ResourceType
	ValidateFunc func(ref domain.ResourceTypeRef, spec domain.ResourceSpec) error
}

func (c Catalog) Get(_ context.Context, ref domain.ResourceTypeRef) (application.ResourceContract, error) {
	typeValue, ok := c.Types[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return basicContract{resourceType: typeValue, validate: c.ValidateFunc}, nil
}

func (c Catalog) List(_ context.Context) ([]application.ResourceContract, error) {
	refs := make([]domain.ResourceTypeRef, 0, len(c.Types))
	for ref := range c.Types {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Version < refs[j].Version
	})
	contracts := make([]application.ResourceContract, 0, len(refs))
	for _, ref := range refs {
		contracts = append(contracts, basicContract{resourceType: c.Types[ref], validate: c.ValidateFunc})
	}
	return contracts, nil
}

// basicContract adapts a bare domain.ResourceType to the application contract.
type basicContract struct {
	resourceType domain.ResourceType
	validate     func(ref domain.ResourceTypeRef, spec domain.ResourceSpec) error
}

func (b basicContract) Ref() domain.ResourceTypeRef       { return b.resourceType.Ref() }
func (b basicContract) DisplayName() string               { return b.resourceType.Ref().Name }
func (b basicContract) Description() string               { return b.resourceType.Description() }
func (b basicContract) Capabilities() []domain.Capability { return b.resourceType.Capabilities() }
func (b basicContract) Domain() domain.ResourceType       { return b.resourceType }

// ValidateSpec is permissive by default: bare fakes carry no schema.
func (b basicContract) ValidateSpec(spec domain.ResourceSpec) error {
	if b.validate == nil {
		return nil
	}
	return b.validate(b.resourceType.Ref(), spec)
}

// ValidateUpdate is permissive by default: bare fakes declare no transition
// rules. Tests that need rejecting contracts wrap their own contract type or
// use the catalog ValidateFunc for spec-level rejection.
func (b basicContract) ValidateUpdate(_, _ domain.ResourceSpec) error { return nil }

// OutputContract is nil by default: bare fakes publish no outputs.
func (b basicContract) OutputContract() *resourcecontract.OutputContract { return nil }

func (b basicContract) SpecSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

type Selector struct {
	mu    sync.Mutex
	Ref   application.ProvisionerRef
	Calls int
	Err   error
}

func (s *Selector) Select(_ context.Context, _ domain.ResourceTypeRef, _ domain.Capability) (application.ProvisionerRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if s.Err != nil {
		return "", s.Err
	}
	return s.Ref, nil
}

type Resolver struct {
	mu        sync.Mutex
	Providers map[application.ProvisionerRef]provisioning.Provisioner
	Calls     map[application.ProvisionerRef]int
}

func (r *Resolver) Resolve(_ context.Context, ref application.ProvisionerRef) (provisioning.Provisioner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Calls == nil {
		r.Calls = make(map[application.ProvisionerRef]int)
	}
	r.Calls[ref]++
	provider, ok := r.Providers[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return provider, nil
}
