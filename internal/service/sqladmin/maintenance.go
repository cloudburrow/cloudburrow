package sqladmin

// Maintenance events (#1524). Cloud SQL schedules a maintenance update on
// an instance, reports it as scheduledMaintenance (startTime,
// canReschedule, scheduleDeadlineTime), lets instances.rescheduleMaintenance
// move it, and at its start time restarts the instance, which "becomes
// unavailable" while "existing connections are dropped", in an operation
// of type MAINTENANCE. CloudBurrow's servers are their versions' pinned
// images, so Google's own updates never come; an event is scheduled
// locally instead, through the CloudBurrow control endpoint
// MaintenanceControlPrefix (not a Cloud SQL API) or the console's
// Schedule maintenance, and from then on behaves as Google documents
// (docs.cloud.google.com/sql/docs/mysql/maintenance and
// /set-maintenance-window, read 2026-11-18):
//
//   - The event starts in the instance's maintenanceWindow (day 1, Monday,
//     to 7, Sunday, or 0 for any day; hour 0 to 23 UTC; "Maintenance
//     windows last for one hour"). A scheduled event with no startTime
//     given starts in the first window at least a week away: notifications
//     come "at least one week before maintenance is scheduled".
//   - scheduleDeadlineTime: "you can reschedule maintenance by up to 28
//     days" on the canary and stable (week 1, week 2) update tracks, "up to
//     7 days" on week5.
//   - "You must reschedule maintenance at least 24 hours before the
//     originally scheduled maintenance event happens": canReschedule is
//     false, and rescheduleMaintenance refused, within 24 hours of the
//     start.
//   - IMMEDIATE: "maintenance generally starts within five minutes"; here
//     it starts at once. NEXT_AVAILABLE_WINDOW: "the next available
//     maintenance window following the current scheduled time".
//     SPECIFIC_TIME: scheduleTime, no earlier than the current start (gcloud
//     checks "Must be after original scheduled time") nor the deadline.
//   - "The deny maintenance period takes precedence ... and the
//     maintenance event is skipped"; a replica's is denied by its
//     primary's too. "If an instance is stopped during its scheduled
//     maintenance update, then Cloud SQL skips the maintenance update."
//   - "If a maintenance operation is in progress, rescheduling is delayed
//     until the operation is complete": an event that comes due while the
//     instance is busy runs once it is RUNNABLE again.
//   - A deny maintenance period "can span from one to 90 days", "Maximum of
//     one per instance at a time".
//
// UNVERIFIED (re-read 2026-11-18: docs.cloud.google.com/sql/docs/mysql/set-maintenance-window, none given): the text and
// status of the errors for an instance with no scheduled maintenance, a
// reschedule within 24 hours, a time past the deadline, and the deny
// period limits; that NEXT_AVAILABLE_WINDOW with no maintenanceWindow set
// moves the event a week; that canDefer (deprecated) equals canReschedule.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Maintenance is a scheduled maintenance event.
type Maintenance struct {
	Start    time.Time `json:"startTime"`
	Deadline time.Time `json:"scheduleDeadlineTime"`
}

// MaintenanceControlPrefix is CloudBurrow's own control endpoint, not a
// Cloud SQL API: POST
// /cloudburrow/v1/projects/{project}/instances/{instance}:scheduleMaintenance
// with {"startTime", "scheduleDeadlineTime"} (both optional, RFC 3339)
// schedules an event on a created instance and answers its
// scheduledMaintenance; :cancelMaintenance removes it.
const MaintenanceControlPrefix = "/cloudburrow/v1/projects/"

// rescheduleNotice is how long before its start an event can last be
// rescheduled.
const rescheduleNotice = 24 * time.Hour

// window is spec's maintenanceWindow: its day (0 for any), hour and
// update track, and whether one is set.
func window(spec Spec) (day, hour int, track string, set bool) {
	mw, ok := spec.Settings["maintenanceWindow"].(map[string]any)
	if !ok {
		return 0, 0, "", false
	}
	track, _ = mw["updateTrack"].(string)
	_, hasDay := mw["day"]
	_, hasHour := mw["hour"]
	return intSetting(mw["day"]), intSetting(mw["hour"]), track, hasDay || hasHour
}

// nextWindow is the start of spec's first maintenance window at or after
// t, t itself (to the hour) when no window is set.
func nextWindow(spec Spec, t time.Time) time.Time {
	t = t.UTC()
	day, hour, _, set := window(spec)
	if !set {
		if tr := t.Truncate(time.Hour); tr.Before(t) {
			return tr.Add(time.Hour)
		}
		return t
	}
	c := time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, time.UTC)
	for i := 0; i < 15; i++ {
		// Cloud SQL's day is 1 (Monday) to 7 (Sunday); Go's Sunday is 0.
		if !c.Before(t) && (day == 0 || int(c.Weekday()) == day%7) {
			return c
		}
		c = c.AddDate(0, 0, 1)
	}
	return c
}

// deadlineFor is the schedule deadline of an event first set to start.
func deadlineFor(spec Spec, start time.Time) time.Time {
	if _, _, track, _ := window(spec); track == "week5" {
		return start.Add(7 * 24 * time.Hour)
	}
	return start.Add(28 * 24 * time.Hour)
}

// denyPeriods are each deny maintenance period's span in the year of t (or, for
// one that recurs and crosses the new year, the span that ends in it).
func denyPeriods(spec Spec, t time.Time) [][2]time.Time {
	ps, _ := spec.Settings["denyMaintenancePeriods"].([]any)
	var out [][2]time.Time
	for _, p := range ps {
		m, _ := p.(map[string]any)
		sd, _ := m["startDate"].(string)
		ed, _ := m["endDate"].(string)
		tm, _ := m["time"].(string)
		if tm == "" {
			tm = "00:00:00"
		}
		for _, y := range []int{t.Year() - 1, t.Year()} {
			a, okA := denyDate(sd, tm, y)
			b, okB := denyDate(ed, tm, y)
			if !okA || !okB {
				continue
			}
			if b.Before(a) {
				// A recurring "12-20" to "01-05" ends in the next year.
				b = b.AddDate(1, 0, 0)
			}
			out = append(out, [2]time.Time{a, b})
		}
	}
	return out
}

// denyDate parses a deny period's date, yyyy-mm-dd or mm-dd (every year,
// here year), at tm (HH:mm:SS, UTC).
func denyDate(d, tm string, year int) (time.Time, bool) {
	if len(d) == 5 {
		d = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-" + d
	}
	t, err := time.Parse("2006-01-02 15:04:05", d+" "+tm)
	return t, err == nil
}

// denied reports whether a deny maintenance period of spec holds t.
func denied(spec Spec, t time.Time) bool {
	for _, p := range denyPeriods(spec, t) {
		if !t.Before(p[0]) && !t.After(p[1]) {
			return true
		}
	}
	return false
}

// checkMaintenanceSettings checks settings.maintenanceWindow and
// settings.denyMaintenancePeriods against Cloud SQL's documented limits.
func checkMaintenanceSettings(st map[string]any) error {
	if mw, ok := st["maintenanceWindow"].(map[string]any); ok {
		if d := intSetting(mw["day"]); d < 0 || d > 7 {
			return Errorf(http.StatusBadRequest, "Invalid value for settings.maintenanceWindow.day: %v. It is 1 (Monday) to 7 (Sunday), or 0 for any day.", mw["day"])
		}
		if h := intSetting(mw["hour"]); h < 0 || h > 23 {
			return Errorf(http.StatusBadRequest, "Invalid value for settings.maintenanceWindow.hour: %v. It is 0 to 23 (UTC).", mw["hour"])
		}
		switch tr, _ := mw["updateTrack"].(string); tr {
		case "", "SQL_UPDATE_TRACK_UNSPECIFIED", "canary", "stable", "week5":
		default:
			return Errorf(http.StatusBadRequest, "Invalid value for settings.maintenanceWindow.updateTrack: %s.", tr)
		}
	}
	ps, ok := st["denyMaintenancePeriods"].([]any)
	if !ok {
		return nil
	}
	if len(ps) > 1 {
		return Errorf(http.StatusBadRequest, "Invalid request: an instance has at most one deny maintenance period, not %d.", len(ps))
	}
	for _, p := range ps {
		m, _ := p.(map[string]any)
		sd, _ := m["startDate"].(string)
		ed, _ := m["endDate"].(string)
		tm, _ := m["time"].(string)
		if tm == "" {
			tm = "00:00:00"
		}
		if (len(sd) == 5) != (len(ed) == 5) {
			return Errorf(http.StatusBadRequest, "Invalid request: a deny maintenance period's startDate and endDate are both yyyy-mm-dd or both mm-dd.")
		}
		a, okA := denyDate(sd, tm, 2001)
		b, okB := denyDate(ed, tm, 2001)
		if !okA || !okB {
			return Errorf(http.StatusBadRequest, "Invalid request: a deny maintenance period's startDate and endDate are yyyy-mm-dd or mm-dd, and its time HH:mm:SS: %q, %q, %q.", sd, ed, tm)
		}
		if len(sd) == 5 && b.Before(a) {
			b = b.AddDate(1, 0, 0)
		}
		if days := b.Sub(a).Hours() / 24; days < 1 || days > 90 {
			return Errorf(http.StatusBadRequest, "Invalid request: a deny maintenance period spans from one to 90 days, not %s to %s.", sd, ed)
		}
	}
	return nil
}

// canReschedule is whether the event can still be moved at now.
func (m *Maintenance) canReschedule(now time.Time) bool {
	return now.Before(m.Start.Add(-rescheduleNotice))
}

// scheduledJSON is the event as Cloud SQL's SqlScheduledMaintenance.
func (m *Maintenance) scheduledJSON(now time.Time) map[string]any {
	can := m.canReschedule(now)
	return map[string]any{
		"startTime":            m.Start.UTC().Format(time.RFC3339Nano),
		"canDefer":             can,
		"canReschedule":        can,
		"scheduleDeadlineTime": m.Deadline.UTC().Format(time.RFC3339Nano),
	}
}

// maintenanceJSON sets in's scheduledMaintenance, when an event is.
func maintenanceJSON(in map[string]any, spec Spec, now time.Time) {
	if spec.Maintenance != nil {
		in["scheduledMaintenance"] = spec.Maintenance.scheduledJSON(now)
	}
}

type rescheduleBody struct {
	Reschedule *struct {
		RescheduleType string `json:"rescheduleType"`
		ScheduleTime   string `json:"scheduleTime"`
		// The REST page's example uses the proto names.
		RescheduleTypeProto string `json:"reschedule_type"`
		ScheduleTimeProto   string `json:"schedule_time"`
	} `json:"reschedule"`
}

// rescheduleMaintenance is instances.rescheduleMaintenance, for an
// instance that exists (the router has found it).
func (s *Server) rescheduleMaintenance(r *http.Request, base, project, name string) (any, error) {
	var b rescheduleBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Reschedule == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule is required.")
	}
	typ, at := b.Reschedule.RescheduleType, b.Reschedule.ScheduleTime
	if typ == "" {
		typ = b.Reschedule.RescheduleTypeProto
	}
	if at == "" {
		at = b.Reschedule.ScheduleTimeProto
	}
	var when time.Time
	switch typ {
	case "IMMEDIATE", "NEXT_AVAILABLE_WINDOW":
	case "SPECIFIC_TIME":
		if at == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.scheduleTime is required with rescheduleType SPECIFIC_TIME.")
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid value at 'reschedule.schedule_time' (type.googleapis.com/google.protobuf.Timestamp), Field 'scheduleTime', Invalid time format: %q", at)
		}
		when = t.UTC()
	case "", "RESCHEDULE_TYPE_UNSPECIFIED":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.rescheduleType is required: IMMEDIATE, NEXT_AVAILABLE_WINDOW or SPECIFIC_TIME.")
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid value at 'reschedule.reschedule_type' (type.googleapis.com/google.cloud.sql.v1.SqlInstancesRescheduleMaintenanceRequestBody.RescheduleType), %q", typ)
	}
	// The wording is gcloud's own client-side check
	// (googlecloudsdk/command_lib/sql/reschedule_maintenance_util.py).
	noEvent := Errorf(http.StatusBadRequest, "Invalid request: This instance does not have any scheduled maintenance at this time.")
	c := s.createdNamed(project, name)
	if c == nil {
		// The instances `up` runs are never given an event.
		return nil, noEvent
	}
	now := time.Now().UTC()
	s.emu.Lock()
	spec := c.spec
	m := spec.Maintenance
	if m == nil {
		s.emu.Unlock()
		return nil, noEvent
	}
	if !m.canReschedule(now) {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Cannot reschedule this instance's maintenance: it starts at %s, "+
			"and maintenance is rescheduled at least 24 hours before it starts.", m.Start.Format(time.RFC3339))
	}
	next := *m
	switch typ {
	case "IMMEDIATE":
		next.Start = now
	case "NEXT_AVAILABLE_WINDOW":
		if _, _, _, set := window(spec); set {
			next.Start = nextWindow(spec, m.Start.Add(time.Hour))
		} else {
			next.Start = m.Start.Add(7 * 24 * time.Hour)
		}
		if next.Start.After(m.Deadline) {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: the next available maintenance window, %s, is after the schedule deadline time, %s.",
				next.Start.Format(time.RFC3339), m.Deadline.Format(time.RFC3339))
		}
	case "SPECIFIC_TIME":
		if when.Before(m.Start) {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.scheduleTime %s: Must be after original scheduled time, %s.",
				when.Format(time.RFC3339), m.Start.Format(time.RFC3339))
		}
		if when.After(m.Deadline) {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: reschedule.scheduleTime %s is after the schedule deadline time, %s.",
				when.Format(time.RFC3339), m.Deadline.Format(time.RFC3339))
		}
		next.Start = when
	}
	spec.Maintenance = &next
	c.spec = spec
	err := s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	op := s.done(base, project, namedEngine(name), "RESCHEDULE_MAINTENANCE", name)
	if typ == "IMMEDIATE" {
		go s.runMaintenance(name, now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyOp(op), nil
}

// maintenanceLoop starts each event when its time comes, checking every
// ten seconds.
func (s *Server) maintenanceLoop(stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.maintenanceTick(now.UTC())
		}
	}
}

func (s *Server) maintenanceTick(now time.Time) {
	var due []string
	s.emu.RLock()
	for name, c := range s.writes.byName {
		if m := c.spec.Maintenance; m != nil && !m.Start.After(now) && c.state == "RUNNABLE" {
			due = append(due, name)
		}
	}
	s.emu.RUnlock()
	for _, name := range due {
		go s.runMaintenance(name, now)
	}
}

// runMaintenance performs name's event, due at now: skipped in a deny
// period or on a stopped instance, otherwise a restart of its server in
// a MAINTENANCE operation, the instance MAINTENANCE meanwhile.
func (s *Server) runMaintenance(name string, now time.Time) {
	s.emu.Lock()
	c := s.writes.byName[name]
	if c == nil || c.spec.Maintenance == nil || c.spec.Maintenance.Start.After(now) || c.state != "RUNNABLE" {
		s.emu.Unlock()
		return
	}
	spec := c.spec
	spec.Maintenance = nil
	c.spec = spec
	skip := spec.ActivationPolicy == "NEVER" || denied(spec, now)
	if p := s.writes.byName[spec.MasterInstanceName]; spec.MasterInstanceName != "" && p != nil && denied(p.spec, now) {
		skip = true
	}
	if !skip {
		c.state = "MAINTENANCE"
	}
	_ = s.saveLocked()
	s.emu.Unlock()
	if skip {
		return
	}
	op := s.pending("http://"+s.Addr()+"/v1/", spec.Project, name, "MAINTENANCE")
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	err := s.writes.prov.Restart(ctx, spec)
	s.emu.Lock()
	if s.writes.byName[name] == c && c.state == "MAINTENANCE" {
		c.state = "RUNNABLE"
		if err != nil {
			c.state, c.failure = "FAILED", err.Error()
		}
	}
	s.emu.Unlock()
	s.finish(op["name"].(string), err)
}

// serveMaintenanceControl serves MaintenanceControlPrefix.
func (s *Server) serveMaintenanceControl(r *http.Request) (any, error) {
	rest := strings.TrimPrefix(r.URL.Path, MaintenanceControlPrefix)
	parts := strings.Split(rest, "/")
	if r.Method != http.MethodPost || len(parts) != 3 || parts[1] != "instances" {
		return nil, Errorf(http.StatusNotFound, "%s %s is not a CloudBurrow maintenance control path: POST %s{project}/instances/{instance}:scheduleMaintenance or :cancelMaintenance",
			r.Method, r.URL.Path, MaintenanceControlPrefix)
	}
	project := parts[0]
	name, verb, _ := strings.Cut(parts[2], ":")
	if verb != "scheduleMaintenance" && verb != "cancelMaintenance" {
		return nil, Errorf(http.StatusNotFound, "%s is not a CloudBurrow maintenance control verb: scheduleMaintenance or cancelMaintenance", verb)
	}
	c, err := s.createdFor(project, name, "maintenance events")
	if err != nil {
		return nil, err
	}
	var body struct {
		StartTime            string `json:"startTime"`
		ScheduleDeadlineTime string `json:"scheduleDeadlineTime"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: %v.", err)
		}
	}
	now := time.Now().UTC()
	s.emu.Lock()
	spec := c.spec
	if verb == "cancelMaintenance" {
		spec.Maintenance = nil
		c.spec = spec
		err := s.saveLocked()
		s.emu.Unlock()
		return map[string]any{}, err
	}
	m := &Maintenance{Start: nextWindow(spec, now.Add(7*24*time.Hour))}
	if body.StartTime != "" {
		t, err := time.Parse(time.RFC3339Nano, body.StartTime)
		if err != nil {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: startTime %q is not an RFC 3339 time.", body.StartTime)
		}
		m.Start = t.UTC()
	}
	m.Deadline = deadlineFor(spec, m.Start)
	if body.ScheduleDeadlineTime != "" {
		t, err := time.Parse(time.RFC3339Nano, body.ScheduleDeadlineTime)
		if err != nil || t.Before(m.Start) {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: scheduleDeadlineTime %q is not an RFC 3339 time at or after startTime.", body.ScheduleDeadlineTime)
		}
		m.Deadline = t.UTC()
	}
	spec.Maintenance = m
	c.spec = spec
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	if !m.Start.After(now) {
		go s.maintenanceTick(now)
	}
	return map[string]any{"scheduledMaintenance": m.scheduledJSON(now)}, nil
}
