package byoc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// moveNeededEngine answers as remotecompute.Engine does for an instance
// whose cluster is on this machine's Docker.
type moveNeededEngine struct{ recordingEngine }

func (e *moveNeededEngine) Connect(_ context.Context, t byoc.RemoteTarget) error {
	return fmt.Errorf("%s: %w", t.Instance, byoc.ErrMoveNeeded)
}

func TestCreateForAnInstanceOnThisMachineSaysToMoveIt(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	m, err := byoc.NewManager(byoc.Options{Dir: f.dir, Keychain: f.keys, Drivers: []byoc.Driver{f.aws, f.azure}, Engine: &moveNeededEngine{},
		IPLookup: func(context.Context) (string, error) { return "203.0.113.9", nil }})
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Create(context.Background(), byoc.CreateRequest{Instance: "dev-vm", Connection: conn.ID, Region: "us-east-1"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := op.Events[len(op.Events)-1]
	if last.Step != "remote" || last.Status != byoc.StepDone || !strings.Contains(last.Message, "cloudburrow cloud move dev-vm --to dev-vm") {
		t.Fatalf("last event %+v", last)
	}
}

func TestAMovedInstanceUsesTheVM(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "lab-vm", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.m.SetUsedBy("lab-vm", "lab"); err != nil {
		t.Fatal(err)
	}
	tg, err := f.m.RemoteTarget("lab-vm")
	if err != nil || tg.Instance != "lab" {
		t.Fatalf("RemoteTarget = %+v, %v", tg, err)
	}
	// A start hands the VM to the instance that uses it.
	if _, err := f.m.Stop(ctx, "lab-vm", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Start(ctx, "lab-vm", true, nil); err != nil {
		t.Fatal(err)
	}
	f.engine.mu.Lock()
	got := f.engine.connected[len(f.engine.connected)-1].Instance
	closed := f.engine.closed[len(f.engine.closed)-1]
	f.engine.mu.Unlock()
	if got != "lab" || closed != "lab" {
		t.Fatalf("the start connected %q and the stop closed %q, not lab", got, closed)
	}
	if err := f.m.SetUsedBy("lab-vm", "lab-vm"); err != nil {
		t.Fatal(err)
	}
	if mc, _ := f.m.Machine("lab-vm"); mc.UsedBy != "" {
		t.Fatalf("UsedBy %q after it was given back", mc.UsedBy)
	}
}
