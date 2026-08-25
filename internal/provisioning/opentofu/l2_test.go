// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

const (
	l2OutputMapping = "l2-output-v1"
	l2Canary        = "LIFTR_OPENTOFU_L2_SENSITIVE_CANARY"
	l2MaxHTTPBody   = 20 << 20
)

var l2ResourceType = domain.ResourceTypeRef{Name: "L2ManagedResource", Version: "v1"}

type httpFault string

const (
	httpFaultNone       httpFault = ""
	httpFaultOutage     httpFault = "outage"
	httpFaultReject     httpFault = "reject-state"
	httpFaultResponse   httpFault = "write-then-response-loss"
	httpFaultBlockWrite httpFault = "block-state-write"
)

type httpRequestMetadata struct {
	Method     string
	Path       string
	Query      string
	BodyBytes  int
	BodyDigest [sha256.Size]byte
	LockID     string
}

type l2HTTPBackend struct {
	mu               sync.Mutex
	server           *httptest.Server
	state            []byte
	lockBody         []byte
	lockID           string
	contentionStatus int
	fault            httpFault
	events           []httpRequestMetadata
	postArrived      chan struct{}
	postOnce         sync.Once
}

func newL2HTTPBackend(t *testing.T) *l2HTTPBackend {
	t.Helper()
	backend := &l2HTTPBackend{contentionStatus: http.StatusConflict, postArrived: make(chan struct{})}
	backend.server = httptest.NewServer(http.HandlerFunc(backend.serveHTTP))
	t.Cleanup(backend.server.Close)
	return backend
}

func (b *l2HTTPBackend) URLs() (string, string, string) {
	return b.server.URL + "/state", b.server.URL + "/lock", b.server.URL + "/unlock"
}

func (b *l2HTTPBackend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, l2MaxHTTPBody+1))
	if err != nil || len(body) > l2MaxHTTPBody {
		http.Error(w, "request rejected", http.StatusRequestEntityTooLarge)
		return
	}
	metadata := httpRequestMetadata{
		Method:     boundedMetadata(r.Method),
		Path:       boundedMetadata(r.URL.EscapedPath()),
		Query:      boundedMetadata(r.URL.RawQuery),
		BodyBytes:  len(body),
		BodyDigest: sha256.Sum256(body),
	}
	if r.Method == "LOCK" || r.Method == "UNLOCK" {
		var lock struct {
			ID string `json:"ID"`
		}
		if json.Unmarshal(body, &lock) != nil || strings.TrimSpace(lock.ID) == "" {
			http.Error(w, "invalid lock request", http.StatusBadRequest)
			return
		}
		metadata.LockID = boundedMetadata(lock.ID)
	}

	b.mu.Lock()
	b.events = append(b.events, metadata)
	fault := b.fault
	if fault == httpFaultOutage {
		b.mu.Unlock()
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/state/") && r.Method == http.MethodGet:
		state := append([]byte(nil), b.state...)
		b.mu.Unlock()
		if len(state) == 0 {
			http.Error(w, "state not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(state)
	case strings.HasPrefix(r.URL.Path, "/state/") && r.Method == http.MethodPost:
		b.postOnce.Do(func() { close(b.postArrived) })
		if b.lockID == "" || r.URL.Query().Get("ID") != b.lockID {
			b.mu.Unlock()
			http.Error(w, "state lock mismatch", http.StatusLocked)
			return
		}
		if fault == httpFaultReject {
			b.mu.Unlock()
			http.Error(w, "state persistence failed", http.StatusInternalServerError)
			return
		}
		if fault == httpFaultBlockWrite {
			b.mu.Unlock()
			<-r.Context().Done()
			return
		}
		b.state = append([]byte(nil), body...)
		b.mu.Unlock()
		if fault == httpFaultResponse {
			if hijacker, ok := w.(http.Hijacker); ok {
				connection, _, hijackErr := hijacker.Hijack()
				if hijackErr == nil {
					_ = connection.Close()
					return
				}
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/lock/") && r.Method == "LOCK":
		if len(b.lockBody) != 0 {
			status, existing := b.contentionStatus, append([]byte(nil), b.lockBody...)
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(existing)
			return
		}
		b.lockBody = append([]byte(nil), body...)
		b.lockID = metadata.LockID
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/unlock/") && r.Method == "UNLOCK":
		if b.lockID == "" || metadata.LockID != b.lockID {
			b.mu.Unlock()
			http.Error(w, "unlock mismatch", http.StatusConflict)
			return
		}
		b.lockBody = nil
		b.lockID = ""
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		b.mu.Unlock()
		http.Error(w, "unsupported backend request", http.StatusMethodNotAllowed)
	}
}

func boundedMetadata(value string) string {
	const limit = 1024
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func (b *l2HTTPBackend) setFault(fault httpFault) {
	b.mu.Lock()
	b.fault = fault
	b.mu.Unlock()
}

func (b *l2HTTPBackend) prelock(status int) {
	body := []byte(`{"ID":"contended-lock","Operation":"OperationTypePlan","Info":"deterministic contention","Who":"l2-test","Version":"1.12.6","Created":"2026-01-01T00:00:00Z","Path":"state"}`)
	b.mu.Lock()
	b.lockBody = body
	b.lockID = "contended-lock"
	b.contentionStatus = status
	b.mu.Unlock()
}

func (b *l2HTTPBackend) clearLock() {
	b.mu.Lock()
	b.lockBody = nil
	b.lockID = ""
	b.mu.Unlock()
}

func (b *l2HTTPBackend) snapshot() ([]byte, []httpRequestMetadata) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.state...), append([]httpRequestMetadata(nil), b.events...)
}

func (b *l2HTTPBackend) replaceState(raw []byte) {
	b.mu.Lock()
	b.state = append([]byte(nil), raw...)
	b.mu.Unlock()
}

func (b *l2HTTPBackend) assertNoDelete(t *testing.T) {
	t.Helper()
	_, events := b.snapshot()
	for _, event := range events {
		if event.Method == http.MethodDelete {
			t.Fatal("OpenTofu HTTP backend DELETE was observed")
		}
	}
}

type l2EvidenceStore struct {
	mu       sync.Mutex
	attempts map[AttemptKey]AttemptEvidence
	bindings map[domain.ResourceID]StateBinding
	leases   map[string]l2Lease
	clock    uint64
}

type l2Lease struct {
	token string
	key   AttemptKey
}

func newL2EvidenceStore() *l2EvidenceStore {
	return &l2EvidenceStore{attempts: map[AttemptKey]AttemptEvidence{}, bindings: map[domain.ResourceID]StateBinding{}, leases: map[string]l2Lease{}}
}

func (s *l2EvidenceStore) authorize(key AttemptKey, fence LeaseFence) error {
	lease, found := s.leases[fence.MessageID]
	if key.Validate() != nil || fence.Validate() != nil || !found || lease.token != fence.Token || lease.key != key {
		return ErrFenceRejected
	}
	return nil
}

func (s *l2EvidenceStore) allow(key AttemptKey, fence provisioning.ExecutionFence) {
	s.mu.Lock()
	s.leases[fence.MessageID] = l2Lease{token: fence.LeaseToken, key: key}
	s.mu.Unlock()
}

func (s *l2EvidenceStore) now() time.Time {
	s.clock++
	return time.Unix(0, int64(s.clock)).UTC()
}

func (s *l2EvidenceStore) PrepareAttempt(_ context.Context, key AttemptKey, fence LeaseFence) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(key, fence); err != nil {
		return AttemptEvidence{}, err
	}
	if _, exists := s.attempts[key]; exists {
		return AttemptEvidence{}, ErrEvidenceConflict
	}
	now := s.now()
	attempt := AttemptEvidence{Key: key, Phase: AttemptPrepared, Version: 1, CreatedAt: now, UpdatedAt: now}
	s.attempts[key] = attempt
	return attempt, nil
}

func (s *l2EvidenceStore) LoadAttempt(_ context.Context, key AttemptKey) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key.Validate() != nil {
		return AttemptEvidence{}, ErrInvalidEvidence
	}
	attempt, found := s.attempts[key]
	if !found {
		return AttemptEvidence{}, ErrEvidenceNotFound
	}
	return attempt, nil
}

func (s *l2EvidenceStore) AdvanceAttempt(_ context.Context, key AttemptKey, fence LeaseFence, phase AttemptPhase, version uint64, next AttemptPhase) (AttemptEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(key, fence); err != nil {
		return AttemptEvidence{}, err
	}
	attempt, found := s.attempts[key]
	if !found || attempt.Phase != phase || attempt.Version != version || !phase.CanAdvanceTo(next) {
		return AttemptEvidence{}, ErrEvidenceConflict
	}
	attempt.Phase = next
	attempt.Version++
	attempt.UpdatedAt = s.now()
	s.attempts[key] = attempt
	return attempt, nil
}

func (s *l2EvidenceStore) CreateStateBinding(_ context.Context, key AttemptKey, fence LeaseFence, identity StateBindingIdentity) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(key, fence); err != nil {
		return StateBinding{}, err
	}
	if identity.Validate() != nil || identity.ResourceID != key.ResourceID {
		return StateBinding{}, ErrInvalidEvidence
	}
	if _, found := s.attempts[key]; !found {
		return StateBinding{}, ErrEvidenceConflict
	}
	if _, found := s.bindings[key.ResourceID]; found {
		return StateBinding{}, ErrEvidenceConflict
	}
	now := s.now()
	binding := StateBinding{Identity: identity, Version: 1, CreatedAt: now, UpdatedAt: now}
	s.bindings[key.ResourceID] = binding
	return cloneL2Binding(binding), nil
}

func (s *l2EvidenceStore) LoadStateBinding(_ context.Context, resourceID domain.ResourceID) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, found := s.bindings[resourceID]
	if !found {
		return StateBinding{}, ErrEvidenceNotFound
	}
	return cloneL2Binding(binding), nil
}

func (s *l2EvidenceStore) UpdateState(_ context.Context, key AttemptKey, fence LeaseFence, version uint64, state StateEvidence) (StateBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(key, fence); err != nil {
		return StateBinding{}, err
	}
	attempt, found := s.attempts[key]
	if !found || attempt.Phase == AttemptPrepared || state.Validate() != nil || state.Digest == (StateDigest{}) {
		return StateBinding{}, ErrInvalidEvidence
	}
	binding, found := s.bindings[key.ResourceID]
	if !found || binding.Version != version || binding.Identity.ResourceID != key.ResourceID || binding.Identity.ProvisionerRef != key.ProvisionerRef {
		return StateBinding{}, ErrEvidenceConflict
	}
	if binding.State != nil && !stateCompatible(*binding.State, state) {
		return StateBinding{}, ErrEvidenceConflict
	}
	binding.State = &state
	binding.Version++
	binding.UpdatedAt = s.now()
	s.bindings[key.ResourceID] = binding
	return cloneL2Binding(binding), nil
}

func cloneL2Binding(binding StateBinding) StateBinding {
	if binding.State != nil {
		state := *binding.State
		binding.State = &state
	}
	return binding
}

type l2CommandRunner struct {
	mu               sync.Mutex
	commands         [][]string
	providerArtifact bool
	providerDownload bool
	lockFile         bool
	stderrCanary     bool
}

func (r *l2CommandRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	result, err := (OSCommandRunner{}).Run(ctx, command)
	r.mu.Lock()
	r.commands = append(r.commands, append([]string(nil), command.Args...))
	if bytes.Contains(result.Stderr, []byte(l2Canary)) {
		r.stderrCanary = true
	}
	diagnostics := append(append([]byte(nil), result.Stdout...), result.Stderr...)
	if bytes.Contains(diagnostics, []byte("registry.opentofu.org")) || bytes.Contains(diagnostics, []byte("Installing provider")) || bytes.Contains(diagnostics, []byte("Downloading")) {
		r.providerDownload = true
	}
	if command.Dir != "" {
		if _, statErr := os.Lstat(filepath.Join(command.Dir, ".terraform.lock.hcl")); statErr == nil {
			r.lockFile = true
		}
		if entries, readErr := os.ReadDir(filepath.Join(command.Dir, ".terraform", "providers")); readErr == nil && len(entries) != 0 {
			r.providerArtifact = true
		}
	}
	r.mu.Unlock()
	return result, err
}

func (r *l2CommandRunner) assertBuiltInOnly(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lockFile {
		t.Fatal("built-in-only OpenTofu generated a dependency lock file")
	}
	if r.providerArtifact {
		t.Fatal("built-in-only OpenTofu installed a provider package")
	}
	if r.providerDownload {
		t.Fatal("built-in-only OpenTofu attempted a provider download")
	}
	if r.stderrCanary {
		t.Fatal("sensitive canary appeared in command diagnostics")
	}
	for _, args := range r.commands {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-lock=false") || strings.Contains(joined, "force-unlock") {
			t.Fatal("OpenTofu lock safety fallback was attempted")
		}
	}
}

type l2Harness struct {
	t          *testing.T
	executable string
	digest     string
	root       string
	source     string
	backend    *l2HTTPBackend
	evidence   *l2EvidenceStore
	runner     *l2CommandRunner
}

func newL2Harness(t *testing.T) *l2Harness {
	t.Helper()
	executable := os.Getenv("LIFTR_TEST_OPENTOFU_BIN")
	if executable == "" {
		t.Skip("LIFTR_TEST_OPENTOFU_BIN is not set")
	}
	abs, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal("resolve OpenTofu test binary")
	}
	digest, err := digestFile(abs, maxExecutableBytes)
	if err != nil {
		t.Fatal("digest OpenTofu test binary")
	}
	root := t.TempDir()
	for _, name := range []string{"work", "quarantine", "source"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal("create private OpenTofu test directory")
		}
	}
	source := filepath.Join(root, "source")
	program := `variable "liftr" {
  type = any
}

variable "desired_present" {
  type = bool
}

resource "terraform_data" "liftr_control" {
  input = var.liftr
}

resource "terraform_data" "workload" {
  count = var.desired_present ? 1 : 0
  input = {
    endpoint   = "${var.liftr.resourceId}-${var.liftr.targetGeneration}"
    generation = var.liftr.targetGeneration
  }
}

output "liftr_envelope" {
  value = {
    version          = 1
    mapping          = "l2-output-v1"
    resourceId       = var.liftr.resourceId
    targetGeneration = var.liftr.targetGeneration
    values = {
      endpoint   = var.desired_present ? terraform_data.workload[0].output.endpoint : "absent"
      generation = var.liftr.targetGeneration
    }
  }
}

output "sensitive_canary" {
  sensitive = true
  value     = "LIFTR_OPENTOFU_L2_SENSITIVE_CANARY"
}
`
	if err := os.WriteFile(filepath.Join(source, "main.tf"), []byte(program), 0o600); err != nil {
		t.Fatal("write trusted OpenTofu test program")
	}
	return &l2Harness{t: t, executable: abs, digest: digest, root: root, source: source, backend: newL2HTTPBackend(t), evidence: newL2EvidenceStore(), runner: &l2CommandRunner{}}
}

func (h *l2Harness) provisioner() *Provisioner {
	h.t.Helper()
	program := Program{
		Ref:          "l2-program-v1",
		ResourceType: l2ResourceType,
		Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		SourceDir:    h.source,
		BuiltInOnly:  true,
		EncodeInput: func(input Input) (map[string]any, error) {
			return map[string]any{"desired_present": input.DesiredPresent}, nil
		},
		ControlMarkerAddress:     "terraform_data.liftr_control",
		ManagedWorkloadAddresses: []string{"terraform_data.workload[0]"},
		OutputMappings: []OutputMapping{{Ref: l2OutputMapping, EnvelopeName: "liftr_envelope", Fields: map[string]string{
			"endpoint": "endpoint", "generation": "generation",
		}}},
		CurrentOutputMappingRef: l2OutputMapping,
	}
	applySourceDefaults(&program)
	digest, err := SourceDigest(program.SourceDir, sourceLimits(program))
	if err != nil {
		h.t.Fatal("digest trusted OpenTofu test program")
	}
	program.SourceDigest = digest
	stateURL, lockURL, unlockURL := h.backend.URLs()
	provider, err := New(Config{
		Executable:                h.executable,
		ExecutableSHA256:          h.digest,
		WorkRoot:                  filepath.Join(h.root, "work"),
		QuarantineRoot:            filepath.Join(h.root, "quarantine"),
		Evidence:                  h.evidence,
		Runner:                    h.runner,
		LockTimeout:               250 * time.Millisecond,
		AllowInsecureHTTPForTests: true,
		Registration: Registration{
			ProvisionerRef:  "l2-opentofu-v1",
			Identity:        "l2-platform",
			StateKeyVersion: StateKeyVersionV1,
			Program:         program,
			Backend:         BackendProfile{Ref: "l2-http-v1", StateURL: stateURL, LockURL: lockURL, UnlockURL: unlockURL},
		},
	})
	if err != nil {
		h.t.Fatalf("construct real OpenTofu provisioner: %v", err)
	}
	return provider
}

func TestL2ProductionAdmissionPinsOfficialExecutable(t *testing.T) {
	h := newL2Harness(t)
	configuredDir := filepath.Join(h.root, "configured")
	if err := os.Mkdir(configuredDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(configuredDir, "tofu")
	input, err := os.Open(h.executable)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(configured, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err == nil {
		_, err = io.Copy(output, input)
	}
	_ = input.Close()
	if output != nil {
		_ = output.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	h.executable = configured
	base := h.provisioner().config.Config
	base.Runner = nil
	base.WorkRoot = filepath.Join(h.root, "production-work")
	base.QuarantineRoot = filepath.Join(h.root, "production-quarantine")
	for _, root := range []string{base.WorkRoot, base.QuarantineRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	provider, err := New(base)
	if err != nil {
		t.Fatalf("production admission rejected official binary: %v", err)
	}
	if provider.config.Executable == configured || !pathWithin(base.WorkRoot, provider.config.Executable) {
		t.Fatalf("executable was not privately pinned: %q", provider.config.Executable)
	}
	if err := os.WriteFile(configured, []byte("replaced after startup"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := provider.config.Runner.Run(context.Background(), Command{
		Path: provider.config.Executable, Args: []string{"version", "-json"},
		Env: baseEnvironment(provider.config.Executable, "", ""), MaxOutputBytes: defaultMaxOutput,
	})
	var identity struct {
		Version string `json:"terraform_version"`
	}
	if err != nil || result.ExitCode != 0 || json.Unmarshal(result.Stdout, &identity) != nil || identity.Version != EngineVersion {
		t.Fatalf("pinned executable changed with configured path: result=%s error=%v", result.Stdout, err)
	}
}

func TestL2SemanticInputFailureIsTerminalBeforeApply(t *testing.T) {
	h := newL2Harness(t)
	path := filepath.Join(h.source, "main.tf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nvariable \"required_but_unmapped\" {\n  type = string\n}\n")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := h.provisioner()
	request := l2Request("l2-semantic-rejection", domain.CapabilityCreate, 1)
	submission, err := h.submit(provider, request, "semantic-rejection")
	if err != nil {
		t.Fatalf("semantic rejection was returned as retryable or ambiguous: %v", err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound || submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("semantic rejection was not terminal: %+v", submission.Observation)
	}
	attempt, err := h.evidence.LoadAttempt(context.Background(), provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber))
	if err != nil || attempt.Phase != AttemptPrepared {
		t.Fatalf("semantic rejection phase=%s error=%v", attempt.Phase, err)
	}
}

func l2Request(operation string, capability domain.Capability, generation uint64) provisioning.ExecutionRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"size": "small"})
	request := provisioning.ExecutionRequest{
		OperationID: domain.OperationID(operation), AttemptNumber: 1, ResourceID: "l2-resource-1", ResourceType: l2ResourceType,
		Spec: spec, Capability: capability, TargetGeneration: generation,
	}
	if capability != domain.CapabilityDelete {
		request.OutputMappingRef = l2OutputMapping
	}
	return request
}

func l2Observation(request provisioning.ExecutionRequest) provisioning.ObservationRequest {
	return provisioning.ObservationRequest{
		OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID,
		ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability,
		TargetGeneration: request.TargetGeneration, OutputMappingRef: request.OutputMappingRef,
	}
}

func l2Fence(label string) provisioning.ExecutionFence {
	return provisioning.ExecutionFence{MessageID: "l2-message-" + label, LeaseToken: "l2-token-" + label}
}

func (h *l2Harness) submit(provider *Provisioner, request provisioning.ExecutionRequest, label string) (provisioning.Submission, error) {
	fence := l2Fence(label)
	h.evidence.allow(provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber), fence)
	return provider.SubmitFenced(context.Background(), request, fence)
}

func (h *l2Harness) observe(provider *Provisioner, request provisioning.ExecutionRequest, label string) (provisioning.ExecutionObservation, error) {
	fence := l2Fence(label)
	h.evidence.allow(provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber), fence)
	return provider.ObserveFenced(context.Background(), l2Observation(request), fence)
}

func requireL2Succeeded(t *testing.T, observation provisioning.ExecutionObservation, outputs bool) {
	t.Helper()
	if observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded || observation.Correlation != provisioning.RequestCorrelationFound {
		t.Fatal("real OpenTofu execution did not converge")
	}
	if outputs {
		if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
			t.Fatal("real OpenTofu outputs were not available")
		}
		encoded, _ := json.Marshal(observation.Outputs.Values)
		if bytes.Contains(encoded, []byte(l2Canary)) {
			t.Fatal("sensitive canary crossed the output boundary")
		}
	} else if observation.Outputs != nil {
		t.Fatal("delete returned outputs")
	}
}

func stateInstanceAddresses(t *testing.T, raw []byte) []string {
	t.Helper()
	var state struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Index any `json:"index_key"`
			} `json:"instances"`
		} `json:"resources"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&state) != nil {
		t.Fatal("decode backend state closure")
	}
	var addresses []string
	for _, resource := range state.Resources {
		if resource.Mode != "managed" {
			continue
		}
		for _, instance := range resource.Instances {
			address := resource.Type + "." + resource.Name
			if instance.Index != nil {
				address += "[" + fmt.Sprint(instance.Index) + "]"
			}
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	return addresses
}

func requireStateClosure(t *testing.T, raw []byte, workload bool) StateEvidence {
	t.Helper()
	state, err := parseState(raw)
	if err != nil {
		t.Fatal("parse backend state evidence")
	}
	expected := []string{"terraform_data.liftr_control"}
	if workload {
		expected = append(expected, "terraform_data.workload[0]")
	}
	actual := stateInstanceAddresses(t, raw)
	if strings.Join(actual, ",") != strings.Join(expected, ",") {
		t.Fatal("backend state managed closure did not match registration")
	}
	return state
}

func TestHTTPBackendProtocolAndFaultModes(t *testing.T) {
	backend := newL2HTTPBackend(t)
	client := backend.server.Client()
	stateURL, lockURL, unlockURL := backend.URLs()
	key := "/v1/test-key"
	lockBody := []byte(`{"ID":"lock-1","Operation":"OperationTypeApply","Info":"l2","Who":"test","Version":"1.12.6","Created":"2026-01-01T00:00:00Z","Path":"state"}`)

	response, err := client.Get(stateURL + key)
	if err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatal("missing backend state protocol failed")
	}
	_ = response.Body.Close()
	request, _ := http.NewRequest("LOCK", lockURL+key, bytes.NewReader(lockBody))
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("backend lock protocol failed")
	}
	_ = response.Body.Close()
	state := []byte(`{"version":4,"terraform_version":"1.12.6","serial":1,"lineage":"lineage","outputs":{},"resources":[],"check_results":null}`)
	request, _ = http.NewRequest(http.MethodPost, stateURL+key+"?ID=lock-1", bytes.NewReader(state))
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("backend state update protocol failed")
	}
	_ = response.Body.Close()
	response, err = client.Get(stateURL + key)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("backend state retrieval protocol failed")
	}
	got, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(got, state) {
		t.Fatal("backend did not preserve exact state bytes")
	}
	request, _ = http.NewRequest("UNLOCK", unlockURL+key, bytes.NewReader(lockBody))
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("backend unlock protocol failed")
	}
	_ = response.Body.Close()

	backend.setFault(httpFaultReject)
	backend.prelock(http.StatusConflict)
	request, _ = http.NewRequest(http.MethodPost, stateURL+key+"?ID=contended-lock", bytes.NewReader(state))
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusInternalServerError {
		t.Fatal("backend rejected-write fault failed")
	}
	_ = response.Body.Close()
	backend.setFault(httpFaultOutage)
	response, err = client.Get(stateURL + key)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("backend outage fault failed")
	}
	_ = response.Body.Close()
	backend.assertNoDelete(t)
	_, events := backend.snapshot()
	if len(events) == 0 {
		t.Fatal("backend request metadata was not recorded")
	}
	for _, event := range events {
		if len(event.Method) > 1024 || len(event.Path) > 1024 || len(event.Query) > 1024 {
			t.Fatal("backend request metadata exceeded its bound")
		}
	}
}

func TestL2RealLifecycleAcrossRestarts(t *testing.T) {
	h := newL2Harness(t)
	provider := h.provisioner()
	stateKey := provider.stateKey("l2-resource-1")
	if stateKey != "v1/5a13d92c3350fc2841b535973ab4a7a0f6d70a7552c552bed3ccce655d6740de" || stateKey != StateKey("l2-platform", "l2-opentofu-v1", l2ResourceType, "l2-resource-1") {
		t.Fatal("stable OpenTofu state key mismatch")
	}

	create := l2Request("l2-create", domain.CapabilityCreate, 1)
	submission, err := h.submit(provider, create, "create-submit")
	if err != nil {
		t.Fatalf("real OpenTofu create failed: %v", err)
	}
	requireL2Succeeded(t, submission.Observation, true)
	if submission.Observation.Outputs.Values["endpoint"] != "l2-resource-1-1" || submission.Observation.Outputs.Values["generation"] != int64(1) {
		t.Fatal("create output envelope values did not match")
	}
	createStateRaw, _ := h.backend.snapshot()
	createState := requireStateClosure(t, createStateRaw, true)

	provider = h.provisioner()
	observed, err := h.observe(provider, create, "create-observe")
	if err != nil {
		t.Fatalf("real OpenTofu create observation failed: %v", err)
	}
	requireL2Succeeded(t, observed, true)
	afterCreateObserve, _ := h.backend.snapshot()
	if state := requireStateClosure(t, afterCreateObserve, true); state != createState {
		t.Fatal("no-change create observation changed state")
	}

	update := l2Request("l2-update", domain.CapabilityUpdate, 2)
	submission, err = h.submit(provider, update, "update-submit")
	if err != nil {
		t.Fatalf("real OpenTofu update failed: %v", err)
	}
	requireL2Succeeded(t, submission.Observation, true)
	if submission.Observation.Outputs.Values["endpoint"] != "l2-resource-1-2" || submission.Observation.Outputs.Values["generation"] != int64(2) {
		t.Fatal("update output envelope values did not match")
	}
	updateStateRaw, _ := h.backend.snapshot()
	updateState := requireStateClosure(t, updateStateRaw, true)
	if updateState.Lineage != createState.Lineage || updateState.Serial < createState.Serial {
		t.Fatal("update state identity was not monotonic")
	}

	provider = h.provisioner()
	observed, err = h.observe(provider, update, "update-observe")
	if err != nil {
		t.Fatalf("real OpenTofu update observation failed: %v", err)
	}
	requireL2Succeeded(t, observed, true)
	afterUpdateObserve, _ := h.backend.snapshot()
	if state := requireStateClosure(t, afterUpdateObserve, true); state != updateState {
		t.Fatal("no-change update observation changed state")
	}

	deleteRequest := l2Request("l2-delete", domain.CapabilityDelete, 3)
	submission, err = h.submit(provider, deleteRequest, "delete-submit")
	if err != nil {
		t.Fatalf("real OpenTofu delete failed: %v", err)
	}
	requireL2Succeeded(t, submission.Observation, false)
	if submission.Observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatal("delete did not report terminal absence")
	}
	deleteStateRaw, _ := h.backend.snapshot()
	deleteState := requireStateClosure(t, deleteStateRaw, false)
	if len(deleteStateRaw) == 0 || deleteState.Lineage != createState.Lineage || deleteState.Serial < updateState.Serial {
		t.Fatal("delete did not retain monotonic control state")
	}

	provider = h.provisioner()
	observed, err = h.observe(provider, deleteRequest, "delete-observe")
	if err != nil {
		t.Fatalf("real OpenTofu terminal observation failed: %v", err)
	}
	requireL2Succeeded(t, observed, false)
	if observed.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatal("terminal observation did not report absence")
	}

	_, events := h.backend.snapshot()
	var locks, unlocks, lockedUpdates int
	for _, event := range events {
		if event.Path != "/state/"+stateKey && event.Path != "/lock/"+stateKey && event.Path != "/unlock/"+stateKey {
			t.Fatal("backend request used an unstable state key")
		}
		switch event.Method {
		case "LOCK":
			locks++
			if event.LockID == "" {
				t.Fatal("LOCK body omitted its ID")
			}
		case "UNLOCK":
			unlocks++
			if event.LockID == "" {
				t.Fatal("UNLOCK body omitted its ID")
			}
		case http.MethodPost:
			query, parseErr := url.ParseQuery(event.Query)
			if parseErr == nil && query.Get("ID") != "" {
				lockedUpdates++
			}
		}
	}
	if locks == 0 || locks != unlocks || lockedUpdates < 3 {
		t.Fatal("backend lock, unlock, or lock-ID update protocol was incomplete")
	}
	h.backend.assertNoDelete(t)
	h.runner.assertBuiltInOnly(t)
}

func TestL2LockContentionIsNotAttempted(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusLocked} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newL2Harness(t)
			h.backend.prelock(status)
			provider := h.provisioner()
			request := l2Request("l2-contention", domain.CapabilityCreate, 1)
			_, err := h.submit(provider, request, "contention-submit")
			if !errors.Is(err, provisioning.ErrSubmissionNotAttempted) {
				t.Fatalf("lock contention classification: %v", err)
			}
			key := provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber)
			attempt, loadErr := h.evidence.LoadAttempt(context.Background(), key)
			if loadErr != nil || attempt.Phase != AttemptPrepared {
				t.Fatal("lock contention crossed ApplyMayStart")
			}
			h.runner.assertBuiltInOnly(t)
			h.backend.assertNoDelete(t)
		})
	}
}

func TestL2WriteThenResponseLossRecoversOnObserve(t *testing.T) {
	h := newL2Harness(t)
	h.backend.setFault(httpFaultResponse)
	provider := h.provisioner()
	request := l2Request("l2-response-loss", domain.CapabilityCreate, 1)
	_, err := h.submit(provider, request, "response-loss-submit")
	if !errors.Is(err, provisioning.ErrAmbiguousSubmission) || strings.Contains(fmt.Sprint(err), l2Canary) {
		t.Fatalf("response-loss classification: %v", err)
	}
	raw, events := h.backend.snapshot()
	state := requireStateClosure(t, raw, true)
	persistedDigest := sha256.Sum256(raw)
	postCount := 0
	for _, event := range events {
		if event.Method == http.MethodPost {
			postCount++
			if event.BodyDigest != persistedDigest || event.BodyBytes != len(raw) {
				t.Fatal("response-loss retries did not preserve the exact state update")
			}
		}
	}
	if postCount == 0 {
		t.Fatal("response-loss fault did not receive a state update")
	}
	key := provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber)
	attempt, _ := h.evidence.LoadAttempt(context.Background(), key)
	if attempt.Phase != AttemptApplyOutcomeUnknown {
		t.Fatal("response loss did not retain durable ambiguity")
	}

	h.backend.setFault(httpFaultNone)
	provider = h.provisioner()
	observation, err := h.observe(provider, request, "response-loss-observe")
	if err != nil {
		t.Fatalf("response-loss recovery failed: %v", err)
	}
	requireL2Succeeded(t, observation, true)
	binding, loadErr := h.evidence.LoadStateBinding(context.Background(), request.ResourceID)
	if loadErr != nil || binding.State == nil || binding.State.Lineage != state.Lineage || binding.State.Serial != state.Serial {
		t.Fatal("response-loss recovery did not bind the exact persisted state")
	}
	h.backend.assertNoDelete(t)
	h.runner.assertBuiltInOnly(t)
}

func TestL2RejectedStateWriteQuarantinesAndAbsentStateStaysUnknown(t *testing.T) {
	h := newL2Harness(t)
	h.backend.setFault(httpFaultReject)
	provider := h.provisioner()
	request := l2Request("l2-rejected-write", domain.CapabilityCreate, 1)
	_, err := h.submit(provider, request, "rejected-write-submit")
	if !errors.Is(err, provisioning.ErrAmbiguousSubmission) || strings.Contains(fmt.Sprint(err), l2Canary) {
		t.Fatalf("rejected-write classification: %v", err)
	}
	raw, _ := h.backend.snapshot()
	if len(raw) != 0 {
		t.Fatal("rejected state write unexpectedly persisted state")
	}
	if !quarantineContainsErroredState(t, filepath.Join(h.root, "quarantine")) {
		t.Fatal("OpenTofu errored.tfstate was not quarantined")
	}

	h.backend.setFault(httpFaultNone)
	provider = h.provisioner()
	observation, observeErr := h.observe(provider, request, "rejected-write-observe")
	if observeErr != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown || observation.Resource.Presence != provisioning.ResourcePresenceUnknown {
		t.Fatal("state absence after ApplyMayStart was not preserved as Unknown")
	}
	h.backend.assertNoDelete(t)
	h.runner.assertBuiltInOnly(t)
}

func quarantineContainsErroredState(t *testing.T, root string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), "errored.tfstate") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal("inspect OpenTofu quarantine")
	}
	return found
}

func TestL2ApplyProcessInterruptionStaysUnknown(t *testing.T) {
	h := newL2Harness(t)
	h.backend.setFault(httpFaultBlockWrite)
	provider := h.provisioner()
	request := l2Request("l2-interrupted-apply", domain.CapabilityCreate, 1)
	fence := l2Fence("interrupted-submit")
	h.evidence.allow(provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber), fence)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := provider.SubmitFenced(ctx, request, fence)
		result <- err
	}()
	select {
	case <-h.backend.postArrived:
		cancel()
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("real OpenTofu apply did not reach state persistence")
	}
	select {
	case err := <-result:
		if !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
			t.Fatalf("interrupted apply classification: %v", err)
		}
	case <-time.After(gracefulProcessDrain + 5*time.Second):
		t.Fatal("interrupted OpenTofu process did not terminate")
	}
	h.backend.setFault(httpFaultNone)
	raw, _ := h.backend.snapshot()
	if len(raw) != 0 {
		t.Fatal("interrupted state update unexpectedly persisted state")
	}
	provider = h.provisioner()
	observation, err := h.observe(provider, request, "interrupted-observe")
	if err != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown {
		t.Fatal("interrupted apply without state was not Unknown")
	}
	h.backend.assertNoDelete(t)
}

func TestL2BackendOutageIsNotAttempted(t *testing.T) {
	h := newL2Harness(t)
	h.backend.setFault(httpFaultOutage)
	provider := h.provisioner()
	request := l2Request("l2-outage", domain.CapabilityCreate, 1)
	_, err := h.submit(provider, request, "outage-submit")
	if !errors.Is(err, provisioning.ErrSubmissionNotAttempted) {
		t.Fatalf("backend outage classification: %v", err)
	}
	key := provider.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber)
	attempt, loadErr := h.evidence.LoadAttempt(context.Background(), key)
	if loadErr != nil || attempt.Phase != AttemptPrepared {
		t.Fatal("backend outage crossed ApplyMayStart")
	}
	h.backend.assertNoDelete(t)
}

func TestL2ObserveRejectsAlteredSameSerialState(t *testing.T) {
	h := newL2Harness(t)
	provider := h.provisioner()
	request := l2Request("l2-state-conflict", domain.CapabilityCreate, 1)
	submission, err := h.submit(provider, request, "state-conflict-submit")
	if err != nil {
		t.Fatalf("state-conflict setup failed: %v", err)
	}
	requireL2Succeeded(t, submission.Observation, true)
	original, _ := h.backend.snapshot()
	originalEvidence := requireStateClosure(t, original, true)
	var altered map[string]any
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	if decoder.Decode(&altered) != nil {
		t.Fatal("decode state for same-serial conflict")
	}
	outputs, ok := altered["outputs"].(map[string]any)
	if !ok {
		t.Fatal("state outputs unavailable for same-serial conflict")
	}
	canary, ok := outputs["sensitive_canary"].(map[string]any)
	if !ok {
		t.Fatal("sensitive state output unavailable for conflict mutation")
	}
	canary["value"] = "altered-value"
	alteredRaw, marshalErr := json.Marshal(altered)
	if marshalErr != nil {
		t.Fatal("encode same-serial conflict")
	}
	alteredEvidence, parseErr := parseState(alteredRaw)
	if parseErr != nil || alteredEvidence.Lineage != originalEvidence.Lineage || alteredEvidence.Serial != originalEvidence.Serial || alteredEvidence.Digest == originalEvidence.Digest {
		t.Fatal("same-serial conflict fixture was invalid")
	}
	h.backend.replaceState(alteredRaw)

	provider = h.provisioner()
	observation, observeErr := h.observe(provider, request, "state-conflict-observe")
	if observeErr != nil || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown {
		t.Fatal("altered same-serial state was not rejected as Unknown")
	}
	h.backend.assertNoDelete(t)
}

func TestHTTPBackendLockContentionBodies(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusLocked} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			backend := newL2HTTPBackend(t)
			backend.prelock(status)
			_, lockURL, _ := backend.URLs()
			request, _ := http.NewRequest("LOCK", lockURL+"/v1/key", bytes.NewReader([]byte(`{"ID":"challenger"}`)))
			response, err := backend.server.Client().Do(request)
			if err != nil || response.StatusCode != status {
				t.Fatal("backend contention status mismatch")
			}
			var existing struct {
				ID string `json:"ID"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&existing)
			_ = response.Body.Close()
			if decodeErr != nil || existing.ID != "contended-lock" {
				t.Fatal("backend contention body omitted existing lock ID")
			}
		})
	}
}

func TestHTTPBackendStateMetadataContainsOnlyDigest(t *testing.T) {
	backend := newL2HTTPBackend(t)
	backend.prelock(http.StatusConflict)
	stateURL, _, _ := backend.URLs()
	state := []byte(`{"lineage":"metadata-test","serial":1,"private":"state-value-must-not-be-metadata"}`)
	request, _ := http.NewRequest(http.MethodPost, stateURL+"/v1/key?ID=contended-lock", bytes.NewReader(state))
	response, err := backend.server.Client().Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("metadata state update failed")
	}
	_ = response.Body.Close()
	_, events := backend.snapshot()
	encoded, _ := json.Marshal(events)
	if bytes.Contains(encoded, state) || bytes.Contains(encoded, []byte("state-value-must-not-be-metadata")) {
		t.Fatal("state values leaked into backend request metadata")
	}
	expected := sha256.Sum256(state)
	if len(events) != 1 || events[0].BodyBytes != len(state) || events[0].BodyDigest != expected {
		t.Fatal("bounded backend body metadata was incomplete")
	}
	backend.assertNoDelete(t)
}

func TestL2BinaryDigestIsExact(t *testing.T) {
	executable := os.Getenv("LIFTR_TEST_OPENTOFU_BIN")
	if executable == "" {
		t.Skip("LIFTR_TEST_OPENTOFU_BIN is not set")
	}
	digest, err := digestFile(executable, maxExecutableBytes)
	if err != nil || len(digest) != hex.EncodedLen(sha256.Size) {
		t.Fatal("real OpenTofu binary digest was unavailable")
	}
}
