package redisadmin

import (
	"context"
	"testing"
)

// TestTrustOf (#1563): the CA bundle every member of cross-instance
// replication trusts holds each member's CA once, and none of a member
// without in-transit encryption; a node's trust is its own CA and its
// peers'.
func TestTrustOf(t *testing.T) {
	a, b := &clusterRecord{TransitCA: "A\n"}, &clusterRecord{TransitCA: "B\n"}
	if got := trustOf([]*clusterRecord{a, b, a, {}}); got != "A\nB\n" {
		t.Errorf("trustOf = %q, want both CAs once", got)
	}
	if got := trustOf([]*clusterRecord{{}, {}}); got != "" {
		t.Errorf("trustOf without in-transit encryption = %q, want none", got)
	}
	a.Valkey = &valkeyFields{CIR: &cirConfig{PeerCAs: "A\nB\n"}}
	if s := a.spec("n"); s.TransitTrust != "A\nA\nB\n" {
		t.Errorf("a member's node trusts %q, want its CA and its peers'", s.TransitTrust)
	}
}

// TestSourceOf: a secondary replicates its primary instance's primary
// Service when that instance has a replica (it fails over), the primary
// node's own Service when it has none, and its pod's IP without a
// RoleRouter (#1614).
func TestSourceOf(t *testing.T) {
	name := "projects/p/locations/us-central1/instances/src"
	base := ValkeyServerBase(name)
	two := &clusterRecord{Nodes: []*clusterNode{
		{Server: base + "-0", Primary: true, podIP: "10.0.0.1", port: 30001},
		{Server: base + "-1", podIP: "10.0.0.2", port: 30002}}}
	two.Name, two.AdminPassword = name, "pw"
	one := &clusterRecord{Nodes: two.Nodes[:1]}
	one.Name = name
	rr := &clusters{rt: &vkNodes{t: t}}
	got := rr.sourceOf(two)
	if got.host != base+"-primary.svc" || got.port != 6379 || got.password != "pw" || got.base != base ||
		got.roles[base+"-0"] != rolePrimary || got.roles[base+"-1"] != roleReplica {
		t.Errorf("with a replica: %+v", got)
	}
	two.TransitCA = "ca"
	if got := rr.sourceOf(two); got.port != TLSPort {
		t.Errorf("with in-transit encryption: port %d, want %d", got.port, TLSPort)
	}
	if got := rr.sourceOf(one); got.host != base+"-0.svc" || got.port != 30001 || got.roles != nil {
		t.Errorf("without a replica: %+v", got)
	}
	if got := (&clusters{rt: plainRuntime{}}).sourceOf(two); got.host != "10.0.0.1" || got.port != 30001 {
		t.Errorf("without a RoleRouter: %+v", got)
	}
}

// plainRuntime is a ClusterRuntime that is no RoleRouter.
type plainRuntime struct{}

func (plainRuntime) StartClusterNode(context.Context, Spec) (string, string, int, error) {
	return "", "", 0, nil
}

func (plainRuntime) Remove(context.Context, string) error { return nil }
