// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracer and Span are the minimal tracing surface used at Liftr's
// instrumentation seams. When no OTLP endpoint is configured the concrete
// implementation is a no-op, so spans cost effectively nothing and sampling
// can never influence execution (ADR-0018).
type Tracer interface {
	StartSpan(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, Span)
}

type Span interface {
	SetString(key, value string)
	SetInt(key string, value int64)
	RecordError(err error)
	End()
}

type sdkTracer struct{ tracer trace.Tracer }

func (s sdkTracer) StartSpan(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, Span) {
	ctx, span := s.tracer.Start(ctx, name, trace.WithAttributes(attributes...))
	return ctx, sdkSpan{span}
}

type sdkSpan struct{ span trace.Span }

func (s sdkSpan) SetString(key, value string) { s.span.SetAttributes(attribute.String(key, value)) }
func (s sdkSpan) SetInt(key string, value int64) {
	s.span.SetAttributes(attribute.Int64(key, value))
}
func (s sdkSpan) RecordError(err error) { s.span.RecordError(err) }
func (s sdkSpan) End()                  { s.span.End() }

type noopTracer struct{}

func (noopTracer) StartSpan(ctx context.Context, _ string, _ ...attribute.KeyValue) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetString(string, string) {}
func (noopSpan) SetInt(string, int64)     {}
func (noopSpan) RecordError(error)        {}
func (noopSpan) End()                     {}

var (
	_ codes.Code // keep the codes import referenced for RecordError users
)
