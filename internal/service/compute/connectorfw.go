package compute

import "strings"

// Serverless VPC Access's firewall rules (#2140). A connector in a
// project's own network gets these, named
// aet-CONNECTOR_REGION-CONNECTOR_NAME-SUFFIX, for as long as it exists
// (vpc/docs/serverless-vpc-access, "Firewall rules", read 2026-11-22):
// hcfw, rsgfw, earfw and egrfw for the connector's own instances (tag
// vpc-connector-REGION-NAME), and sbntfw and tagfw letting the connector's
// traffic, by its range and by that tag, into the network's VMs. They are
// real firewall rules of the network, listed and enforced as others are.

// ConnectorTag is the network tag of a connector's instances.
func ConnectorTag(region, name string) string { return "vpc-connector-" + region + "-" + name }

// connectorFirewalls are a connector's rules.
func connectorFirewalls(network, region, name, cidr string) []obj {
	tag := ConnectorTag(region, name)
	pre := "aet-" + region + "-" + name + "-"
	all := []any{obj{"IPProtocol": "tcp"}, obj{"IPProtocol": "udp"}, obj{"IPProtocol": "icmp"}}
	svc := []any{obj{"IPProtocol": "tcp", "ports": []any{"667"}}, obj{"IPProtocol": "udp", "ports": []any{"665-666"}}, obj{"IPProtocol": "icmp"}}
	rule := func(suffix string, b obj) obj {
		b["name"], b["network"] = pre+suffix, network
		if _, ok := b["priority"]; !ok {
			b["priority"] = float64(1000)
		}
		return b
	}
	out := []obj{
		rule("hcfw", obj{"direction": "INGRESS", "priority": float64(100), "sourceRanges": []any{"35.191.0.0/16", "130.211.0.0/22"},
			"targetTags": []any{tag}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"667"}}}}),
		rule("rsgfw", obj{"direction": "INGRESS", "priority": float64(100), "sourceRanges": []any{"35.199.224.0/19"},
			"targetTags": []any{tag}, "allowed": svc}),
		rule("earfw", obj{"direction": "EGRESS", "priority": float64(100), "destinationRanges": []any{"35.199.224.0/19"},
			"targetTags": []any{tag}, "allowed": svc}),
		rule("egrfw", obj{"direction": "EGRESS", "priority": float64(100), "destinationRanges": []any{"35.199.224.0/19"},
			"targetTags": []any{tag}, "denied": []any{obj{"IPProtocol": "tcp"}, obj{"IPProtocol": "udp"}}}),
		rule("tagfw", obj{"direction": "INGRESS", "sourceTags": []any{tag}, "allowed": all}),
	}
	if cidr != "" {
		out = append(out, rule("sbntfw", obj{"direction": "INGRESS", "sourceRanges": []any{cidr}, "allowed": all}))
	}
	return out
}

// SetConnectorFirewalls makes (on) or removes a connector's firewall
// rules in its network: network is its relative name; cidr the
// connector's range, or its subnet's for a connector made on a subnet.
func (s *Server) SetConnectorFirewalls(network, region, name, cidr string, on bool) error {
	parts := strings.Split(network, "/")
	if len(parts) < 2 {
		return nil
	}
	project := parts[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[network]; !ok && on {
		return nil
	}
	for _, b := range connectorFirewalls(network, region, name, cidr) {
		rel := firewallRel(project, str(b, "name"))
		delete(s.res, rel)
		if on {
			if _, err := s.createFirewallLocked(project, b); err != nil {
				return err
			}
		}
	}
	return s.saveLocked()
}
