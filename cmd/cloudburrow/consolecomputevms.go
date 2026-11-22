package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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

const vmNote = "Each VM runs as a systemd container in the instance's Kubernetes cluster. Its natIP is 127.0.0.1 and its port 22 is " +
	"forwarded to the loopback port the Connect column names, so gcloud compute ssh takes --ssh-flag=\"-p PORT\". " +
	"Firewall rules are not enforced between VMs."

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
	listing := console.Listing{Columns: []string{"Zone", "Machine type", "Internal IP", "External IP", "Connect"},
		Noun: "VM instances", Note: vmNote, AlwaysStatus: true, PermissionsButton: true}
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
	for _, o := range l {
		zone := zoneOf(vpcStr(o, "zone"))
		target := []string{zone, vpcStr(o, "name")}
		connect := "—"
		if port := vmSSHPort(o); port != "" {
			connect = "SSH (port " + port + ")"
		}
		ext := vmExternalIP(o)
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Status: vpcStr(o, "status"),
			Opens: target, Target: target, Actions: p.instanceActions(o, false), Info: computePermissionsTab(),
			Fields: map[string]string{"Zone": zone, "Machine type": vpcLast(vpcStr(o, "machineType")),
				"Internal IP": vpcDash(vmInternalIP(o)), "External IP": vpcDash(ext), "Connect": connect}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

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
		acts = append(acts, console.Action{ID: "stop", Label: "Stop", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Stop " + name + "? Its processes end; its disks keep what they hold."})
	}
	if status == "RUNNING" {
		acts = append(acts,
			console.Action{ID: "suspend", Label: "Suspend", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Suspend " + name + "? Its processes are frozen until it is resumed."},
			console.Action{ID: "reset", Label: "Reset", ConfirmStyle: console.DeleteConfirmClick, Confirm: "Reset " + name + "? It restarts at once, without shutting down cleanly."})
	}
	if onPage {
		acts = append(acts, p.diskActions(o)...)
	}
	return append(acts, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete VM instance " + name + "? Its boot disk is deleted with it when auto-delete is on."})
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

func (p computeInstancesProvider) CreateForm() (string, []console.Field) {
	external := map[string]string{"externalIP": "Ephemeral"}
	return "Create instance", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		vmLabelsField(nil, ""),
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions()},
		vmZoneField("zone"),
		{Name: "machineType", Label: "Machine type", Type: "select", Section: "Machine configuration", Required: true, Default: "e2-medium",
			Options: machineTypeNames(), Help: "The VM's container gets the type's vCPUs and memory as its limits."},
		{Name: "image", Label: "Image", Type: "select", Section: "Boot disk", Required: true, Default: "debian-cloud/debian-12",
			Options: imageFamilies(), OptionLabels: imageFamilyLabels(),
			Help: "Container-Optimized OS, Windows and licensed images are not served: see the Images page."},
		{Name: "bootDiskType", Label: "Boot disk type", Type: "select", Section: "Boot disk", Default: "pd-balanced", Options: diskTypeNames()},
		{Name: "bootDiskSize", Label: "Size (GB)", Type: "text", Section: "Boot disk", Default: "10", Pattern: `^[0-9]{1,5}$`},
		{Name: "network", Label: "Network", Type: "text", Section: "Networking", Required: true, Default: "default"},
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Section: "Networking",
			Help: "Optional: a subnet of the network in the instance's region; empty uses the network's subnet there."},
		{Name: "externalIP", Label: "External IPv4 address", Type: "select", Section: "Networking", Default: "Ephemeral", Options: []string{"Ephemeral", "None"},
			Help: "Ephemeral gives the instance the natIP 127.0.0.1, with its port 22 forwarded to a loopback port."},
		{Name: "networkTags", Label: "Network tags", Type: "text", Section: "Networking", VisibleWhen: external, Help: "Comma-separated, for example http-server, https-server."},
		{Name: "serviceAccount", Label: "Service account", Type: "select", Section: "Identity and API access", Default: "Compute Engine default service account",
			Options: []string{"Compute Engine default service account", "No service account"}},
		{Name: "scopes", Label: "Access scopes", Type: "radio", Section: "Identity and API access", Default: "Allow default access",
			Options: []string{"Allow default access", "Allow full access to all Cloud APIs"}},
		{Name: "startupScript", Label: "Startup script", Type: "textarea", Section: "Management",
			Help: "Runs as root at every boot (the startup-script metadata key); its output is on the serial port."},
		{Name: "metadata", Label: "Metadata", Type: console.GroupFieldType, Section: "Management", AddLabel: "Add item", Group: []console.Field{
			{Name: "key", Label: "Key", Type: "text", Required: true},
			{Name: "value", Label: "Value", Type: "textarea"},
		}},
		{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Section: "Management"},
	}
}

var defaultScopes = []any{
	"https://www.googleapis.com/auth/devstorage.read_only", "https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/monitoring.write", "https://www.googleapis.com/auth/service.management.readonly",
	"https://www.googleapis.com/auth/servicecontrol", "https://www.googleapis.com/auth/trace.append",
}

func (p computeInstancesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if !compute.IsZone(zone) {
		return "", fmt.Errorf("zone: %q is not a zone", zone)
	}
	size, err := strconv.Atoi(strings.TrimSpace(vpcOr(values["bootDiskSize"], "10")))
	if err != nil {
		return "", fmt.Errorf("boot disk size: %q is not a number", values["bootDiskSize"])
	}
	img := strings.TrimSpace(vpcOr(values["image"], "debian-cloud/debian-12"))
	proj, fam, ok := strings.Cut(img, "/")
	if !ok {
		return "", fmt.Errorf("image: %q is not project/family", img)
	}
	nic := map[string]any{"network": "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default"))}
	if s := strings.TrimSpace(values["subnetwork"]); s != "" {
		nic["subnetwork"] = "regions/" + zone[:strings.LastIndex(zone, "-")] + "/subnetworks/" + s
	}
	if values["externalIP"] != "None" {
		nic["accessConfigs"] = []any{map[string]any{"name": "External NAT", "type": "ONE_TO_ONE_NAT", "networkTier": "PREMIUM"}}
	}
	body := map[string]any{"name": name, "machineType": "zones/" + zone + "/machineTypes/" + strings.TrimSpace(vpcOr(values["machineType"], "e2-medium")),
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "deviceName": name, "initializeParams": map[string]any{
			"sourceImage": "projects/" + proj + "/global/images/family/" + fam, "diskSizeGb": strconv.Itoa(size),
			"diskType": "zones/" + zone + "/diskTypes/" + vpcOr(values["bootDiskType"], "pd-balanced")}}},
		"networkInterfaces": []any{nic}, "deletionProtection": values["deletionProtection"] == "true"}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		body["labels"] = l
	}
	if tags := vpcList(values["networkTags"]); len(tags) > 0 {
		body["tags"] = map[string]any{"items": tags}
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
		return "", err
	}
	if len(items) > 0 {
		body["metadata"] = map[string]any{"items": items}
	}
	if _, err := p.call("POST", project, "zones/"+zone+"/instances", nil, body); err != nil {
		return "", err
	}
	return name, nil
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
	nics := console.Listing{Columns: []string{"Network", "Subnetwork", "Primary internal IP", "External IP", "Network tier"}, Noun: "network interfaces"}
	for _, n := range vmMaps(o, "networkInterfaces") {
		tier, ext := "—", "None"
		for _, ac := range vmMaps(n, "accessConfigs") {
			tier, ext = vpcStr(ac, "networkTier"), vpcDash(vpcStr(ac, "natIP"))
		}
		nics.Items = append(nics.Items, console.Resource{Name: vpcStr(n, "name"), Fields: map[string]string{"Network": vpcLast(vpcStr(n, "network")),
			"Subnetwork": vpcLast(vpcStr(n, "subnetwork")), "Primary internal IP": vpcStr(n, "networkIP"), "External IP": ext, "Network tier": tier}})
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
		Edit: &console.EditForm{Label: "Edit", Note: "The machine type changes only while the instance is stopped.", Fields: []console.Field{
			vmLabelsField(vmLabels(o), ""),
			{Name: "networkTags", Label: "Network tags", Type: "text", Default: tags},
			{Name: "machineType", Label: "Machine type", Type: "select", Default: vpcLast(vpcStr(o, "machineType")), Options: machineTypeNames()},
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
		"gcloud compute ssh " + name + " --zone " + zone + " --project " + project + " --ssh-flag=\"-p " + port + "\""
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
	case "stop", "suspend", "reset":
		_, err = p.call("POST", project, base+"/"+action, nil, nil)
	case "delete":
		_, err = p.call("DELETE", project, base, nil, nil)
	case "attachdisk":
		d := strings.TrimSpace(values["disk"])
		b := map[string]any{"source": "zones/" + path[0] + "/disks/" + d, "deviceName": vpcOr(strings.TrimSpace(values["deviceName"]), d)}
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

// --- Disks --------------------------------------------------------------------

// computeDisksProvider is Disks: every disk of the project across zones;
// Create disk on its own page (blank, from an image or from a snapshot);
// a disk's page at [zone, name] with Create snapshot, Edit (its size) and
// Delete.
type computeDisksProvider struct{ computeScreen }

func (computeDisksProvider) ID() string    { return "compute-disks" }
func (computeDisksProvider) Title() string { return "Disks" }

func (p computeDisksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Size", "Zone", "In use by"}, Noun: "disks", AlwaysStatus: true, InfoOnSelect: true,
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
		zone := zoneOf(vpcStr(d, "zone"))
		target := []string{zone, vpcStr(d, "name")}
		var users []string
		for _, u := range vpcStrs(d, "users") {
			users = append(users, vpcLast(u))
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(d, "name"), Status: vpcStr(d, "status"), Opens: target, Target: target,
			Actions: p.diskActions(d, false), Info: computePermissionsTab(),
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
	acts = append(acts, labelsAction(d))
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, Confirm: "Delete disk " + name + "?"}
	if users := vpcStrs(d, "users"); len(users) > 0 {
		del.Unavailable = "The disk is attached to " + vpcLast(users[0]) + ": detach it first."
	}
	return append(acts, del)
}

func (computeDisksProvider) CreateOnPage() bool   { return true }
func (computeDisksProvider) CreateTitle() string  { return "Create a disk" }
func (computeDisksProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p computeDisksProvider) CreateForm() (string, []console.Field) {
	return "Create disk", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		vmZoneField("zone"),
		{Name: "source", Label: "Disk source type", Type: "radio", Section: "Disk settings", Default: "Blank disk", Options: []string{"Blank disk", "Image", "Snapshot"}},
		{Name: "image", Label: "Source image", Type: "select", Section: "Disk settings", Default: "debian-cloud/debian-12", Options: imageFamilies(),
			OptionLabels: imageFamilyLabels(), VisibleWhen: map[string]string{"source": "Image"}},
		{Name: "snapshot", Label: "Source snapshot", Type: "text", Section: "Disk settings", VisibleWhen: map[string]string{"source": "Snapshot"},
			Help: "The name of a snapshot on the Snapshots page."},
		{Name: "type", Label: "Disk type", Type: "select", Section: "Disk settings", Default: "pd-balanced", Options: diskTypeNames()},
		{Name: "sizeGb", Label: "Size (GB)", Type: "text", Section: "Disk settings", Default: "10", Pattern: `^[0-9]{1,5}$`},
		vmLabelsField(nil, "Disk settings"),
	}
}

func (p computeDisksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	if !compute.IsZone(zone) {
		return "", fmt.Errorf("zone: %q is not a zone", zone)
	}
	b := map[string]any{"name": name, "type": "zones/" + zone + "/diskTypes/" + vpcOr(values["type"], "pd-balanced")}
	if s := strings.TrimSpace(values["sizeGb"]); s != "" {
		b["sizeGb"] = s
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	switch values["source"] {
	case "Image":
		proj, fam, _ := strings.Cut(values["image"], "/")
		b["sourceImage"] = "projects/" + proj + "/global/images/family/" + fam
	case "Snapshot":
		b["sourceSnapshot"] = "global/snapshots/" + strings.TrimSpace(values["snapshot"])
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
	if _, err := p.call("POST", project, "zones/"+zone+"/disks", nil, b); err != nil {
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
	d, err := p.call("GET", project, "zones/"+path[0]+"/disks/"+path[1], nil, nil)
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
	return console.Detail{Title: path[1],
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Status", Value: vpcStr(d, "status")}, {Label: "Type", Value: vpcLast(vpcStr(d, "type"))},
			{Label: "Size", Value: vpcStr(d, "sizeGb") + " GB"}, {Label: "Zone", Value: path[0]},
			{Label: "In use by", Value: vpcDash(strings.Join(users, ", "))}, {Label: "Source", Value: vpcDash(src)},
			{Label: "Labels", Value: vpcDash(vmLabelsText(d))}, {Label: "Description", Value: vpcDash(vpcStr(d, "description"))},
			{Label: "Creation time", Value: vpcStr(d, "creationTimestamp")},
		}}}}},
	}, nil
}

func (p computeDisksProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	d, err := p.call("GET", project, "zones/"+path[0]+"/disks/"+path[1], nil, nil)
	if err != nil {
		return nil
	}
	return p.diskActions(d, true)
}

func (p computeDisksProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "zones/" + path[0] + "/disks/" + path[1]
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
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
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
	listing := console.Listing{Columns: []string{"Location", "Snapshot size", "Disk size", "Source disk", "Creation time"}, Noun: "snapshots", AlwaysStatus: true,
		InfoOnSelect: true,
		Note:         "A snapshot is a full copy of its disk's volume, taken while the disk is detached or its instance runs."}
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
			Actions: diskSnapshotActions(s), Info: computePermissionsTab(),
			Fields: map[string]string{"Location": vpcDash(strings.Join(vpcStrs(s, "storageLocations"), ", ")),
				"Snapshot size": bytesText(vpcStr(s, "storageBytes")), "Disk size": vpcStr(s, "diskSizeGb") + " GB",
				"Source disk": vpcLast(vpcStr(s, "sourceDisk")), "Creation time": vpcStr(s, "creationTimestamp")}})
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

func (computeSnapshotsProvider) CreateForm() (string, []console.Field) {
	return "Create snapshot", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		vmZoneField("zone"),
		{Name: "disk", Label: "Source disk", Type: "text", Required: true, Help: "The name of a disk in the zone."},
	}
}

func (p computeSnapshotsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	zone := strings.TrimSpace(vpcOr(values["zone"], defaultZone))
	b["sourceDisk"] = "zones/" + zone + "/disks/" + strings.TrimSpace(values["disk"])
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

func (p computeImagesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Family", "Project", "Disk size", "Description"}, Noun: "images", AlwaysStatus: true,
		Note: "These are the public images served, each a systemd container image of the distribution built locally at first use. " +
			"Not served: " + refusedImagesText() + ". Custom images (images.insert) are not served."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
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
				Fields: map[string]string{"Family": vpcStr(im, "family"), "Project": pi.Project, "Disk size": vpcStr(im, "diskSizeGb") + " GB",
					"Description": vpcStr(im, "description")}})
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
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
