package run

import "testing"

// A route Kourier has programmed is ready even while its load-balancer probe,
// which reaches the instance's own container, is not answered 200: the
// console's instance that serves no /healthz never became ready (#1348).
func TestKIngressReadyOnceProgrammed(t *testing.T) {
	waiting := ksvcCondition{Type: "LoadBalancerReady", Status: "Unknown", Message: "Waiting for load balancer to be ready"}
	for _, tc := range []struct {
		name  string
		conds []ksvcCondition
		ready bool
	}{
		{"ready", []ksvcCondition{{Type: "Ready", Status: "True"}}, true},
		{"programmed, probe unanswered", []ksvcCondition{waiting, {Type: "NetworkConfigured", Status: "True"},
			{Type: "Ready", Status: "Unknown", Message: waiting.Message}}, true},
		{"not programmed", []ksvcCondition{{Type: "NetworkConfigured", Status: "Unknown"},
			{Type: "Ready", Status: "Unknown", Message: "Waiting"}}, false},
		{"no status yet", nil, false},
	} {
		if got, msg := kingressReady(tc.conds); got != tc.ready || (!got && msg == "") {
			t.Errorf("%s: ready %v (%q), want %v", tc.name, got, msg, tc.ready)
		}
	}
}
