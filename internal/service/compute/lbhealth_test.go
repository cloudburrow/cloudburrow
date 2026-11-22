package compute

import (
	"context"
	"testing"
)

// TestLBProbeSeesAnInstanceStartLater: a managed group's VMs are still
// starting when their backend service is made; their probes must see them
// running once they are, not keep the endpoint they began with (#2153).
func TestLBProbeSeesAnInstanceStartLater(t *testing.T) {
	var h lbHealthState
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := lbProbeTask{key: "k", probe: hcProbe{interval: 3600, healthy: 1, unhealthy: 1}, ep: lbEndpoint{kind: "vm", running: false}, allowed: true}
	h.sync(ctx, nil, nil)
	h.mu.Lock()
	h.states = map[string]*hcState{"k": {cancel: func() {}, allowed: true, ep: task.ep}}
	h.mu.Unlock()
	task.ep.running = true
	h.sync(ctx, nil, []lbProbeTask{task})
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.states["k"].ep.running {
		t.Fatal("the probe kept the endpoint it began with")
	}
}
