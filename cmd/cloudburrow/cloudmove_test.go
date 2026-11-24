package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// A move refuses before it stops anything: an instance whose compute is a
// VM already, one with no cluster on this machine, a VM name with no
// provisioner, and a target that is not ssh://.
func TestCloudMoveRefusesBeforeStoppingAnything(t *testing.T) {
	cfg := config.Config{Name: "mvcli", StateDir: t.TempDir()}
	clusters := []string{cfg.ClusterName()}
	old := kindClusters
	kindClusters = func(context.Context) ([]string, error) { return clusters, nil }
	t.Cleanup(func() { kindClusters = old })
	ctx := context.Background()

	if err := moveInstance(ctx, cfg, nil, "lab", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "no provisioner") {
		t.Fatalf("a VM name with no provisioner: %v", err)
	}
	if err := moveInstance(ctx, cfg, nil, "ssh://", nil, io.Discard); err == nil {
		t.Fatal("an ssh:// target with no host was taken")
	}

	clusters = nil
	if err := moveInstance(ctx, cfg, nil, "ssh://dev@203.0.113.5", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "no cluster on this machine") {
		t.Fatalf("no local cluster: %v", err)
	}

	clusters = []string{cfg.ClusterName()}
	if err := remotecompute.SaveRecord(cfg.InstanceDir(), remotecompute.Record{Target: remotecompute.Target{User: "dev", Host: "203.0.113.5", Port: 22}}); err != nil {
		t.Fatal(err)
	}
	if err := moveInstance(ctx, cfg, nil, "ssh://dev@203.0.113.6", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("an instance on a VM already: %v", err)
	}
}
