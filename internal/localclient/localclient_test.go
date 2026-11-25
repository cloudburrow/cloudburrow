package localclient

import (
	"os"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/spanner"
)

func TestSpannerConfigDisablesNativeMetrics(t *testing.T) {
	cfg := SpannerConfig(spanner.ClientConfig{NumChannels: 2})
	if !cfg.DisableNativeMetrics {
		t.Error("DisableNativeMetrics is false: the client exports to monitoring.googleapis.com")
	}
	if cfg.NumChannels != 2 {
		t.Errorf("NumChannels = %d, want the caller's 2", cfg.NumChannels)
	}
}

func TestBigtableConfigDisablesMetricsAndDirectAccess(t *testing.T) {
	cfg := BigtableConfig(bigtable.ClientConfig{AppProfile: "p", MetricsProvider: bigtable.DefaultMetricsProvider{}})
	if _, ok := cfg.MetricsProvider.(bigtable.NoopMetricsProvider); !ok {
		t.Errorf("MetricsProvider = %T, want NoopMetricsProvider: the built-in exporter sends to Cloud Monitoring", cfg.MetricsProvider)
	}
	if !cfg.DisableDirectAccess {
		t.Error("DisableDirectAccess is false: the Direct Access check probes the GCE metadata server")
	}
	if cfg.AppProfile != "p" {
		t.Errorf("AppProfile = %q, want the caller's", cfg.AppProfile)
	}
}

func TestSpannerLocalOnly(t *testing.T) {
	t.Setenv("SPANNER_EMULATOR_HOST", "")
	SpannerLocalOnly("")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q for no endpoint, want unset", got)
	}
	SpannerLocalOnly("127.0.0.1:9010")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the endpoint", got)
	}
	SpannerLocalOnly("127.0.0.1:1")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the one already set", got)
	}
}
