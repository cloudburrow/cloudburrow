package gke

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The Kubernetes Engine clock (#2076). Automatic upgrades run only inside
// a cluster's maintenance window, and one that runs past the window's end
// is paused until the next (upgrade.go); a window is hours or days away,
// so tests and the console reach it by moving this clock forward, as the
// Memorystore clock is moved (redisadmin, #1909). Google has no such call:
// ClockAdvancePath is CloudBurrow's own, on the API's REST port.

// ClockAdvancePath is POST {"duration": "3600s"}: the clock moves that
// far forward (never back), and the automatic upgrades now due start. It
// answers {"now": RFC 3339, "advancedBy": duration}. A duration of 0s
// only reads the clock.
const ClockAdvancePath = "/cloudburrow/v1/clock:advance"

// now is the Kubernetes Engine clock: Options.Now, advanced.
func (s *Server) now() time.Time {
	return s.opts.Now().Add(time.Duration(s.clockOffset.Load()))
}

// Now is the Kubernetes Engine clock.
func (s *Server) Now() time.Time { return s.now() }

// AdvanceClock moves the clock d forward and starts the automatic
// upgrades now due; it returns the new time.
func (s *Server) AdvanceClock(d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, fmt.Errorf("duration %s is negative: the clock only moves forward", d)
	}
	s.clockOffset.Add(int64(d))
	if d > 0 {
		s.AutoUpgrade()
	}
	return s.now(), nil
}

// AdvancedBy is how far the clock has been moved forward.
func (s *Server) AdvancedBy() time.Duration { return time.Duration(s.clockOffset.Load()) }

func (s *Server) serveClock(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(code int, msg string) {
		reply(code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	if r.Method != http.MethodPost {
		fail(http.StatusMethodNotAllowed, "POST "+ClockAdvancePath+" with {\"duration\": \"3600s\"}")
		return
	}
	var body struct {
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(http.StatusBadRequest, "the body is not JSON: "+err.Error())
		return
	}
	d, err := time.ParseDuration(body.Duration)
	if err != nil {
		fail(http.StatusBadRequest, fmt.Sprintf("duration %q is not a duration such as 3600s", body.Duration))
		return
	}
	now, err := s.AdvanceClock(d)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	reply(http.StatusOK, map[string]any{"now": now.UTC().Format(time.RFC3339Nano), "advancedBy": fmt.Sprintf("%ds", int64(s.AdvancedBy()/time.Second))})
}
