// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/observability"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/worker"
)

// Runtime is a fully wired Liftr process: an HTTP handler over an
// application service backed by durable transactions, plus the outbox worker
// that drives provisioning work.
type Runtime struct {
	handler        http.Handler
	service        *application.Service
	worker         *worker.Worker
	logger         *slog.Logger
	workerInterval time.Duration
	telemetry      *observability.Telemetry
	sampler        OperationalReader
	sampleInterval time.Duration
	thresholds     DiagnosticThresholds

	draining atomic.Bool

	stopOnce sync.Once
	done     chan struct{}
}

// DiagnosticThresholds carries the configurable stuck-candidate thresholds.
// They drive diagnostic gauges only and never influence lifecycle state.
type DiagnosticThresholds struct {
	LongRunningWarnAfter time.Duration
	LongRunningCritAfter time.Duration
	SilentAfter          time.Duration
}

// OperationalReader supplies one bounded cluster-global sample of durable
// truth. It is defined here as a consumer port; the PostgreSQL adapter in the
// server binary satisfies it through a small composition wrapper. The sampler
// never runs inside a request or a scrape.
type OperationalReader interface {
	SnapshotOperationalState(ctx context.Context, thresholds DiagnosticThresholds) (observability.ClusterSample, error)
}

// AuthConfig carries the secured-runtime authentication configuration: one
// RFC 9068 OIDC issuer plus the claim-to-membership mapping (ADR-0012).
type AuthConfig struct {
	Issuer      string
	Audience    string
	Algorithms  []string
	GroupClaim  string
	GroupPrefix string
	KindClaim   string
	// GrantsFile optionally points at the static subject/group → owner
	// grants JSON document.
	GrantsFile string
	// Clock, Skew, JWKSRefreshEvery, and HTTPClient are test hooks. Leave
	// them zero/nil in production composition.
	Clock            func() time.Time
	Skew             time.Duration
	JWKSRefreshEvery time.Duration
	// HTTPClient overrides the bounded metadata client for deterministic
	// tests only.
	HTTPClient *http.Client
}

// Config carries the composition dependencies. Transactions must be durable
// in production; tests may supply deterministic fakes. Authentication is
// mandatory for the full runtime: either Auth is configured or InsecureAuth
// is explicitly opted into — never neither (ADR-0012).
type Config struct {
	Transactions application.TransactionRunner
	Catalog      application.ResourceTypeCatalog
	// AdmissionPolicy is one immutable process-lifetime policy revision. Nil
	// means the built-in no-restrictions policy.
	AdmissionPolicy application.AdmissionPolicy
	// Provisioners maps private ProvisionerRef values to provisioners.
	Provisioners map[application.ProvisionerRef]provisioning.Provisioner
	// DefaultProvisionerRef is selected for new Resources.
	DefaultProvisionerRef application.ProvisionerRef
	// ResourceTypeProvisionerRefs privately overrides the default for new
	// Resources of a registered ResourceType. Existing Resources always use
	// their persisted ProvisionerRef through the resolver.
	ResourceTypeProvisionerRefs map[domain.ResourceTypeRef]application.ProvisionerRef
	// WorkerInterval spaces out idle polling of the outbox. It does not
	// affect in-flight work; long provider calls run under renewed leases.
	WorkerInterval time.Duration
	Logger         *slog.Logger
	// Telemetry optionally instruments transactions, provisioners, worker,
	// and transport. Nil keeps behavior identical without telemetry.
	Telemetry *observability.Telemetry
	// ProvisionerKinds maps each registered provisioner reference onto its
	// bounded software-defined kind used as a metric dimension. Required when
	// Telemetry is set; arbitrary refs never become metric labels.
	ProvisionerKinds map[application.ProvisionerRef]observability.ProvisionerKind
	// Sampler supplies cluster-global operational snapshots. Nil disables the
	// operational sampler.
	Sampler OperationalReader
	// SampleInterval spaces operational samples; zero means 15s.
	SampleInterval time.Duration
	// Thresholds configures long-running and reconciliation-silence
	// diagnostics.
	Thresholds DiagnosticThresholds

	// Auth configures secured JWT access-token authentication. Exactly one
	// of Auth and InsecureAuth must be provided.
	Auth *AuthConfig
	// InsecureAuth opts into development-only composition: a fixed
	// development principal and allow-all authorization. It is never
	// inferred from missing configuration and always logs a prominent
	// warning.
	InsecureAuth bool
}

// Compose wires the process-level object graph without starting any work.
func Compose(config Config) (*Runtime, error) {
	if config.Transactions == nil || config.Catalog == nil {
		return nil, fmt.Errorf("%w: transactions and catalog are required", application.ErrInvalidApplicationCall)
	}
	if len(config.Provisioners) == 0 {
		return nil, fmt.Errorf("%w: at least one provisioner is required", application.ErrInvalidApplicationCall)
	}
	defaultRef := config.DefaultProvisionerRef
	if defaultRef == "" {
		return nil, fmt.Errorf("%w: default provisioner reference is required", application.ErrInvalidApplicationCall)
	}
	if _, ok := config.Provisioners[defaultRef]; !ok {
		return nil, fmt.Errorf("%w: default provisioner %q is not registered", application.ErrInvalidApplicationCall, defaultRef)
	}
	routes, routeCapabilities, err := validateProvisionerRoutes(config)
	if err != nil {
		return nil, err
	}
	authenticator, authorizer, err := composeAuth(config)
	if err != nil {
		return nil, err
	}
	interval := config.WorkerInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	sampleInterval := config.SampleInterval
	if sampleInterval <= 0 {
		sampleInterval = observability.DefaultSampleInterval
	}
	if config.Telemetry != nil {
		if len(config.ProvisionerKinds) != len(config.Provisioners) {
			return nil, fmt.Errorf("%w: every registered provisioner needs a code-defined kind for metrics", application.ErrInvalidApplicationCall)
		}
		for ref := range config.Provisioners {
			if !config.ProvisionerKinds[ref].Valid() {
				return nil, fmt.Errorf("%w: provisioner %q has no valid metric kind", application.ErrInvalidApplicationCall, string(ref))
			}
		}
	}
	transactions := config.Transactions
	if config.Telemetry != nil {
		wrapped, wrapErr := observability.InstrumentTransactions(config.Transactions, config.Telemetry)
		if wrapErr != nil {
			return nil, wrapErr
		}
		transactions = wrapped
	}
	instrumented := make(map[application.ProvisionerRef]provisioning.Provisioner, len(config.Provisioners))
	for ref, provider := range config.Provisioners {
		if config.Telemetry != nil {
			wrapped, wrapErr := observability.InstrumentProvisioner(provider, config.ProvisionerKinds[ref], config.Telemetry)
			if wrapErr != nil {
				return nil, wrapErr
			}
			provider = wrapped
		}
		instrumented[ref] = provider
	}
	admissionPolicy := config.AdmissionPolicy
	if admissionPolicy == nil {
		admissionPolicy = application.NoRestrictionsAdmissionPolicy{}
	}
	if config.Telemetry != nil {
		wrapped, wrapErr := observability.InstrumentAdmissionPolicy(admissionPolicy, config.Telemetry)
		if wrapErr != nil {
			return nil, wrapErr
		}
		admissionPolicy = wrapped
	}
	service, err := application.NewService(config.Catalog, staticSelector{defaultRef: defaultRef, routes: routes, capabilities: routeCapabilities}, staticResolver{providers: instrumented}, transactions, authorizer, admissionPolicy)
	if err != nil {
		return nil, err
	}
	instance, err := worker.NewWithCatalog(transactions, staticResolver{providers: instrumented}, config.Catalog)
	if err != nil {
		return nil, err
	}
	instance.RetryBase = interval
	instance.Telemetry = workerTelemetry(config.Telemetry)
	runtime := &Runtime{service: service, worker: instance, logger: logger, workerInterval: interval,
		telemetry: config.Telemetry, sampler: config.Sampler, sampleInterval: sampleInterval,
		thresholds: config.Thresholds, done: make(chan struct{})}
	runtime.handler = apihttp.NewHandler(apihttp.Deps{Service: service, Auth: authenticator, Logger: logger,
		Telemetry: config.Telemetry, Draining: runtime.IsDraining})
	return runtime, nil
}

// IsDraining reports whether graceful shutdown has begun; readiness answers
// 503 while true (ADR-0018).
func (r *Runtime) IsDraining() bool { return r.draining.Load() }

// workerTelemetry adapts the observability sink onto the worker port without
// making the worker package depend on telemetry.
func workerTelemetry(tel *observability.Telemetry) worker.TelemetrySink {
	if tel == nil {
		return nil
	}
	return tel
}

// composeAuth resolves exactly one authentication mode. The full runtime
// refuses to start without explicit authentication configuration — silently
// keeping today's open behavior is rejected by ADR-0012.
func composeAuth(config Config) (apihttp.Authenticator, application.Authorizer, error) {
	switch {
	case config.InsecureAuth && config.Auth != nil:
		return nil, nil, fmt.Errorf("auth mode conflict: LIFTR_AUTH_MODE=insecure cannot be combined with OIDC configuration")
	case config.InsecureAuth:
		logger := config.Logger
		if logger != nil {
			logger.Warn("INSECURE MODE: Liftr is running WITHOUT authentication and authorization; "+
				"every caller acts as a fixed development principal with allow-all permissions. "+
				"Never expose this instance beyond local development.",
				"mode", "insecure")
		}
		return newInsecureAuthenticator(), allowAllAuthorizer{}, nil
	case config.Auth == nil:
		return nil, nil, fmt.Errorf("authentication configuration is required: set the auth issuer and audience, or explicitly opt into LIFTR_AUTH_MODE=insecure")
	default:
		grants, err := auth.LoadStaticGrants(config.Auth.GrantsFile)
		if err != nil {
			return nil, nil, err
		}
		oidcAuthenticator, err := auth.NewOIDCAuthenticator(context.Background(), auth.Config{
			Issuer:     config.Auth.Issuer,
			Audience:   config.Auth.Audience,
			Algorithms: config.Auth.Algorithms,
			KindClaim:  config.Auth.KindClaim,
			Mapper: auth.ClaimMapper{
				GroupClaim: config.Auth.GroupClaim,
				Prefix:     config.Auth.GroupPrefix,
				Grants:     grants,
			},
			ClockSkew:        config.Auth.Skew,
			JWKSRefreshEvery: config.Auth.JWKSRefreshEvery,
			HTTPClient:       config.Auth.HTTPClient,
			Observers:        authObservers(config.Telemetry),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("compose OIDC authenticator: %w", err)
		}
		return oidcAuthenticator, auth.OwnerAuthorizer{}, nil
	}
}

// authObservers wires the typed authentication-boundary events onto
// telemetry. Nil telemetry yields silent observers.
func authObservers(tel *observability.Telemetry) auth.Observers {
	if tel == nil {
		return auth.Observers{}
	}
	return auth.Observers{
		Authentication:       tel.AuthenticationObserved,
		JWKSRefresh:          tel.JWKSRefreshed,
		ForcedRefreshLimited: tel.ForcedRefreshLimited,
	}
}

// Handler returns the composed HTTP surface.
func (r *Runtime) Handler() http.Handler { return r.handler }

// Service exposes the composed application boundary.
func (r *Runtime) Service() *application.Service { return r.service }

// Worker exposes the composed outbox worker for direct pumping in tests.
func (r *Runtime) Worker() *worker.Worker { return r.worker }

// StartWorker runs the outbox worker loop until ctx is canceled or Stop is
// called. Each tick drains claimable work in bounded batches; there is no
// tight spin loop. Crashed or leased-in-flight work stays recoverable through
// existing lease expiry fencing regardless of this loop's lifetime. Multiple
// processes running this loop concurrently are safe by lease design.
//
// The API and worker deployment topologies are independent composition
// choices: callers that prefer separated control-plane/data-plane deployments
// can compose the handler without StartWorker and run workers elsewhere.
func (r *Runtime) StartWorker(ctx context.Context) {
	interval := r.workerInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				r.stopOnce.Do(func() { close(r.done) })
				return
			case <-ticker.C:
				r.drainBatch(ctx)
			}
		}
	}()
}

// Done reports when the worker loop has fully stopped after shutdown.
func (r *Runtime) Done() <-chan struct{} { return r.done }

func (r *Runtime) drainBatch(ctx context.Context) {
	const maxBatch = 16
	for processed := 0; processed < maxBatch; processed++ {
		if ctx.Err() != nil {
			return
		}
		found, err := r.worker.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, worker.ErrRecoveredPanic) {
			// Recovered panics were already logged with full sanitized
			// context by the telemetry sink.
			r.logger.Error("outbox work failed", "error", err, "error_class", "worker")
		}
		if !found {
			return
		}
	}
}

// StartOperationalSampler periodically samples cluster-global durable truth
// into gauges. Each cycle runs under a strict context budget in its own
// goroutine — never inside a request or a metrics scrape. Failures retain
// previous gauge values, count once, and log a bounded warning; they never
// crash Liftr (ADR-0018).
func (r *Runtime) StartOperationalSampler(ctx context.Context) {
	if r.sampler == nil || r.telemetry == nil {
		return
	}
	interval := r.sampleInterval
	if interval <= 0 {
		interval = observability.DefaultSampleInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		r.sampleOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sampleOnce(ctx)
			}
		}
	}()
}

func (r *Runtime) sampleOnce(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if r.telemetry != nil {
				r.telemetry.SampleFailed()
			}
			r.logger.Error("operational sampler panicked", "error_class", "panic",
				"panic_value", fmt.Sprintf("%v", recovered))
		}
	}()
	budget := r.sampleInterval / 2
	if budget > 5*time.Second {
		budget = 5 * time.Second
	}
	sampleCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sample, err := r.sampler.SnapshotOperationalState(sampleCtx, r.thresholds)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return // shutting down; not a sampling failure
		}
		r.telemetry.SampleFailed()
		r.logger.Warn("operational sample failed; retaining previous gauge values",
			"error_class", "sampler", "error", err.Error())
		return
	}
	r.telemetry.RecordClusterSample(sample)
}

// Wait blocks until the worker loop stops.
func (r *Runtime) Wait() {
	<-r.done
}

type staticSelector struct {
	defaultRef   application.ProvisionerRef
	routes       map[domain.ResourceTypeRef]application.ProvisionerRef
	capabilities map[application.ProvisionerRef]map[provisioning.ProvisionerCapability]struct{}
}

func (s staticSelector) Select(_ context.Context, resourceType domain.ResourceTypeRef, capability domain.Capability) (application.ProvisionerRef, error) {
	ref, routed := s.routes[resourceType]
	if !routed {
		return s.defaultRef, nil
	}
	registered := provisioning.ProvisionerCapability{ResourceType: resourceType, Capability: capability}
	if _, ok := s.capabilities[ref][registered]; !ok {
		return "", fmt.Errorf("provisioner %q does not declare %s for %s/%s", ref, capability, resourceType.Name, resourceType.Version)
	}
	return ref, nil
}

func validateProvisionerRoutes(config Config) (map[domain.ResourceTypeRef]application.ProvisionerRef, map[application.ProvisionerRef]map[provisioning.ProvisionerCapability]struct{}, error) {
	routes := make(map[domain.ResourceTypeRef]application.ProvisionerRef, len(config.ResourceTypeProvisionerRefs))
	capabilities := make(map[application.ProvisionerRef]map[provisioning.ProvisionerCapability]struct{})
	for resourceType, ref := range config.ResourceTypeProvisionerRefs {
		if _, err := application.NewProvisionerRef(string(ref)); err != nil {
			return nil, nil, fmt.Errorf("%w: invalid provisioner route for %s/%s: %v", application.ErrInvalidApplicationCall, resourceType.Name, resourceType.Version, err)
		}
		provider, ok := config.Provisioners[ref]
		if !ok || provider == nil {
			return nil, nil, fmt.Errorf("%w: routed provisioner %q is not registered", application.ErrInvalidApplicationCall, ref)
		}
		contract, err := config.Catalog.Get(context.Background(), resourceType)
		if err != nil || contract == nil {
			return nil, nil, fmt.Errorf("%w: routed resource type %s/%s is not registered", application.ErrInvalidApplicationCall, resourceType.Name, resourceType.Version)
		}
		declared := make(map[provisioning.ProvisionerCapability]struct{})
		for _, capability := range provider.Capabilities() {
			if _, duplicate := declared[capability]; duplicate {
				return nil, nil, fmt.Errorf("%w: provisioner %q declares a duplicate capability", application.ErrInvalidApplicationCall, ref)
			}
			declared[capability] = struct{}{}
			if capability.ResourceType == resourceType && !contract.Domain().Supports(capability.Capability) {
				return nil, nil, fmt.Errorf("%w: provisioner %q declares unsupported capability %q for %s/%s", application.ErrInvalidApplicationCall, ref, capability.Capability, resourceType.Name, resourceType.Version)
			}
		}
		create := provisioning.ProvisionerCapability{ResourceType: resourceType, Capability: domain.CapabilityCreate}
		if _, ok := declared[create]; !ok {
			return nil, nil, fmt.Errorf("%w: routed provisioner %q does not declare create for %s/%s", application.ErrInvalidApplicationCall, ref, resourceType.Name, resourceType.Version)
		}
		routes[resourceType] = ref
		capabilities[ref] = declared
	}
	return routes, capabilities, nil
}

type staticResolver struct {
	providers map[application.ProvisionerRef]provisioning.Provisioner
}

func (s staticResolver) Resolve(_ context.Context, ref application.ProvisionerRef) (provisioning.Provisioner, error) {
	provider, ok := s.providers[ref]
	if !ok {
		return nil, fmt.Errorf("no provisioner registered under reference %q", string(ref))
	}
	return provider, nil
}

// SetDraining flips readiness to not-ready before HTTP draining begins
// during graceful shutdown (ADR-0018).
func (r *Runtime) SetDraining() { r.draining.Store(true) }
