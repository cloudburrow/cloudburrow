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
// address: the guest's ruleset sends it on (vpc.go). The forwardingRules
// collection itself is psc.go's (#2140), which takes these targets as well
// as the service attachments it keeps; load balancers' rules are lbrules.go's.

// ServiceAttachmentFunc resolves a service attachment
// (projects/P/regions/R/serviceAttachments/N) for an endpoint of
// consumer: where its connections go, and whether consumer may connect;
// ok is false when no producer publishes it. It is called with the
// server's lock held, and must not call back into it.
type ServiceAttachmentFunc func(attachment, consumer string) (target PrivateService, accepted, ok bool)

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
		if pu := str(a, "purpose"); str(a, "addressType") != "INTERNAL" || lastSeg(str(a, "region")) != region || (pu != "" && pu != "GCE_ENDPOINT") {
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

// pscStatusLocked sets the pscConnectionStatus of o, an endpoint whose
// target is another service's attachment (a Cloud SQL instance's), from
// its producer's accept list now: ACCEPTED, PENDING, or CLOSED once the
// attachment is gone. An endpoint of an attachment this server keeps
// (psc.go), of Google APIs, or a load balancer's rule is left as it is.
func (s *Server) pscStatusLocked(o obj) {
	t := str(o, "target")
	if !strings.Contains(t, "/serviceAttachments/") || str(o, "loadBalancingScheme") != "" {
		return
	}
	if _, ok := s.res[t]; ok {
		return
	}
	status := "CLOSED"
	if s.opts.ServiceAttachment != nil {
		project := strings.Split(str(o, "selfLink"), "/")[1]
		if _, accepted, ok := s.opts.ServiceAttachment(t, project); ok {
			status = "PENDING"
			if accepted {
				status = "ACCEPTED"
			}
		}
	}
	o["pscConnectionStatus"] = status
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
