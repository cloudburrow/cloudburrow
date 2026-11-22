package main

import (
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Create an instance in a Shared VPC service project (#2140), as
// vpc/docs/provisioning-shared-vpc "Create an instance in a service
// project" (read 2026-11-22) draws it: under Networking's Network
// interfaces, the radio "Networks in this project" or "Networks shared with
// me (from host project: H)", the latter with a Shared subnetwork list of
// the host's subnets (projects.getXpnHost, then the host's subnetworks).
// The instance's interface then names the host's network and subnet, which
// the API accepts from an attached service project (internal/service/
// compute/xpn.go).

const (
	vmNetLocal  = "local"
	vmNetShared = "shared"
)

// xpnHost is the Shared VPC host project the project is attached to, or
// "".
func (p computeInstancesProvider) xpnHost(project string) string {
	h, err := p.call("GET", project, "getXpnHost", nil, nil)
	if err != nil {
		return ""
	}
	return vpcStr(h, "name")
}

// withSharedVPC adds, in a service project, the Network interfaces radio
// and the host's Shared subnetwork list; the project's own Network and
// Subnetwork then show for "Networks in this project" only.
func (p computeInstancesProvider) withSharedVPC(project string, fields []console.Field) []console.Field {
	host := p.xpnHost(project)
	if host == "" {
		return fields
	}
	subs := computeSharedVPCProvider(p).hostSubnets(host)
	var opts []string
	labels := map[string]string{}
	for _, s := range subs {
		opt := vpcLast(vpcStr(s, "region")) + "/" + vpcStr(s, "name")
		opts = append(opts, opt)
		labels[opt] = vpcStr(s, "name") + " (" + vpcLast(vpcStr(s, "region")) + ", network " + vpcLast(vpcStr(s, "network")) + ", " + vpcStr(s, "ipCidrRange") + ")"
	}
	local := map[string]string{"networkSource": vmNetLocal}
	shared := console.Field{Name: "sharedSubnetwork", Label: "Shared subnetwork", Type: "select", Section: "Networking", Required: true,
		Options: opts, OptionLabels: labels, VisibleWhen: map[string]string{"networkSource": vmNetShared},
		Help: "A subnet of host project " + host + " in the instance's region; the instance is on the host's network, under its firewall rules."}
	if len(opts) == 0 {
		shared.Type, shared.Required = "text", false
		shared.Help = "Host project " + host + " has no subnets to share."
	}
	radio := console.Field{Name: "networkSource", Label: "Network interfaces", Type: "radio", Section: "Networking", Default: vmNetLocal,
		Options: []string{vmNetLocal, vmNetShared},
		OptionLabels: map[string]string{vmNetLocal: "Networks in this project",
			vmNetShared: "Networks shared with me (from host project: " + host + ")"}}
	var out []console.Field
	for _, f := range fields {
		switch f.Name {
		case "network":
			f.VisibleWhen = local
			out = append(out, radio, f)
		case "subnetwork":
			f.VisibleWhen = local
			out = append(out, f, shared)
		default:
			out = append(out, f)
		}
	}
	return out
}

// sharedNIC is the network interface on the host's subnet that values'
// sharedSubnetwork (region/name) names, for an instance in zone.
func (p computeInstancesProvider) sharedNIC(project, zone string, values map[string]string) (map[string]any, error) {
	host := p.xpnHost(project)
	if host == "" {
		return nil, fmt.Errorf("Network interfaces: project %s is not attached to a Shared VPC host project", project)
	}
	sel := strings.TrimSpace(values["sharedSubnetwork"])
	region, name, ok := strings.Cut(sel, "/")
	if !ok || name == "" {
		return nil, fmt.Errorf("Shared subnetwork: choose a subnet of host project %s", host)
	}
	if zr := zone[:strings.LastIndex(zone, "-")]; zr != region {
		return nil, fmt.Errorf("Shared subnetwork: %s is in region %s, not the instance's region %s", name, region, zr)
	}
	s, err := p.call("GET", host, "regions/"+region+"/subnetworks/"+name, nil, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"network": "projects/" + host + "/global/networks/" + vpcLast(vpcStr(s, "network")),
		"subnetwork": "projects/" + host + "/regions/" + region + "/subnetworks/" + name}, nil
}
