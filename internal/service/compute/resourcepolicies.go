package compute

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Resource policies (#2139): snapshot schedules
// (https://docs.cloud.google.com/compute/docs/disks/scheduled-snapshots,
// read 2026-11-22) and instance schedules
// (https://docs.cloud.google.com/compute/docs/instances/schedule-instance-start-stop).
// resourcePolicies insert, get, list, aggregatedList and delete (refused
// while a disk or an instance uses the policy); disks and instances take
// them at insert and with addResourcePolicies / removeResourcePolicies. A
// runner (runSchedulesLocked) takes each scheduled disk's snapshots when
// they fall due, deletes those past the retention, and starts and stops the
// instances of an instance schedule at its cron times.

func policyRel(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/resourcePolicies/" + name
}

var hhmmRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):00$`)

func (s *Server) resourcePolicies(c call, region string) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/regions/" + region + "/resourcePolicies"
	scope := "regions/" + region
	switch {
	case n == 3 && c.method == http.MethodGet:
		return s.listOut("compute#resourcePolicyList", coll, s.res, c.query)
	case n == 3 && c.method == http.MethodPost:
		return s.insertPolicy(c, region)
	}
	if n != 4 || (c.method != http.MethodGet && c.method != http.MethodDelete) {
		return nil, unimplemented("%s on resourcePolicies/%s is not served", c.method, strings.Join(p[3:], "/"))
	}
	rel := coll + "/" + p[3]
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	switch c.method {
	case http.MethodGet:
		return o, nil
	case http.MethodDelete:
		if users := s.policyUsersLocked(rel); len(users) > 0 {
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The resource_policy resource '%s' is already being used by '%s'", rel, users[0])
		}
		delete(s.res, rel)
		return s.done(c.project, scope, "delete", rel, o["id"])
	}
	return nil, unimplemented("%s on resourcePolicies/%s is not served", c.method, p[3])
}

// policyUsersLocked are the disks and instances a policy is attached to.
func (s *Server) policyUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		if (strings.Contains(k, "/disks/") || strings.Contains(k, "/instances/")) && hasStr(strs(o, "resourcePolicies"), rel) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) insertPolicy(c call, region string) (obj, error) {
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := policyRel(c.project, region, name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	o := obj{"kind": "compute#resourcePolicy", "id": newID(), "name": name, "region": "projects/" + c.project + "/regions/" + region,
		"selfLink": rel, "creationTimestamp": s.stamp(), "status": "READY"}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	ss, _ := b["snapshotSchedulePolicy"].(map[string]any)
	is, _ := b["instanceSchedulePolicy"].(map[string]any)
	for _, k := range []string{"groupPlacementPolicy", "diskConsistencyGroupPolicy", "workloadPolicy"} {
		if b[k] != nil {
			return nil, unimplemented("%s resource policies are not served: placement and consistency groups need more than one host", k)
		}
	}
	switch {
	case ss != nil && is == nil:
		pol, err := checkSnapshotSchedule(ss)
		if err != nil {
			return nil, err
		}
		o["snapshotSchedulePolicy"] = pol
	case is != nil && ss == nil:
		pol, err := checkInstanceSchedule(is)
		if err != nil {
			return nil, err
		}
		o["instanceSchedulePolicy"] = pol
	default:
		return nil, invalid("Invalid value for field 'resource': ''. Exactly one of snapshotSchedulePolicy or instanceSchedulePolicy must be specified.")
	}
	s.res[rel] = norm(o)
	return s.done(c.project, "regions/"+region, "insert", rel, o["id"])
}

// checkSnapshotSchedule validates a snapshotSchedulePolicy as Google does
// (scheduled-snapshots: an hourly, daily or weekly schedule whose start
// time is on the hour, UTC; a retention of at least a day).
func checkSnapshotSchedule(ss map[string]any) (obj, error) {
	sch, _ := ss["schedule"].(map[string]any)
	n := 0
	out := obj{}
	if h, ok := sch["hourlySchedule"].(map[string]any); ok {
		n++
		hrs, _ := h["hoursInCycle"].(float64)
		if hrs < 1 || hrs > 23 {
			return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.hourlySchedule.hoursInCycle': '%v'. Must be between 1 and 23.", h["hoursInCycle"])
		}
		if !hhmmRE.MatchString(str(h, "startTime")) {
			return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.hourlySchedule.startTime': '%s'. Must be on the hour, as HH:00.", str(h, "startTime"))
		}
		out["hourlySchedule"] = obj{"hoursInCycle": hrs, "startTime": str(h, "startTime"), "duration": "PT14400S"}
	}
	if d, ok := sch["dailySchedule"].(map[string]any); ok {
		n++
		days, _ := d["daysInCycle"].(float64)
		if days != 1 {
			return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.dailySchedule.daysInCycle': '%v'. Must be 1.", d["daysInCycle"])
		}
		if !hhmmRE.MatchString(str(d, "startTime")) {
			return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.dailySchedule.startTime': '%s'. Must be on the hour, as HH:00.", str(d, "startTime"))
		}
		out["dailySchedule"] = obj{"daysInCycle": days, "startTime": str(d, "startTime"), "duration": "PT14400S"}
	}
	if w, ok := sch["weeklySchedule"].(map[string]any); ok {
		n++
		days, _ := w["dayOfWeeks"].([]any)
		if len(days) == 0 {
			return nil, required("resource.snapshotSchedulePolicy.schedule.weeklySchedule.dayOfWeeks")
		}
		var outDays []any
		for i, e := range days {
			dm, _ := e.(map[string]any)
			if _, ok := weekdays[str(dm, "day")]; !ok {
				return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.weeklySchedule.dayOfWeeks[%d].day': '%s'.", i, str(dm, "day"))
			}
			if !hhmmRE.MatchString(str(dm, "startTime")) {
				return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule.weeklySchedule.dayOfWeeks[%d].startTime': '%s'. Must be on the hour, as HH:00.", i, str(dm, "startTime"))
			}
			outDays = append(outDays, obj{"day": str(dm, "day"), "startTime": str(dm, "startTime"), "duration": "PT14400S"})
		}
		out["weeklySchedule"] = obj{"dayOfWeeks": outDays}
	}
	if n != 1 {
		return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.schedule': ''. Exactly one of hourlySchedule, dailySchedule or weeklySchedule must be specified.")
	}
	pol := obj{"schedule": out}
	rp, _ := ss["retentionPolicy"].(map[string]any)
	days := 14.0
	if v, ok := rp["maxRetentionDays"].(float64); ok {
		days = v
	}
	if days < 1 {
		return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.retentionPolicy.maxRetentionDays': '%v'. Must be at least 1.", days)
	}
	onDel := vpcOrStr(str(rp, "onSourceDiskDelete"), "KEEP_AUTO_SNAPSHOTS")
	if onDel != "KEEP_AUTO_SNAPSHOTS" && onDel != "APPLY_RETENTION_POLICY" {
		return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.retentionPolicy.onSourceDiskDelete': '%s'.", onDel)
	}
	pol["retentionPolicy"] = obj{"maxRetentionDays": days, "onSourceDiskDelete": onDel}
	if sp, ok := ss["snapshotProperties"].(map[string]any); ok {
		props := obj{}
		for _, k := range []string{"labels", "storageLocations", "guestFlush", "chainName"} {
			if v, ok := sp[k]; ok {
				props[k] = v
			}
		}
		pol["snapshotProperties"] = props
	}
	return pol, nil
}

var weekdays = map[string]time.Weekday{"SUNDAY": time.Sunday, "MONDAY": time.Monday, "TUESDAY": time.Tuesday, "WEDNESDAY": time.Wednesday,
	"THURSDAY": time.Thursday, "FRIDAY": time.Friday, "SATURDAY": time.Saturday}

func vpcOrStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// checkInstanceSchedule validates an instanceSchedulePolicy: a start or a
// stop schedule (or both) in unix-cron format, and an IANA time zone.
func checkInstanceSchedule(is map[string]any) (obj, error) {
	out := obj{}
	tz := vpcOrStr(str(is, "timeZone"), "UTC")
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, invalid("Invalid value for field 'resource.instanceSchedulePolicy.timeZone': '%s'. Must be an IANA time zone.", tz)
	}
	out["timeZone"] = tz
	given := false
	for _, k := range []string{"vmStartSchedule", "vmStopSchedule"} {
		m, _ := is[k].(map[string]any)
		if m == nil {
			continue
		}
		if _, err := parseCron(str(m, "schedule")); err != nil {
			return nil, invalid("Invalid value for field 'resource.instanceSchedulePolicy.%s.schedule': '%s'. %v", k, str(m, "schedule"), err)
		}
		out[k] = obj{"schedule": str(m, "schedule")}
		given = true
	}
	if !given {
		return nil, invalid("Invalid value for field 'resource.instanceSchedulePolicy': ''. At least one of vmStartSchedule or vmStopSchedule must be specified.")
	}
	for _, k := range []string{"startTime", "expirationTime"} {
		if v := str(is, k); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return nil, invalid("Invalid value for field 'resource.instanceSchedulePolicy.%s': '%s'. Must be an RFC 3339 time.", k, v)
			}
			out[k] = v
		}
	}
	return out, nil
}

// cron is a parsed unix-cron schedule: the minutes, hours, days of the
// month, months and days of the week it fires at.
type cron [5]map[int]bool

var cronRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

func parseCron(spec string) (cron, error) {
	var c cron
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return c, errString("a schedule is five unix-cron fields: minute, hour, day of month, month and day of week")
	}
	for i, f := range fields {
		c[i] = map[int]bool{}
		lo, hi := cronRanges[i][0], cronRanges[i][1]
		for _, part := range strings.Split(f, ",") {
			step := 1
			if a, b, ok := strings.Cut(part, "/"); ok {
				n, err := strconv.Atoi(b)
				if err != nil || n < 1 {
					return c, errString("bad step " + b)
				}
				part, step = a, n
			}
			from, to := lo, hi
			if part != "*" {
				a, b, isRange := strings.Cut(part, "-")
				x, err := strconv.Atoi(a)
				if err != nil {
					return c, errString("bad value " + a)
				}
				from, to = x, x
				if isRange {
					y, err := strconv.Atoi(b)
					if err != nil {
						return c, errString("bad value " + b)
					}
					to = y
				}
			}
			if i == 4 && to == 7 {
				to = 6
				c[i][0] = true
			}
			if from < lo || to > hi || from > to {
				return c, errString("value out of range in " + f)
			}
			for v := from; v <= to; v += step {
				c[i][v] = true
			}
		}
	}
	return c, nil
}

func (c cron) match(t time.Time) bool {
	return c[0][t.Minute()] && c[1][t.Hour()] && c[2][t.Day()] && c[3][int(t.Month())] && c[4][int(t.Weekday())]
}

type errString string

func (e errString) Error() string { return string(e) }

// checkPoliciesLocked resolves the resource policies a disk or an instance
// is given: of its region, existing, of the kind it takes.
func (s *Server) checkPoliciesLocked(project, region string, refs []string, field, kind string) ([]string, error) {
	var out []string
	for i, r := range refs {
		rel := Rel(r, project)
		if !strings.Contains(rel, "/") {
			rel = policyRel(project, region, rel)
		}
		o, ok := s.res[rel]
		if !ok || !strings.Contains(rel, "/resourcePolicies/") {
			return nil, notFound(rel)
		}
		if lastSeg(str(o, "region")) != region {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. The resource policy must be in region '%s'.", field, i, r, region)
		}
		if o[kind] == nil {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. The resource policy is not a %s.", field, i, r, map[string]string{
				"snapshotSchedulePolicy": "snapshot schedule", "instanceSchedulePolicy": "instance schedule"}[kind])
		}
		if !hasStr(out, rel) {
			out = append(out, rel)
		}
	}
	if len(out) > 1 && kind == "instanceSchedulePolicy" {
		return nil, invalid("Invalid value for field '%s': an instance can have at most one instance schedule.", field)
	}
	return out, nil
}

func (s *Server) diskPoliciesLocked(project, region string, refs []string, field string) ([]string, error) {
	return s.checkPoliciesLocked(project, region, refs, field, "snapshotSchedulePolicy")
}

// attachPolicies is addResourcePolicies / removeResourcePolicies of a disk
// or an instance.
func (s *Server) attachPolicies(project, scope, rel string, o obj, b obj, add bool, kind string) (obj, error) {
	refs := strsAny(b["resourcePolicies"])
	if len(refs) == 0 {
		return nil, required("resourcePolicies")
	}
	region := diskRegion(o) // a zonal or regional disk's, or an instance's
	rels, err := s.checkPoliciesLocked(project, region, refs, "resourcePolicies", kind)
	if err != nil {
		return nil, err
	}
	cur := strs(o, "resourcePolicies")
	verb := "addResourcePolicies"
	if add {
		for _, r := range rels {
			if hasStr(cur, r) {
				return nil, errf(http.StatusBadRequest, "alreadyExists", "The resource policy '%s' is already attached to '%s'.", r, rel)
			}
			cur = append(cur, r)
		}
		if kind == "instanceSchedulePolicy" && len(cur) > 1 {
			return nil, invalid("Invalid value for field 'resourcePolicies': an instance can have at most one instance schedule.")
		}
		if kind == "snapshotSchedulePolicy" {
			s.markScheduledLocked(o, rels)
		}
	} else {
		verb = "removeResourcePolicies"
		var keep []string
		for _, c := range cur {
			if !hasStr(rels, c) {
				keep = append(keep, c)
			}
		}
		cur = keep
	}
	if len(cur) == 0 {
		delete(o, "resourcePolicies")
	} else {
		o["resourcePolicies"] = toAny(cur)
	}
	return s.done(project, scope, verb, rel, o["id"])
}

// markScheduledLocked records when a disk was given each snapshot
// schedule: its first snapshot is the schedule's next time after that.
func (s *Server) markScheduledLocked(d obj, pols []string) {
	last, _ := d["_lastScheduled"].(map[string]any)
	if last == nil {
		last = map[string]any{}
	}
	for _, p := range pols {
		if _, ok := last[p]; !ok {
			last[p] = s.opts.Now().UTC().Format(time.RFC3339)
		}
	}
	d["_lastScheduled"] = last
}

// lastDue is the latest time at or before now a snapshot schedule falls
// due (UTC, as Google's schedules are).
func lastDue(sch map[string]any, now time.Time) time.Time {
	now = now.UTC()
	hour := func(hhmm string) int { h, _ := strconv.Atoi(strings.SplitN(hhmm, ":", 2)[0]); return h }
	if h, ok := sch["hourlySchedule"].(map[string]any); ok {
		step, _ := h["hoursInCycle"].(float64)
		base := time.Date(now.Year(), now.Month(), now.Day(), hour(str(h, "startTime")), 0, 0, 0, time.UTC)
		for base.After(now) {
			base = base.Add(-24 * time.Hour)
		}
		k := int(now.Sub(base) / (time.Duration(step) * time.Hour))
		return base.Add(time.Duration(k) * time.Duration(step) * time.Hour)
	}
	if d, ok := sch["dailySchedule"].(map[string]any); ok {
		t := time.Date(now.Year(), now.Month(), now.Day(), hour(str(d, "startTime")), 0, 0, 0, time.UTC)
		if t.After(now) {
			t = t.Add(-24 * time.Hour)
		}
		return t
	}
	var best time.Time
	w, _ := sch["weeklySchedule"].(map[string]any)
	for _, e := range listOf(w["dayOfWeeks"]) {
		dm, _ := e.(map[string]any)
		for back := 0; back < 8; back++ {
			day := now.AddDate(0, 0, -back)
			t := time.Date(day.Year(), day.Month(), day.Day(), hour(str(dm, "startTime")), 0, 0, 0, time.UTC)
			if t.Weekday() == weekdays[str(dm, "day")] && !t.After(now) {
				if t.After(best) {
					best = t
				}
				break
			}
		}
	}
	return best
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

// runSchedulesLocked takes the snapshots that fell due since the last run,
// applies each schedule's retention, and fires the instance schedules'
// start and stop times in (since, now].
func (s *Server) runSchedulesLocked(since, now time.Time) {
	keys := make([]string, 0, len(s.res))
	for k := range s.res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o, ok := s.res[k]
		if !ok {
			continue
		}
		switch {
		case strings.Contains(k, "/disks/"):
			s.snapshotDueLocked(k, o, now)
		case strings.Contains(k, "/instances/"):
			s.instanceScheduleLocked(k, o, since, now)
		}
	}
	s.applyRetentionLocked(now)
}

func (s *Server) snapshotDueLocked(rel string, d obj, now time.Time) {
	if str(d, "status") != "READY" || s.opts.VMs == nil {
		return
	}
	last, _ := d["_lastScheduled"].(map[string]any)
	for _, pr := range strs(d, "resourcePolicies") {
		pol, ok := s.res[pr]
		ss, _ := pol["snapshotSchedulePolicy"].(map[string]any)
		if !ok || ss == nil {
			continue
		}
		sch, _ := ss["schedule"].(map[string]any)
		due := lastDue(sch, now)
		prev, _ := time.Parse(time.RFC3339, str(last, pr))
		if !due.After(prev) {
			continue
		}
		if last == nil {
			last = map[string]any{}
			d["_lastScheduled"] = last
		}
		last[pr] = now.UTC().Format(time.RFC3339)
		project := strings.Split(rel, "/")[1]
		// Google's scheduled snapshot name: DISK-ZONE-TIMESTAMP-RANDOM
		// (scheduled-snapshots, "snapshots ... named
		// disk-name-zone-timestamp"), UNVERIFIED suffix.
		loc, scope := lastSeg(str(d, "zone")), "zones/"+lastSeg(str(d, "zone"))
		if loc == "" {
			// A regional disk's (regiondisks.go): DISK-REGION-TIMESTAMP-RANDOM.
			loc, scope = diskRegion(d), "regions/"+diskRegion(d)
		}
		name := str(d, "name") + "-" + loc + "-" + now.UTC().Format("20060102150405") + "-" + randHex(4)[:8]
		if len(name) > 63 {
			name = name[len(name)-63:]
			name = "s" + strings.TrimLeft(name[1:], "-")
		}
		body := obj{"name": name}
		props, _ := ss["snapshotProperties"].(map[string]any)
		if l, ok := props["labels"].(map[string]any); ok {
			body["labels"] = l
		}
		if _, err := s.createSnapshot(project, rel, body, scope); err == nil {
			if snap, ok := s.res["projects/"+project+"/global/snapshots/"+name]; ok {
				snap["autoCreated"] = true
				snap["sourceSnapshotSchedulePolicy"] = pr
				snap["sourceSnapshotSchedulePolicyId"] = pol["id"]
				if l, ok := props["labels"].(map[string]any); ok && len(l) > 0 {
					snap["labels"], snap["labelFingerprint"] = l, labelFingerprint(l)
				}
			}
		}
	}
}

// applyRetentionLocked deletes the scheduled snapshots older than their
// schedule's maxRetentionDays (and, with APPLY_RETENTION_POLICY, those of
// deleted disks too; KEEP_AUTO_SNAPSHOTS keeps a deleted disk's).
func (s *Server) applyRetentionLocked(now time.Time) {
	for rel, snap := range s.res {
		if !strings.Contains(rel, "/global/snapshots/") || snap["autoCreated"] != true || str(snap, "status") != "READY" {
			continue
		}
		pol, ok := s.res[str(snap, "sourceSnapshotSchedulePolicy")]
		if !ok {
			continue
		}
		ss, _ := pol["snapshotSchedulePolicy"].(map[string]any)
		rp, _ := ss["retentionPolicy"].(map[string]any)
		if _, diskLives := s.res[str(snap, "sourceDisk")]; !diskLives && str(rp, "onSourceDiskDelete") != "APPLY_RETENTION_POLICY" {
			continue
		}
		days, _ := rp["maxRetentionDays"].(float64)
		made, err := time.Parse("2006-01-02T15:04:05.000-07:00", str(snap, "creationTimestamp"))
		if err != nil || now.Sub(made) < time.Duration(days*24)*time.Hour {
			continue
		}
		project := strings.Split(rel, "/")[1]
		_, _ = s.snapshots(call{method: http.MethodDelete, project: project, parts: []string{"global", "snapshots", lastSeg(rel)}})
	}
}

// instanceScheduleLocked starts or stops an instance whose schedule has a
// start or stop time in (since, now], the later of the two winning.
func (s *Server) instanceScheduleLocked(rel string, o obj, since, now time.Time) {
	for _, pr := range strs(o, "resourcePolicies") {
		pol, ok := s.res[pr]
		is, _ := pol["instanceSchedulePolicy"].(map[string]any)
		if !ok || is == nil {
			continue
		}
		loc, err := time.LoadLocation(vpcOrStr(str(is, "timeZone"), "UTC"))
		if err != nil {
			continue
		}
		if st, err := time.Parse(time.RFC3339, str(is, "startTime")); err == nil && now.Before(st) {
			continue
		}
		if et, err := time.Parse(time.RFC3339, str(is, "expirationTime")); err == nil && !now.Before(et) {
			continue
		}
		var startAt, stopAt time.Time
		for _, k := range []string{"vmStartSchedule", "vmStopSchedule"} {
			m, _ := is[k].(map[string]any)
			c, err := parseCron(str(m, "schedule"))
			if m == nil || err != nil {
				continue
			}
			// Each minute in (since, now]; a runner that was down for
			// longer looks back an hour at most.
			from := since.Truncate(time.Minute).Add(time.Minute)
			if now.Sub(from) > time.Hour {
				from = now.Add(-time.Hour).Truncate(time.Minute)
			}
			for t := from; !t.After(now); t = t.Add(time.Minute) {
				if c.match(t.In(loc)) {
					if k == "vmStartSchedule" {
						startAt = t
					} else {
						stopAt = t
					}
				}
			}
		}
		verb := ""
		switch {
		case !startAt.IsZero() && startAt.After(stopAt):
			verb = "start"
		case !stopAt.IsZero():
			verb = "stop"
		}
		if verb == "" {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) != 6 {
			continue
		}
		status := str(o, "status")
		if (verb == "start" && status != stTerminated) || (verb == "stop" && status != stRunning) {
			continue
		}
		_, _ = s.instances(call{method: http.MethodPost, project: parts[1], parts: []string{"zones", parts[3], "instances", parts[5], verb}, body: obj{}}, parts[3])
	}
}

// scheduleLoop runs the schedules every 30 seconds until done closes.
func (s *Server) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	last := s.opts.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := s.opts.Now()
		s.mu.Lock()
		s.runSchedulesLocked(last, now)
		_ = s.saveLocked()
		s.mu.Unlock()
		last = now
	}
}
