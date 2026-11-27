package main

import (
	"context"
	"testing"
)

// Create a schedule's Start time, Stop time and Frequency (#2260), as
// Google's console has them, become the API's unix-cron vmStartSchedule and
// vmStopSchedule; Use CRON expression takes the expressions as typed; the
// schedule's page says each cron the form could have made in its terms.
func TestConsoleComputeInstanceScheduleFrequency(t *testing.T) {
	for _, c := range []struct {
		clock, freq, days, dom, want string
	}{
		{"09:00", "Daily", "", "", "0 9 * * *"},
		{"18:30", "Weekly", "1\n2\n3\n4\n5", "", "30 18 * * 1,2,3,4,5"},
		{"07:05", "Weekly", "0\n6\n1", "", "5 7 * * 1,6,0"},
		{"23:45", "Monthly", "", "15", "45 23 15 * *"},
		{"", "Daily", "", "", ""},
	} {
		got, err := scheduleCron(c.clock, c.freq, c.days, c.dom)
		if err != nil || got != c.want {
			t.Errorf("scheduleCron(%q, %q, %q, %q) = %q, %v; want %q", c.clock, c.freq, c.days, c.dom, got, err, c.want)
		}
	}
	for _, c := range [][4]string{{"9am", "Daily", "", ""}, {"09:00", "Weekly", "", ""}, {"09:00", "Monthly", "", "32"}, {"09:00", "Hourly", "", ""}} {
		if got, err := scheduleCron(c[0], c[1], c[2], c[3]); err == nil {
			t.Errorf("scheduleCron(%q) = %q, want an error", c, got)
		}
	}
	for cron, want := range map[string]string{"0 9 * * *": "09:00, daily", "30 18 * * 1,2,3,4,5": "18:30, weekly on Monday, Tuesday, Wednesday, Thursday, Friday",
		"45 23 15 * *": "23:45, monthly on day 15", "0 9 * * 1-5": "", "0 9 1 6 *": ""} {
		if got := scheduleInWords(cron); got != want {
			t.Errorf("scheduleInWords(%q) = %q, want %q", cron, got, want)
		}
	}

	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "freq-p"
	scheds := computeInstanceSchedulesProvider{s}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "close", "region": "us-central1", "startClock": "09:00", "stopClock": "09:10",
		"frequency": "Daily", "useCron": "false"}); err == nil {
		t.Error("a start and a stop 10 minutes apart were accepted")
	}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "none", "region": "us-central1", "useCron": "false", "frequency": "Daily"}); err == nil {
		t.Error("a schedule with neither a start nor a stop was accepted")
	}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "weekdays", "region": "us-central1", "useCron": "false",
		"startClock": "08:30", "stopClock": "19:00", "frequency": "Weekly", "weekdays": "1\n2\n3\n4\n5", "timeZone": "Europe/Paris",
		"startTime": "2027-01-04", "expirationTime": "2027-06-30", "startSchedule": "", "stopSchedule": ""}); err != nil {
		t.Fatal(err)
	}
	// Use CRON expression: the expressions are the API's, the times ignored.
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "cron", "region": "us-central1", "useCron": "true",
		"startClock": "08:30", "startSchedule": "0 9 * * 1-5", "stopSchedule": "0 18 * * 1-5", "frequency": "Daily"}); err != nil {
		t.Fatal(err)
	}
	pol, err := s.call("GET", project, "regions/us-central1/resourcePolicies/weekdays", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	isp := vmMap(pol, "instanceSchedulePolicy")
	if got := instanceScheduleOf(pol, "vmStartSchedule"); got != "30 8 * * 1,2,3,4,5" {
		t.Errorf("vmStartSchedule = %q", got)
	}
	if got := instanceScheduleOf(pol, "vmStopSchedule"); got != "0 19 * * 1,2,3,4,5" {
		t.Errorf("vmStopSchedule = %q", got)
	}
	if vpcStr(isp, "startTime") != "2027-01-04T00:00:00+01:00" || vpcStr(isp, "expirationTime") != "2027-06-30T00:00:00+02:00" {
		t.Errorf("initiate and end dates = %q, %q; want their midnights in Europe/Paris", vpcStr(isp, "startTime"), vpcStr(isp, "expirationTime"))
	}
	cron, err := s.call("GET", project, "regions/us-central1/resourcePolicies/cron", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := instanceScheduleOf(cron, "vmStartSchedule"); got != "0 9 * * 1-5" {
		t.Errorf("Use CRON expression's start = %q", got)
	}
	det, err := scheds.Detail(ctx, project, []string{"us-central1", "weekdays"})
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, p := range det.Sections[0].Groups[0].Properties {
		props[p.Label] = p.Value
	}
	if props["Start time"] != "08:30, weekly on Monday, Tuesday, Wednesday, Thursday, Friday" || props["Stop schedule"] != "0 19 * * 1,2,3,4,5" {
		t.Errorf("the schedule's page: %v", props)
	}
}
