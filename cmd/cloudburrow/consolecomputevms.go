package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The Compute Engine pages (#1965), Compute Engine in Google's console
// menu: VM instances (with Google's Create an instance page and an
// instance's page and tabs), Disks, Snapshots, Images and Machine types.
// As the VPC network pages, each read and change is a call on the
// compute.Server the Compute Engine API serves: an instance made here is
// the one gcloud compute and Terraform read, and a refusal is the API's
// own message. Changes return as soon as the API has started the
// operation, as Google's pages do; the list shows the instance's status
// while it boots or stops.

const vmNote = "Each VM runs as a systemd container in the instance's Kubernetes cluster. Its external IP (natIP) is an address of its own from 192.0.2.0/24, " +
	"a documentation range nothing routes; its port 22 is forwarded to the 127.0.0.1 port the Connect column names, so gcloud compute ssh takes " +
	"--ssh-flag=\"-p PORT\" --ssh-flag=\"-o HostName=127.0.0.1\". " +
	"VMs reach each other over their VPC network, its firewall rules enforced on each."

const defaultZone = "us-central1-a"

// vmZones are every zone, in region order.
func vmZones() []string {
	var out []string
	for _, r := range compute.Regions() {
		out = append(out, compute.Zones(r)...)
	}
	return out
}

func vmZoneField(name string) console.Field {
	return console.Field{Name: name, Label: "Zone", Type: "select", Required: true, Default: defaultZone, Options: vmZones()}
}

// imageFamilies are the public image families, as "project/family".
func imageFamilies() []string {
	var out []string
	for _, p := range compute.PublicImages {
		out = append(out, p.Project+"/"+p.Family)
	}
	return out
}

func imageFamilyLabels() map[string]string {
	out := map[string]string{}
	for _, p := range compute.PublicImages {
		out[p.Project+"/"+p.Family] = strings.SplitN(p.Description, " (", 2)[0]
	}
	return out
}

func machineTypeNames() []string {
	var out []string
	for _, m := range compute.MachineTypes {
		out = append(out, m.Name)
	}
	return out
}

// machineSeries is a machine type's series as Google's Series field
// names it (compute/docs/general-purpose-machines): f1-micro and g1-small
// are N1's shared-core types.
func machineSeries(name string) string {
	fam, _, _ := strings.Cut(name, "-")
	switch fam {
	case "f1", "g1":
		return "N1"
	}
	return strings.ToUpper(fam)
}

// seriesTypes are the machine types of each series, in their order.
func seriesTypes() (series []string, types map[string][]string) {
	types = map[string][]string{}
	for _, m := range compute.MachineTypes {
		sr := machineSeries(m.Name)
		if _, ok := types[sr]; !ok {
			series = append(series, sr)
		}
		types[sr] = append(types[sr], m.Name)
	}
	return series, types
}

func diskTypeNames() []string {
	return []string{"pd-balanced", "pd-ssd", "pd-standard", "pd-extreme", "hyperdisk-balanced"}
}

// zoneOf is a zonal link's zone.
func zoneOf(link string) string {
	parts := strings.Split(link, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "zones" {
			return parts[i+1]
		}
	}
	return ""
}

// aggregatedItems are an aggregatedList's resources of one collection, by
// name order.
func aggregatedItems(m map[string]any, coll string) []map[string]any {
	items, _ := m["items"].(map[string]any)
	var out []map[string]any
	for _, sc := range items {
		e, _ := sc.(map[string]any)
		l, _ := e[coll].([]any)
		for _, it := range l {
			if o, ok := it.(map[string]any); ok {
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := vpcStr(out[i], "name"), vpcStr(out[j], "name"); a != b {
			return a < b
		}
		return vpcStr(out[i], "zone") < vpcStr(out[j], "zone")
	})
	return out
}

func vmMap(o map[string]any, k string) map[string]any {
	m, _ := o[k].(map[string]any)
	return m
}

// vmLabelsField is a Compute Engine resource's labels.
func vmLabelsField(l map[string]string, section string) console.Field {
	return pubsubLabelsField(console.FormatMap(l), section, "One key=value per line, such as env=dev. Empty: no labels. Up to 64 labels. "+
		"A key is 1 to 63 lowercase letters, international characters, digits, _ and -, starting with a lowercase letter or an international character; "+
		"a value is 0 to 63 of the same characters.")
}

func vmMaps(o map[string]any, k string) []map[string]any {
	l, _ := o[k].([]any)
	var out []map[string]any
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func vmLabels(o map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range vmMap(o, "labels") {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func vmLabelsText(o map[string]any) string {
	l := vmLabels(o)
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+l[k])
	}
	return strings.Join(parts, ", ")
}

func vmInternalIP(o map[string]any) string {
	for _, n := range vmMaps(o, "networkInterfaces") {
		return vpcStr(n, "networkIP")
	}
	return ""
}

func vmExternalIP(o map[string]any) string {
	for _, n := range vmMaps(o, "networkInterfaces") {
		for _, ac := range vmMaps(n, "accessConfigs") {
			if ip := vpcStr(ac, "natIP"); ip != "" {
				return ip
			}
		}
	}
	return ""
}

// vmSSHPort is the loopback port a running instance's port 22 is
// forwarded to, or "".
func vmSSHPort(o map[string]any) string {
	_, port, err := net.SplitHostPort(vpcStr(o, "cloudburrowSshAddress"))
	if err != nil {
		return ""
	}
	return port
}

func vmMetadata(o map[string]any) []map[string]string {
	var rows []map[string]string
	for _, it := range vmMaps(vmMap(o, "metadata"), "items") {
		rows = append(rows, map[string]string{"key": vpcStr(it, "key"), "value": vpcStr(it, "value")})
	}
	return rows
}

// --- VM instances -------------------------------------------------------------

// computeInstancesProvider is VM instances: every instance of the project
// across zones (the aggregated list), each row with SSH and its menu
// (Start / Resume, Stop, Suspend, Reset, Delete); Create instance on its
// own page (machine configuration, boot disk, networking, metadata and the
// startup script, the service account); an instance's page at [zone,
// name] with its Details, Storage, Networking and Serial port 1 tabs,
// Edit (metadata, labels, network tags, deletion protection), Attach and
// Detach disk.
type computeInstancesProvider struct{ computeScreen }

func (computeInstancesProvider) ID() string    { return "compute-instances" }
func (computeInstancesProvider) Title() string { return "VM instances" }

func (p computeInstancesProvider) instances(project string) ([]map[string]any, error) {
	m, err := p.call("GET", project, "aggregated/instances", nil, nil)
	if err != nil {
		return nil, err
	}
	return aggregatedItems(m, "instances"), nil
}

func (p computeInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// Google's VM instances columns (compute/docs/instances/view-vm-details,
	// read 2026-11-22): Zone, Machine type, the provisioning model, In use
	// by (the instance group), the IP addresses and Connect.
	listing := console.Listing{Columns: []string{"Zone", "Machine type", "Provisioning model", "In use by", "Internal IP", "External IP", "Connect"},
		Noun: "VM instances", Note: vmNote, AlwaysStatus: true}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "VM instances are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.instances(project)
	if err != nil {
		return listing, err
	}
	groups := p.unmanagedGroupsOf(project)
	for _, o := range l {
		zone := zoneOf(vpcStr(o, "zone"))
		target := []string{zone, vpcStr(o, "name")}
		if g := groups[zone+"/"+vpcStr(o, "name")]; g != "" && vmInUseBy(o) == "" {
			o["cloudburrowInstanceGroup"] = g
		}
		connect := "—"
		if port := vmSSHPort(o); port != "" {
			connect = "SSH (port " + port + ")"
		}
		ext := vmExternalIP(o)
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Status: vpcStr(o, "status"),
			Opens: target, Target: target, Actions: p.instanceActions(o, false), Selectable: true,
			Fields: map[string]string{"Zone": zone, "Machine type": vpcLast(vpcStr(o, "machineType")),
				"Provisioning model": provisioningModelText(o), "In use by": vpcDash(vmInUseBy(o)),
				"Internal IP": vpcDash(vmInternalIP(o)), "External IP": vpcDash(ext), "Connect": connect}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// provisioningModelText is the instance's provisioning model as Google's
// list writes it: Standard or Spot.
func provisioningModelText(o map[string]any) string {
	if vpcStr(vmMap(o, "scheduling"), "provisioningModel") == "SPOT" {
		return "Spot"
	}
	return "Standard"
}

// vmInUseBy is the instance group the VM belongs to: a managed group's
// created-by metadata, or the unmanaged groups that list it.
func vmInUseBy(o map[string]any) string {
	for _, m := range vmMetadata(o) {
		if m["key"] == "created-by" {
			return vpcLast(m["value"])
		}
	}
	return vpcStr(o, "cloudburrowInstanceGroup")
}

// unmanagedGroupsOf are the unmanaged instance groups' VMs, as
// ZONE/NAME to the group's name.
func (p computeInstancesProvider) unmanagedGroupsOf(project string) map[string]string {
	out := map[string]string{}
	m, err := p.call("GET", project, "aggregated/instanceGroups", nil, nil)
	if err != nil {
		return out
	}
	for _, g := range aggregatedItems(m, "instanceGroups") {
		zone := zoneOf(vpcStr(g, "zone"))
		if zone == "" || vpcStr(g, "region") != "" {
			// A regional group is a manager's: its VMs name it in their
			// created-by metadata.
			continue
		}
		l, err := p.call("POST", project, "zones/"+zone+"/instanceGroups/"+vpcStr(g, "name")+"/listInstances", nil, map[string]any{})
		if err != nil {
			continue
		}
		for _, it := range vmMaps(l, "items") {
			out[zone+"/"+vpcLast(vpcStr(it, "instance"))] = vpcStr(g, "name")
		}
	}
	return out
}

// skipGraceful is the Stop and Delete dialogs' checkbox
// (compute/docs/instances/stop-start-instance, deleting-instance, read
// 2026-11-22: "Optional: To skip graceful shutdown, select the Skip
// graceful shutdown (if applicable) checkbox").
func skipGraceful() console.Field {
	return console.Field{Name: "noGracefulShutdown", Label: "Skip graceful shutdown (if applicable)", Type: "checkbox",
		Help: "The guest gets no shutdown time: its container is stopped at once."}
}

// gracefulAction is Google's one Stop or Delete dialog for a VM: "Stop
// NAME?" with what it does, the Skip graceful shutdown checkbox, Cancel
// and the action's button, nothing typed.
func gracefulAction(id, label, title, consequence string) console.Action {
	f := skipGraceful()
	f.Help = consequence + " " + f.Help
	return console.Action{ID: id, Label: label, Title: title, Submit: label, Fields: []console.Field{f}}
}

// instanceSelectField carries the checked instances, one ZONE/NAME... per
// line as the rows' names.
const instanceSelectField = "instances"

// ListSelectActions are the actions Google offers over the checked VMs
// (stop-start-instance: "Select one or more VMs ... click Stop").
func (p computeInstancesProvider) ListSelectActions(ctx context.Context, project string) []console.Action {
	sel := console.Field{Name: instanceSelectField, Type: "hidden"}
	act := func(id, label, confirm string, extra ...console.Field) console.Action {
		return console.Action{ID: id, Label: label, SelectionField: instanceSelectField, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: confirm, Fields: append([]console.Field{sel}, extra...), Destructive: id == "delete"}
	}
	return []console.Action{
		act("start", "Start / Resume", "Start or resume the selected VM instances?"),
		act("stop", "Stop", "Stop the selected VM instances? Their processes end; their disks keep what they hold.", skipGraceful()),
		act("suspend", "Suspend", "Suspend the selected VM instances?"),
		act("reset", "Reset", "Reset the selected VM instances? They restart at once, without shutting down cleanly."),
		act("delete", "Delete", "Delete the selected VM instances? Their boot disks are deleted with them when auto-delete is on.", skipGraceful()),
	}
}

var _ console.SelectionLister = computeInstancesProvider{}

// instanceActions are what Google offers an instance in its status: SSH
// and Stop, Suspend and Reset while it runs, Start / Resume when it is
// stopped or suspended, and Delete. onPage adds the page's own actions.
//
// Each is confirmed as Google's console confirms it, by its button alone,
// nothing typed: "To confirm, click Stop", "Click Suspend, and then click
// Suspend to confirm", "Click Start / Resume, and then click Start",
// "click Reset to confirm", and Delete's dialog "click the Delete button"
// (docs.cloud.google.com/compute/docs/instances/stop-start-instance,
// suspend-resume-instance, reset-instance, deleting-instance).
func (p computeInstancesProvider) instanceActions(o map[string]any, onPage bool) []console.Action {
	name := vpcStr(o, "name")
	status := vpcStr(o, "status")
	var acts []console.Action
	if status == "RUNNING" {
		acts = append(acts, console.Action{ID: "ssh", Label: "SSH", Button: true})
	}
	if status == "TERMINATED" || status == "SUSPENDED" {
		acts = append(acts, console.Action{ID: "start", Label: "Start / Resume", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Start or resume " + name + "?"})
	}
	if status == "RUNNING" || status == "SUSPENDED" {
		acts = append(acts, gracefulAction("stop", "Stop", "Stop "+name+"?", "Its processes end; its disks keep what they hold."))
	}
	if status == "RUNNING" {
		acts = append(acts,
			console.Action{ID: "suspend", Label: "Suspend", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Suspend " + name + "? Its processes are frozen until it is resumed."},
			console.Action{ID: "reset", Label: "Reset", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Reset " + name + "? It restarts at once, without shutting down cleanly."})
	}
	if onPage {
		acts = append(acts, p.diskActions(o)...)
	}
	del := gracefulAction("delete", "Delete", "Delete "+name+"?", "Its boot disk is deleted with it when auto-delete is on.")
	del.Destructive, del.Leaves = true, onPage
	if o["deletionProtection"] == true {
		del = console.Action{ID: "delete", Label: "Delete", Destructive: true, Unavailable: "Deletion protection is on: turn it off in Edit first."}
	}
	return append(acts, del)
}

func (p computeInstancesProvider) diskActions(o map[string]any) []console.Action {
	zone := zoneOf(vpcStr(o, "zone"))
	project := vpcStr(o, "selfLink")
	if i := strings.Index(project, "projects/"); i >= 0 {
		project = strings.SplitN(project[i+len("projects/"):], "/", 2)[0]
	}
	var free []string
	if l, err := p.list(project, "zones/"+zone+"/disks"); err == nil {
		for _, d := range l {
			if len(vpcStrs(d, "users")) == 0 {
				free = append(free, vpcStr(d, "name"))
			}
		}
	}
	free = append(free, p.freeRegionalDisks(project, zone)...)
	var acts []console.Action
	attach := console.Action{ID: "attachdisk", Label: "Attach existing disk", Submit: "Save", Fields: []console.Field{
		{Name: "disk", Label: "Disk", Type: "select", Required: true, Options: free},
		{Name: "deviceName", Label: "Device name", Type: "text", Pattern: `^$|` + computeNamePattern,
			Help: "Optional: the disk is mounted at /mnt/disks/DEVICE in the guest; empty uses the disk's name."},
		{Name: "mode", Label: "Mode", Type: "radio", Default: "Read/write", Options: []string{"Read/write", "Read only"}},
	}}
	if len(free) == 0 {
		attach.Unavailable = "No disk in " + zone + " is free to attach: create one on the Disks page."
		attach.Fields = nil
	}
	acts = append(acts, attach)
	var devices []string
	for _, d := range vmMaps(o, "disks") {
		if d["boot"] != true {
			devices = append(devices, vpcStr(d, "deviceName"))
		}
	}
	if len(devices) > 0 {
		acts = append(acts, console.Action{ID: "detachdisk", Label: "Detach disk", Submit: "Detach", Fields: []console.Field{
			{Name: "deviceName", Label: "Disk", Type: "select", Required: true, Options: devices}}})
	}
	return acts
}

func (computeInstancesProvider) CreateOnPage() bool   { return true }
func (computeInstancesProvider) CreateTitle() string  { return "Create an instance" }
func (computeInstancesProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateForm is Google's Create an instance, its panes in Google's order
// (compute/docs/instances/create-start-instance, create-use-spot,
// create-vm-specific-subnet, preventing-accidental-vm-deletion, read
// 2026-11-22): Machine configuration, OS and storage, Data protection,
// Networking, Observability, Security and Advanced.
func (p computeInstancesProvider) CreateForm() (string, []console.Field) {
	return "Create instance", p.createFields(nil, nil)
}

// CreateFormFor fills the project's snapshot schedules and networks in.
func (p computeInstancesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		_, f := p.CreateForm()
		return f
	}
	return p.withStaticExternal(project, p.createFields(p.snapshotScheduleNames(project), p.networkNames(ctx, project)))
}

func (p computeInstancesProvider) createFields(schedules, networks []string) []console.Field {
	external := map[string]string{"externalIP": "Ephemeral"}
	spot := map[string]string{"provisioningModel": "Spot"}
	series, types := seriesTypes()
	mc, os, dp, nw, ob, sec, adv := "Machine configuration", "OS and storage", "Data protection", "Networking", "Observability", "Security", "Advanced"
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Section: nw, Required: true, Default: "default", Options: networks}
	if len(networks) == 0 {
		netField.Type = "text"
	}
	backup := console.Field{Name: "snapshotSchedule", Label: "Back up your data", Type: "select", Section: dp, Default: noBackups,
		Options: append([]string{noBackups}, schedules...),
		Help:    "A snapshot schedule of the region takes the boot disk's snapshots; create one on the Snapshots page's Snapshot schedules tab."}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Section: mc, Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		vmLabelsField(nil, mc),
		{Name: "region", Label: "Region", Type: "select", Section: mc, Required: true, Default: "us-central1", Options: compute.Regions()},
		withSection(vmZoneField("zone"), mc),
		{Name: "series", Label: "Series", Type: "select", Section: mc, Required: true, Default: "E2", Options: series},
		{Name: "machineType", Label: "Machine type", Type: "select", Section: mc, Required: true, Default: "e2-medium",
			Options: machineTypeNames(), OptionsBy: "series", OptionsFor: types,
			Help: "The VM's container gets the type's vCPUs and memory as its limits."},
		{Name: "provisioningModel", Label: "VM provisioning model", Type: "select", Section: mc, Default: "Standard", Options: []string{"Standard", "Spot"},
			Help: "Spot VMs cost less and can be preempted; locally nothing preempts them."},
		{Name: "terminationAction", Label: "On VM termination", Type: "select", Section: mc, Default: "Stop", Options: []string{"Stop", "Delete"},
			VisibleWhen: spot, Help: "What happens to the VM when it is preempted."},
		{Name: "image", Label: "Image", Type: "select", Section: os, Required: true, Default: "debian-cloud/debian-12",
			Options: imageFamilies(), OptionLabels: imageFamilyLabels(),
			Help: "Container-Optimized OS, Windows and licensed images are not served: see the Images page."},
		{Name: "bootDiskType", Label: "Boot disk type", Type: "select", Section: os, Default: "pd-balanced", Options: diskTypeNames()},
		{Name: "bootDiskSize", Label: "Size (GB)", Type: "text", Section: os, Default: "10", Pattern: `^[0-9]{1,5}$`},
		backup,
		{Name: "allowHttp", Label: "Allow HTTP traffic", Type: "checkbox", Section: nw,
			Help: "Tags the VM http-server and adds the network's allow-http rule (tcp:80 from 0.0.0.0/0) when it has none."},
		{Name: "allowHttps", Label: "Allow HTTPS traffic", Type: "checkbox", Section: nw,
			Help: "Tags the VM https-server and adds the network's allow-https rule (tcp:443 from 0.0.0.0/0) when it has none."},
		{Name: "networkTags", Label: "Network tags", Type: "text", Section: nw, Help: "Comma-separated, for example web, backend."},
		netField,
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Section: nw,
			Help: "Optional: a subnet of the network in the instance's region; empty uses the network's subnet there."},
		vmStackTypeField(nw),
		{Name: "internalIP", Label: "Primary internal IPv4 address", Type: "text", Section: nw, Pattern: `^$|^[0-9.]+$`,
			Help: "Optional: Ephemeral (Automatic) when empty, or an address of the subnet, a reserved static internal one among them."},
		{Name: "externalIP", Label: "External IPv4 address", Type: "select", Section: nw, Default: "Ephemeral", Options: []string{"Ephemeral", "None"},
			Help: "Ephemeral gives the instance an external IP of its own from 192.0.2.0/24 (promote it to static on the IP addresses page); its port 22 is forwarded to a port on 127.0.0.1."},
		{Name: "networkTier", Label: "Network Service Tier", Type: "radio", Section: nw, Default: "Premium", Options: []string{"Premium", "Standard"}, VisibleWhen: external},
		{Name: "opsAgent", Label: "Install Ops Agent for Monitoring and Logging", Type: "checkbox", Section: ob,
			Unavailable: "The Ops Agent is not served: Cloud Monitoring and Cloud Logging do not collect a VM's metrics or logs here; its serial port output is on its page."},
		{Name: "serviceAccount", Label: "Service account", Type: "select", Section: sec, Default: "Compute Engine default service account",
			Options: []string{"Compute Engine default service account", "No service account"}},
		{Name: "scopes", Label: "Access scopes", Type: "radio", Section: sec, Default: "Allow default access",
			Options: []string{"Allow default access", "Allow full access to all Cloud APIs"}},
		{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Section: adv},
		{Name: "startupScript", Label: "Startup script", Type: "textarea", Section: adv,
			Help: "Runs as root at every boot (the startup-script metadata key); its output is on the serial port."},
		{Name: "metadata", Label: "Metadata", Type: console.GroupFieldType, Section: adv, AddLabel: "Add item", Group: []console.Field{
			{Name: "key", Label: "Key", Type: "text", Required: true},
			{Name: "value", Label: "Value", Type: "textarea"},
		}},
	}
}

// noBackups is Data protection's choice of no snapshot schedule
// (compute/docs/disks/default-backup).
const noBackups = "Don't use automatically scheduled backups"

// snapshotScheduleNames are the project's snapshot schedules as
// REGION/NAME.
func (c computeScreen) snapshotScheduleNames(project string) []string {
	m, err := c.call("GET", project, "aggregated/resourcePolicies", nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range aggregatedItems(m, "resourcePolicies") {
		if o["snapshotSchedulePolicy"] != nil {
			out = append(out, vpcLast(vpcStr(o, "region"))+"/"+vpcStr(o, "name"))
		}
	}
	sort.Strings(out)
	return out
}

// allowWebRule makes sure the network has Google's allow rule for a web
// port: default-allow-http on default, NETWORK-allow-http on another (the
// console's name for another network is UNVERIFIED, re-read 2026-11-22:
// https://docs.cloud.google.com/compute/docs/create-windows-server-vm-instance).
func (p computeInstancesProvider) allowWebRule(project, network, tag, port string) error {
	name := network + "-allow-" + strings.TrimSuffix(tag, "-server")
	if _, err := p.call("GET", project, "global/firewalls/"+name, nil, nil); err == nil {
		return nil
	}
	_, err := p.call("POST", project, "global/firewalls", nil, map[string]any{"name": name, "network": "global/networks/" + network,
		"direction": "INGRESS", "priority": 1000.0, "sourceRanges": []any{"0.0.0.0/0"}, "targetTags": []any{tag},
		"allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []any{port}}}})
	return err
}

var defaultScopes = []any{
	"https://www.googleapis.com/auth/devstorage.read_only", "https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/monitoring.write", "https://www.googleapis.com/auth/service.management.readonly",
	"https://www.googleapis.com/auth/servicecontrol", "https://www.googleapis.com/auth/trace.append",
}

func (p computeInstancesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	body, err := p.createBody(project, values)
	if err != nil {
		return "", err
	}
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if _, err := p.call("POST", project, "zones/"+zone+"/instances", nil, body); err != nil {
		return "", err
	}
	return vpcStr(body, "name"), nil
}

// createBody is Create an instance's instances.insert body; the network's
// allow-http and allow-https rules it asks for are made on the way.
func (p computeInstancesProvider) createBody(project string, values map[string]string) (map[string]any, error) {
	name := strings.TrimSpace(values["name"])
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if !compute.IsZone(zone) {
		return nil, fmt.Errorf("zone: %q is not a zone", zone)
	}
	size, err := strconv.Atoi(strings.TrimSpace(vpcOr(values["bootDiskSize"], "10")))
	if err != nil {
		return nil, fmt.Errorf("boot disk size: %q is not a number", values["bootDiskSize"])
	}
	img := strings.TrimSpace(vpcOr(values["image"], "debian-cloud/debian-12"))
	proj, fam, ok := strings.Cut(img, "/")
	if !ok {
		return nil, fmt.Errorf("image: %q is not project/family", img)
	}
	nic := map[string]any{"network": "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default"))}
	if s := strings.TrimSpace(values["subnetwork"]); s != "" {
		nic["subnetwork"] = "regions/" + zone[:strings.LastIndex(zone, "-")] + "/subnetworks/" + s
	}
	if ac, err := p.createAccessConfig(project, values); err != nil {
		return nil, err
	} else if ac != nil {
		nic["accessConfigs"] = []any{ac}
	}
	if ip := strings.TrimSpace(values["internalIP"]); ip != "" {
		nic["networkIP"] = ip
	}
	if values["stackType"] == "IPV4_IPV6" {
		nic["stackType"] = "IPV4_IPV6"
	}
	body := map[string]any{"name": name, "machineType": "zones/" + zone + "/machineTypes/" + strings.TrimSpace(vpcOr(values["machineType"], "e2-medium")),
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "deviceName": name, "initializeParams": map[string]any{
			"sourceImage": "projects/" + proj + "/global/images/family/" + fam, "diskSizeGb": strconv.Itoa(size),
			"diskType": "zones/" + zone + "/diskTypes/" + vpcOr(values["bootDiskType"], "pd-balanced")}}},
		"networkInterfaces": []any{nic}, "deletionProtection": values["deletionProtection"] == "true"}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		body["labels"] = l
	}
	var tags []string
	for _, t := range vpcList(values["networkTags"]) {
		tags = append(tags, t.(string))
	}
	network := strings.TrimSpace(vpcOr(values["network"], "default"))
	for _, web := range []struct{ field, tag, port string }{{"allowHttp", "http-server", "80"}, {"allowHttps", "https-server", "443"}} {
		if values[web.field] != "true" {
			continue
		}
		if !slices.Contains(tags, web.tag) {
			tags = append(tags, web.tag)
		}
		if err := p.allowWebRule(project, network, web.tag, web.port); err != nil {
			return nil, err
		}
	}
	if len(tags) > 0 {
		items := make([]any, len(tags))
		for i, t := range tags {
			items[i] = t
		}
		body["tags"] = map[string]any{"items": items}
	}
	if values["provisioningModel"] == "Spot" {
		body["scheduling"] = map[string]any{"provisioningModel": "SPOT", "instanceTerminationAction": strings.ToUpper(vpcOr(values["terminationAction"], "Stop")),
			"automaticRestart": false, "onHostMaintenance": "TERMINATE"}
	}
	if sch := values["snapshotSchedule"]; sch != "" && sch != noBackups {
		region, pol, ok := strings.Cut(sch, "/")
		if !ok || region != zone[:strings.LastIndex(zone, "-")] {
			return nil, fmt.Errorf("back up your data: the schedule %q is not in the instance's region", sch)
		}
		disks := body["disks"].([]any)
		disks[0].(map[string]any)["initializeParams"].(map[string]any)["resourcePolicies"] = []any{"regions/" + region + "/resourcePolicies/" + pol}
	}
	if values["serviceAccount"] != "No service account" {
		scopes := defaultScopes
		if values["scopes"] == "Allow full access to all Cloud APIs" {
			scopes = []any{"https://www.googleapis.com/auth/cloud-platform"}
		}
		body["serviceAccounts"] = []any{map[string]any{"email": "default", "scopes": scopes}}
	}
	items, err := metadataBody(values["metadata"], values["startupScript"])
	if err != nil {
		return nil, err
	}
	if len(items) > 0 {
		body["metadata"] = map[string]any{"items": items}
	}
	return body, nil
}

// metadataBody is a metadata group field's rows as the API's items, with
// the startup script, when given, as startup-script.
func metadataBody(group, script string) ([]any, error) {
	rows, err := console.ParseGroup(group)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, r := range rows {
		k := strings.TrimSpace(r["key"])
		if k == "" || (k == "startup-script" && strings.TrimSpace(script) != "") {
			continue
		}
		items = append(items, map[string]any{"key": k, "value": r["value"]})
	}
	if strings.TrimSpace(script) != "" {
		items = append(items, map[string]any{"key": "startup-script", "value": script})
	}
	return items, nil
}

func (p computeInstancesProvider) instance(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("an instance is addressed by its zone and name, not %v", path)
	}
	return p.call("GET", project, "zones/"+path[0]+"/instances/"+path[1], nil, nil)
}

func (p computeInstancesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.DeeperThan(2, path), nil
	}
	o, err := p.instance(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	zone, name := path[0], path[1]
	sa := "None"
	scopes := "—"
	for _, s := range vmMaps(o, "serviceAccounts") {
		sa = vpcStr(s, "email")
		scopes = strings.Join(vpcStrs(s, "scopes"), ", ")
	}
	md := vmMetadata(o)
	var mdProps []console.Property
	for _, r := range md {
		mdProps = append(mdProps, console.Property{Label: r["key"], Value: r["value"]})
	}
	if len(mdProps) == 0 {
		mdProps = []console.Property{{Label: "Custom metadata", Value: "None"}}
	}
	sshCmd, gcloudCmd := sshCommands(project, zone, name, o)
	details := console.Section{ID: "details", Label: "Details", Kind: console.KindProperties, Note: vmNote, Groups: []console.PropertyGroup{
		{Heading: "Basic information", Properties: []console.Property{
			{Label: "Name", Value: name}, {Label: "Instance ID", Value: vpcStr(o, "id")},
			{Label: "Description", Value: vpcDash(vpcStr(o, "description"))}, {Label: "Status", Value: vpcStr(o, "status")},
			{Label: "Creation time", Value: vpcStr(o, "creationTimestamp")}, {Label: "Zone", Value: zone},
			{Label: "Labels", Value: vpcDash(vmLabelsText(o))},
			{Label: "Deletion protection", Value: map[bool]string{true: "Enabled", false: "Disabled"}[o["deletionProtection"] == true]},
		}},
		{Heading: "Machine configuration", Properties: []console.Property{
			{Label: "Machine type", Value: vpcLast(vpcStr(o, "machineType"))}, {Label: "CPU platform", Value: vpcStr(o, "cpuPlatform")},
			{Label: "Architecture", Value: vpcDash(vpcStr(o, "cpuPlatform"))},
		}},
		{Heading: "Networking", Properties: []console.Property{
			{Label: "Internal IP", Value: vpcDash(vmInternalIP(o))}, {Label: "External IP", Value: vpcDash(vmExternalIP(o))},
			{Label: "Network tags", Value: vpcDash(strings.Join(vpcStrs(vmMap(o, "tags"), "items"), ", "))},
			{Label: "SSH address", Value: vpcDash(vpcStr(o, "cloudburrowSshAddress")), Copy: vpcStr(o, "cloudburrowSshAddress")},
		}},
		availabilityGroup(o),
		{Heading: "Metadata", Properties: mdProps},
		{Heading: "API and identity management", Properties: []console.Property{
			{Label: "Service account", Value: sa}, {Label: "Cloud API access scopes", Value: scopes}}},
		{Heading: "Connect", Properties: []console.Property{
			{Label: "gcloud", Value: vpcDash(gcloudCmd), Copy: gcloudCmd}, {Label: "ssh", Value: vpcDash(sshCmd), Copy: sshCmd}}},
	}}
	if strings.HasPrefix(vpcStr(o, "cpuPlatform"), "Unknown") {
		details.Groups[1].Properties = details.Groups[1].Properties[:2]
	}
	disks := console.Listing{Columns: []string{"Device name", "Type", "Size", "Mode", "When deleting instance"}, Noun: "disks"}
	for _, d := range vmMaps(o, "disks") {
		src := vpcLast(vpcStr(d, "source"))
		kind := "Additional disk"
		if d["boot"] == true {
			kind = "Boot disk"
		}
		del := "Keep disk"
		if d["autoDelete"] == true {
			del = "Delete disk"
		}
		mode := "Read/write"
		if vpcStr(d, "mode") == "READ_ONLY" {
			mode = "Read only"
		}
		size := vpcStr(d, "diskSizeGb")
		if size != "" {
			size += " GB"
		}
		disks.Items = append(disks.Items, console.Resource{Name: src, Fields: map[string]string{"Device name": vpcStr(d, "deviceName"),
			"Type": kind, "Size": vpcDash(size), "Mode": mode, "When deleting instance": del}})
	}
	disks.Total = len(disks.Items)
	nics := console.Listing{Columns: []string{"Network", "Subnetwork", "IP stack type", "Primary internal IP", "Internal IPv6", "External IP", "External IPv6", "Network tier"},
		Noun: "network interfaces"}
	for _, n := range vmMaps(o, "networkInterfaces") {
		tier, ext := "—", "None"
		for _, ac := range vmMaps(n, "accessConfigs") {
			tier, ext = vpcStr(ac, "networkTier"), vpcDash(vpcStr(ac, "natIP"))
		}
		nics.Items = append(nics.Items, console.Resource{Name: vpcStr(n, "name"), Fields: map[string]string{"Network": vpcLast(vpcStr(n, "network")),
			"Subnetwork": vpcLast(vpcStr(n, "subnetwork")), "Primary internal IP": vpcStr(n, "networkIP"), "External IP": ext, "Network tier": tier,
			"IP stack type": stackTypeText(vpcStr(n, "stackType")), "Internal IPv6": vpcDash(vpcStr(n, "ipv6Address")), "External IPv6": vpcDash(nicExternalIPv6(n))}})
	}
	nics.Total = len(nics.Items)
	serial := console.Section{ID: "serial", Label: "Serial port 1 (console)", Kind: console.KindText}
	if out, err := p.call("GET", project, "zones/"+zone+"/instances/"+name+"/serialPort", nil, nil); err == nil {
		text := vpcStr(out, "contents")
		if len(text) > 64<<10 {
			text = text[len(text)-64<<10:]
		}
		serial.Text = text
		if text == "" {
			serial.Text = "The instance has written nothing to its serial port yet."
		}
	} else {
		serial.Unavailable = err.Error()
	}
	tags := strings.Join(vpcStrs(vmMap(o, "tags"), "items"), ", ")
	return console.Detail{
		Title: name,
		Summary: []console.Property{{Label: "Status", Value: vpcStr(o, "status")}, {Label: "Zone", Value: zone},
			{Label: "Machine type", Value: vpcLast(vpcStr(o, "machineType"))}},
		Sections: []console.Section{details,
			{ID: "storage", Label: "Storage", Listing: disks},
			{ID: "networking", Label: "Networking", Listing: nics},
			serial},
		Edit: &console.EditForm{Label: "Edit", Note: "The machine type and the VM provisioning model change only while the instance is stopped.", Fields: []console.Field{
			vmLabelsField(vmLabels(o), ""),
			{Name: "networkTags", Label: "Network tags", Type: "text", Default: tags},
			{Name: "machineType", Label: "Machine type", Type: "select", Default: vpcLast(vpcStr(o, "machineType")), Options: machineTypeNames()},
			provisioningModelEditField(o),
			terminationActionEditField(o),
			{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Default: strconv.FormatBool(o["deletionProtection"] == true)},
			{Name: "metadata", Label: "Metadata", Type: console.GroupFieldType, AddLabel: "Add item", Default: console.FormatGroup(md), Group: []console.Field{
				{Name: "key", Label: "Key", Type: "text", Required: true},
				{Name: "value", Label: "Value", Type: "textarea"},
			}},
		}},
	}, nil
}

// sshCommands are how to reach a running instance's guest from this
// machine: ssh to its forwarded port, and gcloud compute ssh with that port.
func sshCommands(project, zone, name string, o map[string]any) (sshCmd, gcloudCmd string) {
	port := vmSSHPort(o)
	if port == "" {
		return "", ""
	}
	host, _, _ := net.SplitHostPort(vpcStr(o, "cloudburrowSshAddress"))
	return "ssh -p " + port + " -i ~/.ssh/google_compute_engine USER@" + host,
		"gcloud compute ssh " + name + " --zone " + zone + " --project " + project + " --ssh-flag=\"-p " + port + "\" --ssh-flag=\"-o HostName=" + host + "\""
}

// Edit is the instance page's Edit: labels (setLabels), network tags
// (setTags), the machine type (setMachineType, stopped only), deletion
// protection and metadata (setMetadata), each sent only when it changed.
func (p computeInstancesProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	o, err := p.instance(project, path)
	if err != nil {
		return err
	}
	base := "zones/" + path[0] + "/instances/" + path[1]
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	if console.FormatMap(labels) != console.FormatMap(vmLabels(o)) {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		if _, err := p.call("POST", project, base+"/setLabels", nil, map[string]any{"labels": l, "labelFingerprint": vpcStr(o, "labelFingerprint")}); err != nil {
			return err
		}
	}
	tags := vmMap(o, "tags")
	if strings.Join(vpcStrs(tags, "items"), ",") != strings.Join(func() []string {
		var s []string
		for _, t := range vpcList(values["networkTags"]) {
			s = append(s, t.(string))
		}
		return s
	}(), ",") {
		if _, err := p.call("POST", project, base+"/setTags", nil, map[string]any{"items": vpcList(values["networkTags"]), "fingerprint": vpcStr(tags, "fingerprint")}); err != nil {
			return err
		}
	}
	if mt := strings.TrimSpace(values["machineType"]); mt != "" && mt != vpcLast(vpcStr(o, "machineType")) {
		if _, err := p.call("POST", project, base+"/setMachineType", nil, map[string]any{"machineType": "zones/" + path[0] + "/machineTypes/" + mt}); err != nil {
			return err
		}
	}
	if err := p.editProvisioningModel(project, base, o, values); err != nil {
		return err
	}
	if dp := values["deletionProtection"] == "true"; dp != (o["deletionProtection"] == true) {
		if _, err := p.call("POST", project, base+"/setDeletionProtection", url.Values{"deletionProtection": {strconv.FormatBool(dp)}}, nil); err != nil {
			return err
		}
	}
	if _, ok := values["metadata"]; ok && values["metadata"] != console.FormatGroup(vmMetadata(o)) {
		items, err := metadataBody(values["metadata"], "")
		if err != nil {
			return err
		}
		if _, err := p.call("POST", project, base+"/setMetadata", nil, map[string]any{"items": items,
			"fingerprint": vpcStr(vmMap(o, "metadata"), "fingerprint")}); err != nil {
			return err
		}
	}
	return nil
}

func (p computeInstancesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	o, err := p.instance(project, path)
	if err != nil {
		return nil
	}
	return p.instanceActions(o, true)
}

func (p computeInstancesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult performs an instance's action; SSH answers with the
// commands that reach the guest.
func (p computeInstancesProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 && values[instanceSelectField] != "" {
		return nil, p.actOnSelection(project, action, values)
	}
	if len(path) != 2 {
		return nil, fmt.Errorf("%s is not available here", action)
	}
	base := "zones/" + path[0] + "/instances/" + path[1]
	var err error
	switch action {
	case "ssh":
		o, err := p.instance(project, path)
		if err != nil {
			return nil, err
		}
		sshCmd, gcloudCmd := sshCommands(project, path[0], path[1], o)
		if sshCmd == "" {
			return nil, fmt.Errorf("%s is %s: SSH reaches a running instance only", path[1], vpcStr(o, "status"))
		}
		return &console.Listing{Columns: []string{"Command"}, Noun: "ways to connect", Total: 2,
			Note: "The guest's port 22 is forwarded to " + vpcStr(o, "cloudburrowSshAddress") + ". gcloud compute ssh adds its key to the project's ssh-keys metadata, which the guest applies within seconds.",
			Items: []console.Resource{{Name: "gcloud", Fields: map[string]string{"Command": gcloudCmd}},
				{Name: "ssh", Fields: map[string]string{"Command": sshCmd}}}}, nil
	case "start":
		o, e := p.instance(project, path)
		if e != nil {
			return nil, e
		}
		verb := "start"
		if vpcStr(o, "status") == "SUSPENDED" {
			verb = "resume"
		}
		_, err = p.call("POST", project, base+"/"+verb, nil, nil)
	case "stop":
		_, err = p.call("POST", project, base+"/stop", gracefulQuery(values), nil)
	case "suspend", "reset":
		_, err = p.call("POST", project, base+"/"+action, nil, nil)
	case "delete":
		_, err = p.call("DELETE", project, base, gracefulQuery(values), nil)
	case "attachdisk":
		d := strings.TrimSpace(values["disk"])
		src := "zones/" + path[0] + "/disks/" + d
		if region, name, ok := strings.Cut(d, "/"); ok {
			// A regional disk, REGION/NAME (consolecomputea1.go).
			src, d = "regions/"+region+"/disks/"+name, name
		}
		b := map[string]any{"source": src, "deviceName": vpcOr(strings.TrimSpace(values["deviceName"]), d)}
		if values["mode"] == "Read only" {
			b["mode"] = "READ_ONLY"
		}
		_, err = p.call("POST", project, base+"/attachDisk", nil, b)
	case "detachdisk":
		_, err = p.call("POST", project, base+"/detachDisk", url.Values{"deviceName": {strings.TrimSpace(values["deviceName"])}}, nil)
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
	}
	return nil, err
}

func gracefulQuery(values map[string]string) url.Values {
	if values["noGracefulShutdown"] == "true" {
		return url.Values{"noGracefulShutdown": {"true"}}
	}
	return nil
}

// actOnSelection performs a selection action on each checked instance,
// found by name across the project's zones; an instance whose status the
// action does not apply to is skipped, as Google's console skips it.
func (p computeInstancesProvider) actOnSelection(project, action string, values map[string]string) error {
	all, err := p.instances(project)
	if err != nil {
		return err
	}
	var errs []string
	for _, name := range strings.Split(values[instanceSelectField], "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		for _, o := range all {
			if vpcStr(o, "name") != name {
				continue
			}
			found = true
			ok := false
			for _, a := range p.instanceActions(o, false) {
				ok = ok || (a.ID == action && a.Unavailable == "")
			}
			if !ok {
				continue
			}
			if _, err := p.ActAtResult(context.Background(), project, []string{zoneOf(vpcStr(o, "zone")), name}, action, values); err != nil {
				errs = append(errs, name+": "+err.Error())
			}
		}
		if !found {
			errs = append(errs, name+": no such instance")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// --- Disks --------------------------------------------------------------------

// computeDisksProvider is Disks: every disk of the project across zones;
// Create disk on its own page (blank, from an image or from a snapshot);
// a disk's page at [zone, name] with Create snapshot, Edit (its size) and
// Delete.
type computeDisksProvider struct{ computeScreen }

func (computeDisksProvider) ID() string    { return "compute-disks" }
func (computeDisksProvider) Title() string { return "Disks" }

func (p computeDisksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Size", "Zone", "In use by"}, Noun: "disks", AlwaysStatus: true,
		Note: "A disk is a volume in the instance's Kubernetes cluster: an attached disk is mounted at /mnt/disks/DEVICE in the guest, as a directory rather than a block device."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Disks are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/disks", nil, nil)
	if err != nil {
		return listing, err
	}
	for _, d := range aggregatedItems(m, "disks") {
		zone := diskZoneText(d)
		target := []string{diskLoc(d), vpcStr(d, "name")}
		var users []string
		for _, u := range vpcStrs(d, "users") {
			users = append(users, vpcLast(u))
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(d, "name"), Status: vpcStr(d, "status"), Opens: target, Target: target,
			Actions: p.diskActions(d, false),
			Fields: map[string]string{"Type": vpcLast(vpcStr(d, "type")), "Size": vpcStr(d, "sizeGb") + " GB", "Zone": zone,
				"In use by": vpcDash(strings.Join(users, ", "))}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeDisksProvider) diskActions(d map[string]any, onPage bool) []console.Action {
	name := vpcStr(d, "name")
	acts := []console.Action{
		{ID: "snapshot", Label: "Create snapshot", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
			{Name: "description", Label: "Description", Type: "text"}}},
		{ID: "resize", Label: "Edit size", Submit: "Save", Fields: []console.Field{
			{Name: "sizeGb", Label: "Size (GB)", Type: "text", Required: true, Default: vpcStr(d, "sizeGb"), Pattern: `^[0-9]{1,5}$`,
				Help: "A disk can be made larger, never smaller."}}},
	}
	acts = append(acts, labelsAction(d),
		console.Action{ID: "clone", Label: "Clone disk", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Default: name + "-clone", Help: computeNameHelp},
			{Name: "description", Label: "Description", Type: "text"}}},
		console.Action{ID: "image", Label: "Create image", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
			{Name: "family", Label: "Family", Type: "text", Pattern: `^$|` + computeNamePattern},
			{Name: "description", Label: "Description", Type: "text"},
			{Name: "force", Label: "Keep instance running (not recommended)", Type: "checkbox",
				Help: "An image of a disk a running VM writes to may catch its files half-written."}}})
	var schedules []string
	for _, r := range vpcStrs(d, "resourcePolicies") {
		schedules = append(schedules, vpcLast(r))
	}
	acts = append(acts, console.Action{ID: "schedule", Label: "Edit snapshot schedule", Submit: "Save", Fields: []console.Field{
		{Name: "schedule", Label: "Snapshot schedule", Type: "text", Default: strings.Join(schedules, ","),
			Help: "The name of a snapshot schedule of the disk's region, or empty for none."}}})
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, Confirm: "Delete disk " + name + "?"}
	if users := vpcStrs(d, "users"); len(users) > 0 {
		del.Unavailable = "The disk is attached to " + vpcLast(users[0]) + ": detach it first."
	}
	return append(acts, del)
}

func (computeDisksProvider) CreateOnPage() bool   { return true }
func (computeDisksProvider) CreateTitle() string  { return "Create a disk" }
func (computeDisksProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateForm is Google's Create a disk (compute/docs/disks/add-persistent-disk,
// regional-persistent-disk, create-disk-from-source, read 2026-11-22):
// Name, Description, Location (Single zone, or Regional with its Replica
// zone), Region and Zone, Source (Blank disk, Image, Snapshot or another Disk),
// Disk type and Size, Snapshot schedule, Labels.
func (p computeDisksProvider) CreateForm() (string, []console.Field) {
	return "Create disk", p.diskFields(nil, nil, nil, nil)
}

func (p computeDisksProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		_, f := p.CreateForm()
		return f
	}
	return p.diskFields(p.snapshotNames(project), p.diskNames(project), p.customImageNames(project), p.snapshotScheduleNames(project))
}

func (p computeDisksProvider) diskFields(snapshots, disks, images, schedules []string) []console.Field {
	imgs := append(imageFamilies(), images...)
	labels := imageFamilyLabels()
	for _, i := range images {
		labels[i] = i + " (custom image)"
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "location", Label: "Location", Type: "radio", Section: "Location", Default: "Single zone", Options: []string{"Single zone", "Regional"},
			Help: "Regional replicates the disk's data synchronously in two zones of the region."},
		{Name: "region", Label: "Region", Type: "select", Section: "Location", Default: "us-central1", Options: compute.Regions()},
		withSection(vmZoneField("zone"), "Location"),
		replicaZoneField(),
		{Name: "source", Label: "Disk source type", Type: "radio", Section: "Source", Default: "Blank disk", Options: []string{"Blank disk", "Image", "Snapshot", "Disk"}},
		{Name: "image", Label: "Source image", Type: "select", Section: "Source", Default: "debian-cloud/debian-12", Options: imgs,
			OptionLabels: labels, VisibleWhen: map[string]string{"source": "Image"}},
		selectOrText(console.Field{Name: "snapshot", Label: "Source snapshot", Section: "Source", VisibleWhen: map[string]string{"source": "Snapshot"}},
			snapshots, "The name of a snapshot: none exists yet."),
		selectOrText(console.Field{Name: "sourceDisk", Label: "Source disk", Section: "Source", VisibleWhen: map[string]string{"source": "Disk"}},
			disks, "ZONE/NAME of a disk of the zone: none exists yet."),
		{Name: "type", Label: "Disk type", Type: "select", Section: "Disk settings", Default: "pd-balanced", Options: diskTypeNames()},
		{Name: "sizeGb", Label: "Size (GB)", Type: "text", Section: "Disk settings", Default: "10", Pattern: `^[0-9]{1,5}$`},
		{Name: "snapshotSchedule", Label: "Snapshot schedule", Type: "select", Section: "Snapshot schedule", Default: noBackups,
			Options: append([]string{noBackups}, schedules...), Help: "A schedule of the disk's region, as REGION/NAME."},
		vmLabelsField(nil, "Labels"),
	}
}

func (p computeDisksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if !compute.IsZone(zone) {
		return "", fmt.Errorf("zone: %q is not a zone", zone)
	}
	b := map[string]any{"name": name, "type": "zones/" + zone + "/diskTypes/" + vpcOr(values["type"], "pd-balanced")}
	coll := "zones/" + zone + "/disks"
	if values["location"] == "Regional" {
		region := regionOfZoneName(zone)
		replica := strings.TrimSpace(values["replicaZone"])
		b["type"] = "regions/" + region + "/diskTypes/" + vpcOr(values["type"], "pd-balanced")
		b["replicaZones"] = []any{"zones/" + zone, "zones/" + replica}
		coll = "regions/" + region + "/disks"
	}
	if s := strings.TrimSpace(values["sizeGb"]); s != "" {
		b["sizeGb"] = s
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	switch values["source"] {
	case "Image":
		img := strings.TrimSpace(values["image"])
		if proj, fam, ok := strings.Cut(img, "/"); ok {
			b["sourceImage"] = "projects/" + proj + "/global/images/family/" + fam
		} else {
			b["sourceImage"] = "global/images/" + img
		}
	case "Snapshot":
		b["sourceSnapshot"] = "global/snapshots/" + strings.TrimSpace(values["snapshot"])
	case "Disk":
		dz, dn, err := splitZoneName(values["sourceDisk"], "source disk")
		if err != nil {
			return "", err
		}
		b["sourceDisk"] = diskPath(dz, dn)
	}
	if sch := values["snapshotSchedule"]; sch != "" && sch != noBackups {
		region, pol, ok := strings.Cut(sch, "/")
		if !ok {
			return "", fmt.Errorf("snapshot schedule: %q is not REGION/NAME", sch)
		}
		b["resourcePolicies"] = []any{"regions/" + region + "/resourcePolicies/" + pol}
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
		b["labels"] = l
	}
	if _, err := p.call("POST", project, coll, nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeDisksProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.DeeperThan(2, path), nil
	}
	d, err := p.call("GET", project, diskPath(path[0], path[1]), nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	var users []string
	for _, u := range vpcStrs(d, "users") {
		users = append(users, vpcLast(u))
	}
	src := vpcLast(vpcStr(d, "sourceImage"))
	if s := vpcStr(d, "sourceSnapshot"); s != "" {
		src = "Snapshot " + vpcLast(s)
	}
	if s := vpcStr(d, "sourceDisk"); s != "" {
		src = "Disk " + vpcLast(s)
	}
	return console.Detail{Title: path[1],
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Status", Value: vpcStr(d, "status")}, {Label: "Type", Value: vpcLast(vpcStr(d, "type"))},
			{Label: "Size", Value: vpcStr(d, "sizeGb") + " GB"}, diskLocationProperty(d),
			{Label: "In use by", Value: vpcDash(strings.Join(users, ", "))}, {Label: "Source", Value: vpcDash(src)},
			{Label: "Snapshot schedule", Value: vpcDash(strings.Join(func() []string {
				var out []string
				for _, r := range vpcStrs(d, "resourcePolicies") {
					out = append(out, vpcLast(r))
				}
				return out
			}(), ", "))},
			{Label: "Labels", Value: vpcDash(vmLabelsText(d))}, {Label: "Description", Value: vpcDash(vpcStr(d, "description"))},
			{Label: "Creation time", Value: vpcStr(d, "creationTimestamp")},
		}}}}},
	}, nil
}

func (p computeDisksProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	d, err := p.call("GET", project, diskPath(path[0], path[1]), nil, nil)
	if err != nil {
		return nil
	}
	return p.diskActions(d, true)
}

func (p computeDisksProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := diskPath(path[0], path[1])
	var err error
	switch action {
	case "snapshot":
		b := map[string]any{"name": strings.TrimSpace(values["name"])}
		if d := strings.TrimSpace(values["description"]); d != "" {
			b["description"] = d
		}
		_, err = p.call("POST", project, base+"/createSnapshot", nil, b)
	case "resize":
		_, err = p.call("POST", project, base+"/resize", nil, map[string]any{"sizeGb": strings.TrimSpace(values["sizeGb"])})
	case "labels":
		err = p.setLabels(project, base, values)
	case "delete":
		_, err = p.call("DELETE", project, base, nil, nil)
	case "clone":
		b := map[string]any{"name": strings.TrimSpace(values["name"]), "sourceDisk": base}
		if d := strings.TrimSpace(values["description"]); d != "" {
			b["description"] = d
		}
		err = p.cloneDisk(project, path, b)
	case "image":
		b := map[string]any{"name": strings.TrimSpace(values["name"]), "sourceDisk": base, "forceCreate": values["force"] == "true"}
		for _, k := range []string{"family", "description"} {
			if v := strings.TrimSpace(values[k]); v != "" {
				b[k] = v
			}
		}
		_, err = p.call("POST", project, "global/images", nil, b)
	case "schedule":
		err = p.setSchedule(project, path, strings.TrimSpace(values["schedule"]))
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
	}
	return err
}

// setSchedule replaces a disk's snapshot schedule.
func (p computeDisksProvider) setSchedule(project string, path []string, want string) error {
	base := diskPath(path[0], path[1])
	d, err := p.call("GET", project, base, nil, nil)
	if err != nil {
		return err
	}
	region := regionOfZoneName(path[0])
	if compute.IsRegion(path[0]) {
		region = path[0]
	}
	wantRel := ""
	if want != "" {
		wantRel = "regions/" + region + "/resourcePolicies/" + want
	}
	var drop []any
	for _, r := range vpcStrs(d, "resourcePolicies") {
		if !strings.HasSuffix(r, "/resourcePolicies/"+want) || want == "" {
			drop = append(drop, r)
		}
	}
	if len(drop) > 0 {
		if _, err := p.call("POST", project, base+"/removeResourcePolicies", nil, map[string]any{"resourcePolicies": drop}); err != nil {
			return err
		}
	}
	if wantRel != "" && len(drop) == len(vpcStrs(d, "resourcePolicies")) {
		_, err = p.call("POST", project, base+"/addResourcePolicies", nil, map[string]any{"resourcePolicies": []any{wantRel}})
	}
	return err
}

// --- Snapshots ----------------------------------------------------------------

// computeSnapshotsProvider is Snapshots: the project's disk snapshots,
// Create snapshot (of a disk, by zone and name), Create disk from one, and
// Delete.
type computeSnapshotsProvider struct{ computeScreen }

func (computeSnapshotsProvider) ID() string    { return "compute-snapshots" }
func (computeSnapshotsProvider) Title() string { return "Snapshots" }

func (p computeSnapshotsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Snapshot size", "Disk size", "Source disk", "Type", "Creation time"}, Noun: "snapshots", AlwaysStatus: true,
		Note: "A snapshot is a full copy of its disk's volume, taken while the disk is detached or its instance runs."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Snapshots are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.list(project, "global/snapshots")
	if err != nil {
		return listing, err
	}
	for _, s := range l {
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(s, "name"), Status: vpcStr(s, "status"),
			Actions: diskSnapshotActions(s),
			Fields: map[string]string{"Location": vpcDash(strings.Join(vpcStrs(s, "storageLocations"), ", ")),
				"Snapshot size": bytesText(vpcStr(s, "storageBytes")), "Disk size": vpcStr(s, "diskSizeGb") + " GB",
				"Source disk": vpcLast(vpcStr(s, "sourceDisk")), "Creation time": vpcStr(s, "creationTimestamp"),
				"Type": map[bool]string{true: "Archive snapshot", false: map[bool]string{true: "Scheduled snapshot", false: "Snapshot"}[s["autoCreated"] == true]}[vpcStr(s, "snapshotType") == "ARCHIVE"]}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func bytesText(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return "0 B"
	}
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 2, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 2, 64) + " MB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// labelsAction is Edit labels on a disk or snapshot.
func labelsAction(o map[string]any) console.Action {
	return console.Action{ID: "labels", Label: "Edit labels", Submit: "Save", Fields: []console.Field{vmLabelsField(vmLabels(o), ""),
		{Name: "labelFingerprint", Type: "hidden", Default: vpcStr(o, "labelFingerprint")}}}
}

// setLabels is setLabels on the resource at base, under its fingerprint.
func (c computeScreen) setLabels(project, base string, values map[string]string) error {
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	fp := values["labelFingerprint"]
	if fp == "" {
		o, err := c.call("GET", project, base, nil, nil)
		if err != nil {
			return err
		}
		fp = vpcStr(o, "labelFingerprint")
	}
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	_, err = c.call("POST", project, base+"/setLabels", nil, map[string]any{"labels": l, "labelFingerprint": fp})
	return err
}

func diskSnapshotActions(s map[string]any) []console.Action {
	return []console.Action{labelsAction(s), {ID: "createdisk", Label: "Create disk", Submit: "Create", Fields: []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		vmZoneField("zone"),
		{Name: "type", Label: "Disk type", Type: "select", Default: "pd-balanced", Options: diskTypeNames()}}}}
}

// CreateForm is Google's Create a snapshot (compute/docs/disks/create-snapshots,
// read 2026-11-22): Name, Description, Source disk, Location
// (Multi-regional or Regional), Snapshot type (Snapshot or Archive
// snapshot) and Labels.
func (p computeSnapshotsProvider) CreateForm() (string, []console.Field) {
	return "Create snapshot", p.snapshotFields(nil)
}

func (p computeSnapshotsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return p.snapshotFields(nil)
	}
	return p.snapshotFields(p.diskNames(project))
}

func (computeSnapshotsProvider) CreateTitle() string { return "Create a snapshot" }

func (p computeSnapshotsProvider) snapshotFields(disks []string) []console.Field {
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		selectOrText(console.Field{Name: "disk", Label: "Source disk", Required: true}, disks, "ZONE/NAME of a disk: none exists yet."),
		{Name: "location", Label: "Location", Type: "radio", Default: "Multi-regional", Options: []string{"Multi-regional", "Regional"},
			Help: "Where the snapshot is kept: the disk's multi-region (us, eu, asia) or its region."},
		{Name: "snapshotType", Label: "Snapshot type", Type: "radio", Default: "Snapshot", Options: []string{"Snapshot", "Archive snapshot"}},
		vmLabelsField(nil, ""),
	}
}

func (p computeSnapshotsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	src := strings.TrimSpace(values["disk"])
	zone, disk, ok := strings.Cut(src, "/")
	if !ok {
		// A disk's name alone, in the zone the form names (the older form).
		zone, disk = strings.TrimSpace(vpcOr(values["zone"], defaultZone)), src
	}
	b["sourceDisk"] = diskPath(zone, disk)
	region := regionOfZoneName(zone)
	if compute.IsRegion(zone) {
		region = zone
	}
	if values["location"] == "Regional" {
		b["storageLocations"] = []any{region}
	} else if values["location"] != "" {
		b["storageLocations"] = []any{strings.SplitN(region, "-", 2)[0]}
	}
	if values["snapshotType"] == "Archive snapshot" {
		b["snapshotType"] = "ARCHIVE"
	}
	if labels, err := console.ParseMap(values["labels"]); err != nil {
		return "", err
	} else if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		b["labels"] = l
	}
	if _, err := p.call("POST", project, "global/snapshots", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeSnapshotsProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.call("DELETE", project, "global/snapshots/"+name, nil, nil)
	return err
}

func (p computeSnapshotsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || p.api() == nil {
		return nil
	}
	s, err := p.call("GET", project, "global/snapshots/"+path[0], nil, nil)
	if err != nil {
		return nil
	}
	return diskSnapshotActions(s)
}

func (p computeSnapshotsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 1 && action == "labels" {
		return p.setLabels(project, "global/snapshots/"+path[0], values)
	}
	if len(path) != 1 || action != "createdisk" {
		return fmt.Errorf("%s is not available here", action)
	}
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	_, err := p.call("POST", project, "zones/"+zone+"/disks", nil, map[string]any{"name": strings.TrimSpace(values["name"]),
		"sourceSnapshot": "global/snapshots/" + path[0], "type": "zones/" + zone + "/diskTypes/" + vpcOr(values["type"], "pd-balanced")})
	return err
}

// --- Images -------------------------------------------------------------------

// computeImagesProvider is Images: the public images served, each from its
// image project, with the public image projects that are refused and why.
type computeImagesProvider struct{ computeScreen }

func (computeImagesProvider) ID() string    { return "compute-images" }
func (computeImagesProvider) Title() string { return "Images" }

// List is Google's Images (compute/docs/images, read 2026-11-22): the
// project's own images beside the public ones, with Location, Family, Disk
// size, Created by and Creation time; a custom image's Create instance,
// Deprecate and Delete.
func (p computeImagesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Family", "Disk size", "Created by", "Creation time"}, Noun: "images", AlwaysStatus: true,
		Note: "The public images served are systemd container images of the distribution, built locally at first use. " +
			"Not served: " + refusedImagesText() + ". A custom image is a copy of its source disk."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project != "" {
		l, err := p.list(project, "global/images")
		if err != nil {
			return listing, err
		}
		for _, im := range l {
			status := vpcStr(im, "status")
			if dep := vpcStr(vmMap(im, "deprecated"), "state"); dep != "" {
				status = dep
			}
			listing.Items = append(listing.Items, console.Resource{Name: vpcStr(im, "name"), Status: status, Actions: customImageActions(im),
				Fields: map[string]string{"Location": strings.Join(vpcStrs(im, "storageLocations"), ", "), "Family": vpcDash(vpcStr(im, "family")),
					"Disk size": vpcStr(im, "diskSizeGb") + " GB", "Created by": project, "Creation time": vpcStr(im, "creationTimestamp")}})
		}
	}
	seen := map[string]bool{}
	for _, pi := range compute.PublicImages {
		if seen[pi.Project] {
			continue
		}
		seen[pi.Project] = true
		l, err := p.call("GET", pi.Project, "global/images", nil, nil)
		if err != nil {
			return listing, err
		}
		for _, im := range computeItems(l) {
			listing.Items = append(listing.Items, console.Resource{Name: vpcStr(im, "name"), Status: vpcStr(im, "status"),
				Fields: map[string]string{"Location": strings.Join(vpcStrs(im, "storageLocations"), ", "), "Family": vpcStr(im, "family"),
					"Disk size": vpcStr(im, "diskSizeGb") + " GB", "Created by": pi.Project, "Creation time": vpcStr(im, "creationTimestamp")}})
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// customImageActions are a custom image's: Edit labels
// (labeling-resources: an image's labels change after it is made, under its
// labelFingerprint), Deprecate (images/deprecate-custom: "Deprecate ...
// select the state ... the replacement image") and Delete.
func customImageActions(im map[string]any) []console.Action {
	return []console.Action{labelsAction(im),
		{ID: "deprecate", Label: "Deprecate", Submit: "Deprecate", Fields: []console.Field{
			{Name: "state", Label: "State", Type: "select", Default: "Deprecated", Options: []string{"Active", "Deprecated", "Obsolete", "Deleted"},
				Help: "Deprecated still makes disks, with a warning; Obsolete and Deleted refuse new disks."},
			{Name: "replacement", Label: "Replacement image", Type: "text", Help: "Optional: the image to use instead."}}},
		{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete image " + vpcStr(im, "name") + "?"},
	}
}

func (computeImagesProvider) CreateOnPage() bool   { return true }
func (computeImagesProvider) CreateTitle() string  { return "Create an image" }
func (computeImagesProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateForm is Google's Create an image (images/create-custom, read
// 2026-11-22): Name, Source (Disk, Snapshot or Image), the source,
// Location, Family, Description and Labels.
func (p computeImagesProvider) CreateForm() (string, []console.Field) {
	return "Create image", p.imageFields(nil, nil, nil)
}

func (p computeImagesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return p.imageFields(nil, nil, nil)
	}
	return p.imageFields(p.diskNames(project), p.snapshotNames(project), p.customImageNames(project))
}

func (p computeImagesProvider) imageFields(disks, snapshots, images []string) []console.Field {
	imgs := append(imageFamilies(), images...)
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "source", Label: "Source", Type: "select", Default: "Disk", Options: []string{"Disk", "Snapshot", "Image", "Cloud Storage file", "Virtual disk (VMDK, VHD)"},
			DisabledOptions: []string{"Cloud Storage file", "Virtual disk (VMDK, VHD)"},
			Help:            "Importing a Cloud Storage tarball or a virtual disk is not served: make the image from a disk, a snapshot or an image."},
		selectOrText(console.Field{Name: "disk", Label: "Source disk", Required: true, VisibleWhen: map[string]string{"source": "Disk"}}, disks, "ZONE/NAME of a disk: none exists yet."),
		{Name: "force", Label: "Keep instance running (not recommended)", Type: "checkbox", VisibleWhen: map[string]string{"source": "Disk"}},
		selectOrText(console.Field{Name: "snapshot", Label: "Source snapshot", Required: true, VisibleWhen: map[string]string{"source": "Snapshot"}}, snapshots, "A snapshot's name: none exists yet."),
		{Name: "image", Label: "Source image", Type: "select", Default: "debian-cloud/debian-12", Options: imgs, VisibleWhen: map[string]string{"source": "Image"}},
		{Name: "location", Label: "Location", Type: "radio", Default: "Multi-regional", Options: []string{"Multi-regional", "Regional"}},
		{Name: "locationName", Label: "Select location", Type: "select", Default: "us", Options: append([]string{"us", "eu", "asia"}, compute.Regions()...)},
		{Name: "family", Label: "Family", Type: "text", Pattern: `^$|` + computeNamePattern, Help: "Optional: Create an instance can take the family's newest image."},
		{Name: "description", Label: "Description", Type: "textarea"},
		vmLabelsField(nil, ""),
	}
}

func (p computeImagesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name}
	switch values["source"] {
	case "", "Disk":
		zone, disk, err := splitZoneName(values["disk"], "source disk")
		if err != nil {
			return "", err
		}
		b["sourceDisk"] = diskPath(zone, disk)
		b["forceCreate"] = values["force"] == "true"
	case "Snapshot":
		b["sourceSnapshot"] = "global/snapshots/" + strings.TrimSpace(values["snapshot"])
	case "Image":
		img := strings.TrimSpace(values["image"])
		if proj, fam, ok := strings.Cut(img, "/"); ok {
			b["sourceImage"] = "projects/" + proj + "/global/images/family/" + fam
		} else {
			b["sourceImage"] = "global/images/" + img
		}
	default:
		return "", fmt.Errorf("source: %s is not served", values["source"])
	}
	if loc := strings.TrimSpace(values["locationName"]); loc != "" {
		b["storageLocations"] = []any{loc}
	}
	for _, k := range []string{"family", "description"} {
		if v := strings.TrimSpace(values[k]); v != "" {
			b[k] = v
		}
	}
	if labels, err := console.ParseMap(values["labels"]); err != nil {
		return "", err
	} else if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		b["labels"] = l
	}
	if _, err := p.call("POST", project, "global/images", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeImagesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || p.api() == nil {
		return nil
	}
	im, err := p.call("GET", project, "global/images/"+path[0], nil, nil)
	if err != nil {
		return nil
	}
	return customImageActions(im)
}

func (p computeImagesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "global/images/" + path[0]
	switch action {
	case "labels":
		return p.setLabels(project, base, values)
	case "delete":
		_, err := p.call("DELETE", project, base, nil, nil)
		return err
	case "deprecate":
		b := map[string]any{"state": strings.ToUpper(vpcOr(values["state"], "Deprecated"))}
		if r := strings.TrimSpace(values["replacement"]); r != "" {
			b["replacement"] = "global/images/" + r
		}
		_, err := p.call("POST", project, base+"/deprecate", nil, b)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

func refusedImagesText() string {
	var out []string
	for _, r := range compute.RefusedImageProjects() {
		out = append(out, r[0]+" ("+r[1]+")")
	}
	return strings.Join(out, "; ")
}

// --- Machine types ------------------------------------------------------------

// computeMachineTypesProvider is Machine types: the predefined types every
// zone offers here, with their vCPUs and memory.
type computeMachineTypesProvider struct{ computeScreen }

func (computeMachineTypesProvider) ID() string    { return "compute-machinetypes" }
func (computeMachineTypesProvider) Title() string { return "Machine types" }

func (p computeMachineTypesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Series", "vCPUs", "Memory", "Shared core", "Description"}, Noun: "machine types",
		Note: "Every zone offers these types; a VM's container gets the type's vCPUs and memory as its limits."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Machine types are listed per project and zone. Choose a project in the toolbar."
		return listing, nil
	}
	l, err := p.list(project, "zones/"+defaultZone+"/machineTypes")
	if err != nil {
		return listing, err
	}
	for _, m := range l {
		name := vpcStr(m, "name")
		mem, _ := strconv.ParseFloat(vpcStr(m, "memoryMb"), 64)
		listing.Items = append(listing.Items, console.Resource{Name: name, Fields: map[string]string{
			"Series": strings.ToUpper(strings.SplitN(name, "-", 2)[0]), "vCPUs": vpcStr(m, "guestCpus"),
			"Memory": strconv.FormatFloat(mem/1024, 'f', -1, 64) + " GB", "Shared core": map[bool]string{true: "Yes", false: "No"}[m["isSharedCpu"] == true],
			"Description": vpcStr(m, "description")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// --- Metadata -----------------------------------------------------------------

// computeMetadataProvider is Metadata (Compute Engine > Settings >
// Metadata): the project's common instance metadata, which every VM reads
// (ssh-keys included, as gcloud compute ssh adds them), with Edit.
type computeMetadataProvider struct{ computeScreen }

func (computeMetadataProvider) ID() string    { return "compute-metadata" }
func (computeMetadataProvider) Title() string { return "Metadata" }

func (p computeMetadataProvider) project(project string) (map[string]any, []map[string]string, error) {
	o, err := p.call("GET", project, "", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return o, vmMetadata(map[string]any{"metadata": vmMap(o, "commonInstanceMetadata")}), nil
}

func (p computeMetadataProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Value"}, Noun: "metadata items", NameColumn: "Key",
		Note: "Project metadata applies to every VM of the project; an instance's own value of a key wins. ssh-keys here are applied by every guest within seconds."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Metadata is set per project. Choose one in the toolbar."
		return listing, nil
	}
	_, rows, err := p.project(project)
	if err != nil {
		return listing, err
	}
	for _, r := range rows {
		v := r["value"]
		if len(v) > 200 {
			v = v[:200] + "…"
		}
		listing.Items = append(listing.Items, console.Resource{Name: r["key"], Fields: map[string]string{"Value": v}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeMetadataProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	o, rows, err := p.project(project)
	if err != nil {
		return nil
	}
	return []console.Action{{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
		{Name: "metadata", Label: "Metadata", Type: console.GroupFieldType, AddLabel: "Add item", Default: console.FormatGroup(rows), Group: []console.Field{
			{Name: "key", Label: "Key", Type: "text", Required: true},
			{Name: "value", Label: "Value", Type: "textarea"},
		}},
		{Name: "fingerprint", Type: "hidden", Default: vpcStr(vmMap(o, "commonInstanceMetadata"), "fingerprint")}}}}
}

func (p computeMetadataProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	return nil
}

func (p computeMetadataProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 0 || action != "edit" {
		return fmt.Errorf("%s is not available here", action)
	}
	items, err := metadataBody(values["metadata"], "")
	if err != nil {
		return err
	}
	_, err = p.call("POST", project, "setCommonInstanceMetadata", nil, map[string]any{"items": items, "fingerprint": values["fingerprint"]})
	return err
}
