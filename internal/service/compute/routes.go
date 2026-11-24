package compute

import (
	"net"
	"net/http"
	"strings"
)

// Routes (https://docs.cloud.google.com/vpc/docs/routes and
// https://docs.cloud.google.com/compute/docs/reference/rest/v1/routes,
// re-read 2026-11-19). A network has a default route to the internet,
// which can be deleted, and one subnet route per subnet range, which
// cannot; a static route has one next hop: the default internet gateway or
// an internal IP address in one of the network's subnets, or an instance
// of the network (#2140); IPv6 routes take an IPv6 next hop or a
// dual-stack instance (#2140). Next hops on VPN tunnels and load
// balancers are not served (501). A static route's destination may not
// match or fit within a subnet's range.

func routeRel(project, name string) string { return "projects/" + project + "/global/routes/" + name }

// addRouteLocked records a system route: "internet" for the default
// route, or the subnet whose route it is.
func (s *Server) addRouteLocked(project string, in obj, system string) {
	rel := routeRel(project, str(in, "name"))
	o := obj{"kind": "compute#route", "id": newID(), "creationTimestamp": s.stamp(), "selfLink": rel, "_system": system,
		"routeType": "STATIC"}
	if system != "internet" {
		o["routeType"] = "SUBNET"
	}
	for k, v := range in {
		o[k] = v
	}
	s.res[rel] = norm(o)
}

func (s *Server) insertRoute(project string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := routeRel(project, name)
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	nw := str(b, "network")
	if nw == "" {
		return nil, required("resource.network")
	}
	nw = Rel(nw, project)
	if !strings.Contains(nw, "/") {
		nw = networkRel(project, nw)
	}
	if _, ok := s.res[nw]; !ok {
		return nil, notFound(nw)
	}
	dr := str(b, "destRange")
	if dr == "" {
		return nil, required("resource.destRange")
	}
	_, dn, err := net.ParseCIDR(dr)
	if err != nil {
		return nil, invalid("Invalid value for field 'resource.destRange': '%s'. Must be a valid IPv4 or IPv6 CIDR range.", dr)
	}
	// An IPv6 static route (#2140, vpc/docs/routes#ipv6-routes, read
	// 2026-11-22): its next hop the default internet gateway, an IPv6
	// address of a dual-stack subnet of the network, or a dual-stack
	// instance of it; its destination outside the subnets' IPv6 ranges.
	v6 := dn.IP.To4() == nil
	prio := 1000.0
	if v, ok := b["priority"].(float64); ok {
		if v < 0 || v > 65535 {
			return nil, invalid("Invalid value for field 'resource.priority': '%v'. Must be greater than or equal to 0 and less than or equal to 65535", v)
		}
		prio = v
	}
	hops := 0
	for _, k := range []string{"nextHopGateway", "nextHopIp", "nextHopInstance", "nextHopVpnTunnel", "nextHopIlb"} {
		if str(b, k) != "" {
			hops++
		}
	}
	if hops != 1 {
		return nil, invalid("Invalid value for field 'resource': ''. A route must have exactly one next hop: nextHopGateway, nextHopIp, nextHopInstance, nextHopIlb or nextHopVpnTunnel.")
	}
	o := obj{"name": name, "network": nw, "destRange": dn.String(), "priority": prio}
	switch {
	case str(b, "nextHopGateway") != "":
		g := Rel(str(b, "nextHopGateway"), project)
		if g != "projects/"+project+"/"+internetGateway {
			return nil, invalid("Invalid value for field 'resource.nextHopGateway': '%s'. The only gateway is global/gateways/default-internet-gateway.", str(b, "nextHopGateway"))
		}
		o["nextHopGateway"] = g
	case str(b, "nextHopIp") != "":
		ip := net.ParseIP(str(b, "nextHopIp"))
		if ip == nil || (ip.To4() == nil) != v6 {
			fam := map[bool]string{true: "IPv6", false: "IPv4"}[v6]
			return nil, invalid("Invalid value for field 'resource.nextHopIp': '%s'. Must be an %s address, as the destination range is.", str(b, "nextHopIp"), fam)
		}
		in := false
		for _, r := range s.rangesLocked(nw, "") {
			if r.kind == "subnetwork" && r.n.Contains(ip) {
				in = true
			}
		}
		for _, n6 := range s.subnetV6RangesLocked(nw) {
			in = in || v6 && n6.n.Contains(ip)
		}
		if !in {
			// UNVERIFIED wording (docs re-read 2026-11-19).
			return nil, invalid("Invalid value for field 'resource.nextHopIp': '%s'. The next hop IP address must be in a subnetwork of network '%s'.", ip, lastSeg(nw))
		}
		o["nextHopIp"] = ip.String()
	case str(b, "nextHopInstance") != "":
		// An instance of the route's network (#2140), as Google requires
		// (https://docs.cloud.google.com/vpc/docs/routes#next-hops, read
		// 2026-11-22: "an instance in the same VPC network as the route").
		ir := Rel(str(b, "nextHopInstance"), project)
		io, ok := s.res[ir]
		if !ok || !strings.Contains(ir, "/instances/") {
			return nil, invalid("Invalid value for field 'resource.nextHopInstance': '%s'. The instance does not exist.", str(b, "nextHopInstance"))
		}
		v, ok := vmNetOf(ir, io)
		if !ok || v.network != nw {
			// UNVERIFIED wording (re-read 2026-11-22:
			// https://docs.cloud.google.com/vpc/docs/routes#next_hop_instance).
			return nil, invalid("Invalid value for field 'resource.nextHopInstance': '%s'. The instance must be in network '%s'.", str(b, "nextHopInstance"), lastSeg(nw))
		}
		if v6 && v.ip6 == "" {
			return nil, invalid("Invalid value for field 'resource.nextHopInstance': '%s'. The next hop of an IPv6 route must be a dual-stack instance.", str(b, "nextHopInstance"))
		}
		o["nextHopInstance"] = ir
		o["nextHopNetwork"] = nw
	default:
		return nil, unimplemented("a next hop on a VPN tunnel or internal load balancer: those resources are not served")
	}
	if ones, _ := dn.Mask.Size(); ones > 0 && v6 {
		for _, r := range s.subnetV6RangesLocked(nw) {
			if contains(r.n, dn) {
				return nil, invalid("Invalid value for field 'resource.destRange': '%s'. The destination range matches or fits within the range %s of subnetwork '%s'.", dn, r.n, r.name)
			}
		}
	} else if ones > 0 {
		for _, r := range s.rangesLocked(nw, "") {
			if r.kind == "subnetwork" && contains(r.n, dn) {
				// UNVERIFIED wording (docs re-read 2026-11-19).
				return nil, invalid("Invalid value for field 'resource.destRange': '%s'. The destination range matches or fits within the range %s of subnetwork '%s'.", dn, r.n, lastSeg(r.holder))
			}
		}
	}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if tags := strsAny(b["tags"]); len(tags) > 0 {
		o["tags"] = tags
	}
	o["kind"], o["id"], o["creationTimestamp"], o["selfLink"], o["routeType"] = "compute#route", newID(), s.stamp(), rel, "STATIC"
	s.res[rel] = norm(o)
	return s.done(project, "global", "insert", rel, s.res[rel]["id"])
}

func (s *Server) deleteRoute(project, name string) (obj, error) {
	rel := routeRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if sys := str(o, "_system"); sys != "" && sys != "internet" {
		// UNVERIFIED wording (docs re-read 2026-11-19): a subnet route goes with its subnet.
		return nil, invalid("Invalid resource usage: 'Cannot delete the subnet route %s: it is deleted with its subnetwork %s.'.", rel, sys)
	}
	delete(s.res, rel)
	return s.done(project, "global", "delete", rel, o["id"])
}

// subnetV6Range is a dual-stack subnet's IPv6 range.
type subnetV6Range struct {
	n    *net.IPNet
	name string
}

// subnetV6RangesLocked are the IPv6 ranges of network nw's subnets.
func (s *Server) subnetV6RangesLocked(nw string) []subnetV6Range {
	var out []subnetV6Range
	for k, o := range s.res {
		if !strings.Contains(k, "/subnetworks/") || str(o, "network") != nw {
			continue
		}
		if _, n, err := net.ParseCIDR(str(o, "ipv6CidrRange")); err == nil {
			out = append(out, subnetV6Range{n, str(o, "name")})
		}
	}
	return out
}
