// SPDX-License-Identifier: Apache-2.0

// Command liftr-demo-server runs a self-contained, obviously non-production
// Liftr control plane for the local demonstration under examples/demo/.
//
// Differences from the production cmd/liftr-server composition:
//
//   - It registers the three demo-only ResourceTypes defined in this directory
//     (DemoDatabase/v1, DemoApp/v1, DemoFault/v1) and one deterministic demo
//     provisioner that models no real backend. Production composition never
//     sees any of these.
//   - It runs the explicit insecure development authentication mode on loopback
//     listeners: every caller is treated as the fixed development principal.
//     This is acceptable ONLY because both listeners bind loopback by default
//     and refuse other addresses without LIFTR_DEMO_ALLOW_REMOTE=1.
//   - No Pulumi, Crossplane, or OpenTofu adapter is composed; no cloud
//     credentials are read; telemetry export stays disabled.
//
// Everything else — PostgreSQL durability, transactional outbox, worker loop,
// developer HTTP API, operator admin API, admission policy overlay, lifecycle
// engine — is the real production code path via internal/server.Compose.
package main

import (
	"context"
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
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/policy"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/server"
)

const (
	defaultAddr        = "127.0.0.1:18080"
	defaultAdminAddr   = "127.0.0.1:18090"
	defaultControlAddr = "127.0.0.1:18099"
	defaultDatabaseURL = "postgres://liftr:liftr@127.0.0.1:55432/liftr_demo?sslmode=disable"
	defaultOpenAPIDir  = "docs/openapi"
	shutdownWindow     = 10 * time.Second
	demoWorkerInterval = 100 * time.Millisecond
)

// demoDevHandler decorates the developer API with demo-only documentation
// surfaces: a browser landing page at / and the handwritten OpenAPI documents.
// Every other path falls through to the untouched production API handler; the
// API surface itself does not change.
func demoDevHandler(next http.Handler, openAPIDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(demoIndexHTML))
		case "/openapi/v1.yaml":
			serveOpenAPI(w, filepath.Join(openAPIDir, "v1", "openapi.yaml"))
		case "/openapi/admin-v1.yaml":
			serveOpenAPI(w, filepath.Join(openAPIDir, "admin", "v1", "openapi-admin.yaml"))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func serveOpenAPI(w http.ResponseWriter, path string) {
	contents, err := os.ReadFile(path)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w,
			"OpenAPI document not available; run the demo from the repository root (make demo-up)",
			http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(contents)
}

const demoIndexHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Liftr Demo Server (non-production)</title>
<style>body{font-family:ui-monospace,Menlo,monospace;margin:2rem;line-height:1.5}
h1{font-size:1.2rem}li{margin:.25rem 0}.warn{color:#b45309}</style></head>
<body>
<h1>LIFTR DEMO SERVER &mdash; NON-PRODUCTION</h1>
<p class="warn">Insecure development authentication on loopback listeners only.</p>
<ul>
<li><a href="/openapi/v1.yaml">OpenAPI &mdash; developer API (/v1)</a> &middot; <a href="http://127.0.0.1:18081">Swagger UI</a></li>
<li><a href="/openapi/admin-v1.yaml">OpenAPI &mdash; operator plane (/admin/v1)</a></li>
<li><a href="/v1/resource-types">GET /v1/resource-types</a> &mdash; discovery</li>
<li><a href="/v1/resources">GET /v1/resources</a> &mdash; inventory</li>
<li><a href="/healthz">GET /healthz</a> &middot; <a href="/readyz">GET /readyz</a></li>
</ul>
<p>Drive lifecycle actions through the liftr CLI (<code>make demo</code> shows how).</p>
</body></html>`

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := envOr("LIFTR_ADDR", defaultAddr)
	adminAddr := envOr("LIFTR_ADMIN_ADDR", defaultAdminAddr)
	databaseURL := envOr("LIFTR_DEMO_DATABASE_URL", defaultDatabaseURL)
	for _, check := range []struct{ name, address string }{
		{"LIFTR_ADDR", addr}, {"LIFTR_ADMIN_ADDR", adminAddr},
	} {
		if err := requireLoopback(check.name, check.address); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, closePool, err := openDatabase(ctx, databaseURL, logger)
	if err != nil {
		logger.Error("demo database unavailable; run 'make demo-up' first", "error", err)
		os.Exit(1)
	}
	defer closePool()

	contracts, err := buildContracts()
	if err != nil {
		logger.Error("build demo contracts", "error", err)
		os.Exit(1)
	}
	catalog, err := resourcetypes.NewRegistry(contracts...)
	if err != nil {
		logger.Error("register demo contracts", "error", err)
		os.Exit(1)
	}

	admissionPolicy, err := policy.LoadFile(ctx, os.Getenv("LIFTR_POLICY_FILE"), catalog)
	if err != nil {
		logger.Error("load admission policy", "error", err)
		os.Exit(1)
	}

	provisionerRef, err := application.NewProvisionerRef("liftr-demo-v1")
	if err != nil {
		logger.Error("compose provisioner reference", "error", err)
		os.Exit(1)
	}
	provisioner := newDemoProvisioner()

	store, err := postgres.NewStore(pool)
	if err != nil {
		logger.Error("open demo store", "error", err)
		os.Exit(1)
	}

	runtime, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		AdmissionPolicy:       admissionPolicy,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{provisionerRef: provisioner},
		DefaultProvisionerRef: provisionerRef,
		Logger:                logger,
		WorkerInterval:        demoWorkerInterval,
		InsecureAuth:          true,
		AdminAuth:             &server.AdminAuthConfig{},
	})
	if err != nil {
		logger.Error("demo runtime composition failed", "error", err)
		os.Exit(1)
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	runtime.StartWorker(workerCtx)

	devHandler := demoDevHandler(runtime.Handler(), envOr("LIFTR_OPENAPI_DIR", defaultOpenAPIDir))
	devServer := &http.Server{Addr: addr, Handler: devHandler, ReadHeaderTimeout: 5 * time.Second}
	adminServer := &http.Server{Addr: adminAddr, Handler: runtime.AdminHandler(), ReadHeaderTimeout: 5 * time.Second}
	controlAddr := envOr("LIFTR_CONTROL_ADDR", defaultControlAddr)
	if err := requireLoopback("LIFTR_CONTROL_ADDR", controlAddr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	controlServer := &http.Server{Addr: controlAddr, Handler: controlHandler(provisioner), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = devServer.ListenAndServe() }()
	go func() { _ = adminServer.ListenAndServe() }()
	go func() { _ = controlServer.ListenAndServe() }()

	fmt.Println("===================================================================")
	fmt.Println(" LIFTR DEMO SERVER — NON-PRODUCTION")
	fmt.Println("   developer API : http://" + addr + "   (insecure dev auth; ANY caller accepted)")
	fmt.Println("   admin API     : http://" + adminAddr + " (separate operator plane)")
	fmt.Println("   demo control  : http://" + controlAddr + " (simulates backend convergence; demo only)")
	fmt.Println("   resource types: DemoApp/v1, DemoDatabase/v1, DemoFault/v1 (deterministic)")
	fmt.Println("   durable state : " + databaseURL)
	fmt.Println("===================================================================")

	<-ctx.Done()
	logger.Info("demo shutdown requested")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownWindow)
	defer cancelShutdown()
	_ = devServer.Shutdown(shutdownCtx)
	_ = adminServer.Shutdown(shutdownCtx)
	_ = controlServer.Shutdown(shutdownCtx)
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

// controlHandler is the demo-only stand-in for real backend convergence. A
// POST /release/{resourceID} tells the simulated backend that the Resource
// finished converging; the next observation then reports success. Liftr itself
// has no forced-success primitive — this listener exists only in the demo
// binary, binds loopback by default, and never touches production composition.
func controlHandler(provisioner *demoProvisioner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /release/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "resource id required", http.StatusBadRequest)
			return
		}
		provisioner.Release(id)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// requireLoopback refuses to expose the demo control plane beyond the local
// machine unless the operator explicitly sets LIFTR_DEMO_ALLOW_REMOTE=1.
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
		return fmt.Errorf("%s must be a loopback address in the demo composition (got %q); set LIFTR_DEMO_ALLOW_REMOTE=1 to override explicitly", name, address)
	}
	return nil
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
	// The demo owns its database and applies migrations itself so the whole
	// environment comes up with one command. Production servers never do this;
	// operators run liftr-migrate explicitly.
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}
	if err := postgres.VerifySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	logger.Info("demo schema migrated and verified")
	return pool, pool.Close, nil
}
