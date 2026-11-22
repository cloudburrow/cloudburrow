package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNodeZonesStayWithTheirNodes (#2100): a pool's new nodes go to the
// zone with the fewest of its nodes and keep that zone; when a zone is
// removed, its nodes are the ones outside the pool's zones and the first
// a shrink removes, and the others keep theirs; a shrink otherwise takes
// from the zone with the most nodes; a removed node is forgotten; a zone
// file from before nodes had their own (a list of zones) still reads.
func TestNodeZonesStayWithTheirNodes(t *testing.T) {
	g := &GKE{Dir: t.TempDir()}
	const kind, pool = OwnerPrefix + "z", "default-pool"
	if err := os.MkdirAll(filepath.Join(g.Dir, kind), 0o700); err != nil {
		t.Fatal(err)
	}
	if z := g.nodeZone(kind, pool, "n-1"); z != "" {
		t.Errorf("a node of a pool with no zones has zone %q", z)
	}
	if err := g.writeZones(kind, pool, poolZones{Zones: []string{"us-central1-a", "us-central1-b", "us-central1-c"}}); err != nil {
		t.Fatal(err)
	}
	nodes := []string{"n-1", "n-2", "n-3", "n-4", "n-5", "n-6"}
	var got []string
	for _, n := range nodes {
		got = append(got, g.nodeZone(kind, pool, n))
	}
	if want := []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-a", "us-central1-b", "us-central1-c"}; !slices.Equal(got, want) {
		t.Errorf("zones given = %v, want %v", got, want)
	}
	if z := g.nodeZone(kind, pool, "n-2"); z != "us-central1-b" {
		t.Errorf("n-2 asked again is in %s", z)
	}
	if l := g.zoneLabels(kind, pool, "n-3"); l != ",topology.kubernetes.io/zone=us-central1-c,topology.kubernetes.io/region=us-central1" {
		t.Errorf("zone labels = %q", l)
	}
	// Zone b removed: its nodes are outside, the rest keep their zones.
	pz := g.readZones(kind, pool)
	pz.Zones = []string{"us-central1-a", "us-central1-c"}
	if err := g.writeZones(kind, pool, pz); err != nil {
		t.Fatal(err)
	}
	if out := g.outsideZones(kind, pool, nodes); !slices.Equal(out, []string{"n-2", "n-5"}) {
		t.Errorf("outside the zones = %v", out)
	}
	if o := g.shrinkOrder(kind, pool, nodes); !slices.Equal(o[:2], []string{"n-5", "n-2"}) {
		t.Errorf("shrink order = %v, want the removed zone's first", o)
	}
	g.forgetNode(kind, pool, "n-2")
	g.forgetNode(kind, pool, "n-5")
	left := []string{"n-1", "n-3", "n-4", "n-6"}
	for _, n := range left {
		if z := g.nodeZone(kind, pool, n); z == "us-central1-b" {
			t.Errorf("%s moved to the removed zone", n)
		}
	}
	// A new node goes to the zone with the fewest: a tie, the first.
	if z := g.nodeZone(kind, pool, "n-7"); z != "us-central1-a" {
		t.Errorf("n-7 in %s", z)
	}
	// Three in a, two in c: a shrink of one takes a's highest index.
	if o := g.shrinkOrder(kind, pool, append(left, "n-7")); o[0] != "n-7" {
		t.Errorf("shrink order = %v, want n-7 (zone a has the most) first", o)
	}
	// The old format: zones one per line, no nodes.
	if err := os.WriteFile(g.zonesFile(kind, "old"), []byte("us-east1-b\nus-east1-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if z := g.nodeZone(kind, "old", "o-1"); z != "us-east1-b" {
		t.Errorf("an old file's first node is in %q", z)
	}
	if z := g.nodeZone(kind, "old", "o-2"); z != "us-east1-c" {
		t.Errorf("an old file's second node is in %q", z)
	}
	// No zones: highest index first.
	if o := (&GKE{Dir: g.Dir}).shrinkOrder(kind, "none", []string{"x-1", "x-2", "x-3"}); !slices.Equal(o, []string{"x-3", "x-2", "x-1"}) {
		t.Errorf("shrink order without zones = %v", o)
	}
}

// TestKeepEndpointPutsTheRulesBack (#2100): a cluster on its first port
// with no rotation runs nothing; after a rotation completed onto the spare
// port, the redirect is added and the old port's refusal too when a check
// (-C) finds them missing, as after a control plane restart; during a
// rotation both ports serve.
func TestKeepEndpointPutsTheRulesBack(t *testing.T) {
	r := newRunner()
	g := &GKE{Dir: t.TempDir(), Runner: r}
	kind := OwnerPrefix + "ep"
	if err := os.MkdirAll(filepath.Join(g.Dir, kind), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := g.KeepEndpoint(context.Background(), kind); err != nil || len(r.calls) != 0 {
		t.Fatalf("an unrotated cluster: %v, ran %v", err, r.calls)
	}
	r.script("docker port", "127.0.0.1:41234\n", nil).
		script("docker network inspect", "172.18.0.1 fc00::1", nil).
		script("docker exec "+kind+"-control-plane iptables -t nat -C", "", errors.New("no rule")).
		script("docker exec "+kind+"-control-plane iptables -C", "", errors.New("no rule"))
	if err := g.saveEndpointPorts(kind, endpointState{Serving: spareAPIPort}); err != nil {
		t.Fatal(err)
	}
	if err := g.KeepEndpoint(context.Background(), kind); err != nil {
		t.Fatal(err)
	}
	if !r.ran("iptables -t nat -A PREROUTING -p tcp --dport 6444 -j REDIRECT --to-ports 6443") {
		t.Errorf("the spare port's redirect was not put back: %v", r.calls)
	}
	if !r.ran("iptables -A INPUT -p tcp -s 172.18.0.1 --dport 6443") {
		t.Errorf("the old port's refusal was not put back: %v", r.calls)
	}
	r.calls = nil
	if err := g.saveEndpointPorts(kind, endpointState{Serving: defaultAPIPort, Next: spareAPIPort}); err != nil {
		t.Fatal(err)
	}
	if err := g.KeepEndpoint(context.Background(), kind); err != nil {
		t.Fatal(err)
	}
	if !r.ran("iptables -t nat -A PREROUTING") || r.ran("iptables -A INPUT") {
		t.Errorf("during a rotation both ports should serve: %v", r.calls)
	}
}

// TestJoinConfigCarriesThePoolsKubelet (#2100): kubeadm join takes the
// kubelet's configuration from the cluster, so a pool's kubelet settings
// are a kubeadm merge patch the join configuration points at; a pool
// without settings has no patches.
func TestJoinConfigCarriesThePoolsKubelet(t *testing.T) {
	settings := map[string]any{"podPidsLimit": 4096, "kubeReserved": map[string]any{"cpu": "100m"}}
	got := joinConfig("k", "n", "10.0.0.2", "t", "p", settings)
	if !strings.Contains(got, "patches:\n  directory: "+kubeletPatchDir+"\n") || !strings.Contains(got, "kind: KubeletConfiguration\n") {
		t.Errorf("join config with kubelet settings:\n%s", got)
	}
	if strings.Contains(joinConfig("k", "n", "10.0.0.2", "t", "p", nil), "patches:") {
		t.Error("a pool without kubelet settings got patches")
	}
	patch, err := kubeletPatch(settings)
	if err != nil || patch != `{"kubeReserved":{"cpu":"100m"},"podPidsLimit":4096}` {
		t.Errorf("patch = %s, %v", patch, err)
	}
	if p, _ := kubeletPatch(nil); p != "" {
		t.Errorf("no settings patched %q", p)
	}
}
