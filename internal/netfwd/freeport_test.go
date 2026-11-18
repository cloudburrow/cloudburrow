package netfwd

import (
	"strconv"
	"strings"
	"testing"
)

// Two tunnels given one kubectl port carried each other's traffic on main's
// CI run 44326182935: freePort never hands a port out twice.
func TestFreePortNeverHandsAPortOutTwice(t *testing.T) {
	seen := map[int]bool{}
	for range 2000 {
		p, err := freePort(kubeBind)
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("freePort handed out %d twice", p)
		}
		seen[p] = true
	}
}

// A guarded tunnel whose kubectl could not bind its port moves to another,
// and its guard follows.
func TestRefusedKubePortIsRepicked(t *testing.T) {
	f := &Forwarder{target: Target{Name: "datastore", Guarded: true}, kubePort: 38095}
	f.Logf = t.Logf
	var out syncBuffer
	_, _ = out.Write([]byte("Forwarding from 127.0.0.1:38095 -> 8081\n"))
	f.repickRefusedKubePort(&out)
	if f.kubePort != 38095 {
		t.Fatalf("kubectl bound its port, and the tunnel moved to %d", f.kubePort)
	}
	_, _ = out.Write([]byte("error: unable to listen on any of the requested ports: [{38095 8081}]\n"))
	f.repickRefusedKubePort(&out)
	if f.kubePort == 38095 || f.kubePort == 0 {
		t.Fatalf("kubePort = %d; want a new port", f.kubePort)
	}
	if !strings.HasSuffix(f.kubeAddr(), ":"+strconv.Itoa(f.kubePort)) {
		t.Fatalf("kubeAddr = %s; want the new port %d", f.kubeAddr(), f.kubePort)
	}
}
