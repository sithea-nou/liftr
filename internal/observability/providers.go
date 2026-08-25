// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// Telemetry owns Liftr's metric and trace instrumentation. It is always safe
// to construct and use: when no exporter is configured the underlying OTel
// machinery is inert, so recording costs are negligible and behavior is
// unchanged (ADR-0018).
type Telemetry struct {
	config Config

	// PrometheusRegistry backs the operator /metrics endpoint on the
	// separate listener. It is always created; without LIFTR_METRICS_ADDR it
	// is simply never served.
	PrometheusRegistry *promclient.Registry

	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider

	instruments  *instruments
	logger       *boundedLogger
	shutdownOnce sync.Once
}

// Shutdown flushes and releases both providers within the given budget.
// Failures are reported but never fatal: telemetry must not affect lifecycle
// outcomes (ADR-0018). Safe to call more than once.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var err error
	t.shutdownOnce.Do(func() {
		if t.tracerProvider != nil {
			if shutdownErr := t.tracerProvider.Shutdown(ctx); shutdownErr != nil {
				err = fmt.Errorf("shutdown tracer provider: %w", shutdownErr)
			}
		}
		if t.meterProvider != nil {
			if shutdownErr := t.meterProvider.Shutdown(ctx); shutdownErr != nil && err == nil {
				err = fmt.Errorf("shutdown meter provider: %w", shutdownErr)
			}
		}
	})
	return err
}

// otlpEndpointConfigured reports whether OTLP push was requested through the
// standard OTEL_EXPORTER_OTLP_ENDPOINT environment variable (or its
// signal-specific overrides).
func otlpEndpointConfigured() bool {
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func otlpProtocol() string {
	value := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	if value == "" {
		return "grpc"
	}
	return value
}

// NewTelemetry builds the providers from Liftr config plus the standard
// OTEL_* environment variables:
//
//   - Metrics are always collected into the Prometheus pull registry, which
//     main serves only when LIFTR_METRICS_ADDR is configured.
//   - When OTEL_EXPORTER_OTLP_ENDPOINT is set, an OTLP periodic reader
//     (metrics) and a batch span processor (traces) push through gRPC; any
//     other OTEL_EXPORTER_OTLP_PROTOCOL value fails startup loudly rather
//     than silently dropping telemetry.
//   - Traces follow OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG (spec
//     defaults). Without an endpoint the tracer stays no-op, so sampling can
//     never affect execution.
func NewTelemetry(ctx context.Context, config Config) (*Telemetry, error) {
	if config.ServiceVersion == "" {
		config.ServiceVersion = "dev"
	}
	telemetry := &Telemetry{config: config, logger: newBoundedLogger(config.Logger)}

	registry := promclient.NewRegistry()
	promExporter, err := promexporter.New(promexporter.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}
	telemetry.PrometheusRegistry = registry

	options := []sdkmetric.Option{
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithView(operationDurationView()),
		sdkmetric.WithView(provisionerCallDurationView()),
	}
	if otlpEndpointConfigured() {
		if otlpProtocol() != "grpc" {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL %q is not supported by this build; use grpc", otlpProtocol())
		}
		metricExporter, metricErr := otlpmetricgrpc.New(ctx)
		if metricErr != nil {
			return nil, fmt.Errorf("create otlp metric exporter: %w", metricErr)
		}
		options = append(options, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)))
	}
	telemetry.meterProvider = sdkmetric.NewMeterProvider(options...)

	res, resErr := telemetryResource(config)
	if resErr != nil {
		return nil, fmt.Errorf("build otel resource: %w", resErr)
	}
	if otlpEndpointConfigured() {
		if otlpProtocol() != "grpc" {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL %q is not supported by this build; use grpc", otlpProtocol())
		}
		sampler, samplerErr := traceSamplerFromEnv()
		if samplerErr != nil {
			return nil, samplerErr
		}
		spanExporter, spanErr := otlptracegrpc.New(ctx)
		if spanErr != nil {
			return nil, fmt.Errorf("create otlp span exporter: %w", spanErr)
		}
		telemetry.tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sampler),
			sdktrace.WithBatcher(spanExporter),
		)
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(telemetry.logger)

	instruments, instrumentsErr := newInstruments(telemetry.meterProvider.Meter("github.com/sithea-nou/liftr/internal/observability"))
	if instrumentsErr != nil {
		return nil, fmt.Errorf("create instruments: %w", instrumentsErr)
	}
	telemetry.instruments = instruments
	return telemetry, nil
}

// Tracer returns the boundary tracer. It is a no-op tracer unless an OTLP
// endpoint is configured, so callers never branch on configuration.
func (t *Telemetry) Tracer() Tracer {
	if t == nil || t.tracerProvider == nil {
		return noopTracer{}
	}
	return sdkTracer{tracer: t.tracerProvider.Tracer("github.com/sithea-nou/liftr")}
}

func telemetryResource(config Config) (*resource.Resource, error) {
	// resource.Default() already merges OTEL_RESOURCE_ATTRIBUTES and detects
	// OTEL_SERVICE_NAME; our values fill what the environment left unset and
	// stamp build metadata as service.version.
	return resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(config.ServiceName),
			semconv.ServiceVersion(config.ServiceVersion),
		),
	)
}

// traceSamplerFromEnv maps the standard OTEL_TRACES_SAMPLER variable onto the
// SDK sampler. Unset means parentbased_always_on per the specification;
// because spans export only when an OTLP endpoint is configured, the default
// has zero runtime effect otherwise.
func traceSamplerFromEnv() (sdktrace.Sampler, error) {
	name := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER"))
	arg := 1.0
	if raw := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); raw != "" {
		parsed, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil || parsed < 0 || parsed > 1 {
			return nil, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be a ratio between 0 and 1")
		}
		arg = parsed
	}
	switch name {
	case "", "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(arg), nil
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(arg)), nil
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_SAMPLER %q", name)
	}
}

// operationDurationView widens the Operation-duration histogram beyond SDK
// defaults: lifecycle latency legitimately spans seconds to hours and a
// short-capped default bucket would collapse the entire long tail into +Inf.
func operationDurationView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "liftr.operation.duration"},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 7200, 21600},
			},
		},
	)
}

// provisionerCallDurationView covers Submit/Observe calls that may run for
// minutes under renewed leases.
func provisionerCallDurationView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "liftr.provisioner.call.duration"},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.05, 0.1, 0.25, 0.5, 1, 5, 10, 30, 60, 300, 600},
			},
		},
	)
}
