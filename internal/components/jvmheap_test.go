package components

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestJavaEmulatorsHaveABoundedHeapAndMemoryLimit: the Pub/Sub, Firestore
// and Datastore emulators are JVMs, which with no bound sized their heap at
// a quarter of the node's memory and grew it for as long as a soak ran
// (#1603). Each gets -Xmx, and an exit rather than a hang when the heap
// is full, through JAVA_TOOL_OPTIONS, which reaches the JVM
// however it is started, and a container memory request and limit sized
// from it; 0 leaves them as they were; no other backend gets either.
func TestJavaEmulatorsHaveABoundedHeapAndMemoryLimit(t *testing.T) {
	t.Parallel()
	jvms := map[string]bool{"pubsub": true, "firestore": true, "datastore": true}
	for _, mode := range []config.Mode{config.ModePersistent, config.ModeEphemeral} {
		for _, heap := range []int{config.DefaultEmulatorHeapMiB, 1024, 0} {
			var cfg config.Config
			cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceFirestore,
				config.ServiceDatastore, config.ServiceBigtable, config.ServiceBigQuery}
			cfg.Mode = mode
			cfg.Cluster.Namespace = "cloudburrow"
			cfg.EmulatorHeapMiB = heap
			c := NewLifecycleComponent("kc", cfg, io.Discard)
			c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
			seen := 0
			for _, b := range c.Backends() {
				m := b.Manifest("cloudburrow", "i")
				bounded := jvms[b.Name] && heap > 0
				if jvms[b.Name] {
					seen++
				}
				xmx := `value: "-Xmx` + strconv.Itoa(heap) + `m -XX:+ExitOnOutOfMemoryError"`
				limit := "limits:\n              memory: " + strconv.Itoa(heap+JVMOverheadMiB) + "Mi\n"
				request := "memory: " + strconv.Itoa(heap/2+256) + "Mi\n"
				if got := strings.Contains(m, "name: JAVA_TOOL_OPTIONS\n              "+xmx); got != bounded {
					t.Errorf("%s (%s, heap %d): -Xmx in env = %v, want %v:\n%s", b.Name, mode, heap, got, bounded, m)
				}
				if got := strings.Contains(m, limit) && strings.Contains(m, request); got != bounded {
					t.Errorf("%s (%s, heap %d): memory request and limit = %v, want %v:\n%s", b.Name, mode, heap, got, bounded, m)
				}
				if !bounded && (strings.Contains(m, "JAVA_TOOL_OPTIONS") || strings.Contains(m, "limits:")) {
					t.Errorf("%s (%s, heap %d): bounded, want unbounded:\n%s", b.Name, mode, heap, m)
				}
			}
			if seen != len(jvms) {
				t.Errorf("%s, heap %d: %d Java emulators, want %d", mode, heap, seen, len(jvms))
			}
		}
	}
}
