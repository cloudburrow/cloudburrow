package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The wrapper takes a running instance's services, as `env` does (#652):
// `cloudburrow terraform --name x` used to give the provider the default
// services' endpoints, so an opt-in service the instance serves, such as
// BigQuery, got none and its resources went to real Google (#1236).
func TestTheWrapperUsesTheServicesARunningInstanceServes(t *testing.T) {
	fakeTerraform(t, "exit 0")
	inModule(t, "")
	dir := t.TempDir()
	cfg, err := config.Load(config.Options{Args: []string{"--name", "only-bq", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), ProcStart: selfStart(t), Services: []config.Service{config.ServiceBigQuery},
		Endpoints: map[string]string{"bigquery": "127.0.0.1:9744"}}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runTerraform([]string{"--name", "only-bq", "--state-dir", dir, "--", "plan"}, &out, &strings.Builder{}); err != nil {
		t.Fatalf("runTerraform = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `big_query_custom_endpoint = "http://127.0.0.1:9744/bigquery/v2/"`) {
		t.Errorf("the provider file has no BigQuery endpoint at the instance's port:\n%s", out.String())
	}
	for _, attr := range []string{"pubsub_custom_endpoint", "cloud_tasks_custom_endpoint", "secret_manager_custom_endpoint"} {
		if strings.Contains(out.String(), attr) {
			t.Errorf("the provider file sets %s for an instance that does not serve it:\n%s", attr, out.String())
		}
	}
}
