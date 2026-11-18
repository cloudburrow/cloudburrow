package main

import (
	"os"
	"testing"

	"cloud.google.com/go/bigtable"
)

// The switches that keep the console's client libraries off Google's hosts
// (#1825); TestNoGoogleEgress (integration) proves them on a running
// instance. Each is a default of the library that reaches Google whatever
// endpoint the client is given.
func TestConsoleBigtableConfigIsLocalOnly(t *testing.T) {
	if _, ok := consoleBigtableConfig.MetricsProvider.(bigtable.NoopMetricsProvider); !ok {
		t.Errorf("MetricsProvider = %T, want NoopMetricsProvider: the built-in exporter sends to Cloud Monitoring", consoleBigtableConfig.MetricsProvider)
	}
	if !consoleBigtableConfig.DisableDirectAccess {
		t.Error("DisableDirectAccess is false: the Direct Access check probes the GCE metadata server")
	}
}

func TestSpannerLocalOnlySetsTheEmulatorHost(t *testing.T) {
	t.Setenv("SPANNER_EMULATOR_HOST", "")
	spannerLocalOnly("127.0.0.1:9010")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the endpoint: unset, every Spanner client probes the metadata server", got)
	}
	// One the process already has is kept.
	spannerLocalOnly("127.0.0.1:1")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the one already set", got)
	}
}
