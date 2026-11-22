package compute

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Subnetworks (https://docs.cloud.google.com/vpc/docs/subnets and
// https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks,
// re-read 2026-11-19): every primary and secondary range in a network is a
// valid range (checkSubnetRange) overlapping no other subnet's primary or
// secondary range, no allocated range (a VPC_PEERING global address) and
// no static route's destination; a primary range can be expanded, never
// shrunk or replaced; an auto mode network's own subnets stay in
// 10.128.0.0/9, expand to /16 at most, and cannot be deleted. Error texts
// marked UNVERIFIED follow Google's form, not a captured response.

const maxSecondaryRanges = 170

func subnetRel(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/subnetworks/" + name
}

// netRange is one range in a network, and what holds it.
type netRange struct {
	n      *net.IPNet
	holder string // a subnetwork's or an address's relative name
	region string
	kind   string // subnetwork, allocated or route
}

// rangesLocked are the ranges held in a network, but skip's.
func (s *Server) rangesLocked(network, skip string) []netRange {
	var out []netRange
	for k, o := range s.res {
		if k == skip || str(o, "network") != network {
			continue
		}
		switch {
		case strings.Contains(k, "/subnetworks/"):
			region := lastSeg(str(o, "region"))
			if n, ok := ParseCIDR(str(o, "ipCidrRange")); ok {
				out = append(out, netRange{n, k, region, "subnetwork"})
			}
			for _, sr := range secondaries(o) {
				if n, ok := ParseCIDR(str(sr, "ipCidrRange")); ok {
					out = append(out, netRange{n, k, region, "subnetwork"})
				}
			}
		case strings.Contains(k, "/global/addresses/") && str(o, "purpose") == "VPC_PEERING":
			if n, ok := ParseCIDR(fmt.Sprintf("%s/%v", str(o, "address"), o["prefixLength"])); ok {
				out = append(out, netRange{n, k, "", "allocated"})
			}
		case strings.Contains(k, "/global/routes/") && o["_system"] == nil:
			if n, ok := ParseCIDR(str(o, "destRange")); ok {
				if ones, _ := n.Mask.Size(); ones > 0 {
					out = append(out, netRange{n, k, "", "route"})
				}
			}
		}
	}
	return out
}

func secondaries(o obj) []map[string]any {
	l, _ := o["secondaryIpRanges"].([]any)
	out := make([]map[string]any, 0, len(l))
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// conflict is the refusal of n, which overlaps r.
func conflict(n *net.IPNet, r netRange) error {
	switch r.kind {
	case "allocated":
		// UNVERIFIED wording (docs re-read 2026-11-19).
		return invalid("Invalid IPCidrRange: %s conflicts with the allocated IP range '%s' (%s).", n, lastSeg(r.holder), r.n)
	case "route":
		// UNVERIFIED wording (docs re-read 2026-11-19).
		return invalid("Invalid IPCidrRange: %s conflicts with the destination range %s of route '%s'.", n, r.n, lastSeg(r.holder))
	}
	return invalid("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.", n, lastSeg(r.holder), r.region)
}

func checkFree(n *net.IPNet, ranges []netRange) error {
	for _, r := range ranges {
		// A static route conflicts only when it matches or fits within
		// the range; a broader one is overridden by the subnet route.
		if r.kind == "route" && !contains(n, r.n) {
			continue
		}
		if Overlaps(n, r.n) {
			return conflict(n, r)
		}
	}
	return nil
}

func (s *Server) insertSubnetwork(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := subnetRel(project, region, name)
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
	netObj, ok := s.res[nw]
	if !ok {
		return nil, notFound(nw)
	}
	v6 := obj{"selfLink": rel}
	if err := s.dualStackLocked(netObj, b, v6, "resource"); err != nil {
		return nil, err
	}
	if str(b, "ipCidrRange") == "" {
		return nil, required("resource.ipCidrRange")
	}
	n, err := checkSubnetRange("resource.ipCidrRange", str(b, "ipCidrRange"))
	if err != nil {
		return nil, err
	}
	if auto, _ := netObj["autoCreateSubnetworks"].(bool); auto && Overlaps(n, autoBlock) {
		// UNVERIFIED wording (docs re-read 2026-11-19): 10.128.0.0/9 is the auto mode subnets'.
		return nil, invalid("Invalid IPCidrRange: %s conflicts with 10.128.0.0/9, the range reserved for the auto mode subnetworks of network '%s'.", n, lastSeg(nw))
	}
	ranges := s.rangesLocked(nw, "")
	if err := checkFree(n, ranges); err != nil {
		return nil, err
	}
	sec, err := checkSecondaries(b["secondaryIpRanges"], n, ranges, "resource")
	if err != nil {
		return nil, err
	}
	in := obj{"name": name, "network": nw, "ipCidrRange": n.String(), "description": str(b, "description"),
		"privateIpGoogleAccess": b["privateIpGoogleAccess"] == true, "secondaryIpRanges": sec}
	for _, k := range []string{"purpose", "role", "logConfig"} {
		if v, ok := b[k]; ok {
			in[k] = v
		}
	}
	o := s.addSubnetLocked(project, region, in, false)
	for k, v := range v6 {
		if k != "selfLink" {
			o[k] = v
		}
	}
	return s.done(project, "regions/"+region, "insert", rel, o["id"])
}

// checkSecondaries checks a subnet's secondary ranges against its primary
// range, each other and the network's ranges.
func checkSecondaries(v any, primary *net.IPNet, ranges []netRange, field string) ([]any, error) {
	l, _ := v.([]any)
	if len(l) > maxSecondaryRanges {
		return nil, invalid("Invalid value for field '%s.secondaryIpRanges': a subnetwork has at most %d secondary ranges.", field, maxSecondaryRanges)
	}
	out := make([]any, 0, len(l))
	seen := map[string]bool{}
	var mine []*net.IPNet
	for i, e := range l {
		m, _ := e.(map[string]any)
		f := fmt.Sprintf("%s.secondaryIpRanges[%d]", field, i)
		rn, _ := m["rangeName"].(string)
		if err := checkName(f+".rangeName", rn); err != nil {
			return nil, err
		}
		if seen[rn] {
			return nil, invalid("Invalid value for field '%s.rangeName': '%s'. The secondary range name must be unique within the subnetwork.", f, rn)
		}
		seen[rn] = true
		cidr, _ := m["ipCidrRange"].(string)
		if cidr == "" {
			return nil, required(f + ".ipCidrRange")
		}
		n, err := checkSubnetRange(f+".ipCidrRange", cidr)
		if err != nil {
			return nil, err
		}
		if primary != nil && Overlaps(n, primary) {
			return nil, invalid("Invalid IPCidrRange: %s conflicts with the primary IP range %s of the subnetwork.", n, primary)
		}
		for _, o := range mine {
			if Overlaps(n, o) {
				return nil, invalid("Invalid IPCidrRange: %s conflicts with the secondary IP range %s of the subnetwork.", n, o)
			}
		}
		if err := checkFree(n, ranges); err != nil {
			return nil, err
		}
		mine = append(mine, n)
		out = append(out, map[string]any{"rangeName": rn, "ipCidrRange": n.String()})
	}
	return out, nil
}

// addSubnetLocked records a checked subnet, its subnet route, and its
// place in its network's subnetworks.
func (s *Server) addSubnetLocked(project, region string, in obj, auto bool) obj {
	rel := subnetRel(project, region, str(in, "name"))
	n, _ := ParseCIDR(str(in, "ipCidrRange"))
	o := obj{"kind": "compute#subnetwork", "id": newID(), "creationTimestamp": s.stamp(), "name": in["name"],
		"network": in["network"], "ipCidrRange": n.String(), "gatewayAddress": gateway(n),
		"region": "projects/" + project + "/regions/" + region, "selfLink": rel,
		"privateIpGoogleAccess": in["privateIpGoogleAccess"] == true, "fingerprint": newFingerprint(),
		"purpose": "PRIVATE", "stackType": "IPV4_ONLY", "privateIpv6GoogleAccess": "DISABLE_GOOGLE_ACCESS"}
	if d := str(in, "description"); d != "" {
		o["description"] = d
	}
	if l, _ := in["secondaryIpRanges"].([]any); len(l) > 0 {
		o["secondaryIpRanges"] = l
	}
	for _, k := range []string{"purpose", "role", "logConfig"} {
		if v, ok := in[k]; ok && v != nil && v != "" {
			o[k] = v
		}
	}
	if auto {
		o["_auto"] = true
	}
	o = norm(o)
	s.res[rel] = o
	if nw, ok := s.res[str(in, "network")]; ok {
		nw["subnetworks"] = append(nw["subnetworks"].([]any), rel)
	}
	s.addRouteLocked(project, obj{"name": "default-route-" + randHex(8), "network": in["network"], "destRange": n.String(),
		"priority": 0.0, "nextHopNetwork": in["network"],
		"description": "Default local route to the subnetwork " + n.String() + "."}, rel)
	return o
}

// removeSubnetLocked forgets a subnet, its route, and its place in its
// network.
func (s *Server) removeSubnetLocked(rel string) {
	o, ok := s.res[rel]
	if !ok {
		return
	}
	delete(s.res, rel)
	for k, r := range s.res {
		if strings.Contains(k, "/global/routes/") && str(r, "_system") == rel {
			delete(s.res, k)
		}
	}
	if nw, ok := s.res[str(o, "network")]; ok {
		var keep []any
		for _, sl := range strs(nw, "subnetworks") {
			if sl != rel {
				keep = append(keep, sl)
			}
		}
		if keep == nil {
			keep = []any{}
		}
		nw["subnetworks"] = keep
	}
}

func (s *Server) subnetLocked(project, region, name string) (obj, string, error) {
	rel := subnetRel(project, region, name)
	o, ok := s.res[rel]
	if !ok {
		return nil, rel, notFound(rel)
	}
	return o, rel, nil
}

func (s *Server) deleteSubnetwork(project, region, name string) (obj, error) {
	o, rel, err := s.subnetLocked(project, region, name)
	if err != nil {
		return nil, err
	}
	if o["_auto"] != nil {
		// UNVERIFIED wording (docs re-read 2026-11-19).
		return nil, invalid("Invalid resource usage: 'Cannot delete auto subnetwork from an auto subnet mode network.'.")
	}
	for k, a := range s.res {
		if strings.Contains(k, "/addresses/") && str(a, "subnetwork") == rel {
			return nil, inUse("subnetwork", rel, k)
		}
	}
	if s.opts.NetworkUsers != nil {
		if users := s.opts.NetworkUsers(rel); len(users) > 0 {
			return nil, inUse("subnetwork", rel, users[0])
		}
	}
	s.removeSubnetLocked(rel)
	return s.done(project, "regions/"+region, "delete", rel, o["id"])
}

func (s *Server) checkFingerprint(o obj, b obj) error {
	fp, _ := b["fingerprint"].(string)
	if fp == "" {
		return required("resource.fingerprint")
	}
	if fp != str(o, "fingerprint") {
		return errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Supplied fingerprint does not match current metadata fingerprint.")
	}
	return nil
}

// patchSubnetwork changes a subnet's description, secondary ranges,
// Private Google Access, log configuration or role, under its fingerprint.
func (s *Server) patchSubnetwork(project, region, name string, b obj) (obj, error) {
	o, rel, err := s.subnetLocked(project, region, name)
	if err != nil {
		return nil, err
	}
	if err := s.checkFingerprint(o, b); err != nil {
		return nil, err
	}
	next := norm(o)
	for k, v := range b {
		switch k {
		case "description", "logConfig", "role", "privateIpGoogleAccess":
			if v == nil {
				delete(next, k)
			} else {
				next[k] = v
			}
		case "secondaryIpRanges":
			primary, _ := ParseCIDR(str(o, "ipCidrRange"))
			sec, err := checkSecondaries(v, primary, s.rangesLocked(str(o, "network"), rel), "resource")
			if err != nil {
				return nil, err
			}
			if len(sec) == 0 {
				delete(next, k)
			} else {
				next[k] = sec
			}
		case "ipCidrRange":
			if v != nil && v != o[k] {
				return nil, invalid("Invalid value for field 'resource.ipCidrRange': '%v'. Use expandIpCidrRange to change the primary IP range.", v)
			}
		case "stackType", "ipv6AccessType":
			if err := s.patchStackLocked(rel, o, next, b); err != nil {
				return nil, err
			}
		case "network", "region", "name":
			if v != nil && Rel(fmt.Sprint(v), project) != o[k] && v != o[k] {
				return nil, invalid("Invalid value for field 'resource.%s': '%v'. The field cannot be modified.", k, v)
			}
		}
	}
	next["fingerprint"] = newFingerprint()
	s.res[rel] = next
	return s.done(project, "regions/"+region, "patch", rel, o["id"])
}

func (s *Server) expandSubnetwork(project, region, name string, b obj) (obj, error) {
	o, rel, err := s.subnetLocked(project, region, name)
	if err != nil {
		return nil, err
	}
	cur, _ := ParseCIDR(str(o, "ipCidrRange"))
	want := str(b, "ipCidrRange")
	if want == "" {
		return nil, required("ipCidrRange")
	}
	n, err := checkSubnetRange("ipCidrRange", want)
	if err != nil {
		return nil, err
	}
	co, _ := cur.Mask.Size()
	no, _ := n.Mask.Size()
	if no >= co || !n.Contains(cur.IP) {
		// UNVERIFIED wording (docs re-read 2026-11-19); the rule is documented: expand, never
		// shrink or replace.
		return nil, invalid("Invalid IPCidrRange: %s. The new range must be larger than the subnetwork's current range %s and contain it.", n, cur)
	}
	if o["_auto"] != nil && no < 16 {
		return nil, invalid("Invalid IPCidrRange: %s. A subnetwork of an auto mode network can be expanded to /16 at most.", n)
	}
	ranges := s.rangesLocked(str(o, "network"), rel)
	if err := checkFree(n, ranges); err != nil {
		return nil, err
	}
	for _, sr := range secondaries(o) {
		if sn, ok := ParseCIDR(str(sr, "ipCidrRange")); ok && Overlaps(n, sn) {
			return nil, invalid("Invalid IPCidrRange: %s conflicts with the secondary IP range %s of the subnetwork.", n, sn)
		}
	}
	o["ipCidrRange"] = n.String()
	o["fingerprint"] = newFingerprint()
	for _, r := range s.res {
		if str(r, "_system") == rel {
			r["destRange"] = n.String()
			r["description"] = "Default local route to the subnetwork " + n.String() + "."
		}
	}
	return s.done(project, "regions/"+region, "expandIpCidrRange", rel, o["id"])
}

func (s *Server) setPrivateAccess(project, region, name string, b obj) (obj, error) {
	o, rel, err := s.subnetLocked(project, region, name)
	if err != nil {
		return nil, err
	}
	o["privateIpGoogleAccess"] = b["privateIpGoogleAccess"] == true
	o["fingerprint"] = newFingerprint()
	return s.done(project, "regions/"+region, "setPrivateIpGoogleAccess", rel, o["id"])
}
