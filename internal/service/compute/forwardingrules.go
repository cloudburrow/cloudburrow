package compute

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// Private Service Connect endpoints (#2141): a regional forwarding rule
// whose target is a producer's service attachment, as a Cloud SQL
// instance with Private Service Connect publishes one
// (https://docs.cloud.google.com/sql/docs/postgres/configure-private-service-connect,
// https://docs.cloud.google.com/vpc/docs/configure-private-service-connect-services,
// https://docs.cloud.google.com/compute/docs/reference/rest/v1/forwardingRules,
// re-read 2026-11-22): its IPAddress is a regional INTERNAL address of
// the network (a reserved address's name or link, or an address of one
// of the network's subnets in the region), and its pscConnectionStatus is
// ACCEPTED while the endpoint's project is on the producer's accept list
// and PENDING otherwise ("it stays in a PENDING state until that project
// is added"), read again each time, so removing a project closes its
// connection. A VM of the network reaches the producer at the endpoint's
// address: the guest's ruleset sends it on (vpc.go). Load balancing
// forwarding rules are not served.

// ServiceAttachmentFunc resolves a service attachment
// (projects/P/regions/R/serviceAttachments/N) for an endpoint of
// consumer: where its connections go, and whether consumer may connect;
// ok is false when no producer publishes it. It is called with the
// server's lock held, and must not call back into it.
type ServiceAttachmentFunc func(attachment, consumer string) (target PrivateService, accepted, ok bool)

func (s *Server) insertForwardingRule(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	scope := "regions/" + region
	rel := "projects/" + project + "/" + scope + "/forwardingRules/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	target := str(b, "target")
	if target == "" {
		return nil, required("resource.target")
	}
	attach := Rel(target, project)
	if !strings.Contains(attach, "/serviceAttachments/") {
		return nil, unimplemented("a forwarding rule whose target is %s: only Private Service Connect endpoints (a service attachment target) are served", target)
	}
	if s.opts.ServiceAttachment == nil {
		return nil, notFound(attach)
	}
	if _, _, ok := s.opts.ServiceAttachment(attach, project); !ok {
		return nil, notFound(attach)
	}
	if lb := str(b, "loadBalancingScheme"); lb != "" {
		return nil, invalid("Invalid value for field 'resource.loadBalancingScheme': '%s'. A Private Service Connect forwarding rule has no load balancing scheme.", lb)
	}
	network := str(b, "network")
	if network == "" {
		network = "projects/" + project + "/global/networks/default"
	}
	network = Rel(network, project)
	if !strings.Contains(network, "/") {
		network = "projects/" + project + "/global/networks/" + network
	}
	if _, ok := s.res[network]; !ok {
		return nil, notFound(network)
	}
	ip, addrRel, sub, err := s.endpointAddressLocked(project, region, network, str(b, "IPAddress"), str(b, "subnetwork"))
	if err != nil {
		return nil, err
	}
	o := obj{"kind": "compute#forwardingRule", "name": name, "selfLink": rel, "region": "projects/" + project + "/" + scope,
		"IPAddress": ip, "network": network, "target": attach, "pscConnectionId": newID(), "fingerprint": newFingerprint()}
	if sub != "" {
		o["subnetwork"] = sub
	}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if v, ok := b["allowPscGlobalAccess"].(bool); ok && v {
		o["allowPscGlobalAccess"] = true
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"] = l
	}
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	s.pscStatusLocked(o)
	if addrRel != "" {
		a := s.res[addrRel]
		a["status"], a["users"] = "IN_USE", []any{rel}
	}
	s.res[rel] = norm(o)
	return s.done(project, scope, "insert", rel, s.res[rel]["id"])
}

// endpointAddressLocked is an endpoint's address: a reserved regional
// internal address (its name, link or IP) of network, or, unset, the
// first free address of the network's subnet in region (sub, or the
// first).
func (s *Server) endpointAddressLocked(project, region, network, ref, sub string) (ip, addrRel, subnet string, err error) {
	if ref != "" && net.ParseIP(ref) == nil {
		addrRel = Rel(ref, project)
		if !strings.Contains(addrRel, "/") {
			addrRel = "projects/" + project + "/regions/" + region + "/addresses/" + ref
		}
	} else if ref != "" {
		for k, a := range s.res {
			if strings.Contains(k, "/regions/"+region+"/addresses/") && str(a, "address") == ref && str(a, "network") == network {
				addrRel = k
			}
		}
	}
	if addrRel != "" {
		a, ok := s.res[addrRel]
		if !ok {
			return "", "", "", notFound(addrRel)
		}
		if str(a, "addressType") != "INTERNAL" || lastSeg(str(a, "region")) != region {
			return "", "", "", invalid("Invalid value for field 'resource.IPAddress': '%s'. A Private Service Connect endpoint's address must be a regional internal address in region '%s'.", ref, region)
		}
		if str(a, "network") != network {
			return "", "", "", invalid("Invalid value for field 'resource.IPAddress': '%s'. The address is not in network '%s'.", ref, network)
		}
		if users := strs(a, "users"); len(users) > 0 {
			return "", "", "", errf(http.StatusBadRequest, "invalid", "IP address '%s' is already being used by another resource.", str(a, "address"))
		}
		return str(a, "address"), addrRel, str(a, "subnetwork"), nil
	}
	if sub != "" {
		sub = Rel(sub, project)
		if !strings.Contains(sub, "/") {
			sub = subnetRel(project, region, sub)
		}
	} else {
		var subs []string
		for k, o := range s.res {
			if strings.Contains(k, "/regions/"+region+"/subnetworks/") && str(o, "network") == network {
				subs = append(subs, k)
			}
		}
		sort.Strings(subs)
		if len(subs) == 0 {
			return "", "", "", invalid("Invalid value for field 'resource.network': '%s'. The network has no subnetwork in region '%s'.", network, region)
		}
		sub = subs[0]
	}
	so, ok := s.res[sub]
	if !ok {
		return "", "", "", notFound(sub)
	}
	if str(so, "network") != network {
		return "", "", "", invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork is not in network '%s'.", sub, network)
	}
	n, _ := ParseCIDR(str(so, "ipCidrRange"))
	used := s.usedInSubnetLocked(sub)
	if ref != "" {
		v := net.ParseIP(ref).To4()
		if v == nil || n == nil || !n.Contains(v) || reservedAddr(n, v) {
			return "", "", "", invalid("Invalid value for field 'resource.IPAddress': '%s'. The address is not a usable address of subnetwork '%s'.", ref, lastSeg(sub))
		}
		if used[v.String()] != "" {
			return "", "", "", errf(http.StatusBadRequest, "invalid", "IP address '%s' is already being used by another resource.", v)
		}
		return v.String(), "", sub, nil
	}
	lo := ip4(n.IP)
	for v := lo + 2; v < lo+rangeSize(n)-2; v++ {
		if used[fromU32(v).String()] == "" {
			return fromU32(v).String(), "", sub, nil
		}
	}
	return "", "", "", errf(http.StatusBadRequest, "ipSpaceExhausted", "IP space of '%s' is exhausted.", sub)
}

// pscStatusLocked sets o's pscConnectionStatus from its producer's accept
// list now: ACCEPTED, PENDING, or CLOSED once the attachment is gone.
func (s *Server) pscStatusLocked(o obj) {
	status := "CLOSED"
	if s.opts.ServiceAttachment != nil {
		project := strings.Split(str(o, "selfLink"), "/")[1]
		if _, accepted, ok := s.opts.ServiceAttachment(str(o, "target"), project); ok {
			status = "PENDING"
			if accepted {
				status = "ACCEPTED"
			}
		}
	}
	o["pscConnectionStatus"] = status
}

func (s *Server) deleteForwardingRule(project, region, name string) (obj, error) {
	rel := "projects/" + project + "/regions/" + region + "/forwardingRules/" + name
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	for k, a := range s.res {
		if strings.Contains(k, "/addresses/") {
			if users := strs(a, "users"); len(users) == 1 && users[0] == rel {
				delete(a, "users")
				a["status"] = "RESERVED"
			}
		}
	}
	delete(s.res, rel)
	return s.done(project, "regions/"+region, "delete", rel, o["id"])
}

// forwardingRulesLocked answers the regional forwardingRules collection.
func (s *Server) forwardingRulesLocked(c call, region string, p []string, get, post, del bool) (obj, error) {
	n := len(p)
	rel := "projects/" + c.project + "/regions/" + region + "/forwardingRules"
	switch {
	case n == 3 && get:
		for k, o := range s.res {
			if strings.HasPrefix(k, rel+"/") {
				s.pscStatusLocked(o)
			}
		}
		return s.listOut("compute#forwardingRuleList", rel, s.res, c.query)
	case n == 3 && post:
		return s.insertForwardingRule(c.project, region, c.body)
	case n == 4 && get:
		o, ok := s.res[rel+"/"+p[3]]
		if !ok {
			return nil, notFound(rel + "/" + p[3])
		}
		s.pscStatusLocked(o)
		return o, nil
	case n == 4 && del:
		if s.automated[rel+"/"+p[3]] {
			// "You cannot directly delete Private Service Connect endpoints
			// that are created through service connectivity automation"
			// (about-service-connection-policies). The wording is UNVERIFIED
			// (re-read 2026-11-22: the same page quotes no error).
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The forwarding rule '%s' was created by service connectivity automation and cannot be deleted directly: "+
					"decommission the service instance that uses it.", rel+"/"+p[3])
		}
		return s.deleteForwardingRule(c.project, region, p[3])
	case n == 5 && post && p[4] == "setLabels":
		return s.setLabels(c.project, "regions/"+region, rel+"/"+p[3], c.body)
	}
	return nil, unimplemented("%s %s is not served", c.method, strings.Join(p, "/"))
}

// pscEndpointsLocked are the accepted Private Service Connect endpoints
// a VM of network in region reaches: an endpoint of another region only
// with global access.
func (s *Server) pscEndpointsLocked(network, region string) []PrivateService {
	if s.opts.ServiceAttachment == nil {
		return nil
	}
	var out []PrivateService
	for k, o := range s.res {
		if !strings.Contains(k, "/forwardingRules/") || str(o, "network") != network {
			continue
		}
		if lastSeg(str(o, "region")) != region && o["allowPscGlobalAccess"] != true {
			continue
		}
		project := strings.Split(k, "/")[1]
		t, accepted, ok := s.opts.ServiceAttachment(str(o, "target"), project)
		if !ok || !accepted {
			continue
		}
		t.Name, t.IP = "psc "+lastSeg(k), str(o, "IPAddress")
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}
