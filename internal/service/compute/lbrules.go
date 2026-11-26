package compute

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Load-balancing forwarding rules (#2153), as
// https://docs.cloud.google.com/load-balancing/docs/forwarding-rule-concepts
// and the forwardingRules and globalForwardingRules REST references
// describe them (read 2026-11-22): a rule with a load balancing scheme
// sends its address and ports to a target proxy, a target pool, a target
// instance, or (a passthrough Network Load Balancer) a regional backend
// service.
//
// Private Service Connect forwarding rules (no scheme; a service
// attachment or a Google API bundle as target) are not handled here: the
// Private Service Connect code serves them (psc.go), and this file takes
// only requests that are load balancing's, and records it made (_lb), so
// both live in the one forwardingRules collection.
//
// Here a rule's address is CloudBurrow's networking model's: an external
// rule's ephemeral IPAddress is 127.0.0.1, as a VM's external IP is (a
// reserved address keeps its own); an internal rule's is chosen in its
// subnetwork. Each port of the rule is listened on at 127.0.0.1, on a port
// this machine chooses, named in cloudburrowAddress (the first port) and
// cloudburrowAddresses (every port, PORT=HOST:PORT), as an instance's SSH
// port is in cloudburrowSshAddress.

var lbTargetColls = []string{"targetHttpProxies", "targetHttpsProxies", "targetTcpProxies", "targetSslProxies", "targetPools",
	"targetInstances", "targetGrpcProxies", "targetVpnGateways"}

// maxRulePorts is how many ports of one rule are listened on.
const maxRulePorts = 64

// isLBRuleBody is whether an insert is load balancing's.
func isLBRuleBody(b obj) bool {
	if str(b, "loadBalancingScheme") != "" || str(b, "backendService") != "" {
		return true
	}
	t := Rel(str(b, "target"), "")
	for _, c := range lbTargetColls {
		if strings.Contains(t, "/"+c+"/") {
			return true
		}
	}
	return false
}

// lbForwardingRules takes the forwardingRules calls that are load
// balancing's; ok is false for the rest (Private Service Connect).
func (s *Server) lbForwardingRules(c call, x lbCtx, rest []string) (obj, error, bool) {
	coll := x.collRel()
	switch {
	case len(rest) == 1 && c.method == http.MethodPost && isLBRuleBody(c.body):
		out, err := s.insertLBRule(x, norm(c.body))
		return out, err, true
	case len(rest) >= 2:
		x.rel = coll + "/" + rest[1]
		o, ok := s.res[x.rel]
		// A Private Service Connect endpoint's calls, its patch and
		// setTarget among them, are pscupdate.go's (#2140).
		if !ok || o["_lb"] != true {
			return nil, nil, false
		}
		switch {
		case len(rest) == 2 && c.method == http.MethodDelete:
			out, err := s.deleteLBRule(x, o)
			return out, err, true
		case len(rest) == 2 && c.method == http.MethodPatch:
			out, err := s.patchLBRule(x, o, norm(c.body))
			return out, err, true
		case len(rest) == 3 && c.method == http.MethodPost && rest[2] == "setTarget":
			out, err := s.setLBRuleTarget(x, o, norm(c.body))
			return out, err, true
		}
	}
	return nil, nil, false
}

// ruleTarget checks a rule's target (or backendService) against its scope
// and scheme, and returns its relative name.
func (s *Server) ruleTargetLocked(x lbCtx, scheme string, b obj) (target, bs string, err error) {
	t, svc := str(b, "target"), str(b, "backendService")
	if (t == "") == (svc == "") {
		return "", "", invalid("Invalid value for field 'resource': ''. Exactly one of target or backendService must be set.")
	}
	if svc != "" {
		if x.region == "" {
			return "", "", invalid("Invalid value for field 'resource.backendService': '%s'. Only a regional forwarding rule can have a backend service.", svc)
		}
		rel, o, err := s.lbRef(x, "resource.backendService", svc, "backendServices")
		if err != nil {
			return "", "", err
		}
		if scopeOf(rel) != x.scope {
			return "", "", invalid("Invalid value for field 'resource.backendService': '%s'. The backend service must be in region '%s'.", svc, x.region)
		}
		if bsc := str(o, "loadBalancingScheme"); bsc != scheme || (scheme != "INTERNAL" && scheme != "EXTERNAL") {
			return "", "", invalid("Invalid value for field 'resource.backendService': '%s'. A forwarding rule of scheme %s cannot use a backend service of scheme %s.", svc, scheme, bsc)
		}
		return "", rel, nil
	}
	rel := Rel(t, x.project)
	if !strings.HasPrefix(rel, "projects/") {
		return "", "", invalid("Invalid value for field 'resource.target': '%s'. The URL is malformed.", t)
	}
	parts := strings.Split(rel, "/")
	coll := parts[len(parts)-2]
	switch coll {
	case "targetGrpcProxies":
		if x.region != "" || scheme != "INTERNAL_SELF_MANAGED" {
			return "", "", invalid("Invalid value for field 'resource.target': '%s'. A target gRPC proxy is the target of a global forwarding rule of the INTERNAL_SELF_MANAGED load balancing scheme.", t)
		}
		to, ok := s.res[rel]
		if !ok {
			return "", "", notFound(rel)
		}
		if err := s.checkRuleSchemesLocked(rel, scheme, str(to, "urlMap")); err != nil {
			return "", "", err
		}
		return rel, "", nil
	case "targetVpnGateways":
		return "", "", unimplemented("Classic VPN is not served (#2153)")
	}
	to, ok := s.res[rel]
	if !ok {
		return "", "", notFound(rel)
	}
	tscope := scopeOf(rel)
	switch coll {
	case "targetInstances":
		if x.region == "" || regionOfScope(tscope) != x.region {
			return "", "", invalid("Invalid value for field 'resource.target': '%s'. The target instance must be in region '%s'.", t, x.region)
		}
		if scheme != "EXTERNAL" && scheme != "INTERNAL" {
			return "", "", invalid("Invalid value for field 'resource.loadBalancingScheme': '%s'. A target instance's forwarding rule is EXTERNAL or INTERNAL.", scheme)
		}
		return rel, "", nil
	case "targetPools":
		if tscope != x.scope {
			return "", "", invalid("Invalid value for field 'resource.target': '%s'. The target pool must be in region '%s'.", t, x.region)
		}
		if scheme != "EXTERNAL" {
			return "", "", invalid("Invalid value for field 'resource.loadBalancingScheme': '%s'. A target pool's forwarding rule is EXTERNAL.", scheme)
		}
		return rel, "", nil
	}
	if tscope != x.scope {
		return "", "", invalid("Invalid value for field 'resource.target': '%s'. The target proxy must be in the forwarding rule's scope (%s).", t, x.scope)
	}
	if err := s.checkRuleSchemesLocked(rel, scheme, str(to, "urlMap")); err != nil {
		return "", "", err
	}
	if svc := str(to, "service"); svc != "" {
		if bsc := str(s.res[svc], "loadBalancingScheme"); bsc != scheme {
			return "", "", invalid("Invalid value for field 'resource.target': '%s'. A forwarding rule of scheme %s cannot use a backend service of scheme %s.", t, scheme, bsc)
		}
	}
	return rel, "", nil
}

// checkRuleSchemesLocked checks that a URL map's backend services have
// the scheme of the rules that use proxy (scheme "", every rule that
// targets proxy).
func (s *Server) checkRuleSchemesLocked(proxy, scheme, urlMap string) error {
	if urlMap == "" {
		return nil
	}
	schemes := []string{scheme}
	if scheme == "" {
		schemes = nil
		for _, o := range s.res {
			if o["_lb"] == true && str(o, "target") == proxy {
				schemes = append(schemes, str(o, "loadBalancingScheme"))
			}
		}
	}
	for _, svc := range urlMapServices(s.res[urlMap]) {
		bo := s.res[svc]
		if str(bo, "kind") == "compute#backendBucket" {
			for _, sc := range schemes {
				if sc != "EXTERNAL" && sc != "EXTERNAL_MANAGED" {
					return invalid("Invalid value for field 'resource.target': '%s'. A backend bucket serves only external Application Load Balancers.", proxy)
				}
			}
			continue
		}
		for _, sc := range schemes {
			if bsc := str(bo, "loadBalancingScheme"); bsc != sc {
				return invalid("Invalid value for field 'resource.target': '%s'. A forwarding rule of scheme %s cannot use backend service '%s' of scheme %s.", proxy, sc, svc, bsc)
			}
		}
	}
	return nil
}

// parseRulePorts are a rule's ports, from portRange or ports; nil for
// every port.
func parseRulePorts(b obj) ([]int, string, error) {
	pr, ports := str(b, "portRange"), strs(b, "ports")
	if pr != "" && len(ports) > 0 {
		return nil, "", invalid("Invalid value for field 'resource.ports': ''. Only one of portRange and ports can be set.")
	}
	if b["allPorts"] == true {
		if pr != "" || len(ports) > 0 {
			return nil, "", invalid("Invalid value for field 'resource.allPorts': 'true'. allPorts cannot be set with portRange or ports.")
		}
		return nil, "", nil
	}
	parse := func(field, v string) (int, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return 0, invalid("Invalid value for field '%s': '%s'. Must be a port between 1 and 65535.", field, v)
		}
		return n, nil
	}
	if pr != "" {
		lo, hi := pr, pr
		if i := strings.Index(pr, "-"); i >= 0 {
			lo, hi = pr[:i], pr[i+1:]
		}
		a, err := parse("resource.portRange", lo)
		if err != nil {
			return nil, "", err
		}
		z, err := parse("resource.portRange", hi)
		if err != nil {
			return nil, "", err
		}
		if z < a {
			return nil, "", invalid("Invalid value for field 'resource.portRange': '%s'. The range is empty.", pr)
		}
		var out []int
		for p := a; p <= z; p++ {
			out = append(out, p)
		}
		return out, strconv.Itoa(a) + "-" + strconv.Itoa(z), nil
	}
	if len(ports) > 5 {
		return nil, "", invalid("Invalid value for field 'resource.ports': ''. At most 5 ports can be given; use portRange or allPorts.")
	}
	var out []int
	for i, v := range ports {
		n, err := parse("resource.ports["+strconv.Itoa(i)+"]", v)
		if err != nil {
			return nil, "", err
		}
		out = append(out, n)
	}
	return out, "", nil
}

func (s *Server) insertLBRule(x lbCtx, b obj) (obj, error) {
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	x.rel = x.collRel() + "/" + name
	if _, ok := s.res[x.rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", x.rel)
	}
	scheme, err := oneOf("resource.loadBalancingScheme", str(b, "loadBalancingScheme"), "EXTERNAL", bsSchemes...)
	if err != nil {
		return nil, err
	}
	if x.region == "" && scheme == "INTERNAL" {
		return nil, invalid("Invalid value for field 'resource.loadBalancingScheme': 'INTERNAL'. A global forwarding rule cannot have the INTERNAL load balancing scheme.")
	}
	target, bs, err := s.ruleTargetLocked(x, scheme, b)
	if err != nil {
		return nil, err
	}
	o := obj{"kind": "compute#forwardingRule", "name": name, "selfLink": x.rel, "loadBalancingScheme": scheme, "_lb": true,
		"fingerprint": newFingerprint()}
	if target != "" {
		o["target"] = target
	} else {
		o["backendService"] = bs
	}
	proxy := target != "" && (strings.Contains(target, "/targetHttp") || strings.Contains(target, "/targetTcpProxies/") || strings.Contains(target, "/targetSslProxies/"))
	proto := str(b, "IPProtocol")
	if proto == "" {
		proto = "TCP"
	}
	proto, err = oneOf("resource.IPProtocol", proto, "TCP", "TCP", "UDP", "ESP", "AH", "SCTP", "ICMP", "L3_DEFAULT")
	if err != nil {
		return nil, err
	}
	if proxy && proto != "TCP" {
		return nil, invalid("Invalid value for field 'resource.IPProtocol': '%s'. A forwarding rule of a proxy load balancer uses TCP.", proto)
	}
	o["IPProtocol"] = proto
	ports, pr, err := parseRulePorts(b)
	if err != nil {
		return nil, err
	}
	if proxy {
		if len(ports) == 0 {
			return nil, invalid("Invalid value for field 'resource.portRange': ''. A forwarding rule of a proxy load balancer needs a port.")
		}
		if len(ports) != 1 {
			return nil, invalid("Invalid value for field 'resource.portRange': '%s'. A forwarding rule of a proxy load balancer has a single port.", pr)
		}
	}
	switch {
	case pr != "":
		o["portRange"] = pr
	case len(ports) > 0:
		var l []any
		for _, p := range ports {
			l = append(l, strconv.Itoa(p))
		}
		o["ports"] = l
	case b["allPorts"] == true:
		o["allPorts"] = true
	}
	tier, err := oneOf("resource.networkTier", str(b, "networkTier"), "PREMIUM", "PREMIUM", "STANDARD")
	if err != nil {
		return nil, err
	}
	if tier == "STANDARD" && x.region == "" {
		return nil, invalid("Invalid value for field 'resource.networkTier': 'STANDARD'. A global forwarding rule must be in the Premium network tier.")
	}
	internal := strings.HasPrefix(scheme, "INTERNAL")
	if !internal {
		o["networkTier"] = tier
	}
	if x.region != "" {
		o["region"] = "projects/" + x.project + "/regions/" + x.region
	}
	// The network, and for regional managed proxies the proxy-only subnet
	// (load-balancing/docs/proxy-only-subnets: "you must create a
	// proxy-only subnet ... before you create the forwarding rule").
	if nw := str(b, "network"); nw != "" || internal || (x.region != "" && scheme == "EXTERNAL_MANAGED") {
		if nw == "" {
			nw = "global/networks/default"
		}
		rel, _ := NetworkName(nw, x.project)
		if _, ok := s.res[rel]; !ok {
			return nil, notFound(rel)
		}
		o["network"] = rel
		if x.region != "" && (scheme == "INTERNAL_MANAGED" || scheme == "EXTERNAL_MANAGED") && !s.hasProxyOnlySubnetLocked(rel, x.region) {
			return nil, invalid("Invalid value for field 'resource.target': '%s'. An active proxy-only subnetwork (purpose REGIONAL_MANAGED_PROXY) is required in the same region and VPC network as the forwarding rule.", str(b, "target"))
		}
	}
	if v, ok := b["allowGlobalAccess"].(bool); ok && v {
		if !internal {
			return nil, invalid("Invalid value for field 'resource.allowGlobalAccess': 'true'. Global access is for internal load balancers.")
		}
		o["allowGlobalAccess"] = true
	}
	copyFields(o, b, "sourceIpRanges", "description", "serviceDirectoryRegistrations", "metadataFilters")
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"] = l
	}
	o["labelFingerprint"] = labelFingerprint(o["labels"])
	if err := s.ruleIPVersionLocked(x, b, o, internal); err != nil {
		return nil, err
	}
	if err := s.ruleAddressLocked(x, b, o, internal, ports); err != nil {
		return nil, err
	}
	if sl := str(b, "serviceLabel"); sl != "" {
		if !internal || x.region == "" {
			return nil, invalid("Invalid value for field 'resource.serviceLabel': '%s'. A service label is for regional internal forwarding rules.", sl)
		}
		if !namedPortRE.MatchString(sl) {
			return nil, invalid("Invalid value for field 'resource.serviceLabel': '%s'. Must be a match of regex '[a-z]([-a-z0-9]{0,61}[a-z0-9])?'", sl)
		}
		layer := "il4"
		if scheme == "INTERNAL_MANAGED" {
			layer = "il7"
		}
		o["serviceLabel"] = sl
		o["serviceName"] = sl + "." + name + "." + layer + "." + x.region + ".lb." + x.project + ".internal"
	}
	o["id"], o["creationTimestamp"] = newID(), s.stamp()
	s.res[x.rel] = norm(o)
	if a, ok := s.res[str(o, "_address")]; ok {
		a["status"] = "IN_USE"
		a["users"] = toAny(append(strs(a, "users"), x.rel))
	}
	s.lbListenLocked(x.rel)
	return s.done(x.project, x.scope, "insert", x.rel, s.res[x.rel]["id"])
}

// hasProxyOnlySubnetLocked is whether network has an active proxy-only
// subnet in region.
func (s *Server) hasProxyOnlySubnetLocked(network, region string) bool {
	for k, o := range s.res {
		if strings.Contains(k, "/regions/"+region+"/subnetworks/") && str(o, "network") == network &&
			str(o, "purpose") == "REGIONAL_MANAGED_PROXY" && str(o, "role") != "BACKUP" {
			return true
		}
	}
	return false
}

// ruleAddressLocked sets a rule's IPAddress: a reserved address (its IP,
// name or link), or an ephemeral one.
func (s *Server) ruleAddressLocked(x lbCtx, b, o obj, internal bool, ports []int) error {
	if x.region != "" && str(o, "ipVersion") == "IPV6" {
		return s.ruleIPv6Locked(x, b, o, internal, ports)
	}
	ref := str(b, "IPAddress")
	scope := x.scope
	if str(o, "loadBalancingScheme") == "INTERNAL_SELF_MANAGED" && strings.Contains(str(o, "target"), "/targetGrpcProxies/") {
		// Proxyless gRPC's rule: 0.0.0.0, its clients' xDS matching by
		// port alone (proxyless-overview).
		if ref != "" && ref != "0.0.0.0" {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. A forwarding rule of a target gRPC proxy must have the address 0.0.0.0.", ref)
		}
		o["IPAddress"] = "0.0.0.0"
		return nil
	}
	var addrRel string
	if ref != "" && net.ParseIP(ref) == nil {
		addrRel = Rel(ref, x.project)
		if !strings.Contains(addrRel, "/") {
			addrRel = "projects/" + x.project + "/" + scope + "/addresses/" + ref
		}
		if _, ok := s.res[addrRel]; !ok {
			return notFound(addrRel)
		}
	} else if ref != "" {
		for k, a := range s.res {
			if strings.HasPrefix(k, "projects/"+x.project+"/"+scope+"/addresses/") && str(a, "address") == ref {
				addrRel = k
			}
		}
		if addrRel == "" && !(ref == "127.0.0.1" && !internal) {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address must be reserved first, or left unset for an ephemeral one.", ref)
		}
	}
	if addrRel != "" {
		a := s.res[addrRel]
		if scopeOf(addrRel) != scope {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address must be in the forwarding rule's scope (%s).", ref, scope)
		}
		if typ := str(a, "addressType"); (typ == "INTERNAL") != internal {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. A forwarding rule of this scheme needs an %s address.", ref, map[bool]string{true: "INTERNAL", false: "EXTERNAL"}[internal])
		}
		// An address may be shared by rules of other ports (an internal
		// one only with purpose SHARED_LOADBALANCER_VIP).
		for _, u := range strs(a, "users") {
			other := s.res[u]
			if internal && str(a, "purpose") != "SHARED_LOADBALANCER_VIP" {
				return errf(http.StatusBadRequest, "invalid", "IP address '%s' is already being used by another resource.", str(a, "address"))
			}
			if portsOverlap(other, ports) && str(other, "IPProtocol") == str(o, "IPProtocol") {
				return errf(http.StatusBadRequest, "invalid", "IP address '%s' and port are already being used by forwarding rule '%s'.", str(a, "address"), u)
			}
		}
		if v6 := str(a, "ipVersion") == "IPV6"; v6 != (str(o, "ipVersion") == "IPV6") {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address's IP version (%s) is not the forwarding rule's ipVersion.", ref, map[bool]string{true: "IPV6", false: "IPV4"}[v6])
		}
		o["IPAddress"], o["_address"] = str(a, "address"), addrRel
		if internal {
			o["subnetwork"] = str(a, "subnetwork")
		}
		return nil
	}
	if !internal && str(o, "ipVersion") == "IPV6" {
		// An ephemeral global IPv6 address, as a reserved one is chosen.
		a, err := s.freeGlobalV6Locked(x.rel)
		if err != nil {
			return err
		}
		o["IPAddress"] = a
		return nil
	}
	if !internal {
		o["IPAddress"] = "127.0.0.1"
		return nil
	}
	if x.region == "" {
		return unimplemented("an ephemeral address for a global internal forwarding rule: reserve a global internal address first (#2153)")
	}
	tmp := obj{}
	sub := str(b, "subnetwork")
	if sub == "" {
		// Google: the subnetwork "is optional" in an auto mode network,
		// whose subnet in the region is named after it, and "must be
		// specified" in a custom mode one (forwardingRules, subnetwork).
		net := str(o, "network")
		no, ok := s.res[net]
		if !ok {
			return notFound(net)
		}
		if a, _ := no["autoCreateSubnetworks"].(bool); !a {
			return invalid("Invalid value for field 'resource.subnetwork': ''. Subnetwork should be specified for custom subnetmode network")
		}
		sub = subnetRel(strings.Split(net, "/")[1], x.region, lastSeg(net))
	}
	if err := s.regionalInternal(x.project, x.region, obj{"subnetwork": sub}, tmp, "", ""); err != nil {
		return err
	}
	if str(tmp, "network") != str(o, "network") {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork is not in network '%s'.", sub, lastSeg(str(o, "network")))
	}
	o["IPAddress"], o["subnetwork"] = tmp["address"], tmp["subnetwork"]
	return nil
}

// rulePorts are a stored rule's ports, nil for every port.
func rulePorts(o obj) []int {
	ports, _, _ := parseRulePorts(obj{"portRange": o["portRange"], "ports": o["ports"], "allPorts": o["allPorts"]})
	return ports
}

func portsOverlap(o obj, ports []int) bool {
	have := rulePorts(o)
	if have == nil || ports == nil {
		return true
	}
	for _, a := range have {
		for _, b := range ports {
			if a == b {
				return true
			}
		}
	}
	return false
}

func (s *Server) deleteLBRule(x lbCtx, o obj) (obj, error) {
	if users := s.lbUsersLocked(x.rel); len(users) > 0 {
		return nil, inUse("forwardingRule", x.rel, users[0])
	}
	if a, ok := s.res[str(o, "_address")]; ok {
		var keep []string
		for _, u := range strs(a, "users") {
			if u != x.rel {
				keep = append(keep, u)
			}
		}
		if len(keep) == 0 {
			a["status"] = "RESERVED"
			delete(a, "users")
		} else {
			a["users"] = toAny(keep)
		}
	}
	delete(s.res, x.rel)
	s.lbChangedLocked()
	return s.done(x.project, x.scope, "delete", x.rel, o["id"])
}

// patchLBRule is forwardingRules.patch: the fields Google lets a rule
// change in place (allowGlobalAccess, and the network tier of a rule of an
// external passthrough load balancer).
func (s *Server) patchLBRule(x lbCtx, o, b obj) (obj, error) {
	if fp := str(b, "fingerprint"); fp != "" && !sameFingerprint(fp, str(o, "fingerprint")) {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet", "Supplied fingerprint does not match current metadata fingerprint.")
	}
	for k := range b {
		switch k {
		case "allowGlobalAccess", "allowPscGlobalAccess", "fingerprint", "name", "networkTier", "sourceIpRanges":
		default:
			return nil, invalid("Invalid value for field 'resource.%s': ''. The field cannot be patched.", k)
		}
	}
	return s.lbSet(x, o, "patch", func(o obj) error {
		if v, ok := b["allowGlobalAccess"].(bool); ok {
			if !strings.HasPrefix(str(o, "loadBalancingScheme"), "INTERNAL") {
				return invalid("Invalid value for field 'resource.allowGlobalAccess': '%v'. Global access is for internal load balancers.", v)
			}
			o["allowGlobalAccess"] = v
		}
		if t := str(b, "networkTier"); t != "" {
			if x.region == "" && t != "PREMIUM" {
				return invalid("Invalid value for field 'resource.networkTier': '%s'. A global forwarding rule must be in the Premium network tier.", t)
			}
			if _, err := oneOf("resource.networkTier", t, "", "PREMIUM", "STANDARD"); err != nil {
				return err
			}
			o["networkTier"] = t
		}
		if v, ok := b["allowPscGlobalAccess"].(bool); ok {
			if o["_lb"] == true {
				return invalid("Invalid value for field 'resource.allowPscGlobalAccess': '%v'. Only a Private Service Connect endpoint has global access to its service.", v)
			}
			o["allowPscGlobalAccess"] = v
		}
		if v, ok := b["sourceIpRanges"]; ok {
			o["sourceIpRanges"] = v
		}
		return nil
	})
}

// setLBRuleTarget is setTarget: another target of the same kind and scope.
func (s *Server) setLBRuleTarget(x lbCtx, o, b obj) (obj, error) {
	if str(o, "target") == "" {
		return nil, invalid("Invalid value for field 'targetReference.target': ''. A forwarding rule with a backend service has no target to set.")
	}
	t, _, err := s.ruleTargetLocked(x, str(o, "loadBalancingScheme"), obj{"target": str(b, "target")})
	if err != nil {
		return nil, err
	}
	kind := func(rel string) string { p := strings.Split(rel, "/"); return p[len(p)-2] }
	oldK, newK := kind(str(o, "target")), kind(t)
	proxyKinds := map[string]bool{"targetHttpProxies": true, "targetHttpsProxies": true}
	if oldK != newK && !(proxyKinds[oldK] && proxyKinds[newK]) {
		return nil, invalid("Invalid value for field 'targetReference.target': '%s'. The new target must be of the same type (%s).", str(b, "target"), oldK)
	}
	return s.lbSet(x, o, "setTarget", func(o obj) error { o["target"] = t; return nil })
}

// ruleIPVersionLocked sets a rule's ipVersion (IPV4 or IPV6): given, or
// IPV6 when its IPAddress is an IPv6 address or a reserved global IPv6
// one (vpc/docs/reserve-static-external-ip-address). A global IPv6 rule
// is on a global IPv6 address (ipv6.go); a regional one, external or
// internal, on a /96 of a dual-stack or IPv6-only subnet's range
// (ruleIPv6Locked). Their traffic arrives at their listeners as every
// rule's does (lbplane.go); VMs reach an internal one through their
// relay6 (vpcservices.go).
func (s *Server) ruleIPVersionLocked(x lbCtx, b, o obj, internal bool) error {
	v := str(b, "ipVersion")
	if v == "" {
		ref := strings.TrimSuffix(str(b, "IPAddress"), "/96")
		if ip := net.ParseIP(ref); ip != nil && ip.To4() == nil {
			v = "IPV6"
		} else if ref != "" && ip == nil {
			rel := Rel(ref, x.project)
			if !strings.Contains(rel, "/") {
				rel = "projects/" + x.project + "/" + x.scope + "/addresses/" + ref
			}
			if str(s.res[rel], "ipVersion") == "IPV6" {
				v = "IPV6"
			}
		}
	}
	switch v {
	case "":
		return nil
	case "IPV4":
	case "IPV6":
		// load-balancing/docs/forwarding-rule-concepts: "Only the backend
		// service-based regional external passthrough Network Load
		// Balancers can handle IPv6 traffic."
		if strings.Contains(str(o, "target"), "/targetPools/") {
			return invalid("Invalid value for field 'resource.ipVersion': 'IPV6'. A target pool-based external passthrough Network Load Balancer is IPv4 only; use a backend service.")
		}
	default:
		return invalid("Invalid value for field 'resource.ipVersion': '%s'. Must be one of [IPV4, IPV6].", v)
	}
	o["ipVersion"] = v
	return nil
}

// ruleIPv6Locked sets a regional IPv6 forwarding rule's IPAddress, a /96
// as Google gives it ("IPv6 address range, as in 2600:1234::/96",
// compute/docs/reference/rest/v1/forwardingRules): a reserved regional
// IPv6 address of the rule's type (by name, link or address), or an
// ephemeral /96 of its subnetwork, which an IPv6 rule must name
// ("required ... when creating an external forwarding rule with IPv6")
// and whose IPv6 access type must be the rule's: INTERNAL for an internal
// load balancer, EXTERNAL (Premium Tier) for an external one
// (load-balancing/docs/forwarding-rule-concepts).
func (s *Server) ruleIPv6Locked(x lbCtx, b, o obj, internal bool, ports []int) error {
	typ := "EXTERNAL"
	if internal {
		typ = "INTERNAL"
	} else if str(o, "networkTier") != "PREMIUM" {
		return invalid("Invalid value for field 'resource.networkTier': '%s'. External IPv6 forwarding rules are in the Premium Tier.", str(o, "networkTier"))
	}
	ref := strings.TrimSuffix(str(b, "IPAddress"), "/96")
	var addrRel string
	if ip := net.ParseIP(ref); ref != "" && ip == nil {
		addrRel = Rel(ref, x.project)
		if !strings.Contains(addrRel, "/") {
			addrRel = "projects/" + x.project + "/" + x.scope + "/addresses/" + ref
		}
		if _, ok := s.res[addrRel]; !ok {
			return notFound(addrRel)
		}
	} else if ip != nil {
		if ip.To4() != nil {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address's IP version (IPV4) is not the forwarding rule's ipVersion.", ref)
		}
		ref = ip.String()
		for k, a := range s.res {
			if strings.HasPrefix(k, "projects/"+x.project+"/"+x.scope+"/addresses/") && str(a, "address") == ref {
				addrRel = k
			}
		}
	}
	if addrRel != "" {
		a := s.res[addrRel]
		if scopeOf(addrRel) != x.scope {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address must be in the forwarding rule's scope (%s).", ref, x.scope)
		}
		if str(a, "ipVersion") != "IPV6" {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. The address's IP version (IPV4) is not the forwarding rule's ipVersion.", ref)
		}
		if str(a, "addressType") != typ {
			return invalid("Invalid value for field 'resource.IPAddress': '%s'. A forwarding rule of this scheme needs an %s address.", ref, typ)
		}
		for _, u := range strs(a, "users") {
			if other := s.res[u]; portsOverlap(other, ports) && str(other, "IPProtocol") == str(o, "IPProtocol") {
				return errf(http.StatusBadRequest, "invalid", "IP address '%s' and port are already being used by forwarding rule '%s'.", str(a, "address"), u)
			}
		}
		o["IPAddress"], o["_address"], o["subnetwork"] = str(a, "address")+"/96", addrRel, str(a, "subnetwork")
		return nil
	}
	sub := Rel(str(b, "subnetwork"), x.project)
	if sub == "" {
		return required("resource.subnetwork")
	}
	if !strings.Contains(sub, "/") {
		sub = subnetRel(x.project, x.region, sub)
	}
	so, ok := s.res[sub]
	if !ok {
		return notFound(sub)
	}
	if lastSeg(str(so, "region")) != x.region {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork must be in region '%s'.", sub, x.region)
	}
	if nw := str(o, "network"); nw != "" && str(so, "network") != nw {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork is not in network '%s'.", sub, lastSeg(nw))
	}
	if (str(so, "stackType") != "IPV4_IPV6" && str(so, "stackType") != "IPV6_ONLY") || str(so, "ipv6AccessType") != typ {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. An IPv6 forwarding rule of this scheme needs a dual-stack or IPv6-only subnetwork whose IPv6 access type is %s.", sub, typ)
	}
	_, base, err := net.ParseCIDR(str(so, "ipv6CidrRange"))
	if err != nil {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork has no IPv6 range.", sub)
	}
	addr, static, err := s.wantV6Locked(so, base, ref, "resource.IPAddress")
	if err != nil {
		return err
	}
	if static != "" {
		o["_address"] = static
	}
	o["IPAddress"], o["subnetwork"] = addr+"/96", sub
	if internal && str(o, "network") == "" {
		o["network"] = str(so, "network")
	}
	return nil
}
