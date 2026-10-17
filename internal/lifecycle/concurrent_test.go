package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// gateComponent blocks in Start until every member of its group has
// entered Start, which only a concurrent start can satisfy.
type gateComponent struct {
	name     string
	rec      *recorder
	arrived  *sync.WaitGroup
	startErr error
	// waitCtx makes Start return only when its context is cancelled.
	waitCtx bool
}

func (g *gateComponent) Name() string { return g.name }

func (g *gateComponent) Start(ctx context.Context) error {
	g.arrived.Done()
	done := make(chan struct{})
	go func() { g.arrived.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return errors.New("the others never started: the group ran one at a time")
	}
	if g.waitCtx {
		<-ctx.Done()
		return ctx.Err()
	}
	if g.startErr == nil {
		g.rec.record("start:" + g.name)
	}
	return g.startErr
}

func (g *gateComponent) Stop(context.Context) error {
	g.rec.record("stop:" + g.name)
	return nil
}

func TestRegisterConcurrentStartsTogetherAndStopsInReverse(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	var arrived sync.WaitGroup
	arrived.Add(3)
	c := New(time.Second)
	c.Register(&fakeComponent{name: "before", rec: rec})
	c.RegisterConcurrent(
		&gateComponent{name: "a", rec: rec, arrived: &arrived},
		&gateComponent{name: "b", rec: rec, arrived: &arrived},
		&gateComponent{name: "c", rec: rec, arrived: &arrived},
	)
	c.Register(&fakeComponent{name: "after", rec: rec})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := rec.snapshot()
	if calls[0] != "start:before" || calls[len(calls)-1] != "start:after" {
		t.Fatalf("the group did not start between its neighbours: %v", calls)
	}
	ready := c.ReadyComponents()
	for _, n := range []string{"before", "a", "b", "c", "after"} {
		if !ready[n] {
			t.Errorf("%s not ready: %v", n, ready)
		}
		if _, ok := c.Timings()[n]; !ok {
			t.Errorf("%s has no timing", n)
		}
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stops := rec.snapshot()[len(calls):]
	want := []string{"stop:after", "stop:c", "stop:b", "stop:a", "stop:before"}
	if !reflect.DeepEqual(stops, want) {
		t.Fatalf("stops = %v, want %v", stops, want)
	}
}

func TestRegisterConcurrentFailureCancelsAndUnwinds(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	var arrived sync.WaitGroup
	arrived.Add(3)
	boom := errors.New("boom")
	c := New(time.Second)
	c.Register(&fakeComponent{name: "before", rec: rec})
	c.RegisterConcurrent(
		&gateComponent{name: "ok", rec: rec, arrived: &arrived},
		&gateComponent{name: "waits", rec: rec, arrived: &arrived, waitCtx: true},
		&gateComponent{name: "fails", rec: rec, arrived: &arrived, startErr: boom},
	)
	c.Register(&fakeComponent{name: "after", rec: rec})
	err := c.Start(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want the member's own failure, not the cancellation it caused", err)
	}
	if c.State() != StateFailed {
		t.Fatalf("state = %s", c.State())
	}
	calls := rec.snapshot()
	for _, s := range calls {
		if s == "start:after" {
			t.Fatalf("a component after a failed group started: %v", calls)
		}
	}
	// What started is stopped; what failed or was cancelled is not.
	stopped := map[string]bool{}
	for _, s := range calls {
		stopped[s] = true
	}
	if !stopped["stop:ok"] || !stopped["stop:before"] || stopped["stop:fails"] || stopped["stop:waits"] {
		t.Fatalf("unwind = %v", calls)
	}
	if c.ReadyComponents()["fails"] {
		t.Fatal("the failed member is reported ready")
	}
}

func TestRegisterConcurrentEmptyIsNothing(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(time.Second)
	c.Register(&fakeComponent{name: "a", rec: rec})
	c.RegisterConcurrent()
	c.Register(&fakeComponent{name: "b", rec: rec})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.ComponentNames(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("names = %v", got)
	}
}
