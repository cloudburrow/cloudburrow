package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// The Kubernetes clusters page over the Kubernetes Engine API (#1913): with
// `gke` among the services, the page lists the project's served clusters
// beside the instance's own, and Create cluster (Autopilot or Standard),
// Edit, Upgrade and Delete call the API, as Google's console does.

// gkeAPI is the served API, nil when this instance does not serve it.
func (p gkeClustersProvider) gkeAPI() *gke.Server {
	if p.svc == nil {
		return nil
	}
	return p.svc.API()
}

// gkeOwnClusterNote is why the instance's own cluster cannot be edited,
// upgraded or deleted here, when the API is served.
const gkeOwnClusterNote = "Not available locally: this is the instance's own kind cluster, made by `cloudburrow up` and removed by `cloudburrow delete`; " +
	"it is not a Kubernetes Engine cluster. Create cluster makes one."

// served lists the project's clusters.
func (p gkeClustersProvider) served(ctx context.Context, project string) []*containerpb.Cluster {
	api := p.gkeAPI()
	if api == nil || project == "" {
		return nil
	}
	l, err := api.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		return nil
	}
	return l.GetClusters()
}

// servedCluster finds a served cluster by name; the instance's own cluster
// name is never one.
func (p gkeClustersProvider) servedCluster(ctx context.Context, project, name string) *containerpb.Cluster {
	if name == p.name {
		return nil
	}
	for _, c := range p.served(ctx, project) {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

func gkeMode(c *containerpb.Cluster) string {
	if c.GetAutopilot().GetEnabled() {
		return "Autopilot"
	}
	return "Standard"
}

func gkeStatusText(c *containerpb.Cluster) string {
	switch c.GetStatus() {
	case containerpb.Cluster_RUNNING:
		return "Running"
	case containerpb.Cluster_PROVISIONING:
		return "Provisioning"
	case containerpb.Cluster_RECONCILING:
		return "Reconciling"
	case containerpb.Cluster_STOPPING:
		return "Stopping"
	case containerpb.Cluster_DEGRADED:
		return "Degraded"
	}
	return "Error"
}

func gkeLabels(m map[string]string) string {
	if len(m) == 0 {
		return "—"
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, k+": "+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func gkeChannel(c *containerpb.Cluster) string {
	ch := c.GetReleaseChannel().GetChannel()
	if ch == containerpb.ReleaseChannel_UNSPECIFIED {
		return "None"
	}
	s := strings.ToLower(ch.String())
	return strings.ToUpper(s[:1]) + s[1:] + " channel"
}

// servedRow is a served cluster's row of the list.
func (p gkeClustersProvider) servedRow(ctx context.Context, project string, c *containerpb.Cluster) console.Resource {
	cpu, memory := p.servedTotals(ctx, project, c)
	return console.Resource{Name: c.GetName(), Status: gkeStatusText(c), Fields: map[string]string{
		"Location": c.GetLocation(), "Tier": "Standard", "Mode": gkeMode(c),
		"Number of nodes": strconv.Itoa(int(c.GetCurrentNodeCount())), "Total vCPUs": cpu, "Total memory": memory,
		"Notifications": "—", "Labels": gkeLabels(c.GetResourceLabels()),
	}}
}

// kubeconfigOf is where a served cluster's kubeconfig is.
func (p gkeClustersProvider) kubeconfigOf(project string, c *containerpb.Cluster) string {
	kind := gke.KindName(p.name, "projects/"+project+"/locations/"+c.GetLocation()+"/clusters/"+c.GetName())
	return filepath.Join(p.gkeDir, kind, "kubeconfig")
}

// servedDetail is a served cluster's page: the API's record, and its nodes
// read from the cluster itself.
func (p gkeClustersProvider) servedDetail(ctx context.Context, project string, c *containerpb.Cluster) console.Detail {
	basics := console.PropertyGroup{Heading: "Cluster basics", Properties: []console.Property{
		{Label: "Name", Value: c.GetName()},
		{Label: "Mode", Value: gkeMode(c)},
		{Label: "Location type", Value: map[bool]string{true: "Zonal", false: "Regional"}[len(c.GetLocations()) == 1]},
		{Label: "Control plane location", Value: c.GetLocation()},
		{Label: "Default node zones", Value: strings.Join(c.GetLocations(), ", ")},
		{Label: "Release channel", Value: gkeChannel(c)},
		{Label: "Version", Value: c.GetCurrentMasterVersion()},
		{Label: "Total size", Value: strconv.Itoa(int(c.GetCurrentNodeCount()))},
		{Label: "External endpoint", Value: unsetAs(c.GetEndpoint(), "—")},
		{Label: "Created", Value: unsetAs(shortTime(c.GetCreateTime()), "—")},
		{Label: "Labels", Value: gkeLabels(c.GetResourceLabels())},
	}}
	if c.GetStatusMessage() != "" {
		basics.Properties = append(basics.Properties, console.Property{Label: "Status message", Value: c.GetStatusMessage()})
	}
	networking := console.PropertyGroup{Heading: "Networking", Properties: []console.Property{
		{Label: "Network", Value: c.GetNetwork()}, {Label: "Subnet", Value: c.GetSubnetwork()},
		{Label: "Cluster pod IPv4 range", Value: c.GetClusterIpv4Cidr()}, {Label: "Service IPv4 range", Value: c.GetServicesIpv4Cidr()},
		{Label: "Network policy", Value: gkeEnabledText(enforcesNetworkPolicy(c)), Action: "netpolicy", ActionText: "Edit network policy"},
	}}
	security := console.PropertyGroup{Heading: "Security", Properties: []console.Property{
		{Label: "Legacy authorization", Value: gkeEnabledText(c.GetLegacyAbac().GetEnabled()), Action: "abac", ActionText: "Edit legacy authorization"},
	}}
	details := console.Section{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{basics, networking, security}}
	pools := p.poolListing(ctx, project, c)
	// Google's Nodes tab holds the cluster's node pools and its nodes:
	// two sections drawn as one tab, Nodes, with Node pools and Nodes on a
	// strip inside it.
	poolsSection := console.Section{ID: "nodepools", Label: "Node pools", Group: "Nodes", Listing: pools}
	if a := p.addPoolAction(c); a.ID != "" {
		poolsSection.Actions = []console.Action{a}
	}
	var nodes, storage console.Listing
	if gkeServedReadable(c) {
		kc := p.kubeconfigOf(project, c)
		nodes = gkeNodesListing(ctx, kc, gkeClustersProvider{kubeconfig: kc}.facts(ctx), "")
		storage = gkeVolumesListing(ctx, kc)
	} else {
		why := "The cluster is " + strings.ToLower(gkeStatusText(c)) + "."
		nodes = console.Listing{NameColumn: "Name", Noun: "nodes", Unavailable: why}
		storage = console.Listing{NameColumn: "Persistent volume", Noun: "persistent volumes", Unavailable: why}
	}
	if v := p.upgradeTarget(ctx, project, c); v != "" {
		for i, prop := range basics.Properties {
			if prop.Label == "Version" {
				basics.Properties[i].Action, basics.Properties[i].ActionText = "upgrade", "Upgrade available"
			}
		}
		details.Groups[0] = basics
	}
	return console.Detail{
		Summary: stored(
			console.Property{Label: "Status", Value: gkeStatusText(c)},
			console.Property{Label: "Location", Value: c.GetLocation()},
			console.Property{Label: "Version", Value: c.GetCurrentMasterVersion()},
		),
		Sections: []console.Section{details, poolsSection, {ID: "nodes", Label: "Nodes", Group: "Nodes", Listing: nodes},
			{ID: "storage", Label: "Storage", Listing: storage}, gkeObservability(), gkeLogs(),
			p.operationsSection(ctx, project, c)},
	}
}

// gkeServedReadable is whether a served cluster's Kubernetes API can be
// read: it is running, or reconciling an update.
func gkeServedReadable(c *containerpb.Cluster) bool {
	return c.GetStatus() == containerpb.Cluster_RUNNING || c.GetStatus() == containerpb.Cluster_RECONCILING
}

// servedTotals are a served cluster's Total vCPUs and Total memory, read
// from its nodes, as the list shows them; "—" while it cannot be read.
func (p gkeClustersProvider) servedTotals(ctx context.Context, project string, c *containerpb.Cluster) (string, string) {
	if !gkeServedReadable(c) {
		return "—", "—"
	}
	f := gkeClustersProvider{kubeconfig: p.kubeconfigOf(project, c)}.facts(ctx)
	if f.unavailable != "" || len(f.nodes) == 0 {
		return "—", "—"
	}
	return gkeCoresText(f.cpu), gkeMemory(f.memory)
}

// servedVersions are the versions the API serves in the cluster's
// location, GetServerConfig's valid master versions.
func (p gkeClustersProvider) servedVersions(ctx context.Context, project string, c *containerpb.Cluster) []string {
	cfg, err := p.gkeAPI().GetServerConfig(ctx, &containerpb.GetServerConfigRequest{Name: "projects/" + project + "/locations/" + c.GetLocation()})
	if err != nil {
		return nil
	}
	return cfg.GetValidMasterVersions()
}

// gkeNewer are the versions newer than from, newest first.
func gkeNewer(versions []string, from string) []string {
	var out []string
	for _, v := range versions {
		if v != from && gkeVersionLess(from, v) {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return gkeVersionLess(out[j], out[i]) })
	return out
}

// upgradeTarget is the newest served version newer than the cluster's
// control plane, or "".
func (p gkeClustersProvider) upgradeTarget(ctx context.Context, project string, c *containerpb.Cluster) string {
	if t := gkeNewer(p.servedVersions(ctx, project, c), c.GetCurrentMasterVersion()); len(t) > 0 {
		return t[0]
	}
	return ""
}

// gkePoolTargets are the versions a node pool can be upgraded to: newer
// than its own, and no newer than the control plane's, as Google's "node
// pools cannot run a newer version than the control plane".
func gkePoolTargets(versions []string, c *containerpb.Cluster, np *containerpb.NodePool) []string {
	var out []string
	for _, v := range gkeNewer(versions, np.GetVersion()) {
		if !gkeVersionLess(c.GetCurrentMasterVersion(), v) {
			out = append(out, v)
		}
	}
	return out
}

// gkeVersionLess compares X.Y.Z-gke.N versions numerically.
func gkeVersionLess(a, b string) bool {
	num := func(s string) []int {
		var out []int
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
			n, _ := strconv.Atoi(f)
			out = append(out, n)
		}
		return out
	}
	x, y := num(a), num(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

var gkeChannels = map[string]containerpb.ReleaseChannel_Channel{
	"Rapid channel": containerpb.ReleaseChannel_RAPID, "Regular channel": containerpb.ReleaseChannel_REGULAR,
	"Stable channel": containerpb.ReleaseChannel_STABLE, "Extended channel": containerpb.ReleaseChannel_EXTENDED,
}

func (p gkeClustersProvider) servedActions(ctx context.Context, project string, c *containerpb.Cluster) []console.Action {
	upgrade := console.Action{ID: "upgrade", Label: "Upgrade", Unavailable: "No newer Kubernetes Engine version is available locally: " +
		"the versions served are the kind node images this instance has (" + c.GetCurrentMasterVersion() + ")."}
	if targets := gkeNewer(p.servedVersions(ctx, project, c), c.GetCurrentMasterVersion()); len(targets) > 0 {
		// Google's Upgrade control plane: the target version, and the
		// note that the node pools follow separately.
		upgrade = console.Action{ID: "upgrade", Label: "Upgrade", Title: "Upgrade control plane", Submit: "Save changes", Fields: []console.Field{
			{Name: "version", Label: "Target version", Type: "select", Options: targets, Default: targets[0],
				Help: "Current version: " + c.GetCurrentMasterVersion() + ". The control plane is upgraded first; each node pool is upgraded on its own, from the Nodes tab."}}}
	}
	labels := ""
	for k, v := range c.GetResourceLabels() {
		labels += k + "=" + v + "\n"
	}
	return []console.Action{
		{ID: "connect", Label: "Connect", Submit: "OK", Fields: []console.Field{
			{Name: "command", Label: "Command-line access", Type: "textarea", Immutable: true,
				Default: "gcloud container clusters get-credentials " + c.GetName() + " --location " + c.GetLocation() + " --project " + project,
				Help: "With this instance's gcloud configuration (`cloudburrow gcloud-setup`). kubectl authenticates with the cluster's client certificate " +
					"(created with Issue a client certificate, and gcloud's container/use_client_certificate)."},
		}},
		{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
			{Name: "channel", Label: "Release channel", Type: "select", Options: []string{"Rapid channel", "Regular channel", "Stable channel", "Extended channel"}, Default: gkeChannel(c)},
			{Name: "labels", Label: "Labels", Type: "textarea", Default: strings.TrimSpace(labels), Help: "One key=value per line."},
		}},
		upgrade,
		gkeMaintenanceAction(c),
		gkeNetworkPolicyAction(c),
		gkeLegacyAbacAction(c),
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: c.GetName(), Leaves: true},
		p.addPoolAction(c),
	}
}

func gkeStatusErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(status.Convert(err).Message())
}

func (p gkeClustersProvider) servedAct(ctx context.Context, project string, c *containerpb.Cluster, action string, v map[string]string) error {
	api := p.gkeAPI()
	name := "projects/" + project + "/locations/" + c.GetLocation() + "/clusters/" + c.GetName()
	switch action {
	case "connect":
		return nil
	case "addpool":
		return p.addPool(ctx, name, c, v)
	case "delete":
		_, err := api.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: name})
		return gkeStatusErr(err)
	case "upgrade":
		_, err := api.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: v["version"]}})
		return gkeStatusErr(err)
	case "netpolicy", "abac":
		return p.settingsAct(ctx, name, c, action, v)
	case "maintenance":
		mp, err := gkeParseMaintenance(v["window"], v["exclusions"])
		if err != nil {
			return err
		}
		mp.ResourceVersion = c.GetMaintenancePolicy().GetResourceVersion()
		_, err = api.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name, MaintenancePolicy: mp})
		return gkeStatusErr(err)
	case "edit":
		labels := map[string]string{}
		for _, line := range strings.Split(v["labels"], "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			k, val, ok := strings.Cut(line, "=")
			if !ok {
				return fmt.Errorf("label %q is not key=value", line)
			}
			labels[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
		if ch, ok := gkeChannels[v["channel"]]; ok && ch != c.GetReleaseChannel().GetChannel() {
			if _, err := api.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name,
				Update: &containerpb.ClusterUpdate{DesiredReleaseChannel: &containerpb.ReleaseChannel{Channel: ch}}}); err != nil {
				return gkeStatusErr(err)
			}
		}
		if gkeLabels(labels) != gkeLabels(c.GetResourceLabels()) {
			// The update's operation is done by the time it is read
			// again; the fingerprint is the current one's.
			for i := 0; i < 50; i++ {
				cur, err := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
				if err != nil {
					return gkeStatusErr(err)
				}
				_, err = api.SetLabels(ctx, &containerpb.SetLabelsRequest{Name: name, ResourceLabels: labels, LabelFingerprint: cur.GetLabelFingerprint()})
				if err == nil || !strings.Contains(err.Error(), "incompatible operation") {
					return gkeStatusErr(err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-gkeTick():
				}
			}
			return errors.New("the cluster is still busy; try again")
		}
		return nil
	}
	return fmt.Errorf("unknown action %s", action)
}

// servedCreate is Create cluster over the API.
func (p gkeClustersProvider) servedCreate(ctx context.Context, project string, v map[string]string) (string, error) {
	location := strings.TrimSpace(v["region"])
	if v["locationType"] == "Zonal" {
		location = strings.TrimSpace(v["zone"])
	}
	c := &containerpb.Cluster{Name: strings.TrimSpace(v["name"]), InitialClusterVersion: strings.TrimSpace(v["version"]),
		Network: v["network"], Subnetwork: v["subnet"],
		ReleaseChannel: &containerpb.ReleaseChannel{Channel: gkeChannels[v["channel"]]},
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{
			IssueClientCertificate: v["clientCert"] == "true" || v["clientCert"] == "on"}}}
	if v["mode"] == "Standard" {
		n, err := strconv.Atoi(strings.TrimSpace(v["nodes"]))
		if err != nil {
			return "", fmt.Errorf("number of nodes %q is not a number", v["nodes"])
		}
		c.NodePools = []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: int32(n),
			Config: &containerpb.NodeConfig{MachineType: strings.TrimSpace(v["machine"])}}}
		more, err := console.ParseGroup(v["pools"])
		if err != nil {
			return "", fmt.Errorf("node pools: %w", err)
		}
		for _, row := range more {
			pool := strings.TrimSpace(row["name"])
			if pool == "" {
				return "", fmt.Errorf("a node pool has no name")
			}
			n, err := strconv.Atoi(strings.TrimSpace(row["nodes"]))
			if err != nil {
				return "", fmt.Errorf("node pool %s: number of nodes %q is not a number", pool, row["nodes"])
			}
			c.NodePools = append(c.NodePools, &containerpb.NodePool{Name: pool, InitialNodeCount: int32(n),
				Config: &containerpb.NodeConfig{MachineType: strings.TrimSpace(row["machine"]), ImageType: row["image"]}})
		}
	} else {
		c.Autopilot = &containerpb.Autopilot{Enabled: true}
	}
	if _, err := p.gkeAPI().CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + location, Cluster: c}); err != nil {
		return "", gkeStatusErr(err)
	}
	return c.GetName(), nil
}

// gkeTitle is an enum name as a status word: RUNNING_WITH_ERROR is
// "Running with error".
func gkeTitle(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func gkeTick() <-chan time.Time { return time.After(200 * time.Millisecond) }
