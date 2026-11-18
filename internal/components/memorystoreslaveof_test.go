package components

import (
	"strings"
	"testing"
)

// TestMemorystoreReplicaFlagByVersion (#1524): a Redis before 5.0 is
// started a replica with --slaveof, the only name it has; later ones and
// Valkey with --replicaof.
func TestMemorystoreReplicaFlagByVersion(t *testing.T) {
	for image, want := range map[string]string{
		MemorystoreRedis32Image: "--slaveof", MemorystoreRedis40Image: "--slaveof",
		MemorystoreRedis50Image: "--replicaof", MemorystoreRedis62Image: "--replicaof", "": "--replicaof",
	} {
		b := MemorystoreServerOf(MemorystoreSettings{Name: "n", MaxMemory: 1 << 30, Image: image, ReplicaOf: "p:6379"})
		args := strings.Join(b.Args, " ")
		if !strings.Contains(args, want+" p 6379") {
			t.Errorf("%s: %s, want %s p 6379", image, args, want)
		}
	}
}
