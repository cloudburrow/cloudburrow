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

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
