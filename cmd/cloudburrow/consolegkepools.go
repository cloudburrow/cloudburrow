package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A served cluster's node pools and operations on the console (#1925), as
// Google's console has them: the Node pools tab's Add node pool, and on each
// pool Resize, Edit and Delete; and the cluster's operations, a running one
// with Cancel. Each calls the same Kubernetes Engine API gcloud and
// Terraform do, so a resize adds or removes real kind nodes.

// gkePoolPath is the path a node pool row's actions are performed at:
// [cluster, "nodePools", pool], apart from the operations' [cluster,
// "operations", op], as a pool and an operation could share a name.
const (
	gkePoolSeg = "nodePools"
	gkeOpSeg   = "operations"
)

// gkeImageTypes are the node image types the API serves.
var gkeImageTypes = []string{"COS_CONTAINERD", "UBUNTU_CONTAINERD"}

// gkeAutopilotPools is why an Autopilot cluster's pools cannot be changed.
const gkeAutopilotPools = "Autopilot manages this cluster's nodes: node pools cannot be added, resized, edited or deleted."

func gkePoolNodes(np *containerpb.NodePool) int {
	return int(np.GetInitialNodeCount()) * len(np.GetLocations())
}

// poolListing is the Node pools tab: each pool, with its actions.
func (p gkeClustersProvider) poolListing(ctx context.Context, project string, c *containerpb.Cluster) console.Listing {
	versions := p.servedVersions(ctx, project, c)
	l := console.Listing{Columns: []string{"Number of nodes", "Machine type", "Image type", "Version", "Labels"}, NameColumn: "Name",
		Noun: "node pools", AlwaysStatus: true}
	for _, np := range c.GetNodePools() {
		r := console.Resource{Name: np.GetName(), Status: gkeTitle(np.GetStatus().String()), Fields: map[string]string{
			"Number of nodes": strconv.Itoa(gkePoolNodes(np)),
			"Machine type":    np.GetConfig().GetMachineType(), "Image type": np.GetConfig().GetImageType(), "Version": np.GetVersion(),
			"Labels": gkeLabels(np.GetConfig().GetResourceLabels()),
		}}
		if !c.GetAutopilot().GetEnabled() {
			r.Target = []string{c.GetName(), gkePoolSeg, np.GetName()}
			r.Actions = p.poolActions(c, np, versions)
		}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	if c.GetAutopilot().GetEnabled() {
		l.Note = "Autopilot manages this cluster's nodes."
	}
	return l
}

// addPoolAction is the Node pools tab's Add node pool.
func (p gkeClustersProvider) addPoolAction(c *containerpb.Cluster) console.Action {
	if c.GetAutopilot().GetEnabled() {
		return console.Action{ID: "addpool", Label: "Add node pool", Unavailable: gkeAutopilotPools}
	}
	return console.Action{ID: "addpool", Label: "Add node pool", Submit: "Create", Fields: []console.Field{
		{Name: "name", Label: "Name", Type: "text", Default: fmt.Sprintf("pool-%d", len(c.GetNodePools())+1), Pattern: `^[a-z]([a-z0-9\-]{0,38}[a-z0-9])?$`,
			Section: "Node pool details"},
		{Name: "nodes", Label: "Number of nodes (per zone)", Type: "text", Default: "1", Pattern: `^[0-9]+$`, Section: "Node pool details",
			Help: "Each node is a kind node of this cluster's: a container on this machine."},
		{Name: "machine", Label: "Machine type", Type: "text", Default: "e2-medium", Section: "Nodes",
			Help: "Recorded; a local node has this machine's resources."},
		{Name: "image", Label: "Image type", Type: "select", Options: gkeImageTypes, Default: gkeImageTypes[0], Section: "Nodes"},
	}}
}

func (p gkeClustersProvider) poolActions(c *containerpb.Cluster, np *containerpb.NodePool, versions []string) []console.Action {
	labels := ""
	keys := make([]string, 0, len(np.GetConfig().GetResourceLabels()))
	for k := range np.GetConfig().GetResourceLabels() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels += k + "=" + np.GetConfig().GetResourceLabels()[k] + "\n"
	}
	image := np.GetConfig().GetImageType()
	if image == "" {
		image = gkeImageTypes[0]
	}
	return []console.Action{
		{ID: "resize", Label: "Resize", Submit: "Resize", Fields: []console.Field{
			{Name: "nodes", Label: "Number of nodes (per zone)", Type: "text", Default: strconv.Itoa(int(np.GetInitialNodeCount())), Pattern: `^[0-9]+$`},
		}},
		{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
			{Name: "image", Label: "Image type", Type: "select", Options: gkeImageTypes, Default: image},
			{Name: "labels", Label: "Labels", Type: "textarea", Default: strings.TrimSpace(labels), Help: "The nodes' resource labels, one key=value per line."},
		}},
		gkePoolUpgrade(c, np, versions),
		gkePoolCompleteUpgrade(np),
		gkePoolRollback(np),
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: np.GetName()},
	}
}

// gkePoolCompleteUpgrade ends a blue-green upgrade's soak, as `gcloud
// container node-pools complete-upgrade` does (#2069); otherwise it says
// why it is not offered.
func gkePoolCompleteUpgrade(np *containerpb.NodePool) console.Action {
	a := console.Action{ID: "complete", Label: "Complete upgrade", Submit: "Complete upgrade", Fields: []console.Field{
		{Name: "note", Label: "Soaking", Type: "textarea", Immutable: true,
			Default: "The green nodes run " + np.GetUpdateInfo().GetBlueGreenInfo().GetGreenPoolVersion() + ". Completing ends the soak: the blue nodes are deleted now."}}}
	if np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() != containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING {
		return console.Action{ID: "complete", Label: "Complete upgrade", Unavailable: "This node pool is not soaking in a blue-green upgrade."}
	}
	return a
}

// gkePoolRollback is a node pool's Roll back, as `gcloud container
// node-pools rollback` does it (#2076): offered while an upgrade is
// incomplete (cancelled, failed, paused at the end of its maintenance
// window) or a blue-green one runs before it deletes its blue nodes; the
// API refuses the rest with its reason.
// The label is UNVERIFIED (re-read 2026-11-20: how-to/upgrading-a-cluster gives the gcloud command and the API only).
func gkePoolRollback(np *containerpb.NodePool) console.Action {
	ph := np.GetUpdateInfo().GetBlueGreenInfo().GetPhase()
	incomplete := np.GetStatus() == containerpb.NodePool_RUNNING_WITH_ERROR || np.GetStatusMessage() != "" ||
		(ph != containerpb.NodePool_UpdateInfo_BlueGreenInfo_PHASE_UNSPECIFIED && ph != containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL &&
			ph != containerpb.NodePool_UpdateInfo_BlueGreenInfo_ROLLBACK_STARTED)
	if !incomplete {
		why := "This node pool has no incomplete upgrade to roll back: a node pool cannot be rolled back once it has been successfully upgraded."
		if ph == containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL {
			why = "This node pool is deleting its blue nodes: a blue-green upgrade cannot be rolled back in that phase."
		}
		return console.Action{ID: "rollback", Label: "Roll back", Unavailable: why}
	}
	return console.Action{ID: "rollback", Label: "Roll back", Title: "Roll back node pool upgrade", Submit: "Roll back", Fields: []console.Field{
		{Name: "note", Label: "Rollback", Type: "textarea", Immutable: true,
			Default: "The nodes already upgraded are recreated on " + np.GetVersion() + "; a blue-green upgrade's blue nodes are uncordoned and its green nodes drained and deleted."},
		{Name: "respectPdb", Label: "Respect pod disruption budgets", Type: "checkbox",
			Help: "Drain the nodes through evictions, as an upgrade does (gcloud's --respect-pdb). Unchecked, the pods are deleted."},
	}}
}

// gkePoolUpgrade is a node pool's Upgrade: Google's node pool page has
// Upgrade beside the node version, to a version no newer than the
// control plane's.
func gkePoolUpgrade(c *containerpb.Cluster, np *containerpb.NodePool, versions []string) console.Action {
	targets := gkePoolTargets(versions, c, np)
	if len(targets) == 0 {
		why := "This node pool runs the newest version served locally (" + np.GetVersion() + ")."
		if len(gkeNewer(versions, np.GetVersion())) > 0 {
			why = "This node pool runs the control plane's version (" + np.GetVersion() + "): upgrade the control plane first."
		}
		return console.Action{ID: "upgrade", Label: "Upgrade", Unavailable: why}
	}
	return console.Action{ID: "upgrade", Label: "Upgrade", Title: "Upgrade node pool", Submit: "Save changes", Fields: []console.Field{
		{Name: "version", Label: "Target version", Type: "select", Options: targets, Default: targets[0],
			Help: "Current version: " + np.GetVersion() + ". Control plane version: " + c.GetCurrentMasterVersion() + "."}}}
}

// gkeParseLabels reads key=value lines.
func gkeParseLabels(text string) (map[string]string, error) {
	labels := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("label %q is not key=value", line)
		}
		labels[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return labels, nil
}

func (p gkeClustersProvider) addPool(ctx context.Context, cluster string, c *containerpb.Cluster, v map[string]string) error {
	if c.GetAutopilot().GetEnabled() {
		return fmt.Errorf("%s", gkeAutopilotPools)
	}
	n, err := strconv.Atoi(strings.TrimSpace(v["nodes"]))
	if err != nil {
		return fmt.Errorf("number of nodes %q is not a number", v["nodes"])
	}
	np := &containerpb.NodePool{Name: strings.TrimSpace(v["name"]), InitialNodeCount: int32(n),
		Config: &containerpb.NodeConfig{MachineType: strings.TrimSpace(v["machine"]), ImageType: v["image"]}}
	_, err = p.gkeAPI().CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: cluster, NodePool: np})
	return gkeStatusErr(err)
}

// poolAct is a node pool row's action.
func (p gkeClustersProvider) poolAct(ctx context.Context, cluster string, c *containerpb.Cluster, pool, action string, v map[string]string) error {
	var np *containerpb.NodePool
	for _, x := range c.GetNodePools() {
		if x.GetName() == pool {
			np = x
		}
	}
	if np == nil {
		return fmt.Errorf("no node pool named %s in %s", pool, c.GetName())
	}
	api := p.gkeAPI()
	name := cluster + "/nodePools/" + pool
	switch action {
	case "delete":
		_, err := api.DeleteNodePool(ctx, &containerpb.DeleteNodePoolRequest{Name: name})
		return gkeStatusErr(err)
	case "complete":
		_, err := api.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: name})
		return gkeStatusErr(err)
	case "rollback":
		_, err := api.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: name, RespectPdb: v["respectPdb"] == "true"})
		return gkeStatusErr(err)
	case "upgrade":
		v := strings.TrimSpace(v["version"])
		if v == "" {
			return fmt.Errorf("choose a target version")
		}
		image := np.GetConfig().GetImageType()
		if image == "" {
			image = gkeImageTypes[0]
		}
		_, err := api.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name, NodeVersion: v, ImageType: image})
		return gkeStatusErr(err)
	case "resize":
		n, err := strconv.Atoi(strings.TrimSpace(v["nodes"]))
		if err != nil {
			return fmt.Errorf("number of nodes %q is not a number", v["nodes"])
		}
		_, err = api.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: name, NodeCount: int32(n)})
		return gkeStatusErr(err)
	case "edit":
		labels, err := gkeParseLabels(v["labels"])
		if err != nil {
			return err
		}
		image := v["image"]
		if image == "" {
			image = np.GetConfig().GetImageType()
		}
		_, err = api.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name, NodeVersion: np.GetVersion(), ImageType: image,
			ResourceLabels: &containerpb.ResourceLabels{Labels: labels}})
		return gkeStatusErr(err)
	}
	return fmt.Errorf("unknown node pool action %s", action)
}

// gkeOpType is an operation's type as the console words it:
// CREATE_NODE_POOL is "Create node pool".
func gkeOpType(t containerpb.Operation_Type) string { return gkeTitle(t.String()) }

// clusterOps are the operations on a cluster and its node pools, newest
// first.
func (p gkeClustersProvider) clusterOps(ctx context.Context, project string, c *containerpb.Cluster) ([]*containerpb.Operation, error) {
	l, err := p.gkeAPI().ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: "projects/" + project + "/locations/" + c.GetLocation()})
	if err != nil {
		return nil, gkeStatusErr(err)
	}
	var out []*containerpb.Operation
	for _, o := range l.GetOperations() {
		if t := o.GetTargetLink(); t == c.GetSelfLink() || strings.HasPrefix(t, c.GetSelfLink()+"/") {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetStartTime() > out[j].GetStartTime() })
	return out, nil
}

// operationsSection is the cluster's Operations tab: what was done to the
// cluster and its node pools, a running operation with Cancel.
func (p gkeClustersProvider) operationsSection(ctx context.Context, project string, c *containerpb.Cluster) console.Section {
	l := console.Listing{Columns: []string{"Type", "Target", "Started", "Ended", "Message"}, NameColumn: "Name", Noun: "operations", AlwaysStatus: true}
	ops, err := p.clusterOps(ctx, project, c)
	if err != nil {
		l.Unavailable = err.Error()
	}
	for _, o := range ops {
		target := c.GetName()
		if _, pool, ok := strings.Cut(o.GetTargetLink(), "/nodePools/"); ok {
			target = c.GetName() + "/" + pool
		}
		st := gkeTitle(o.GetStatus().String())
		if o.GetStatus() == containerpb.Operation_DONE && o.GetError() != nil {
			st = "Error"
			if o.GetError().GetCode() == 1 {
				// CANCELLED: a cancelled or rolled-back upgrade (#2076).
				st = "Cancelled"
			}
		}
		r := console.Resource{Name: o.GetName(), Status: st, Fields: map[string]string{
			"Type": gkeOpType(o.GetOperationType()), "Target": target,
			"Started": unsetAs(shortTime(o.GetStartTime()), "—"), "Ended": unsetAs(shortTime(o.GetEndTime()), "—"),
			"Message": unsetAs(o.GetStatusMessage(), "—"),
		}}
		// ABORTING (a cancel asked, its started nodes completing) has
		// nothing left to cancel.
		if o.GetStatus() == containerpb.Operation_RUNNING || o.GetStatus() == containerpb.Operation_PENDING {
			r.Target = []string{c.GetName(), gkeOpSeg, o.GetName()}
			r.Actions = []console.Action{{ID: "cancel", Label: "Cancel", Destructive: true, Confirm: o.GetName()}}
		}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return console.Section{ID: "operations", Label: "Operations", Listing: l}
}

// opAct cancels a running operation of the cluster's.
func (p gkeClustersProvider) opAct(ctx context.Context, project string, c *containerpb.Cluster, op, action string) error {
	if action != "cancel" {
		return fmt.Errorf("unknown operation action %s", action)
	}
	_, err := p.gkeAPI().CancelOperation(ctx, &containerpb.CancelOperationRequest{
		Name: "projects/" + project + "/locations/" + c.GetLocation() + "/operations/" + op})
	return gkeStatusErr(err)
}

// servedSubActions are a pool's or an operation's actions, at
// [cluster, nodePools|operations, name].
func (p gkeClustersProvider) servedSubActions(ctx context.Context, project string, c *containerpb.Cluster, seg, name string) []console.Action {
	switch seg {
	case gkePoolSeg:
		if c.GetAutopilot().GetEnabled() {
			return nil
		}
		for _, np := range c.GetNodePools() {
			if np.GetName() == name {
				return p.poolActions(c, np, p.servedVersions(ctx, project, c))
			}
		}
	case gkeOpSeg:
		return []console.Action{{ID: "cancel", Label: "Cancel", Destructive: true, Confirm: name}}
	}
	return nil
}

func (p gkeClustersProvider) servedSubAct(ctx context.Context, project string, c *containerpb.Cluster, seg, name, action string, v map[string]string) error {
	cluster := "projects/" + project + "/locations/" + c.GetLocation() + "/clusters/" + c.GetName()
	switch seg {
	case gkePoolSeg:
		if c.GetAutopilot().GetEnabled() {
			return fmt.Errorf("%s", gkeAutopilotPools)
		}
		return p.poolAct(ctx, cluster, c, name, action, v)
	case gkeOpSeg:
		return p.opAct(ctx, project, c, name, action)
	}
	return fmt.Errorf("no %s under cluster %s", seg, c.GetName())
}

// gkeExclusionScopes are the console's words for an exclusion's scope.
var gkeExclusionScopes = map[string]containerpb.MaintenanceExclusionOptions_Scope{
	"no_upgrades": containerpb.MaintenanceExclusionOptions_NO_UPGRADES, "no_minor_upgrades": containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES,
	"no_minor_or_node_upgrades": containerpb.MaintenanceExclusionOptions_NO_MINOR_OR_NODE_UPGRADES,
}

// gkeMaintenanceAction edits the cluster's maintenance window and
// exclusions (Google's Details, Automation: Maintenance window and
// Maintenance exclusions; #2069), which automatic upgrades respect and
// manual ones ignore.
func gkeMaintenanceAction(c *containerpb.Cluster) console.Action {
	w := c.GetMaintenancePolicy().GetWindow()
	window := ""
	if d := w.GetDailyMaintenanceWindow(); d != nil {
		window = "daily " + d.GetStartTime()
	} else if r := w.GetRecurringWindow(); r != nil {
		window = r.GetWindow().GetStartTime().AsTime().UTC().Format(time.RFC3339) + " " +
			r.GetWindow().GetEndTime().AsTime().UTC().Format(time.RFC3339) + " " + r.GetRecurrence()
	}
	names := make([]string, 0, len(w.GetMaintenanceExclusions()))
	for n := range w.GetMaintenanceExclusions() {
		names = append(names, n)
	}
	sort.Strings(names)
	var ex []string
	for _, n := range names {
		tw := w.GetMaintenanceExclusions()[n]
		ex = append(ex, n+" "+tw.GetStartTime().AsTime().UTC().Format(time.RFC3339)+" "+tw.GetEndTime().AsTime().UTC().Format(time.RFC3339)+" "+
			strings.ToLower(tw.GetMaintenanceExclusionOptions().GetScope().String()))
	}
	return console.Action{ID: "maintenance", Label: "Edit maintenance", Title: "Maintenance window and exclusions", Submit: "Save", Fields: []console.Field{
		{Name: "window", Label: "Maintenance window", Type: "text", Default: window,
			Help: "Empty for any time; \"daily HH:MM\" (UTC, four hours); or \"START END RRULE\", RFC 3339 times and FREQ=DAILY or FREQ=WEEKLY;BYDAY=SA,SU. Automatic upgrades start only inside it; manual upgrades ignore it."},
		{Name: "exclusions", Label: "Maintenance exclusions", Type: "textarea", Default: strings.Join(ex, "\n"),
			Help: "One per line: NAME START END SCOPE, RFC 3339 times, scope no_upgrades, no_minor_upgrades or no_minor_or_node_upgrades."},
	}}
}

// gkeParseMaintenance reads the Edit maintenance dialog.
func gkeParseMaintenance(window, exclusions string) (*containerpb.MaintenancePolicy, error) {
	mw := &containerpb.MaintenanceWindow{MaintenanceExclusions: map[string]*containerpb.TimeWindow{}}
	ts := func(s string) (*timestamppb.Timestamp, error) {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an RFC 3339 time", s)
		}
		return timestamppb.New(t), nil
	}
	if f := strings.Fields(window); len(f) > 0 {
		switch {
		case len(f) == 2 && f[0] == "daily":
			mw.Policy = &containerpb.MaintenanceWindow_DailyMaintenanceWindow{DailyMaintenanceWindow: &containerpb.DailyMaintenanceWindow{StartTime: f[1]}}
		case len(f) == 3:
			start, err := ts(f[0])
			if err != nil {
				return nil, err
			}
			end, err := ts(f[1])
			if err != nil {
				return nil, err
			}
			mw.Policy = &containerpb.MaintenanceWindow_RecurringWindow{RecurringWindow: &containerpb.RecurringTimeWindow{
				Window: &containerpb.TimeWindow{StartTime: start, EndTime: end}, Recurrence: f[2]}}
		default:
			return nil, fmt.Errorf("maintenance window %q is neither \"daily HH:MM\" nor \"START END RRULE\"", window)
		}
	}
	for _, line := range strings.Split(exclusions, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("exclusion %q is not NAME START END SCOPE", strings.TrimSpace(line))
		}
		start, err := ts(f[1])
		if err != nil {
			return nil, err
		}
		end, err := ts(f[2])
		if err != nil {
			return nil, err
		}
		scope, ok := gkeExclusionScopes[strings.ToLower(f[3])]
		if !ok {
			return nil, fmt.Errorf("exclusion scope %q is not no_upgrades, no_minor_upgrades or no_minor_or_node_upgrades", f[3])
		}
		mw.MaintenanceExclusions[f[0]] = &containerpb.TimeWindow{StartTime: start, EndTime: end,
			Options: &containerpb.TimeWindow_MaintenanceExclusionOptions{MaintenanceExclusionOptions: &containerpb.MaintenanceExclusionOptions{Scope: scope}}}
	}
	return &containerpb.MaintenancePolicy{Window: mw}, nil
}
