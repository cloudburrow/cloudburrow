package compute

// Snapshot schedules (#2206): resource policies with a
// snapshotSchedulePolicy, attached to zonal and regional disks, as
// https://docs.cloud.google.com/compute/docs/disks/scheduled-snapshots and
// https://docs.cloud.google.com/compute/docs/disks/about-snapshot-schedules
// document them (read 2026-11-24):
//
//   - "You must create your snapshot schedule in the same region where
//     your disk resides";
//   - one of hourlySchedule (hoursInCycle "between 1 and 23"),
//     dailySchedule and weeklySchedule ("mutually-exclusive"), each start
//     time in UTC that "must start on the hour";
//   - retentionPolicy.maxRetentionDays "1 or higher", or none ("retained
//     indefinitely"); onSourceDiskDelete KEEP_AUTO_SNAPSHOTS (the default)
//     or APPLY_RETENTION_POLICY; an expired snapshot is deleted "only if
//     you have a newer snapshot of the disk available";
//   - "You can attach a maximum of 10 snapshot schedules at a time to a
//     Persistent Disk volume", "a snapshot schedule to a maximum of 1000
//     disks", "a maximum of 1000 in-use snapshot schedules per region";
//   - "you can make changes to the description, schedule, retention
//     policy, and labels" (resourcePolicies.patch); other changes need a
//     new schedule;
//   - a snapshot is named "[disk-name-prefix]-[scope-name]-[timestamp]-
//     [random-chars]", the prefix at most 20 characters, the timestamp
//     YYYYMMDDHHMMSS in UTC; it "begins within the hour of the specified
//     start time" (here, at the first scheduler pass in that hour); a CMEK
//     disk's snapshots are "encrypted with the same key".
//
// Placement, instance schedule and workload policies apply to instances,
// whose addResourcePolicies is not served; they stay 501.

import (
	"context"
	"crypto/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxDiskSchedules     = 10
	maxScheduleDisks     = 1000
	maxRegionSchedules   = 1000
	lastScheduledKeyPref = "_lastScheduled:"
)

var weekdays = map[string]time.Weekday{"SUNDAY": time.Sunday, "MONDAY": time.Monday, "TUESDAY": time.Tuesday,
	"WEDNESDAY": time.Wednesday, "THURSDAY": time.Thursday, "FRIDAY": time.Friday, "SATURDAY": time.Saturday}

func isSchedule(po obj) bool { return po["snapshotSchedulePolicy"] != nil }

// startHour parses a start time that is on the hour ("HH:00").
func startHour(v, field string) (int, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, invalid("Invalid value for field '%s': '%s'. The start time is in the HH:MM format, in UTC.", field, v)
	}
	if t.Minute() != 0 {
		return 0, invalid("Invalid value for field '%s': '%s'. The start time must start on the hour.", field, v)
	}
	return t.Hour(), nil
}

// checkSchedule validates a snapshotSchedulePolicy.schedule and returns it
// with Google's outputs (each schedule's duration).
func checkSchedule(sc obj, field string) (obj, error) {
	var kinds []string
	for _, k := range []string{"hourlySchedule", "dailySchedule", "weeklySchedule"} {
		if sc[k] != nil {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return nil, invalid("Invalid value for field '%s': exactly one of hourlySchedule, dailySchedule and weeklySchedule must be set.", field)
	}
	k := kinds[0]
	s, _ := sc[k].(map[string]any)
	f := field + "." + k
	out := obj{}
	switch k {
	case "hourlySchedule":
		n, _, err := intField(s["hoursInCycle"], f+".hoursInCycle")
		if err != nil {
			return nil, err
		}
		if n < 1 || n > 23 {
			return nil, invalid("Invalid value for field '%s.hoursInCycle': '%d'. Must be an integer between 1 and 23.", f, n)
		}
		if _, err := startHour(str(s, "startTime"), f+".startTime"); err != nil {
			return nil, err
		}
		out[k] = obj{"hoursInCycle": float64(n), "startTime": str(s, "startTime"), "duration": "PT14400S"}
	case "dailySchedule":
		n, _, err := intField(s["daysInCycle"], f+".daysInCycle")
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, invalid("Invalid value for field '%s.daysInCycle': '%d'. A daily schedule runs every day: daysInCycle must be 1.", f, n)
		}
		if _, err := startHour(str(s, "startTime"), f+".startTime"); err != nil {
			return nil, err
		}
		out[k] = obj{"daysInCycle": float64(n), "startTime": str(s, "startTime"), "duration": "PT14400S"}
	case "weeklySchedule":
		l, _ := s["dayOfWeeks"].([]any)
		if len(l) == 0 {
			return nil, required(f + ".dayOfWeeks")
		}
		var days []any
		for i, e := range l {
			dw, _ := e.(map[string]any)
			if _, ok := weekdays[str(dw, "day")]; !ok {
				return nil, invalid("Invalid value for field '%s.dayOfWeeks[%d].day': '%s'.", f, i, str(dw, "day"))
			}
			if _, err := startHour(str(dw, "startTime"), f+".dayOfWeeks["+strconv.Itoa(i)+"].startTime"); err != nil {
				return nil, err
			}
			days = append(days, obj{"day": str(dw, "day"), "startTime": str(dw, "startTime"), "duration": "PT14400S"})
		}
		out[k] = obj{"dayOfWeeks": days}
	}
	return out, nil
}

func checkRetention(rp obj, field string) (obj, error) {
	out := obj{"onSourceDiskDelete": "KEEP_AUTO_SNAPSHOTS"}
	if rp == nil {
		return out, nil
	}
	if v, set, err := intField(rp["maxRetentionDays"], field+".maxRetentionDays"); err != nil {
		return nil, err
	} else if set {
		if v < 1 {
			return nil, invalid("Invalid value for field '%s.maxRetentionDays': '%d'. You must use a value of 1 or higher.", field, v)
		}
		out["maxRetentionDays"] = float64(v)
	}
	switch o := str(rp, "onSourceDiskDelete"); o {
	case "", "KEEP_AUTO_SNAPSHOTS", "APPLY_RETENTION_POLICY":
		if o != "" {
			out["onSourceDiskDelete"] = o
		}
	default:
		return nil, invalid("Invalid value for field '%s.onSourceDiskDelete': '%s'.", field, o)
	}
	return out, nil
}

// newSnapshotSchedule is the snapshotSchedulePolicy of a new policy.
func (s *Server) newSnapshotSchedule(project, region string, b obj) (obj, error) {
	sp, _ := b["snapshotSchedulePolicy"].(map[string]any)
	sc, _ := sp["schedule"].(map[string]any)
	if sc == nil {
		return nil, required("resource.snapshotSchedulePolicy.schedule")
	}
	sched, err := checkSchedule(sc, "resource.snapshotSchedulePolicy.schedule")
	if err != nil {
		return nil, err
	}
	rp, _ := sp["retentionPolicy"].(map[string]any)
	ret, err := checkRetention(rp, "resource.snapshotSchedulePolicy.retentionPolicy")
	if err != nil {
		return nil, err
	}
	out := obj{"schedule": sched, "retentionPolicy": ret}
	if props, ok := sp["snapshotProperties"].(map[string]any); ok {
		p := obj{}
		for _, k := range []string{"labels", "storageLocations", "guestFlush", "chainName"} {
			if v, ok := props[k]; ok {
				p[k] = v
			}
		}
		if l := strs(props, "storageLocations"); len(l) > 1 {
			return nil, invalid("Invalid value for field 'resource.snapshotSchedulePolicy.snapshotProperties.storageLocations': a snapshot has one storage location.")
		}
		out["snapshotProperties"] = p
	}
	n := 0
	for k, o := range s.res {
		if strings.HasPrefix(k, "projects/"+project+"/regions/"+region+"/resourcePolicies/") && isSchedule(o) && len(s.policyUsersLocked(k)) > 0 {
			n++
		}
	}
	if n >= maxRegionSchedules {
		return nil, errf(http.StatusForbidden, "quotaExceeded", "You can create a maximum of %d in-use snapshot schedules per region.", maxRegionSchedules)
	}
	return out, nil
}

// patchResourcePolicy is resourcePolicies.patch of a snapshot schedule:
// its description, schedule, retention policy and labels.
func (s *Server) patchResourcePolicy(c call, rel string, po obj) (obj, error) {
	if !isSchedule(po) {
		return nil, invalid("Invalid value for field 'resource': only a snapshot schedule's description, schedule, retention policy and labels can be changed: delete the resource policy and create a new one.")
	}
	var paths []string
	for _, p := range c.query["updateMask"] {
		for _, f := range strings.Split(p, ",") {
			if f = strings.TrimSpace(f); f != "" {
				paths = append(paths, camel(f))
			}
		}
	}
	b := c.body
	sp, _ := b["snapshotSchedulePolicy"].(map[string]any)
	if len(paths) == 0 {
		// Without a mask, the fields the body sets.
		if _, ok := b["description"]; ok {
			paths = append(paths, "description")
		}
		if _, ok := b["labels"]; ok {
			paths = append(paths, "labels")
		}
		if sp["schedule"] != nil {
			paths = append(paths, "snapshotSchedulePolicy.schedule")
		}
		if sp["retentionPolicy"] != nil {
			paths = append(paths, "snapshotSchedulePolicy.retentionPolicy")
		}
	}
	cur, _ := po["snapshotSchedulePolicy"].(map[string]any)
	next := obj{}
	for k, v := range cur {
		next[k] = v
	}
	desc, labels := po["description"], po["labels"]
	for _, p := range paths {
		switch {
		case p == "description":
			desc = str(b, "description")
		case p == "labels":
			labels = b["labels"]
		case p == "snapshotSchedulePolicy.schedule" || strings.HasPrefix(p, "snapshotSchedulePolicy.schedule."):
			sc, _ := sp["schedule"].(map[string]any)
			v, err := checkSchedule(sc, "resource.snapshotSchedulePolicy.schedule")
			if err != nil {
				return nil, err
			}
			next["schedule"] = v
		case p == "snapshotSchedulePolicy.retentionPolicy" || strings.HasPrefix(p, "snapshotSchedulePolicy.retentionPolicy."):
			rp, _ := sp["retentionPolicy"].(map[string]any)
			v, err := checkRetention(rp, "resource.snapshotSchedulePolicy.retentionPolicy")
			if err != nil {
				return nil, err
			}
			next["retentionPolicy"] = v
		default:
			return nil, invalid("Invalid value for field 'updateMask': '%s'. After you create a snapshot schedule, you can change its description, schedule, retention policy and labels only.", p)
		}
	}
	po["snapshotSchedulePolicy"] = norm(next)
	if d, _ := desc.(string); d != "" {
		po["description"] = d
	} else {
		delete(po, "description")
	}
	if l, ok := labels.(map[string]any); ok && len(l) > 0 {
		po["labels"] = l
	} else {
		delete(po, "labels")
	}
	return s.done(c.project, "regions/"+lastSeg(str(po, "region")), "patch", rel, po["id"])
}

// checkScheduleAttachLocked applies the snapshot schedule limits to d
// taking the schedule pol, d already having those in cur.
func (s *Server) checkScheduleAttachLocked(cur []string, pol, field string, i int) error {
	n := 0
	for _, p := range cur {
		if po, ok := s.res[p]; ok && isSchedule(po) {
			n++
		}
	}
	if n >= maxDiskSchedules {
		return invalid("Invalid value for field '%s[%d]': '%s'. You can attach a maximum of %d snapshot schedules at a time to a disk.", field, i, pol, maxDiskSchedules)
	}
	if len(s.policyUsersLocked(pol)) >= maxScheduleDisks {
		return invalid("Invalid value for field '%s[%d]': '%s'. You can attach a snapshot schedule to a maximum of %d disks.", field, i, pol, maxScheduleDisks)
	}
	return nil
}

// RunSnapshotSchedules takes the scheduled snapshots and applies the
// retention policies every minute until ctx is done.
func (s *Server) RunSnapshotSchedules(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			s.runSchedulesLocked()
			s.mu.Unlock()
		}
	}
}

// dueSlot is the latest start of the schedule sc at or before now.
func dueSlot(sc obj, now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch {
	case sc["hourlySchedule"] != nil:
		h, _ := sc["hourlySchedule"].(map[string]any)
		start, _ := startHour(str(h, "startTime"), "")
		n, _, _ := intField(h["hoursInCycle"], "")
		if n < 1 {
			return time.Time{}
		}
		t := day.Add(time.Duration(start) * time.Hour)
		if t.After(now) {
			t = t.Add(-24 * time.Hour)
		}
		k := int64(now.Sub(t) / (time.Duration(n) * time.Hour))
		return t.Add(time.Duration(k*n) * time.Hour)
	case sc["dailySchedule"] != nil:
		d, _ := sc["dailySchedule"].(map[string]any)
		start, _ := startHour(str(d, "startTime"), "")
		t := day.Add(time.Duration(start) * time.Hour)
		if t.After(now) {
			t = t.Add(-24 * time.Hour)
		}
		return t
	case sc["weeklySchedule"] != nil:
		w, _ := sc["weeklySchedule"].(map[string]any)
		l, _ := w["dayOfWeeks"].([]any)
		var best time.Time
		for _, e := range l {
			dw, _ := e.(map[string]any)
			start, _ := startHour(str(dw, "startTime"), "")
			wd := weekdays[str(dw, "day")]
			back := (int(now.Weekday()) - int(wd) + 7) % 7
			t := day.AddDate(0, 0, -back).Add(time.Duration(start) * time.Hour)
			if t.After(now) {
				t = t.AddDate(0, 0, -7)
			}
			if t.After(best) {
				best = t
			}
		}
		return best
	}
	return time.Time{}
}

// runSchedulesLocked takes each disk's due snapshots and deletes the
// expired ones.
func (s *Server) runSchedulesLocked() {
	if s.opts.VMs == nil {
		return
	}
	now := s.opts.Now().UTC()
	var pols []string
	for k, o := range s.res {
		if strings.Contains(k, "/resourcePolicies/") && isSchedule(o) {
			pols = append(pols, k)
		}
	}
	sort.Strings(pols)
	for _, pol := range pols {
		po := s.res[pol]
		sp, _ := po["snapshotSchedulePolicy"].(map[string]any)
		sc, _ := sp["schedule"].(map[string]any)
		slot := dueSlot(sc, now)
		created, _ := time.Parse(time.RFC3339Nano, str(po, "creationTimestamp"))
		if slot.IsZero() || slot.Before(created.Truncate(time.Hour)) {
			continue
		}
		for _, dr := range s.policyUsersLocked(pol) {
			d := s.res[dr]
			if str(d, "status") != "READY" {
				continue
			}
			last, _ := time.Parse(time.RFC3339, str(d, lastScheduledKeyPref+pol))
			if !last.Before(slot) {
				continue
			}
			d[lastScheduledKeyPref+pol] = slot.Format(time.RFC3339)
			s.scheduledSnapshotLocked(pol, po, dr, d, now)
		}
	}
	s.expireSnapshotsLocked(now)
}

func randChars(n int) string {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = a[int(b[i])%len(a)]
	}
	return string(b)
}

// scheduledSnapshotLocked takes the snapshot the schedule pol owes the
// disk dr.
func (s *Server) scheduledSnapshotLocked(pol string, po obj, dr string, d obj, now time.Time) {
	prefix := str(d, "name")
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	scopeName := lastSeg(str(d, "zone"))
	scope := "zones/" + scopeName
	if isRegional(d) {
		scopeName = diskRegion(d)
		scope = "regions/" + scopeName
	}
	name := strings.TrimRight(prefix, "-") + "-" + scopeName + "-" + now.Format("20060102150405") + "-" + randChars(8)
	project := strings.Split(dr, "/")[1]
	sp, _ := po["snapshotSchedulePolicy"].(map[string]any)
	props, _ := sp["snapshotProperties"].(map[string]any)
	if _, err := s.createSnapshot(project, dr, obj{"name": name}, scope); err != nil {
		return
	}
	snap := s.res["projects/"+project+"/global/snapshots/"+name]
	if snap == nil {
		return
	}
	snap["autoCreated"] = true
	snap["sourceSnapshotSchedulePolicy"] = pol
	snap["sourceSnapshotSchedulePolicyId"] = po["id"]
	if l, ok := props["labels"].(map[string]any); ok && len(l) > 0 {
		snap["labels"] = l
		snap["labelFingerprint"] = labelFingerprint(l)
	}
	if l := strs(props, "storageLocations"); len(l) > 0 {
		snap["storageLocations"] = toAny(l)
	}
	if g, _ := props["guestFlush"].(bool); g {
		snap["guestFlush"] = true
	}
	if c := str(props, "chainName"); c != "" {
		snap["chainName"] = c
	}
}

// expireSnapshotsLocked deletes the scheduled snapshots older than their
// schedule's maxRetentionDays, each only while a newer snapshot of its
// disk exists; a deleted disk's are kept unless its schedule says
// APPLY_RETENTION_POLICY.
func (s *Server) expireSnapshotsLocked(now time.Time) {
	newest := map[string]time.Time{}
	for k, o := range s.res {
		if strings.Contains(k, "/global/snapshots/") && str(o, "status") == "READY" {
			t, _ := time.Parse(time.RFC3339Nano, str(o, "creationTimestamp"))
			if src := str(o, "sourceDisk"); t.After(newest[src]) {
				newest[src] = t
			}
		}
	}
	var expired []string
	for k, o := range s.res {
		auto, _ := o["autoCreated"].(bool)
		if !auto || !strings.Contains(k, "/global/snapshots/") || str(o, "status") != "READY" {
			continue
		}
		po, ok := s.res[str(o, "sourceSnapshotSchedulePolicy")]
		if !ok {
			continue
		}
		sp, _ := po["snapshotSchedulePolicy"].(map[string]any)
		rp, _ := sp["retentionPolicy"].(map[string]any)
		days, set, _ := intField(rp["maxRetentionDays"], "")
		if !set {
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, str(o, "creationTimestamp"))
		if now.Sub(t) < time.Duration(days)*24*time.Hour {
			continue
		}
		src := str(o, "sourceDisk")
		if _, alive := s.res[src]; alive {
			if !newest[src].After(t) {
				continue // the disk's newest snapshot is kept
			}
		} else if str(rp, "onSourceDiskDelete") != "APPLY_RETENTION_POLICY" {
			continue
		}
		expired = append(expired, k)
	}
	sort.Strings(expired)
	for _, rel := range expired {
		o := s.res[rel]
		key := vmKey("snap", o)
		o["status"] = "DELETING"
		project := strings.Split(rel, "/")[1]
		_, _ = s.runOpLocked(project, "global", "delete", rel, func(ctx context.Context) error {
			return s.opts.VMs.DeleteSnapshot(ctx, key)
		}, func(err error) {
			if err == nil {
				delete(s.res, rel)
			} else if o, ok := s.res[rel]; ok {
				o["status"] = "READY"
			}
		})
	}
}
