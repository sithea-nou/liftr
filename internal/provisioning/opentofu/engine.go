// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

type assembledCall struct {
	workspace   *workspace
	dir         string
	env         []string
	backendFile string
	varsFile    string
	planFile    string
}

func (p *Provisioner) assemble(ctx context.Context, input Input) (*assembledCall, error) {
	if err := validateDependencySupplyChain(p.config.program); err != nil {
		return nil, fmt.Errorf("OpenTofu dependency supply chain changed")
	}
	work, err := newWorkspace(p.config.WorkRoot, p.config.QuarantineRoot)
	if err != nil {
		return nil, err
	}
	dir, err := copySource(p.config.program.SourceDir, work, sourceLimits(p.config.program), p.config.program.SourceDigest)
	if err != nil {
		work.quarantine()
		_ = work.close()
		return nil, err
	}
	values, err := p.config.program.EncodeInput(input)
	if err != nil {
		_ = work.close()
		return nil, fmt.Errorf("encode OpenTofu input")
	}
	if values == nil {
		values = map[string]any{}
	}
	if _, exists := values["liftr"]; exists {
		_ = work.close()
		return nil, fmt.Errorf("input encoder used reserved Liftr variable")
	}
	values["liftr"] = markerValue(input)
	encoded, err := json.Marshal(values)
	if err != nil {
		_ = work.close()
		return nil, fmt.Errorf("encode OpenTofu variables")
	}
	varsFile := filepath.Join(work.path, "liftr.tfvars.json")
	if err := writePrivateFile(varsFile, encoded, 0o600); err != nil {
		_ = work.close()
		return nil, err
	}
	stateKey := p.stateKey(input.ResourceID)
	backendType, backendConfig, err := p.backendConfiguration(stateKey)
	if err != nil {
		_ = work.close()
		return nil, err
	}
	backendDeclaration := []byte("terraform {\n  backend \"" + backendType + "\" {}\n}\n")
	if err := writePrivateFile(filepath.Join(dir, "liftr.generated.backend.tf"), backendDeclaration, 0o600); err != nil {
		_ = work.close()
		return nil, err
	}
	backendFile := filepath.Join(work.path, "backend.hcl")
	if err := writePrivateFile(backendFile, []byte(backendConfig), 0o600); err != nil {
		_ = work.close()
		return nil, err
	}
	cliConfig := cliConfiguration(p.config.program)
	if p.config.program.BuiltInOnly {
		emptyMirror := filepath.Join(work.path, "provider-mirror")
		if err := os.Mkdir(emptyMirror, 0o700); err != nil {
			_ = work.close()
			return nil, err
		}
		cliConfig = builtInCLIConfiguration(emptyMirror)
	}
	cliFile := filepath.Join(work.path, "tofurc")
	if err := writePrivateFile(cliFile, []byte(cliConfig), 0o600); err != nil {
		_ = work.close()
		return nil, err
	}
	environment, err := p.environment(ctx, cliFile, filepath.Join(work.path, "tmp"))
	if err != nil {
		_ = work.close()
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(work.path, "tmp"), 0o700); err != nil {
		_ = work.close()
		return nil, err
	}
	return &assembledCall{workspace: work, dir: dir, env: environment, backendFile: backendFile, varsFile: varsFile, planFile: filepath.Join(work.path, "saved.tfplan")}, nil
}

func markerValue(input Input) map[string]any {
	return map[string]any{
		"resourceId": string(input.ResourceID), "operationId": string(input.OperationID), "attemptNumber": input.AttemptNumber,
		"targetGeneration": input.TargetGeneration, "capability": string(input.Capability), "desiredPresent": input.DesiredPresent,
	}
}

func (p *Provisioner) backendConfiguration(stateKey string) (string, string, error) {
	b := p.config.Registration.Backend
	if b.DevelopmentLocal {
		path := filepath.Join(b.LocalStateRoot, filepath.FromSlash(stateKey), "terraform.tfstate")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", err
		}
		return "local", "path = " + strconv.Quote(path) + "\n", nil
	}
	state, err := deriveBackendURL(b.StateURL, stateKey)
	if err != nil {
		return "", "", err
	}
	lock, err := deriveBackendURL(b.LockURL, stateKey)
	if err != nil {
		return "", "", err
	}
	unlock, err := deriveBackendURL(b.UnlockURL, stateKey)
	if err != nil {
		return "", "", err
	}
	return "http", "address = " + strconv.Quote(state) + "\nlock_address = " + strconv.Quote(lock) + "\nunlock_address = " + strconv.Quote(unlock) + "\nlock_method = \"LOCK\"\nunlock_method = \"UNLOCK\"\n", nil
}

func deriveBackendURL(base, stateKey string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + stateKey
	u.RawPath = ""
	return u.String(), nil
}

func cliConfiguration(program Program) string {
	addresses := make([]string, 0, len(program.ProviderConstraints))
	for address := range program.ProviderConstraints {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	patterns := make([]string, len(addresses))
	for index, address := range addresses {
		patterns[index] = strconv.Quote(address)
	}
	return "disable_checkpoint = true\nprovider_installation {\n  filesystem_mirror {\n    path = " + strconv.Quote(program.ProviderMirror) + "\n    include = [" + strings.Join(patterns, ", ") + "]\n  }\n}\n"
}

func builtInCLIConfiguration(emptyMirror string) string {
	return "disable_checkpoint = true\nprovider_installation {\n  filesystem_mirror {\n    path = " + strconv.Quote(emptyMirror) + "\n  }\n}\n"
}

func (p *Provisioner) environment(ctx context.Context, cliFile, tempDir string) ([]string, error) {
	provided := map[string]string{}
	if p.config.Registration.Environment != nil {
		var err error
		provided, err = p.config.Registration.Environment(ctx)
		if err != nil {
			return nil, transientError("EnvironmentUnavailable")
		}
	}
	required := append([]string(nil), p.config.program.RequiredEnvironment...)
	required = append(required, p.config.Registration.Backend.RequiredEnvironment...)
	additional := map[string]string{}
	for _, name := range required {
		value, ok := provided[name]
		if !ok {
			return nil, fmt.Errorf("required OpenTofu environment is unavailable")
		}
		additional[name] = value
	}
	return baseEnvironment(p.config.Executable, cliFile, tempDir, additional), nil
}

func baseEnvironment(executable, cliFile, tempDir string, additional ...map[string]string) []string {
	values := map[string]string{
		"PATH": filepath.Dir(executable), "HOME": tempDir, "TMPDIR": tempDir, "TMP": tempDir, "TEMP": tempDir,
		"TF_IN_AUTOMATION": "1", "TF_INPUT": "0", "CHECKPOINT_DISABLE": "1", "USER": "liftr", "LOGNAME": "liftr",
	}
	if cliFile != "" {
		values["TF_CLI_CONFIG_FILE"] = cliFile
	}
	for _, extras := range additional {
		for key, value := range extras {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (p *Provisioner) command(ctx context.Context, call *assembledCall, args ...string) (CommandResult, error) {
	for _, arg := range args {
		if arg == "-lock=false" || strings.Contains(arg, "force-unlock") || arg == "import" || arg == "state" && len(args) > 1 && args[1] != "pull" {
			return CommandResult{}, fmt.Errorf("prohibited OpenTofu command")
		}
	}
	return p.config.Runner.Run(ctx, Command{Path: p.config.Executable, Args: args, Env: call.env, Dir: call.dir, MaxOutputBytes: p.config.MaxCommandOutput})
}

func (p *Provisioner) init(ctx context.Context, call *assembledCall) error {
	result, err := p.command(ctx, call, "init", "-json", "-input=false", "-lockfile=readonly", "-backend-config="+call.backendFile)
	if err != nil || result.ExitCode != 0 || result.Overflow {
		if result.Failure == CommandFailureDeterministic || deterministicMachineFailure(result.Stdout) {
			return fmt.Errorf("OpenTofu initialization was deterministically rejected")
		}
		return transientError("InitializationUnavailable")
	}
	if err := validateMachineUI(result.Stdout); err != nil {
		return fmt.Errorf("OpenTofu initialization protocol is invalid")
	}
	return nil
}

func (p *Provisioner) plan(ctx context.Context, call *assembledCall, output string) (CommandResult, error) {
	args := []string{"plan", "-json", "-input=false", "-detailed-exitcode", "-lock-timeout=" + p.config.LockTimeout.String(), "-var-file=" + call.varsFile}
	if output != "" {
		args = append(args, "-out="+output)
	}
	result, err := p.command(ctx, call, args...)
	if err != nil || result.Overflow || (result.ExitCode != 0 && result.ExitCode != 2) {
		if result.Failure == CommandFailureDeterministic || deterministicMachineFailure(result.Stdout) {
			return result, fmt.Errorf("OpenTofu plan was deterministically rejected")
		}
		return result, transientError("PlanningUnavailable")
	}
	if err := validateMachineUI(result.Stdout); err != nil {
		return result, fmt.Errorf("OpenTofu planning protocol is invalid")
	}
	return result, nil
}

func deterministicMachineFailure(raw []byte) bool {
	known := map[string]bool{
		"Invalid value for input variable":       true,
		"Invalid value for variable":             true,
		"No value for required variable":         true,
		"Invalid function argument":              true,
		"Invalid reference":                      true,
		"Reference to undeclared resource":       true,
		"Reference to undeclared input variable": true,
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 1024), 1<<20)
	found := false
	for scanner.Scan() {
		var event struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "diagnostic" || event.Diagnostic.Severity != "error" {
			continue
		}
		if !known[event.Diagnostic.Summary] {
			return false
		}
		found = true
	}
	return scanner.Err() == nil && found
}

func validateMachineUI(raw []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 1024), 1<<20)
	first := true
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			if first {
				return fmt.Errorf("invalid machine UI event")
			}
			// OpenTofu 1.12.6 init can emit untyped informational lines after
			// the version event. They carry no protocol evidence.
			continue
		}
		if first {
			first = false
			if event["type"] != "version" || event["tofu"] != EngineVersion {
				return fmt.Errorf("invalid machine UI version event")
			}
			ui, ok := event["ui"].(string)
			if !ok || !strings.HasPrefix(ui, "1.") {
				return fmt.Errorf("unsupported machine UI version")
			}
		}
	}
	if scanner.Err() != nil || first {
		return fmt.Errorf("machine UI version event is missing")
	}
	return nil
}

type planDocument struct {
	FormatVersion   string `json:"format_version"`
	ResourceChanges []struct {
		Address         string `json:"address"`
		PreviousAddress string `json:"previous_address"`
		Mode            string `json:"mode"`
		Deposed         string `json:"deposed"`
		Change          struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`
	PlannedValues struct {
		RootModule *planModule `json:"root_module"`
	} `json:"planned_values"`
}

type planModule struct {
	Resources []struct {
		Address string         `json:"address"`
		Mode    string         `json:"mode"`
		Values  map[string]any `json:"values"`
	} `json:"resources"`
	ChildModules []planModule `json:"child_modules"`
}

func decodePlan(raw []byte) (planDocument, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document planDocument
	if decoder.Decode(&document) != nil || document.FormatVersion == "" || !strings.HasPrefix(document.FormatVersion, "1.") || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return planDocument{}, fmt.Errorf("saved plan JSON is invalid")
	}
	return document, nil
}

func validatePlan(document planDocument, program Program, input Input, noChanges bool) error {
	registeredWorkload := make(map[string]bool, len(program.ManagedWorkloadAddresses))
	for _, address := range program.ManagedWorkloadAddresses {
		registeredWorkload[address] = true
	}
	seenChanges := map[string]bool{}
	for _, change := range document.ResourceChanges {
		if change.Mode != "managed" {
			continue
		}
		if change.Address == "" || seenChanges[change.Address] || change.Deposed != "" || change.PreviousAddress != "" {
			return fmt.Errorf("saved plan contains unexpected managed change metadata")
		}
		seenChanges[change.Address] = true
		workload := registeredWorkload[change.Address]
		if change.Address != program.ControlMarkerAddress && !workload {
			return fmt.Errorf("saved plan contains an unregistered managed change")
		}
		actions := strings.Join(change.Change.Actions, ",")
		allowed := actions == "no-op" || actions == "create" || actions == "update"
		if workload {
			allowed = allowed || actions == "delete" || actions == "delete,create" || actions == "create,delete"
			if !input.DesiredPresent {
				allowed = actions == "delete" || actions == "no-op"
			}
		}
		if !allowed {
			return fmt.Errorf("saved plan contains an unsupported action")
		}
		if noChanges && actions != "no-op" {
			return fmt.Errorf("verification plan contains changes")
		}
	}
	addresses := map[string]map[string]any{}
	if !collectPlanned(document.PlannedValues.RootModule, addresses) {
		return fmt.Errorf("saved plan contains duplicated managed addresses")
	}
	expected := map[string]bool{program.ControlMarkerAddress: true}
	if input.DesiredPresent {
		for _, address := range program.ManagedWorkloadAddresses {
			expected[address] = true
		}
	}
	if len(addresses) != len(expected) {
		return fmt.Errorf("saved plan managed binding closure does not match registration")
	}
	for address := range addresses {
		if !expected[address] {
			return fmt.Errorf("saved plan managed binding closure does not match registration")
		}
	}
	if !sameJSONValue(addresses[program.ControlMarkerAddress]["input"], markerValue(input)) {
		return fmt.Errorf("saved plan control marker does not match request")
	}
	return nil
}

func collectPlanned(module *planModule, result map[string]map[string]any) bool {
	if module == nil {
		return true
	}
	for _, resource := range module.Resources {
		if resource.Mode == "managed" {
			if _, found := result[resource.Address]; resource.Address == "" || found {
				return false
			}
			result[resource.Address] = resource.Values
		}
	}
	for i := range module.ChildModules {
		if !collectPlanned(&module.ChildModules[i], result) {
			return false
		}
	}
	return true
}

func sameJSONValue(left, right any) bool {
	l, err1 := json.Marshal(left)
	r, err2 := json.Marshal(right)
	if err1 != nil || err2 != nil {
		return false
	}
	var lv, rv any
	ld := json.NewDecoder(bytes.NewReader(l))
	ld.UseNumber()
	rd := json.NewDecoder(bytes.NewReader(r))
	rd.UseNumber()
	return ld.Decode(&lv) == nil && rd.Decode(&rv) == nil && fmt.Sprint(lv) == fmt.Sprint(rv)
}

func parseState(raw []byte) (StateEvidence, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var state struct {
		Lineage string      `json:"lineage"`
		Serial  json.Number `json:"serial"`
	}
	if decoder.Decode(&state) != nil || state.Lineage == "" || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return StateEvidence{}, fmt.Errorf("OpenTofu state is invalid")
	}
	serial, err := strconv.ParseUint(state.Serial.String(), 10, 64)
	if err != nil {
		return StateEvidence{}, fmt.Errorf("OpenTofu state serial is invalid")
	}
	return StateEvidence{Lineage: state.Lineage, Serial: serial, Digest: sha256.Sum256(raw)}, nil
}

func desiredPresent(capability domain.Capability) bool { return capability != domain.CapabilityDelete }
