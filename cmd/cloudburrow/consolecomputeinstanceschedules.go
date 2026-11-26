package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// computeInstanceSchedulesProvider is Instance schedules (#2206), Google's
// VM instances page's Instance schedules tab: the project's resource
// policies with an instanceSchedulePolicy at [region, name], with their
// start and stop schedules, time zone and VMs; Create schedule (name,
// description, region, start and stop schedules, time zone, initiation and
// end dates); a schedule's page with its VMs, Add instances to schedule,
// Remove instances from schedule and Delete.
type computeInstanceSchedulesProvider struct{ computeScreen }

func (computeInstanceSchedulesProvider) ID() string    { return "compute-instance-schedules" }
func (computeInstanceSchedulesProvider) Title() string { return "Instance schedules" }

// scheduleTimeZones are the time zones Create schedule offers first; any
// IANA name can be typed.
var scheduleTimeZones = []string{"UTC", "America/Los_Angeles", "America/New_York", "America/Sao_Paulo", "Europe/London",
	"Europe/Paris", "Europe/Berlin", "Asia/Kolkata", "Asia/Singapore", "Asia/Tokyo", "Australia/Sydney"}

func instanceScheduleOf(pol map[string]any, k string) string {
	return vpcStr(vmMap(vmMap(pol, "instanceSchedulePolicy"), k), "schedule")
}

func (p computeInstanceSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Start schedule", "Stop schedule", "Time zone", "In use by", "Next run"},
		Noun: "instance schedules", InfoOnSelect: true,
		Note: "An instance schedule starts and stops its VMs (at most one start and one stop an hour); a VM follows one schedule of its region."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Instance schedules are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/resourcePolicies", nil, nil)
	if err != nil {
		return listing, err
	}
	vms, err := p.call("GET", project, "aggregated/instances", nil, nil)
	if err != nil {
		return listing, err
	}
	count := map[string]int{}
	for _, v := range aggregatedItems(vms, "instances") {
		for _, rp := range vpcStrs(v, "resourcePolicies") {
			count[rp]++
		}
	}
	for _, pol := range aggregatedItems(m, "resourcePolicies") {
		isp := vmMap(pol, "instanceSchedulePolicy")
		if isp == nil {
			continue
		}
		path := []string{vpcLast(vpcStr(pol, "region")), vpcStr(pol, "name")}
		next := vpcStr(vmMap(vmMap(pol, "resourceStatus"), "instanceSchedulePolicy"), "nextRunStartTime")
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(pol, "name"), Opens: path, Target: path,
			Actions: p.actions(pol, nil, nil, false), Info: computePermissionsTab(),
			Fields: map[string]string{"Region": path[0], "Start schedule": vpcDash(instanceScheduleOf(pol, "vmStartSchedule")),
				"Stop schedule": vpcDash(instanceScheduleOf(pol, "vmStopSchedule")), "Time zone": vpcStr(isp, "timeZone"),
				"In use by": fmt.Sprintf("%d VM instances", count[vpcStr(pol, "selfLink")]), "Next run": vpcDash(next)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeInstanceSchedulesProvider) CreateOnPage() bool   { return true }
func (computeInstanceSchedulesProvider) CreateTitle() string  { return "Create a schedule" }
func (computeInstanceSchedulesProvider) CreateLayout() string { return console.CreateLayoutPage }

const cronPattern = `^$|^[0-9*,\-]+ [0-9*,\-]+ [0-9*,\-]+ [0-9*,\-]+ [0-9A-Za-z*,\-]+$`

// Create a schedule's Frequency (#2260). Google's console (Create a
// schedule, https://docs.cloud.google.com/compute/docs/instances/schedule-instance-start-stop,
// read 2026-11-25) has a Start time and a Stop time, and "the Frequency
// drop-down menu at the bottom of the pane" for "how often the Start time
// and Stop time repeat"; or, with the "Use CRON expression" toggle at the
// top of the pane, a Start CRON expression and a Stop CRON expression.
// The page does not list the menu's choices: these are the repeats one
// cron expression of the API's form says with a time of day, and the
// console turns them into the API's vmStartSchedule.schedule and
// vmStopSchedule.schedule, the only form the API has.
const (
	scheduleDaily   = "Daily"
	scheduleWeekly  = "Weekly"
	scheduleMonthly = "Monthly"
)

// scheduleWeekdays are cron's days of the week, 0 (Sunday) to 6.
var scheduleWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func scheduleDaysOfMonth() []string {
	out := make([]string, 31)
	for i := range out {
		out[i] = strconv.Itoa(i + 1)
	}
	return out
}

func (computeInstanceSchedulesProvider) CreateForm() (string, []console.Field) {
	return "Create schedule", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions(),
			Help: "A schedule applies to the VM instances of its region."},
		{Name: "useCron", Label: "Use CRON expression", Type: "checkbox", Section: "Schedule",
			Help: "For a schedule the start and stop times and Frequency cannot say: unix-cron, with *, - and ,."},
		{Name: "startClock", Label: "Start time", Type: "time", Section: "Schedule", ShowWhen: "!useCron",
			Help: "When the VM instances start. Leave it empty to start none. Scheduled operations can run up to 15 minutes late."},
		{Name: "stopClock", Label: "Stop time", Type: "time", Section: "Schedule", ShowWhen: "!useCron",
			Help: "When the VM instances stop. Keep at least 15 minutes between the start and the stop."},
		{Name: "startSchedule", Label: "Start CRON expression", Type: "text", Section: "Schedule", ShowWhen: "useCron", Pattern: cronPattern,
			Help: "minute hour day-of-month month day-of-week, such as 0 9 * * 1-5. Empty starts none."},
		{Name: "stopSchedule", Label: "Stop CRON expression", Type: "text", Section: "Schedule", ShowWhen: "useCron", Pattern: cronPattern,
			Help: "When to stop the VM instances, such as 0 18 * * 1-5."},
		{Name: "timeZone", Label: "Time zone", Type: "select", Section: "Schedule", Default: "UTC", Options: scheduleTimeZones,
			Help: "The time zone of the start and stop times."},
		{Name: "startTime", Label: "Initiate date", Type: "date", Section: "Schedule",
			Help: "Optional: the schedule takes effect on this date. Without it, it takes effect at once."},
		{Name: "expirationTime", Label: "End date", Type: "date", Section: "Schedule",
			Help: "Optional: the schedule ends on this date. Without it, it runs until it is deleted."},
		{Name: "frequency", Label: "Frequency", Type: "select", Section: "Schedule", ShowWhen: "!useCron", Default: scheduleDaily,
			Options: []string{scheduleDaily, scheduleWeekly, scheduleMonthly}, Help: "How often the start time and stop time repeat."},
		{Name: "weekdays", Label: "Days of the week", Type: "multiselect", Section: "Schedule", ShowWhen: "!useCron&frequency=" + scheduleWeekly,
			Options: []string{"1", "2", "3", "4", "5", "6", "0"}, OptionLabels: weekdayLabels(), Default: "1\n2\n3\n4\n5"},
		{Name: "dayOfMonth", Label: "Day of the month", Type: "select", Section: "Schedule", ShowWhen: "!useCron&frequency=" + scheduleMonthly,
			Default: "1", Options: scheduleDaysOfMonth(), Help: "Days 29 to 31 run only in the months that have them."},
	}
}

func weekdayLabels() map[string]string {
	m := map[string]string{}
	for i, d := range scheduleWeekdays {
		m[strconv.Itoa(i)] = d
	}
	return m
}

// scheduleCron is a time of day ("HH:MM") repeated at frequency as the
// API's unix-cron expression; "" for no time.
func scheduleCron(clock, frequency, weekdays, dayOfMonth string) (string, error) {
	clock = strings.TrimSpace(clock)
	if clock == "" {
		return "", nil
	}
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return "", fmt.Errorf("%q is not a time of day (HH:MM)", clock)
	}
	at := fmt.Sprintf("%d %d", t.Minute(), t.Hour())
	switch vpcOr(strings.TrimSpace(frequency), scheduleDaily) {
	case scheduleDaily:
		return at + " * * *", nil
	case scheduleWeekly:
		var days []string
		for _, d := range strings.FieldsFunc(weekdays, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 || n > 6 {
				return "", fmt.Errorf("days of the week: %q is not a day", d)
			}
			days = append(days, strconv.Itoa(n))
		}
		if len(days) == 0 {
			return "", errors.New("days of the week: choose at least one day")
		}
		slices.SortFunc(days, func(a, b string) int { return cmpWeekday(a) - cmpWeekday(b) })
		return at + " * * " + strings.Join(slices.Compact(days), ","), nil
	case scheduleMonthly:
		n, err := strconv.Atoi(strings.TrimSpace(dayOfMonth))
		if err != nil || n < 1 || n > 31 {
			return "", fmt.Errorf("day of the month: %q is not a day from 1 to 31", dayOfMonth)
		}
		return fmt.Sprintf("%s %d * *", at, n), nil
	}
	return "", fmt.Errorf("frequency: %q is not one of %s, %s and %s", frequency, scheduleDaily, scheduleWeekly, scheduleMonthly)
}

// cmpWeekday orders Monday first and Sunday last, as the menu lists them.
func cmpWeekday(d string) int {
	n, _ := strconv.Atoi(d)
	return (n + 6) % 7
}

// scheduleDate is a date field's YYYY-MM-DD as the API's RFC 3339 time:
// its midnight in the schedule's time zone.
func scheduleDate(v, zone, field string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if _, err := time.Parse(time.RFC3339, v); err == nil {
		return v, nil
	}
	loc, err := time.LoadLocation(vpcOr(zone, "UTC"))
	if err != nil {
		loc = time.UTC
	}
	d, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return "", fmt.Errorf("%s: %q is not a date", field, v)
	}
	return d.Format(time.RFC3339), nil
}

// scheduleInWords is an operation's cron in the form's terms, "09:00,
// weekly on Monday, Tuesday", when the form could have made it; "" else.
func scheduleInWords(cron string) string {
	f := strings.Fields(cron)
	if len(f) != 5 || f[3] != "*" {
		return ""
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return ""
	}
	at := fmt.Sprintf("%02d:%02d", h, m)
	switch {
	case f[2] == "*" && f[4] == "*":
		return at + ", daily"
	case f[2] == "*":
		var days []string
		for _, d := range strings.Split(f[4], ",") {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 || n > 6 {
				return ""
			}
			days = append(days, scheduleWeekdays[n])
		}
		return at + ", weekly on " + strings.Join(days, ", ")
	case f[4] == "*":
		if n, err := strconv.Atoi(f[2]); err == nil && n >= 1 && n <= 31 {
			return fmt.Sprintf("%s, monthly on day %d", at, n)
		}
	}
	return ""
}

func (p computeInstanceSchedulesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := strings.TrimSpace(values["region"])
	if !compute.IsRegion(region) {
		return "", fmt.Errorf("region: %q is not a region", region)
	}
	zone := vpcOr(values["timeZone"], "UTC")
	isp := map[string]any{"timeZone": zone}
	start, stop := strings.TrimSpace(values["startSchedule"]), strings.TrimSpace(values["stopSchedule"])
	// The form's times and Frequency unless Use CRON expression is on (an
	// API caller that names only the cron fields gets them as they are).
	if values["useCron"] != "true" && (strings.TrimSpace(values["startClock"]) != "" || strings.TrimSpace(values["stopClock"]) != "") {
		var err error
		if start, err = scheduleCron(values["startClock"], values["frequency"], values["weekdays"], values["dayOfMonth"]); err != nil {
			return "", fmt.Errorf("start time: %w", err)
		}
		if stop, err = scheduleCron(values["stopClock"], values["frequency"], values["weekdays"], values["dayOfMonth"]); err != nil {
			return "", fmt.Errorf("stop time: %w", err)
		}
		if start != "" && stop != "" {
			a, _ := time.Parse("15:04", strings.TrimSpace(values["startClock"]))
			b, _ := time.Parse("15:04", strings.TrimSpace(values["stopClock"]))
			gap := int(b.Sub(a).Abs().Minutes())
			if gap > 12*60 {
				gap = 24*60 - gap
			}
			if gap < 15 {
				return "", errors.New("stop time: keep at least 15 minutes between the start and stop operations")
			}
		}
	}
	if start == "" && stop == "" {
		return "", errors.New("start time: give a start time, a stop time or both")
	}
	for k, v := range map[string]string{"vmStartSchedule": start, "vmStopSchedule": stop} {
		if v != "" {
			isp[k] = map[string]any{"schedule": v}
		}
	}
	for k, label := range map[string]string{"startTime": "initiate date", "expirationTime": "end date"} {
		v, err := scheduleDate(values[k], zone, label)
		if err != nil {
			return "", err
		}
		if v != "" {
			isp[k] = v
		}
	}
	b := map[string]any{"name": name, "instanceSchedulePolicy": isp}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "regions/"+region+"/resourcePolicies", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

// vmsOf are the VM instances with the schedule pol, and the region's
// VM instances.
func (p computeInstanceSchedulesProvider) vmsOf(project, pol, region string) (in, all []map[string]any, err error) {
	m, err := p.call("GET", project, "aggregated/instances", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range aggregatedItems(m, "instances") {
		if regionOfZone(vpcLast(vpcStr(v, "zone"))) != region {
			continue
		}
		all = append(all, v)
		for _, rp := range vpcStrs(v, "resourcePolicies") {
			if rp == pol || strings.HasSuffix(rp, "/"+pol) {
				in = append(in, v)
			}
		}
	}
	return in, all, nil
}

func vmPathOf(v map[string]any) string {
	return "zones/" + vpcLast(vpcStr(v, "zone")) + "/instances/" + vpcStr(v, "name")
}

func (p computeInstanceSchedulesProvider) actions(pol map[string]any, in, all []map[string]any, onPage bool) []console.Action {
	region := vpcLast(vpcStr(pol, "region"))
	var acts []console.Action
	if onPage {
		member := map[string]bool{}
		labels := map[string]string{}
		var add, remove []string
		for _, v := range in {
			member[vmPathOf(v)] = true
			remove = append(remove, vmPathOf(v))
		}
		for _, v := range all {
			labels[vmPathOf(v)] = vpcStr(v, "name") + " (" + vpcLast(vpcStr(v, "zone")) + ")"
			if !member[vmPathOf(v)] {
				add = append(add, vmPathOf(v))
			}
		}
		a := console.Action{ID: "add", Label: "Add instances to schedule", Submit: "Add", Fields: []console.Field{
			{Name: "instance", Label: "VM instance", Type: "select", Required: true, Options: add, OptionLabels: labels,
				Help: "A VM instance of " + region + " that follows no other schedule."}}}
		if len(add) == 0 {
			a.Unavailable = "No other VM instance of " + region + " exists."
		}
		r := console.Action{ID: "remove", Label: "Remove instances from schedule", Submit: "Remove", Fields: []console.Field{
			{Name: "instance", Label: "VM instance", Type: "select", Required: true, Options: remove, OptionLabels: labels}}}
		if len(remove) == 0 {
			r.Unavailable = "No VM instance follows the schedule."
		}
		acts = append(acts, a, r)
	}
	// Google's confirm-only dialog, as every Compute Engine delete (#2139).
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete instance schedule " + vpcStr(pol, "name") + "?"}
	if onPage && len(in) > 0 {
		del.Unavailable = "The schedule has VM instances (" + vpcStr(in[0], "name") + "): remove them first."
	}
	return append(acts, del)
}

func (p computeInstanceSchedulesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.DeeperThan(2, path), nil
	}
	pol, err := p.call("GET", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	in, _, err := p.vmsOf(project, vpcStr(pol, "selfLink"), path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	vms := console.Listing{Columns: []string{"Zone", "Status"}, Noun: "VM instances"}
	for _, v := range in {
		vms.Items = append(vms.Items, console.Resource{Name: vpcStr(v, "name"),
			Fields: map[string]string{"Zone": vpcLast(vpcStr(v, "zone")), "Status": vpcStr(v, "status")}})
	}
	vms.Total = len(vms.Items)
	isp := vmMap(pol, "instanceSchedulePolicy")
	st := vmMap(vmMap(pol, "resourceStatus"), "instanceSchedulePolicy")
	return console.Detail{Title: path[1], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Region", Value: path[0]},
			{Label: "Start time", Value: vpcDash(scheduleInWords(instanceScheduleOf(pol, "vmStartSchedule")))},
			{Label: "Stop time", Value: vpcDash(scheduleInWords(instanceScheduleOf(pol, "vmStopSchedule")))},
			{Label: "Start schedule", Value: vpcDash(instanceScheduleOf(pol, "vmStartSchedule"))},
			{Label: "Stop schedule", Value: vpcDash(instanceScheduleOf(pol, "vmStopSchedule"))},
			{Label: "Time zone", Value: vpcStr(isp, "timeZone")},
			{Label: "Initiation date", Value: vpcDash(vpcStr(isp, "startTime"))}, {Label: "End date", Value: vpcDash(vpcStr(isp, "expirationTime"))},
			{Label: "Last run", Value: vpcDash(vpcStr(st, "lastRunStartTime"))}, {Label: "Next run", Value: vpcDash(vpcStr(st, "nextRunStartTime"))},
			{Label: "Description", Value: vpcDash(vpcStr(pol, "description"))}, {Label: "Creation time", Value: vpcStr(pol, "creationTimestamp")}}}}},
		{ID: "instances", Label: "VM instances", Listing: vms},
	}}, nil
}

func (p computeInstanceSchedulesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	pol, err := p.call("GET", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	if err != nil {
		return nil
	}
	in, all, err := p.vmsOf(project, vpcStr(pol, "selfLink"), path[0])
	if err != nil {
		return nil
	}
	return p.actions(pol, in, all, true)
}

func (p computeInstanceSchedulesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "regions/" + path[0] + "/resourcePolicies/" + path[1]
	link := "projects/" + project + "/" + base
	var err error
	switch action {
	case "add", "remove":
		vm := strings.TrimSpace(values["instance"])
		if !strings.Contains(vm, "/instances/") {
			return errors.New("instance: choose a VM instance")
		}
		verb := "/addResourcePolicies"
		if action == "remove" {
			verb = "/removeResourcePolicies"
		}
		_, err = p.call("POST", project, vm+verb, nil, map[string]any{"resourcePolicies": []any{link}})
	case "delete":
		_, err = p.call("DELETE", project, base, nil, nil)
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
	}
	return err
}
