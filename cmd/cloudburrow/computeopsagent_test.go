package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pod tells the guest's Ops Agent where Cloud Logging and Cloud
// Monitoring are, each only when it is served (#2139).
func TestOpsAgentEnv(t *testing.T) {
	b := &computeVMs{}
	if env := b.opsAgentEnv(); env != nil {
		t.Fatalf("no hosts: %v", env)
	}
	b.opsAgentHosts = func() (string, string) { return "cloudburrow-host.ns.svc.cluster.local:9020", "" }
	env := b.opsAgentEnv()
	if len(env) != 1 || env[0].(map[string]string)["name"] != opsAgentLoggingEnv {
		t.Fatalf("logging only: %v", env)
	}
	b.opsAgentHosts = func() (string, string) { return "l:1", "m:2" }
	if env := b.opsAgentEnv(); len(env) != 2 || env[1].(map[string]string)["value"] != "m:2" {
		t.Fatalf("both: %v", env)
	}
	var s *computeService
	if l, m := s.telemetryHosts(); l != "" || m != "" {
		t.Fatalf("nil service: %q %q", l, m)
	}
}

// The guest's units run the agent only behind its fence: each agent unit
// starts after the fence and the metadata relay, in the fenced group, with
// its ExecCondition, and the main unit skips Google's startup health
// checks, which dial Google's APIs.
func TestOpsAgentUnitsAreFenced(t *testing.T) {
	for _, u := range []string{"google-cloud-ops-agent-fluent-bit", "google-cloud-ops-agent-opentelemetry-collector", "google-cloud-ops-agent"} {
		b := readGuestFile(t, "etc/systemd/system/"+u+".service.d/zz-cloudburrow.conf")
		for _, want := range []string{"Requires=cloudburrow-ops-agent-fence.service cloudburrow-metadata-server.service",
			"Group=cloudburrow-opsagent", "ExecCondition=+/usr/lib/cloudburrow/ops-agent condition", "\nExecStartPre=\n"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: no %q", u, want)
			}
		}
		if u != "google-cloud-ops-agent" && !strings.Contains(b, "ops-agent localize") {
			t.Errorf("%s: its configuration is not localized", u)
		}
	}
	for _, df := range []string{"Dockerfile.debian", "Dockerfile.rocky"} {
		b := readGuestFile(t, "../"+df)
		if !strings.Contains(b, "ARG OPS_AGENT_VERSION=2.73.0") || !strings.Contains(b, "cloudburrow-ops-agent-fence.service") {
			t.Errorf("%s: the pinned agent or its fence is missing", df)
		}
	}
}

// readGuestFile reads a file of the VM image's root/ tree.
func readGuestFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "compute", "guest", "root", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
