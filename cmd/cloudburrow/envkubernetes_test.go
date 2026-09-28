package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// everyService enables every selectable service.
func everyService(t *testing.T) config.Config {
	t.Helper()
	var names []string
	for _, s := range config.KnownServices() {
		names = append(names, string(s))
	}
	return formatsConfig(t, strings.Join(names, ","))
}

// parseKubernetesEnv reads an `env:` list back as name -> value.
func parseKubernetesEnv(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	var name string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "  - name: "):
			name = strings.TrimPrefix(line, "  - name: ")
		case strings.HasPrefix(line, "    value: "):
			v, err := strconv.Unquote(strings.TrimPrefix(line, "    value: "))
			if err != nil {
				t.Fatalf("value of %s is not a quoted string: %q", name, line)
			}
			if _, dup := got[name]; dup {
				t.Errorf("%s is listed twice", name)
			}
			got[name] = v
		}
	}
	return got
}

// hostPublished stands in for a started clusterHost: the CLI-hosted
// services and the metadata server published through cloudburrow-host, and
// BigQuery's REST tunnel with its validating front (#874).
func hostPublished(cfg config.Config) *clusterHost {
	return &clusterHost{cfg: cfg, ports: map[string]int{"run": 9005, "tasks": 9002, "secretmanager": 9003,
		"kms": 9008, "scheduler": 9009, "logging": 9010, "resourcemanager": 9011, "metadata": 9004,
		"bigquery": 9050}}
}

// #576: `env --format kubernetes` lists, for every enabled service, the
// variable that points a pod's client at it, with the address netfwd gives
// that service's in-cluster Service — never a host address — plus the
// CLI-hosted services at cloudburrow-host, GCE_METADATA_HOST and the
// project. No credentials: the host's fixture is a host path.
func TestEnvKubernetesListsTheInClusterAddressOfEveryEnabledService(t *testing.T) {
	cfg := everyService(t)
	host := hostPublished(cfg)
	info := runtimeInfo{Endpoints: map[string]string{}, InCluster: host.InClusterAll()}
	want := map[string]string{"GOOGLE_CLOUD_PROJECT": cfg.DefaultProject()}
	for _, s := range cfg.EnabledServices() {
		for _, tg := range forwardTargets(cfg, s) {
			key := targetKey(tg)
			info.Endpoints[key] = "127.0.0.1:1"
			addr := tg.InClusterAddr()
			if !strings.HasSuffix(strings.Split(addr, ":")[0], "."+cfg.Cluster.Namespace+".svc.cluster.local") {
				t.Fatalf("%s's in-cluster address %q is not a Service name", key, addr)
			}
			switch {
			case netfwd.EnvVarFor(key) != "":
				want[netfwd.EnvVarFor(key)] = netfwd.EnvValueFor(key, addr)
			case key == "bigquery":
				// Not the emulator's Service, which would skip every check
				// (#874): the host tunnel's front, through cloudburrow-host.
				want["CLOUDBURROW_BIGQUERY_ENDPOINT"] = "http://" + host.InCluster("bigquery")
			case key == "bigquery-storage":
				want["CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT"] = addr
			}
		}
	}
	for _, s := range []string{"tasks", "secretmanager", "kms", "scheduler", "logging", "resourcemanager", "run"} {
		want[cliEndpointVar(s)] = host.InCluster(s)
	}
	want["GCE_METADATA_HOST"] = host.InCluster("metadata")
	for _, name := range []string{"STORAGE_EMULATOR_HOST", "PUBSUB_EMULATOR_HOST", "FIRESTORE_EMULATOR_HOST",
		"DATASTORE_EMULATOR_HOST", "BIGTABLE_EMULATOR_HOST", "SPANNER_EMULATOR_HOST", "CLOUDBURROW_BIGQUERY_ENDPOINT"} {
		if want[name] == "" {
			t.Fatalf("the test expects nothing for %s: every enabled emulator should have a variable", name)
		}
	}

	var out bytes.Buffer
	writeKubernetesEnv(&out, cfg.Name, kubernetesEnvVars(cfg, info, cfg.DefaultProject()))
	got := parseKubernetesEnv(t, out.String())
	if !maps.Equal(got, want) {
		t.Errorf("env --format kubernetes =\n%s\nwant %v", out.String(), want)
	}
	for _, bad := range []string{"127.0.0.1", "localhost", "GOOGLE_APPLICATION_CREDENTIALS", "MYSQL_PASSWORD"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("the kubernetes env names %s:\n%s", bad, out.String())
		}
	}
	if !strings.Contains(out.String(), "\nenv:\n  - name: ") {
		t.Errorf("not a container env: list:\n%s", out.String())
	}

	// The Cloud Run adapter injects the same table, less the project,
	// which it takes from each service's own name.
	injected := podEnvMap(podAddresses(buildForwarders(cfg), host))
	delete(want, "GOOGLE_CLOUD_PROJECT")
	if !maps.Equal(injected, want) {
		t.Errorf("the adapter's injected environment differs from env --format kubernetes:\n got %v\nwant %v", injected, want)
	}
	if got := injected["CLOUDBURROW_BIGQUERY_ENDPOINT"]; !strings.HasPrefix(got, "http://"+ClusterHostService+".") {
		t.Errorf("pods are given BigQuery at %q; want the front at %s (#874)", got, ClusterHostService)
	}

	// The banner and `status` report the same in-cluster address.
	for _, e := range startupEndpoints(cfg, buildForwarders(cfg), nil, nil, nil, nil, host) {
		want := ""
		switch e.Service {
		case "bigquery":
			want = host.InCluster("bigquery")
		case "bigquery-storage":
			// gRPC only: no front, so the emulator's Service.
			want = "bigquery." + cfg.Cluster.Namespace + ".svc.cluster.local:9060"
		default:
			continue
		}
		if e.InCluster != want {
			t.Errorf("the banner gives %s's in-cluster address as %q, want %q", e.Service, e.InCluster, want)
		}
	}
}

// Only what the running instance recorded is listed: a service this
// invocation's flags enable but the instance did not bind, and a CLI-hosted
// service not published to the cluster, are absent rather than guessed.
func TestEnvKubernetesListsOnlyTheRunningInstancesServices(t *testing.T) {
	cfg := everyService(t)
	info := runtimeInfo{Endpoints: map[string]string{"storage": "127.0.0.1:1"},
		InCluster: map[string]string{"tasks": "cloudburrow-host.cloudburrow.svc.cluster.local:9002",
			"secretmanager": "127.0.0.1:9003 (served by the CLI, not the cluster)"}}
	got := map[string]string{}
	for _, v := range kubernetesEnvVars(cfg, info, "p-1") {
		got[v.Name] = v.Value
	}
	want := map[string]string{
		"STORAGE_EMULATOR_HOST":      "http://storage.cloudburrow.svc.cluster.local:" + fmt.Sprint(forwardTargets(cfg, config.ServiceStorage)[0].ServicePort),
		"CLOUDBURROW_TASKS_ENDPOINT": "cloudburrow-host.cloudburrow.svc.cluster.local:9002",
		"GOOGLE_CLOUD_PROJECT":       "p-1",
	}
	if !maps.Equal(got, want) {
		t.Errorf("kubernetes env = %v, want %v", got, want)
	}
}

// --offline is refused for the kubernetes format: the CLI-hosted services'
// in-cluster addresses exist only once a running `up` has published them.
// A running instance's are printed.
func TestEnvKubernetesNeedsARunningInstance(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{"env", "--offline", "--format", "kubernetes", "--name", "k8s", "--state-dir", dir}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "running instance") || stdout.Len() != 0 {
		t.Errorf("env --offline --format kubernetes = %v, stdout %q; want a refusal naming the running instance", err, stdout.String())
	}

	cfg, err := config.Load(config.Options{Args: []string{"--name", "k8s", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(runtimeInfo{PID: os.Getpid(), ProcStart: selfStart(t), Endpoints: map[string]string{"pubsub": "127.0.0.1:9701"},
		InCluster: map[string]string{"metadata": "cloudburrow-host.cloudburrow.svc.cluster.local:9004"}})
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"env", "--format", "kubernetes", "--name", "k8s", "--state-dir", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("env --format kubernetes = %v\n%s", err, stderr.String())
	}
	got := parseKubernetesEnv(t, stdout.String())
	if got["PUBSUB_EMULATOR_HOST"] != "pubsub.cloudburrow.svc.cluster.local:8085" ||
		got["GCE_METADATA_HOST"] != "cloudburrow-host.cloudburrow.svc.cluster.local:9004" || got["GOOGLE_CLOUD_PROJECT"] == "" {
		t.Errorf("env --format kubernetes for a running instance:\n%s", stdout.String())
	}
}
