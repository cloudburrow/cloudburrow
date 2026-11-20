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
// an internal IP address in one of the network's subnets. Next hops on VM
// instances, VPN tunnels and load balancers are not served (501). A static
// route's destination may not match or fit within a subnet's range.

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
	if err != nil || dn.IP.To4() == nil {
		return nil, invalid("Invalid value for field 'resource.destRange': '%s'. Must be a valid IPv4 CIDR range.", dr)
	}
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
		if ip == nil || ip.To4() == nil {
			return nil, invalid("Invalid value for field 'resource.nextHopIp': '%s'. Must be an IPv4 address.", str(b, "nextHopIp"))
		}
		in := false
		for _, r := range s.rangesLocked(nw, "") {
			if r.kind == "subnetwork" && r.n.Contains(ip) {
				in = true
			}
		}
		if !in {
			// UNVERIFIED wording (docs re-read 2026-11-19).
			return nil, invalid("Invalid value for field 'resource.nextHopIp': '%s'. The next hop IP address must be in a subnetwork of network '%s'.", ip, lastSeg(nw))
		}
		o["nextHopIp"] = ip.String()
	default:
		return nil, unimplemented("a next hop on a VM instance, VPN tunnel or internal load balancer: those resources are not served")
	}
	if ones, _ := dn.Mask.Size(); ones > 0 {
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
