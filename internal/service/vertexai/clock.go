package vertexai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The Agent Platform clock (#2214). A model deployment monitoring job's
// runs come every monitor_interval, an hour at least, and schedules'
// runs by their cron; tests and the console reach them by moving this
// clock forward, as the Kubernetes Engine and Memorystore clocks are
// moved. Google has no such call: ClockAdvancePath is CloudBurrow's own,
// on the local AI endpoint's REST port.

// ClockAdvancePath is POST {"duration": "3600s"}: the clock moves that
// far forward (never back), and the runs now due start. It answers
// {"now": RFC 3339, "advancedBy": duration}; a duration of 0s only reads
// the clock.
const ClockAdvancePath = "/cloudburrow/v1/clock:advance"

// clockNow is the clock: now, advanced.
func (s *Server) clockNow() time.Time { return time.Now().Add(time.Duration(s.clockOffset.Load())) }

// AdvanceClock moves the clock d forward and starts the runs now due; it
// returns the new time.
func (s *Server) AdvanceClock(d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, fmt.Errorf("duration %s is negative: the clock only moves forward", d)
	}
	s.clockOffset.Add(int64(d))
	if d > 0 {
		s.runDueSchedules()
	}
	return s.notebooks().now(), nil
}

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
		fail(http.StatusMethodNotAllowed, "POST "+ClockAdvancePath+` with {"duration": "3600s"}`)
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
	reply(http.StatusOK, map[string]any{"now": now.UTC().Format(time.RFC3339Nano), "advancedBy": fmt.Sprintf("%ds", s.clockOffset.Load()/int64(time.Second))})
}
