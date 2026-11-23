package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestConcurrentClockReadsNeverSetTheEngineBack: a read of the clock
// (clock:advance of 0 seconds, as Data Transfer's scheduler makes every few
// seconds) that overlaps an advance never leaves the engine on the offset
// from before the advance (main run 45126544176: the engine's clock went
// back 49 hours, TestBigQueryAlterSchemaTimeTravelLetsGoFirst). The engine
// holds the read's setting until the advance has added its offset, which
// is the interleaving that set it back when the offset was read outside
// clockMu.
func TestConcurrentClockReadsNeverSetTheEngineBack(t *testing.T) {
	var mu sync.Mutex
	var last int64
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OffsetNanos int64 `json:"offsetNanos"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		entered <- struct{}{}
		if body.OffsetNanos == 0 {
			// The read's setting, of the offset before the advance:
			// held until the advance has had its chance to run.
			select {
			case <-release:
			case <-time.After(300 * time.Millisecond):
			}
		}
		mu.Lock()
		last = body.OffsetNanos
		mu.Unlock()
		writeJSON(w, http.StatusOK, body)
	})
	s := &streamBuffers{}
	advance := func(seconds string) {
		rec := httptest.NewRecorder()
		if !s.serveClock(rec, httptest.NewRequest(http.MethodPost, "/cloudburrow/bigquery/v2/clock:advance",
			strings.NewReader(`{"seconds": `+seconds+`}`)), engine) || rec.Code != http.StatusOK {
			t.Errorf("clock:advance %s: %d %s", seconds, rec.Code, rec.Body)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); advance("0") }()
	<-entered // the read is setting the engine
	wg.Add(1)
	go func() { defer wg.Done(); advance("176400") }()
	time.Sleep(50 * time.Millisecond) // the advance runs, or waits for the read
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if want := s.offset.Load(); last != want {
		t.Errorf("the engine's clock was left at offset %v, the front's is %v", time.Duration(last), time.Duration(want))
	}
}
