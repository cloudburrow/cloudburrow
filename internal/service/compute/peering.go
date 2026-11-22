package compute

import (
	"net/http"
	"strings"
	"time"
)

// VPC Network Peering (#2140): networks.addPeering, removePeering and
// updatePeering (https://docs.cloud.google.com/compute/docs/reference/rest/v1/networks/addPeering,
// read 2026-11-22). A peering is an entry of the network's peerings, named
// and pointing at the peer network, in this project or another this
// server keeps. It is INACTIVE until the peer network has a peering back
// to this one, when both become ACTIVE, as Google's do
// (https://docs.cloud.google.com/vpc/docs/using-vpc-peering: "the peering
// becomes active when you create a matching configuration in the peer
// network"); removing either side makes the other INACTIVE again. While
// ACTIVE, the two networks' VMs share a VXLAN segment and reach each other
// over the subnet routes Google always exchanges (vpc.go); custom routes
// are records, and listPeeringRoutes stays 501.

func (s *Server) peeringsLocked(o obj) []any {
	l, _ := o["peerings"].([]any)
	return l
}

// peeringStamp is stateDetails' time prefix.
func (s *Server) peeringStamp() string {
	return "[" + s.opts.Now().UTC().Format(time.TimeOnly) + "]"
}

// syncPeeringLocked sets the state of the peering p of network rel from
// whether its peer has one back.
func (s *Server) syncPeeringLocked(rel string, p map[string]any) {
	peer, ok := s.res[Rel(str(p, "network"), "")]
	back := false
	if ok {
		for _, e := range s.peeringsLocked(peer) {
			if m, _ := e.(map[string]any); Rel(str(m, "network"), "") == rel {
				back = true
			}
		}
	}
	if back {
		p["state"], p["stateDetails"] = "ACTIVE", s.peeringStamp()+": Connected."
		if mtu, ok := peer["mtu"]; ok {
			p["peerMtu"] = mtu
		}
		return
	}
	p["state"], p["stateDetails"] = "INACTIVE", s.peeringStamp()+": Waiting for peer network to connect."
	delete(p, "peerMtu")
}

// resyncPeersLocked brings every peering pointing at rel up to date.
func (s *Server) resyncPeersLocked(rel string) {
	for k, o := range s.res {
		if !strings.Contains(k, "/global/networks/") {
			continue
		}
		for _, e := range s.peeringsLocked(o) {
			if m, _ := e.(map[string]any); Rel(str(m, "network"), "") == rel {
				s.syncPeeringLocked(k, m)
			}
		}
	}
}

func (s *Server) addPeering(project, name string, b obj) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	np, _ := b["networkPeering"].(map[string]any)
	if np == nil {
		// The request's own fields, which the API still takes.
		np = map[string]any{"name": b["name"], "network": b["peerNetwork"]}
		if v, ok := b["exchangeSubnetRoutes"]; ok {
			np["exchangeSubnetRoutes"] = v
		}
	}
	pname := str(np, "name")
	if err := checkName("networkPeering.name", pname); err != nil {
		return nil, err
	}
	peerLink := str(np, "network")
	if peerLink == "" {
		return nil, required("networkPeering.network")
	}
	peerRel := Rel(peerLink, project)
	if !strings.HasPrefix(peerRel, "projects/") || !strings.Contains(peerRel, "/global/networks/") {
		peerRel = networkRel(project, lastSeg(peerLink))
	}
	if peerRel == rel {
		// UNVERIFIED wording (docs read 2026-11-22 give none).
		return nil, invalid("Invalid value for field 'networkPeering.network': '%s'. A network cannot be peered with itself.", peerLink)
	}
	if _, ok := s.res[peerRel]; !ok {
		return nil, notFound(peerRel)
	}
	for _, e := range s.peeringsLocked(o) {
		m, _ := e.(map[string]any)
		if str(m, "name") == pname {
			return nil, errf(http.StatusBadRequest, "alreadyExists", "There is already a peering %s in network %s.", pname, rel)
		}
		if Rel(str(m, "network"), "") == peerRel {
			return nil, errf(http.StatusBadRequest, "alreadyExists", "There is already a peering %s to network %s in network %s.", str(m, "name"), peerRel, rel)
		}
	}
	// Peered networks' subnet ranges may not overlap
	// (https://docs.cloud.google.com/vpc/docs/vpc-peering: "a subnet IP
	// range in one peered network cannot overlap with a subnet range in the
	// other"). UNVERIFIED wording.
	for _, a := range s.rangesLocked(rel, "") {
		for _, c := range s.rangesLocked(peerRel, "") {
			if a.kind == "subnetwork" && c.kind == "subnetwork" && Overlaps(a.n, c.n) {
				return nil, invalid("An IP range in the peer network (%s) overlaps with an IP range in the local network (%s).", c.n, a.n)
			}
		}
	}
	p := map[string]any{"name": pname, "network": peerRel, "autoCreateRoutes": true, "exchangeSubnetRoutes": true,
		"exportCustomRoutes": np["exportCustomRoutes"] == true, "importCustomRoutes": np["importCustomRoutes"] == true,
		"exportSubnetRoutesWithPublicIp": np["exportSubnetRoutesWithPublicIp"] != false,
		"importSubnetRoutesWithPublicIp": np["importSubnetRoutesWithPublicIp"] == true,
		"stackType":                      "IPV4_ONLY", "updateStrategy": "INDEPENDENT"}
	if st := str(np, "stackType"); st != "" && st != "IPV4_ONLY" {
		return nil, unimplemented("networkPeering.stackType %s: IPv6 is not served", st)
	}
	if us := str(np, "updateStrategy"); us != "" {
		if us != "INDEPENDENT" && us != "CONSENSUS" {
			return nil, invalid("Invalid value for field 'networkPeering.updateStrategy': '%s'.", us)
		}
		p["updateStrategy"] = us
	}
	o["peerings"] = append(s.peeringsLocked(o), p)
	s.syncPeeringLocked(rel, p)
	s.resyncPeersLocked(rel)
	return s.done(project, "global", "addPeering", rel, o["id"])
}

func (s *Server) removePeering(project, name string, b obj) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	pname := str(b, "name")
	var keep []any
	found := false
	for _, e := range s.peeringsLocked(o) {
		if m, _ := e.(map[string]any); str(m, "name") == pname {
			found = true
			continue
		}
		keep = append(keep, e)
	}
	if !found {
		return nil, invalid("Invalid value for field 'name': '%s'. There is no peering %s in network %s.", pname, pname, rel)
	}
	if len(keep) == 0 {
		delete(o, "peerings")
	} else {
		o["peerings"] = keep
	}
	s.resyncPeersLocked(rel)
	return s.done(project, "global", "removePeering", rel, o["id"])
}

func (s *Server) updatePeering(project, name string, b obj) (obj, error) {
	rel := networkRel(project, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	np, _ := b["networkPeering"].(map[string]any)
	pname := str(np, "name")
	for _, e := range s.peeringsLocked(o) {
		m, _ := e.(map[string]any)
		if str(m, "name") != pname {
			continue
		}
		for _, k := range []string{"exportCustomRoutes", "importCustomRoutes", "exportSubnetRoutesWithPublicIp", "importSubnetRoutesWithPublicIp"} {
			if v, ok := np[k].(bool); ok {
				m[k] = v
			}
		}
		if us := str(np, "updateStrategy"); us != "" {
			if us != "INDEPENDENT" && us != "CONSENSUS" {
				return nil, invalid("Invalid value for field 'networkPeering.updateStrategy': '%s'.", us)
			}
			m["updateStrategy"] = us
		}
		return s.done(project, "global", "updatePeering", rel, o["id"])
	}
	return nil, invalid("Invalid value for field 'networkPeering.name': '%s'. There is no peering %s in network %s.", pname, pname, rel)
}
