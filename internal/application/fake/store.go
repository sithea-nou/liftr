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
)

// ErrNotFound reports a missing record from non-repository fakes (catalog,
// resolver). Repository fakes return the application sentinel errors that the
// PostgreSQL store surfaces.
var ErrNotFound = errors.New("fake record not found")

type Store struct {
	mu          sync.Mutex
	resources   map[domain.ResourceID]application.ResourceRecord
	operations  map[domain.OperationID]application.OperationRecord
	events      map[domain.EventID]domain.Event
	executions  map[domain.OperationID]application.ProvisioningExecutionRecord
	idempotency map[string]application.IdempotencyRecord
	attempts    map[string]application.SubmissionAttemptRecord
	outbox      map[string]application.OutboxMessage
}

func NewStore() *Store {
	return &Store{
		resources:   make(map[domain.ResourceID]application.ResourceRecord),
		operations:  make(map[domain.OperationID]application.OperationRecord),
		events:      make(map[domain.EventID]domain.Event),
		executions:  make(map[domain.OperationID]application.ProvisioningExecutionRecord),
		idempotency: make(map[string]application.IdempotencyRecord),
		attempts:    make(map[string]application.SubmissionAttemptRecord),
		outbox:      make(map[string]application.OutboxMessage),
	}
}

func (s *Store) Within(_ context.Context, fn func(application.UnitOfWork) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Store{
		resources:   cloneMap(s.resources),
		operations:  cloneMap(s.operations),
		events:      cloneMap(s.events),
		executions:  cloneExecutions(s.executions),
		idempotency: cloneMap(s.idempotency),
		attempts:    cloneMap(s.attempts),
		outbox:      cloneMap(s.outbox),
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.resources = tx.resources
	s.operations = tx.operations
	s.events = tx.events
	s.executions = tx.executions
	s.idempotency = tx.idempotency
	s.attempts = tx.attempts
	s.outbox = tx.outbox
	return nil
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
	Resources   int
	Operations  int
	Events      int
	Executions  int
	Idempotency int
	Attempts    int
	Outbox      int
}

func (s *Store) RecordCounts() RecordCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RecordCounts{
		Resources:   len(s.resources),
		Operations:  len(s.operations),
		Events:      len(s.events),
		Executions:  len(s.executions),
		Idempotency: len(s.idempotency),
		Attempts:    len(s.attempts),
		Outbox:      len(s.outbox),
	}
}

func (s *Store) Resources() application.ResourceRepository                   { return s }
func (s *Store) Operations() application.OperationRepository                 { return s }
func (s *Store) Events() application.EventRepository                         { return s }
func (s *Store) Executions() application.ExecutionRepository                 { return s }
func (s *Store) Idempotency() application.IdempotencyRepository              { return s }
func (s *Store) SubmissionAttempts() application.SubmissionAttemptRepository { return s }
func (s *Store) Outbox() application.OutboxRepository                        { return s }

func (s *Store) GetResource(_ context.Context, id domain.ResourceID) (application.ResourceRecord, error) {
	record, ok := s.resources[id]
	if !ok {
		return application.ResourceRecord{}, application.ErrResourceNotFound
	}
	return record, nil
}

func (s *Store) CreateResource(_ context.Context, record application.ResourceRecord) error {
	if _, exists := s.resources[record.Resource.ID()]; exists {
		return fmt.Errorf("%w: resource already exists", application.ErrConcurrencyConflict)
	}
	if record.Version == 0 {
		record.Version = 1
	}
	s.resources[record.Resource.ID()] = record
	return nil
}

func (s *Store) SaveResource(_ context.Context, record application.ResourceRecord, expectedVersion uint64) error {
	current, ok := s.resources[record.Resource.ID()]
	if !ok {
		return application.ErrConcurrencyConflict
	}
	if current.Version != expectedVersion {
		return application.ErrConcurrencyConflict
	}
	record.Version = expectedVersion + 1
	s.resources[record.Resource.ID()] = record
	return nil
}

func (s *Store) GetOperation(_ context.Context, id domain.OperationID) (application.OperationRecord, error) {
	record, ok := s.operations[id]
	if !ok {
		return application.OperationRecord{}, application.ErrOperationNotFound
	}
	return record, nil
}

func (s *Store) ActiveForResource(_ context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	for _, record := range s.operations {
		if record.Operation.ResourceID() == id && !record.Operation.IsTerminal() {
			return record, true, nil
		}
	}
	return application.OperationRecord{}, false, nil
}

// LatestForResource mirrors the PostgreSQL ordering exactly: newest
// requested_at_ns first and, for equal timestamps, the descending Operation ID
// compared byte-wise. Repeated calls always select the same Operation.
func (s *Store) LatestForResource(_ context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	var latest application.OperationRecord
	found := false
	for _, record := range s.operations {
		if record.Operation.ResourceID() != id {
			continue
		}
		if !found || operationPrecedes(latest.Operation, record.Operation) {
			latest = record
			found = true
		}
	}
	return latest, found, nil
}

// operationPrecedes reports whether left is older than right under the shared
// deterministic ordering. Timestamps compare as Unix nanoseconds to match the
// bigint requested_at_ns column; Operation IDs compare byte-wise to match a
// "C"-collated text column.
func operationPrecedes(left, right domain.Operation) bool {
	leftNS, rightNS := left.RequestedAt().UnixNano(), right.RequestedAt().UnixNano()
	if leftNS != rightNS {
		return leftNS < rightNS
	}
	return left.ID() < right.ID()
}

func (s *Store) CreateOperation(_ context.Context, record application.OperationRecord) error {
	if _, exists := s.operations[record.Operation.ID()]; exists {
		return fmt.Errorf("operation already exists")
	}
	if record.Version == 0 {
		record.Version = 1
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

func (s *Store) CreateExecution(_ context.Context, record application.ProvisioningExecutionRecord) error {
	if _, exists := s.executions[record.OperationID]; exists {
		return fmt.Errorf("execution already exists")
	}
	if record.Version == 0 {
		record.Version = 1
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
	record.Version = expectedVersion + 1
	s.executions[record.OperationID] = cloneExecution(record)
	return nil
}

func (s *Store) GetIdempotency(_ context.Context, key string) (application.IdempotencyRecord, error) {
	record, ok := s.idempotency[key]
	if !ok {
		return application.IdempotencyRecord{}, application.ErrIdempotencyNotFound
	}
	return record, nil
}

func (s *Store) PutIdempotency(_ context.Context, record application.IdempotencyRecord) error {
	if _, exists := s.idempotency[record.Key]; exists {
		return fmt.Errorf("idempotency key already exists")
	}
	s.idempotency[record.Key] = record
	return nil
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
