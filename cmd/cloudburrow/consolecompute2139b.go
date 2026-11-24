package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// --- Instance groups ----------------------------------------------------------

// computeGroupsProvider is Instance groups
// (compute/docs/instance-groups/create-zonal-mig, distributing-instances-
// with-regional-instance-groups, creating-groups-of-unmanaged-instances,
// compute/docs/autoscaler/scaling-cpu, scale-in-controls, read
// 2026-11-22): managed groups in a single zone or across a region's zones
// ("Multiple zones", with the Target distribution shape) and unmanaged
// groups together. Create instance group has Google's Location,
// Autoscaling (mode, minimum and maximum, the CPU utilization signal,
// Initialization period, Scale-in controls) and Port mapping sections. A
// group's page lists its VMs, with Start, Stop, Suspend, Resume and Delete
// instance over the checked ones, and offers Update VMs (a new instance
// template for the VMs made from now on), Restart/replace VMs, Resize (not
// while autoscaled), Edit autoscaling, Edit port mapping, Add and Remove
// instances (unmanaged) and Delete.
//
// A group's path is LOCATION/NAME, LOCATION a zone or, for a regional
// group, a region.
type computeGroupsProvider struct{ computeScreen }

func (computeGroupsProvider) ID() string    { return "compute-instance-groups" }
func (computeGroupsProvider) Title() string { return "Instance groups" }

const (
	groupManaged   = "New managed instance group (stateless)"
	groupUnmanaged = "New unmanaged instance group"

	groupSingleZone    = "Single zone"
	groupMultipleZones = "Multiple zones"

	// The Autoscaling mode choices, Google's words.
	autoscaleOn       = "On: add and remove instances to the group"
	autoscaleScaleOut = "Only scale out: add instances to the group"
	autoscaleOff      = "Off: do not autoscale"

	// groupVMsField carries the VMs checked on a group's page.
	groupVMsField = "vms"
)

var autoscaleModes = map[string]string{autoscaleOn: "ON", autoscaleScaleOut: "ONLY_SCALE_OUT", autoscaleOff: "OFF"}

// groupScope is a group location's API scope: zones/Z or regions/R.
func groupScope(loc string) string {
	if compute.IsRegion(loc) {
		return "regions/" + loc
	}
	return "zones/" + loc
}

// relInProject is a link's path below projects/P/.
func relInProject(link string) string {
	i := strings.Index(link, "projects/")
	if i < 0 {
		return link
	}
	rest := link[i+len("projects/"):]
	if j := strings.Index(rest, "/"); j >= 0 {
		return rest[j+1:]
	}
	return ""
}

// groupLocation is a group's (or manager's) zone or region.
func groupLocation(g map[string]any) string {
	if z := zoneOf(vpcStr(g, "zone")); z != "" {
		return z
	}
	return vpcLast(vpcStr(g, "region"))
}

func vpcNum(o map[string]any, k string) float64 {
	switch v := o[k].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

// autoscalerOf is the autoscaler a manager's status names, or nil.
func (p computeGroupsProvider) autoscalerOf(project string, mgr map[string]any) map[string]any {
	link := vpcStr(vmMap(mgr, "status"), "autoscaler")
	if mgr == nil || link == "" {
		return nil
	}
	a, err := p.call("GET", project, relInProject(link), nil, nil)
	if err != nil {
		return nil
	}
	return a
}

// autoscaleMode is an autoscaler's mode, ON when unset as Google's is.
func autoscaleMode(a map[string]any) string {
	if a == nil {
		return "OFF"
	}
	if m := vpcStr(vmMap(a, "autoscalingPolicy"), "mode"); m != "" {
		return m
	}
	return "ON"
}

// autoscaleWords is the Autoscaling column, as Google's list has it:
// "On: Target CPU utilization 60%", "Off".
func autoscaleWords(a map[string]any) string {
	mode := autoscaleMode(a)
	if mode == "OFF" {
		return "Off"
	}
	words := map[string]string{"ON": "On", "ONLY_SCALE_OUT": "Only scale out"}[mode]
	pol := vmMap(a, "autoscalingPolicy")
	target := vpcNum(vmMap(pol, "cpuUtilization"), "utilizationTarget")
	if target == 0 {
		target = 0.6
	}
	return fmt.Sprintf("%s: Target CPU utilization %d%%", words, int(math.Round(target*100)))
}

func (p computeGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Instances", "Template", "Group type", "Location", "Autoscaling", "Creation time"}, Noun: "instance groups", Note: vmNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Instance groups are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	mgrs := map[string]map[string]any{}
	if m, err := p.call("GET", project, "aggregated/instanceGroupManagers", nil, nil); err == nil {
		for _, g := range aggregatedItems(m, "instanceGroupManagers") {
			mgrs[groupLocation(g)+"/"+vpcStr(g, "name")] = g
		}
	}
	m, err := p.call("GET", project, "aggregated/instanceGroups", nil, nil)
	if err != nil {
		return listing, err
	}
	for _, g := range aggregatedItems(m, "instanceGroups") {
		loc := groupLocation(g)
		t := []string{loc, vpcStr(g, "name")}
		typ, tpl, auto := "Unmanaged", "—", "—"
		mgr := mgrs[loc+"/"+vpcStr(g, "name")]
		status := ""
		if mgr != nil {
			typ, tpl = "Managed (stateless)", vpcLast(vpcStr(mgr, "instanceTemplate"))
			if vmMap(mgr, "status")["isStable"] == true {
				status = "Ready"
			} else {
				status = "Updating"
			}
			auto = autoscaleWords(p.autoscalerOf(project, mgr))
		}
		size := "0"
		if f, ok := g["size"].(float64); ok {
			size = strconv.Itoa(int(f))
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(g, "name"), Status: status, Opens: t, Target: t, Actions: p.groupActions(project, g, mgr, false),
			Fields: map[string]string{"Instances": size, "Template": tpl, "Group type": typ, "Location": loc, "Autoscaling": auto,
				"Creation time": vpcStr(g, "creationTimestamp")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// templateOptions are the project's instance templates as LOCATION/NAME.
func (p computeGroupsProvider) templateOptions(project string) []string {
	var tpls []string
	if l, err := (computeTemplatesProvider{p.computeScreen}).templates(project); err == nil {
		for _, o := range l {
			tpls = append(tpls, strings.Join(templateTarget(o), "/"))
		}
	}
	return tpls
}

func (p computeGroupsProvider) groupActions(project string, g, mgr map[string]any, onPage bool) []console.Action {
	name := vpcStr(g, "name")
	var acts []console.Action
	if mgr != nil {
		a := p.autoscalerOf(project, mgr)
		resize := console.Action{ID: "resize", Label: "Resize", Submit: "Resize", Fields: []console.Field{
			{Name: "size", Label: "Number of instances", Type: "text", Required: true, Pattern: `^[0-9]{1,3}$`,
				Default: strconv.Itoa(int(vpcNum(mgr, "targetSize")))}}}
		if mode := autoscaleMode(a); mode != "OFF" {
			resize = console.Action{ID: "resize", Label: "Resize",
				Unavailable: "Autoscaling is on: the autoscaler sets the number of instances. Set the minimum and maximum in Edit autoscaling, or turn autoscaling off."}
		}
		acts = append(acts, resize,
			console.Action{ID: "restart", Label: "Restart/replace VMs", Submit: "Restart", Fields: []console.Field{
				{Name: "operation", Label: "Operation", Type: "radio", Default: "Restart", Options: []string{"Restart", "Replace"},
					Help: "Restart stops and starts each VM; Replace re-creates each VM's boot disk from the template."}}})
		if onPage {
			acts = append(acts,
				p.updateVMsAction(project, mgr),
				p.autoscalingAction(a))
		}
	} else {
		zone := zoneOf(vpcStr(g, "zone"))
		var free []string
		if vms, err := (computeInstancesProvider{p.computeScreen}).instances(project); err == nil {
			for _, vm := range vms {
				if zoneOf(vpcStr(vm, "zone")) == zone {
					free = append(free, vpcStr(vm, "name"))
				}
			}
		}
		acts = append(acts,
			console.Action{ID: "addinstances", Label: "Add instances", Submit: "Add", Fields: []console.Field{
				{Name: "instances", Label: "VM instances", Type: "text", Required: true, Help: "Comma-separated names of VMs in " + zone + ": " + vpcDash(strings.Join(free, ", ")) + "."}}},
			console.Action{ID: "removeinstances", Label: "Remove instances", Submit: "Remove", Fields: []console.Field{
				{Name: "instances", Label: "VM instances", Type: "text", Required: true, Help: "Comma-separated names of the group's VMs."}}})
	}
	if onPage {
		acts = append(acts, console.Action{ID: "editports", Label: "Edit port mapping", Submit: "Save", Fields: []console.Field{
			namedPortsField(namedPortsRows(g), "")}})
	}
	confirm := "Delete instance group " + name + "?"
	if mgr != nil {
		confirm = "Delete instance group " + name + "? Its VMs are deleted with it."
	}
	return append(acts, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, ConfirmStyle: console.DeleteConfirmClick, Confirm: confirm})
}

// groupTemplateTarget is a manager's template as LOCATION/NAME.
func groupTemplateTarget(mgr map[string]any) string {
	rel := relInProject(vpcStr(mgr, "instanceTemplate"))
	if loc, rest, ok := strings.Cut(rel, "/instanceTemplates/"); ok {
		if strings.HasPrefix(loc, "regions/") {
			return strings.TrimPrefix(loc, "regions/") + "/" + rest
		}
		return "global/" + rest
	}
	return ""
}

// --- Port mapping -------------------------------------------------------------

// namedPortsField is Google's Port mapping: rows of Port name and Port
// numbers (compute/docs/instance-groups/creating-groups-of-unmanaged-
// instances#assign_named_ports).
func namedPortsField(rows []map[string]string, section string) console.Field {
	return console.Field{Name: "namedPorts", Label: "Port mapping", Type: console.GroupFieldType, Section: section, AddLabel: "Add port",
		Default: console.FormatGroup(rows),
		Help:    "A named port maps a name to the port numbers a load balancer sends traffic to on the group's VMs. A name is 1 to 63 lowercase letters, digits and dashes, starting with a letter and not ending with a dash; a port is 1 to 65535.",
		Group: []console.Field{
			{Name: "name", Label: "Port name", Type: "text", Required: true, Pattern: computeNamePattern},
			{Name: "ports", Label: "Port numbers", Type: "text", Required: true, Pattern: `^[0-9]{1,5}(\s*,\s*[0-9]{1,5})*$`, Help: "One or more, comma-separated."},
		}}
}

// namedPortsRows are a group's named ports as Port mapping rows, the ports
// of one name together.
func namedPortsRows(g map[string]any) []map[string]string {
	var rows []map[string]string
	at := map[string]int{}
	for _, np := range vmMaps(g, "namedPorts") {
		n := vpcStr(np, "name")
		port := strconv.Itoa(int(vpcNum(np, "port")))
		if i, ok := at[n]; ok {
			rows[i]["ports"] += "," + port
			continue
		}
		at[n] = len(rows)
		rows = append(rows, map[string]string{"name": n, "ports": port})
	}
	return rows
}

var portNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// parseNamedPorts reads Port mapping into namedPorts[], checked as the
// API checks them.
func parseNamedPorts(value string) ([]any, error) {
	rows, err := console.ParseGroup(value)
	if err != nil {
		return nil, fmt.Errorf("port mapping: %w", err)
	}
	out := []any{}
	for _, r := range rows {
		name := strings.TrimSpace(r["name"])
		if name == "" && strings.TrimSpace(r["ports"]) == "" {
			continue
		}
		if !portNameRE.MatchString(name) {
			return nil, fmt.Errorf("port name %q: must be 1 to 63 lowercase letters, digits and dashes, starting with a letter and not ending with a dash", name)
		}
		ports := vpcList(r["ports"])
		if len(ports) == 0 {
			return nil, fmt.Errorf("port name %s: give at least one port number", name)
		}
		for _, ps := range ports {
			n, err := strconv.Atoi(ps.(string))
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("port name %s: %q is not a port number from 1 to 65535", name, ps)
			}
			out = append(out, map[string]any{"name": name, "port": float64(n)})
		}
	}
	return out, nil
}

// --- Autoscaling --------------------------------------------------------------

// autoscalingFields are Google's Autoscaling section: Autoscaling mode,
// Minimum and Maximum number of instances, the CPU utilization signal,
// Initialization period and Scale-in controls
// (compute/docs/autoscaler/scaling-cpu, scale-in-controls).
func autoscalingFields(a map[string]any, section string, when map[string]string) []console.Field {
	mode, minR, maxR, target, init := autoscaleOn, "1", "10", "60", "60"
	scaleIn, maxIn, unit, window := "false", "1", "VMs", "10"
	if a != nil {
		pol := vmMap(a, "autoscalingPolicy")
		for k, v := range autoscaleModes {
			if v == autoscaleMode(a) {
				mode = k
			}
		}
		minR = strconv.Itoa(int(vpcNum(pol, "minNumReplicas")))
		maxR = strconv.Itoa(int(vpcNum(pol, "maxNumReplicas")))
		if t := vpcNum(vmMap(pol, "cpuUtilization"), "utilizationTarget"); t > 0 {
			target = strconv.Itoa(int(math.Round(t * 100)))
		}
		if c := vpcNum(pol, "coolDownPeriodSec"); c > 0 {
			init = strconv.Itoa(int(c))
		}
		if sic := vmMap(pol, "scaleInControl"); sic != nil {
			mr := vmMap(sic, "maxScaledInReplicas")
			if _, ok := mr["fixed"]; ok {
				scaleIn, maxIn = "true", strconv.Itoa(int(vpcNum(mr, "fixed")))
			} else if _, ok := mr["percent"]; ok {
				scaleIn, maxIn, unit = "true", strconv.Itoa(int(vpcNum(mr, "percent"))), "%"
			}
			if w := vpcNum(sic, "timeWindowSec"); w > 0 {
				window = strconv.Itoa(int(w / 60))
			}
		}
	}
	on := "autoscaling=" + autoscaleOn + "|" + autoscaleScaleOut
	return []console.Field{
		{Name: "autoscaling", Label: "Autoscaling mode", Type: "select", Default: mode, Options: []string{autoscaleOn, autoscaleScaleOut, autoscaleOff},
			Section: section, VisibleWhen: when, Help: "Off keeps the group at the number of instances you set; the autoscaler goes on computing its recommended size."},
		{Name: "minReplicas", Label: "Minimum number of instances", Type: "text", Default: minR, Pattern: `^[0-9]{1,4}$`, Section: section, VisibleWhen: when, ShowWhen: on},
		{Name: "maxReplicas", Label: "Maximum number of instances", Type: "text", Default: maxR, Pattern: `^[0-9]{1,4}$`, Section: section, VisibleWhen: when, ShowWhen: on},
		{Name: "cpuTarget", Label: "Target CPU utilization", Type: "text", Default: target, Pattern: `^([1-9][0-9]?|100)$`, Section: section, VisibleWhen: when, ShowWhen: on,
			Help: "Autoscaling signals: CPU utilization. The group's average CPU utilization, in percent, the autoscaler keeps it at: 1 to 100."},
		{Name: "initPeriod", Label: "Initialization period", Type: "text", Default: init, Pattern: `^[0-9]{1,4}$`, Section: section, VisibleWhen: when, ShowWhen: on,
			Help: "Seconds a new VM takes to initialize: its CPU is not counted until then."},
		{Name: "scaleIn", Label: "Enable scale-in controls", Type: "checkbox", Default: scaleIn, Section: section, VisibleWhen: when, ShowWhen: "autoscaling=" + autoscaleOn},
		{Name: "scaleInMax", Label: "Don't scale in by more than", Type: "text", Default: maxIn, Pattern: `^[0-9]{1,4}$`, Section: section, VisibleWhen: when,
			ShowWhen: "autoscaling=" + autoscaleOn + "&scaleIn"},
		{Name: "scaleInUnit", Label: "Unit", Type: "select", Default: unit, Options: []string{"VMs", "%"}, OptionLabels: map[string]string{"%": "% of instances"},
			Section: section, VisibleWhen: when, ShowWhen: "autoscaling=" + autoscaleOn + "&scaleIn"},
		{Name: "scaleInWindow", Label: "Over the last (minutes)", Type: "text", Default: window, Pattern: `^[0-9]{1,4}$`, Section: section, VisibleWhen: when,
			ShowWhen: "autoscaling=" + autoscaleOn + "&scaleIn"},
	}
}

// autoscalingPolicy reads the Autoscaling section.
func autoscalingPolicy(values map[string]string) (map[string]any, error) {
	mode, ok := autoscaleModes[vpcOr(values["autoscaling"], autoscaleOn)]
	if !ok {
		return nil, fmt.Errorf("autoscaling mode: %q is not a choice", values["autoscaling"])
	}
	num := func(k, d, what string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(vpcOr(values[k], d)))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s: %q is not a number", what, values[k])
		}
		return n, nil
	}
	minR, err := num("minReplicas", "1", "minimum number of instances")
	if err != nil {
		return nil, err
	}
	maxR, err := num("maxReplicas", "10", "maximum number of instances")
	if err != nil {
		return nil, err
	}
	if maxR < 1 || maxR < minR {
		return nil, errors.New("maximum number of instances: must be at least 1 and not below the minimum")
	}
	target, err := num("cpuTarget", "60", "target CPU utilization")
	if err != nil || target < 1 || target > 100 {
		return nil, errors.New("target CPU utilization: must be 1 to 100 percent")
	}
	init, err := num("initPeriod", "60", "initialization period")
	if err != nil {
		return nil, err
	}
	pol := map[string]any{"mode": mode, "minNumReplicas": float64(minR), "maxNumReplicas": float64(maxR), "coolDownPeriodSec": float64(init),
		"cpuUtilization": map[string]any{"utilizationTarget": float64(target) / 100, "predictiveMethod": "NONE"}}
	if values["scaleIn"] == "true" && mode == "ON" {
		n, err := num("scaleInMax", "1", "don't scale in by more than")
		if err != nil {
			return nil, err
		}
		w, err := num("scaleInWindow", "10", "over the last (minutes)")
		if err != nil {
			return nil, err
		}
		mr := map[string]any{"fixed": float64(n)}
		if values["scaleInUnit"] == "%" {
			if n > 100 {
				return nil, errors.New("don't scale in by more than: a percentage is 0 to 100")
			}
			mr = map[string]any{"percent": float64(n)}
		} else if n < 1 {
			return nil, errors.New("don't scale in by more than: must be a positive number of VMs")
		}
		pol["scaleInControl"] = map[string]any{"maxScaledInReplicas": mr, "timeWindowSec": float64(w * 60)}
	}
	return pol, nil
}

func (p computeGroupsProvider) autoscalingAction(a map[string]any) console.Action {
	return console.Action{ID: "editautoscaling", Label: "Edit autoscaling", Submit: "Save", Fields: autoscalingFields(a, "", nil)}
}

// setAutoscaling makes, changes or turns off a manager's autoscaler, as
// Google's console does: one named after the group, in its zone or region.
func (p computeGroupsProvider) setAutoscaling(project, loc, name string, mgr, a map[string]any, values map[string]string) error {
	pol, err := autoscalingPolicy(values)
	if err != nil {
		return err
	}
	scope := groupScope(loc)
	if a != nil {
		an := vpcStr(a, "name")
		_, err := p.call("PATCH", project, scope+"/autoscalers", url.Values{"autoscaler": {an}},
			map[string]any{"name": an, "target": vpcStr(a, "target"), "autoscalingPolicy": pol})
		return err
	}
	if pol["mode"] == "OFF" {
		return nil
	}
	_, err = p.call("POST", project, scope+"/autoscalers", nil, map[string]any{"name": name,
		"target": scope + "/instanceGroupManagers/" + name, "autoscalingPolicy": pol})
	return err
}

// --- Create instance group ----------------------------------------------------

func (computeGroupsProvider) CreateOnPage() bool   { return true }
func (computeGroupsProvider) CreateTitle() string  { return "Create instance group" }
func (computeGroupsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p computeGroupsProvider) CreateForm() (string, []console.Field) {
	return "Create instance group", p.groupFields(nil, nil)
}

func (p computeGroupsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return p.groupFields(nil, nil)
	}
	return p.groupFields(p.templateOptions(project), p.networkNames(ctx, project))
}

// targetShapes are the Target distribution shape choices, Google's
// (compute/docs/instance-groups/regional-mig-distribution-shape).
var targetShapes = []string{"EVEN", "BALANCED", "ANY", "ANY_SINGLE_ZONE"}

func (p computeGroupsProvider) groupFields(templates, networks []string) []console.Field {
	managed := map[string]string{"kind": groupManaged}
	unmanaged := map[string]string{"kind": groupUnmanaged}
	tpl := selectOrText(console.Field{Name: "template", Label: "Instance template", Required: true, VisibleWhen: managed,
		Help: "LOCATION/NAME: global/web or us-central1/web."}, templates, "LOCATION/NAME of an instance template: create one on the Instance templates page.")
	nw := selectOrText(console.Field{Name: "network", Label: "Network", Default: "default", VisibleWhen: unmanaged}, networks, "A VPC network's name.")
	multi := "locationType=" + groupMultipleZones
	fields := []console.Field{
		{Name: "kind", Label: "Group type", Type: "radio", Default: groupManaged, Options: []string{groupManaged, groupUnmanaged}},
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		tpl,
		{Name: "locationType", Label: "Location", Type: "radio", Default: groupSingleZone, Options: []string{groupSingleZone, groupMultipleZones}, VisibleWhen: managed,
			Section: "Location", Help: "Multiple zones spreads the group's VMs across zones of the region, for higher availability."},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionOptions(), Section: "Location"},
		func() console.Field {
			f := vmZoneField("zone")
			f.Section, f.ShowWhen = "Location", "locationType="+groupSingleZone
			return f
		}(),
		{Name: "zones", Label: "Zones", Type: "text", Section: "Location", VisibleWhen: managed, ShowWhen: multi, Completions: compute.Zones("us-central1"),
			Help: "Comma-separated zones of the region the group's VMs are spread across. Empty: three of the region's zones."},
		{Name: "targetShape", Label: "Target distribution shape", Type: "radio", Default: "EVEN", Options: targetShapes, Section: "Location", VisibleWhen: managed, ShowWhen: multi,
			OptionLabels: map[string]string{"EVEN": "Even", "BALANCED": "Balanced", "ANY": "Any", "ANY_SINGLE_ZONE": "Any single zone"},
			Help:         "Even keeps the number of VMs in each zone within one of the others'; Balanced and Any place VMs where there is capacity, Any single zone puts them all in one zone."},
	}
	fields = append(fields, autoscalingFields(nil, "Autoscaling", managed)...)
	fields = append(fields,
		console.Field{Name: "size", Label: "Number of instances", Type: "text", Default: "1", Pattern: `^[0-9]{1,3}$`, VisibleWhen: managed,
			ShowWhen: "autoscaling=" + autoscaleOff, Section: "Autoscaling"},
		namedPortsField(nil, "Port mapping"),
		nw,
		console.Field{Name: "instances", Label: "VM instances", Type: "text", VisibleWhen: unmanaged, Help: "Optional, comma-separated names of VMs in the zone."},
	)
	return fields
}

func (p computeGroupsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	ports, err := parseNamedPorts(values["namedPorts"])
	if err != nil {
		return "", err
	}
	if len(ports) > 0 {
		b["namedPorts"] = ports
	}
	if values["kind"] == groupUnmanaged {
		zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
		if nw := strings.TrimSpace(values["network"]); nw != "" {
			b["network"] = "global/networks/" + nw
		}
		if _, err := p.call("POST", project, "zones/"+zone+"/instanceGroups", nil, b); err != nil {
			return "", err
		}
		if insts := vpcList(values["instances"]); len(insts) > 0 {
			return name, p.ActAt(ctx, project, []string{zone, name}, "addinstances", map[string]string{"instances": values["instances"]})
		}
		return name, nil
	}
	loc, tpl, ok := strings.Cut(strings.TrimSpace(values["template"]), "/")
	if !ok {
		return "", fmt.Errorf("instance template: %q is not LOCATION/NAME", values["template"])
	}
	var pol map[string]any
	size := 0
	if autoscaleModes[vpcOr(values["autoscaling"], autoscaleOn)] == "OFF" {
		if size, err = strconv.Atoi(strings.TrimSpace(vpcOr(values["size"], "1"))); err != nil {
			return "", fmt.Errorf("number of instances: %q is not a number", values["size"])
		}
	} else {
		if pol, err = autoscalingPolicy(values); err != nil {
			return "", err
		}
		// The group starts at its minimum; the autoscaler takes it from there.
		size = int(pol["minNumReplicas"].(float64))
	}
	b["instanceTemplate"] = templateBase([]string{loc, tpl})
	b["baseInstanceName"] = name
	b["targetSize"] = float64(size)
	where := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if values["locationType"] == groupMultipleZones {
		region := strings.TrimSpace(vpcOr(values["region"], "us-central1"))
		if !compute.IsRegion(region) {
			return "", fmt.Errorf("region: %q is not a region", region)
		}
		where = region
		dp := map[string]any{"targetShape": vpcOr(values["targetShape"], "EVEN")}
		if zs := vpcList(values["zones"]); len(zs) > 0 {
			var zl []any
			for _, z := range zs {
				if regionOfZoneName(z.(string)) != region || !compute.IsZone(z.(string)) {
					return "", fmt.Errorf("zones: %s is not a zone of %s", z, region)
				}
				zl = append(zl, map[string]any{"zone": "zones/" + z.(string)})
			}
			dp["zones"] = zl
		}
		b["distributionPolicy"] = dp
	}
	scope := groupScope(where)
	if _, err := p.call("POST", project, scope+"/instanceGroupManagers", nil, b); err != nil {
		return "", err
	}
	if pol != nil {
		if _, err := p.call("POST", project, scope+"/autoscalers", nil, map[string]any{"name": name,
			"target": scope + "/instanceGroupManagers/" + name, "autoscalingPolicy": pol}); err != nil {
			return name, fmt.Errorf("the group was made, its autoscaler was not: %w", err)
		}
	}
	return name, nil
}

// --- A group's page -----------------------------------------------------------

func (p computeGroupsProvider) group(project string, path []string) (map[string]any, map[string]any, error) {
	scope := groupScope(path[0])
	g, err := p.call("GET", project, scope+"/instanceGroups/"+path[1], nil, nil)
	if err != nil {
		return nil, nil, err
	}
	mgr, err := p.call("GET", project, scope+"/instanceGroupManagers/"+path[1], nil, nil)
	if err != nil {
		mgr = nil
	}
	return g, mgr, nil
}

// groupVMSelectActions are Google's buttons over the checked VMs of a
// managed group (compute/docs/instance-groups/stopping-and-starting-
// vms-in-mig, suspending-and-resuming-vms-in-mig, deleting-vms-in-mig).
func groupVMSelectActions() []console.Action {
	sel := []console.Field{{Name: groupVMsField, Type: "hidden"}}
	act := func(id, label, title, confirm string) console.Action {
		return console.Action{ID: id, Label: label, SelectionField: groupVMsField, Fields: sel, ConfirmStyle: console.DeleteConfirmClick,
			ConfirmTitle: title, Confirm: confirm, Destructive: id == "deletevms"}
	}
	return []console.Action{
		act("startvms", "Start", "Start VMs?", "The checked VMs start; the group keeps them running from now on."),
		act("stopvms", "Stop", "Stop VMs?", "The checked VMs stop; the group keeps them stopped, their disks kept."),
		act("suspendvms", "Suspend", "Suspend VMs?", "The checked VMs are suspended, their memory kept."),
		act("resumevms", "Resume", "Resume VMs?", "The checked suspended VMs resume."),
		act("deletevms", "Delete instance", "Delete instances?", "The checked VMs are deleted, and the group's target size goes down by as many."),
		updateSelectedAction(),
	}
}

func (p computeGroupsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.DeeperThan(2, path), nil
	}
	g, mgr, err := p.group(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	scope := groupScope(path[0])
	vms := console.Listing{Columns: []string{"Zone", "Status", "Template", "Internal IP"}, Noun: "VM instances"}
	versions := map[string]string{}
	if mgr != nil {
		vms.SelectActions = groupVMSelectActions()
		if l, err := p.call("POST", project, scope+"/instanceGroupManagers/"+path[1]+"/listManagedInstances", nil, map[string]any{}); err == nil {
			for _, it := range vmMaps(l, "managedInstances") {
				versions[relInProject(vpcStr(it, "instance"))] = vpcLast(vpcStr(vmMap(it, "version"), "instanceTemplate"))
			}
		}
	}
	l, err := p.call("POST", project, scope+"/instanceGroups/"+path[1]+"/listInstances", nil, map[string]any{})
	if err == nil {
		for _, it := range vmMaps(l, "items") {
			ir := relInProject(vpcStr(it, "instance"))
			zone := zoneOf(ir)
			vm, _ := p.call("GET", project, "zones/"+zone+"/instances/"+vpcLast(ir), nil, nil)
			vms.Items = append(vms.Items, console.Resource{Name: vpcLast(ir), Selectable: mgr != nil,
				Fields: map[string]string{"Zone": zone, "Status": vpcStr(it, "status"), "Template": vpcDash(versions[ir]), "Internal IP": vpcDash(vmInternalIP(vm))}})
		}
	}
	vms.Total = len(vms.Items)
	loc := []console.Property{{Label: "Zone", Value: path[0]}}
	if compute.IsRegion(path[0]) {
		var zs []string
		dp := vmMap(mgr, "distributionPolicy")
		for _, z := range vmMaps(dp, "zones") {
			zs = append(zs, vpcLast(vpcStr(z, "zone")))
		}
		loc = []console.Property{{Label: "Region", Value: path[0]}, {Label: "Zones", Value: vpcDash(strings.Join(zs, ", "))},
			{Label: "Target distribution shape", Value: map[string]string{"EVEN": "Even", "BALANCED": "Balanced", "ANY": "Any", "ANY_SINGLE_ZONE": "Any single zone"}[vpcOr(vpcStr(dp, "targetShape"), "EVEN")]}}
	}
	var ports []string
	for _, r := range namedPortsRows(g) {
		ports = append(ports, r["name"]+":"+r["ports"])
	}
	props := append([]console.Property{{Label: "Group type", Value: "Unmanaged"}}, loc...)
	props = append(props, console.Property{Label: "Network", Value: vpcDash(vpcLast(vpcStr(g, "network")))})
	groups := []console.PropertyGroup{}
	if mgr != nil {
		props = append([]console.Property{{Label: "Group type", Value: "Managed (stateless)"}}, loc...)
		props = append(props,
			console.Property{Label: "Instance template", Value: vpcLast(vpcStr(mgr, "instanceTemplate"))},
			console.Property{Label: "Target size", Value: strconv.Itoa(int(vpcNum(mgr, "targetSize")))},
			console.Property{Label: "Base instance name", Value: vpcStr(mgr, "baseInstanceName")})
		groups = append(groups, console.PropertyGroup{Heading: "Autoscaling", Properties: p.autoscalingProps(p.autoscalerOf(project, mgr))})
	}
	props = append(props, console.Property{Label: "Named ports", Value: vpcDash(strings.Join(ports, ", "))},
		console.Property{Label: "Description", Value: vpcDash(vpcStr(g, "description"))},
		console.Property{Label: "Creation time", Value: vpcStr(g, "creationTimestamp")})
	groups = append([]console.PropertyGroup{{Properties: props}}, groups...)
	return console.Detail{Title: path[1], Heading: "Instance group details", Sections: []console.Section{
		{ID: "overview", Label: "Overview", Listing: vms},
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}}}, nil
}

// autoscalingProps are the group page's Autoscaling: its mode, minimum
// and maximum, signal, initialization period, scale-in controls, the
// autoscaler's recommended size and status.
func (p computeGroupsProvider) autoscalingProps(a map[string]any) []console.Property {
	if a == nil {
		return []console.Property{{Label: "Autoscaling mode", Value: "Off"}}
	}
	pol := vmMap(a, "autoscalingPolicy")
	mode := map[string]string{"ON": "On", "ONLY_SCALE_OUT": "Only scale out", "OFF": "Off"}[autoscaleMode(a)]
	target := vpcNum(vmMap(pol, "cpuUtilization"), "utilizationTarget")
	if target == 0 {
		target = 0.6
	}
	scaleIn := "Off"
	if sic := vmMap(pol, "scaleInControl"); sic != nil {
		mr := vmMap(sic, "maxScaledInReplicas")
		n := strconv.Itoa(int(vpcNum(mr, "fixed"))) + " VMs"
		if _, ok := mr["percent"]; ok {
			n = strconv.Itoa(int(vpcNum(mr, "percent"))) + "%"
		}
		scaleIn = fmt.Sprintf("Don't scale in by more than %s over the last %d minutes", n, int(vpcNum(sic, "timeWindowSec")/60))
	}
	rec := "—"
	if _, ok := a["recommendedSize"]; ok {
		rec = strconv.Itoa(int(vpcNum(a, "recommendedSize")))
	}
	var details []string
	for _, d := range vmMaps(a, "statusDetails") {
		details = append(details, vpcStr(d, "message"))
	}
	return []console.Property{
		{Label: "Autoscaling mode", Value: mode},
		{Label: "Minimum number of instances", Value: strconv.Itoa(int(vpcNum(pol, "minNumReplicas")))},
		{Label: "Maximum number of instances", Value: strconv.Itoa(int(vpcNum(pol, "maxNumReplicas")))},
		{Label: "Autoscaling signals", Value: fmt.Sprintf("CPU utilization: %d%%", int(math.Round(target*100)))},
		{Label: "Initialization period", Value: strconv.Itoa(int(vpcOrNum(vpcNum(pol, "coolDownPeriodSec"), 60))) + " seconds"},
		{Label: "Scale-in controls", Value: scaleIn},
		{Label: "Recommended size", Value: rec},
		{Label: "Status", Value: vpcDash(vpcStr(a, "status"))},
		{Label: "Status details", Value: vpcDash(strings.Join(details, " "))},
	}
}

func vpcOrNum(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}

func (p computeGroupsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	g, mgr, err := p.group(project, path)
	if err != nil {
		return nil
	}
	acts := p.groupActions(project, g, mgr, true)
	if mgr != nil {
		acts = append(acts, groupVMSelectActions()...)
	}
	return acts
}

// managedVMs are a manager's VMs, ZONE-qualified (zones/Z/instances/N), by
// name, skipping those it is deleting.
func (p computeGroupsProvider) managedVMs(project, mg string) (map[string]string, []string, error) {
	l, err := p.call("POST", project, mg+"/listManagedInstances", nil, map[string]any{})
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]string{}
	var all []string
	for _, it := range vmMaps(l, "managedInstances") {
		// A VM the group is deleting (a resize down) is not acted on.
		if vpcStr(it, "currentAction") == "DELETING" {
			continue
		}
		rel := relInProject(vpcStr(it, "instance"))
		byName[vpcLast(rel)] = rel
		all = append(all, rel)
	}
	return byName, all, nil
}

func (p computeGroupsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	scope := groupScope(path[0])
	ig := scope + "/instanceGroups/" + path[1]
	mg := scope + "/instanceGroupManagers/" + path[1]
	switch action {
	case "delete":
		if mgr, err := p.call("GET", project, mg, nil, nil); err == nil {
			// Google's Delete removes the group's autoscaler with it.
			if a := p.autoscalerOf(project, mgr); a != nil {
				if _, err := p.call("DELETE", project, scope+"/autoscalers/"+vpcStr(a, "name"), nil, nil); err != nil {
					return err
				}
			}
			_, err := p.call("DELETE", project, mg, nil, nil)
			return err
		}
		_, err := p.call("DELETE", project, ig, nil, nil)
		return err
	case "resize":
		_, err := p.call("POST", project, mg+"/resize", url.Values{"size": {strings.TrimSpace(values["size"])}}, nil)
		return err
	case "restart":
		_, insts, err := p.managedVMs(project, mg)
		if err != nil {
			return err
		}
		if values["operation"] == "Replace" {
			_, err = p.call("POST", project, mg+"/recreateInstances", nil, map[string]any{"instances": toAnyStrings(insts)})
			return err
		}
		// Each VM is reset in its own zone: a regional group's are spread.
		for _, rel := range insts {
			if _, err := p.call("POST", project, "zones/"+zoneOf(rel)+"/instances/"+vpcLast(rel)+"/reset", nil, nil); err != nil {
				return err
			}
		}
		return nil
	case "startvms", "stopvms", "suspendvms", "resumevms", "deletevms":
		byName, _, err := p.managedVMs(project, mg)
		if err != nil {
			return err
		}
		var insts []string
		for _, n := range vpcList(values[groupVMsField]) {
			rel, ok := byName[n.(string)]
			if !ok {
				return fmt.Errorf("%s is not a VM of %s", n, path[1])
			}
			insts = append(insts, rel)
		}
		if len(insts) == 0 {
			return errors.New("check one or more VMs first")
		}
		verb := strings.TrimSuffix(action, "vms") + "Instances"
		_, err = p.call("POST", project, mg+"/"+verb, nil, map[string]any{"instances": toAnyStrings(insts)})
		return err
	case "updatevms":
		// Versions and updatePolicy (consolecomputeupdate2139.go).
		body, err := updateVMsBody(values)
		if err != nil {
			return err
		}
		_, err = p.call("PATCH", project, mg, nil, body)
		return err
	case "applyupdates":
		byName, _, err := p.managedVMs(project, mg)
		if err != nil {
			return err
		}
		var insts []string
		for _, n := range vpcList(values[groupVMsField]) {
			rel, ok := byName[n.(string)]
			if !ok {
				return fmt.Errorf("%s is not a VM of %s", n, path[1])
			}
			insts = append(insts, rel)
		}
		if len(insts) == 0 {
			return errors.New("check one or more VMs first")
		}
		_, err = p.call("POST", project, mg+"/applyUpdatesToInstances", nil, map[string]any{"instances": toAnyStrings(insts),
			"minimalAction": vpcOr(values["minimalAction"], "NONE"), "mostDisruptiveAllowedAction": vpcOr(values["mostDisruptiveAllowedAction"], "REPLACE")})
		return err
	case "editautoscaling":
		_, mgr, err := p.group(project, path)
		if err != nil {
			return err
		}
		if mgr == nil {
			return errors.New("an unmanaged group is not autoscaled")
		}
		return p.setAutoscaling(project, path[0], path[1], mgr, p.autoscalerOf(project, mgr), values)
	case "editports":
		ports, err := parseNamedPorts(values["namedPorts"])
		if err != nil {
			return err
		}
		g, err := p.call("GET", project, ig, nil, nil)
		if err != nil {
			return err
		}
		_, err = p.call("POST", project, ig+"/setNamedPorts", nil, map[string]any{"namedPorts": ports, "fingerprint": vpcStr(g, "fingerprint")})
		return err
	case "addinstances", "removeinstances":
		var insts []any
		for _, n := range vpcList(values["instances"]) {
			insts = append(insts, map[string]any{"instance": "zones/" + path[0] + "/instances/" + n.(string)})
		}
		verb := map[string]string{"addinstances": "addInstances", "removeinstances": "removeInstances"}[action]
		_, err := p.call("POST", project, ig+"/"+verb, nil, map[string]any{"instances": insts})
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

func toAnyStrings(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
