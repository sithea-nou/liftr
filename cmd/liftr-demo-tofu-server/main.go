// SPDX-License-Identifier: Apache-2.0

// Command liftr-demo-tofu-server runs a NON-PRODUCTION Liftr control plane
// whose provisioner is the REAL OpenTofu CLI adapter (M19) driving the
// built-in terraform_data resource — no cloud, no provider downloads.
//
// Differences from production composition:
//
//   - One demo-only ResourceType (DemoWorkload/v1 with declared outputs).
//   - The state backend is the sanctioned DEVELOPMENT LOCAL file backend,
//     not an operator-supplied conformant HTTPS HTTP backend. Production
//     never uses this mode.
//   - Insecure development authentication on a loopback listener.
//
// Everything else — durable PostgreSQL evidence, fencing, saved-plan apply
// semantics, quarantine, output publication — is the production adapter.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	pgxpool "github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/server"
)

const (
	defaultAddr        = "127.0.0.1:18180"
	defaultDatabaseURL = "postgres://liftr:liftr@127.0.0.1:55432/liftr_demo_tofu?sslmode=disable"
	shutdownWindow     = 10 * time.Second
	demoWorkerInterval = 150 * time.Millisecond
)

const workloadSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:DemoWorkload:v1:spec",
  "title": "DemoWorkload/v1 ResourceSpec (non-production demo type)",
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "sizeGB"],
  "properties": {
    "name": {
      "type": "string",
      "minLength": 1,
      "description": "Free-form workload label."
    },
    "sizeGB": {
      "type": "integer",
      "minimum": 1,
      "description": "Free-form size label; changes trigger a real re-apply."
    }
  }
}`

const workloadProgram = `variable "liftr" {
  type = any
}

variable "spec" {
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
    endpoint = "${var.liftr.resourceId}-gen${var.liftr.targetGeneration}.demo.liftr.internal"
    name     = try(var.spec.name, "demo")
    sizeGB   = try(var.spec.sizeGB, 1)
  }
}

output "liftr_envelope" {
  value = {
    version          = 1
    mapping          = "demo-tofu-output-v1"
    resourceId       = var.liftr.resourceId
    targetGeneration = var.liftr.targetGeneration
    values = {
      endpoint   = var.desired_present ? terraform_data.workload[0].output.endpoint : ""
      generation = var.liftr.targetGeneration
    }
  }
}
`

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := envOr("LIFTR_ADDR", defaultAddr)
	if err := requireLoopback("LIFTR_ADDR", addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	executable := envOr("LIFTR_DEMO_TOFU_BIN", filepath.Join(os.Getenv("HOME"), ".opentofu", "bin", "tofu"))
	databaseURL := envOr("LIFTR_DEMO_TOFU_DATABASE_URL",
		defaultDatabaseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, closePool, err := openDatabase(ctx, databaseURL, logger)
	if err != nil {
		logger.Error("demo tofu database unavailable; run 'make demo-opentofu' first", "error", err)
		os.Exit(1)
	}
	defer closePool()
	store, err := postgres.NewStore(pool)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}

	workloadType, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "DemoWorkload", Version: "v1"},
		"Non-production demo workload provisioned through the REAL OpenTofu CLI.",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		fatal(logger, "build type", err)
	}
	contract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        workloadType,
		DisplayName: "Demo Workload (real OpenTofu, local only)",
		SpecSchema:  []byte(workloadSchema),
		Transitions: func(_, _ map[string]any) []resourcetypes.TransitionViolation { return nil },
		Outputs: []resourcecontract.OutputField{
			{Name: "endpoint", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
			{Name: "generation", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
		},
	})
	if err != nil {
		fatal(logger, "build contract", err)
	}
	catalog, err := resourcetypes.NewRegistry(contract)
	if err != nil {
		fatal(logger, "register contracts", err)
	}

	executableDigest, err := digestFile(executable)
	if err != nil {
		logger.Error("OpenTofu executable unavailable; install OpenTofu 1.12.6 or set LIFTR_DEMO_TOFU_BIN",
			"path", executable, "error", err)
		os.Exit(1)
	}

	baseDir, err := filepath.Abs(".demo/tofu")
	if err != nil {
		fatal(logger, "resolve work root", err)
	}
	for _, dir := range []string{"work", "quarantine", "state", "source"} {
		if err := os.MkdirAll(filepath.Join(baseDir, dir), 0o700); err != nil {
			fatal(logger, "create demo directories", err)
		}
	}
	sourceDir := filepath.Join(baseDir, "source")
	if err := os.WriteFile(filepath.Join(sourceDir, "main.tf"), []byte(workloadProgram), 0o600); err != nil {
		fatal(logger, "materialize demo program", err)
	}
	sourceDigest, err := opentofu.SourceDigest(sourceDir, opentofu.SourceLimits{
		MaxFiles: 64, MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxPathBytes: 1024})
	if err != nil {
		fatal(logger, "digest demo program", err)
	}

	provisionerRef, err := application.NewProvisionerRef("liftr-demo-tofu-v1")
	if err != nil {
		fatal(logger, "provisioner ref", err)
	}
	provider, err := opentofu.New(opentofu.Config{
		Executable:       executable,
		ExecutableSHA256: executableDigest,
		WorkRoot:         filepath.Join(baseDir, "work"),
		QuarantineRoot:   filepath.Join(baseDir, "quarantine"),
		Evidence:         store,
		LockTimeout:      5 * time.Second,
		Registration: opentofu.Registration{
			ProvisionerRef:  string(provisionerRef),
			Identity:        "liftr-demo-tofu",
			StateKeyVersion: opentofu.StateKeyVersionV1,
			Program: opentofu.Program{
				Ref:          "demo-workload-program-v1",
				ResourceType: domain.ResourceTypeRef{Name: "DemoWorkload", Version: "v1"},
				Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
				SourceDir:    sourceDir, SourceDigest: sourceDigest, BuiltInOnly: true,
				EncodeInput: func(input opentofu.Input) (map[string]any, error) {
					return map[string]any{"spec": input.Spec.Values(), "desired_present": input.DesiredPresent}, nil
				},
				ControlMarkerAddress:     "terraform_data.liftr_control",
				ManagedWorkloadAddresses: []string{"terraform_data.workload[0]"},
				OutputMappings: []opentofu.OutputMapping{{
					Ref:          "demo-tofu-output-v1",
					EnvelopeName: "liftr_envelope",
					Fields:       map[string]string{"endpoint": "endpoint", "generation": "generation"},
				}},
				CurrentOutputMappingRef: "demo-tofu-output-v1",
			},
			Backend: opentofu.BackendProfile{
				Ref:              "demo-local-state-v1",
				DevelopmentLocal: true,
				LocalStateRoot:   filepath.Join(baseDir, "state"),
			},
		},
	})
	if err != nil {
		fatal(logger, "compose real OpenTofu provisioner", err)
	}

	runtime, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{provisionerRef: provider},
		DefaultProvisionerRef: provisionerRef,
		Logger:                logger,
		WorkerInterval:        demoWorkerInterval,
		InsecureAuth:          true,
	})
	if err != nil {
		fatal(logger, "compose runtime", err)
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	runtime.StartWorker(workerCtx)

	api := &http.Server{Addr: addr, Handler: runtime.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = api.ListenAndServe() }()

	fmt.Println("===================================================================")
	fmt.Println(" LIFTR OPENTOFU DEMO SERVER — NON-PRODUCTION")
	fmt.Println("   developer API : http://" + addr + " (insecure dev auth)")
	fmt.Println("   resource type : DemoWorkload/v1 -> REAL 'tofu' CLI (terraform_data)")
	fmt.Println("   executable    : " + executable + " sha256=" + short(executableDigest))
	fmt.Println("   state backend : development local files at " + filepath.Join(baseDir, "state"))
	fmt.Println("===================================================================")

	<-ctx.Done()
	logger.Info("demo shutdown requested")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownWindow)
	defer cancelShutdown()
	_ = api.Shutdown(shutdownCtx)
	cancelWorker()
	select {
	case <-runtime.Done():
	case <-time.After(shutdownWindow):
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func openDatabase(ctx context.Context, databaseURL string, logger *slog.Logger) (*pgxpool.Pool, func(), error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}
	if err := postgres.VerifySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	logger.Info("demo tofu schema migrated and verified")
	return pool, pool.Close, nil
}

func requireLoopback(name, address string) error {
	if os.Getenv("LIFTR_DEMO_ALLOW_REMOTE") == "1" {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid host:port address", name, address)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must be a loopback address in the demo composition (got %q)", name, address)
	}
	return nil
}

func digestFile(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func fatal(logger *slog.Logger, action string, err error) {
	logger.Error(action, "error", err)
	os.Exit(1)
}
