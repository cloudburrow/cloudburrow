package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"

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
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: np.GetName()},
	}
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
		}
		r := console.Resource{Name: o.GetName(), Status: st, Fields: map[string]string{
			"Type": gkeOpType(o.GetOperationType()), "Target": target,
			"Started": unsetAs(shortTime(o.GetStartTime()), "—"), "Ended": unsetAs(shortTime(o.GetEndTime()), "—"),
			"Message": unsetAs(o.GetStatusMessage(), "—"),
		}}
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
