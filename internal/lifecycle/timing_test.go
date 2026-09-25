package lifecycle

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// Each component's start and duration is recorded, and /readyz reports them
// beside the readiness map (#312).
func TestReadyzReportsComponentTiming(t *testing.T) {
	rec := &recorder{}
	c := New(time.Second)
	clock := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	// Each call to now advances two seconds: component a takes 2s, b 2s.
	c.now = func() time.Time { clock = clock.Add(2 * time.Second); return clock }
	c.Register(&fakeComponent{name: "a", rec: rec}, &fakeComponent{name: "b", rec: rec})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.TimingOrder(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("TimingOrder = %v", got)
	}

	srv := NewControlServer(0, c)
	w := httptest.NewRecorder()
	srv.handleReady(w, httptest.NewRequest("GET", "/readyz", nil))
	var body struct {
		Components map[string]bool
		Timing     map[string]struct {
			StartedAt    time.Time `json:"started_at"`
			ReadyAfterMS int64     `json:"ready_after_ms"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Components["a"] || !body.Components["b"] {
		t.Errorf("components = %v", body.Components)
	}
	a, b := body.Timing["a"], body.Timing["b"]
	if a.ReadyAfterMS != 2000 || b.ReadyAfterMS != 2000 || !b.StartedAt.After(a.StartedAt) {
		t.Errorf("timing = %+v", body.Timing)
	}
}
