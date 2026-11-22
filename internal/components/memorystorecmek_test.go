package components

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/rediscmek"
)

// A CMEK Memorystore server (#2113) runs under the keeper, copied in from
// the storage image: its own directory, /data, is a memory volume, and its
// claim is mounted at /sealed for the sealed files alone.
func TestMemorystoreCMEKManifest(t *testing.T) {
	wrapped := []byte("wrapped-by-kms")
	cm := &MemorystoreCMEK{Image: "cloudburrow-storage:dev", Key: "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k",
		KMS: "cloudburrow-host.default.svc.cluster.local:4000", Wrapped: wrapped}
	for _, persistent := range []bool{true, false} {
		b := MemorystoreServerOf(MemorystoreSettings{Name: "n", Persistent: persistent, MaxMemory: 1 << 30, RDBSeconds: 3600, CMEK: cm})
		m := b.Manifest("default", "w")
		for _, want := range []string{
			"initContainers:\n        - name: keeper\n          image: cloudburrow-storage:dev\n          imagePullPolicy: Never\n",
			`args: ["install-self", "/cloudburrow-keeper/cloudburrow-storage"]`,
			`command: ["/cloudburrow-keeper/cloudburrow-storage", "memorystore-cmek", "--plain", "/data", "--sealed", "/sealed", "--key", "` + cm.Key + `", "--kms", "` + cm.KMS + `", "--", "docker-entrypoint.sh"]`,
			`- name: ` + rediscmek.EnvWrapped + "\n              value: \"" + base64.StdEncoding.EncodeToString(wrapped) + "\"",
			"- name: plain\n              mountPath: /data\n",
			"- name: data\n              mountPath: /sealed\n",
			"- name: plain\n          emptyDir:\n            medium: Memory\n",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("persistent %v: manifest lacks %q:\n%s", persistent, want, m)
			}
		}
		claim := strings.Contains(m, "- name: data\n          persistentVolumeClaim:\n            claimName: n-data\n")
		if claim != persistent || strings.Contains(m, "- name: data\n          emptyDir: {}\n") == persistent {
			t.Errorf("persistent %v: the sealed files' volume:\n%s", persistent, m)
		}
		if !strings.Contains(strings.Join(b.Args, " "), "--dir /data") && persistent {
			t.Errorf("args = %q", b.Args)
		}
	}
	// persistence_config DISABLED drops the sealed files too.
	b := MemorystoreServerOf(MemorystoreSettings{Name: "n", Persistent: true, MaxMemory: 1 << 30, NoPersistence: true, CMEK: cm})
	if !strings.Contains(b.Manifest("default", "w"), `"--forget", "--", "docker-entrypoint.sh"`) {
		t.Errorf("no persistence: %v", b.Sealed.Args)
	}
	// Without a key, nothing changes.
	if b := MemorystoreServerOf(MemorystoreSettings{Name: "n", Persistent: true, MaxMemory: 1 << 30}); b.Sealed != nil ||
		strings.Contains(b.Manifest("default", "w"), "keeper") {
		t.Error("a server without CMEK has a keeper")
	}
}
