// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
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

	stopOnce sync.Once
	done     chan struct{}
}

// Config carries the composition dependencies. Transactions must be durable
// in production; tests may supply deterministic fakes.
type Config struct {
	Transactions application.TransactionRunner
	Catalog      application.ResourceTypeCatalog
	// Provisioners maps private ProvisionerRef values to provisioners.
	Provisioners map[application.ProvisionerRef]provisioning.Provisioner
	// DefaultProvisionerRef is selected for new Resources.
	DefaultProvisionerRef application.ProvisionerRef
	// WorkerInterval spaces out idle polling of the outbox. It does not
	// affect in-flight work; long provider calls run under renewed leases.
	WorkerInterval time.Duration
	Logger         *slog.Logger
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
	interval := config.WorkerInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	service, err := application.NewService(config.Catalog, staticSelector{ref: defaultRef}, staticResolver{providers: config.Provisioners}, config.Transactions)
	if err != nil {
		return nil, err
	}
	instance, err := worker.NewWithCatalog(config.Transactions, staticResolver{providers: config.Provisioners}, config.Catalog)
	if err != nil {
		return nil, err
	}
	instance.RetryBase = interval
	return &Runtime{handler: apihttp.NewHandler(apihttp.Deps{Service: service}), service: service, worker: instance, logger: logger, workerInterval: interval, done: make(chan struct{})}, nil
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
		if err != nil && !errors.Is(err, context.Canceled) {
			r.logger.Error("outbox work failed", "error", err)
		}
		if !found {
			return
		}
	}
}

// Wait blocks until the worker loop stops.
func (r *Runtime) Wait() {
	<-r.done
}

type staticSelector struct{ ref application.ProvisionerRef }

func (s staticSelector) Select(context.Context, domain.ResourceTypeRef, domain.Capability) (application.ProvisionerRef, error) {
	return s.ref, nil
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
