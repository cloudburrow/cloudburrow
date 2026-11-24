package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// computeSnapshotSchedulesProvider is Snapshot schedules (#2206), Google's
// Snapshots page's Snapshot schedules tab: the project's resource
// policies with a snapshotSchedulePolicy at [region, name], Create
// snapshot schedule (name, description, region, snapshot location,
// frequency, start time, auto-delete, deletion rule, labels), a schedule's
// page with its disks, Attach to disk, Detach from disk and Delete.
type computeSnapshotSchedulesProvider struct{ computeScreen }

func (computeSnapshotSchedulesProvider) ID() string    { return "compute-snapshotschedules" }
func (computeSnapshotSchedulesProvider) Title() string { return "Snapshot schedules" }

// scheduleText is a schedule's frequency as Google's list shows it.
func scheduleText(pol map[string]any) string {
	sp := vmMap(pol, "snapshotSchedulePolicy")
	sc := vmMap(sp, "schedule")
	switch {
	case vmMap(sc, "hourlySchedule") != nil:
		h := vmMap(sc, "hourlySchedule")
		return fmt.Sprintf("Every %v hours from %s UTC", h["hoursInCycle"], vpcStr(h, "startTime"))
	case vmMap(sc, "dailySchedule") != nil:
		return "Daily at " + vpcStr(vmMap(sc, "dailySchedule"), "startTime") + " UTC"
	case vmMap(sc, "weeklySchedule") != nil:
		var days []string
		l, _ := vmMap(sc, "weeklySchedule")["dayOfWeeks"].([]any)
		for _, e := range l {
			if m, ok := e.(map[string]any); ok {
				days = append(days, vpcStr(m, "day")+" "+vpcStr(m, "startTime"))
			}
		}
		return "Weekly: " + strings.Join(days, ", ") + " UTC"
	}
	return "—"
}

func retentionText(pol map[string]any) string {
	rp := vmMap(vmMap(pol, "snapshotSchedulePolicy"), "retentionPolicy")
	if rp["maxRetentionDays"] == nil {
		return "Keep snapshots"
	}
	return fmt.Sprintf("%v days", rp["maxRetentionDays"])
}

func (p computeSnapshotSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Frequency", "Auto-delete snapshots after", "Disks", "Creation time"},
		Noun: "snapshot schedules", InfoOnSelect: true,
		Note: "A snapshot schedule takes its disks' snapshots in UTC, starting on the hour; a disk takes up to 10 schedules of its region."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Snapshot schedules are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/resourcePolicies", nil, nil)
	if err != nil {
		return listing, err
	}
	dm, err := p.call("GET", project, "aggregated/disks", nil, nil)
	if err != nil {
		return listing, err
	}
	count := map[string]int{}
	for _, d := range aggregatedItems(dm, "disks") {
		for _, rp := range vpcStrs(d, "resourcePolicies") {
			count[rp]++
		}
	}
	for _, pol := range aggregatedItems(m, "resourcePolicies") {
		if vmMap(pol, "snapshotSchedulePolicy") == nil {
			continue
		}
		path := []string{vpcLast(vpcStr(pol, "region")), vpcStr(pol, "name")}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(pol, "name"), Opens: path, Target: path,
			Actions: p.scheduleActions(pol, nil, nil, false), Info: computePermissionsTab(),
			Fields: map[string]string{"Region": path[0], "Frequency": scheduleText(pol), "Auto-delete snapshots after": retentionText(pol),
				"Disks": fmt.Sprint(count[vpcStr(pol, "selfLink")]), "Creation time": vpcStr(pol, "creationTimestamp")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeSnapshotSchedulesProvider) CreateOnPage() bool   { return true }
func (computeSnapshotSchedulesProvider) CreateTitle() string  { return "Create a snapshot schedule" }
func (computeSnapshotSchedulesProvider) CreateLayout() string { return console.CreateLayoutPage }

var scheduleDays = []string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

func hourOptions() []string {
	out := make([]string, 24)
	for i := range out {
		out[i] = fmt.Sprintf("%02d:00", i)
	}
	return out
}

func (computeSnapshotSchedulesProvider) CreateForm() (string, []console.Field) {
	return "Create snapshot schedule", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions(),
			Help: "A schedule applies to the disks of its region."},
		{Name: "storageLocation", Label: "Snapshot location", Type: "select", Default: "", Options: []string{"", "us", "eu", "asia"},
			OptionLabels: map[string]string{"": "Regional (the disk's region)", "us": "Multi-regional: us", "eu": "Multi-regional: eu", "asia": "Multi-regional: asia"}},
		{Name: "frequency", Label: "Schedule frequency", Type: "radio", Section: "Schedule", Default: "Daily", Options: []string{"Hourly", "Daily", "Weekly"}},
		{Name: "hours", Label: "Every (hours)", Type: "text", Section: "Schedule", Default: "4", Pattern: `^([1-9]|1[0-9]|2[0-3])$`,
			VisibleWhen: map[string]string{"frequency": "Hourly"}, Help: "1 to 23. A number that divides 24 takes snapshots at the same times each day."},
		{Name: "day", Label: "Day", Type: "select", Section: "Schedule", Default: "MONDAY", Options: scheduleDays,
			VisibleWhen: map[string]string{"frequency": "Weekly"}},
		{Name: "startTime", Label: "Start time (UTC)", Type: "select", Section: "Schedule", Default: "04:00", Options: hourOptions(),
			Help: "A snapshot starts within the hour of the start time."},
		{Name: "retentionDays", Label: "Auto-delete snapshots after (days)", Type: "text", Section: "Schedule", Default: "14", Pattern: `^[0-9]{0,5}$`,
			Help: "1 or more; empty keeps the snapshots."},
		{Name: "onSourceDiskDelete", Label: "Deletion rule", Type: "select", Section: "Schedule", Default: "KEEP_AUTO_SNAPSHOTS",
			Options:      []string{"KEEP_AUTO_SNAPSHOTS", "APPLY_RETENTION_POLICY"},
			OptionLabels: map[string]string{"KEEP_AUTO_SNAPSHOTS": "Keep snapshots", "APPLY_RETENTION_POLICY": "Delete snapshots older than the auto-delete period"},
			Help:         "What becomes of a disk's scheduled snapshots when the disk is deleted."},
		vmLabelsField(nil, "Schedule"),
	}
}

func (p computeSnapshotSchedulesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := strings.TrimSpace(values["region"])
	if !compute.IsRegion(region) {
		return "", fmt.Errorf("region: %q is not a region", region)
	}
	b, err := scheduleBody(values)
	if err != nil {
		return "", err
	}
	b["name"] = name
	sp := b["snapshotSchedulePolicy"].(map[string]any)
	props := map[string]any{}
	if l := values["storageLocation"]; l != "" {
		props["storageLocations"] = []any{l}
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		props["labels"] = l
	}
	if len(props) > 0 {
		sp["snapshotProperties"] = props
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "regions/"+region+"/resourcePolicies", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

// disksOf are the disks with the schedule pol, and every disk.
func (p computeSnapshotSchedulesProvider) disksOf(project, pol string) ([]map[string]any, []map[string]any, error) {
	return computeConsistencyGroupsProvider(p).members(project, pol)
}

func (p computeSnapshotSchedulesProvider) scheduleActions(pol map[string]any, in, all []map[string]any, onPage bool) []console.Action {
	region := vpcLast(vpcStr(pol, "region"))
	var acts []console.Action
	if onPage {
		var attach, detach []string
		labels := map[string]string{}
		member := map[string]bool{}
		for _, d := range in {
			member[vpcStr(d, "selfLink")] = true
			detach = append(detach, diskBase(diskPathOf(vpcStr(d, "selfLink"))))
		}
		for _, d := range all {
			loc := vpcLast(vpcStr(d, "region"))
			if loc == "" {
				loc = regionOfZone(vpcLast(vpcStr(d, "zone")))
			}
			b := diskBase(diskPathOf(vpcStr(d, "selfLink")))
			labels[b] = vpcStr(d, "name") + " (" + diskLoc(d) + ")"
			if loc == region && !member[vpcStr(d, "selfLink")] {
				attach = append(attach, b)
			}
		}
		at := console.Action{ID: "attach", Label: "Attach to disk", Submit: "Attach", Fields: []console.Field{
			{Name: "disk", Label: "Disk", Type: "select", Required: true, Options: attach, OptionLabels: labels,
				Help: "A disk of " + region + "."}}}
		if len(attach) == 0 {
			at.Unavailable = "No other disk of " + region + " exists."
		}
		dt := console.Action{ID: "detach", Label: "Detach from disk", Submit: "Detach", Fields: []console.Field{
			{Name: "disk", Label: "Disk", Type: "select", Required: true, Options: detach, OptionLabels: labels}}}
		if len(detach) == 0 {
			dt.Unavailable = "The schedule is attached to no disk."
		}
		acts = append(acts, at, dt)
	}
	_, form := computeSnapshotSchedulesProvider{}.CreateForm()
	cur := scheduleValues(pol)
	var edit []console.Field
	for _, f := range form {
		switch f.Name {
		case "description", "frequency", "hours", "day", "startTime", "retentionDays", "onSourceDiskDelete":
			if v, ok := cur[f.Name]; ok {
				f.Default = v
			}
			edit = append(edit, f)
		}
	}
	acts = append(acts, console.Action{ID: "edit", Label: "Edit", Submit: "Save", Fields: edit})
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, Confirm: "Delete snapshot schedule " + vpcStr(pol, "name") + "?"}
	if onPage && len(in) > 0 {
		del.Unavailable = "The schedule is attached to " + vpcStr(in[0], "name") + ": detach it first."
	}
	return append(acts, del)
}

func (p computeSnapshotSchedulesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
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
	in, _, err := p.disksOf(project, vpcStr(pol, "selfLink"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	disks := console.Listing{Columns: []string{"Location", "Size"}, Noun: "disks"}
	for _, d := range in {
		disks.Items = append(disks.Items, console.Resource{Name: vpcStr(d, "name"),
			Fields: map[string]string{"Location": diskLoc(d), "Size": vpcStr(d, "sizeGb") + " GB"}})
	}
	disks.Total = len(disks.Items)
	rp := vmMap(vmMap(pol, "snapshotSchedulePolicy"), "retentionPolicy")
	return console.Detail{Title: path[1], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Region", Value: path[0]}, {Label: "Frequency", Value: scheduleText(pol)},
			{Label: "Auto-delete snapshots after", Value: retentionText(pol)},
			{Label: "Deletion rule", Value: vpcDash(vpcStr(rp, "onSourceDiskDelete"))},
			{Label: "Description", Value: vpcDash(vpcStr(pol, "description"))}, {Label: "Creation time", Value: vpcStr(pol, "creationTimestamp")}}}}},
		{ID: "disks", Label: "Disks", Listing: disks},
	}}, nil
}

func (p computeSnapshotSchedulesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	pol, err := p.call("GET", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	if err != nil {
		return nil
	}
	in, all, err := p.disksOf(project, vpcStr(pol, "selfLink"))
	if err != nil {
		return nil
	}
	return p.scheduleActions(pol, in, all, true)
}

func (p computeSnapshotSchedulesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "regions/" + path[0] + "/resourcePolicies/" + path[1]
	link := "projects/" + project + "/" + base
	var err error
	switch action {
	case "attach", "detach":
		disk := strings.TrimSpace(values["disk"])
		if !strings.Contains(disk, "/disks/") {
			return errors.New("disk: choose a disk")
		}
		verb := "/addResourcePolicies"
		if action == "detach" {
			verb = "/removeResourcePolicies"
		}
		_, err = p.call("POST", project, disk+verb, nil, map[string]any{"resourcePolicies": []any{link}})
	case "edit":
		// resourcePolicies.patch: the description, schedule and retention
		// policy, as Google's Edit snapshot schedule.
		b, err2 := scheduleBody(values)
		if err2 != nil {
			return err2
		}
		b["description"] = strings.TrimSpace(values["description"])
		_, err = p.call("PATCH", project, base, url.Values{"updateMask": {"description,snapshotSchedulePolicy.schedule,snapshotSchedulePolicy.retentionPolicy"}}, b)
	case "delete":
		_, err = p.call("DELETE", project, base, nil, nil)
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
	}
	return err
}

// scheduleBody is a resource policy with the form's schedule and
// retention policy.
func scheduleBody(values map[string]string) (map[string]any, error) {
	start := vpcOr(values["startTime"], "04:00")
	var sched map[string]any
	switch values["frequency"] {
	case "Hourly":
		sched = map[string]any{"hourlySchedule": map[string]any{"hoursInCycle": strings.TrimSpace(values["hours"]), "startTime": start}}
	case "Weekly":
		sched = map[string]any{"weeklySchedule": map[string]any{"dayOfWeeks": []any{map[string]any{"day": vpcOr(values["day"], "MONDAY"), "startTime": start}}}}
	case "Daily", "":
		sched = map[string]any{"dailySchedule": map[string]any{"daysInCycle": 1, "startTime": start}}
	default:
		return nil, fmt.Errorf("frequency: %q is not Hourly, Daily or Weekly", values["frequency"])
	}
	rp := map[string]any{"onSourceDiskDelete": vpcOr(values["onSourceDiskDelete"], "KEEP_AUTO_SNAPSHOTS")}
	if d := strings.TrimSpace(values["retentionDays"]); d != "" {
		rp["maxRetentionDays"] = d
	}
	return map[string]any{"snapshotSchedulePolicy": map[string]any{"schedule": sched, "retentionPolicy": rp}}, nil
}

// scheduleValues are a schedule's form values, for Edit's defaults.
func scheduleValues(pol map[string]any) map[string]string {
	sp := vmMap(pol, "snapshotSchedulePolicy")
	sc := vmMap(sp, "schedule")
	rp := vmMap(sp, "retentionPolicy")
	out := map[string]string{"description": vpcStr(pol, "description"), "onSourceDiskDelete": vpcOr(vpcStr(rp, "onSourceDiskDelete"), "KEEP_AUTO_SNAPSHOTS"), "retentionDays": ""}
	if v := rp["maxRetentionDays"]; v != nil {
		out["retentionDays"] = fmt.Sprint(v)
	}
	switch {
	case vmMap(sc, "hourlySchedule") != nil:
		h := vmMap(sc, "hourlySchedule")
		out["frequency"], out["hours"], out["startTime"] = "Hourly", fmt.Sprint(h["hoursInCycle"]), vpcStr(h, "startTime")
	case vmMap(sc, "weeklySchedule") != nil:
		out["frequency"] = "Weekly"
		if l, _ := vmMap(sc, "weeklySchedule")["dayOfWeeks"].([]any); len(l) > 0 {
			if m, ok := l[0].(map[string]any); ok {
				out["day"], out["startTime"] = vpcStr(m, "day"), vpcStr(m, "startTime")
			}
		}
	case vmMap(sc, "dailySchedule") != nil:
		out["frequency"], out["startTime"] = "Daily", vpcStr(vmMap(sc, "dailySchedule"), "startTime")
	}
	return out
}
