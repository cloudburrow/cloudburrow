package components

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The builtin server is one Deployment with a real HTTP readiness request,
// a locally loaded image, and egress only to Pub/Sub and DNS (#514).
func TestStorageManifestSingleDeploymentHTTPReadiness(t *testing.T) {
	var cfg config.Config
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub}
	cfg.Mode = config.ModePersistent
	cfg.Cluster.Namespace = "cloudburrow"
	c := NewLifecycleComponent("kc", cfg, io.Discard)
	c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	var storage []Backend
	for _, b := range c.Backends() {
		if strings.HasPrefix(b.Name, "storage") {
			storage = append(storage, b)
		}
	}
	if len(storage) != 1 || storage[0].Name != "storage" {
		t.Fatalf("storage backends = %v; want exactly one", storage)
	}
	m := storage[0].Manifest("cloudburrow", "i")
	for _, want := range []string{
		"image: dev.local/cloudburrow-storage:abc", "imagePullPolicy: Never",
		"httpGet:", `path: "/storage/v1/b?project=_"`, "--mode\", \"persistent\"", "--data-dir\", \"/data\"",
		"--pubsub-emulator\", \"pubsub.cloudburrow.svc.cluster.local:8085\"", "claimName: storage-data",
		"kind: NetworkPolicy", "policyTypes: [Egress]", "app: pubsub", "port: 53",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "tcpSocket") || strings.Contains(m, "storage-internal") {
		t.Errorf("manifest still has the retired two-Deployment shape:\n%s", m)
	}

	cfg.Mode = config.ModeEphemeral
	eph := NewLifecycleComponent("kc", cfg, io.Discard)
	eph.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
	for _, b := range eph.Backends() {
		if b.Name == "storage" {
			m := b.Manifest("cloudburrow", "i")
			if strings.Contains(m, "persistentVolumeClaim") || strings.Contains(m, "--data-dir") || !strings.Contains(m, "--mode\", \"ephemeral\"") {
				t.Errorf("ephemeral mode mounts a volume or keeps a store:\n%s", m)
			}
		}
	}
}

// The signing keys go into the storage server's arguments sorted by
// account, so the manifest, and with it the Deployment, is the same on every
// `up`; with none, no flag is passed (#577).
func TestStorageManifestPassesSigningKeysSorted(t *testing.T) {
	keys := map[string][]byte{"b@p.iam.gserviceaccount.com": []byte("B"), "a@p.iam.gserviceaccount.com": []byte("A")}
	args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, keys, nil).Args, " ")
	want := "--signing-key a@p.iam.gserviceaccount.com=QQ== --signing-key b@p.iam.gserviceaccount.com=Qg=="
	if !strings.Contains(args, want) {
		t.Errorf("args = %s; want %s", args, want)
	}
	if args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, nil, nil).Args, " "); strings.Contains(args, "--signing-key") {
		t.Errorf("no keys, yet args = %s", args)
	}
}

// The Bigtable emulator ships in the emulators image and is its container's
// command: the install that fetched dl.google.com on every start crashed a
// restart with no DNS (#611), and the guarded fallback kept afterwards would
// still have reached the network had the binary been missing (#604).
func TestBigtableRunsTheEmulatorDirectly(t *testing.T) {
	b, ok := OptionalBackend(config.ServiceBigtable, "p", false)
	if !ok {
		t.Fatal("no Bigtable backend")
	}
	want := []string{bigtableEmulator, "-host", "0.0.0.0", "-port", "8086"}
	if strings.Join(b.Command, " ") != strings.Join(want, " ") {
		t.Errorf("command = %q, want %q", b.Command, want)
	}
}

// installAtStart matches a command that fetches software when a container
// starts: a package manager, the Cloud SDK's component installer, or a
// download tool.
var installAtStart = regexp.MustCompile(`(?i)\b(components\s+install|components\s+update|apt-get|apt\s+install|apk\s+add|yum|dnf|microdnf|pip3?\s+install|npm\s+(install|ci)|go\s+install|curl|wget)\b`)

// No backend installs anything when its container starts (#604): every
// component is in its digest-pinned image, so a pod start needs no network,
// and `prefetch` plus `up --offline` can stand behind that. Every service,
// in both modes, through Backends() as up calls it.
func TestNoBackendInstallsAtContainerStart(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeEphemeral, config.ModePersistent} {
		var cfg config.Config
		cfg.Services = config.KnownServices()
		cfg.Mode = mode
		cfg.Cluster.Namespace = "cloudburrow"
		c := NewLifecycleComponent("kubeconfig", cfg, io.Discard)
		c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:test")
		backends := c.Backends()
		if len(backends) == 0 {
			t.Fatal("no backends")
		}
		for _, b := range backends {
			for _, part := range [][]string{b.Command, b.Args} {
				if m := installAtStart.FindString(strings.Join(part, " ")); m != "" {
					t.Errorf("%s (%s): container start runs %q: %q", b.Name, mode, m, part)
				}
			}
		}
	}
}

// An unrouted backend with a tunnel Service removes the EndpointSlice a
// routed run left (#881), which would otherwise still send part of its
// Service's traffic to the host; a routed one keeps it, and removes what
// an unrouted run's selector left, which would send traffic past the front.
func TestInstallRemovesAStaleRoutedEndpointSlice(t *testing.T) {
	for _, routed := range []bool{false, true} {
		r := &recordingRunner{}
		b := bigQueryBackend("p")
		b.Routed = routed
		if err := newTestInstaller(r).InstallBackends(context.Background(), []Backend{b}, time.Second); err != nil {
			t.Fatal(err)
		}
		stale := r.find("delete endpointslice -l cloudburrow.dev/routes=bigquery")
		if routed == (stale != "") {
			t.Errorf("routed=%v: removes the routed slice: %q", routed, stale)
		}
		// Routed, what the selector left is removed: the Endpoints, then
		// every slice but CloudBurrow's own.
		eps := r.find("-n cloudburrow delete endpoints bigquery --ignore-not-found")
		slices := r.find("delete endpointslice -l kubernetes.io/service-name=bigquery,endpointslice.kubernetes.io/managed-by!=cloudburrow.dev --ignore-not-found")
		if routed != (eps != "" && slices != "") {
			t.Errorf("routed=%v: %q %q", routed, eps, slices)
		}
	}
}
