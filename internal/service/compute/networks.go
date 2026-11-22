package compute

import (
	"net/http"
	"sort"
	"strings"
)

// Networks (https://docs.cloud.google.com/compute/docs/reference/rest/v1/networks,
// re-read 2026-11-19). A network is auto mode (autoCreateSubnetworks true:
// one subnet per region, in 10.128.0.0/9, named after the network) or
// custom mode (false). Legacy networks (IPv4Range, or autoCreateSubnetworks
// unset) are not served. A project starts with its default network, an
// auto mode network and its four default firewall rules, as a project with
// the Compute Engine API enabled does; once deleted it is not made again.

const (
	defaultMTU        = 1460
	minMTU, maxMTU    = 1300, 8896
	internetGateway   = "global/gateways/default-internet-gateway"
	defaultNetworkFmt = "projects/%s/global/networks/default"
)

func networkRel(project, name string) string {
	return "projects/" + project + "/global/networks/" + name
}

// ensureProjectLocked makes a project's default network the first time
// the project is seen.
func (s *Server) ensureProjectLocked(project string) error {
	if s.seeded[project] || s.defaultDeleted[project] {
		return nil
	}
	s.seeded[project] = true
	if _, ok := s.res[networkRel(project, "default")]; ok {
		return nil
	}
	if _, err := s.createNetworkLocked(project, obj{"name": "default", "description": "Default network for the project",
		"autoCreateSubnetworks": true}); err != nil {
		return err
	}
	net := networkRel(project, "default")
	rules := []obj{
		{"name": "default-allow-internal", "description": "Allow internal traffic on the default network",
			"priority": 65534.0, "sourceRanges": []any{"10.128.0.0/9"},
			"allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"0-65535"}}, obj{"IPProtocol": "udp", "ports": []any{"0-65535"}}, obj{"IPProtocol": "icmp"}}},
		{"name": "default-allow-ssh", "description": "Allow SSH from anywhere", "priority": 65534.0,
			"sourceRanges": []any{"0.0.0.0/0"}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"22"}}}},
		{"name": "default-allow-rdp", "description": "Allow RDP from anywhere", "priority": 65534.0,
			"sourceRanges": []any{"0.0.0.0/0"}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"3389"}}}},
		{"name": "default-allow-icmp", "description": "Allow ICMP from anywhere", "priority": 65534.0,
			"sourceRanges": []any{"0.0.0.0/0"}, "allowed": []any{obj{"IPProtocol": "icmp"}}},
	}
	for _, r := range rules {
		r["network"] = net
		if _, err := s.createFirewallLocked(project, r); err != nil {
			return err
		}
	}
	return s.saveLocked()
}

func (s *Server) insertNetwork(project string, b obj) (obj, error) {
	o, err := s.createNetworkLocked(project, b)
	if err != nil {
		return nil, err
	}
	return s.done(project, "global", "insert", str(o, "selfLink"), o["id"])
}

func (s *Server) createNetworkLocked(project string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := networkRel(project, name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	if str(b, "IPv4Range") != "" {
		return nil, unimplemented("legacy networks (IPv4Range) are not served; create a VPC network with autoCreateSubnetworks")
	}
	auto, ok := b["autoCreateSubnetworks"].(bool)
	if !ok {
		return nil, unimplemented("legacy networks are not served: set autoCreateSubnetworks to true (auto mode) or false (custom mode)")
	}
	mtu := float64(defaultMTU)
	if v, ok := b["mtu"].(float64); ok && v != 0 {
		if v < minMTU || v > maxMTU {
			return nil, invalid("Invalid value for field 'resource.mtu': '%d'. Must be between %d and %d.", int(v), minMTU, maxMTU)
		}
		mtu = v
	}
	rc, err := routingConfig(b["routingConfig"], nil)
	if err != nil {
		return nil, err
	}
	order := str(b, "networkFirewallPolicyEnforcementOrder")
	switch order {
	case "":
		order = "AFTER_CLASSIC_FIREWALL"
	case "AFTER_CLASSIC_FIREWALL", "BEFORE_CLASSIC_FIREWALL":
	default:
		return nil, invalid("Invalid value for field 'resource.networkFirewallPolicyEnforcementOrder': '%s'.", order)
	}
	o := norm(obj{"kind": "compute#network", "id": newID(), "creationTimestamp": s.stamp(), "name": name,
		"description": str(b, "description"), "selfLink": rel, "autoCreateSubnetworks": auto,
		"subnetworks": []any{}, "routingConfig": rc, "mtu": mtu, "networkFirewallPolicyEnforcementOrder": order})
	if v, _ := b["enableUlaInternalIpv6"].(bool); v {
		// A ULA internal IPv6 /48 of fd20::/20 (ipv6.go).
		r, err := s.ulaRangeLocked(rel, str(b, "internalIpv6Range"))
		if err != nil {
			return nil, err
		}
		o["enableUlaInternalIpv6"], o["internalIpv6Range"] = true, r
	} else if str(b, "internalIpv6Range") != "" {
		return nil, invalid("Invalid value for field 'resource.internalIpv6Range': '%s'. Set enableUlaInternalIpv6 to give the network an internal IPv6 range.", str(b, "internalIpv6Range"))
	}
	if o["description"] == "" {
		delete(o, "description")
	}
	s.res[rel] = o
	// The default route to the internet, which every new network has.
	s.addRouteLocked(project, obj{"name": "default-route-" + randHex(8), "network": rel, "destRange": "0.0.0.0/0",
		"priority": 1000.0, "nextHopGateway": "projects/" + project + "/" + internetGateway,
		"description": "Default route to the Internet."}, "internet")
	if auto {
		for _, region := range Regions() {
			s.addSubnetLocked(project, region, obj{"name": name, "network": rel, "ipCidrRange": autoRanges[region]}, true)
		}
	}
	return o, nil
}

func routingConfig(v any, cur map[string]any) (map[string]any, error) {
	out := map[string]any{"routingMode": "REGIONAL", "bgpBestPathSelectionMode": "LEGACY"}
	for k, x := range cur {
		out[k] = x
	}
	in, _ := v.(map[string]any)
	if m, ok := in["routingMode"].(string); ok && m != "" {
		if m != "REGIONAL" && m != "GLOBAL" {
			return nil, invalid("Invalid value for field 'resource.routingConfig.routingMode': '%s'. Must be one of [GLOBAL, REGIONAL]", m)
		}
		out["routingMode"] = m
	}
	if m, ok := in["bgpBestPathSelectionMode"].(string); ok && m != "" {
		if m != "LEGACY" && m != "STANDARD" {
			return nil, invalid("Invalid value for field 'resource.routingConfig.bgpBestPathSelectionMode': '%s'.", m)
		}
		out["bgpBestPathSelectionMode"] = m
	}
	return out, nil
}

func (s *Server) patchNetwork(project, name string, b obj) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	next := norm(o)
	for k, v := range b {
		switch k {
		case "routingConfig":
			rc, err := routingConfig(v, o["routingConfig"].(map[string]any))
			if err != nil {
				return nil, err
			}
			next[k] = rc
		case "mtu":
			f, _ := v.(float64)
			if f < minMTU || f > maxMTU {
				return nil, invalid("Invalid value for field 'resource.mtu': '%v'. Must be between %d and %d.", v, minMTU, maxMTU)
			}
			next[k] = f
		case "networkFirewallPolicyEnforcementOrder":
			next[k] = v
		case "enableUlaInternalIpv6":
			on, _ := v.(bool)
			if err := s.patchUlaLocked(rel, o, next, on, str(b, "internalIpv6Range")); err != nil {
				return nil, err
			}
		case "name", "selfLink", "id", "kind", "creationTimestamp", "autoCreateSubnetworks", "subnetworks", "description", "selfLinkWithId":
			// Unchanged or output only: a patch of the whole resource
			// carries them; a change is refused.
			if v != nil && !equalJSON(v, o[k]) && !(k == "selfLink" || k == "selfLinkWithId" || k == "subnetworks") {
				return nil, invalid("Invalid value for field 'resource.%s': '%v'. The field cannot be modified with patch.", k, v)
			}
		}
	}
	s.res[rel] = next
	return s.done(project, "global", "patch", rel, o["id"])
}

func (s *Server) switchToCustomMode(project, name string) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if auto, _ := o["autoCreateSubnetworks"].(bool); !auto {
		return nil, invalid("Invalid resource usage: 'Network %s is already a custom mode network.'.", name)
	}
	o["autoCreateSubnetworks"] = false
	// Its subnets stay, as custom subnets.
	for _, sl := range strs(o, "subnetworks") {
		if sub, ok := s.res[sl]; ok {
			delete(sub, "_auto")
		}
	}
	return s.done(project, "global", "switchToCustomMode", rel, o["id"])
}

func (s *Server) deleteNetwork(project, name string) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if users := s.networkUsersLocked(rel); len(users) > 0 {
		return nil, inUse("network", rel, users[0])
	}
	for _, sl := range strs(o, "subnetworks") {
		s.removeSubnetLocked(sl)
	}
	for k, r := range s.res {
		if strings.Contains(k, "/global/routes/") && str(r, "network") == rel {
			delete(s.res, k)
		}
	}
	delete(s.res, rel)
	// Its peers' peerings to it go INACTIVE (#2140).
	s.resyncPeersLocked(rel)
	if name == "default" {
		s.defaultDeleted[project] = true
	}
	return s.done(project, "global", "delete", rel, o["id"])
}

// networkUsersLocked names what keeps a network from being deleted: a
// subnet it did not make itself, a firewall rule, a static route, an
// address, or a resource of another service.
func (s *Server) networkUsersLocked(rel string) []string {
	var users []string
	for k, r := range s.res {
		switch {
		case strings.Contains(k, "/subnetworks/"):
			if str(r, "network") == rel && r["_auto"] == nil {
				users = append(users, k)
			}
		case strings.Contains(k, "/global/firewalls/"), strings.Contains(k, "/addresses/"):
			if str(r, "network") == rel {
				users = append(users, k)
			}
		case strings.Contains(k, "/routers/"):
			// A Cloud Router (#2140) keeps its network.
			if str(r, "network") == rel {
				users = append(users, k)
			}
		case strings.Contains(k, "/global/routes/"):
			if str(r, "network") == rel && r["_system"] == nil {
				users = append(users, k)
			}
		}
	}
	// Private Service Connect endpoints (#2140).
	users = append(users, s.pscUsersLocked(rel)...)
	sort.Strings(users)
	if s.opts.NetworkUsers != nil {
		users = append(users, s.opts.NetworkUsers(rel)...)
	}
	return users
}

func equalJSON(a, b any) bool {
	return jsonString(a) == jsonString(b)
}
