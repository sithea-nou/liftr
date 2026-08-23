// SPDX-License-Identifier: Apache-2.0

// Command liftr-server runs the Liftr control plane. With LIFTR_DATABASE_URL
// configured it composes the full stack — PostgreSQL persistence, the
// registered ResourceType catalog, the Pulumi provisioner adapter, the HTTP
// surface, and the outbox worker loop. Without it the server runs in a
// deliberately degraded health-only mode.
//
// The full runtime requires authentication (ADR-0012): either OIDC access
// token configuration — LIFTR_AUTH_ISSUER and LIFTR_AUTH_AUDIENCE plus
// optional mapping variables — or an explicit LIFTR_AUTH_MODE=insecure
// development opt-in. Missing authentication configuration fails startup;
// it is never silently treated as insecure.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pgxpool "github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/bindings"
	pulumiprovisioner "github.com/sithea-nou/liftr/internal/provisioning/pulumi"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
	"github.com/sithea-nou/liftr/internal/server"
)

const shutdownTimeout = 10 * time.Second

// Composition-level proof: the concrete registry satisfies the consumer-owned
// application catalog port through the neutral resourcecontract vocabulary.
// Neither side imports the other; only this composition imports both.
var _ application.ResourceTypeCatalog = (*resourcetypes.Registry)(nil)

// azureCredentialVariables are the declared child-process environment names
// the reference implementation needs. Names are fixed by composition; values
// are read from the operator-controlled process environment at startup and
// reach only the isolated Pulumi workspace.
var azureCredentialVariables = []string{
	"ARM_SUBSCRIPTION_ID",
	"ARM_TENANT_ID",
	"ARM_CLIENT_ID",
	"ARM_CLIENT_SECRET",
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := os.Getenv("LIFTR_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	var handler http.Handler
	stopWorker := func() {}
	databaseURL := os.Getenv("LIFTR_DATABASE_URL")
	if databaseURL != "" {
		composed, closeStore, composeErr := composeFullRuntime(context.Background(), logger)
		if composeErr != nil {
			logger.Error("runtime composition failed", "error", composeErr)
			os.Exit(1)
		}
		defer closeStore()
		handler = composed.Handler()

		workerCtx, cancelWorker := context.WithCancel(context.Background())
		defer cancelWorker()
		composed.StartWorker(workerCtx)
		stopWorker = func() {
			cancelWorker()
			select {
			case <-composed.Done():
			case <-time.After(shutdownTimeout):
				logger.Warn("worker loop did not stop within the shutdown window")
			}
		}
	} else {
		handler = server.NewHandler()
		logger.Warn("LIFTR_DATABASE_URL is not configured; serving health endpoints only")
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting", "address", addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown requested")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
		stopWorker()
	}

	logger.Info("server stopped")
}

// composeAuthConfig reads authentication configuration from the process
// environment. LIFTR_AUTH_MODE=insecure is the only non-default mode; it
// composes the explicit development-only principal and allow-all policy.
func composeAuthConfig() (server.AuthConfig, bool, error) {
	if os.Getenv("LIFTR_AUTH_MODE") != "" && os.Getenv("LIFTR_AUTH_MODE") != "insecure" {
		return server.AuthConfig{}, false, fmt.Errorf("LIFTR_AUTH_MODE %q is not supported; only \"insecure\"", os.Getenv("LIFTR_AUTH_MODE"))
	}
	insecure := os.Getenv("LIFTR_AUTH_MODE") == "insecure"
	issuer := os.Getenv("LIFTR_AUTH_ISSUER")
	audience := os.Getenv("LIFTR_AUTH_AUDIENCE")
	if insecure {
		if issuer != "" || audience != "" {
			return server.AuthConfig{}, false, fmt.Errorf("LIFTR_AUTH_MODE=insecure cannot be combined with OIDC configuration")
		}
		return server.AuthConfig{}, true, nil
	}
	if issuer == "" || audience == "" {
		return server.AuthConfig{}, false, fmt.Errorf("authentication configuration is required: set LIFTR_AUTH_ISSUER and LIFTR_AUTH_AUDIENCE, or explicitly set LIFTR_AUTH_MODE=insecure for development")
	}
	var algorithms []string
	if raw := os.Getenv("LIFTR_AUTH_ALGORITHMS"); raw != "" {
		algorithms = strings.Split(raw, ",")
		for i := range algorithms {
			algorithms[i] = strings.TrimSpace(algorithms[i])
		}
	}
	config := server.AuthConfig{
		Issuer:      issuer,
		Audience:    audience,
		Algorithms:  algorithms,
		GroupClaim:  os.Getenv("LIFTR_AUTH_GROUP_CLAIM"),
		GroupPrefix: os.Getenv("LIFTR_AUTH_GROUP_PREFIX"),
		KindClaim:   os.Getenv("LIFTR_AUTH_KIND_CLAIM"),
		GrantsFile:  os.Getenv("LIFTR_AUTH_GRANTS_FILE"),
	}
	return config, false, nil
}

// composeFullRuntime wires durable persistence, the developer contract
// registry, the private Pulumi binding, and the outbox worker. Authentication
// configuration is mandatory and validated before anything starts.
func composeFullRuntime(ctx context.Context, logger *slog.Logger) (*server.Runtime, func(), error) {
	authConfig, insecure, err := composeAuthConfig()
	if err != nil {
		return nil, nil, err
	}
	store, closeStore, err := openPostgres(ctx)
	if err != nil {
		return nil, nil, err
	}
	contractV1, err := postgresqldatabase.Contract()
	if err != nil {
		closeStore()
		return nil, nil, fmt.Errorf("register PostgreSQLDatabase/v1 contract: %w", err)
	}
	contractV2, err := postgresqldatabase.ContractV2()
	if err != nil {
		closeStore()
		return nil, nil, fmt.Errorf("register PostgreSQLDatabase/v2 contract: %w", err)
	}
	catalog, err := resourcetypes.NewRegistry(contractV1, contractV2)
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	providerRef, provider, err := composePulumiProvisioner()
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	runtime, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{providerRef: provider},
		DefaultProvisionerRef: providerRef,
		Logger:                logger,
		Auth:                  authConfigOrNil(authConfig, insecure),
		InsecureAuth:          insecure,
	})
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	return runtime, closeStore, nil
}

// authConfigOrNil keeps Compose's exactly-one-mode contract: insecure mode
// passes no AuthConfig and sets InsecureAuth instead.
func authConfigOrNil(config server.AuthConfig, insecure bool) *server.AuthConfig {
	if insecure {
		return nil
	}
	return &config
}

func openPostgres(ctx context.Context) (*postgres.Store, func(), error) {
	pool, err := pgxpool.New(ctx, os.Getenv("LIFTR_DATABASE_URL"))
	if err != nil {
		return nil, nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return store, pool.Close, nil
}

func composePulumiProvisioner() (application.ProvisionerRef, provisioning.Provisioner, error) {
	root := requireEnv("LIFTR_PULUMI_ROOT")
	goExecutable := requireEnv("LIFTR_PULUMI_GO_EXECUTABLE")
	backendDir := requireEnv("LIFTR_PULUMI_BACKEND_DIR")
	workspaceDir := requireEnv("LIFTR_PULUMI_WORKSPACE_DIR")
	identity := requireEnv("LIFTR_PULUMI_IDENTITY")
	namespace := requireEnv("LIFTR_PULUMI_NAMESPACE")
	sourceDir := requireEnv("LIFTR_PULUMI_PROGRAM_DIR")
	backendURL := (&url.URL{Scheme: "file", Path: filepath.Clean(backendDir)}).String()
	sourceDigest, err := pulumiprovisioner.SourceDigest(sourceDir)
	if err != nil {
		return "", nil, fmt.Errorf("digest registered Pulumi source: %w", err)
	}
	platform := bindings.PostgresPlatform{
		Location:             requireEnv("LIFTR_PG_LOCATION"),
		SkuName:              requireEnv("LIFTR_PG_SKU_NAME"),
		SkuTier:              requireEnv("LIFTR_PG_SKU_TIER"),
		HighAvailabilityMode: requireEnv("LIFTR_PG_HA_MODE"),
		AdministratorLogin:   requireEnv("LIFTR_PG_ADMIN_LOGIN"),
	}
	config := pulumiprovisioner.Config{
		Identity: identity, StackNamingVersion: pulumiprovisioner.StackNamingVersionV1,
		PulumiRoot: root, GoExecutable: goExecutable, BackendURL: backendURL,
		StackNamespace: namespace, WorkspaceRoot: workspaceDir,
		HistoryPageSize: 50, HistoryMaximumPages: 20, StaleWorkspaceAge: time.Hour,
		Environment: supplyDeclaredEnvironment(azureCredentialVariables),
		Programs: []pulumiprovisioner.Program{
			{
				// v1 keeps its released spec-only contract: no output
				// mapping is registered, so no extraction ever runs for it.
				ResourceType: postgresqldatabase.TypeRef(),
				Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
				ProjectName:  "liftr-postgresqldatabase",
				SourceDir:    sourceDir, SourceDigest: sourceDigest,
				RequiredEnvironment:     azureCredentialVariables,
				EncodeInput:             bindings.PostgresEncoder(identity, namespace, platform),
				SecretInputsUnsupported: true,
			},
			{
				// v2 adds the declared non-secret output contract; the same
				// reference program source serves both identities while
				// infrastructure naming keeps the stacks separate.
				ResourceType: postgresqldatabase.V2TypeRef(),
				Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
				ProjectName:  "liftr-postgresqldatabase",
				SourceDir:    sourceDir, SourceDigest: sourceDigest,
				RequiredEnvironment:     azureCredentialVariables,
				EncodeInput:             bindings.PostgresEncoder(identity, namespace, platform),
				SecretInputsUnsupported: true,
				Outputs: &pulumiprovisioner.OutputMapping{
					Ref:        "liftr-azure-pg-outputs-v1",
					ExportName: "liftrOutputs",
				},
			},
		},
	}
	provider, err := pulumiprovisioner.New(config)
	if err != nil {
		return "", nil, err
	}
	ref, err := application.NewProvisionerRef("liftr-pulumi-" + string(pulumiprovisioner.StackNamingVersionV1))
	if err != nil {
		return "", nil, err
	}
	return ref, provider, nil
}

// supplyDeclaredEnvironment reads only the declared variable names from the
// process environment. Undeclared ambient values never reach Pulumi.
func supplyDeclaredEnvironment(names []string) pulumiprovisioner.EnvironmentProvider {
	return func(context.Context) (map[string]string, error) {
		values := make(map[string]string, len(names)+1)
		if passphrase := os.Getenv("PULUMI_CONFIG_PASSPHRASE"); passphrase != "" {
			values["PULUMI_CONFIG_PASSPHRASE"] = passphrase
		}
		for _, name := range names {
			if value := os.Getenv(name); value != "" {
				values[name] = value
			}
		}
		return values, nil
	}
}

func requireEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fmt.Fprintf(os.Stderr, "environment variable %s is required\n", name)
		os.Exit(1)
	}
	return value
}
