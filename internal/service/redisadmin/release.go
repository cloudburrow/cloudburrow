package redisadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A release (#1909): the opt-in way for maintenance to be scheduled in
// each instance's window on its own, as Google does when it releases a
// new version ("Memorystore ... sends a notification at least one week
// before maintenance is scheduled", in the instance's maintenance window:
// docs.cloud.google.com/memorystore/docs/redis/about-maintenance,
// /cluster/about-maintenance, /valkey/about-maintenance). CloudBurrow's
// pinned server images never change under a running instance, so no
// release comes by itself; POST MaintenanceControlPath +
// projects/P/locations/L:releaseUpdate (L a region or "-" for all)
// stands in for one: every Memorystore for Redis instance, Redis cluster
// and Valkey instance of the project (and region) with no maintenance
// scheduled has one scheduled at the first start of its weekly window
// at least a week away, or a week on, to the hour, with no policy. From
// then on it is the scheduled maintenance the APIs report
// (maintenance_schedule), RescheduleMaintenance moves, and that is
// applied when it is due. It is CloudBurrow's own call, not a
// Memorystore API, and an instance with an event already keeps it.

var releaseRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)

// ReleaseUpdate schedules an update on every instance of project in
// region ("-" for all) with none scheduled, and returns their names.
func (s *Server) ReleaseUpdate(ctx context.Context, project, region string) ([]string, error) {
	in := func(name string) bool {
		m := nodesOwnerRE.FindStringSubmatch(name)
		if m == nil {
			m = instanceRE.FindStringSubmatch(name)
		}
		return m != nil && m[1] == project && (region == "-" || m[2] == region)
	}
	var out []string
	if c := s.created; c != nil {
		c.mu.Lock()
		var names []string
		for name, r := range c.byID {
			if r.Scheduled == nil && in(name) {
				names = append(names, name)
			}
		}
		c.mu.Unlock()
		for _, name := range names {
			if _, err := s.ScheduleMaintenance(ctx, name, time.Time{}); err != nil {
				return out, err
			}
			out = append(out, name)
		}
	}
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		c.mu.Lock()
		var names []string
		for name, r := range c.byName {
			if r.Scheduled == nil && in(name) {
				names = append(names, name)
			}
		}
		c.mu.Unlock()
		for _, name := range names {
			if _, err := c.scheduleMaintenance(name, time.Time{}); err != nil {
				return out, err
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// serveRelease serves :releaseUpdate on path, projects/P/locations/L.
func (s *Server) serveRelease(w http.ResponseWriter, r *http.Request, path string, fail func(int, string)) {
	m := releaseRE.FindStringSubmatch(path)
	if m == nil || r.Method != http.MethodPost {
		fail(http.StatusNotFound, "POST "+MaintenanceControlPath+"projects/P/locations/L:releaseUpdate, L a region or -")
		return
	}
	names, err := s.ReleaseUpdate(r.Context(), m[1], m[2])
	if err != nil {
		code := http.StatusInternalServerError
		if e, ok := err.(*apierror.Error); ok {
			code = e.HTTPStatus()
		}
		fail(code, err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"scheduled": names})
}

// isRelease is whether a control path is a release's.
func isRelease(p string) (string, bool) {
	return strings.CutSuffix(p, ":releaseUpdate")
}

// The Memorystore clock (#1909): what every instance's, cluster's and
// Valkey instance's maintenance runs by, so a test can reach a window a
// week away. POST MaintenanceControlPath + "clock:advance" with
// {"duration": "604800s"} moves it forward (never back), as Cloud SQL's
// /cloudburrow/v1/clock:advance moves Cloud SQL's; every scheduled event
// whose start it passes is then applied, as when its time comes. The
// offset lives in memory: `cloudburrow up` starts at the host's time.
// Create, update and operation times follow the clock too.

// ClockAdvancePath is the clock's control path.
const ClockAdvancePath = MaintenanceControlPath + "clock:advance"

// now is the Memorystore clock.
func (s *Server) now() time.Time { return time.Now().Add(time.Duration(s.clockOffset.Load())) }

// AdvanceClock moves the clock d forward and starts every event now due.
func (s *Server) AdvanceClock(d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, apierror.InvalidArgument("duration %s is negative: the clock only moves forward", d)
	}
	s.clockOffset.Add(int64(d))
	if s.created != nil {
		go s.created.maintenanceDue()
	}
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c != nil {
			go c.maintenanceDue()
		}
	}
	return s.now(), nil
}

func (s *Server) serveClock(w http.ResponseWriter, r *http.Request, fail func(int, string)) {
	var body struct {
		Duration string `json:"duration"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
		fail(http.StatusBadRequest, `POST `+ClockAdvancePath+` {"duration": "3600s"}`)
		return
	}
	d, err := time.ParseDuration(body.Duration)
	if err != nil {
		fail(http.StatusBadRequest, "duration "+body.Duration+" is not a duration such as 3600s")
		return
	}
	now, err := s.AdvanceClock(d)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"now": now.UTC().Format(time.RFC3339Nano)})
}
