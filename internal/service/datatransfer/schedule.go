package datatransfer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed transfer schedule, in the English-like syntax
// BigQuery documents for scheduled queries and dataset copies
// (https://docs.cloud.google.com/bigquery/docs/scheduling-queries): "every
// 24 hours", "every 3 hours", "every monday 09:00", "every mon,wed 23:30",
// "every day 00:00", "1st sunday of sep,oct,nov 00:00", "1,15 of month
// 09:00", "every 15 minutes from 08:00 to 18:00", "every 2 hours
// synchronized" and "every hour on mon,tue,wed,thu,fri". Times are UTC.
type Schedule struct {
	text string
	// interval is set for "every N minutes|hours".
	interval time.Duration
	// from and to bound an interval to a window of the day, in minutes
	// after midnight; synchronized aligns it to midnight.
	from, to     int
	window       bool
	synchronized bool
	// onDays limits an interval schedule to weekdays ("every hour on mon").
	onDays map[time.Weekday]bool

	// The calendar forms: at (minutes after midnight) on the days matched.
	at       int
	weekdays map[time.Weekday]bool // nil = every day
	ordinals map[int]bool          // 1-5, -1 for last; with weekdays
	monthDay map[int]bool          // "1,15 of month"
	months   map[time.Month]bool   // nil = every month
}

// String is the schedule as it was given.
func (s *Schedule) String() string { return s.text }

var (
	weekdayNames = map[string]time.Weekday{
		"sun": time.Sunday, "sunday": time.Sunday, "mon": time.Monday, "monday": time.Monday,
		"tue": time.Tuesday, "tuesday": time.Tuesday, "wed": time.Wednesday, "wednesday": time.Wednesday,
		"thu": time.Thursday, "thursday": time.Thursday, "fri": time.Friday, "friday": time.Friday,
		"sat": time.Saturday, "saturday": time.Saturday,
	}
	monthNames = map[string]time.Month{
		"jan": time.January, "january": time.January, "feb": time.February, "february": time.February,
		"mar": time.March, "march": time.March, "apr": time.April, "april": time.April, "may": time.May,
		"jun": time.June, "june": time.June, "jul": time.July, "july": time.July, "aug": time.August,
		"august": time.August, "sep": time.September, "september": time.September, "oct": time.October,
		"october": time.October, "nov": time.November, "november": time.November, "dec": time.December,
		"december": time.December,
	}
	ordinalNames = map[string]int{
		"1st": 1, "first": 1, "2nd": 2, "second": 2, "3rd": 3, "third": 3, "4th": 4, "fourth": 4,
		"5th": 5, "fifth": 5, "last": -1,
	}
	intervalRE = regexp.MustCompile(`^every (\d+ )?(minutes?|mins?|hours?|hrs?)( from (\d\d?:\d\d) to (\d\d?:\d\d))?( synchronized)?( on ([a-z,]+))?$`)
	timeRE     = regexp.MustCompile(`^(\d\d?):(\d\d)$`)
)

// ParseSchedule parses a schedule. The error is the message the API
// answers with INVALID_ARGUMENT.
func ParseSchedule(text string) (*Schedule, error) {
	norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	bad := func(why string) error {
		return fmt.Errorf("Invalid schedule: %q. %s", text, why)
	}
	if norm == "" {
		return nil, bad("The schedule is empty")
	}
	s := &Schedule{text: text}
	if m := intervalRE.FindStringSubmatch(norm); m != nil {
		n := 1
		if m[1] != "" {
			v, err := strconv.Atoi(strings.TrimSpace(m[1]))
			if err != nil || v <= 0 {
				return nil, bad("The interval must be a positive number")
			}
			n = v
		}
		unit := time.Minute
		if strings.HasPrefix(m[2], "h") {
			unit = time.Hour
		}
		s.interval = time.Duration(n) * unit
		if m[3] != "" {
			from, err1 := clockMinutes(m[4])
			to, err2 := clockMinutes(m[5])
			if err1 != nil || err2 != nil {
				return nil, bad("A time of day must be HH:MM")
			}
			s.from, s.to, s.window = from, to, true
		}
		s.synchronized = m[6] != ""
		if m[3] != "" && s.synchronized {
			return nil, bad("\"from ... to\" and \"synchronized\" cannot be combined")
		}
		if m[8] != "" {
			days, err := weekdayList(m[8])
			if err != nil {
				return nil, bad(err.Error())
			}
			s.onDays = days
		}
		return s, nil
	}

	words := strings.Split(norm, " ")
	at, err := clockMinutes(words[len(words)-1])
	if err != nil {
		return nil, bad("A calendar schedule ends with the time of day, HH:MM")
	}
	s.at = at
	words = words[:len(words)-1]
	switch {
	case len(words) == 2 && words[0] == "every":
		if words[1] != "day" {
			days, err := weekdayList(words[1])
			if err != nil {
				return nil, bad(err.Error())
			}
			s.weekdays = days
		}
		return s, nil
	case len(words) == 4 && words[2] == "of":
		ords := map[int]bool{}
		for _, o := range strings.Split(words[0], ",") {
			v, ok := ordinalNames[o]
			if !ok {
				return nil, bad(fmt.Sprintf("%q is not an ordinal such as 1st, second or last", o))
			}
			ords[v] = true
		}
		days, err := weekdayList(words[1])
		if err != nil {
			return nil, bad(err.Error())
		}
		s.ordinals, s.weekdays = ords, days
		if err := s.monthList(words[3]); err != nil {
			return nil, bad(err.Error())
		}
		return s, nil
	case len(words) == 3 && words[1] == "of":
		days := map[int]bool{}
		for _, d := range strings.Split(words[0], ",") {
			v, err := strconv.Atoi(d)
			if err != nil || v < 1 || v > 31 {
				return nil, bad(fmt.Sprintf("%q is not a day of the month, 1 to 31", d))
			}
			days[v] = true
		}
		s.monthDay = days
		if err := s.monthList(words[2]); err != nil {
			return nil, bad(err.Error())
		}
		return s, nil
	}
	return nil, bad("Use a form such as \"every 24 hours\", \"every monday 09:00\" or \"1st sunday of month 00:00\"")
}

func (s *Schedule) monthList(text string) error {
	if text == "month" {
		return nil
	}
	s.months = map[time.Month]bool{}
	for _, m := range strings.Split(text, ",") {
		v, ok := monthNames[m]
		if !ok {
			return fmt.Errorf("%q is not a month", m)
		}
		s.months[v] = true
	}
	return nil
}

func weekdayList(text string) (map[time.Weekday]bool, error) {
	days := map[time.Weekday]bool{}
	for _, d := range strings.Split(text, ",") {
		v, ok := weekdayNames[d]
		if !ok {
			return nil, fmt.Errorf("%q is not a day of the week", d)
		}
		days[v] = true
	}
	return days, nil
}

func clockMinutes(text string) (int, error) {
	m := timeRE.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("%q is not HH:MM", text)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if h > 23 || mi > 59 {
		return 0, fmt.Errorf("%q is not a time of day", text)
	}
	return h*60 + mi, nil
}

// MinimumInterval is the shortest gap between two runs, for the data
// source's minimum_schedule_interval check.
func (s *Schedule) MinimumInterval() time.Duration {
	if s.interval > 0 {
		return s.interval
	}
	return 24 * time.Hour
}

// Next is the first run time strictly after `after`. anchor is where an
// unsynchronized interval schedule counts from: the config's start_time, or
// when it was created.
func (s *Schedule) Next(after, anchor time.Time) time.Time {
	after = after.UTC()
	if s.interval > 0 {
		return s.nextInterval(after, anchor.UTC())
	}
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < 366*5; i++ {
		d := day.AddDate(0, 0, i)
		t := d.Add(time.Duration(s.at) * time.Minute)
		if t.After(after) && s.matchesDay(d) {
			return t
		}
	}
	return time.Time{}
}

func (s *Schedule) matchesDay(d time.Time) bool {
	if s.months != nil && !s.months[d.Month()] {
		return false
	}
	if s.monthDay != nil {
		return s.monthDay[d.Day()]
	}
	if s.weekdays != nil && !s.weekdays[d.Weekday()] {
		return false
	}
	if s.ordinals != nil {
		nth := (d.Day()-1)/7 + 1
		last := d.AddDate(0, 0, 7).Month() != d.Month()
		return s.ordinals[nth] || (last && s.ordinals[-1])
	}
	return true
}

func (s *Schedule) nextInterval(after, anchor time.Time) time.Time {
	dayOK := func(t time.Time) bool { return s.onDays == nil || s.onDays[t.Weekday()] }
	if !s.window && !s.synchronized {
		// The first anchor + k*interval strictly after `after`, for any k,
		// so a backfill before the anchor lands on the same grid.
		diff := after.Sub(anchor)
		k := diff / s.interval
		if diff < 0 && diff%s.interval != 0 {
			k--
		}
		t := anchor.Add((k + 1) * s.interval)
		for i := 0; !dayOK(t) && i < 100000; i++ {
			t = t.Add(s.interval)
		}
		return t
	}
	// Window or synchronized: the runs of each day start at from (or
	// midnight) and step by the interval up to to (or the day's end).
	from, to := 0, 24*60-1
	if s.window {
		from, to = s.from, s.to
	}
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		d := day.AddDate(0, 0, i)
		if !dayOK(d) {
			continue
		}
		start := d.Add(time.Duration(from) * time.Minute)
		end := d.Add(time.Duration(to) * time.Minute)
		if to < from {
			end = end.Add(24 * time.Hour)
		}
		t := start
		if after.After(start) || after.Equal(start) {
			t = start.Add((after.Sub(start)/s.interval + 1) * s.interval)
		}
		if !t.After(end) {
			return t
		}
	}
	return time.Time{}
}
