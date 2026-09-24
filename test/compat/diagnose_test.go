//go:build compat

package compat

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// TestDiagnoseBundleHoldsNoCredentials.
//
// `cloudburrow diagnose` (#293) against the CI instance: the bundle has
// what a bug report needs, and none of what it must not have. A secret is
// created with a known payload first, and the bundle is searched, every file
// of it, for that payload, the ADC fixture's private key and the
// kubeconfig's client key.
func TestDiagnoseBundleHoldsNoCredentials(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	instanceDir := instanceDirFrom(t, flags)

	const payload = "diagnose-canary-7f3c9e1d2b"
	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "diagnose-canary",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })
	if _, err := sc.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{
		Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)}}); err != nil {
		t.Fatal(err)
	}

	forbidden := map[string]string{"the secret payload": payload}
	kc, err := os.ReadFile(filepath.Join(instanceDir, "kubeconfig"))
	if err != nil {
		t.Fatalf("reading the instance kubeconfig: %v", err)
	}
	for _, field := range []string{"client-key-data", "client-certificate-data", "token"} {
		if m := regexp.MustCompile(field + `:\s*(\S+)`).FindSubmatch(kc); m != nil {
			forbidden["kubeconfig "+field] = string(m[1])
		}
	}
	if _, ok := forbidden["kubeconfig client-key-data"]; !ok {
		t.Fatal("the kubeconfig has no client-key-data: this test would check nothing")
	}
	files, _ := filepath.Glob(filepath.Join(instanceDir, "*.json"))
	for _, f := range files {
		var adc struct {
			PrivateKey string `json:"private_key"`
		}
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &adc) == nil && adc.PrivateKey != "" {
			forbidden["the ADC private key in "+filepath.Base(f)] = adc.PrivateKey
			// A PEM key is also searched for by its body alone, in case a
			// file held it with its newlines changed.
			for _, line := range strings.Split(adc.PrivateKey, "\n") {
				if len(line) > 40 {
					forbidden["a line of the ADC private key"] = line
					break
				}
			}
		}
	}
	if len(forbidden) < 3 {
		t.Logf("no ADC fixture found under %s; checking the secret and the kubeconfig only", instanceDir)
	}

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if b, err := exec.Command(cli, append([]string{"diagnose", "-o", out}, flags...)...).CombinedOutput(); err != nil {
		t.Fatalf("cloudburrow diagnose: %v\n%s", err, b)
	}
	got := readBundle(t, out)
	for _, want := range []string{"manifest.json", "version.txt", "config.json", "doctor.txt", "readyz.json",
		"status.json", "runtime.json", "forwarders.json", "admin-events.json",
		"kubernetes/pods.json", "kubernetes/events.json", "logs.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("the bundle has no %s; it has %v", want, keys(got))
		}
	}
	var m struct {
		ClusterUp bool `json:"cluster_up"`
		Steps     []struct{ Step, Status, Reason string }
	}
	if err := json.Unmarshal(got["manifest.json"], &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if !m.ClusterUp {
		t.Error("manifest says the cluster is down, against a running instance")
	}
	for _, s := range m.Steps {
		if s.Status != "ok" && s.Step != "knative services" {
			t.Errorf("step %q %s: %s", s.Step, s.Status, s.Reason)
		}
	}
	for name, data := range got {
		for what, v := range forbidden {
			if strings.Contains(string(data), v) {
				t.Errorf("%s holds %s", name, what)
			}
		}
	}
}

func instanceDirFrom(t *testing.T, flags []string) string {
	t.Helper()
	name, dir := "", ""
	for i := 0; i+1 < len(flags); i++ {
		switch flags[i] {
		case "--name":
			name = flags[i+1]
		case "--state-dir":
			dir = flags[i+1]
		}
	}
	if name == "" || dir == "" {
		t.Skipf("%s does not name the instance and its state dir", EnvCLIArgs)
	}
	return filepath.Join(dir, name)
}

func readBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = b
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
