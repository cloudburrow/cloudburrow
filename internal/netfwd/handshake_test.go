package netfwd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A tunnel with a handshake is up only once the handshake succeeds through
// it (#1262): each failure tears the launch down and binds again, and
// Start returns once one passes.
func TestStartWaitsForTheHandshake(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, logged := newFakeForwarder(t, dir)
	var calls atomic.Int32
	f.SetHandshake(func(ctx context.Context, addr string) error {
		if addr != f.HostAddr() {
			t.Errorf("handshake through %s, want the host address %s", addr, f.HostAddr())
		}
		if calls.Add(1) < 3 {
			return errors.New("unexpected EOF")
		}
		return nil
	})
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("handshake ran %d times, want 3", n)
	}
	if got := launches(t, dir); len(got) != 3 {
		t.Errorf("launches = %v, want 3: a failed handshake binds afresh", got)
	}
	if !strings.Contains(logged(), "unexpected EOF") {
		t.Errorf("the log does not say why the tunnel was retried:\n%s", logged())
	}
}

// One that never succeeds fails Start, bounded, with the handshake's words.
func TestStartFailsWhenTheHandshakeNeverSucceeds(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, _ := newFakeForwarder(t, dir)
	old := HandshakeWait
	HandshakeWait = 2 * time.Second
	t.Cleanup(func() { HandshakeWait = old })
	f.SetHandshake(func(context.Context, string) error { return errors.New("access denied") })
	start := time.Now()
	err := f.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with a handshake that always fails")
	}
	if !errors.Is(err, ErrForwardFailed) || !strings.Contains(err.Error(), "access denied") || !strings.Contains(err.Error(), "did not serve within") {
		t.Errorf("Start = %v; want ErrForwardFailed naming the handshake's error and the bound", err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("Start took %s with a 2s bound", d)
	}
	if f.Running() {
		t.Error("the tunnel runs after a failed Start")
	}
}

// A handshake bounded by its own timeout: one through a stream that hangs
// does not hold Start forever.
func TestTheHandshakeIsGivenADeadline(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"spanner-a", true})
	f, _ := newFakeForwarder(t, dir)
	var calls atomic.Int32
	f.SetHandshake(func(ctx context.Context, _ string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the handshake's context has no deadline")
		}
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("handshake ran %d times, want 2", n)
	}
}
