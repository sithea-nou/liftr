// SPDX-License-Identifier: Apache-2.0

// Package observability implements Liftr's production telemetry: structured
// logging helpers, OpenTelemetry metrics with a Prometheus-compatible pull
// endpoint plus optional OTLP push, and minimal boundary tracing.
//
// The package is the ONLY place in the server that imports a telemetry
// library. Domain, lifecycle, resource-contract, application, worker,
// transport, auth, persistence, and provisioning code stay telemetry-free;
// they expose consumer-side ports that this package satisfies, and
// composition wires them together.
//
// Telemetry is strictly non-authoritative: it describes Liftr but never
// participates in lifecycle decisions, and exporter failures can never affect
// requests or durable outcomes (ADR-0018).
package observability

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Config carries the Liftr-specific telemetry configuration. Standard OTel
// behavior (OTLP endpoint/protocol, sampler, service name, resource
// attributes) is read from the standard OTEL_* environment variables by the
// SDK and exporters; Liftr does not alias it.
type Config struct {
	// MetricsAddr is the separate Prometheus scrape listener address.
	// Empty disables the listener entirely; metrics remain exportable via
	// configured OTLP push. There is no default public exposure.
	MetricsAddr string
	// SampleInterval is the operational-sampler period. Zero means 15s.
	SampleInterval time.Duration
	// LongRunningWarnAfter / LongRunningCritAfter threshold how long an
	// active Operation may run before it is counted as long-running.
	// Zero means 30m / 2h.
	LongRunningWarnAfter time.Duration
	LongRunningCritAfter time.Duration
	// ReconciliationSilentAfter thresholds how long an active Operation may
	// go without reconciliation activity before it is counted as
	// reconciliation-silent. Zero means 10m.
	ReconciliationSilentAfter time.Duration

	// ServiceName defaults to "liftr-server" when OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES do not supply one. ServiceVersion is build
	// metadata stamped onto the OTel resource as service.version.
	ServiceName    string
	ServiceVersion string

	// LogLevel is one of debug|info|warn|error (default info).
	LogLevel string
	// LogFormat is json (default) or text for local development.
	LogFormat string

	// Logger receives bounded telemetry-internal warnings (export failures,
	// sampler failures). Nil means silent.
	Logger *slog.Logger
}

// DefaultSampleInterval is the operational-sampler period applied when
// LIFTR_OBSERVABILITY_SAMPLE_INTERVAL is unset.
const DefaultSampleInterval = 15 * time.Second

// DefaultLongRunningWarnAfter is applied when
// LIFTR_OBSERVABILITY_LONG_RUNNING_WARN_AFTER is unset.
const DefaultLongRunningWarnAfter = 30 * time.Minute

// DefaultLongRunningCritAfter is applied when
// LIFTR_OBSERVABILITY_LONG_RUNNING_CRIT_AFTER is unset.
const DefaultLongRunningCritAfter = 2 * time.Hour

// DefaultReconciliationSilentAfter is applied when
// LIFTR_OBSERVABILITY_RECONCILIATION_SILENT_AFTER is unset.
const DefaultReconciliationSilentAfter = 10 * time.Minute

// LoadConfig reads Liftr-specific observability configuration from the
// environment. Invalid values fail startup with an error carrying no secret
// material.
func LoadConfig() (Config, error) {
	config := Config{
		ServiceName: "liftr-server",
	}
	var err error
	if config.MetricsAddr, err = optionalEnv("LIFTR_METRICS_ADDR"); err != nil {
		return Config{}, err
	}
	if config.SampleInterval, err = durationEnv("LIFTR_OBSERVABILITY_SAMPLE_INTERVAL", DefaultSampleInterval); err != nil {
		return Config{}, err
	}
	if config.LongRunningWarnAfter, err = durationEnv("LIFTR_OBSERVABILITY_LONG_RUNNING_WARN_AFTER", DefaultLongRunningWarnAfter); err != nil {
		return Config{}, err
	}
	if config.LongRunningCritAfter, err = durationEnv("LIFTR_OBSERVABILITY_LONG_RUNNING_CRIT_AFTER", DefaultLongRunningCritAfter); err != nil {
		return Config{}, err
	}
	if config.ReconciliationSilentAfter, err = durationEnv("LIFTR_OBSERVABILITY_RECONCILIATION_SILENT_AFTER", DefaultReconciliationSilentAfter); err != nil {
		return Config{}, err
	}
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		config.ServiceName = name
	}
	if config.LogLevel, err = stringEnv("LIFTR_LOG_LEVEL", "info"); err != nil {
		return Config{}, err
	}
	switch config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("environment variable LIFTR_LOG_LEVEL must be one of debug, info, warn, error")
	}
	if config.LogFormat, err = stringEnv("LIFTR_LOG_FORMAT", "json"); err != nil {
		return Config{}, err
	}
	switch config.LogFormat {
	case "json", "text":
	default:
		return Config{}, fmt.Errorf("environment variable LIFTR_LOG_FORMAT must be json or text")
	}
	return config, nil
}

// SlogLevel maps the configured level onto slog.
func (c Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func stringEnv(name, fallback string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

func optionalEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", nil
	}
	return value, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("environment variable %s must be a positive duration (for example %q)", name, fallback)
	}
	return value, nil
}
