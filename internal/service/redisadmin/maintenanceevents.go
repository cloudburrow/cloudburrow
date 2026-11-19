package redisadmin

// Maintenance events of Memorystore for Redis instances (#1524).
// Memorystore schedules a maintenance update on an instance, reports it
// as maintenance_schedule, lets RescheduleMaintenance move it, and at its
// start applies it. CloudBurrow's servers are their versions' pinned
// images, so Google's updates never come; an event is scheduled locally
// instead, through CloudBurrow's control endpoint MaintenanceControlPath
// (not a Memorystore API) or the console's Schedule maintenance, and from
// then on behaves as Google documents it
// (docs.cloud.google.com/memorystore/docs/redis/about-maintenance and
// /find-and-set-maintenance-windows, and gcloud redis instances
// reschedule-maintenance, read 2026-11-18):
//
//   - "The duration of the maintenance window is 1 hour", on the policy's
//     day at its UTC start hour: an event with no start given starts in
//     the first window at least a week away (notifications come "at least
//     one week before maintenance is scheduled"), or a week on, to the
//     hour, with no policy. end_time is an hour after start_time.
//   - "You can reschedule maintenance up to two times", "as long as it's
//     not more than one week after the originally scheduled time":
//     schedule_deadline_time is the original start and a week.
//   - "You cannot reschedule maintenance if less than an hour remains
//     before currently scheduled maintenance": can_reschedule is false,
//     and RescheduleMaintenance FAILED_PRECONDITION, then and after two
//     reschedules.
//   - IMMEDIATE "Starts the maintenance update immediately".
//     NEXT_AVAILABLE_WINDOW "defers maintenance by one week": the next
//     window after the current start, a week on with no policy.
//     SPECIFIC_TIME: "any specific time within one week after the
//     originally scheduled maintenance time".
//   - "During maintenance, Basic Tier instances are unavailable": the
//     server is stopped and started again, MAINTENANCE meanwhile.
//     "During maintenance, Standard Tier instances undergo a failover":
//     a replica is promoted, as an automatic failover is (FORCE_DATA_LOSS).
//
// UNVERIFIED (re-read 2026-11-18, the pages above, which give none): the
// code and wording of the errors for an instance with no scheduled
// maintenance, a third reschedule, a reschedule within the hour and a
// time out of bounds; the RescheduleMaintenance operation's verb; that
// a maintenance update leaves no operation of its own in ListOperations.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// scheduledMaintenance is an instance's maintenance event, as it is kept.
type scheduledMaintenance struct {
	// Original is when Memorystore first scheduled it; the deadline is a
	// week after.
	Original time.Time `json:"original"`
	// Start is when it starts now.
	Start time.Time `json:"start"`
	// Reschedules counts the reschedules: at most two.
	Reschedules int `json:"reschedules,omitempty"`
}

const (
	maintenanceWindow    = time.Hour
	maintenanceDeadline  = 7 * 24 * time.Hour
	maintenanceNotice    = time.Hour
	maxReschedules       = 2
	maintenanceLeadTime  = 7 * 24 * time.Hour
	maintenanceRestarted = "maintenance"
)

func (m *scheduledMaintenance) deadline() time.Time { return m.Original.Add(maintenanceDeadline) }

func (m *scheduledMaintenance) canReschedule(now time.Time) bool {
	return m.Reschedules < maxReschedules && now.Before(m.Start.Add(-maintenanceNotice))
}

func (m *scheduledMaintenance) render(now time.Time) *redispb.MaintenanceSchedule {
	if m == nil {
		return nil
	}
	return &redispb.MaintenanceSchedule{StartTime: timestamppb.New(m.Start), EndTime: timestamppb.New(m.Start.Add(maintenanceWindow)),
		CanReschedule: m.canReschedule(now), ScheduleDeadlineTime: timestamppb.New(m.deadline())}
}

// policyWindow is the next start of policy kept's weekly window at or
// after t; ok is false when kept has no window.
func policyWindow(kept string, t time.Time) (time.Time, bool) {
	ws := renderPolicy(kept).GetWeeklyMaintenanceWindow()
	if len(ws) == 0 {
		return time.Time{}, false
	}
	w := ws[0]
	t = t.UTC()
	st := w.GetStartTime()
	c := time.Date(t.Year(), t.Month(), t.Day(), int(st.GetHours()), int(st.GetMinutes()), int(st.GetSeconds()), 0, time.UTC)
	// DayOfWeek is 1 (Monday) to 7 (Sunday); Go's Sunday is 0.
	want := time.Weekday(int(w.GetDay()) % 7)
	for i := 0; i < 15; i++ {
		if !c.Before(t) && c.Weekday() == want {
			return c, true
		}
		c = c.AddDate(0, 0, 1)
	}
	return c, true
}

// firstStart is when an event scheduled at now with no start given
// starts: the first window a week away, or a week on to the hour.
func firstStart(kept string, now time.Time) time.Time {
	t := now.UTC().Add(maintenanceLeadTime)
	if w, ok := policyWindow(kept, t); ok {
		return w
	}
	if tr := t.Truncate(time.Hour); tr.Before(t) {
		return tr.Add(time.Hour)
	}
	return t
}

// createdRecord is the created instance name, for a maintenance call.
func (s *Server) createdRecord(name string) error {
	isDefault, err := s.find(name)
	if err != nil {
		return err
	}
	if isDefault {
		return apierror.FailedPrecondition("instance %s has no scheduled maintenance: it is the server `cloudburrow up` runs, which is never maintained", name)
	}
	return nil
}

// ScheduleMaintenance schedules a maintenance event on a created instance,
// as Memorystore would: CloudBurrow's own call, not a Memorystore API. A
// zero start is the first maintenance window a week away. It replaces an
// event already scheduled.
func (s *Server) ScheduleMaintenance(ctx context.Context, name string, start time.Time) (*redispb.Instance, error) {
	if err := s.createdRecord(name); err != nil {
		return nil, err
	}
	c := s.created
	c.mu.Lock()
	r := c.byID[name]
	now := c.now().UTC()
	if start.IsZero() {
		start = firstStart(r.Maintenance, now)
	}
	next := *r
	next.Scheduled = &scheduledMaintenance{Original: start.UTC(), Start: start.UTC()}
	if err := c.save(&next); err != nil {
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep instance %s", name)
	}
	r.Scheduled = next.Scheduled
	inst := c.render(r)
	c.mu.Unlock()
	if !start.After(now) {
		go c.maintenanceDue()
	}
	return inst, nil
}

// CancelMaintenance removes a created instance's scheduled event, as if
// Memorystore had not scheduled it: CloudBurrow's own call.
func (s *Server) CancelMaintenance(ctx context.Context, name string) (*redispb.Instance, error) {
	if err := s.createdRecord(name); err != nil {
		return nil, err
	}
	c := s.created
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.byID[name]
	next := *r
	next.Scheduled = nil
	if err := c.save(&next); err != nil {
		return nil, apierror.Internal(err, "keep instance %s", name)
	}
	r.Scheduled = nil
	return c.render(r), nil
}

// RescheduleMaintenance moves an instance's scheduled maintenance: now
// (IMMEDIATE), to the next window (NEXT_AVAILABLE_WINDOW), or to
// schedule_time (SPECIFIC_TIME), within Memorystore's limits.
func (s *Server) RescheduleMaintenance(ctx context.Context, req *redispb.RescheduleMaintenanceRequest) (*longrunningpb.Operation, error) {
	name := req.GetName()
	if _, err := s.find(name); err != nil {
		return nil, err
	}
	typ := req.GetRescheduleType()
	switch typ {
	case redispb.RescheduleMaintenanceRequest_IMMEDIATE, redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW:
		if req.GetScheduleTime() != nil {
			return nil, apierror.InvalidArgument("schedule_time is given only with reschedule_type SPECIFIC_TIME")
		}
	case redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME:
		if req.GetScheduleTime() == nil {
			return nil, apierror.InvalidArgument("schedule_time is required with reschedule_type SPECIFIC_TIME")
		}
		if err := req.GetScheduleTime().CheckValid(); err != nil {
			return nil, apierror.InvalidArgument("schedule_time: %v", err)
		}
	default:
		return nil, apierror.InvalidArgument("reschedule_type is required: IMMEDIATE, NEXT_AVAILABLE_WINDOW or SPECIFIC_TIME")
	}
	if err := s.createdRecord(name); err != nil {
		return nil, err
	}
	c := s.created
	c.mu.Lock()
	r := c.byID[name]
	now := c.now().UTC()
	m := r.Scheduled
	if m == nil {
		c.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s has no scheduled maintenance to reschedule", name)
	}
	if !now.Before(m.Start.Add(-maintenanceNotice)) {
		c.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s's maintenance starts at %s: maintenance is not rescheduled when less than an hour remains before it",
			name, m.Start.Format(time.RFC3339))
	}
	next := *m
	switch typ {
	case redispb.RescheduleMaintenanceRequest_IMMEDIATE:
		next.Start = now
	default:
		if m.Reschedules >= maxReschedules {
			c.mu.Unlock()
			return nil, apierror.FailedPrecondition("instance %s's maintenance was already rescheduled %d times: it is rescheduled at most twice", name, m.Reschedules)
		}
		if typ == redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW {
			w, ok := policyWindow(r.Maintenance, m.Start.Add(time.Second))
			if !ok {
				w = m.Start.Add(7 * 24 * time.Hour)
			}
			if w.After(m.deadline()) {
				c.mu.Unlock()
				return nil, apierror.FailedPrecondition("the next available maintenance window, %s, is more than one week after the originally scheduled time, %s",
					w.Format(time.RFC3339), m.Original.Format(time.RFC3339))
			}
			next.Start = w
		} else {
			t := req.GetScheduleTime().AsTime().UTC()
			if t.Before(m.Original) || t.After(m.deadline()) {
				c.mu.Unlock()
				return nil, apierror.InvalidArgument("schedule_time %s is not within one week after the originally scheduled maintenance time, %s to %s",
					t.Format(time.RFC3339), m.Original.Format(time.RFC3339), m.deadline().Format(time.RFC3339))
			}
			if t.Before(now) {
				c.mu.Unlock()
				return nil, apierror.InvalidArgument("schedule_time %s is in the past", t.Format(time.RFC3339))
			}
			next.Start = t
		}
		next.Reschedules++
	}
	kept := *r
	kept.Scheduled = &next
	if err := c.save(&kept); err != nil {
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep instance %s", name)
	}
	r.Scheduled = &next
	op := c.newOperation(name, "rescheduleMaintenance")
	inst := c.render(r)
	c.mu.Unlock()
	c.finish(op, inst)
	if typ == redispb.RescheduleMaintenanceRequest_IMMEDIATE {
		go c.maintenanceDue()
	}
	return s.operation(op)
}

// maintenanceDue starts every event whose time has come on a READY
// instance with no operation in progress; the others wait until it is.
func (c *instances) maintenanceDue() {
	c.mu.Lock()
	now := c.now().UTC()
	var due []string
	for name, r := range c.byID {
		if _, pending := c.busy[name]; pending || r.Scheduled == nil || r.Scheduled.Start.After(now) || r.state != redispb.Instance_READY {
			continue
		}
		next := *r
		next.Scheduled = nil
		if err := c.save(&next); err != nil {
			log.Printf("memorystore: %s: keep the maintenance's start: %v", name, err)
			continue
		}
		r.Scheduled = nil
		c.busy[name] = maintenanceRestarted
		r.state, r.message = redispb.Instance_MAINTENANCE, "a maintenance update is being applied"
		due = append(due, name)
	}
	c.mu.Unlock()
	for _, name := range due {
		go c.maintain(name)
	}
}

// Restarter stops a created instance's server and starts it again, the
// server unavailable meanwhile, and returns its new address.
type Restarter interface {
	Restart(ctx context.Context, spec Spec) (addr string, err error)
}

// maintain applies name's maintenance update, claimed by maintenanceDue:
// a Standard tier instance fails over, a Basic tier one's server is
// stopped and started again.
func (c *instances) maintain(name string) {
	c.mu.Lock()
	r, ok := c.byID[name]
	if !ok {
		delete(c.busy, name)
		c.mu.Unlock()
		return
	}
	from := *r
	from.Nodes = append([]int(nil), r.Nodes...)
	addrs := map[int]string{}
	for k, v := range r.nodeAddrs {
		addrs[k] = v
	}
	spec := r.spec()
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(c.bg, haWait)
	defer cancel()
	var addr string
	var err error
	if from.ha() {
		var rs ReplicaSet
		if rs, err = c.replicaSet(); err == nil {
			err = c.failover(ctx, rs, &from, addrs, redispb.FailoverInstanceRequest_FORCE_DATA_LOSS)
		}
	} else if rp, ok := c.prov.(Restarter); ok {
		addr, err = rp.Restart(ctx, spec)
	} else {
		addr, err = c.prov.Provision(ctx, spec)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, name)
	r, ok = c.byID[name]
	if !ok {
		return
	}
	r.state, r.message = redispb.Instance_READY, ""
	if err != nil {
		log.Printf("memorystore: %s: maintenance: %v", name, err)
		r.state, r.message = redispb.Instance_STATE_UNSPECIFIED, "the maintenance update failed: "+err.Error()
		return
	}
	if !from.ha() && addr != "" {
		r.addr = addr
	}
}

// MaintenanceControlPath is CloudBurrow's own control endpoint, not a
// Memorystore API: POST
// /cloudburrow/v1/projects/{project}/locations/{region}/instances/{instance}:scheduleMaintenance
// with {"startTime"} (optional, RFC 3339) schedules an event on a created
// instance and answers the instance; :cancelMaintenance removes it.
const MaintenanceControlPath = "/cloudburrow/v1/"

// ServeMaintenanceControl serves MaintenanceControlPath.
func (s *Server) ServeMaintenanceControl(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	name, verb, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, MaintenanceControlPath), ":")
	if r.Method != http.MethodPost || !instanceRE.MatchString(name) || (verb != "scheduleMaintenance" && verb != "cancelMaintenance") {
		fail(http.StatusNotFound, fmt.Sprintf("%s %s is not a CloudBurrow maintenance control path: POST %sprojects/P/locations/R/instances/I:scheduleMaintenance or :cancelMaintenance",
			r.Method, r.URL.Path, MaintenanceControlPath))
		return
	}
	var body struct {
		StartTime string `json:"startTime"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			fail(http.StatusBadRequest, "the body is not JSON: "+err.Error())
			return
		}
	}
	var inst *redispb.Instance
	var err error
	if verb == "cancelMaintenance" {
		inst, err = s.CancelMaintenance(r.Context(), name)
	} else {
		var start time.Time
		if body.StartTime != "" {
			if start, err = time.Parse(time.RFC3339Nano, body.StartTime); err != nil {
				fail(http.StatusBadRequest, fmt.Sprintf("startTime %q is not an RFC 3339 time", body.StartTime))
				return
			}
		}
		inst, err = s.ScheduleMaintenance(r.Context(), name, start)
	}
	if err != nil {
		code := http.StatusBadRequest
		if e, ok := err.(*apierror.Error); ok {
			code = e.HTTPStatus()
		}
		fail(code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{"name": inst.GetName()}
	if m := inst.GetMaintenanceSchedule(); m != nil {
		out["maintenanceSchedule"] = map[string]any{"startTime": m.GetStartTime().AsTime().Format(time.RFC3339Nano),
			"endTime": m.GetEndTime().AsTime().Format(time.RFC3339Nano), "canReschedule": m.GetCanReschedule(),
			"scheduleDeadlineTime": m.GetScheduleDeadlineTime().AsTime().Format(time.RFC3339Nano)}
	}
	_ = json.NewEncoder(w).Encode(out)
}
