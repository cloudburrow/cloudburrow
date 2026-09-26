package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

func TestMemorystoreIsTunnelled(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceMemorystore}
	for _, f := range buildForwarders(cfg) {
		if f.Name() == "forward:memorystore" {
			want := "127.0.0.1:9016 -> memorystore." + cfg.Cluster.Namespace + ".svc.cluster.local:6379"
			if got := f.HostAddr() + " -> " + f.InClusterAddr(); got != want {
				t.Errorf("forward:memorystore = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("no forwarder for memorystore")
}

// Persistent mode keeps an append-only file on a volume; ephemeral mode
// writes nothing to disk, so a restart starts empty.
func TestTheMemorystoreManifestFollowsTheMode(t *testing.T) {
	persistent, ok := components.OptionalBackend(config.ServiceMemorystore, "p", true)
	if !ok {
		t.Fatal("no backend for memorystore")
	}
	m := persistent.Manifest("cb-ns", "inst")
	for _, want := range []string{"PersistentVolumeClaim", "mountPath: /data", `"--appendonly"`, `"yes"`, "containerPort: 6379", "sha256:"} {
		if !strings.Contains(m, want) {
			t.Errorf("persistent manifest lacks %q:\n%s", want, m)
		}
	}
	ephemeral, _ := components.OptionalBackend(config.ServiceMemorystore, "p", false)
	m = ephemeral.Manifest("cb-ns", "inst")
	if strings.Contains(m, "PersistentVolumeClaim") || strings.Contains(m, `"yes"`) {
		t.Errorf("the ephemeral manifest persists something:\n%s", m)
	}
	if !strings.Contains(m, `"--save"`) {
		t.Errorf("the ephemeral manifest does not disable RDB snapshots:\n%s", m)
	}
}

func TestEnvExportsRedisHostAndPort(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceMemorystore}
	vars := map[string]envVar{}
	for _, v := range envVars(cfg, "dev-project", "") {
		vars[v.Name] = v
	}
	if vars["REDIS_HOST"].Value != "127.0.0.1" || vars["REDIS_PORT"].Value != "9016" {
		t.Errorf("REDIS_HOST=%q REDIS_PORT=%q", vars["REDIS_HOST"].Value, vars["REDIS_PORT"].Value)
	}
	if !strings.Contains(vars["REDIS_HOST"].Comment, "not the Memorystore admin API") {
		t.Errorf("REDIS_HOST's comment does not state the scope: %q", vars["REDIS_HOST"].Comment)
	}

	// A live, OS-assigned port reaches env from the runtime file.
	cfg.Endpoints.Memorystore = 0
	if got := unexportableEmulators(cfg); len(got) != 1 || got[0] != config.ServiceMemorystore {
		t.Errorf("an OS-assigned Memorystore port is not reported as unexportable: %v", got)
	}
	live := withLivePorts(cfg, map[string]string{"memorystore": "127.0.0.1:41234"})
	for _, v := range envVars(live, "dev-project", "") {
		if v.Name == "REDIS_PORT" && v.Value != "41234" {
			t.Errorf("REDIS_PORT from the live port = %q", v.Value)
		}
	}
	if !strings.Contains(netfwd.ScopeNoteFor("memorystore"), "not the Memorystore admin API") {
		t.Error("the endpoint's scope note does not say it is not the admin API")
	}
}
