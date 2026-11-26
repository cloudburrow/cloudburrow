package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Autohealing on Create instance group and a group's page (#2225), as
// Google's console has it (compute/docs/instance-groups/autohealing-
// instances-in-migs, "Console" steps, read 2026-11-24): an Autohealing
// section whose Health check is "No health check" or one of the project's
// health checks, and an Initial delay in seconds (0 to 3600, 300 in the
// console). The group's page shows the policy in its Details, a Health
// check status column on its VMs, and Edit autohealing.

const noHealthCheck = "No health check"

// healthCheckOptions are the project's health checks as LOCATION/NAME:
// global/NAME, or REGION/NAME for a regional one.
func (p computeGroupsProvider) healthCheckOptions(project string) []string {
	var out []string
	l, err := p.call("GET", project, "aggregated/healthChecks", nil, nil)
	if err != nil {
		return nil
	}
	for scope, v := range vmMap(l, "items") {
		sv, _ := v.(map[string]any)
		for _, hc := range vmMaps(sv, "healthChecks") {
			loc := "global"
			if r, ok := strings.CutPrefix(scope, "regions/"); ok {
				loc = r
			}
			out = append(out, loc+"/"+vpcStr(hc, "name"))
		}
	}
	sort.Strings(out)
	return out
}

// autohealingFields are the Autohealing section, set from the group's
// policy (nil for a new group).
func autohealingFields(mgr map[string]any, hcs []string, section string, when map[string]string) []console.Field {
	hc, delay := noHealthCheck, "300"
	if pols := vmMaps(mgr, "autoHealingPolicies"); len(pols) > 0 {
		hc = healthCheckTarget(vpcStr(pols[0], "healthCheck"))
		delay = strconv.Itoa(int(vpcNum(pols[0], "initialDelaySec")))
	}
	opts := append([]string{noHealthCheck}, hcs...)
	if hc != noHealthCheck && !hasString(opts, hc) {
		opts = append(opts, hc)
	}
	f := []console.Field{
		{Name: "healthCheck", Label: "Health check", Type: "select", Default: hc, Options: opts, Section: section, VisibleWhen: when,
			Help: "The group recreates a VM its health check finds unhealthy. Allow Google's probe ranges, 35.191.0.0/16 and 130.211.0.0/22, on the probed port."},
	}
	if len(opts) > 1 {
		f = append(f, console.Field{Name: "initialDelay", Label: "Initial delay", Type: "text", Default: delay, Pattern: `^[0-9]{1,4}$`,
			Section: section, VisibleWhen: when, ShowWhen: "healthCheck=" + strings.Join(opts[1:], "|"),
			Help: "Seconds a new VM has to start before its health check counts: 0 to 3600."})
	}
	return f
}

// healthCheckTarget is a health check link as LOCATION/NAME.
func healthCheckTarget(link string) string {
	rel := relInProject(link)
	if loc, name, ok := strings.Cut(rel, "/healthChecks/"); ok {
		return strings.TrimPrefix(loc, "regions/") + "/" + name
	}
	return rel
}

// autohealingPolicies reads the Autohealing section: nil for No health
// check.
func autohealingPolicies(values map[string]string) ([]any, error) {
	hc := strings.TrimSpace(vpcOr(values["healthCheck"], noHealthCheck))
	if hc == noHealthCheck {
		return []any{}, nil
	}
	loc, name, ok := strings.Cut(hc, "/")
	if !ok || name == "" {
		return nil, fmt.Errorf("health check: %q is not LOCATION/NAME", hc)
	}
	delay, err := strconv.Atoi(strings.TrimSpace(vpcOr(values["initialDelay"], "300")))
	if err != nil || delay < 0 || delay > 3600 {
		return nil, errors.New("initial delay: must be 0 to 3600 seconds")
	}
	link := "global/healthChecks/" + name
	if loc != "global" {
		link = "regions/" + loc + "/healthChecks/" + name
	}
	return []any{map[string]any{"healthCheck": link, "initialDelaySec": float64(delay)}}, nil
}

// autohealingProps are a group's Autohealing details.
func autohealingProps(mgr map[string]any) []console.Property {
	pols := vmMaps(mgr, "autoHealingPolicies")
	if len(pols) == 0 {
		return []console.Property{{Label: "Health check", Value: noHealthCheck}}
	}
	return []console.Property{{Label: "Health check", Value: healthCheckTarget(vpcStr(pols[0], "healthCheck"))},
		{Label: "Initial delay", Value: strconv.Itoa(int(vpcNum(pols[0], "initialDelaySec"))) + " seconds"}}
}

// healthStatusLabel is a managed instance's Health check status.
func healthStatusLabel(mi map[string]any) string {
	ih := vmMaps(mi, "instanceHealth")
	if len(ih) == 0 {
		return "—"
	}
	s := vpcStr(ih[0], "detailedHealthState")
	return map[string]string{"HEALTHY": "Healthy", "UNHEALTHY": "Unhealthy", "TIMEOUT": "Timeout", "DRAINING": "Draining", "UNKNOWN": "Unknown"}[s]
}

func (p computeGroupsProvider) autohealingAction(project string, mgr map[string]any) console.Action {
	return console.Action{ID: "editautohealing", Label: "Edit autohealing", Submit: "Save", Fields: autohealingFields(mgr, p.healthCheckOptions(project), "", nil)}
}

// setAutohealing is Edit autohealing: the group patched with the policy,
// or with none.
func (p computeGroupsProvider) setAutohealing(project, mg string, values map[string]string) error {
	pols, err := autohealingPolicies(values)
	if err != nil {
		return err
	}
	_, err = p.call("PATCH", project, mg, nil, map[string]any{"autoHealingPolicies": pols})
	return err
}
