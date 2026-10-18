package components

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestGoProcessesHaveAGoMemLimit: every Go process CloudBurrow runs in a
// pod has a GOMEMLIMIT and a memory request to match (#1618): the BigQuery
// emulator's from config.BigQueryHeapMiB (0 sets neither), the storage
// server's StorageGoMemLimitMiB, and each front's FrontGoMemLimitMiB. None
// gets a memory limit, which would kill it: GOMEMLIMIT is soft. The Java
// emulators' containers get no GOMEMLIMIT.
func TestGoProcessesHaveAGoMemLimit(t *testing.T) {
	t.Parallel()
	for _, heap := range []int{config.DefaultBigQueryHeapMiB, 1024, 0} {
		var cfg config.Config
		cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceFirestore,
			config.ServiceDatastore, config.ServiceBigtable, config.ServiceSpanner, config.ServiceBigQuery}
		cfg.Mode = config.ModeEphemeral
		cfg.Cluster.Namespace = "cloudburrow"
		cfg.BigQueryHeapMiB = heap
		c := NewLifecycleComponent("kc", cfg, io.Discard)
		c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
		fronts := 0
		for _, b := range c.Backends() {
			m := b.Manifest("cloudburrow", "i")
			var own string
			switch b.Name {
			case "storage":
				own = strconv.Itoa(StorageGoMemLimitMiB)
				if b.MemoryRequest != "64Mi" {
					t.Errorf("storage: memory request %q, want 64Mi", b.MemoryRequest)
				}
			case "bigquery":
				if heap > 0 {
					own = strconv.Itoa(heap)
					if want := strconv.Itoa(heap/2) + "Mi"; b.MemoryRequest != want {
						t.Errorf("bigquery (heap %d): memory request %q, want %q", heap, b.MemoryRequest, want)
					}
				} else if b.MemoryRequest != "" {
					t.Errorf("bigquery (heap 0): memory request %q, want the default", b.MemoryRequest)
				}
			}
			if got := b.Env["GOMEMLIMIT"]; own == "" && got != "" || own != "" && got != own+"MiB" {
				t.Errorf("%s (heap %d): backend GOMEMLIMIT %q, want %q", b.Name, heap, got, own)
			}
			if b.Front != nil {
				fronts++
				if got, want := b.Front.Env["GOMEMLIMIT"], strconv.Itoa(FrontGoMemLimitMiB)+"MiB"; got != want {
					t.Errorf("%s front: GOMEMLIMIT %q, want %q", b.Name, got, want)
				}
				if !strings.Contains(m, "        - name: front\n") ||
					!strings.Contains(m[strings.Index(m, "        - name: front\n"):], "name: GOMEMLIMIT\n              value: \""+strconv.Itoa(FrontGoMemLimitMiB)+"MiB\"") {
					t.Errorf("%s: the front's container has no GOMEMLIMIT:\n%s", b.Name, m)
				}
				if !strings.Contains(m, "cpu: 10m\n              memory: "+frontMemoryRequest+"\n") {
					t.Errorf("%s: the front's memory request is not %s:\n%s", b.Name, frontMemoryRequest, m)
				}
			}
			if !b.JVM && strings.Contains(m, "limits:") {
				t.Errorf("%s (heap %d): a memory limit, want none:\n%s", b.Name, heap, m)
			}
		}
		if fronts != 6 {
			t.Errorf("heap %d: %d fronts, want 6 (pubsub, firestore, datastore, bigtable, spanner, bigquery)", heap, fronts)
		}
	}
}
