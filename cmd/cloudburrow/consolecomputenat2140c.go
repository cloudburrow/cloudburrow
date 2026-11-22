package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Private NAT in Create Cloud NAT gateway and a gateway's Edit, and a
// Cloud Router's Route policies tab (#2140), as Google's console has them:
// NAT type Private hides the NAT IP address, network tier and
// Endpoint-Independent Mapping settings and asks for a Cloud NAT rule (its
// number, its next hop, a Network Connectivity Center hub or hybrid
// connectivity, and the Private NAT subnet of purpose PRIVATE_NAT it
// translates to); the router's Route policies tab lists its import and
// export policies with their terms.

// natPublicOnly are Create Cloud NAT gateway's fields a Private NAT
// gateway has none of.
var natPublicOnly = map[string]bool{"natIpAllocateOption": true, "natIps": true, "autoNetworkTier": true, "enableEndpointIndependentMapping": true}

const natPrivateHubPrefix = "//networkconnectivity.googleapis.com/"

// natTypeFields adapts fields to the NAT type: on Create, public-only
// fields show for Public and the rule fields for Private; on a private
// gateway's Edit, the public-only fields go.
func natTypeFields(c natChoices, cur map[string]any, fields []console.Field) []console.Field {
	private := cur != nil && vpcStr(cur, "type") == "PRIVATE"
	if cur != nil && !private {
		return fields
	}
	const rules = "Cloud NAT rules"
	var out []console.Field
	for _, f := range fields {
		if natPublicOnly[f.Name] {
			if private {
				continue
			}
			vw := map[string]string{"type": "PUBLIC"}
			for k, v := range f.VisibleWhen {
				vw[k] = v
			}
			f.VisibleWhen = vw
		}
		out = append(out, f)
	}
	num, hop, hub, sub := "100", "HYBRID", "", ""
	if private {
		if rs := vmMaps(cur, "rules"); len(rs) > 0 {
			num = vpcStr(rs[0], "ruleNumber")
			m := vpcStr(rs[0], "match")
			if i := strings.Index(m, natPrivateHubPrefix); i >= 0 {
				hop = "HUB"
				hub = strings.TrimRight(m[i+len(natPrivateHubPrefix):], `"') `)
			}
			if a, _ := rs[0]["action"].(map[string]any); len(vpcStrs(a, "sourceNatActiveRanges")) > 0 {
				sub = vpcLast(vpcStrs(a, "sourceNatActiveRanges")[0])
			}
		}
	}
	when := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		if !private {
			m["type"] = "PRIVATE"
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	subField := console.Field{Name: "natSubnet", Label: "Private NAT subnet", Type: "select", Required: true, Section: rules, Default: sub,
		Options: union(c.natSubnets), OptionsWhen: conditional(c.natSubnets), VisibleWhen: when(nil),
		Help: "A subnet of purpose PRIVATE_NAT in the gateway's network and region; its range is the source of the translated packets."}
	if c.networks == nil {
		subField.Type = "text"
	}
	return append(out,
		console.Field{Name: "ruleNumber", Label: "Rule number", Type: "text", Required: true, Section: rules, Default: num, Pattern: `^[0-9]+$`,
			VisibleWhen: when(nil), Help: "0 (highest priority) to 65000."},
		console.Field{Name: "nextHop", Label: "Next hop", Type: "radio", Section: rules, Default: hop, Options: []string{"HYBRID", "HUB"},
			OptionLabels: map[string]string{"HYBRID": "Hybrid connectivity (Cloud VPN, Cloud Interconnect)", "HUB": "Network Connectivity Center hub"},
			VisibleWhen:  when(nil)},
		console.Field{Name: "hub", Label: "Hub", Type: "text", Required: true, Section: rules, Default: hub, VisibleWhen: when(map[string]string{"nextHop": "HUB"}),
			Pattern: `^projects/[a-z0-9-]+/locations/global/hubs/[a-z]([-a-z0-9]*[a-z0-9])?$`,
			Help:    "projects/PROJECT_ID/locations/global/hubs/HUB. Network Connectivity Center is not served, so the hub is recorded as given."},
		subField)
}

// privateNatBody is the Private NAT gateway the form's values make.
func privateNatBody(project, region, name string, values map[string]string) (map[string]any, error) {
	n := map[string]any{"name": name, "type": "PRIVATE", "sourceSubnetworkIpRangesToNat": vpcOr(values["source"], "ALL_SUBNETWORKS_ALL_IP_RANGES"),
		"enableDynamicPortAllocation": values["enableDynamicPortAllocation"] != "false"}
	if n["sourceSubnetworkIpRangesToNat"] == "LIST_OF_SUBNETWORKS" {
		rows, err := console.ParseGroup(values["subnets"])
		if err != nil {
			return nil, err
		}
		var subs []any
		for _, r := range rows {
			subs = append(subs, map[string]any{"name": "regions/" + region + "/subnetworks/" + strings.TrimSpace(r["subnet"]),
				"sourceIpRangesToNat": []any{vpcOr(r["ranges"], "ALL_IP_RANGES")}})
		}
		n["subnetworks"] = subs
	}
	num, err := strconv.Atoi(vpcOr(strings.TrimSpace(values["ruleNumber"]), "100"))
	if err != nil {
		return nil, fmt.Errorf("Rule number: enter a number")
	}
	match := "nexthop.is_hybrid"
	if values["nextHop"] == "HUB" {
		match = `nexthop.hub == "` + natPrivateHubPrefix + strings.TrimSpace(values["hub"]) + `"`
	}
	sub := strings.TrimSpace(values["natSubnet"])
	if sub == "" {
		return nil, fmt.Errorf("Private NAT subnet: choose a subnet of purpose PRIVATE_NAT")
	}
	n["rules"] = []any{map[string]any{"ruleNumber": float64(num), "match": match,
		"action": map[string]any{"sourceNatActiveRanges": []any{"projects/" + project + "/regions/" + region + "/subnetworks/" + sub}}}}
	if l := values["logging"]; l != "" && l != "none" {
		n["logConfig"] = map[string]any{"enable": true, "filter": l}
	}
	dyn := n["enableDynamicPortAllocation"] == true
	for _, k := range []string{"minPortsPerVm", "maxPortsPerVm", "udpIdleTimeoutSec", "icmpIdleTimeoutSec", "tcpEstablishedIdleTimeoutSec",
		"tcpTransitoryIdleTimeoutSec", "tcpTimeWaitTimeoutSec"} {
		v := strings.TrimSpace(values[k])
		if v == "" || (k == "maxPortsPerVm" && !dyn) {
			continue
		}
		if k == "minPortsPerVm" && dyn && v == "64" {
			v = "32"
		}
		i, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s: enter a number", k)
		}
		n[k] = float64(i)
	}
	return n, nil
}

// routePoliciesSection is a Cloud Router's Route policies tab.
func (p computeRoutersProvider) routePoliciesSection(project string, path []string) console.Section {
	l := console.Listing{Columns: []string{"Type", "Terms", "Description"}, Noun: "route policies",
		Note: "No BGP session comes up locally, so policies are recorded and applied to no route; a BGP session's import and export policies name them."}
	m, err := p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1]+"/listRoutePolicies", nil, nil)
	if err != nil {
		return console.Section{ID: "routepolicies", Label: "Route policies", Unavailable: err.Error()}
	}
	for _, rp := range vmMaps(m, "result") {
		typ := "Import"
		if vpcStr(rp, "type") == "ROUTE_POLICY_TYPE_EXPORT" {
			typ = "Export"
		}
		var terms []string
		for _, t := range vmMaps(rp, "terms") {
			match := "all routes"
			if mm, _ := t["match"].(map[string]any); vpcStr(mm, "expression") != "" {
				match = vpcStr(mm, "expression")
			}
			var acts []string
			for _, a := range vmMaps(t, "actions") {
				acts = append(acts, vpcStr(a, "expression"))
			}
			terms = append(terms, vpcStr(t, "priority")+": "+match+" → "+strings.Join(acts, "; "))
		}
		l.Items = append(l.Items, console.Resource{Name: vpcStr(rp, "name"), Fields: map[string]string{"Type": typ,
			"Terms": vpcDash(strings.Join(terms, " | ")), "Description": vpcDash(vpcStr(rp, "description"))}})
	}
	l.Total = len(l.Items)
	return console.Section{ID: "routepolicies", Label: "Route policies", Listing: l}
}
