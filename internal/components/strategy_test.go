package components

import (
	"strings"
	"testing"
)

// TestPersistentBackendsRecreate: a persistent backend's Deployment is
// replaced, never rolled, so a new pod never starts beside the old one on its
// ReadWriteOnce volume; an ephemeral one keeps the default rolling update.
func TestPersistentBackendsRecreate(t *testing.T) {
	persistent := Backend{Name: "datastore", Image: "img", Port: 8081, Persistent: true}
	if m := persistent.Manifest("ns", "ci"); !strings.Contains(m, "  strategy:\n    type: Recreate\n  selector:") {
		t.Errorf("a persistent backend's Deployment has no Recreate strategy:\n%s", m)
	}
	ephemeral := Backend{Name: "datastore", Image: "img", Port: 8081}
	if m := ephemeral.Manifest("ns", "ci"); strings.Contains(m, "strategy:") {
		t.Errorf("an ephemeral backend's Deployment names a strategy:\n%s", m)
	}
}
