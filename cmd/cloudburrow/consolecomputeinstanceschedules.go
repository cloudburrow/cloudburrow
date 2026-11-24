package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

func (computeInstanceSchedulesProvider) ID() string    { return "compute-instanceschedules" }
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
		Note: "An instance schedule starts and stops its VMs (unix-cron, at most one start and one stop an hour); a VM follows one schedule of its region."}
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

func (computeInstanceSchedulesProvider) CreateForm() (string, []console.Field) {
	return "Create schedule", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions(),
			Help: "A schedule applies to the VM instances of its region."},
		{Name: "startSchedule", Label: "Start time (unix-cron)", Type: "text", Section: "Schedule", Default: "0 9 * * 1-5", Pattern: cronPattern,
			Help: "When to start the VMs: minute hour day-of-month month day-of-week, with *, - and ,. Empty starts none."},
		{Name: "stopSchedule", Label: "Stop time (unix-cron)", Type: "text", Section: "Schedule", Default: "0 18 * * 1-5", Pattern: cronPattern,
			Help: "When to stop the VMs. Keep at least 15 minutes between start and stop."},
		{Name: "timeZone", Label: "Time zone", Type: "select", Section: "Schedule", Default: "UTC", Options: scheduleTimeZones},
		{Name: "startTime", Label: "Initiation date (RFC 3339)", Type: "text", Section: "Schedule", Help: "Optional: the schedule runs from this time."},
		{Name: "expirationTime", Label: "End date (RFC 3339)", Type: "text", Section: "Schedule", Help: "Optional: the schedule stops running after this time."},
	}
}

func (p computeInstanceSchedulesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := strings.TrimSpace(values["region"])
	if !compute.IsRegion(region) {
		return "", fmt.Errorf("region: %q is not a region", region)
	}
	isp := map[string]any{"timeZone": vpcOr(values["timeZone"], "UTC")}
	for k, f := range map[string]string{"vmStartSchedule": "startSchedule", "vmStopSchedule": "stopSchedule"} {
		if v := strings.TrimSpace(values[f]); v != "" {
			isp[k] = map[string]any{"schedule": v}
		}
	}
	for _, k := range []string{"startTime", "expirationTime"} {
		if v := strings.TrimSpace(values[k]); v != "" {
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
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, Confirm: "Delete instance schedule " + vpcStr(pol, "name") + "?"}
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
