package compute

import (
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Private Service Connect (#2140), consumer and producer side, as
// https://docs.cloud.google.com/vpc/docs/configure-private-service-connect-apis,
// configure-private-service-connect-services,
// configure-private-service-connect-producer and the REST references of
// globalForwardingRules, forwardingRules and serviceAttachments (read
// 2026-11-22) describe them:
//
//   - an endpoint for Google APIs is a global forwarding rule whose target
//     is the bundle "all-apis" or "vpc-sc", with no load balancing scheme,
//     its IPAddress a global internal address of purpose
//     PRIVATE_SERVICE_CONNECT in its network (which becomes IN_USE by it),
//     its name 1-20 lowercase letters and digits starting with a letter;
//     its pscConnectionStatus is ACCEPTED;
//   - an endpoint for a published service is a regional forwarding rule
//     whose target is a service attachment of its region, its IPAddress a
//     regional internal address of a subnet of its network (or one chosen
//     in its subnetwork); its pscConnectionStatus is ACCEPTED when the
//     attachment accepts automatically or lists the consumer project in
//     consumerAcceptLists, REJECTED when consumerRejectLists names it,
//     PENDING otherwise, and CLOSED once the attachment is deleted;
//   - a service attachment publishes a producer's targetService with
//     natSubnets of purpose PRIVATE_SERVICE_CONNECT in its region, a
//     connectionPreference ACCEPT_AUTOMATIC or ACCEPT_MANUAL, and lists the
//     endpoints connected to it (connectedEndpoints).
//
// What is carried: an endpoint for a published service takes a consumer
// VM's TCP connections at its IP address to the attachment's producer
// load balancer (vpcservices.go); VMs reach the local Google API endpoints
// over the host whatever endpoint for Google APIs exists (docs/gaps.md).

// pscGoogleName is the name rule of an endpoint for Google APIs.
var pscGoogleName = regexp.MustCompile(`^[a-z][a-z0-9]{0,19}$`)

// pscStatus is the pscConnectionStatus of an endpoint of the consumer
// project on attachment a.
func pscStatus(a obj, consumer string) string {
	if a == nil {
		return "CLOSED"
	}
	for _, r := range strs(a, "consumerRejectLists") {
		if r == consumer {
			return "REJECTED"
		}
	}
	if str(a, "connectionPreference") == "ACCEPT_AUTOMATIC" {
		return "ACCEPTED"
	}
	for _, e := range sliceOf(a["consumerAcceptLists"]) {
		m, _ := e.(map[string]any)
		if str(m, "projectIdOrNum") == consumer || (str(m, "networkUrl") != "" && strings.HasPrefix(Rel(str(m, "networkUrl"), ""), "projects/"+consumer+"/")) {
			return "ACCEPTED"
		}
	}
	return "PENDING"
}

// forwardingRules serves globalForwardingRules ("global") and a region's
// forwardingRules.
func (s *Server) forwardingRules(c call, region string) (obj, error) {
	scope, base := "global", 2
	if region != "" {
		scope, base = "regions/"+region, 3
	}
	p, n := c.parts, len(c.parts)
	rel := "projects/" + c.project + "/" + scope + "/forwardingRules"
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == base && get:
		for k, o := range s.res {
			if strings.HasPrefix(k, rel+"/") {
				s.pscStatusLocked(o)
			}
		}
		return s.listOut("compute#forwardingRuleList", rel, s.res, c.query)
	case n == base && post:
		return s.insertForwardingRule(c.project, region, c.body)
	case n == base+1 && get:
		if o, ok := s.res[rel+"/"+p[base]]; ok {
			s.pscStatusLocked(o)
		}
		return s.getRes(rel + "/" + p[base])
	case n == base+1 && del:
		if s.automated[rel+"/"+p[base]] {
			// "You cannot directly delete Private Service Connect endpoints
			// that are created through service connectivity automation"
			// (about-service-connection-policies). The wording is UNVERIFIED
			// (re-read 2026-11-22: the same page quotes no error).
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The forwarding rule '%s' was created by service connectivity automation and cannot be deleted directly: "+
					"decommission the service instance that uses it.", rel+"/"+p[base])
		}
		return s.deleteForwardingRule(c.project, scope, rel+"/"+p[base])
	case n == base+1 && c.method == http.MethodPatch:
		return s.patchForwardingRule(c.project, scope, rel+"/"+p[base], c.body)
	case n == base+2 && post && p[base+1] == "setLabels":
		return s.setLabels(c.project, scope, rel+"/"+p[base], c.body)
	case n == base+2 && post && p[base+1] == "setTarget":
		return s.setForwardingRuleTarget(c.project, scope, rel+"/"+p[base], c.body)
	}
	return nil, unimplemented("%s %s/%s is not served: forwarding rules are Private Service Connect endpoints here (insert, get, list, patch, delete, setLabels, setTarget)",
		c.method, rel, strings.Join(p[base:], "/"))
}

func (s *Server) insertForwardingRule(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	scope := "global"
	if region != "" {
		scope = "regions/" + region
	}
	rel := "projects/" + project + "/" + scope + "/forwardingRules/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	target := str(b, "target")
	if target == "" {
		return nil, unimplemented("a forwarding rule without a target (a backend service's load balancer) is not served: forwarding rules are Private Service Connect endpoints here")
	}
	if ls := str(b, "loadBalancingScheme"); ls != "" {
		return nil, invalid("Invalid value for field 'resource.loadBalancingScheme': '%s'. A Private Service Connect endpoint has no load balancing scheme.", ls)
	}
	nw := str(b, "network")
	if nw == "" {
		return nil, required("resource.network")
	}
	nw, _ = NetworkName(nw, project)
	if _, ok := s.res[nw]; !ok {
		return nil, notFound(nw)
	}
	o := obj{"kind": "compute#forwardingRule", "name": name, "selfLink": rel, "network": nw,
		"pscConnectionId": strconvU(uint64(s.opts.Now().UnixNano()) & (1<<62 - 1)), "fingerprint": newFingerprint()}
	if d := str(b, "description"); d != "" {
		o["description"] = d
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"] = l
	}
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	if region == "" {
		if target != "all-apis" && target != "vpc-sc" {
			return nil, unimplemented("global forwarding rule target '%s': only the Private Service Connect bundles all-apis and vpc-sc are served", target)
		}
		if !pscGoogleName.MatchString(name) {
			return nil, invalid("Invalid value for field 'resource.name': '%s'. The name of an endpoint for Google APIs must be 1-20 characters, lowercase letters and numbers only, starting with a letter.", name)
		}
		addr, err := s.pscAddressLocked(project, "global", str(b, "IPAddress"), nw)
		if err != nil {
			return nil, err
		}
		if str(addr, "purpose") != "PRIVATE_SERVICE_CONNECT" {
			return nil, invalid("Invalid value for field 'resource.IPAddress': '%s'. The address of an endpoint for Google APIs must be a global internal address of purpose PRIVATE_SERVICE_CONNECT.", str(b, "IPAddress"))
		}
		if b["allowPscGlobalAccess"] == true {
			return nil, invalid("Invalid value for field 'resource.allowPscGlobalAccess': true. Global access applies to endpoints for published services only.")
		}
		o["target"], o["IPAddress"], o["pscConnectionStatus"] = target, str(addr, "address"), "ACCEPTED"
		o["_address"] = str(addr, "selfLink")
		addr["status"], addr["users"] = "IN_USE", []any{rel}
	} else {
		o["region"] = "projects/" + project + "/regions/" + region
		att := Rel(target, project)
		parts := strings.Split(att, "/")
		if len(parts) != 6 || parts[2] != "regions" || parts[4] != "serviceAttachments" {
			return nil, unimplemented("forwarding rule target '%s': only a service attachment (Private Service Connect) is served as a regional target", target)
		}
		if parts[3] != region {
			return nil, invalid("Invalid value for field 'resource.target': '%s'. The service attachment must be in region '%s'.", target, region)
		}
		// The attachment is one this server keeps, or another service's:
		// a Cloud SQL instance's with Private Service Connect (#2141).
		a, ok := s.res[att]
		if !ok {
			if s.opts.ServiceAttachment == nil {
				return nil, notFound(att)
			}
			if _, _, found := s.opts.ServiceAttachment(att, project); !found {
				return nil, notFound(att)
			}
		}
		o["target"] = att
		// IPAddress is a reserved regional internal address (its name, link
		// or IP) or an unused address of the subnetwork; unset, one is
		// chosen in the subnetwork (or the network's first in the region).
		ip, addrRel, sub, err := s.endpointAddressLocked(project, region, nw, str(b, "IPAddress"), str(b, "subnetwork"))
		if err != nil {
			return nil, err
		}
		o["IPAddress"] = ip
		if sub != "" {
			o["subnetwork"] = sub
		}
		if addrRel != "" {
			addr := s.res[addrRel]
			o["_address"] = addrRel
			addr["status"], addr["users"] = "IN_USE", []any{rel}
		}
		if v, ok := b["allowPscGlobalAccess"].(bool); ok && v {
			o["allowPscGlobalAccess"] = true
		}
		if ok {
			o["pscConnectionStatus"] = pscStatus(a, project)
		} else {
			s.pscStatusLocked(o)
		}
	}
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	s.res[rel] = norm(o)
	return s.done(project, scope, "insert", rel, s.res[rel]["id"])
}

// pscAddressLocked is the address an endpoint's IPAddress names: a link,
// a name in scope, or an address reserved there.
func (s *Server) pscAddressLocked(project, scope, ref, network string) (obj, error) {
	if ref == "" {
		return nil, required("resource.IPAddress")
	}
	var a obj
	if net.ParseIP(ref) != nil {
		for k, o := range s.res {
			if strings.HasPrefix(k, "projects/"+project+"/"+scope+"/addresses/") && str(o, "address") == ref {
				a = o
			}
		}
		if a == nil {
			return nil, invalid("Invalid value for field 'resource.IPAddress': '%s'. A Private Service Connect endpoint's address must be reserved first.", ref)
		}
	} else {
		rel := Rel(ref, project)
		if !strings.Contains(rel, "/") {
			rel = "projects/" + project + "/" + scope + "/addresses/" + rel
		}
		o, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		a = o
	}
	if str(a, "network") != network {
		return nil, invalid("Invalid value for field 'resource.IPAddress': '%s'. The address is not in network '%s'.", ref, lastSeg(network))
	}
	if len(strs(a, "users")) > 0 {
		return nil, errf(http.StatusBadRequest, "invalid", "IP address '%s' is already being used by another resource.", str(a, "address"))
	}
	return a, nil
}

func (s *Server) deleteForwardingRule(project, scope, rel string) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if a, ok := s.res[str(o, "_address")]; ok {
		a["status"] = "RESERVED"
		delete(a, "users")
	}
	delete(s.res, rel)
	return s.done(project, scope, "delete", rel, o["id"])
}

// serviceAttachments serves a region's serviceAttachments.
func (s *Server) serviceAttachments(c call, region string) (obj, error) {
	p, n := c.parts, len(c.parts)
	scope := "regions/" + region
	rel := "projects/" + c.project + "/" + scope + "/serviceAttachments"
	switch {
	case n == 3 && c.method == http.MethodGet:
		out, err := s.listOut("compute#serviceAttachmentList", rel, s.res, c.query)
		if err == nil {
			for i, it := range out["items"].([]obj) {
				out["items"].([]obj)[i] = s.withEndpointsLocked(it)
			}
		}
		return out, err
	case n == 3 && c.method == http.MethodPost:
		return s.insertServiceAttachment(c.project, region, c.body)
	case n == 4 && c.method == http.MethodGet:
		o, err := s.getRes(rel + "/" + p[3])
		if err != nil {
			return nil, err
		}
		return s.withEndpointsLocked(o), nil
	case n == 4 && c.method == http.MethodPatch:
		return s.patchServiceAttachment(c.project, region, rel+"/"+p[3], c.body)
	case n == 4 && c.method == http.MethodDelete:
		o, ok := s.res[rel+"/"+p[3]]
		if !ok {
			return nil, notFound(rel + "/" + p[3])
		}
		delete(s.res, rel+"/"+p[3])
		s.refreshEndpointsLocked(rel + "/" + p[3])
		return s.done(c.project, scope, "delete", rel+"/"+p[3], o["id"])
	}
	return nil, unimplemented("%s %s/%s is not served", c.method, rel, strings.Join(p[3:], "/"))
}

// checkAttachment validates and normalises an attachment's settable
// fields into o.
func (s *Server) checkAttachment(project, region string, b, o obj, create bool) error {
	if v, ok := b["connectionPreference"]; ok || create {
		cp, _ := v.(string)
		switch cp {
		case "ACCEPT_AUTOMATIC", "ACCEPT_MANUAL":
		case "":
			return required("resource.connectionPreference")
		default:
			return invalid("Invalid value for field 'resource.connectionPreference': '%s'. Must be one of [ACCEPT_AUTOMATIC, ACCEPT_MANUAL]", cp)
		}
		o["connectionPreference"] = cp
	}
	if v, ok := b["natSubnets"]; ok || create {
		l := strsAny(v)
		if len(l) == 0 {
			return required("resource.natSubnets")
		}
		var subs []any
		for i, e := range l {
			sub := Rel(e, project)
			if !strings.Contains(sub, "/") {
				sub = subnetRel(project, region, sub)
			}
			so, ok := s.res[sub]
			if !ok {
				return notFound(sub)
			}
			field := "resource.natSubnets[" + strconv.Itoa(i) + "]"
			if lastSeg(str(so, "region")) != region {
				return invalid("Invalid value for field '%s': '%s'. The subnetwork must be in region '%s'.", field, e, region)
			}
			if str(so, "purpose") != "PRIVATE_SERVICE_CONNECT" {
				return invalid("Invalid value for field '%s': '%s'. A NAT subnet of a service attachment must have purpose PRIVATE_SERVICE_CONNECT.", field, e)
			}
			subs = append(subs, sub)
		}
		o["natSubnets"] = subs
	}
	if v, ok := b["consumerAcceptLists"]; ok {
		var out []any
		for i, e := range sliceOf(v) {
			m, _ := e.(map[string]any)
			if str(m, "projectIdOrNum") == "" && str(m, "networkUrl") == "" {
				return required("resource.consumerAcceptLists[" + strconv.Itoa(i) + "].projectIdOrNum")
			}
			out = append(out, m)
		}
		o["consumerAcceptLists"] = out
	}
	if v, ok := b["consumerRejectLists"]; ok {
		o["consumerRejectLists"] = sliceOf(v)
	}
	for _, k := range []string{"enableProxyProtocol", "description", "domainNames", "reconcileConnections"} {
		if v, ok := b[k]; ok {
			o[k] = v
		}
	}
	return nil
}

func (s *Server) insertServiceAttachment(project, region string, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	scope := "regions/" + region
	rel := "projects/" + project + "/" + scope + "/serviceAttachments/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	ts := str(b, "targetService")
	if ts == "" {
		return nil, required("resource.targetService")
	}
	// Google's targetService is the producer's forwarding rule, of the
	// attachment's own region (vpc/docs/configure-private-service-connect-producer):
	// an internal passthrough Network Load Balancer's, a regional internal
	// proxy load balancer's or internal protocol forwarding's, all served
	// since #2153, so it must exist and be one of those.
	tparts := strings.Split(Rel(ts, project), "/")
	if len(tparts) != 6 || tparts[0] != "projects" || tparts[2] != "regions" || tparts[4] != "forwardingRules" {
		return nil, invalid("Invalid value for field 'resource.targetService': '%s'. Must be the URL of a forwarding rule.", ts)
	}
	if tparts[3] != region {
		return nil, invalid("Invalid value for field 'resource.targetService': '%s'. The forwarding rule must be in the service attachment's region '%s'.", ts, region)
	}
	fr, ok := s.res[Rel(ts, project)]
	if !ok {
		return nil, notFound(Rel(ts, project))
	}
	if sch := str(fr, "loadBalancingScheme"); fr["_lb"] != true || (sch != "INTERNAL" && sch != "INTERNAL_MANAGED") {
		return nil, invalid("Invalid value for field 'resource.targetService': '%s'. The producer forwarding rule must be the forwarding rule of an internal passthrough Network Load Balancer, a regional internal proxy load balancer or internal protocol forwarding.", ts)
	}
	id := newID()
	o := obj{"kind": "compute#serviceAttachment", "name": name, "selfLink": rel, "region": "projects/" + project + "/" + scope,
		"targetService": Rel(ts, project), "enableProxyProtocol": false, "fingerprint": newFingerprint(),
		"pscServiceAttachmentId": obj{"high": stableID(rel), "low": id}}
	if err := s.checkAttachment(project, region, b, o, true); err != nil {
		return nil, err
	}
	o["id"], o["creationTimestamp"] = id, s.stamp()
	s.res[rel] = norm(o)
	return s.done(project, scope, "insert", rel, id)
}

func (s *Server) patchServiceAttachment(project, region, rel string, b obj) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if err := s.checkFingerprint(o, b); err != nil {
		return nil, err
	}
	next := copyObj(o)
	if err := s.checkAttachment(project, region, b, next, false); err != nil {
		return nil, err
	}
	next["fingerprint"] = newFingerprint()
	s.res[rel] = norm(next)
	s.refreshEndpointsLocked(rel)
	return s.done(project, "regions/"+region, "patch", rel, o["id"])
}

// refreshEndpointsLocked re-decides the status of the endpoints
// targeting the attachment.
func (s *Server) refreshEndpointsLocked(att string) {
	a := s.res[att]
	for k, o := range s.res {
		if strings.Contains(k, "/forwardingRules/") && str(o, "target") == att {
			o["pscConnectionStatus"] = pscStatus(a, strings.Split(k, "/")[1])
		}
	}
}

// withEndpointsLocked is the attachment with its connectedEndpoints.
func (s *Server) withEndpointsLocked(a obj) obj {
	rel := str(a, "selfLink")
	var eps []obj
	for k, o := range s.res {
		if strings.Contains(k, "/forwardingRules/") && str(o, "target") == rel {
			eps = append(eps, obj{"endpoint": k, "pscConnectionId": o["pscConnectionId"], "status": str(o, "pscConnectionStatus"),
				"consumerNetwork": str(o, "network")})
		}
	}
	sort.Slice(eps, func(i, j int) bool { return str(eps[i], "endpoint") < str(eps[j], "endpoint") })
	out := copyObj(a)
	if len(eps) > 0 {
		l := make([]any, len(eps))
		for i, e := range eps {
			l[i] = e
		}
		out["connectedEndpoints"] = l
	}
	return out
}

// pscUsersLocked are the forwarding rules and service attachments using
// the subnet or network rel.
func (s *Server) pscUsersLocked(rel string) []string {
	var out []string
	for k, o := range s.res {
		switch {
		case strings.Contains(k, "/forwardingRules/"):
			if str(o, "subnetwork") == rel || str(o, "network") == rel {
				out = append(out, k)
			}
		case strings.Contains(k, "/serviceAttachments/"):
			for _, n := range strs(o, "natSubnets") {
				if n == rel {
					out = append(out, k)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
