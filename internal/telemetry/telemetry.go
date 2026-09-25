// Package telemetry exports OpenTelemetry traces of the requests CloudBurrow
// serves itself (#313).
//
// It is off unless OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set. Off means no exporter is built
// and nothing is dialled: the tracer provider is a no-op and no gRPC server
// gets a stats handler. On, spans go to the configured endpoint and nowhere
// else. The global OpenTelemetry provider is never set, so the Google client
// libraries CloudBurrow uses internally are not traced by accident.
package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
)

// Propagator is W3C Trace Context, the traceparent header.
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// Tracing is the process's tracing, enabled or not.
type Tracing struct {
	provider trace.TracerProvider
	shutdown func(context.Context) error
}

// Disabled is tracing that records nothing and connects nowhere.
func Disabled() *Tracing {
	return &Tracing{provider: noop.NewTracerProvider(), shutdown: func(context.Context) error { return nil }}
}

// Setup reads the standard OTLP variables through getenv. With neither
// endpoint variable set it returns Disabled without building an exporter.
// The exporter itself reads the rest of the OTEL_EXPORTER_OTLP_* variables
// (headers, timeout, insecure) from the process environment, as the SDK
// documents.
func Setup(ctx context.Context, getenv func(string) string, version string) (*Tracing, error) {
	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return Disabled(), nil
	}
	protocol := getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	var exp sdktrace.SpanExporter
	var err error
	switch strings.ToLower(protocol) {
	case "", "http/protobuf":
		exp, err = otlptracehttp.New(ctx)
	case "grpc":
		exp, err = otlptracegrpc.New(ctx)
	default:
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL %q is not supported; use http/protobuf or grpc", protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("OTLP trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "cloudburrow"),
			attribute.String("service.version", version),
		)),
	)
	return &Tracing{provider: tp, shutdown: tp.Shutdown}, nil
}

// Enabled reports whether spans are exported.
func (t *Tracing) Enabled() bool {
	_, noopProvider := t.provider.(noop.TracerProvider)
	return !noopProvider
}

// Provider is the tracer provider: a no-op one when disabled.
func (t *Tracing) Provider() trace.TracerProvider { return t.provider }

// ServerOptions trace a gRPC server's calls, continuing an incoming
// traceparent. None when disabled, so a disabled server is unchanged.
func (t *Tracing) ServerOptions() []grpc.ServerOption {
	if !t.Enabled() {
		return nil
	}
	return []grpc.ServerOption{grpc.StatsHandler(otelgrpc.NewServerHandler(
		otelgrpc.WithTracerProvider(t.provider),
		otelgrpc.WithPropagators(Propagator),
	))}
}

// Shutdown flushes pending spans, bounded by ctx.
func (t *Tracing) Shutdown(ctx context.Context) error { return t.shutdown(ctx) }
