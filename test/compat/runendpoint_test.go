//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestRunClientFromTheExportedEndpoint (#707): the official Cloud Run v2
// client, built from CLOUDBURROW_RUN_ENDPOINT as `cloudburrow env` exports it
// for the running instance, lists services — the way code that deploys to
// Cloud Run is configured, with no port hard-coded.
func TestRunClientFromTheExportedEndpoint(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	out, err := exec.Command(cli, append([]string{"env", "--format", "json"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
	if err != nil {
		t.Fatalf("cloudburrow env: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode env: %v\n%s", err, out)
	}
	endpoint := env["CLOUDBURROW_RUN_ENDPOINT"]
	if endpoint == "" {
		t.Fatalf("env exported no CLOUDBURROW_RUN_ENDPOINT:\n%s", out)
	}
	if endpoint != h.Endpoint(EnvRun) {
		t.Errorf("CLOUDBURROW_RUN_ENDPOINT = %q, the instance's Run endpoint is %q", endpoint, h.Endpoint(EnvRun))
	}

	c, err := run.NewServicesClient(h.Context(), option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	it := c.ListServices(h.Context(), &runpb.ListServicesRequest{Parent: runParent(h)})
	if _, err := it.Next(); err != nil && err != iterator.Done {
		t.Fatalf("ListServices through CLOUDBURROW_RUN_ENDPOINT: %v", err)
	}
}
