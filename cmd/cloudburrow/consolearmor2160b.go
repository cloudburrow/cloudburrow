package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Armor's other tabs (#2160): network edge security services
// (compute.networkEdgeSecurityServices, one a region, naming a
// CLOUD_ARMOR_NETWORK policy that then filters the packets of the region's
// external passthrough Network Load Balancers, protocol forwarding and VMs
// with external addresses, as armor/docs/advanced-network-ddos describes
// it) and the preconfigured WAF rule sets
// (securityPolicies.listPreconfiguredExpressionSets, armor/docs/waf-rules).
// Google's docs give gcloud only for both (re-read 2026-11-22:
// advanced-network-ddos, waf-rules, rule-tuning): the tabs' names, columns
// and forms are UNVERIFIED.

// --- Network edge security services ----------------------------------------

type armorEdgeServicesProvider struct{ computeScreen }

func (armorEdgeServicesProvider) ID() string    { return "netsecurity-armor-edge" }
func (armorEdgeServicesProvider) Title() string { return "Cloud Armor policies" }

const armorEdgeNote = "A network edge security service applies its network edge security policy (type CLOUD_ARMOR_NETWORK) to every external passthrough Network Load Balancer of its region: " +
	"the load balancers' proxy on this machine filters their packets by the policy's network match. A region has at most one."

func (p armorEdgeServicesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Security policy", "Description"}, NameColumn: "Name", Noun: "network edge security services", Note: armorEdgeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Network edge security services are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, o := range p.lbAll(project, "networkEdgeSecurityServices", false, true) {
		path := lbScopePath(o)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields:  map[string]string{"Region": path[0], "Security policy": vpcDash(vpcLast(vpcStr(o, "securityPolicy"))), "Description": vpcDash(vpcStr(o, "description"))},
			Actions: []console.Action{armorEdgeDelete(path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func armorEdgeDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete network edge security service " + path[1] + "? Its policy then no longer filters the region's packets."}
}

func (armorEdgeServicesProvider) CreateOnPage() bool  { return true }
func (armorEdgeServicesProvider) CreateTitle() string { return "Create network edge security service" }

func (p armorEdgeServicesProvider) CreateForm() (string, []console.Field) {
	return "Create", armorEdgeFields(nil, nil)
}

func (p armorEdgeServicesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return armorEdgeFields(p.networkPolicies(project), nil)
}

// networkPolicies are the project's CLOUD_ARMOR_NETWORK policies, as
// regions/R/securityPolicies/NAME.
func (p armorEdgeServicesProvider) networkPolicies(project string) []string {
	var out []string
	for _, o := range p.lbAll(project, "securityPolicies", false, true) {
		if vpcStr(o, "type") == "CLOUD_ARMOR_NETWORK" {
			sp := lbScopePath(o)
			out = append(out, lbScopeColl(sp[0], "securityPolicies")+"/"+sp[1])
		}
	}
	sort.Strings(out)
	return out
}

func armorEdgeFields(policies []string, o map[string]any) []console.Field {
	pf := console.Field{Name: "securityPolicy", Label: "Security policy", Type: "text", Default: lbRel(vpcStr(o, "securityPolicy")),
		Help: "A network edge security policy (CLOUD_ARMOR_NETWORK) of the service's region: regions/REGION/securityPolicies/NAME."}
	if policies != nil {
		pf.Type, pf.Options = "select", append([]string{""}, policies...)
		if d := pf.Default; d != "" {
			pf.Default = d[strings.Index(d, "regions/"):]
		}
	}
	if o != nil {
		return []console.Field{{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description")}, pf}
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options},
		pf,
	}
}

func (p armorEdgeServicesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	if name == "" {
		return "", errors.New("Name: give the service a name")
	}
	region := vpcOr(v["region"], "us-central1")
	body := map[string]any{"name": name}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if sp := strings.TrimSpace(v["securityPolicy"]); sp != "" {
		body["securityPolicy"] = "projects/" + project + "/" + sp
	}
	if _, err := p.call("POST", project, "regions/"+region+"/networkEdgeSecurityServices", nil, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p armorEdgeServicesProvider) service(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a network edge security service is addressed by its region and name, not %v", path)
	}
	return p.call("GET", project, "regions/"+path[0]+"/networkEdgeSecurityServices/"+path[1], nil, nil)
}

func (p armorEdgeServicesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	o, err := p.service(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Region", Value: path[0]}, {Label: "Security policy", Value: vpcDash(vpcLast(vpcStr(o, "securityPolicy")))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "creationTimestamp"))}}
	return console.Detail{Title: path[1], Heading: "Network edge security service details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}, Note: armorEdgeNote}},
		Edit:     &console.EditForm{Label: "Edit", Title: "Edit network edge security service", Submit: "Save", Fields: armorEdgeFields(p.networkPolicies(project), o)}}, nil
}

func (p armorEdgeServicesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := armorEdgeDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

// Edit patches the description and the policy (an empty policy removes
// it), under the service's fingerprint.
func (p armorEdgeServicesProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	o, err := p.service(project, path)
	if err != nil {
		return err
	}
	body := map[string]any{"description": strings.TrimSpace(v["description"]), "fingerprint": vpcStr(o, "fingerprint")}
	sp := strings.TrimSpace(v["securityPolicy"])
	if sp != "" && !strings.HasPrefix(sp, "projects/") {
		sp = "projects/" + project + "/" + sp
	}
	body["securityPolicy"] = sp
	_, err = p.call("PATCH", project, "regions/"+path[0]+"/networkEdgeSecurityServices/"+path[1], nil, body)
	return err
}

func (p armorEdgeServicesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, "regions/"+path[0]+"/networkEdgeSecurityServices/"+path[1], nil, nil)
	return err
}

// --- Preconfigured WAF rules -------------------------------------------------

type armorWAFProvider struct{ computeScreen }

func (armorWAFProvider) ID() string    { return "netsecurity-armor-waf" }
func (armorWAFProvider) Title() string { return "Cloud Armor policies" }

const armorWAFNote = "Each rule set is used in a rule's Advanced mode expression, such as evaluatePreconfiguredWaf('sqli-v33-stable', {'sensitivity': 1}): " +
	"its signatures at or below the sensitivity level are evaluated, and any can be opted out of by its ID."

// wafSets are listPreconfiguredExpressionSets' rule sets.
func (p armorWAFProvider) wafSets(project string) ([]map[string]any, error) {
	m, err := p.call("GET", project, "global/securityPolicies/listPreconfiguredExpressionSets", nil, nil)
	if err != nil {
		return nil, err
	}
	return vmMaps(vmMap(vmMap(m, "preconfiguredExpressionSets"), "wafRules"), "expressionSets"), nil
}

func wafMaxSensitivity(set map[string]any) int {
	max := 0
	for _, e := range vmMaps(set, "expressions") {
		if n, _ := strconv.Atoi(num2s(e, "sensitivity")); n > max {
			max = n
		}
	}
	return max
}

func (p armorWAFProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Signatures", "Sensitivity levels", "Aliases", "Expression"}, NameColumn: "Rule set", Noun: "preconfigured WAF rule sets", Note: armorWAFNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "The preconfigured WAF rules are listed for a project. Choose one in the toolbar."
		return listing, nil
	}
	sets, err := p.wafSets(project)
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, set := range sets {
		id := vpcStr(set, "id")
		listing.Items = append(listing.Items, console.Resource{Name: id, Opens: []string{id}, Target: []string{id},
			Fields: map[string]string{"Signatures": strconv.Itoa(len(vmMaps(set, "expressions"))), "Sensitivity levels": wafLevels(set),
				"Aliases": vpcDash(strings.Join(vpcStrs(set, "aliases"), ", ")), "Expression": "evaluatePreconfiguredWaf('" + id + "')"}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func wafLevels(set map[string]any) string {
	if n := wafMaxSensitivity(set); n > 0 {
		return "1 to " + strconv.Itoa(n)
	}
	return "—"
}

func (p armorWAFProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 1 {
		return console.Detail{Unavailable: "a rule set is addressed by its name"}, nil
	}
	sets, err := p.wafSets(project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	for _, set := range sets {
		if vpcStr(set, "id") != path[0] {
			continue
		}
		sigs := console.Listing{Columns: []string{"Sensitivity level"}, NameColumn: "Signature ID", Noun: "signatures", Items: []console.Resource{}}
		for _, e := range vmMaps(set, "expressions") {
			sigs.Items = append(sigs.Items, console.Resource{Name: vpcStr(e, "id"), Fields: map[string]string{"Sensitivity level": vpcDash(num2s(e, "sensitivity"))}})
		}
		sigs.Total = len(sigs.Items)
		props := []console.Property{{Label: "Rule set", Value: path[0]}, {Label: "Signatures", Value: strconv.Itoa(len(sigs.Items))},
			{Label: "Sensitivity levels", Value: wafLevels(set)}, {Label: "Aliases", Value: vpcDash(strings.Join(vpcStrs(set, "aliases"), ", "))},
			{Label: "Expression", Value: "evaluatePreconfiguredWaf('" + path[0] + "')"}}
		return console.Detail{Title: path[0], Heading: "Rule set details",
			Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}, Note: armorWAFNote},
				{ID: "signatures", Label: "Signatures", Listing: sigs}}}, nil
	}
	return console.Detail{Unavailable: "no preconfigured rule set " + path[0]}, nil
}
