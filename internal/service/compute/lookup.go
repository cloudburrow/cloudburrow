package compute

import (
	"encoding/json"
	"net"
	"strings"
)

// What other services check their network fields against (#1936), with
// Compute Engine served: Memorystore's authorized networks, reserved
// ranges and PSC networks, Kubernetes Engine's cluster networks and
// subnets, Cloud SQL's private networks. Each takes a network or subnet
// in any form a client sends it: a full URL, projects/P/..., global/... or
// regions/..., or a bare name in project.

// NetworkName is the relative name (projects/P/global/networks/N) of a
// network reference in project, and false when it names none.
func NetworkName(ref, project string) (string, bool) {
	rel := Rel(ref, project)
	if !strings.Contains(rel, "/") {
		rel = networkRel(project, rel)
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" {
		return "", false
	}
	return rel, true
}

// HasNetwork reports whether the network ref (in project) exists.
func (s *Server) HasNetwork(ref, project string) bool {
	rel, ok := NetworkName(ref, project)
	if !ok {
		return false
	}
	p := strings.Split(rel, "/")[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureProjectLocked(p)
	_, ok = s.res[rel]
	return ok
}

// Subnetwork is the network of the subnetwork ref (projects/P/regions/R/
// subnetworks/S, regions/R/subnetworks/S, or a bare name in region), and
// false when there is none.
func (s *Server) Subnetwork(ref, project, region string) (network string, ok bool) {
	rel := Rel(ref, project)
	if !strings.Contains(rel, "/") {
		rel = subnetRel(project, region, rel)
	}
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != "regions" || p[4] != "subnetworks" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureProjectLocked(p[1])
	o, ok := s.res[rel]
	if !ok {
		return "", false
	}
	return str(o, "network"), true
}

// SubnetInfo is one subnetwork as other services list it: its relative
// name (projects/P/regions/R/subnetworks/S), its network's
// (projects/P/global/networks/N), its primary range and its secondary
// ranges (name, range).
type SubnetInfo struct {
	Name, Network, IPCIDRRange string
	Secondary                  [][2]string
}

// ListSubnetworks lists project's subnetworks, for Kubernetes Engine's
// usable subnetworks (#2100).
func (s *Server) ListSubnetworks(project string) []SubnetInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureProjectLocked(project)
	prefix := "projects/" + project + "/regions/"
	var out []SubnetInfo
	for rel, o := range s.res {
		p := strings.Split(rel, "/")
		if !strings.HasPrefix(rel, prefix) || len(p) != 6 || p[4] != "subnetworks" {
			continue
		}
		si := SubnetInfo{Name: rel, Network: Rel(str(o, "network"), project), IPCIDRRange: str(o, "ipCidrRange")}
		l, _ := o["secondaryIpRanges"].([]any)
		for _, r := range l {
			m, _ := r.(obj)
			si.Secondary = append(si.Secondary, [2]string{str(m, "rangeName"), str(m, "ipCidrRange")})
		}
		out = append(out, si)
	}
	return out
}

// Conflict is the subnet range (or allocated range) of network, a
// relative name, that n overlaps, or "" when it overlaps none.
func (s *Server) Conflict(network string, n *net.IPNet) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := strings.Split(network, "/"); len(p) > 1 {
		_ = s.ensureProjectLocked(p[1])
	}
	for _, r := range s.rangesLocked(network, "") {
		if r.kind != "route" && Overlaps(n, r.n) {
			if r.kind == "allocated" {
				return "allocated range " + lastSeg(r.holder) + " (" + r.n.String() + ")"
			}
			return "subnetwork " + lastSeg(r.holder) + " in region " + r.region + " (" + r.n.String() + ")"
		}
	}
	return ""
}

// AllocatedRange is the CIDR range of the VPC_PEERING global address name
// in network's project, allocated for private services access on network.
func (s *Server) AllocatedRange(network, name string) (*net.IPNet, bool) {
	p := strings.Split(network, "/")
	if len(p) < 2 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.res["projects/"+p[1]+"/global/addresses/"+name]
	if !ok || str(o, "purpose") != "VPC_PEERING" || str(o, "network") != network {
		return nil, false
	}
	b, _ := json.Marshal(o["prefixLength"])
	n, ok := ParseCIDR(str(o, "address") + "/" + string(b))
	return n, ok
}

// AllocatedRanges are every VPC_PEERING global address range allocated on
// network (projects/P/global/networks/N), by name: the ranges private
// services access gives a producer such as Cloud SQL (#2141).
func (s *Server) AllocatedRanges(network string) map[string]*net.IPNet {
	p := strings.Split(network, "/")
	if len(p) < 2 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]*net.IPNet{}
	prefix := "projects/" + p[1] + "/global/addresses/"
	for k, o := range s.res {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || strings.Contains(rest, "/") || str(o, "purpose") != "VPC_PEERING" || str(o, "network") != network {
			continue
		}
		b, _ := json.Marshal(o["prefixLength"])
		if n, ok := ParseCIDR(str(o, "address") + "/" + string(b)); ok {
			out[rest] = n
		}
	}
	return out
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
