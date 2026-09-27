package components

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, keys).Args, " ")
	want := "--signing-key a@p.iam.gserviceaccount.com=QQ== --signing-key b@p.iam.gserviceaccount.com=Qg=="
	if !strings.Contains(args, want) {
		t.Errorf("args = %s; want %s", args, want)
	}
	if args := strings.Join(BuiltinStorageBackend("cloudburrow", "img", false, false, nil).Args, " "); strings.Contains(args, "--signing-key") {
		t.Errorf("no keys, yet args = %s", args)
	}
}

// The Bigtable emulator ships in the emulators image and is run directly:
// the install that fetched dl.google.com on every start crashed a restart
// with no DNS (#611). It runs only when the binary is missing, and the
// command is valid shell.
func TestBigtableStartsWithoutTheNetwork(t *testing.T) {
	b, ok := OptionalBackend(config.ServiceBigtable, "p", false)
	if !ok {
		t.Fatal("no Bigtable backend")
	}
	script := b.Command[len(b.Command)-1]
	if !strings.HasPrefix(script, "[ -x "+bigtableEmulator+" ] || gcloud components install bigtable") {
		t.Errorf("the install is not guarded by the binary's presence: %s", script)
	}
	dir := t.TempDir()
	emu := filepath.Join(dir, "cbtemulator")
	// A stand-in emulator and a gcloud that fails the test if it is run.
	if err := os.WriteFile(emu, []byte("#!/bin/sh\necho started \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte("#!/bin/sh\necho gcloud ran >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(s string) string {
		cmd := exec.Command("sh", "-c", strings.ReplaceAll(s, bigtableEmulator, emu))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := run(script); strings.Contains(out, "gcloud ran") || !strings.Contains(out, "started -host 0.0.0.0 -port 8086") {
		t.Errorf("with the emulator present: %q; want it started without gcloud", out)
	}
	_ = os.Remove(emu)
	if out := run(script); !strings.Contains(out, "gcloud ran") {
		t.Errorf("with the emulator missing: %q; want the install attempted", out)
	}
}
