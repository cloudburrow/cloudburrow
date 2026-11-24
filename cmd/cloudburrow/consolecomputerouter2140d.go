package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A Cloud Router's Named sets tab (#2140), beside its Route policies tab,
// and a Private Service Connect endpoint's Edit of its global access.
//
// Named sets are what Google's "Create named sets" and "Manage named sets"
// pages (network-connectivity/docs/router/how-to/bgp-route-policies/
// create-named-sets and update-named-sets) make and change: a set of type
// Prefix or Community, a name, and elements, each a CEL expression such as
// prefix('10.0.0.0/20').orLonger() or '64500:100'. Those pages show them
// through gcloud only (UNVERIFIED, re-read 2026-11-22: neither page has a
// Console tab), so the tab follows the Route policies tab's layout: the
// list, Create named set, and each set's Edit elements and Delete, over
// routers.updateNamedSet, patchNamedSet (under the set's fingerprint) and
// deleteNamedSet.
//
// An endpoint for a published service's Edit is Google's "Edit" on the
// endpoint's page, with Enable global access, then Save
// (vpc/docs/manage-endpoints-published-services, read 2026-11-22: "You can
// only update the global access field of endpoints for published
// services"), through forwardingRules.patch with the endpoint's
// fingerprint.

const namedSetsTab = "namedsets"

func namedSetTypeText(t string) string {
	switch t {
	case "NAMED_SET_TYPE_PREFIX":
		return "Prefix"
	case "NAMED_SET_TYPE_COMMUNITY":
		return "Community"
	}
	return vpcDash(t)
}

// namedSetElements are a set's elements' expressions.
func namedSetElements(ns map[string]any) []string {
	var out []string
	for _, e := range vmMaps(ns, "elements") {
		out = append(out, vpcStr(e, "expression"))
	}
	return out
}

// namedSets are the router's named sets, by name.
func (p computeRoutersProvider) namedSets(project string, path []string) ([]map[string]any, error) {
	m, err := p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1]+"/listNamedSets", nil, nil)
	if err != nil {
		return nil, err
	}
	return vmMaps(m, "result"), nil
}

// namedSetsSection is a Cloud Router's Named sets tab.
func (p computeRoutersProvider) namedSetsSection(project string, path []string) console.Section {
	l := console.Listing{Columns: []string{"Type", "Elements", "Description"}, Noun: "named sets",
		Note: "A named set is a list of prefixes or BGP communities that route policies' expressions refer to."}
	sets, err := p.namedSets(project, path)
	if err != nil {
		return console.Section{ID: namedSetsTab, Label: "Named sets", Unavailable: err.Error()}
	}
	for _, ns := range sets {
		name := vpcStr(ns, "name")
		els := namedSetElements(ns)
		l.Items = append(l.Items, console.Resource{Name: name, Target: []string{path[0], path[1], namedSetsTab, name},
			Fields: map[string]string{"Type": namedSetTypeText(vpcStr(ns, "type")), "Elements": vpcDash(strings.Join(els, ", ")),
				"Description": vpcDash(vpcStr(ns, "description"))},
			Actions: namedSetRowActions(name, ns)})
	}
	l.Total = len(l.Items)
	return console.Section{ID: namedSetsTab, Label: "Named sets", Listing: l}
}

const namedSetElementsHelp = "One CEL expression per line: for a prefix set, such as prefix('10.0.0.0/20').orLonger(); for a community set, such as '64500:100'."

// namedSetCreate is the router page's Create named set.
func namedSetCreate() console.Action {
	return console.Action{ID: "createNamedSet", Label: "Create named set", Title: "Create named set", Submit: "Create",
		Fields: []console.Field{
			{Name: "setName", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
			{Name: "setType", Label: "Set type", Type: "radio", Default: "NAMED_SET_TYPE_PREFIX",
				Options:      []string{"NAMED_SET_TYPE_PREFIX", "NAMED_SET_TYPE_COMMUNITY"},
				OptionLabels: map[string]string{"NAMED_SET_TYPE_PREFIX": "Prefix", "NAMED_SET_TYPE_COMMUNITY": "Community"}},
			{Name: "setDescription", Label: "Description", Type: "text"},
			{Name: "elements", Label: "Elements", Type: "textarea", Help: namedSetElementsHelp},
		}}
}

// namedSetRowActions are a named set's Edit elements and Delete.
func namedSetRowActions(name string, ns map[string]any) []console.Action {
	return []console.Action{
		{ID: "editNamedSet", Label: "Edit elements", Title: "Edit named set " + name, Submit: "Save",
			Fields: []console.Field{
				{Name: "setDescription", Label: "Description", Type: "text", Default: vpcStr(ns, "description")},
				{Name: "elements", Label: "Elements", Type: "textarea", Default: strings.Join(namedSetElements(ns), "\n"), Help: namedSetElementsHelp},
			}},
		{ID: "deleteNamedSet", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete named set " + name + "?"},
	}
}

// namedSetElementList is the elements a form's textarea gives.
func namedSetElementList(v string) []any {
	var out []any
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, map[string]any{"expression": line})
		}
	}
	return out
}

// actNamedSet runs Create named set on the router at path ([region,
// router]), and Edit elements or Delete on the set at [region, router,
// namedsets, name].
func (p computeRoutersProvider) actNamedSet(project string, path []string, action string, values map[string]string) error {
	base := "regions/" + path[0] + "/routers/" + path[1] + "/"
	switch {
	case len(path) == 2 && action == "createNamedSet":
		name := strings.TrimSpace(values["setName"])
		for _, ns := range p.namedSetsOrNil(project, path) {
			if vpcStr(ns, "name") == name {
				return fmt.Errorf("Name: the router already has a named set %s", name)
			}
		}
		body := map[string]any{"name": name, "type": vpcOr(values["setType"], "NAMED_SET_TYPE_PREFIX")}
		if d := strings.TrimSpace(values["setDescription"]); d != "" {
			body["description"] = d
		}
		if els := namedSetElementList(values["elements"]); len(els) > 0 {
			body["elements"] = els
		}
		_, err := p.call("POST", project, base+"updateNamedSet", nil, body)
		return err
	case len(path) == 4 && path[2] == namedSetsTab && action == "editNamedSet":
		got, err := p.call("GET", project, base+"getNamedSet", map[string][]string{"namedSet": {path[3]}}, nil)
		if err != nil {
			return err
		}
		cur, _ := got["resource"].(map[string]any)
		els := namedSetElementList(values["elements"])
		if els == nil {
			els = []any{}
		}
		body := map[string]any{"name": path[3], "type": vpcStr(cur, "type"), "fingerprint": vpcStr(cur, "fingerprint"),
			"description": strings.TrimSpace(values["setDescription"]), "elements": els}
		_, err = p.call("POST", project, base+"patchNamedSet", nil, body)
		return err
	case len(path) == 4 && path[2] == namedSetsTab && action == "deleteNamedSet":
		_, err := p.call("POST", project, base+"deleteNamedSet", map[string][]string{"namedSet": {path[3]}}, nil)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

func (p computeRoutersProvider) namedSetsOrNil(project string, path []string) []map[string]any {
	sets, _ := p.namedSets(project, path)
	return sets
}

// isNamedSetAction is whether ActAt's action at path is the Named sets tab's.
func isNamedSetAction(path []string, action string) bool {
	return (len(path) == 2 && action == "createNamedSet") ||
		(len(path) == 4 && path[2] == namedSetsTab && (action == "editNamedSet" || action == "deleteNamedSet"))
}

// --- A Private Service Connect endpoint's Edit --------------------------------

// pscEndpointEdit is an endpoint for a published service's Edit: Enable
// global access, then Save; an endpoint for Google APIs has none, as
// Google's cannot be updated.
func pscEndpointEdit(e map[string]any, path []string) *console.EditForm {
	if len(path) != 2 || path[0] == "global" {
		return nil
	}
	on := "false"
	if e["allowPscGlobalAccess"] == true {
		on = "true"
	}
	return &console.EditForm{Label: "Edit", Title: "Edit endpoint", Submit: "Save",
		Note: "Only an endpoint's global access can be updated; to change anything else, delete the endpoint and create a new one.",
		Fields: []console.Field{{Name: "allowPscGlobalAccess", Label: "Enable global access", Type: "checkbox", Default: on,
			Help: "Makes the endpoint available from any region."}}}
}

// Edit saves an endpoint for a published service's global access.
func (p computePSCEndpointsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 2 || path[0] == "global" {
		return errors.New("only an endpoint for a published service's global access can be updated")
	}
	e, err := p.endpoint(project, path)
	if err != nil {
		return err
	}
	_, err = p.call("PATCH", project, "regions/"+path[0]+"/forwardingRules/"+path[1], nil,
		map[string]any{"allowPscGlobalAccess": values["allowPscGlobalAccess"] == "true", "fingerprint": vpcStr(e, "fingerprint")})
	return err
}

// --- Publish service's Target details -------------------------------------------

// pscLBTypes are Publish service's load balancer types, by the scheme of
// their forwarding rules: a service attachment's target service is an
// internal load balancer's forwarding rule (#2153).
var pscLBTypes = map[string]string{
	"INTERNAL":         "Internal passthrough Network Load Balancer",
	"INTERNAL_MANAGED": "Regional internal Application Load Balancer or regional internal proxy Network Load Balancer",
}

// internalLBs are the project's internal load balancers' forwarding rules
// by "lbType=SCHEME", each its relative name, with labels "NAME (REGION)".
func (p computePSCPublishedProvider) internalLBs(project string) (map[string][]string, map[string]string) {
	byType := map[string][]string{}
	labels := map[string]string{}
	m, err := p.call("GET", project, "aggregated/forwardingRules", nil, nil)
	if err != nil {
		return byType, labels
	}
	for _, r := range aggregatedItems(m, "forwardingRules") {
		scheme := vpcStr(r, "loadBalancingScheme")
		if pscLBTypes[scheme] == "" {
			continue
		}
		region := vpcLast(vpcStr(r, "region"))
		rel := "projects/" + project + "/regions/" + region + "/forwardingRules/" + vpcStr(r, "name")
		byType["lbType="+scheme] = append(byType["lbType="+scheme], rel)
		labels[rel] = vpcStr(r, "name") + " (" + region + ")"
	}
	return byType, labels
}

// pscTargetFields are Publish service's Target details: Load balancer
// type, then the internal load balancer of that type (its forwarding
// rule). Without the project's rules (the form's static shape), the
// forwarding rule is typed.
func pscTargetFields(ilbs map[string][]string, labels map[string]string) []console.Field {
	typ := console.Field{Name: "lbType", Label: "Load balancer type", Type: "select", Default: "INTERNAL", Section: "Target details",
		Options: []string{"INTERNAL", "INTERNAL_MANAGED"}, OptionLabels: pscLBTypes}
	target := console.Field{Name: "targetService", Label: "Internal load balancer", Type: "select", Required: true, Section: "Target details",
		Options: union(ilbs), OptionsWhen: conditional(ilbs), OptionLabels: labels,
		Help: "The internal load balancer that hosts the service, by its forwarding rule in the service's region."}
	if ilbs == nil {
		target.Type = "text"
		target.Help = "The internal load balancer's forwarding rule: projects/PROJECT/regions/REGION/forwardingRules/NAME."
	}
	return []console.Field{typ, target}
}
