// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

var adapterTestType = domain.ResourceTypeRef{Name: "Database", Version: "v1"}

type fakeRunner struct {
	mu               sync.Mutex
	commands         []Command
	version          string
	failCommand      string
	failKind         CommandFailureKind
	malformedCommand string
	stateAbsent      bool
	planCalls        int
	desiredPresent   bool
	input            Input
	states           [][]byte
	stateCalls       int
}

func (r *fakeRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	name := ""
	if len(command.Args) > 0 {
		name = command.Args[0]
	}
	if name == "version" {
		version := r.version
		if version == "" {
			version = EngineVersion
		}
		return resultJSON(map[string]any{"terraform_version": version}), nil
	}
	if name == r.failCommand {
		return CommandResult{ExitCode: 1, Failure: r.failKind}, nil
	}
	if name == r.malformedCommand {
		return CommandResult{ExitCode: 0, Stdout: []byte("not-json\n")}, nil
	}
	switch name {
	case "init", "apply":
		return machineResult(0), nil
	case "plan":
		r.planCalls++
		if r.planCalls == 1 {
			return machineResult(2), nil
		}
		return machineResult(0), nil
	case "show":
		return CommandResult{ExitCode: 0, Stdout: r.planJSON()}, nil
	case "state":
		if r.stateAbsent {
			return CommandResult{ExitCode: 1}, nil
		}
		if r.stateCalls < len(r.states) {
			state := r.states[r.stateCalls]
			r.stateCalls++
			return CommandResult{ExitCode: 0, Stdout: append([]byte(nil), state...)}, nil
		}
		return resultJSON(map[string]any{"lineage": "lineage-1", "serial": 1}), nil
	case "output":
		return resultJSON(map[string]any{}), nil
	default:
		return CommandResult{ExitCode: 1}, nil
	}
}

func (r *fakeRunner) planJSON() []byte {
	marker := markerValue(r.input)
	resources := []map[string]any{{"address": "terraform_data.control", "mode": "managed", "values": map[string]any{"input": marker}}}
	if r.desiredPresent {
		resources = append(resources, map[string]any{"address": "terraform_data.workload", "mode": "managed", "values": map[string]any{"input": "workload"}})
	}
	doc := map[string]any{"format_version": "1.2", "resource_changes": []any{}, "planned_values": map[string]any{"root_module": map[string]any{"resources": resources}}}
	raw, _ := json.Marshal(doc)
	return raw
}

func machineResult(exit int) CommandResult {
	raw, _ := json.Marshal(map[string]any{"type": "version", "tofu": EngineVersion, "ui": "1.2", "future": true})
	return CommandResult{ExitCode: exit, Stdout: append(raw, '\n')}
}

func resultJSON(value any) CommandResult {
	raw, _ := json.Marshal(value)
	return CommandResult{ExitCode: 0, Stdout: raw}
}

type memoryEvidence struct {
	mu             sync.Mutex
	attempts       map[AttemptKey]AttemptEvidence
	binding        *StateBinding
	rejectAdvance  bool
	failUpdateOnce bool
}

func newMemoryEvidence() *memoryEvidence {
	return &memoryEvidence{attempts: map[AttemptKey]AttemptEvidence{}}
}
func (s *memoryEvidence) PrepareAttempt(_ context.Context, key AttemptKey, _ LeaseFence) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.attempts[key]; exists {
		return AttemptEvidence{}, ErrEvidenceConflict
	}
	record := AttemptEvidence{Key: key, Phase: AttemptPrepared, Version: 1}
	s.attempts[key] = record
	return record, nil
}
func (s *memoryEvidence) LoadAttempt(_ context.Context, key AttemptKey) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.attempts[key]
	if !ok {
		return AttemptEvidence{}, ErrEvidenceNotFound
	}
	return value, nil
}
func (s *memoryEvidence) AdvanceAttempt(_ context.Context, key AttemptKey, _ LeaseFence, phase AttemptPhase, version uint64, next AttemptPhase) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectAdvance {
		return AttemptEvidence{}, ErrFenceRejected
	}
	value, ok := s.attempts[key]
	if !ok || value.Phase != phase || value.Version != version || !phase.CanAdvanceTo(next) {
		return AttemptEvidence{}, ErrEvidenceConflict
	}
	value.Phase, value.Version = next, value.Version+1
	s.attempts[key] = value
	return value, nil
}
func (s *memoryEvidence) CreateStateBinding(_ context.Context, _ AttemptKey, _ LeaseFence, identity StateBindingIdentity) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding != nil {
		return StateBinding{}, ErrEvidenceConflict
	}
	value := StateBinding{Identity: identity, Version: 1}
	s.binding = &value
	return value, nil
}
func (s *memoryEvidence) LoadStateBinding(_ context.Context, _ domain.ResourceID) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding == nil {
		return StateBinding{}, ErrEvidenceNotFound
	}
	return *s.binding, nil
}
func (s *memoryEvidence) UpdateState(_ context.Context, _ AttemptKey, _ LeaseFence, version uint64, state StateEvidence) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectAdvance {
		return StateBinding{}, ErrFenceRejected
	}
	if s.failUpdateOnce {
		s.failUpdateOnce = false
		return StateBinding{}, errors.New("simulated state binding write failure")
	}
	if s.binding == nil || s.binding.Version != version {
		return StateBinding{}, ErrEvidenceConflict
	}
	s.binding.State = &state
	s.binding.Version++
	return *s.binding, nil
}

func testConfig(t *testing.T, runner *fakeRunner, evidence *memoryEvidence, capability domain.Capability) Config {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	quarantine := filepath.Join(root, "quarantine")
	source := filepath.Join(root, "source")
	for _, path := range []string{work, quarantine, source} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tofu := filepath.Join(root, "tofu")
	if err := os.WriteFile(tofu, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	hcl := `variable "liftr" { type = any }
resource "terraform_data" "control" { input = var.liftr }
resource "terraform_data" "workload" { input = "workload" }
`
	if err := os.WriteFile(filepath.Join(source, "main.tf"), []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	program := Program{Ref: "program-v1", ResourceType: adapterTestType, Capabilities: []domain.Capability{capability}, SourceDir: source, BuiltInOnly: true,
		EncodeInput: func(input Input) (map[string]any, error) {
			runner.input = input
			runner.desiredPresent = input.DesiredPresent
			return map[string]any{"desired": input.DesiredPresent}, nil
		},
		ControlMarkerAddress: "terraform_data.control", ManagedWorkloadAddresses: []string{"terraform_data.workload"}}
	applySourceDefaults(&program)
	digest, err := SourceDigest(source, sourceLimits(program))
	if err != nil {
		t.Fatal(err)
	}
	program.SourceDigest = digest
	executableDigest := sha256.Sum256([]byte("test"))
	return Config{Executable: tofu, ExecutableSHA256: hex.EncodeToString(executableDigest[:]), WorkRoot: work, QuarantineRoot: quarantine, Evidence: evidence, Runner: runner, LockTimeout: time.Second,
		AllowInsecureHTTPForTests: true, Registration: Registration{ProvisionerRef: "opentofu-v1", Identity: "platform-a", StateKeyVersion: StateKeyVersionV1, Program: program,
			Backend: BackendProfile{Ref: "backend-v1", StateURL: "http://state.test/state", LockURL: "http://state.test/lock", UnlockURL: "http://state.test/unlock"}}}
}

func executionRequest(capability domain.Capability) provisioning.ExecutionRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"size": "small"})
	return provisioning.ExecutionRequest{OperationID: "operation-1", AttemptNumber: 1, ResourceID: "resource-1", ResourceType: adapterTestType, Spec: spec, Capability: capability, TargetGeneration: 1}
}

var testFence = provisioning.ExecutionFence{MessageID: "dispatch:operation-1:1", LeaseToken: "lease-1"}

func TestExactVersionAndStateKey(t *testing.T) {
	runner := &fakeRunner{version: "1.12.5"}
	if _, err := New(testConfig(t, runner, newMemoryEvidence(), domain.CapabilityCreate)); err == nil || !strings.Contains(err.Error(), EngineVersion) {
		t.Fatalf("version rejection error = %v", err)
	}
	key := StateKey("platform-a", "opentofu-v1", adapterTestType, "resource-1")
	if key != "v1/11e91dc061dabaf1250a0d299fc3d8d16c386804754b63916fd8fce8593e8cb3" {
		t.Fatalf("state key = %s", key)
	}
	if key != StateKey("platform-a", "opentofu-v1", adapterTestType, "resource-1") {
		t.Fatal("state key was not stable")
	}
}

func TestSourceAdmissionRejectsRemoteModuleSymlinkAndBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(`module "x" { source = "registry.example/x/y" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := SourceLimits{MaxFiles: 2, MaxFileBytes: 1024, MaxTotalBytes: 1024, MaxPathBytes: 100}
	if _, err := SourceDigest(root, limits); err == nil {
		t.Fatal("remote module source was accepted")
	}
	if err := os.Remove(filepath.Join(root, "main.tf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "main.tf")); err != nil {
		t.Fatal(err)
	}
	if _, err := SourceDigest(root, limits); err == nil {
		t.Fatal("symlink source was accepted")
	}
	if err := os.Remove(filepath.Join(root, "main.tf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tf"), make([]byte, 1025), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SourceDigest(root, limits); err == nil {
		t.Fatal("oversized source was accepted")
	}
}

func TestSubmitCommandAndEnvironmentInvariants(t *testing.T) {
	runner := &fakeRunner{}
	provider, err := New(testConfig(t, runner, newMemoryEvidence(), domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	submission, err := provider.SubmitFenced(context.Background(), request, testFence)
	if err != nil || submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("submission=%+v error=%v", submission, err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		joined := strings.Join(command.Args, " ")
		for _, forbidden := range []string{"-lock=false", "force-unlock", " init -upgrade", "DELETE"} {
			if strings.Contains(" "+joined, forbidden) {
				t.Fatalf("prohibited argv: %s", joined)
			}
		}
		if strings.Contains(strings.Join(command.Env, "\n"), "INHERITED_CANARY") {
			t.Fatal("ambient environment leaked")
		}
	}
	for _, forbiddenCommand := range []string{"force-unlock", "refresh", "import"} {
		for _, command := range runner.commands {
			if len(command.Args) > 0 && command.Args[0] == forbiddenCommand {
				t.Fatalf("sacrificial command %s was run", forbiddenCommand)
			}
		}
	}
}

func TestApplyBoundaryCrashIsAmbiguousAndNotResubmitted(t *testing.T) {
	runner := &fakeRunner{}
	evidence := newMemoryEvidence()
	config := testConfig(t, runner, evidence, domain.CapabilityCreate)
	config.BeforeApply = func() error { return errors.New("simulated crash") }
	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	if _, err := provider.SubmitFenced(context.Background(), request, testFence); !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
		t.Fatalf("boundary error = %v", err)
	}
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	attempt, _ := evidence.LoadAttempt(context.Background(), key)
	if attempt.Phase != AttemptApplyMayStart {
		t.Fatalf("phase = %s", attempt.Phase)
	}
	before := len(runner.commands)
	if _, err := provider.SubmitFenced(context.Background(), request, testFence); !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
		t.Fatalf("resubmit error = %v", err)
	}
	if len(runner.commands) != before {
		t.Fatal("ambiguous attempt executed commands on resubmit")
	}
}

func TestFailureClassificationAcrossApplyBoundary(t *testing.T) {
	for _, test := range []struct {
		name, command string
		notAttempted  bool
	}{
		{"init", "init", true}, {"plan", "plan", true}, {"apply", "apply", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{failCommand: test.command}
			provider, err := New(testConfig(t, runner, newMemoryEvidence(), domain.CapabilityCreate))
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.SubmitFenced(context.Background(), executionRequest(domain.CapabilityCreate), testFence)
			if test.notAttempted != errors.Is(err, provisioning.ErrSubmissionNotAttempted) {
				t.Fatalf("error = %v", err)
			}
			if !test.notAttempted && !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
				t.Fatalf("post-boundary error = %v", err)
			}
		})
	}
}

func TestDeterministicPlanFailureIsConclusive(t *testing.T) {
	runner := &fakeRunner{failCommand: "plan", failKind: CommandFailureDeterministic}
	provider, err := New(testConfig(t, runner, newMemoryEvidence(), domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.SubmitFenced(context.Background(), executionRequest(domain.CapabilityCreate), testFence)
	if err != nil || result.Observation.Correlation != provisioning.RequestCorrelationNotFound || result.Observation.Execution == nil || result.Observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestDeleteRetainsControlMarkerAndHasNoOutputs(t *testing.T) {
	runner := &fakeRunner{}
	provider, err := New(testConfig(t, runner, newMemoryEvidence(), domain.CapabilityDelete))
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.SubmitFenced(context.Background(), executionRequest(domain.CapabilityDelete), testFence)
	if err != nil || result.Observation.Resource.Presence != provisioning.ResourcePresenceNotFound || result.Observation.Outputs != nil {
		t.Fatalf("delete result=%+v error=%v", result, err)
	}
}

func TestObserveMissingStateAfterApplyIsNeverNotFound(t *testing.T) {
	runner := &fakeRunner{}
	evidence := newMemoryEvidence()
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	evidence.attempts[key] = AttemptEvidence{Key: key, Phase: AttemptApplyMayStart, Version: 2}
	binding := StateBinding{Identity: provider.bindingIdentity(request.ResourceID), Version: 1}
	evidence.binding = &binding
	runner.stateAbsent = true
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown || observation.Resource.Presence != provisioning.ResourcePresenceUnknown {
		t.Fatalf("observation=%+v error=%v", observation, err)
	}
}

func TestObservePreparedWithoutStateBindingIsConclusiveNotFound(t *testing.T) {
	runner := &fakeRunner{}
	evidence := newMemoryEvidence()
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	evidence.attempts[key] = AttemptEvidence{Key: key, Phase: AttemptPrepared, Version: 1}
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Correlation != provisioning.RequestCorrelationNotFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("observation=%+v error=%v", observation, err)
	}
}

func TestObserveRecoversApplyMayStartToConverged(t *testing.T) {
	runner := &fakeRunner{planCalls: 1}
	evidence := newMemoryEvidence()
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	runner.input = Input{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: 1, DesiredPresent: true}
	runner.desiredPresent = true
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	evidence.attempts[key] = AttemptEvidence{Key: key, Phase: AttemptApplyMayStart, Version: 2}
	stateResult := resultJSON(map[string]any{"lineage": "lineage-1", "serial": 1})
	state, err := parseState(stateResult.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	binding := StateBinding{Identity: provider.bindingIdentity(request.ResourceID), State: &state, Version: 2}
	evidence.binding = &binding
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("observation=%+v error=%v", observation, err)
	}
	attempt, _ := evidence.LoadAttempt(context.Background(), key)
	if attempt.Phase != AttemptObservedConverged {
		t.Fatalf("recovered phase = %s", attempt.Phase)
	}
}

func TestObserveClaimsFirstStateAfterBindingUpdateCrash(t *testing.T) {
	runner := &fakeRunner{}
	evidence := newMemoryEvidence()
	evidence.failUpdateOnce = true
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	if _, err := provider.SubmitFenced(context.Background(), request, testFence); !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
		t.Fatalf("submission error = %v", err)
	}
	if evidence.binding == nil || evidence.binding.State != nil {
		t.Fatalf("first state was unexpectedly bound: %#v", evidence.binding)
	}
	runner.planCalls = 1
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded || evidence.binding.State == nil {
		t.Fatalf("observation=%+v binding=%#v error=%v", observation, evidence.binding, err)
	}
}

func TestObserveRejectsSameSerialDigestConflict(t *testing.T) {
	runner := &fakeRunner{planCalls: 1, desiredPresent: true}
	evidence := newMemoryEvidence()
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	runner.input = Input{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: 1, DesiredPresent: true}
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	evidence.attempts[key] = AttemptEvidence{Key: key, Phase: AttemptApplyMayStart, Version: 2}
	conflict := StateEvidence{Lineage: "lineage-1", Serial: 1, Digest: StateDigest{1}}
	evidence.binding = &StateBinding{Identity: provider.bindingIdentity(request.ResourceID), State: &conflict, Version: 2}
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown {
		t.Fatalf("same-serial conflict observation=%+v error=%v", observation, err)
	}
}

func TestObserveBindsStatePulledAfterVerification(t *testing.T) {
	first := resultJSON(map[string]any{"lineage": "lineage-1", "serial": 1}).Stdout
	latest := resultJSON(map[string]any{"lineage": "lineage-1", "serial": 2}).Stdout
	runner := &fakeRunner{planCalls: 1, desiredPresent: true, states: [][]byte{first, latest}}
	evidence := newMemoryEvidence()
	provider, err := New(testConfig(t, runner, evidence, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(domain.CapabilityCreate)
	runner.input = Input{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: 1, DesiredPresent: true}
	key := provider.attemptKey(request.ResourceID, request.OperationID, 1)
	evidence.attempts[key] = AttemptEvidence{Key: key, Phase: AttemptApplyOutcomeUnknown, Version: 3}
	initial, err := parseState(first)
	if err != nil {
		t.Fatal(err)
	}
	evidence.binding = &StateBinding{Identity: provider.bindingIdentity(request.ResourceID), State: &initial, Version: 2}
	observationRequest := provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: 1, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: 1}
	observation, err := provider.ObserveFenced(context.Background(), observationRequest, provisioning.ExecutionFence{MessageID: "observe:operation-1:1", LeaseToken: "lease-2"})
	if err != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("observation=%+v error=%v", observation, err)
	}
	if evidence.binding.State == nil || evidence.binding.State.Serial != 2 || evidence.binding.State.Digest != sha256.Sum256(latest) {
		t.Fatalf("bound stale state: %#v", evidence.binding.State)
	}
}

func TestPlanRejectsUnexpectedDeletedAddress(t *testing.T) {
	input := Input{ResourceID: "resource-1", OperationID: "operation-1", AttemptNumber: 1, DesiredPresent: false, Capability: domain.CapabilityDelete}
	raw, err := json.Marshal(map[string]any{
		"format_version":   "1.2",
		"resource_changes": []any{map[string]any{"address": "terraform_data.unregistered", "mode": "managed", "change": map[string]any{"actions": []string{"delete"}}}},
		"planned_values":   map[string]any{"root_module": map[string]any{"resources": []any{map[string]any{"address": "terraform_data.control", "mode": "managed", "values": map[string]any{"input": markerValue(input)}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := decodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	program := Program{ControlMarkerAddress: "terraform_data.control", ManagedWorkloadAddresses: []string{"terraform_data.workload"}}
	if err := validatePlan(document, program, input, false); err == nil {
		t.Fatal("unexpected deleted managed address was accepted")
	}
}

func TestSensitiveOutputCanaryNeverLeaks(t *testing.T) {
	canary := "SUPER_SECRET_CANARY"
	raw, _ := json.Marshal(map[string]any{"envelope": map[string]any{"sensitive": true, "type": "string", "value": canary}})
	_, err := decodeOutputs(raw, OutputMapping{Ref: "m1", EnvelopeName: "envelope", Fields: map[string]string{"host": "host"}}, "resource-1", 1)
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("sensitive error = %v", err)
	}
	tooLarge, _ := json.Marshal(map[string]any{"envelope": map[string]any{"sensitive": false, "type": "string", "value": strings.Repeat("x", maxOutputEnvelopeBytes+1)}})
	if _, err := decodeOutputs(tooLarge, OutputMapping{}, "resource-1", 1); err == nil {
		t.Fatal("oversized selected output envelope was accepted")
	}
}

func TestUnmappedRootOutputsDoNotConsumeEnvelopeLimit(t *testing.T) {
	mapping := OutputMapping{Ref: "m1", EnvelopeName: "envelope", Fields: map[string]string{"host": "host"}}
	raw, err := json.Marshal(map[string]any{
		"unmapped": map[string]any{"sensitive": true, "type": "string", "value": strings.Repeat("x", maxOutputEnvelopeBytes+1)},
		"envelope": map[string]any{"sensitive": false, "type": []any{"object", map[string]any{}}, "value": map[string]any{
			"version": 1, "mapping": "m1", "resourceId": "resource-1", "targetGeneration": 1, "values": map[string]any{"host": "db.example"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	values, err := decodeOutputs(raw, mapping, "resource-1", 1)
	if err != nil || values["host"] != "db.example" {
		t.Fatalf("selected output values=%v error=%v", values, err)
	}
}

func TestOutputJSONIsStrictAndRejectsOutOfRangeIntegers(t *testing.T) {
	mapping := OutputMapping{Ref: "m1", EnvelopeName: "envelope", Fields: map[string]string{"port": "port"}}
	prefix := `{"envelope":{"sensitive":false,"type":["object",{}],"value":{"version":1,"mapping":"m1","resourceId":"resource-1","targetGeneration":1,"values":{"port":`
	values, err := decodeOutputs([]byte(prefix+`9007199254740993}}}}`), mapping, "resource-1", 1)
	if err != nil || values["port"] != int64(9007199254740993) {
		t.Fatalf("exact integer value=%#v error=%v", values["port"], err)
	}
	for name, suffix := range map[string]string{
		"duplicate":        `1,"port":2}}}}}`,
		"large integer":    `9223372036854775808}}}}`,
		"unknown metadata": `1}}},"unexpected":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeOutputs([]byte(prefix+suffix), mapping, "resource-1", 1); err == nil {
				t.Fatal("invalid output JSON was accepted")
			}
		})
	}
}

func TestOSRunnerCancelsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows startup is rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (OSCommandRunner{}).Run(ctx, Command{Path: "/bin/sleep", Args: []string{"30"}, MaxOutputBytes: 1024})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation error=%v duration=%v", err, time.Since(started))
	}
}

func TestOSRunnerRejectsCanceledContextBeforeSpawn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (OSCommandRunner{}).Run(ctx, Command{Path: filepath.Join(t.TempDir(), "must-not-spawn"), MaxOutputBytes: 1024})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled run error = %v", err)
	}
}

func TestRestartQuarantinesErroredStateWithoutDeletingIt(t *testing.T) {
	root := t.TempDir()
	workRoot := filepath.Join(root, "work")
	quarantineRoot := filepath.Join(root, "quarantine")
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(quarantineRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	work, err := newWorkspace(workRoot, quarantineRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work.path, "errored.tfstate"), []byte("crash evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = work.lock.Close()
	if err := scanOrphanWorkspaces(workRoot, quarantineRoot); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(quarantineRoot, "*", "errored.tfstate"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined state matches=%v error=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil || string(raw) != "crash evidence" {
		t.Fatalf("quarantined state=%q error=%v", raw, err)
	}
}

func TestFenceLossRejectsJournalAdvance(t *testing.T) {
	runner := &fakeRunner{}
	evidence := newMemoryEvidence()
	config := testConfig(t, runner, evidence, domain.CapabilityCreate)
	config.BeforeApply = func() error { evidence.mu.Lock(); evidence.rejectAdvance = true; evidence.mu.Unlock(); return nil }
	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.SubmitFenced(context.Background(), executionRequest(domain.CapabilityCreate), testFence)
	if !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
		t.Fatalf("fence-loss result = %v", err)
	}
}

func TestExecutableDigestValidation(t *testing.T) {
	runner := &fakeRunner{}
	config := testConfig(t, runner, newMemoryEvidence(), domain.CapabilityCreate)
	digest := sha256.Sum256([]byte("different"))
	config.ExecutableSHA256 = hex.EncodeToString(digest[:])
	if _, err := New(config); err == nil {
		t.Fatal("invalid executable digest was accepted")
	}
}

func TestExecutableDigestIsRequired(t *testing.T) {
	config := testConfig(t, &fakeRunner{}, newMemoryEvidence(), domain.CapabilityCreate)
	config.ExecutableSHA256 = ""
	if _, err := New(config); err == nil {
		t.Fatal("missing executable digest was accepted")
	}
}
