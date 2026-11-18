package main

import (
	"context"
	"os"

	"cloud.google.com/go/spanner"
)

// newConsoleSpannerClient is a Spanner data client on database at endpoint,
// which is every data client CloudBurrow makes (#1825).
//
// Two things in cloud.google.com/go/spanner v1.96.1 reach Google whatever
// endpoint option is given, and both are on by default:
//
//   - The built-in metrics (metrics.go, newBuiltinMetricsTracerFactory). They
//     export to Cloud Monitoring through createExporterOptions, which
//     overwrites the endpoint with monitoring.googleapis.com:443, on the
//     first period and when the client is closed. ClientConfig's
//     DisableNativeMetrics turns them off.
//   - The client's location (metrics.go, detectClientLocation), which is
//     computed for every client, metrics or not, by OpenTelemetry's GCP
//     detector: metadata.OnGCE, an HTTP request to 169.254.169.254 and a DNS
//     lookup of metadata.google.internal. The only thing that skips it is
//     SPANNER_EMULATOR_HOST being set, which also turns off the metrics.
//
// So the variable is set, when the process has none, to the endpoint the
// client dials anyway. It also selects nothing else here: the emulator
// options the client then prepends (an insecure, unauthenticated dial of
// that address) are what localOpts passes after them.
func newConsoleSpannerClient(ctx context.Context, endpoint, database string) (*spanner.Client, error) {
	spannerLocalOnly(endpoint)
	return spanner.NewClientWithConfig(ctx, database, spanner.ClientConfig{
		SessionPoolConfig:    spanner.DefaultSessionPoolConfig,
		DisableNativeMetrics: true,
	}, localOpts(endpoint)...)
}

// spannerLocalOnly sets SPANNER_EMULATOR_HOST to endpoint when it is unset.
// See newConsoleSpannerClient.
func spannerLocalOnly(endpoint string) {
	if os.Getenv("SPANNER_EMULATOR_HOST") == "" && endpoint != "" {
		_ = os.Setenv("SPANNER_EMULATOR_HOST", endpoint)
	}
}
