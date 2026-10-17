package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOneInstanceClock (#1487): the times the front reports read its
// clock, which clock:advance moves, and an engine that restarted is given
// that clock again.
func TestOneInstanceClock(t *testing.T) {
	var engineOffsets []int64
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudburrow/clock" {
			var body struct {
				OffsetNanos int64 `json:"offsetNanos"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			engineOffsets = append(engineOffsets, body.OffsetNanos)
			writeJSON(w, http.StatusOK, body)
			return
		}
		http.NotFound(w, r)
	})
	s := &streamBuffers{}
	s.syncEngine(engine, t.Logf)
	if len(engineOffsets) != 0 {
		t.Fatalf("a clock never advanced was sent to the engine: %v", engineOffsets)
	}
	rec := httptest.NewRecorder()
	if !s.serveClock(rec, httptest.NewRequest(http.MethodPost, "/cloudburrow/bigquery/v2/clock:advance", strings.NewReader(`{"seconds": 7200}`)), engine) || rec.Code != http.StatusOK {
		t.Fatalf("clock:advance: %d %s", rec.Code, rec.Body)
	}
	f := front{buffers: s}
	if d := time.Until(f.now()); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Errorf("the front's now is %v ahead, want 2 h", d)
	}
	m := &bqModels{now: s.clock}
	if d := time.Until(m.clock()); d < 2*time.Hour-time.Minute {
		t.Errorf("the models' clock is %v ahead, want 2 h", d)
	}
	k := &knownFunctions{now: s.clock}
	k.reset()
	if d := time.Until(time.UnixMilli(k.started)); d < 2*time.Hour-time.Minute {
		t.Errorf("a reset's start is %v ahead, want 2 h", d)
	}
	s.syncEngine(engine, t.Logf) // the engine restarted
	want := int64(2 * time.Hour)
	if len(engineOffsets) != 2 || engineOffsets[0] != want || engineOffsets[1] != want {
		t.Errorf("the engine was sent the offsets %v, want %d twice", engineOffsets, want)
	}
}

// TestClockOutlivesAFrontRestart (#1513): the clock's offset is kept in
// the state directory, so a front that restarted reads the instance's
// clock still, and sets the engine's to it before the next request, again
// at the next one when that fails.
func TestClockOutlivesAFrontRestart(t *testing.T) {
	var engineOffsets []int64
	fail := false
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var body struct {
			OffsetNanos int64 `json:"offsetNanos"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		engineOffsets = append(engineOffsets, body.OffsetNanos)
		writeJSON(w, http.StatusOK, body)
	})
	path := clockStateFile(t.TempDir())
	s := &streamBuffers{}
	s.keepClock(path, t.Logf)
	s.syncPending(engine, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(engineOffsets) != 0 {
		t.Fatalf("a clock never advanced was sent to the engine: %v", engineOffsets)
	}
	rec := httptest.NewRecorder()
	if !s.serveClock(rec, httptest.NewRequest(http.MethodPost, "/cloudburrow/bigquery/v2/clock:advance", strings.NewReader(`{"seconds": 3600}`)), engine) || rec.Code != http.StatusOK {
		t.Fatalf("clock:advance: %d %s", rec.Code, rec.Body)
	}
	engineOffsets = nil

	restarted := &streamBuffers{}
	restarted.keepClock(path, t.Logf)
	if d := time.Until(restarted.clock()); d < time.Hour-time.Minute || d > time.Hour+time.Minute {
		t.Errorf("the restarted front's clock is %v ahead, want 1 h", d)
	}
	fail = true
	restarted.syncPending(engine, httptest.NewRequest(http.MethodGet, "/", nil))
	fail = false
	restarted.syncPending(engine, httptest.NewRequest(http.MethodGet, "/", nil))
	restarted.syncPending(engine, httptest.NewRequest(http.MethodGet, "/", nil))
	if want := int64(time.Hour); len(engineOffsets) != 1 || engineOffsets[0] != want {
		t.Errorf("the engine was sent %v after the restart, want %d once", engineOffsets, want)
	}
}
