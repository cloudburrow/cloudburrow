package redisadmin

import "testing"

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
