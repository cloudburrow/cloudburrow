// Package localclient builds the official Go clients whose built-in
// telemetry exports to Google whatever endpoint they are given, with that
// export off. Every Spanner and Bigtable data client and every Cloud Storage
// gRPC client in this repository, product and tests alike, is made here;
// test/repo's TestOfficialClientsGoThroughLocalclient fails on one made
// anywhere else.
//
// What each library does, in the versions go.mod names:
//
//   - cloud.google.com/go/spanner v1.96.1: built-in metrics are on unless
//     ClientConfig.DisableNativeMetrics, SPANNER_DISABLE_BUILTIN_METRICS=true
//     or SPANNER_EMULATOR_HOST is set (client.go, newClientWithConfig). They
//     export to Cloud Monitoring every minute through createExporterOptions,
//     which overwrites the endpoint with monitoring.googleapis.com:443
//     (metrics.go). A test that dials its endpoint with option.WithEndpoint,
//     without SPANNER_EMULATOR_HOST, exported to Google.
//   - cloud.google.com/go/bigtable v1.58.0: built-in metrics are on unless
//     ClientConfig.MetricsProvider is NoopMetricsProvider or
//     BIGTABLE_EMULATOR_HOST is set (client.go, NewClientWithConfig); there
//     is no other switch. DisableDirectAccess keeps the client from probing
//     the GCE metadata server for Direct Access.
//   - cloud.google.com/go/storage v1.70.0: a gRPC client enables its client
//     metrics, exported to Cloud Monitoring, unless WithDisabledClientMetrics
//     is passed or STORAGE_EMULATOR_HOST_GRPC is set (grpc_client.go,
//     newGRPCStorageClient). The JSON client has none unless
//     GCP_STORAGE_GO_ENABLE_OTEL_METRICS asks for them.
//   - cloud.google.com/go/pubsub/v2 v2.7.0 has no exporter of its own: its
//     tracing (ClientConfig.EnableOpenTelemetryTracing, off by default) uses
//     the process's global OpenTelemetry provider, so it needs nothing here.
package localclient

import (
	"context"
	"os"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/spanner"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Spanner is spanner.NewClient with the built-in metrics off.
func Spanner(ctx context.Context, database string, opts ...option.ClientOption) (*spanner.Client, error) {
	return SpannerWithConfig(ctx, database, spanner.ClientConfig{SessionPoolConfig: spanner.DefaultSessionPoolConfig}, opts...)
}

// SpannerWithConfig is spanner.NewClientWithConfig with the built-in metrics
// off, whatever cfg says.
func SpannerWithConfig(ctx context.Context, database string, cfg spanner.ClientConfig, opts ...option.ClientOption) (*spanner.Client, error) {
	return spanner.NewClientWithConfig(ctx, database, SpannerConfig(cfg), opts...)
}

// SpannerConfig is cfg with the built-in metrics off.
func SpannerConfig(cfg spanner.ClientConfig) spanner.ClientConfig {
	cfg.DisableNativeMetrics = true
	return cfg
}

// Bigtable is bigtable.NewClient with the built-in metrics and the Direct
// Access probe off.
func Bigtable(ctx context.Context, project, instance string, opts ...option.ClientOption) (*bigtable.Client, error) {
	return BigtableWithConfig(ctx, project, instance, bigtable.ClientConfig{}, opts...)
}

// BigtableWithConfig is bigtable.NewClientWithConfig with the built-in
// metrics and the Direct Access probe off, whatever cfg says.
func BigtableWithConfig(ctx context.Context, project, instance string, cfg bigtable.ClientConfig, opts ...option.ClientOption) (*bigtable.Client, error) {
	return bigtable.NewClientWithConfig(ctx, project, instance, BigtableConfig(cfg), opts...)
}

// BigtableConfig is cfg with the built-in metrics and the Direct Access
// probe off.
func BigtableConfig(cfg bigtable.ClientConfig) bigtable.ClientConfig {
	cfg.MetricsProvider = bigtable.NoopMetricsProvider{}
	cfg.DisableDirectAccess = true
	return cfg
}

// StorageGRPC is storage.NewGRPCClient with the client metrics off.
func StorageGRPC(ctx context.Context, opts ...option.ClientOption) (*storage.Client, error) {
	return storage.NewGRPCClient(ctx, append(opts, storage.WithDisabledClientMetrics())...)
}

// SpannerLocalOnly sets SPANNER_EMULATOR_HOST to endpoint when the process
// has none. DisableNativeMetrics stops the export but not the client's
// location detection (spanner metrics.go, detectClientLocation), which every
// client runs through OpenTelemetry's GCP detector: an HTTP request to the
// GCE metadata server and a lookup of metadata.google.internal. Only
// SPANNER_EMULATOR_HOST skips it. The emulator options the client then puts
// first (an insecure, unauthenticated dial of endpoint) are what a caller
// dialing a local, plaintext endpoint passes anyway, and its own options
// come after them.
func SpannerLocalOnly(endpoint string) {
	if os.Getenv("SPANNER_EMULATOR_HOST") == "" && endpoint != "" {
		_ = os.Setenv("SPANNER_EMULATOR_HOST", endpoint)
	}
}
