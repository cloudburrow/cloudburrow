package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A managed instance group's Update VMs (#2139), as compute/docs/
// instance-groups/rolling-out-updates-to-managed-instance-groups (read
// 2026-11-22) draws it: the New template (and an optional second, canary
// template with its target size), Update type Automatic (PROACTIVE) or
// Selective (OPPORTUNISTIC), the Minimal action and Most disruptive
// allowed action, Maximum surge and Maximum unavailable (a number or a
// percent), and the Replacement method. Saving patches the group's
// versions and updatePolicy; an Automatic update then replaces, restarts
// or refreshes the VMs not on their version, a few at a time. The checked
// VMs' Update applies a Selective update to them
// (applyUpdatesToInstances).

var updateActions = []string{"REFRESH", "RESTART", "REPLACE"}

var updateActionLabels = map[string]string{"NONE": "No action", "REFRESH": "Refresh (update metadata only)",
	"RESTART": "Restart", "REPLACE": "Replace"}

func (p computeGroupsProvider) updateVMsAction(project string, mgr map[string]any) console.Action {
	up, _ := mgr["updatePolicy"].(map[string]any)
	typ := "Automatic"
	if vpcStr(up, "type") != "PROACTIVE" {
		typ = "Selective"
	}
	fp := func(k, def string) string {
		m, _ := up[k].(map[string]any)
		if v, ok := m["percent"].(float64); ok {
			return strconv.Itoa(int(v)) + "%"
		}
		if v, ok := m["fixed"].(float64); ok {
			return strconv.Itoa(int(v))
		}
		return def
	}
	zones := "1"
	if vpcStr(mgr, "region") != "" {
		zones = "3"
		if dp, _ := mgr["distributionPolicy"].(map[string]any); dp != nil {
			zones = strconv.Itoa(len(vmMaps(dp, "zones")))
		}
	}
	method := "Substitute"
	if vpcStr(up, "replacementMethod") == "RECREATE" {
		method = "Recreate"
	}
	templates := p.templateOptions(project)
	automatic := map[string]string{"updateType": "Automatic"}
	canary, canarySize := "", ""
	for _, v := range vmMaps(mgr, "versions") {
		ts, _ := v["targetSize"].(map[string]any)
		if ts["fixed"] == nil && ts["percent"] == nil {
			continue
		}
		canary = groupTemplateTarget(map[string]any{"instanceTemplate": vpcStr(v, "instanceTemplate")})
		if f, ok := ts["fixed"].(float64); ok {
			canarySize = strconv.Itoa(int(f))
		} else if pc, ok := ts["percent"].(float64); ok {
			canarySize = strconv.Itoa(int(pc)) + "%"
		}
	}
	return console.Action{ID: "updatevms", Label: "Update VMs", Submit: "Update VMs", Title: "Update VMs in " + vpcStr(mgr, "name"), Fields: []console.Field{
		selectOrText(console.Field{Name: "template", Label: "New template", Required: true, Default: groupTemplateTarget(mgr), Section: "New template",
			Help: "The template the group's VMs are brought to, and new VMs are made from."}, templates, "LOCATION/NAME of an instance template."),
		selectOrText(console.Field{Name: "canaryTemplate", Label: "Second template (canary)", Default: canary, Section: "New template",
			Help: "Optional: a second version, run by a target size of the group's VMs; the rest run the new template."}, append([]string{""}, templates...), "LOCATION/NAME, or empty."),
		{Name: "canarySize", Label: "Target size of the second template", Type: "text", Default: canarySize, Section: "New template", Pattern: `^$|^[0-9]{1,4}%?$`,
			Help: "A number of VMs, or a percent such as 20%."},
		{Name: "updateType", Label: "Update type", Type: "radio", Default: typ, Options: []string{"Automatic", "Selective"}, Section: "Update configuration",
			OptionLabels: map[string]string{"Automatic": "Automatic: the group updates its VMs now",
				"Selective": "Selective: VMs are updated when they are replaced, or when you update them"}},
		{Name: "minimalAction", Label: "When instance configuration requires", Type: "select", Default: vpcOr(vpcStr(up, "minimalAction"), "REPLACE"),
			Options: updateActions, OptionLabels: updateActionLabels, Section: "Update configuration",
			Help: "The least the group does to each VM it updates; a template change beyond metadata always needs Replace."},
		{Name: "mostDisruptiveAllowedAction", Label: "Most disruptive allowed action", Type: "select", Default: vpcOr(vpcStr(up, "mostDisruptiveAllowedAction"), "REPLACE"),
			Options: updateActions, OptionLabels: updateActionLabels, Section: "Update configuration",
			Help: "A VM whose update needs more than this is left as it is."},
		{Name: "maxSurge", Label: "Maximum surge", Type: "text", Default: fp("maxSurge", zones), Section: "Update configuration", Pattern: `^[0-9]{1,4}%?$`, VisibleWhen: automatic,
			Help: "How many VMs the group may make above its target size during the update: a number or a percent."},
		{Name: "maxUnavailable", Label: "Maximum unavailable", Type: "text", Default: fp("maxUnavailable", zones), Section: "Update configuration", Pattern: `^[0-9]{1,4}%?$`, VisibleWhen: automatic,
			Help: "How many VMs may be offline at once during the update: a number or a percent."},
		{Name: "replacementMethod", Label: "Replacement method", Type: "radio", Default: method, Options: []string{"Substitute", "Recreate"}, Section: "Update configuration",
			OptionLabels: map[string]string{"Substitute": "Substitute: new VMs with new names", "Recreate": "Recreate: keep the VMs' names (Maximum surge 0)"}},
	}}
}

// fixedOrPercentValue is "3" as {fixed: 3} and "20%" as {percent: 20}.
func fixedOrPercentValue(field, v string) (map[string]any, error) {
	v = strings.TrimSpace(v)
	pct := strings.HasSuffix(v, "%")
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%s: %q is not a number or a percent", field, v)
	}
	if pct {
		return map[string]any{"percent": float64(n)}, nil
	}
	return map[string]any{"fixed": float64(n)}, nil
}

// updateVMsBody is the patch Update VMs sends.
func updateVMsBody(values map[string]string) (map[string]any, error) {
	ref := func(field, v string) (string, error) {
		loc, tpl, ok := strings.Cut(strings.TrimSpace(v), "/")
		if !ok {
			return "", fmt.Errorf("%s: %q is not LOCATION/NAME", field, v)
		}
		return templateBase([]string{loc, tpl}), nil
	}
	main, err := ref("New template", values["template"])
	if err != nil {
		return nil, err
	}
	versions := []any{map[string]any{"instanceTemplate": main}}
	if c := strings.TrimSpace(values["canaryTemplate"]); c != "" {
		ct, err := ref("Second template", c)
		if err != nil {
			return nil, err
		}
		size, err := fixedOrPercentValue("Target size of the second template", vpcOr(values["canarySize"], "1"))
		if err != nil {
			return nil, err
		}
		versions = append(versions, map[string]any{"instanceTemplate": ct, "targetSize": size})
	}
	up := map[string]any{"type": "OPPORTUNISTIC", "minimalAction": vpcOr(values["minimalAction"], "REPLACE"),
		"mostDisruptiveAllowedAction": vpcOr(values["mostDisruptiveAllowedAction"], "REPLACE"), "replacementMethod": "SUBSTITUTE"}
	if values["replacementMethod"] == "Recreate" {
		up["replacementMethod"] = "RECREATE"
	}
	if values["updateType"] == "Automatic" {
		up["type"] = "PROACTIVE"
		for _, k := range []string{"maxSurge", "maxUnavailable"} {
			if v := strings.TrimSpace(values[k]); v != "" {
				fp, err := fixedOrPercentValue(map[string]string{"maxSurge": "Maximum surge", "maxUnavailable": "Maximum unavailable"}[k], v)
				if err != nil {
					return nil, err
				}
				up[k] = fp
			}
		}
		if up["replacementMethod"] == "RECREATE" && strings.TrimSpace(values["maxSurge"]) == "" {
			up["maxSurge"] = map[string]any{"fixed": 0.0}
		}
	}
	return map[string]any{"versions": versions, "updatePolicy": up}, nil
}

// updateSelectedAction is the checked VMs' Update: a Selective update
// applied to them now.
func updateSelectedAction() console.Action {
	return console.Action{ID: "applyupdates", Label: "Update", SelectionField: groupVMsField, Submit: "Update",
		Title: "Update the checked VMs", Fields: []console.Field{
			{Name: groupVMsField, Type: "hidden"},
			{Name: "minimalAction", Label: "Minimal action", Type: "select", Default: "NONE", Options: append([]string{"NONE"}, updateActions...), OptionLabels: updateActionLabels,
				Help: "No action does only what each VM's update needs."},
			{Name: "mostDisruptiveAllowedAction", Label: "Most disruptive allowed action", Type: "select", Default: "REPLACE", Options: updateActions, OptionLabels: updateActionLabels},
		}}
}
