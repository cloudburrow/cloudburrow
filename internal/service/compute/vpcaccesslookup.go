package compute

import "strings"

// SubnetRange is the network and primary IPv4 range of the subnetwork ref
// (any form Subnetwork takes), for Serverless VPC Access (#2140), whose
// connector needs a /28 subnet of its own; false when there is none.
func (s *Server) SubnetRange(ref, project, region string) (network, cidr string, ok bool) {
	rel := Rel(ref, project)
	if !strings.Contains(rel, "/") {
		rel = subnetRel(project, region, rel)
	}
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != "regions" || p[4] != "subnetworks" {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureProjectLocked(p[1])
	o, ok := s.res[rel]
	if !ok {
		return "", "", false
	}
	return str(o, "network"), str(o, "ipCidrRange"), true
}

// SubnetUsers are the VM instances with a network interface in the
// subnetwork rel (projects/P/regions/R/subnetworks/S): a connector's
// subnet is its own, which no VM may use (vpc/docs/configure-serverless-
// vpc-access, read 2026-11-22).
func (s *Server) SubnetUsers(rel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		for _, e := range sliceOf(o["networkInterfaces"]) {
			if nic, _ := e.(map[string]any); str(nic, "subnetwork") == rel {
				out = append(out, k)
			}
		}
	}
	return out
}
