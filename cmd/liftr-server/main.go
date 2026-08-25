// SPDX-License-Identifier: Apache-2.0

// Command liftr-server runs the Liftr control plane. With LIFTR_DATABASE_URL
// configured it composes the full stack — PostgreSQL persistence, the
// registered ResourceType catalog, the Pulumi provisioner adapter, an optional
// operator-configured OpenTofu registration, the HTTP surface, and the outbox
// worker loop. Without it the server runs in a deliberately degraded
// health-only mode.
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
	"sync"
	"syscall"
	"time"

	pgxpool "github.com/jackc/pgx/v5/pgxpool"
	prometheus "github.com/prometheus/client_golang/prometheus"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/observability"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/policy"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/bindings"
	pulumiprovisioner "github.com/sithea-nou/liftr/internal/provisioning/pulumi"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
	"github.com/sithea-nou/liftr/internal/server"
)

const shutdownTimeout = 10 * time.Second

// version is build metadata stamped via -ldflags; it becomes the OTel
// service.version resource attribute.
var version = "dev"

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
	obsConfig, err := observability.LoadConfig()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("configuration invalid", "error", err, "error_class", "configuration")
		os.Exit(1)
	}
	logger := newLogger(obsConfig)
	obsConfig.Logger = logger
	obsConfig.ServiceVersion = version
	telemetry, err := observability.NewTelemetry(context.Background(), obsConfig)
	if err != nil {
		logger.Error("telemetry configuration invalid", "error", err, "error_class", "configuration")
		os.Exit(1)
	}

	addr := os.Getenv("LIFTR_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	var handler http.Handler
	var composed *server.Runtime
	stopWorker := func() {}
	closeStore := func() {}
	databaseURL := os.Getenv("LIFTR_DATABASE_URL")
	adminAddr := os.Getenv("LIFTR_ADMIN_ADDR")
	if adminAddr != "" && databaseURL == "" {
		logger.Error("admin listener requires durable PostgreSQL composition", "error_class", "configuration")
		os.Exit(1)
	}
	if databaseURL != "" {
		runtime, close, composeErr := composeFullRuntime(context.Background(), logger, obsConfig, telemetry)
		if composeErr != nil {
			logger.Error("runtime composition failed", "error", composeErr, "error_class", "configuration")
			os.Exit(1)
		}
		composed = runtime
		closeStore = close
		handler = composed.Handler()

		workerCtx, cancelWorker := context.WithCancel(context.Background())
		defer cancelWorker()
		composed.StartWorker(workerCtx)
		composed.StartOperationalSampler(workerCtx)
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
	var adminServer *http.Server
	if adminAddr != "" {
		adminServer = &http.Server{
			Addr: adminAddr, Handler: composed.AdminHandler(), ReadHeaderTimeout: 5 * time.Second,
		}
	}
	metricsServer := startMetricsListener(logger, telemetry.PrometheusRegistry, obsConfig.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("server starting",
			"address", addr,
			"service_version", version,
			"metrics_addr", obsConfig.MetricsAddr,
		)
		errCh <- httpServer.ListenAndServe()
	}()
	if adminServer != nil {
		go func() {
			logger.Info("admin server starting", "address", adminAddr, "service_version", version)
			errCh <- adminServer.ListenAndServe()
		}()
	}

	select {
	case serveErr := <-errCh:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", serveErr, "error_class", "invariant")
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown requested")
		shutdownInOrder(httpServer, adminServer, metricsServer, composed, stopWorker, telemetry, closeStore, logger)
	}

	if err := telemetry.Shutdown(context.Background()); err != nil {
		logger.Warn("telemetry flush failed after shutdown window", "error_class", "telemetry_export", "error", err.Error())
	}
	logger.Info("server stopped")
}

// shutdownInOrder implements the approved M17 sequence:
//  1. readiness flips false (draining) so load balancers drain first;
//  2. HTTP stops accepting and finishes in-flight requests;
//  3. the metrics listener (if any) stops;
//  4. the worker is canceled and waited on boundedly — leases stay intact so
//     ambiguous Submits recover through existing expiry machinery;
//  5. telemetry flushes boundedly;
//  6. PostgreSQL closes.
//
// Telemetry flush failure never alters persisted lifecycle outcomes.
func shutdownInOrder(httpServer, adminServer, metricsServer *http.Server, draining interface{ SetDraining() }, stopWorker func(), telemetry *observability.Telemetry, closeStore func(), logger *slog.Logger) {
	if draining != nil {
		draining.SetDraining()
		logger.Info("readiness flipped to not-ready (draining)")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var listeners sync.WaitGroup
	shutdownListener := func(name string, server *http.Server) {
		defer listeners.Done()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error(name+" graceful shutdown failed", "error", err, "error_class", "invariant")
			if closeErr := server.Close(); closeErr != nil {
				logger.Warn(name+" forced close failed", "error", closeErr, "error_class", "invariant")
			}
		}
	}
	listeners.Add(1)
	go shutdownListener("public server", httpServer)
	if adminServer != nil {
		listeners.Add(1)
		go shutdownListener("admin server", adminServer)
	}
	listeners.Wait()
	if metricsServer != nil {
		metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer metricsCancel()
		if err := metricsServer.Shutdown(metricsCtx); err != nil {
			logger.Warn("metrics listener did not stop within its window", "error_class", "invariant")
		}
	}
	stopWorker()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := telemetry.Shutdown(flushCtx); err != nil {
		logger.Warn("telemetry flush failed", "error_class", "telemetry_export", "error", err.Error())
	}
	closeStore()
}

func newLogger(config observability.Config) *slog.Logger {
	options := &slog.HandlerOptions{Level: config.SlogLevel()}
	if config.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}

// startMetricsListener serves /metrics on a dedicated operator listener over
// the telemetry registry. Disabled unless LIFTR_METRICS_ADDR is explicitly
// configured; there is no default public exposure (ADR-0018).
func startMetricsListener(logger *slog.Logger, registry *prometheus.Registry, addr string) *http.Server {
	if addr == "" {
		return nil
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("metrics listener starting", "address", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics listener stopped unexpectedly", "error", err, "error_class", "invariant")
			os.Exit(1)
		}
	}()
	return server
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

func composeAdminAuthConfig(apiAuth server.AuthConfig, insecure, enabled bool) (*server.AdminAuthConfig, error) {
	if !enabled {
		return nil, nil
	}
	if insecure {
		return &server.AdminAuthConfig{}, nil
	}
	issuer := os.Getenv("LIFTR_ADMIN_AUTH_ISSUER")
	if issuer == "" {
		issuer = apiAuth.Issuer
	}
	audience := os.Getenv("LIFTR_ADMIN_AUTH_AUDIENCE")
	if audience == "" {
		return nil, fmt.Errorf("LIFTR_ADMIN_AUTH_AUDIENCE is required when LIFTR_ADMIN_ADDR is configured")
	}
	if audience == apiAuth.Audience {
		return nil, fmt.Errorf("LIFTR_ADMIN_AUTH_AUDIENCE must differ from LIFTR_AUTH_AUDIENCE")
	}
	algorithms := apiAuth.Algorithms
	if raw := os.Getenv("LIFTR_ADMIN_AUTH_ALGORITHMS"); raw != "" {
		algorithms = strings.Split(raw, ",")
		for i := range algorithms {
			algorithms[i] = strings.TrimSpace(algorithms[i])
		}
	}
	kindClaim := os.Getenv("LIFTR_ADMIN_AUTH_KIND_CLAIM")
	if kindClaim == "" {
		kindClaim = apiAuth.KindClaim
	}
	return &server.AdminAuthConfig{
		Issuer: issuer, Audience: audience, Algorithms: algorithms,
		KindClaim: kindClaim, GrantsFile: os.Getenv("LIFTR_ADMIN_AUTH_GRANTS_FILE"),
	}, nil
}

// samplerReader adapts the PostgreSQL store onto the server's operational
// reader port, mapping adapter types onto telemetry-neutral sample values.
type samplerReader struct{ store *postgres.Store }

func (s samplerReader) SnapshotOperationalState(ctx context.Context, thresholds server.DiagnosticThresholds) (observability.ClusterSample, error) {
	snapshot, err := s.store.SnapshotOperationalState(ctx, postgres.DiagnosticThresholds{
		LongRunningWarnAfter: thresholds.LongRunningWarnAfter,
		LongRunningCritAfter: thresholds.LongRunningCritAfter,
		SilentAfter:          thresholds.SilentAfter,
	})
	if err != nil {
		return observability.ClusterSample{}, err
	}
	pool := s.store.PoolStats()
	return observability.ClusterSample{
		SampledAt:                        snapshot.SampledAt,
		OutboxPendingDepth:               snapshot.OutboxPendingDepth,
		OutboxPendingOldestAgeSeconds:    snapshot.OutboxPendingOldestAgeSeconds,
		OutboxExpiredLeases:              snapshot.OutboxExpiredLeases,
		OutboxDead:                       snapshot.OutboxDead,
		ActiveOperations:                 snapshot.ActiveOperations,
		ActiveOperationsOldestAgeSeconds: snapshot.ActiveOldestAgeSeconds,
		LongRunningWarning:               snapshot.LongRunningWarningByCapability,
		LongRunningCritical:              snapshot.LongRunningCriticalByCapability,
		ReconciliationSilent:             snapshot.SilentByCapability,
		Pool: observability.PoolStats{
			Acquired:   pool.Acquired,
			Idle:       pool.Idle,
			Connecting: pool.Connecting,
			MaxTotal:   pool.MaxTotal,
		},
	}, nil
}

// composeFullRuntime wires durable persistence, the developer contract
// registry, the private Pulumi binding, and the outbox worker. Authentication
// configuration is mandatory and validated before anything starts; schema
// compatibility is verified before serving (ADR-0018).
func composeFullRuntime(ctx context.Context, logger *slog.Logger, obsConfig observability.Config, telemetry *observability.Telemetry) (*server.Runtime, func(), error) {
	authConfig, insecure, err := composeAuthConfig()
	if err != nil {
		return nil, nil, err
	}
	adminAuth, err := composeAdminAuthConfig(authConfig, insecure, os.Getenv("LIFTR_ADMIN_ADDR") != "")
	if err != nil {
		return nil, nil, err
	}
	store, closeStore, err := openPostgres(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := postgres.VerifySchema(ctx, store.Pool()); err != nil {
		closeStore()
		return nil, nil, fmt.Errorf("schema is not compatible with this build (run liftr-migrate): %w", err)
	}
	logger.Info("schema verified")
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
	admissionPolicy, err := policy.LoadFile(ctx, os.Getenv("LIFTR_POLICY_FILE"), catalog)
	if err != nil {
		closeStore()
		return nil, nil, fmt.Errorf("load platform admission policy: %w", err)
	}
	logger.Info("platform admission policy loaded", "policy_revision", admissionPolicy.Revision())
	providerRef, provider, err := composePulumiProvisioner()
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	kinds := map[application.ProvisionerRef]observability.ProvisionerKind{
		providerRef: observability.ProvisionerKindPulumi,
	}
	provisioners := map[application.ProvisionerRef]provisioning.Provisioner{providerRef: provider}
	routes := map[domain.ResourceTypeRef]application.ProvisionerRef{}
	if openTofuConfigFile := os.Getenv("LIFTR_OPENTOFU_CONFIG_FILE"); openTofuConfigFile != "" {
		openTofuProviders, openTofuRoutes, composeErr := composeOpenTofuProvisioners(ctx, openTofuConfigFile, catalog, store)
		if composeErr != nil {
			closeStore()
			return nil, nil, composeErr
		}
		for openTofuRef, openTofuProvider := range openTofuProviders {
			if _, duplicate := provisioners[openTofuRef]; duplicate {
				closeStore()
				return nil, nil, fmt.Errorf("OpenTofu provisioner reference %q is already registered", openTofuRef)
			}
			provisioners[openTofuRef] = openTofuProvider
			kinds[openTofuRef] = observability.ProvisionerKindOpenTofu
		}
		for resourceType, openTofuRef := range openTofuRoutes {
			routes[resourceType] = openTofuRef
		}
	}
	runtime, err := server.Compose(server.Config{
		Transactions:                store,
		Catalog:                     catalog,
		AdmissionPolicy:             admissionPolicy,
		Provisioners:                provisioners,
		DefaultProvisionerRef:       providerRef,
		ResourceTypeProvisionerRefs: routes,
		Logger:                      logger,
		Telemetry:                   telemetry,
		ProvisionerKinds:            kinds,
		Sampler:                     samplerReader{store: store},
		Thresholds: server.DiagnosticThresholds{
			LongRunningWarnAfter: obsConfig.LongRunningWarnAfter,
			LongRunningCritAfter: obsConfig.LongRunningCritAfter,
			SilentAfter:          obsConfig.ReconciliationSilentAfter,
		},
		Auth:         authConfigOrNil(authConfig, insecure),
		AdminAuth:    adminAuth,
		InsecureAuth: insecure,
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
				OutputMappings: []pulumiprovisioner.OutputMapping{{
					Ref:        "liftr-azure-pg-outputs-v1",
					ExportName: "liftrOutputs",
				}},
				CurrentOutputMappingRef: "liftr-azure-pg-outputs-v1",
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
