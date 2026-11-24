package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The Compute Engine parity round of 2026-11-22 (#2139): the VM instances
// page's Instance schedules tab, Instance templates, Instance groups,
// Machine images, the Snapshots page's Snapshot schedules tab, custom
// images and the Metadata page's SSH keys tab, each over the Compute
// Engine API in process.

// computeRegionOptions are the regions a form offers.
func computeRegionOptions() []string { return compute.Regions() }

// regionOfZoneName is a zone's region.
func regionOfZoneName(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// diskNames are the project's disks as ZONE/NAME.
func (c computeScreen) diskNames(project string) []string {
	m, err := c.call("GET", project, "aggregated/disks", nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range aggregatedItems(m, "disks") {
		out = append(out, diskLoc(d)+"/"+vpcStr(d, "name"))
	}
	sort.Strings(out)
	return out
}

// snapshotNames are the project's snapshots.
func (c computeScreen) snapshotNames(project string) []string {
	l, err := c.list(project, "global/snapshots")
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range l {
		out = append(out, vpcStr(s, "name"))
	}
	sort.Strings(out)
	return out
}

// customImageNames are the project's own images.
func (c computeScreen) customImageNames(project string) []string {
	l, err := c.list(project, "global/images")
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range l {
		out = append(out, vpcStr(s, "name"))
	}
	sort.Strings(out)
	return out
}

// selectOrText is a select of options, or a text field when there are none.
func selectOrText(f console.Field, options []string, none string) console.Field {
	f.Options = options
	f.Type = "select"
	if len(options) == 0 {
		f.Type, f.Help = "text", none
	}
	return f
}

func splitZoneName(s, what string) (string, string, error) {
	zone, name, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || zone == "" || name == "" {
		return "", "", fmt.Errorf("%s: %q is not ZONE/NAME", what, s)
	}
	return zone, name, nil
}

// --- Instance schedules -------------------------------------------------------

// computeInstanceSchedulesProvider is the VM instances page's Instance
// schedules tab (compute/docs/instances/schedule-instance-start-stop, read
// 2026-11-22): Create schedule (Name, Region, Description, Start time,
// Stop time, Time zone, Frequency), and each schedule's Add instances to
// schedule, Remove instances from schedule and Delete.
type computeInstanceSchedulesProvider struct{ computeScreen }

func (computeInstanceSchedulesProvider) ID() string    { return "compute-instance-schedules" }
func (computeInstanceSchedulesProvider) Title() string { return "Instance schedules" }

var scheduleFrequencies = []string{"Daily", "Weekdays", "Weekends", "Weekly on Monday", "Monthly on the 1st"}

// scheduleCron is the unix-cron of a time of day at a frequency.
func scheduleCron(hhmm, freq string) (string, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(hhmm), ":")
	hi, err1 := strconv.Atoi(h)
	mi, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hi < 0 || hi > 23 || mi < 0 || mi > 59 {
		return "", fmt.Errorf("%q is not a time of day as HH:MM", hhmm)
	}
	dow, dom := "*", "*"
	switch freq {
	case "Weekdays":
		dow = "1-5"
	case "Weekends":
		dow = "0,6"
	case "Weekly on Monday":
		dow = "1"
	case "Monthly on the 1st":
		dom = "1"
	}
	return fmt.Sprintf("%d %d %s * %s", mi, hi, dom, dow), nil
}

func (c computeScreen) policies(project, kind string) ([]map[string]any, error) {
	m, err := c.call("GET", project, "aggregated/resourcePolicies", nil, nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, o := range aggregatedItems(m, "resourcePolicies") {
		if o[kind] != nil {
			out = append(out, o)
		}
	}
	return out, nil
}

// policyUsers are the instances or disks a policy is attached to, by name.
func (c computeScreen) policyUsers(project, coll, policy string) []string {
	m, err := c.call("GET", project, "aggregated/"+coll, nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range aggregatedItems(m, coll) {
		for _, r := range vpcStrs(o, "resourcePolicies") {
			if strings.HasSuffix(r, "/resourcePolicies/"+policy) {
				out = append(out, vpcStr(o, "name"))
			}
		}
	}
	sort.Strings(out)
	return out
}

func (p computeInstanceSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Start", "Stop", "Time zone", "In use by"}, Noun: "instance schedules", Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Instance schedules are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.policies(project, "instanceSchedulePolicy")
	if err != nil {
		return listing, err
	}
	for _, o := range l {
		is := vmMap(o, "instanceSchedulePolicy")
		region := vpcLast(vpcStr(o, "region"))
		users := p.policyUsers(project, "instances", vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Status: vpcTitle(vpcStr(o, "status")),
			Target: []string{region, vpcStr(o, "name")}, Actions: p.scheduleActions(project, o, users),
			Fields: map[string]string{"Region": region, "Start": vpcDash(vpcStr(vmMap(is, "vmStartSchedule"), "schedule")),
				"Stop": vpcDash(vpcStr(vmMap(is, "vmStopSchedule"), "schedule")), "Time zone": vpcStr(is, "timeZone"),
				"In use by": vpcDash(strings.Join(users, ", "))}})
	}
	listing.Total = len(listing.Items)
	listing.AlwaysStatus = true
	return listing, nil
}

func (p computeInstanceSchedulesProvider) scheduleActions(project string, o map[string]any, users []string) []console.Action {
	region := vpcLast(vpcStr(o, "region"))
	var free []string
	if vms, err := (computeInstancesProvider{p.computeScreen}).instances(project); err == nil {
		for _, vm := range vms {
			zone := zoneOf(vpcStr(vm, "zone"))
			if regionOfZoneName(zone) == region && len(vpcStrs(vm, "resourcePolicies")) == 0 {
				free = append(free, zone+"/"+vpcStr(vm, "name"))
			}
		}
	}
	add := console.Action{ID: "addinstances", Label: "Add instances to schedule", Submit: "Add", Fields: []console.Field{
		{Name: "instances", Label: "VM instances", Type: "text", Required: true,
			Help: "Comma-separated, as ZONE/NAME: VMs of " + region + " without a schedule (" + vpcDash(strings.Join(free, ", ")) + ")."}}}
	if len(free) == 0 {
		add.Fields, add.Unavailable = nil, "No VM of "+region+" is free to schedule: a VM takes one instance schedule."
	}
	acts := []console.Action{add}
	if len(users) > 0 {
		acts = append(acts, console.Action{ID: "removeinstances", Label: "Remove instances from schedule", Submit: "Remove", Fields: []console.Field{
			{Name: "instances", Label: "VM instances", Type: "text", Required: true, Default: "", Help: "Comma-separated names: " + strings.Join(users, ", ") + "."}}})
	}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete instance schedule " + vpcStr(o, "name") + "?"}
	if len(users) > 0 {
		del.Unavailable = "Remove its VM instances from the schedule first: " + strings.Join(users, ", ") + "."
	}
	return append(acts, del)
}

func (computeInstanceSchedulesProvider) CreateTitle() string { return "Create schedule" }

// CreateSubmit is Google's: the Create a schedule pane ends in Submit.
func (computeInstanceSchedulesProvider) CreateSubmit() string { return "Submit" }

func (p computeInstanceSchedulesProvider) CreateForm() (string, []console.Field) {
	return "Create schedule", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: computeRegionOptions()},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "startTime", Label: "Start time", Type: "text", Pattern: `^$|^[0-9]{1,2}:[0-9]{2}$`, Help: "Optional, HH:MM: when the VMs start."},
		{Name: "stopTime", Label: "Stop time", Type: "text", Pattern: `^$|^[0-9]{1,2}:[0-9]{2}$`, Help: "Optional, HH:MM: when the VMs stop."},
		{Name: "timeZone", Label: "Time zone", Type: "text", Required: true, Default: "UTC", Help: "An IANA time zone, such as America/New_York."},
		{Name: "frequency", Label: "Frequency", Type: "select", Default: "Daily", Options: scheduleFrequencies},
	}
}

func (p computeInstanceSchedulesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	pol := map[string]any{"timeZone": strings.TrimSpace(vpcOr(values["timeZone"], "UTC"))}
	for field, key := range map[string]string{"startTime": "vmStartSchedule", "stopTime": "vmStopSchedule"} {
		if t := strings.TrimSpace(values[field]); t != "" {
			cr, err := scheduleCron(t, values["frequency"])
			if err != nil {
				return "", fmt.Errorf("%s: %w", map[string]string{"startTime": "start time", "stopTime": "stop time"}[field], err)
			}
			pol[key] = map[string]any{"schedule": cr}
		}
	}
	b := map[string]any{"name": name, "instanceSchedulePolicy": pol}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "regions/"+vpcOr(values["region"], "us-central1")+"/resourcePolicies", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeInstanceSchedulesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 2 || p.api() == nil {
		return nil
	}
	o, err := p.call("GET", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	if err != nil {
		return nil
	}
	return p.scheduleActions(project, o, p.policyUsers(project, "instances", path[1]))
}

func (p computeInstanceSchedulesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	pol := "regions/" + path[0] + "/resourcePolicies/" + path[1]
	switch action {
	case "delete":
		_, err := p.call("DELETE", project, pol, nil, nil)
		return err
	case "addinstances", "removeinstances":
		vms, err := (computeInstancesProvider{p.computeScreen}).instances(project)
		if err != nil {
			return err
		}
		verb := map[string]string{"addinstances": "addResourcePolicies", "removeinstances": "removeResourcePolicies"}[action]
		for _, want := range vpcList(values["instances"]) {
			w := want.(string)
			found := false
			for _, vm := range vms {
				zone := zoneOf(vpcStr(vm, "zone"))
				if w == vpcStr(vm, "name") || w == zone+"/"+vpcStr(vm, "name") {
					found = true
					if _, err := p.call("POST", project, "zones/"+zone+"/instances/"+vpcStr(vm, "name")+"/"+verb, nil,
						map[string]any{"resourcePolicies": []any{pol}}); err != nil {
						return err
					}
				}
			}
			if !found {
				return fmt.Errorf("no VM instance %q", w)
			}
		}
		return nil
	}
	return fmt.Errorf("%s is not available here", action)
}

// --- Snapshot schedules -------------------------------------------------------

// computeSnapshotSchedulesProvider is the Snapshots page's Snapshot
// schedules tab (compute/docs/disks/scheduled-snapshots, read 2026-11-22):
// Create snapshot schedule (Name, Description, Region, Snapshot location,
// Schedule frequency, Start time, Autodelete snapshots after, Deletion
// rule) and Delete, refused while a disk uses the schedule.
type computeSnapshotSchedulesProvider struct{ computeScreen }

func (computeSnapshotSchedulesProvider) ID() string    { return "compute-snapshot-schedules" }
func (computeSnapshotSchedulesProvider) Title() string { return "Snapshot schedules" }

func hourOptions() []string {
	var out []string
	for h := 0; h < 24; h++ {
		out = append(out, fmt.Sprintf("%02d:00", h))
	}
	return out
}

func (p computeSnapshotSchedulesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Frequency", "Start time (UTC)", "Autodelete after", "In use by"}, Noun: "snapshot schedules",
		Note: "Each schedule's snapshots are taken by the instance at their start time (UTC), named DISK-ZONE-TIMESTAMP, and deleted when they are older than its retention."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Snapshot schedules are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.policies(project, "snapshotSchedulePolicy")
	if err != nil {
		return listing, err
	}
	for _, o := range l {
		ss := vmMap(o, "snapshotSchedulePolicy")
		sch := vmMap(ss, "schedule")
		freq, start := "", ""
		switch {
		case sch["hourlySchedule"] != nil:
			h := vmMap(sch, "hourlySchedule")
			freq, start = "Every "+vpcStr(h, "hoursInCycle")+" hours", vpcStr(h, "startTime")
			if hc, ok := h["hoursInCycle"].(float64); ok {
				freq = "Every " + strconv.Itoa(int(hc)) + " hours"
			}
		case sch["dailySchedule"] != nil:
			freq, start = "Daily", vpcStr(vmMap(sch, "dailySchedule"), "startTime")
		case sch["weeklySchedule"] != nil:
			var days []string
			for _, d := range vmMaps(vmMap(sch, "weeklySchedule"), "dayOfWeeks") {
				days = append(days, vpcTitle(vpcStr(d, "day")))
				start = vpcStr(d, "startTime")
			}
			freq = "Weekly on " + strings.Join(days, ", ")
		}
		ret := ""
		if d, ok := vmMap(ss, "retentionPolicy")["maxRetentionDays"].(float64); ok {
			ret = strconv.Itoa(int(d)) + " days"
		}
		region := vpcLast(vpcStr(o, "region"))
		users := p.policyUsers(project, "disks", vpcStr(o, "name"))
		del := console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete snapshot schedule " + vpcStr(o, "name") + "?"}
		if len(users) > 0 {
			del.Unavailable = "Detach the schedule from its disks first: " + strings.Join(users, ", ") + "."
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Status: vpcTitle(vpcStr(o, "status")),
			Target: []string{region, vpcStr(o, "name")}, Actions: []console.Action{del},
			Fields: map[string]string{"Region": region, "Frequency": freq, "Start time (UTC)": start, "Autodelete after": vpcDash(ret),
				"In use by": vpcDash(strings.Join(users, ", "))}})
	}
	listing.Total = len(listing.Items)
	listing.AlwaysStatus = true
	return listing, nil
}

func (computeSnapshotSchedulesProvider) CreateTitle() string { return "Create a snapshot schedule" }

func (p computeSnapshotSchedulesProvider) CreateForm() (string, []console.Field) {
	weekly := map[string]string{"frequency": "Weekly"}
	return "Create snapshot schedule", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: computeRegionOptions()},
		{Name: "location", Label: "Snapshot location", Type: "radio", Default: "Multi-regional", Options: []string{"Multi-regional", "Regional"}},
		{Name: "frequency", Label: "Schedule frequency", Type: "select", Default: "Daily", Options: []string{"Hourly", "Daily", "Weekly"}},
		{Name: "hours", Label: "Hours in cycle", Type: "text", Default: "1", Pattern: `^([1-9]|1[0-9]|2[0-3])$`, VisibleWhen: map[string]string{"frequency": "Hourly"}},
		{Name: "day", Label: "Day of week", Type: "select", Default: "Monday", VisibleWhen: weekly,
			Options: []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}},
		{Name: "startTime", Label: "Start time (UTC)", Type: "select", Default: "00:00", Options: hourOptions()},
		{Name: "retention", Label: "Autodelete snapshots after", Type: "text", Required: true, Default: "14", Pattern: `^[0-9]{1,4}$`, Help: "Days."},
		{Name: "deletionRule", Label: "Deletion rule", Type: "radio", Default: "Keep snapshots", Options: []string{"Keep snapshots", "Delete snapshots older than the retention"},
			Help: "What happens to the schedule's snapshots when their source disk is deleted."},
		vmLabelsField(nil, ""),
	}
}

func (p computeSnapshotSchedulesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	start := vpcOr(values["startTime"], "00:00")
	var sch map[string]any
	switch values["frequency"] {
	case "Hourly":
		h, err := strconv.Atoi(strings.TrimSpace(vpcOr(values["hours"], "1")))
		if err != nil {
			return "", fmt.Errorf("hours in cycle: %q is not a number", values["hours"])
		}
		sch = map[string]any{"hourlySchedule": map[string]any{"hoursInCycle": float64(h), "startTime": start}}
	case "Weekly":
		sch = map[string]any{"weeklySchedule": map[string]any{"dayOfWeeks": []any{map[string]any{"day": strings.ToUpper(vpcOr(values["day"], "Monday")), "startTime": start}}}}
	default:
		sch = map[string]any{"dailySchedule": map[string]any{"daysInCycle": 1.0, "startTime": start}}
	}
	days, err := strconv.Atoi(strings.TrimSpace(vpcOr(values["retention"], "14")))
	if err != nil {
		return "", fmt.Errorf("autodelete snapshots after: %q is not a number of days", values["retention"])
	}
	onDel := "KEEP_AUTO_SNAPSHOTS"
	if strings.HasPrefix(values["deletionRule"], "Delete") {
		onDel = "APPLY_RETENTION_POLICY"
	}
	region := vpcOr(values["region"], "us-central1")
	props := map[string]any{"storageLocations": []any{map[string]string{"Regional": region}[values["location"]]}}
	if values["location"] != "Regional" {
		props["storageLocations"] = []any{strings.SplitN(region, "-", 2)[0]}
	}
	if labels, err := console.ParseMap(values["labels"]); err != nil {
		return "", err
	} else if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		props["labels"] = l
	}
	b := map[string]any{"name": name, "snapshotSchedulePolicy": map[string]any{"schedule": sch,
		"retentionPolicy": map[string]any{"maxRetentionDays": float64(days), "onSourceDiskDelete": onDel}, "snapshotProperties": props}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "regions/"+region+"/resourcePolicies", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeSnapshotSchedulesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 2 {
		return nil
	}
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true}}
}

func (p computeSnapshotSchedulesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	return err
}

// --- Instance templates -------------------------------------------------------

// computeTemplatesProvider is Instance templates
// (compute/docs/instance-templates/create-instance-templates,
// get-list-delete-instance-templates, read 2026-11-22): the list, Create
// instance template on its own page (Name, Location Global or Regional,
// then the machine configuration, boot disk, networking and management of
// Create an instance), the template's page, Create VM, Create similar and
// Delete.
type computeTemplatesProvider struct{ computeScreen }

func (computeTemplatesProvider) ID() string    { return "compute-instance-templates" }
func (computeTemplatesProvider) Title() string { return "Instance templates" }

func (p computeTemplatesProvider) templates(project string) ([]map[string]any, error) {
	m, err := p.call("GET", project, "aggregated/instanceTemplates", nil, nil)
	if err != nil {
		return nil, err
	}
	return aggregatedItems(m, "instanceTemplates"), nil
}

// templateTarget is a template's path: global/NAME or REGION/NAME.
func templateTarget(o map[string]any) []string {
	loc := "global"
	if r := vpcStr(o, "region"); r != "" {
		loc = vpcLast(r)
	}
	return []string{loc, vpcStr(o, "name")}
}

func templateBase(path []string) string {
	if path[0] == "global" {
		return "global/instanceTemplates/" + path[1]
	}
	return "regions/" + path[0] + "/instanceTemplates/" + path[1]
}

func templateImage(props map[string]any) string {
	for _, d := range vmMaps(props, "disks") {
		if d["boot"] == true {
			return vpcLast(vpcStr(vmMap(d, "initializeParams"), "sourceImage"))
		}
	}
	return ""
}

func (p computeTemplatesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Machine type", "Image", "Location", "In use by", "Creation time"}, Noun: "instance templates", Note: vmNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Instance templates are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.templates(project)
	if err != nil {
		return listing, err
	}
	users := p.templateUsers(project)
	for _, o := range l {
		t := templateTarget(o)
		props := vmMap(o, "properties")
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Opens: t, Target: t, Actions: p.templateActions(o, false),
			Fields: map[string]string{"Machine type": vpcLast(vpcStr(props, "machineType")), "Image": vpcDash(templateImage(props)),
				"Location": vpcTitle(t[0]), "In use by": vpcDash(strings.Join(users[vpcStr(o, "selfLink")], ", ")),
				"Creation time": vpcStr(o, "creationTimestamp")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// templateUsers are the managed instance groups of each template.
func (p computeTemplatesProvider) templateUsers(project string) map[string][]string {
	out := map[string][]string{}
	m, err := p.call("GET", project, "aggregated/instanceGroupManagers", nil, nil)
	if err != nil {
		return out
	}
	for _, g := range aggregatedItems(m, "instanceGroupManagers") {
		t := vpcStr(g, "instanceTemplate")
		out[t] = append(out[t], vpcStr(g, "name"))
	}
	return out
}

func (p computeTemplatesProvider) templateActions(o map[string]any, onPage bool) []console.Action {
	zones := vmZoneField("zone")
	if r := vpcStr(o, "region"); r != "" {
		zones.Options = compute.Zones(vpcLast(r))
		zones.Default = zones.Options[0]
	}
	return []console.Action{
		{ID: "createvm", Label: "Create VM", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp}, zones}},
		{ID: "similar", Label: "Create similar", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Default: vpcStr(o, "name") + "-1", Help: computeNameHelp},
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description")}}},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "Delete instance template " + vpcStr(o, "name") + "?"},
	}
}

func (computeTemplatesProvider) CreateOnPage() bool   { return true }
func (computeTemplatesProvider) CreateTitle() string  { return "Create an instance template" }
func (computeTemplatesProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p computeTemplatesProvider) CreateForm() (string, []console.Field) {
	return "Create instance template", p.templateFields(nil)
}

func (p computeTemplatesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return p.templateFields(nil)
	}
	return p.templateFields(p.networkNames(ctx, project))
}

// templateFields are Create an instance's, with Location in place of the
// zone and without what a template does not hold (Data protection).
func (p computeTemplatesProvider) templateFields(networks []string) []console.Field {
	vm := computeInstancesProvider{p.computeScreen}.createFields(nil, networks)
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "location", Label: "Location", Type: "radio", Default: "Global", Options: []string{"Global", "Regional"},
			Help: "A regional template makes VMs of its region only."},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionOptions(), VisibleWhen: map[string]string{"location": "Regional"}},
	}
	for _, f := range vm {
		switch f.Name {
		case "name", "region", "zone", "snapshotSchedule", "internalIP", "deletionProtection":
			continue
		}
		fields = append(fields, f)
	}
	return fields
}

func (p computeTemplatesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := vpcOr(values["region"], "us-central1")
	zone := compute.Zones(region)[0]
	// The VM's own body, made by Create an instance, then kept as the
	// template's properties.
	vmValues := map[string]string{}
	for k, v := range values {
		vmValues[k] = v
	}
	vmValues["zone"], vmValues["name"] = zone, name
	body, err := (computeInstancesProvider{p.computeScreen}).createBody(project, vmValues)
	if err != nil {
		return "", err
	}
	props := map[string]any{"machineType": vpcLast(vpcStr(body, "machineType"))}
	for _, k := range []string{"disks", "networkInterfaces", "metadata", "tags", "labels", "scheduling", "serviceAccounts"} {
		if v, ok := body[k]; ok {
			props[k] = v
		}
	}
	for _, d := range vmMaps(props, "disks") {
		ip := vmMap(d, "initializeParams")
		ip["diskType"] = vpcLast(vpcStr(ip, "diskType"))
		delete(d, "deviceName")
	}
	for _, n := range vmMaps(props, "networkInterfaces") {
		if values["location"] != "Regional" {
			delete(n, "subnetwork")
		}
	}
	b := map[string]any{"name": name, "properties": props}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	coll := "global/instanceTemplates"
	if values["location"] == "Regional" {
		coll = "regions/" + region + "/instanceTemplates"
	}
	if _, err := p.call("POST", project, coll, nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeTemplatesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.DeeperThan(2, path), nil
	}
	o, err := p.call("GET", project, templateBase(path), nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := vmMap(o, "properties")
	var nets, tags []string
	for _, n := range vmMaps(props, "networkInterfaces") {
		nets = append(nets, vpcLast(vpcStr(n, "network")))
	}
	for _, t := range vpcStrs(vmMap(props, "tags"), "items") {
		tags = append(tags, t)
	}
	var md []string
	for _, m := range vmMetadata(map[string]any{"metadata": vmMap(props, "metadata")}) {
		md = append(md, m["key"])
	}
	var bootType, bootSize string
	for _, d := range vmMaps(props, "disks") {
		if d["boot"] == true {
			ip := vmMap(d, "initializeParams")
			bootType, bootSize = vpcLast(vpcStr(ip, "diskType")), vpcStr(ip, "diskSizeGb")
		}
	}
	return console.Detail{Title: path[1], Heading: "Instance template details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Machine configuration", Properties: []console.Property{
				{Label: "Machine type", Value: vpcLast(vpcStr(props, "machineType"))},
				{Label: "VM provisioning model", Value: provisioningModelText(props)},
				{Label: "Location", Value: vpcTitle(path[0])},
				{Label: "Description", Value: vpcDash(vpcStr(o, "description"))}}},
			{Heading: "Boot disk", Properties: []console.Property{
				{Label: "Image", Value: vpcDash(templateImage(props))}, {Label: "Type", Value: vpcDash(bootType)},
				{Label: "Size", Value: vpcDash(bootSize) + " GB"}}},
			{Heading: "Networking", Properties: []console.Property{
				{Label: "Network", Value: vpcDash(strings.Join(nets, ", "))}, {Label: "Network tags", Value: vpcDash(strings.Join(tags, ", "))}}},
			{Heading: "Management", Properties: []console.Property{
				{Label: "Metadata", Value: vpcDash(strings.Join(md, ", "))}, {Label: "Labels", Value: vpcDash(vmLabelsText(props))},
				{Label: "Creation time", Value: vpcStr(o, "creationTimestamp")}}},
		}}}}, nil
}

func (p computeTemplatesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	o, err := p.call("GET", project, templateBase(path), nil, nil)
	if err != nil {
		return nil
	}
	return p.templateActions(o, true)
}

func (p computeTemplatesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := templateBase(path)
	switch action {
	case "delete":
		_, err := p.call("DELETE", project, base, nil, nil)
		return err
	case "createvm":
		zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
		_, err := p.call("POST", project, "zones/"+zone+"/instances", url.Values{"sourceInstanceTemplate": {base}},
			map[string]any{"name": strings.TrimSpace(values["name"])})
		return err
	case "similar":
		o, err := p.call("GET", project, base, nil, nil)
		if err != nil {
			return err
		}
		b := map[string]any{"name": strings.TrimSpace(values["name"]), "properties": o["properties"]}
		if d := strings.TrimSpace(values["description"]); d != "" {
			b["description"] = d
		}
		coll := strings.TrimSuffix(base, "/"+path[1])
		_, err = p.call("POST", project, coll, nil, b)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// --- Machine images -----------------------------------------------------------

// computeMachineImagesProvider is Machine images
// (compute/docs/machine-images/create-machine-images, read 2026-11-22):
// the list, Create machine image (Name, Description, Source VM instance,
// Location), Create instance from one, and Delete.
type computeMachineImagesProvider struct{ computeScreen }

func (computeMachineImagesProvider) ID() string    { return "compute-machine-images" }
func (computeMachineImagesProvider) Title() string { return "Machine images" }

func (p computeMachineImagesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Source instance", "Location", "Creation time"}, Noun: "machine images", AlwaysStatus: true,
		Note: "A machine image holds its source VM's properties and a copy of each of its disks."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Machine images are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.list(project, "global/machineImages")
	if err != nil {
		return listing, err
	}
	for _, o := range l {
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Status: vpcStr(o, "status"), Actions: machineImageActions(o),
			Fields: map[string]string{"Source instance": vpcLast(vpcStr(o, "sourceInstance")), "Location": strings.Join(vpcStrs(o, "storageLocations"), ", "),
				"Creation time": vpcStr(o, "creationTimestamp")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func machineImageActions(o map[string]any) []console.Action {
	return []console.Action{labelsAction(o), {ID: "createvm", Label: "Create instance", Submit: "Create", Fields: []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp}, vmZoneField("zone")}}}
}

func (computeMachineImagesProvider) CreateTitle() string { return "Create a machine image" }

func (p computeMachineImagesProvider) CreateForm() (string, []console.Field) {
	return "Create machine image", p.machineImageFields(nil)
}

func (p computeMachineImagesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return p.machineImageFields(nil)
	}
	return p.machineImageFields(p.instanceNames(project))
}

func (p computeMachineImagesProvider) machineImageFields(vms []string) []console.Field {
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		selectOrText(console.Field{Name: "source", Label: "Source VM instance", Required: true}, vms, "ZONE/NAME of a VM instance: none exists yet."),
		{Name: "location", Label: "Location", Type: "radio", Default: "Multi-regional", Options: []string{"Multi-regional", "Regional"}},
	}
}

func (p computeMachineImagesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	zone, vm, err := splitZoneName(values["source"], "source VM instance")
	if err != nil {
		return "", err
	}
	loc := regionOfZoneName(zone)
	if values["location"] != "Regional" {
		loc = strings.SplitN(loc, "-", 2)[0]
	}
	b := map[string]any{"name": name, "sourceInstance": "zones/" + zone + "/instances/" + vm, "storageLocations": []any{loc}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "global/machineImages", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeMachineImagesProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.call("DELETE", project, "global/machineImages/"+name, nil, nil)
	return err
}

func (p computeMachineImagesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || p.api() == nil {
		return nil
	}
	o, err := p.call("GET", project, "global/machineImages/"+path[0], nil, nil)
	if err != nil {
		return nil
	}
	return machineImageActions(o)
}

func (p computeMachineImagesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 1 && action == "labels" {
		return p.setLabels(project, "global/machineImages/"+path[0], values)
	}
	if len(path) != 1 || action != "createvm" {
		return fmt.Errorf("%s is not available here", action)
	}
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	_, err := p.call("POST", project, "zones/"+zone+"/instances", nil, map[string]any{"name": strings.TrimSpace(values["name"]),
		"sourceMachineImage": "global/machineImages/" + path[0]})
	return err
}

// --- SSH keys -----------------------------------------------------------------

// computeSSHKeysProvider is the Metadata page's SSH keys tab
// (compute/docs/connect/add-ssh-keys, read 2026-11-22: "go to the
// Metadata page. Click SSH keys. Click Edit. Click Add item"): the
// project's ssh-keys, one row a key, with Edit.
type computeSSHKeysProvider struct{ computeMetadataProvider }

func (computeSSHKeysProvider) ID() string    { return "compute-ssh-keys" }
func (computeSSHKeysProvider) Title() string { return "SSH keys" }

// sshKeyRows are the ssh-keys value's keys: USERNAME:KEY per line.
func sshKeyRows(v string) []map[string]string {
	var out []map[string]string
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		user, key, _ := strings.Cut(line, ":")
		out = append(out, map[string]string{"username": user, "key": key})
	}
	return out
}

func (p computeSSHKeysProvider) sshKeys(project string) (map[string]any, []map[string]string, string, error) {
	o, rows, err := p.project(project)
	if err != nil {
		return nil, nil, "", err
	}
	for _, r := range rows {
		if r["key"] == "ssh-keys" {
			return o, rows, r["value"], nil
		}
	}
	return o, rows, "", nil
}

func (p computeSSHKeysProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Key"}, Noun: "SSH keys", NameColumn: "Username",
		Note: "Every VM of the project applies these keys within seconds; gcloud compute ssh adds its own here."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "SSH keys are set per project. Choose one in the toolbar."
		return listing, nil
	}
	_, _, v, err := p.sshKeys(project)
	if err != nil {
		return listing, err
	}
	for _, r := range sshKeyRows(v) {
		k := r["key"]
		if len(k) > 120 {
			k = k[:120] + "…"
		}
		listing.Items = append(listing.Items, console.Resource{Name: r["username"], Fields: map[string]string{"Key": k}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeSSHKeysProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	o, _, v, err := p.sshKeys(project)
	if err != nil {
		return nil
	}
	var rows []map[string]string
	for _, r := range sshKeyRows(v) {
		rows = append(rows, map[string]string{"key": r["username"] + ":" + r["key"]})
	}
	return []console.Action{{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
		{Name: "keys", Label: "SSH keys", Type: console.GroupFieldType, AddLabel: "Add item", Default: console.FormatGroup(rows), Group: []console.Field{
			{Name: "key", Label: "SSH key", Type: "textarea", Required: true, Help: "USERNAME:ssh-ed25519 AAAA… or the public key with its comment, USERNAME after the colon's left."},
		}},
		{Name: "fingerprint", Type: "hidden", Default: vpcStr(vmMap(o, "commonInstanceMetadata"), "fingerprint")}}}}
}

func (p computeSSHKeysProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 0 || action != "edit" {
		return fmt.Errorf("%s is not available here", action)
	}
	keys, err := console.ParseGroup(values["keys"])
	if err != nil {
		return err
	}
	var lines []string
	for _, k := range keys {
		line := strings.TrimSpace(k["key"])
		if line == "" {
			continue
		}
		if user, rest, ok := strings.Cut(line, ":"); !ok || user == "" || strings.Contains(user, " ") || rest == "" {
			// A bare public key: its comment's user part is the username,
			// as Google's console takes it.
			f := strings.Fields(line)
			if len(f) < 3 {
				return fmt.Errorf("SSH key %q has no username: write USERNAME:KEY or end the key with USERNAME", line)
			}
			u, _, _ := strings.Cut(f[len(f)-1], "@")
			line = u + ":" + line
		}
		lines = append(lines, line)
	}
	_, rows, _, err := p.sshKeys(project)
	if err != nil {
		return err
	}
	var items []any
	for _, r := range rows {
		if r["key"] != "ssh-keys" {
			items = append(items, map[string]any{"key": r["key"], "value": r["value"]})
		}
	}
	if len(lines) > 0 {
		items = append(items, map[string]any{"key": "ssh-keys", "value": strings.Join(lines, "\n")})
	}
	if items == nil {
		items = []any{}
	}
	_, err = p.call("POST", project, "setCommonInstanceMetadata", nil, map[string]any{"items": items, "fingerprint": values["fingerprint"]})
	return err
}
