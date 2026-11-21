package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
