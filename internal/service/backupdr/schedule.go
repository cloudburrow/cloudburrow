package backupdr

import (
	"strings"
	"time"
)

// schedule takes each rule's scheduled backups and drops expired ones,
// checking once a minute. A backup whose time passes while CloudBurrow is
// not running is taken at the next check inside its window.
func (s *Server) schedule(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

// tick takes the backups due at now and deletes the expired ones.
func (s *Server) tick(now time.Time) {
	type due struct{ assoc, rule string }
	var todo []due
	var expired []string
	s.mu.Lock()
	for _, n := range sortedKeys(s.st.Associations) {
		a := s.st.Associations[n]
		p := s.st.Plans[a.Plan]
		if p == nil {
			continue
		}
		for _, r := range p.Rules {
			if a.LastBackup[r.RuleID].State == "PENDING" {
				continue
			}
			if isDue(r.Schedule, s.lastScheduled(a.DataSource, r.RuleID), now) {
				todo = append(todo, due{n, r.RuleID})
			}
		}
	}
	for n, b := range s.st.Backups {
		if e, err := time.Parse(time.RFC3339Nano, b.ExpireTime); err == nil && now.After(e) {
			expired = append(expired, b.RunID)
			delete(s.st.Backups, n)
		}
	}
	if len(expired) > 0 {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	for _, id := range expired {
		_ = s.sql.DeleteRun(id)
	}
	for _, d := range todo {
		_, _, _ = s.backup(d.assoc, d.rule, false, 0, nil)
	}
}

// lastScheduled is when the rule's last scheduled backup of the data
// source was taken, zero if none, with s.mu held.
func (s *Server) lastScheduled(ds, rule string) time.Time {
	var last time.Time
	for _, b := range s.st.Backups {
		if b.DataSource == ds && b.RuleID == rule {
			if t, err := time.Parse(time.RFC3339Nano, b.CreateTime); err == nil && t.After(last) {
				last = t
			}
		}
	}
	return last
}

// isDue is whether a standardSchedule wants a backup at now, given the
// last one: now is inside the backup window, on a day the schedule names,
// and no backup was taken in this period (this window, or for HOURLY the
// last hourlyFrequency hours).
func isDue(sch map[string]any, last, now time.Time) bool {
	loc := time.UTC
	if tz, _ := sch["timeZone"].(string); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	now = now.In(loc)
	start, end := 0, 24
	if w, ok := sch["backupWindow"].(map[string]any); ok {
		if v, ok := w["startHourOfDay"].(float64); ok {
			start = int(v)
		}
		if v, ok := w["endHourOfDay"].(float64); ok && v > 0 {
			end = int(v)
		}
	}
	if now.Hour() < start || now.Hour() >= end {
		return false
	}
	rt, _ := sch["recurrenceType"].(string)
	if rt == "HOURLY" {
		f, _ := sch["hourlyFrequency"].(float64)
		if f < 1 {
			f = 1
		}
		return last.IsZero() || now.Sub(last) >= time.Duration(f)*time.Hour
	}
	switch rt {
	case "WEEKLY":
		if !names(sch["daysOfWeek"], strings.ToUpper(now.Weekday().String())) {
			return false
		}
	case "MONTHLY":
		if !numbers(sch["daysOfMonth"], now.Day()) {
			return false
		}
	case "YEARLY":
		if !names(sch["months"], strings.ToUpper(now.Month().String())) || !numbers(sch["daysOfMonth"], now.Day()) {
			return false
		}
	}
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), start, 0, 0, 0, loc)
	return last.IsZero() || last.Before(windowStart)
}

func names(v any, want string) bool {
	l, _ := v.([]any)
	for _, x := range l {
		if s, _ := x.(string); s == want {
			return true
		}
	}
	return false
}

func numbers(v any, want int) bool {
	l, _ := v.([]any)
	for _, x := range l {
		if f, _ := x.(float64); int(f) == want {
			return true
		}
	}
	return false
}
