package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// A change across size families (#2209) is one guided action: a new VM of
// the size, the instance that runs on the old VM moved onto it, and the old
// VM deleted. A failed move deletes the new VM and leaves the old one.
func TestReplaceVMsMakesMovesAndDeletes(t *testing.T) {
	cfg := config.Config{Name: "lab", StateDir: t.TempDir()}
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: keychain.NewMemory(), Drivers: []byoc.Driver{byoctest.New(byoc.AWS)},
		IPLookup: func(context.Context) (string, error) { return "203.0.113.4", nil }, OpenBrowser: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: "A", SecretAccessKey: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, byoc.CreateRequest{Instance: "lab", Connection: conn.ID, Region: "us-east-1", Preset: byoc.Small, AutoStopMinutes: 45}, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := remotecompute.SaveRecord(cfg.InstanceDir(), remotecompute.Record{Target: remotecompute.Target{User: "ubuntu", Host: "198.51.100.7", Port: 22},
		Created: time.Now(), Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanResize(ctx, "lab", byoc.GPU, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NewVM || plan.NewName != "lab-gpu" || plan.DeltaUSD <= 0 || !strings.Contains(plan.Note, "lab is deleted") {
		t.Fatalf("plan %+v", plan)
	}

	old := moveInstanceFn
	t.Cleanup(func() { moveInstanceFn = old })
	var movedTo string
	moveErr := errors.New("node is not running after the move")
	moveInstanceFn = func(_ context.Context, c config.Config, _ *byoc.Manager, to string, _ moveStep, _ io.Writer) error {
		movedTo = c.Name + "->" + to
		return moveErr
	}
	var steps []string
	step := func(s, _ string) { steps = append(steps, s) }
	if err := replaceVMs(ctx, cfg, m, "lab", plan, step, nil, io.Discard); !errors.Is(err, moveErr) {
		t.Fatalf("a failed move: %v", err)
	}
	if movedTo != "lab->lab-gpu" {
		t.Fatalf("moved %q", movedTo)
	}
	if _, err := m.Machine("lab-gpu"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("the new VM stays after a failed move: %v", err)
	}
	if mc, err := m.Machine("lab"); err != nil || mc.State != byoc.Running {
		t.Fatalf("the old VM after a failed move: %+v %v", mc, err)
	}
	if got := strings.Join(steps, ","); got != "create,move,rollback" {
		t.Fatalf("steps %s", got)
	}

	moveErr, steps = nil, nil
	moveInstanceFn = func(_ context.Context, c config.Config, _ *byoc.Manager, to string, _ moveStep, _ io.Writer) error {
		movedTo = c.Name + "->" + to
		return nil
	}
	if err := replaceVMs(ctx, cfg, m, "lab", plan, step, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, ","); got != "create,move,delete,done" {
		t.Fatalf("steps %s", got)
	}
	nv, err := m.Machine("lab-gpu")
	if err != nil || nv.Preset != byoc.GPU || nv.AutoStopMinutes != 45 || nv.Region != "us-east-1" || nv.Connection != conn.ID {
		t.Fatalf("the new VM %+v %v", nv, err)
	}
	if _, err := m.Machine("lab"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("the old VM stays: %v", err)
	}
}
