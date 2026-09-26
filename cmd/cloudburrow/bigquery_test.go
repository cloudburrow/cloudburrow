package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestBigQueryIsTunnelledOnBothPorts.
//
// BigQuery is the first backend serving two APIs from one Service: REST, and
// the gRPC Storage Read API the Go client's iterator reads large results
// through. Each needs its own tunnel, on its own fixed host port, and the
// second must not take the first's name, or everything that looks a forwarder
// up by service would find the wrong one.
func TestBigQueryIsTunnelledOnBothPorts(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceBigQuery}

	got := map[string]string{}
	for _, f := range buildForwarders(cfg) {
		got[f.Name()] = f.HostAddr() + " -> " + f.InClusterAddr()
	}
	ns := cfg.Cluster.Namespace
	for name, want := range map[string]string{
		"forward:bigquery":         "127.0.0.1:9014 -> bigquery." + ns + ".svc.cluster.local:9050",
		"forward:bigquery-storage": "127.0.0.1:9015 -> bigquery." + ns + ".svc.cluster.local:9060",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

func TestTheBigQueryManifestPublishesBothPorts(t *testing.T) {
	b, ok := components.OptionalBackend(config.ServiceBigQuery, "dev-project", true)
	if !ok {
		t.Fatal("no backend for bigquery")
	}
	m := b.Manifest("cb-ns", "inst")
	for _, want := range []string{
		"containerPort: 9050", "containerPort: 9060",
		"name: api\n      port: 9050", "name: storage-read\n      port: 9060",
		`"--project=dev-project"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	// In-memory: a persistent instance must still not be given a volume
	// that would imply otherwise.
	if strings.Contains(m, "PersistentVolumeClaim") {
		t.Error("the BigQuery emulator was given a volume; it keeps nothing across a restart")
	}
}

// No official client reads a BigQuery emulator variable, so what env exports
// must say so rather than look like one.
func TestEnvExportsTheBigQueryEndpointsForCode(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceBigQuery}
	vars := map[string]envVar{}
	for _, v := range envVars(cfg, "dev-project", "") {
		vars[v.Name] = v
	}
	for name, value := range map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY": "http://127.0.0.1:9014/",
		"CLOUDBURROW_BIGQUERY_ENDPOINT":            "http://127.0.0.1:9014",
		"CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT":    "127.0.0.1:9015",
	} {
		if vars[name].Value != value {
			t.Errorf("%s = %q, want %q", name, vars[name].Value, value)
		}
	}
	if !strings.Contains(vars["CLOUDBURROW_BIGQUERY_ENDPOINT"].Comment, "option.WithEndpoint") {
		t.Errorf("the endpoint variable does not say how it is used: %q", vars["CLOUDBURROW_BIGQUERY_ENDPOINT"].Comment)
	}

	cfg.Endpoints.BigQuery = 0
	if got := unexportableEmulators(cfg); len(got) != 1 || got[0] != config.ServiceBigQuery {
		t.Errorf("an OS-assigned BigQuery port is not reported as unexportable: %v", got)
	}
	for _, v := range envVars(cfg, "dev-project", "") {
		if strings.Contains(v.Name, "BIGQUERY") && !strings.Contains(v.Name, "STORAGE") {
			t.Errorf("exported %s for a port only `up` knows", v.Name)
		}
	}
}

// `status` showed no endpoints at all, so "status shows its endpoint" (#277)
// could not be true of any service. It now lists each enabled one as
// configured, BigQuery's two with the scope note beside them.
func TestStatusShowsTheBigQueryEndpoints(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceBigQuery}
	cfg.Endpoints.Storage = 0
	var out strings.Builder
	printConfiguredEndpoints(&out, cfg)
	got := out.String()
	for _, want := range []string{
		"bigquery         127.0.0.1:9014",
		"bigquery-storage 127.0.0.1:9015",
		"serves this instance's project only",
		"storage          OS-assigned; `up` prints it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
}

// The deployed emulator must be started with the instance's project, the one
// `env` tells every client to use. It was started with a fixed placeholder,
// harmless for the multi-project Google emulators, and every BigQuery call
// from a client configured by `env` was 404 "project ... is not found" (#277,
// found by the first compat run).
func TestTheDeployedBigQueryServesTheInstanceProject(t *testing.T) {
	cfg := config.Default()
	cfg.Name = "bq-proj-test"
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceBigQuery}
	comp := components.NewLifecycleComponent("", cfg, io.Discard)
	for _, b := range comp.Backends() {
		if b.Name != "bigquery" {
			continue
		}
		want := "--project=" + cfg.DefaultProject()
		for _, a := range b.Args {
			if a == want {
				return
			}
		}
		t.Fatalf("the deployed emulator's args are %v, want %s", b.Args, want)
	}
	t.Fatal("no bigquery backend was deployed")
}
