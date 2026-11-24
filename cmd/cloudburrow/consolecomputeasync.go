package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Asynchronous Replication and disk consistency groups in the console
// (#2206), at Google's pages and with Google's words
// (https://docs.cloud.google.com/compute/docs/disks/async-pd/configure-async-pd,
// manage-replication, manage-consistency-groups, read 2026-11-24): a
// disk's page has Create secondary disk ("Compute Engine creates the disk
// and starts replication"), a secondary disk's Start replication, and
// Terminate replication on either disk of an active pair; the
// Asynchronous replication page lists the secondary disks with their
// primaries and states; Consistency groups has Create consistency group
// (name and region), a group's page with its members, Assign disks,
// Remove disks, Terminate replication, Clone consistency group and Delete.

// diskPathOf is a disk link's page path: [zone, name] or [region, name].
func diskPathOf(link string) []string {
	parts := strings.Split(link, "/")
	for i := 0; i+3 < len(parts); i++ {
		if (parts[i] == "zones" || parts[i] == "regions") && parts[i+2] == "disks" {
			return []string{parts[i+1], parts[i+3]}
		}
	}
	return nil
}

// diskLoc is a disk path's location, its replica zones for a regional disk.
func diskLoc(d map[string]any) string {
	if r := vpcStr(d, "region"); r != "" {
		var zs []string
		for _, z := range vpcStrs(d, "replicaZones") {
			zs = append(zs, vpcLast(z))
		}
		return vpcLast(r) + " (" + strings.Join(zs, ", ") + ")"
	}
	return vpcLast(vpcStr(d, "zone"))
}

// secondaryState is a secondary disk's replication state, "" for a disk
// that is not a secondary.
func secondaryState(d map[string]any) string {
	if vmMap(d, "asyncPrimaryDisk") == nil {
		return ""
	}
	if s := vpcStr(vmMap(vmMap(d, "resourceStatus"), "asyncPrimaryDisk"), "state"); s != "" {
		return s
	}
	return "CREATED"
}

// activeSecondary is a primary disk's actively replicating secondary.
func activeSecondary(d map[string]any) string {
	m := vmMap(vmMap(d, "resourceStatus"), "asyncSecondaryDisks")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if e, _ := m[k].(map[string]any); vpcStr(e, "state") == "ACTIVE" {
			return k
		}
	}
	return ""
}

// asyncActions are a disk's Asynchronous Replication actions.
func asyncActions(d map[string]any) []console.Action {
	var acts []console.Action
	if vmMap(d, "asyncPrimaryDisk") == nil {
		act := console.Action{ID: "secondary", Label: "Create secondary disk", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
			{Name: "location", Label: "Location", Type: "radio", Default: "Regional", Options: []string{"Regional", "Single zone"},
				Help: "The secondary disk is in another region of the primary's continent, of the primary's size and type."},
			{Name: "zone", Label: "Zone", Type: "select", Required: true, Default: "us-east1-b", Options: vmZones()},
			{Name: "replicaZone", Label: "Replica zone", Type: "select", Options: vmZones(), Default: "us-east1-c",
				VisibleWhen: map[string]string{"location": "Regional"}, Help: "The second zone, in the zone's region."}}}
		if activeSecondary(d) != "" {
			act.Unavailable = "The disk already replicates to " + vpcLast(activeSecondary(d)) + ": a primary disk has one secondary disk at a time."
		}
		acts = append(acts, act)
	}
	switch secondaryState(d) {
	case "CREATED":
		acts = append(acts, console.Action{ID: "startreplication", Label: "Start replication", Submit: "Start replication",
			Confirm: "Start replication from " + vpcLast(vpcStr(vmMap(d, "asyncPrimaryDisk"), "disk")) + " to " + vpcStr(d, "name") + "?"})
	case "ACTIVE":
		acts = append(acts, terminateAction(vpcStr(d, "name")))
	}
	if activeSecondary(d) != "" {
		acts = append(acts, terminateAction(vpcStr(d, "name")))
	}
	return acts
}

func terminateAction(name string) console.Action {
	return console.Action{ID: "terminatereplication", Label: "Terminate replication", Destructive: true, ConfirmTitle: "Terminate replication",
		Confirm: "Replication of " + name + " stops, and cannot be restarted: a new secondary disk is needed to replicate again."}
}

// asyncProperties are a disk page's Asynchronous replication rows.
func asyncProperties(d map[string]any) []console.Property {
	var props []console.Property
	if ap := vmMap(d, "asyncPrimaryDisk"); ap != nil {
		props = append(props, console.Property{Label: "Primary disk", Value: vpcLast(vpcStr(ap, "disk"))},
			console.Property{Label: "Replication state", Value: secondaryState(d)})
	}
	m := vmMap(vmMap(d, "resourceStatus"), "asyncSecondaryDisks")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e, _ := m[k].(map[string]any)
		props = append(props, console.Property{Label: "Secondary disk", Value: vpcLast(k) + " (" + vpcStr(e, "state") + ")"})
	}
	if pols := vpcStrs(d, "resourcePolicies"); len(pols) > 0 {
		props = append(props, console.Property{Label: "Consistency group", Value: vpcLast(pols[0])})
	}
	return props
}

// asyncAct performs a disk's replication action, false when action is
// not one.
func (c computeScreen) asyncAct(project string, path []string, action string, values map[string]string) (bool, error) {
	base := diskBase(path)
	switch action {
	case "secondary":
		d, err := c.call("GET", project, base, nil, nil)
		if err != nil {
			return true, err
		}
		return true, c.createSecondary(project, d, values)
	case "startreplication":
		d, err := c.call("GET", project, base, nil, nil)
		if err != nil {
			return true, err
		}
		pp := diskPathOf(vpcStr(vmMap(d, "asyncPrimaryDisk"), "disk"))
		if pp == nil {
			return true, errors.New("the disk is not a secondary disk")
		}
		_, err = c.call("POST", project, diskBase(pp)+"/startAsyncReplication", nil, map[string]any{"asyncSecondaryDisk": vpcStr(d, "selfLink")})
		return true, err
	case "terminatereplication":
		_, err := c.call("POST", project, base+"/stopAsyncReplication", nil, map[string]any{})
		return true, err
	}
	return false, nil
}

// createSecondary makes primary's secondary disk, blank and of its size and
// type, and starts replication to it once it is ready, as Google's Create
// secondary disk does.
func (c computeScreen) createSecondary(project string, primary map[string]any, values map[string]string) error {
	name := strings.TrimSpace(values["name"])
	zone := strings.TrimSpace(values["zone"])
	if !compute.IsZone(zone) {
		return fmt.Errorf("zone: %q is not a zone", zone)
	}
	region := regionOfZone(zone)
	typ := vpcLast(vpcStr(primary, "type"))
	b := map[string]any{"name": name, "sizeGb": vpcStr(primary, "sizeGb"),
		"asyncPrimaryDisk": map[string]any{"disk": vpcStr(primary, "selfLink")}}
	coll := "zones/" + zone + "/disks"
	b["type"] = "zones/" + zone + "/diskTypes/" + typ
	if values["location"] != "Single zone" {
		rz := strings.TrimSpace(values["replicaZone"])
		b["replicaZones"] = []any{"zones/" + zone, "zones/" + rz}
		b["type"] = "regions/" + region + "/diskTypes/" + typ
		coll = "regions/" + region + "/disks"
	}
	if _, err := c.call("POST", project, coll, nil, b); err != nil {
		return err
	}
	rel := coll + "/" + name
	deadline := time.Now().Add(2 * time.Minute)
	for {
		d, err := c.call("GET", project, rel, nil, nil)
		if err == nil && vpcStr(d, "status") == "READY" {
			_, err = c.call("POST", project, diskBase(diskPathOf(vpcStr(primary, "selfLink")))+"/startAsyncReplication", nil,
				map[string]any{"asyncSecondaryDisk": vpcStr(d, "selfLink")})
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the secondary disk %s was made but is not ready; start replication on its page", name)
		}
		time.Sleep(time.Second)
	}
}

// --- Asynchronous replication -------------------------------------------------

// computeAsyncReplicationProvider is Asynchronous replication: the
// project's secondary disks, each with its primary and the pair's state,
// opening the secondary disk's page.
type computeAsyncReplicationProvider struct{ computeScreen }

func (computeAsyncReplicationProvider) ID() string    { return "compute-asyncreplication" }
func (computeAsyncReplicationProvider) Title() string { return "Asynchronous replication" }

func (p computeAsyncReplicationProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Primary disk", "Primary location", "Secondary location", "Replication state"},
		Noun: "secondary disks", InfoOnSelect: true,
		Note: "A secondary disk takes its primary's data when replication starts, before it is read and when replication stops."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Secondary disks are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/disks", nil, nil)
	if err != nil {
		return listing, err
	}
	byLink := map[string]map[string]any{}
	all := aggregatedItems(m, "disks")
	for _, d := range all {
		byLink[vpcStr(d, "selfLink")] = d
	}
	for _, d := range all {
		ap := vmMap(d, "asyncPrimaryDisk")
		if ap == nil {
			continue
		}
		path := diskPathOf(vpcStr(d, "selfLink"))
		ploc := "—"
		if pd := byLink[vpcStr(ap, "disk")]; pd != nil {
			ploc = diskLoc(pd)
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(d, "name"), Opens: path, Target: path,
			Actions: asyncActions(d), Info: computePermissionsTab(),
			Fields: map[string]string{"Primary disk": vpcLast(vpcStr(ap, "disk")), "Primary location": ploc,
				"Secondary location": diskLoc(d), "Replication state": secondaryState(d)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeAsyncReplicationProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	return computeDisksProvider(p).Detail(ctx, project, path)
}

func (p computeAsyncReplicationProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	d, err := p.call("GET", project, diskBase(path), nil, nil)
	if err != nil {
		return nil
	}
	return asyncActions(d)
}

func (p computeAsyncReplicationProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	if ok, err := p.asyncAct(project, path, action, values); ok {
		return err
	}
	return fmt.Errorf("%s is not available on this resource", action)
}

// --- Consistency groups -------------------------------------------------------

// computeConsistencyGroupsProvider is Consistency groups: the project's
// disk consistency groups (resource policies with
// diskConsistencyGroupPolicy) at [region, name].
type computeConsistencyGroupsProvider struct{ computeScreen }

func (computeConsistencyGroupsProvider) ID() string    { return "compute-consistencygroups" }
func (computeConsistencyGroupsProvider) Title() string { return "Consistency groups" }

// members are the disks of the group whose link is pol.
func (p computeConsistencyGroupsProvider) members(project, pol string) ([]map[string]any, []map[string]any, error) {
	m, err := p.call("GET", project, "aggregated/disks", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var in, all []map[string]any
	for _, d := range aggregatedItems(m, "disks") {
		all = append(all, d)
		for _, rp := range vpcStrs(d, "resourcePolicies") {
			if rp == pol {
				in = append(in, d)
			}
		}
	}
	return in, all, nil
}

func (p computeConsistencyGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Disks", "Creation time"}, Noun: "consistency groups", InfoOnSelect: true,
		Note: "A consistency group holds up to 128 disks of one region: primary disks or secondary disks, not both."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Consistency groups are listed per project. Choose one in the toolbar."
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
		if vmMap(pol, "diskConsistencyGroupPolicy") == nil {
			continue
		}
		path := []string{vpcLast(vpcStr(pol, "region")), vpcStr(pol, "name")}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(pol, "name"), Opens: path, Target: path,
			Actions: p.groupActions(pol, nil, nil, false), Info: computePermissionsTab(),
			Fields: map[string]string{"Region": path[0], "Disks": fmt.Sprint(count[vpcStr(pol, "selfLink")]),
				"Creation time": vpcStr(pol, "creationTimestamp")}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeConsistencyGroupsProvider) CreateForm() (string, []console.Field) {
	regions := compute.Regions()
	return "Create consistency group", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: regions,
			Help: "The primary region for a group of primary disks, the secondary region for a group of secondary disks."},
		{Name: "description", Label: "Description", Type: "textarea"},
	}
}

func (p computeConsistencyGroupsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := strings.TrimSpace(values["region"])
	if !compute.IsRegion(region) {
		return "", fmt.Errorf("region: %q is not a region", region)
	}
	b := map[string]any{"name": name, "diskConsistencyGroupPolicy": map[string]any{}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if _, err := p.call("POST", project, "regions/"+region+"/resourcePolicies", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

// groupActions are a group's actions; in and all, when known, are its
// member disks and every disk, for Assign disks' and Remove disks' choices.
func (p computeConsistencyGroupsProvider) groupActions(pol map[string]any, in, all []map[string]any, onPage bool) []console.Action {
	region := vpcLast(vpcStr(pol, "region"))
	var acts []console.Action
	if onPage {
		var assign, remove []string
		labels := map[string]string{}
		member := map[string]bool{}
		for _, d := range in {
			member[vpcStr(d, "selfLink")] = true
			remove = append(remove, diskBase(diskPathOf(vpcStr(d, "selfLink"))))
		}
		for _, d := range all {
			loc := vpcLast(vpcStr(d, "region"))
			if loc == "" {
				loc = regionOfZone(vpcLast(vpcStr(d, "zone")))
			}
			b := diskBase(diskPathOf(vpcStr(d, "selfLink")))
			labels[b] = vpcStr(d, "name") + " (" + diskLoc(d) + ")"
			if loc == region && !member[vpcStr(d, "selfLink")] && len(vpcStrs(d, "resourcePolicies")) == 0 {
				assign = append(assign, b)
			}
		}
		assignAct := console.Action{ID: "assign", Label: "Assign disks", Submit: "Assign disks", Fields: []console.Field{
			{Name: "disk", Label: "Disk", Type: "select", Required: true, Options: assign, OptionLabels: labels,
				Help: "A disk of " + region + " in no other consistency group."}}}
		if len(assign) == 0 {
			assignAct.Unavailable = "No disk of " + region + " is outside a consistency group."
		}
		removeAct := console.Action{ID: "remove", Label: "Remove disks", Submit: "Remove", Fields: []console.Field{
			{Name: "disk", Label: "Disk", Type: "select", Required: true, Options: remove, OptionLabels: labels}}}
		if len(remove) == 0 {
			removeAct.Unavailable = "The consistency group has no disks."
		}
		acts = append(acts, assignAct, removeAct)
	}
	acts = append(acts,
		console.Action{ID: "terminatereplication", Label: "Terminate replication", Destructive: true, ConfirmTitle: "Terminate replication",
			Confirm: "Replication of every disk of " + vpcStr(pol, "name") + " stops, and cannot be restarted."},
		console.Action{ID: "clone", Label: "Clone consistency group", Submit: "Clone consistency group",
			Confirm: "Each secondary disk of " + vpcStr(pol, "name") + " is cloned, as NAME-clone, at one point in time."},
		console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: onPage, Confirm: "Delete consistency group " + vpcStr(pol, "name") + "?"})
	return acts
}

func (p computeConsistencyGroupsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
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
	in, _, err := p.members(project, vpcStr(pol, "selfLink"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	members := console.Listing{Columns: []string{"Location", "Size", "Replication state"}, Noun: "disks"}
	for _, d := range in {
		state := secondaryState(d)
		if state == "" {
			state = "—"
			if activeSecondary(d) != "" {
				state = "ACTIVE"
			}
		}
		members.Items = append(members.Items, console.Resource{Name: vpcStr(d, "name"),
			Fields: map[string]string{"Location": diskLoc(d), "Size": vpcStr(d, "sizeGb") + " GB", "Replication state": state}})
	}
	members.Total = len(members.Items)
	return console.Detail{Title: path[1], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Region", Value: path[0]}, {Label: "Status", Value: vpcStr(pol, "status")},
			{Label: "Description", Value: vpcDash(vpcStr(pol, "description"))}, {Label: "Creation time", Value: vpcStr(pol, "creationTimestamp")}}}}},
		{ID: "members", Label: "Consistency group members", Listing: members},
	}}, nil
}

func (p computeConsistencyGroupsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	pol, err := p.call("GET", project, "regions/"+path[0]+"/resourcePolicies/"+path[1], nil, nil)
	if err != nil {
		return nil
	}
	in, all, err := p.members(project, vpcStr(pol, "selfLink"))
	if err != nil {
		return nil
	}
	return p.groupActions(pol, in, all, true)
}

func (p computeConsistencyGroupsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "regions/" + path[0] + "/resourcePolicies/" + path[1]
	link := "projects/" + project + "/" + base
	var err error
	switch action {
	case "assign", "remove":
		disk := strings.TrimSpace(values["disk"])
		if !strings.Contains(disk, "/disks/") {
			return errors.New("disk: choose a disk")
		}
		verb := "/addResourcePolicies"
		if action == "remove" {
			verb = "/removeResourcePolicies"
		}
		_, err = p.call("POST", project, disk+verb, nil, map[string]any{"resourcePolicies": []any{link}})
	case "terminatereplication":
		_, err = p.call("POST", project, "regions/"+path[0]+"/disks/stopGroupAsyncReplication", nil, map[string]any{"resourcePolicy": link})
	case "clone":
		_, err = p.call("POST", project, "regions/"+path[0]+"/disks/bulkInsert", nil, map[string]any{"sourceConsistencyGroupPolicy": link})
	case "delete":
		_, err = p.call("DELETE", project, base, nil, nil)
	default:
		err = fmt.Errorf("%s is not available on this resource", action)
	}
	return err
}

// regionOfZone is a zone's region: us-central1 for us-central1-a.
func regionOfZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}
