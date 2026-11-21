package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Kubernetes clusters page's settings over the Kubernetes Engine API
// (#2100), where Google's console has them: the cluster's Details tab,
// Networking's Network policy and Security's Legacy authorization, each
// with its edit; a node pool's Edit, Autoscaling (Enable cluster
// autoscaler, minimum and maximum nodes per zone) and Management
// (Enable auto-upgrade, Enable auto-repair).

func gkeEnabledText(on bool) string {
	if on {
		return "Enabled"
	}
	return "Disabled"
}

// enforcesNetworkPolicy is the API's rule: Autopilot and Dataplane V2
// always enforce.
func enforcesNetworkPolicy(c *containerpb.Cluster) bool {
	return c.GetAutopilot().GetEnabled() || c.GetNetworkPolicy().GetEnabled() ||
		c.GetNetworkConfig().GetDatapathProvider() == containerpb.DatapathProvider_ADVANCED_DATAPATH
}

func gkeNetworkPolicyAction(c *containerpb.Cluster) console.Action {
	if c.GetAutopilot().GetEnabled() {
		return console.Action{ID: "netpolicy", Label: "Edit network policy", Unavailable: "Autopilot clusters always enforce network policy (GKE Dataplane V2)."}
	}
	return console.Action{ID: "netpolicy", Label: "Edit network policy", Title: "Edit network policy", Submit: "Save changes", Fields: []console.Field{
		{Name: "enabled", Label: "Enable network policy for control plane and nodes", Type: "checkbox", Default: strconv.FormatBool(c.GetNetworkPolicy().GetEnabled()),
			Help: "NetworkPolicy objects are enforced only while this is on. Saving enables the NetworkPolicy add-on first, as Google's two steps do."},
	}}
}

func gkeLegacyAbacAction(c *containerpb.Cluster) console.Action {
	if c.GetAutopilot().GetEnabled() {
		return console.Action{ID: "abac", Label: "Edit legacy authorization", Unavailable: "Legacy ABAC can't be enabled in Autopilot clusters."}
	}
	return console.Action{ID: "abac", Label: "Edit legacy authorization", Title: "Legacy authorization", Submit: "Save changes", Fields: []console.Field{
		{Name: "enabled", Label: "Enable legacy authorization", Type: "checkbox", Default: strconv.FormatBool(c.GetLegacyAbac().GetEnabled()),
			Help: "Legacy ABAC grants every service account full access to the Kubernetes API, beside RBAC. Google recommends leaving it disabled."},
	}}
}

// settingsAct saves the network policy or legacy authorization dialog.
func (p gkeClustersProvider) settingsAct(ctx context.Context, name string, c *containerpb.Cluster, act string, v map[string]string) error {
	api := p.gkeAPI()
	on := v["enabled"] == "true" || v["enabled"] == "on"
	switch act {
	case "abac":
		_, err := api.SetLegacyAbac(ctx, &containerpb.SetLegacyAbacRequest{Name: name, Enabled: on})
		return gkeStatusErr(err)
	case "netpolicy":
		if on && c.GetAddonsConfig().GetNetworkPolicyConfig().GetDisabled() {
			if _, err := api.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name,
				AddonsConfig: &containerpb.AddonsConfig{NetworkPolicyConfig: &containerpb.NetworkPolicyConfig{}}}); err != nil {
				return gkeStatusErr(err)
			}
		}
		_, err := api.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: on}})
		return gkeStatusErr(err)
	}
	return fmt.Errorf("unknown action %s", act)
}

func gkePoolAutoscalingAction(np *containerpb.NodePool) console.Action {
	a := np.GetAutoscaling()
	lo, hi := "1", "3"
	if a.GetEnabled() {
		lo, hi = strconv.Itoa(int(a.GetMinNodeCount())), strconv.Itoa(int(a.GetMaxNodeCount()))
	}
	return console.Action{ID: "autoscaling", Label: "Autoscaling", Title: "Edit node pool autoscaling", Submit: "Save", Fields: []console.Field{
		{Name: "enabled", Label: "Enable cluster autoscaler", Type: "checkbox", Default: strconv.FormatBool(a.GetEnabled()),
			Help: "Nodes are added when pods cannot be scheduled, and an empty node is removed after ten minutes."},
		{Name: "min", Label: "Minimum number of nodes (per zone)", Type: "text", Default: lo, Pattern: `^[0-9]+$`},
		{Name: "max", Label: "Maximum number of nodes (per zone)", Type: "text", Default: hi, Pattern: `^[0-9]+$`},
	}}
}

func gkePoolManagementAction(np *containerpb.NodePool) console.Action {
	m := np.GetManagement()
	return console.Action{ID: "management", Label: "Management", Title: "Edit node pool management", Submit: "Save", Fields: []console.Field{
		{Name: "autoUpgrade", Label: "Enable auto-upgrade", Type: "checkbox", Default: strconv.FormatBool(m.GetAutoUpgrade()),
			Help: "A cluster on a release channel keeps auto-upgrade on."},
		{Name: "autoRepair", Label: "Enable auto-repair", Type: "checkbox", Default: strconv.FormatBool(m.GetAutoRepair()),
			Help: "A node NotReady for about ten minutes is re-created."},
	}}
}

// poolSettingsAct saves a pool's autoscaling or management dialog.
func (p gkeClustersProvider) poolSettingsAct(ctx context.Context, name, act string, v map[string]string) error {
	api := p.gkeAPI()
	on := func(k string) bool { return v[k] == "true" || v[k] == "on" }
	switch act {
	case "autoscaling":
		a := &containerpb.NodePoolAutoscaling{Enabled: on("enabled")}
		if a.Enabled {
			lo, err1 := strconv.Atoi(strings.TrimSpace(v["min"]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(v["max"]))
			if err1 != nil || err2 != nil {
				return fmt.Errorf("the minimum and maximum numbers of nodes must be numbers")
			}
			a.MinNodeCount, a.MaxNodeCount = int32(lo), int32(hi)
		}
		_, err := api.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: name, Autoscaling: a})
		return gkeStatusErr(err)
	case "management":
		_, err := api.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: name,
			Management: &containerpb.NodeManagement{AutoUpgrade: on("autoUpgrade"), AutoRepair: on("autoRepair")}})
		return gkeStatusErr(err)
	}
	return fmt.Errorf("unknown node pool action %s", act)
}

// The rest of the cluster's settings (#2100, round 2), where Google's
// console has them: Cluster basics' Default node zones with Edit zones;
// Security's Credential rotation with Start and Complete credential
// rotation; Features' Logging and Cloud Monitoring with their edits; the
// Version's automatic upgrade status (FetchClusterUpgradeInfo), and the
// Upgrade dialog's control plane soak, a rollback-safe upgrade's, with
// Complete control plane upgrade during the soak.

func gkeZonesAction(c *containerpb.Cluster) console.Action {
	if c.GetAutopilot().GetEnabled() {
		return console.Action{ID: "zones", Label: "Edit zones", Unavailable: "Google manages an Autopilot cluster's nodes and their zones."}
	}
	return console.Action{ID: "zones", Label: "Edit zones", Title: "Edit default node zones", Submit: "Save changes", Fields: []console.Field{
		{Name: "zones", Label: "Node zones", Type: "text", Default: strings.Join(c.GetLocations(), ","), Pattern: `^[a-z0-9\-]+(,[a-z0-9\-]+)*$`,
			Help: "Comma-separated zones of the cluster's region. A zonal cluster keeps its own zone. Every node pool has its node count in each zone: nodes are added or removed at once."},
	}}
}

func gkeRotationProperty(since time.Time, rotating bool) console.Property {
	if rotating {
		return console.Property{Label: "Credential rotation", Value: "In progress since " + since.UTC().Format(time.RFC3339) +
			"; completes by itself seven days after it started", Action: "rotate-complete", ActionText: "Complete credential rotation"}
	}
	return console.Property{Label: "Credential rotation", Value: "Not in progress", Action: "rotate-start", ActionText: "Start credential rotation"}
}

func gkeRotationActions(rotating bool) []console.Action {
	start := console.Action{ID: "rotate-start", Label: "Start credential rotation", Title: "Start credential rotation", Submit: "Start rotation",
		Fields: []console.Field{{Name: "note", Label: "What happens", Type: "textarea", Immutable: true,
			Default: "A new cluster root CA is made. Until you complete the rotation, the API server and the nodes trust both the old and the new CA. " +
				"Update your clients (gcloud container clusters get-credentials), then complete the rotation."}}}
	complete := console.Action{ID: "rotate-complete", Label: "Complete credential rotation", Title: "Complete credential rotation", Submit: "Complete rotation",
		Fields: []console.Field{{Name: "note", Label: "What happens", Type: "textarea", Immutable: true,
			Default: "The old CA is no longer trusted: clients still using credentials of the old CA lose access, and the pods are re-created."}}}
	if rotating {
		start = console.Action{ID: "rotate-start", Label: "Start credential rotation", Unavailable: "A credential rotation is already in progress: complete it first."}
	} else {
		complete = console.Action{ID: "rotate-complete", Label: "Complete credential rotation", Unavailable: "No credential rotation is in progress."}
	}
	return []console.Action{start, complete}
}

func gkeHas[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func gkeLoggingText(c *containerpb.Cluster) string {
	comps := c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()
	var on []string
	if gkeHas(comps, containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS) {
		on = append(on, "System")
	}
	if gkeHas(comps, containerpb.LoggingComponentConfig_WORKLOADS) {
		on = append(on, "Workloads")
	}
	if len(on) == 0 {
		return "Disabled"
	}
	return strings.Join(on, ", ")
}

func gkeMonitoringText(c *containerpb.Cluster) string {
	if gkeHas(c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents(), containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS) {
		return "System"
	}
	return "Disabled"
}

func gkeTelemetryActions(c *containerpb.Cluster) []console.Action {
	lc := c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()
	return []console.Action{
		{ID: "logging", Label: "Edit logging", Title: "Edit Cloud Logging", Submit: "Save changes", Fields: []console.Field{
			{Name: "system", Label: "System logs", Type: "checkbox", Default: strconv.FormatBool(gkeHas(lc, containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS)),
				Help: "Logs of the kube-system and other GKE system namespaces, and of the control plane."},
			{Name: "workloads", Label: "Workload logs", Type: "checkbox", Default: strconv.FormatBool(gkeHas(lc, containerpb.LoggingComponentConfig_WORKLOADS)),
				Help: "Every other container's stdout and stderr, as k8s_container entries in Cloud Logging. Workload logs need system logs."},
		}},
		{ID: "monitoring", Label: "Edit Cloud Monitoring", Title: "Edit Cloud Monitoring", Submit: "Save changes", Fields: []console.Field{
			{Name: "system", Label: "System metrics", Type: "checkbox", Default: strconv.FormatBool(gkeHas(c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents(), containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS)),
				Help: "The kubernetes.io container, pod and node metrics, read from each node's kubelet every minute."},
		}},
	}
}

// gkeUpgradeInfoText is the automatic upgrade status and its targets.
func gkeUpgradeInfoText(info *containerpb.ClusterUpgradeInfo) string {
	if info == nil {
		return "—"
	}
	var parts []string
	for _, s := range info.GetAutoUpgradeStatus() {
		parts = append(parts, strings.ReplaceAll(strings.ToLower(s.String()), "_", " "))
	}
	for _, r := range info.GetPausedReason() {
		parts = append(parts, "paused by "+strings.ReplaceAll(strings.ToLower(r.String()), "_", " "))
	}
	if v := info.GetMinorTargetVersion(); v != "" {
		parts = append(parts, "minor target "+v)
	}
	if v := info.GetPatchTargetVersion(); v != "" {
		parts = append(parts, "patch target "+v)
	}
	if rs := info.GetRollbackSafeUpgradeStatus(); rs != nil {
		parts = append(parts, "rollback-safe upgrade from "+rs.GetPreviousVersion()+" until "+rs.GetControlPlaneUpgradeRollbackEndTime().AsTime().UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, "; ")
}

func gkeCompleteUpgradeAction(info *containerpb.ClusterUpgradeInfo) console.Action {
	rs := info.GetRollbackSafeUpgradeStatus()
	if rs == nil {
		return console.Action{ID: "complete-upgrade", Label: "Complete control plane upgrade",
			Unavailable: "No rollback-safe control plane upgrade is soaking: upgrade the control plane with a soak duration first."}
	}
	return console.Action{ID: "complete-upgrade", Label: "Complete control plane upgrade", Title: "Complete control plane upgrade", Submit: "Complete upgrade",
		Fields: []console.Field{{Name: "note", Label: "What happens", Type: "textarea", Immutable: true,
			Default: "The control plane stops emulating " + rs.GetPreviousVersion() + ". Afterwards you can no longer go back to the previous minor version."}}}
}

// moreSettingsAct saves the zones, rotation, logging, monitoring and
// complete upgrade dialogs.
func (p gkeClustersProvider) moreSettingsAct(ctx context.Context, name, act string, v map[string]string) error {
	api := p.gkeAPI()
	on := func(k string) bool { return v[k] == "true" || v[k] == "on" }
	switch act {
	case "zones":
		var zones []string
		for _, z := range strings.Split(v["zones"], ",") {
			if z = strings.TrimSpace(z); z != "" {
				zones = append(zones, z)
			}
		}
		_, err := api.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: zones})
		return gkeStatusErr(err)
	case "rotate-start":
		_, err := api.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name, RotateCredentials: true})
		return gkeStatusErr(err)
	case "rotate-complete":
		_, err := api.CompleteIPRotation(ctx, &containerpb.CompleteIPRotationRequest{Name: name})
		return gkeStatusErr(err)
	case "complete-upgrade":
		_, err := api.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
		return gkeStatusErr(err)
	case "logging":
		cfg := &containerpb.LoggingComponentConfig{}
		if on("system") {
			cfg.EnableComponents = append(cfg.EnableComponents, containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS)
		}
		if on("workloads") {
			cfg.EnableComponents = append(cfg.EnableComponents, containerpb.LoggingComponentConfig_WORKLOADS)
		}
		_, err := api.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
			DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: cfg}}})
		return gkeStatusErr(err)
	case "monitoring":
		cfg := &containerpb.MonitoringComponentConfig{}
		if on("system") {
			cfg.EnableComponents = append(cfg.EnableComponents, containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS)
		}
		_, err := api.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
			DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: cfg}}})
		return gkeStatusErr(err)
	}
	return fmt.Errorf("unknown action %s", act)
}
