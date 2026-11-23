package netfwd

import (
	"net"
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

// The host port New chooses stays held until the tunnel binds it: main run
// 45147981577's restarted instance found forward:memorystore's chosen port
// another process's while its cluster started, and failed to start.
func TestChosenHostPortIsHeldUntilReleased(t *testing.T) {
	f := New(Target{Name: "memorystore", Namespace: "n", ServicePort: 6379}, "/tmp/kc", "")
	addr := f.HostAddr()
	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("another process could bind %s before the tunnel started", addr)
	}
	f.release()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("after release the tunnel could not bind %s: %v", addr, err)
	}
	_ = l.Close()
	f.release() // a second release is a no-op

	// A requested port is the caller's and is not held.
	g := New(Target{Name: "s", Namespace: "n", ServicePort: 1, HostPort: 19998}, "/tmp/kc", "")
	if g.held != nil {
		t.Error("a requested host port was held")
	}
}
