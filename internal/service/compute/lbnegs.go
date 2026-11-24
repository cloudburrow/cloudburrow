package compute

import (
	"net"
	"strconv"
	"strings"
)

// Network endpoint groups (#2153), as https://docs.cloud.google.com/load-balancing/docs/negs,
// .../negs/serverless-neg-concepts, .../negs/internet-neg-concepts,
// .../negs/hybrid-neg-concepts, the Private Service Connect backends'
// pages and the networkEndpointGroups, regionNetworkEndpointGroups and
// globalNetworkEndpointGroups REST references describe them (read
// 2026-11-22):
//
//   - zonal: GCE_VM_IP_PORT (an instance of the zone and network, and a
//     port, the defaultPort when unset; the instance's primary internal IP
//     when no address is given), GCE_VM_IP, and the hybrid
//     NON_GCP_PRIVATE_IP_PORT (an address and port);
//   - regional: SERVERLESS (a Cloud Run service, or a Cloud Run functions
//     function, by name or URL mask), PRIVATE_SERVICE_CONNECT (a published
//     service's attachment, or a Google API), and the regional internet
//     NEGs;
//   - global: the internet NEGs (INTERNET_FQDN_PORT, INTERNET_IP_PORT), of
//     one endpoint each.

var negTypes = map[string][]string{
	"zones":   {"GCE_VM_IP_PORT", "GCE_VM_IP", "NON_GCP_PRIVATE_IP_PORT"},
	"regions": {"SERVERLESS", "PRIVATE_SERVICE_CONNECT", "INTERNET_FQDN_PORT", "INTERNET_IP_PORT"},
	"global":  {"INTERNET_FQDN_PORT", "INTERNET_IP_PORT"},
}

func init() {
	registerLB(&lbColl{coll: "networkEndpointGroups", kind: "compute#networkEndpointGroup", global: true, regional: true, zonal: true,
		testIam: true, build: buildNEG, decorate: decorateNEG, noun: "networkEndpointGroup",
		post: lbPost{"attachNetworkEndpoints": negEndpoints(true), "detachNetworkEndpoints": negEndpoints(false),
			"listNetworkEndpoints": listNetworkEndpoints}})
}

func negScope(x lbCtx) string {
	switch {
	case x.zone != "":
		return "zones"
	case x.region != "":
		return "regions"
	}
	return "global"
}

func buildNEG(s *Server, x lbCtx, b, old obj) (obj, error) {
	types := negTypes[negScope(x)]
	typ, err := oneOf("resource.networkEndpointType", str(b, "networkEndpointType"), types[0], types...)
	if err != nil {
		return nil, err
	}
	o := obj{"networkEndpointType": typ, "size": 0.0}
	if v, ok := num(b, "defaultPort"); ok {
		if v < 1 || v > 65535 {
			return nil, invalid("Invalid value for field 'resource.defaultPort': '%v'. Must be between 1 and 65535.", v)
		}
		o["defaultPort"] = v
	}
	copyFields(o, b, "annotations")
	network := func(need bool) error {
		nw := str(b, "network")
		if nw == "" {
			if !need {
				return nil
			}
			nw = "global/networks/default"
		}
		rel, _ := NetworkName(nw, x.project)
		if _, ok := s.res[rel]; !ok {
			return notFound(rel)
		}
		o["network"] = rel
		if sub := str(b, "subnetwork"); sub != "" {
			srel := Rel(sub, x.project)
			if !strings.Contains(srel, "/") {
				srel = subnetRel(x.project, x.region, srel)
			}
			so, ok := s.res[srel]
			if !ok {
				return notFound(srel)
			}
			if str(so, "network") != rel {
				return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork is not in network '%s'.", sub, lastSeg(rel))
			}
			if regionOfScope(scopeOf(srel)) != x.region {
				return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork must be in region '%s'.", sub, x.region)
			}
			o["subnetwork"] = srel
		}
		return nil
	}
	switch typ {
	case "GCE_VM_IP_PORT", "GCE_VM_IP", "NON_GCP_PRIVATE_IP_PORT":
		if err := network(true); err != nil {
			return nil, err
		}
	case "SERVERLESS":
		n := 0
		for _, k := range []string{"cloudRun", "cloudFunction", "appEngine", "serverlessDeployment"} {
			if b[k] != nil {
				n++
			}
		}
		if n != 1 {
			return nil, invalid("Invalid value for field 'resource': ''. A serverless network endpoint group needs exactly one of cloudRun, cloudFunction, appEngine or serverlessDeployment.")
		}
		if b["appEngine"] != nil {
			return nil, unimplemented("App Engine is not served, so a serverless network endpoint group cannot point at it (#2153)")
		}
		if b["serverlessDeployment"] != nil {
			return nil, unimplemented("API Gateway is not served, so a serverless network endpoint group cannot point at it (#2153)")
		}
		if cr := objOf(b["cloudRun"]); cr != nil {
			if str(cr, "service") == "" && str(cr, "urlMask") == "" {
				return nil, invalid("Invalid value for field 'resource.cloudRun': ''. One of service or urlMask must be set.")
			}
			o["cloudRun"] = deepCopy(cr)
		}
		if cf := objOf(b["cloudFunction"]); cf != nil {
			if str(cf, "function") == "" && str(cf, "urlMask") == "" {
				return nil, invalid("Invalid value for field 'resource.cloudFunction': ''. One of function or urlMask must be set.")
			}
			o["cloudFunction"] = deepCopy(cf)
		}
		if b["network"] != nil || b["defaultPort"] != nil {
			return nil, invalid("Invalid value for field 'resource.network': ''. A serverless network endpoint group has no network or default port.")
		}
	case "PRIVATE_SERVICE_CONNECT":
		t := str(b, "pscTargetService")
		if t == "" {
			return nil, required("resource.pscTargetService")
		}
		if err := network(false); err != nil {
			return nil, err
		}
		rel := Rel(t, x.project)
		if strings.Contains(rel, "/serviceAttachments/") {
			a, ok := s.res[rel]
			if !ok {
				return nil, notFound(rel)
			}
			if regionOfScope(scopeOf(rel)) != x.region {
				return nil, invalid("Invalid value for field 'resource.pscTargetService': '%s'. The service attachment must be in region '%s'.", t, x.region)
			}
			if o["network"] == nil {
				return nil, required("resource.network")
			}
			o["pscTargetService"] = rel
			o["pscData"] = obj{"pscConnectionId": strconvU(uint64(s.opts.Now().UnixNano()) & (1<<62 - 1)),
				"pscConnectionStatus": pscStatus(a, x.project)}
		} else if strings.HasSuffix(t, ".googleapis.com") {
			o["pscTargetService"] = t
		} else {
			return nil, invalid("Invalid value for field 'resource.pscTargetService': '%s'. Must be a service attachment or a Google API's host name.", t)
		}
	}
	if old != nil {
		o["_endpoints"] = old["_endpoints"]
	}
	return o, nil
}

// decorateNEG adds a group's size, and a Private Service Connect group's
// current connection status.
func decorateNEG(s *Server, o obj) obj {
	o["size"] = float64(len(listOf(o["_endpoints"])))
	if pd := objOf(o["pscData"]); pd != nil {
		a := s.res[str(o, "pscTargetService")]
		pd["pscConnectionStatus"] = pscStatus(a, strings.Split(str(o, "selfLink"), "/")[1])
	}
	return o
}

// negEndpoint checks one endpoint of an attach or detach call.
func (s *Server) negEndpoint(x lbCtx, g obj, field string, e obj) (obj, error) {
	typ := str(g, "networkEndpointType")
	out := obj{}
	port, hasPort := num(e, "port")
	if !hasPort {
		if dp, ok := num(g, "defaultPort"); ok {
			port, hasPort = dp, true
		}
	}
	if hasPort && (port < 1 || port > 65535) {
		return nil, invalid("Invalid value for field '%s.port': '%v'. Must be between 1 and 65535.", field, port)
	}
	switch typ {
	case "GCE_VM_IP_PORT", "GCE_VM_IP":
		inst := str(e, "instance")
		if inst == "" {
			return nil, required(field + ".instance")
		}
		rel := Rel(inst, x.project)
		if !strings.Contains(rel, "/") {
			rel = zoneRel(x.project, x.zone, "instances", inst)
		}
		vm, ok := s.res[rel]
		if !ok {
			return nil, notFound(rel)
		}
		if lastSeg(str(vm, "zone")) != x.zone {
			return nil, invalid("Invalid value for field '%s.instance': '%s'. The instance must be in zone '%s'.", field, inst, x.zone)
		}
		v, _ := vmNetOf(rel, vm)
		if v.network != str(g, "network") {
			return nil, invalid("Invalid value for field '%s.instance': '%s'. The instance is not in network '%s'.", field, inst, lastSeg(str(g, "network")))
		}
		ip := str(e, "ipAddress")
		if ip == "" {
			ip = v.ip
		}
		out["instance"], out["ipAddress"] = lastSeg(rel), ip
		if typ == "GCE_VM_IP_PORT" {
			if !hasPort {
				return nil, invalid("Invalid value for field '%s.port': ''. The endpoint needs a port, or the group a defaultPort.", field)
			}
			out["port"] = port
		}
	case "NON_GCP_PRIVATE_IP_PORT", "INTERNET_IP_PORT":
		ip := net.ParseIP(str(e, "ipAddress"))
		if ip == nil || ip.To4() == nil {
			return nil, invalid("Invalid value for field '%s.ipAddress': '%s'. Must be an IPv4 address.", field, str(e, "ipAddress"))
		}
		if typ == "INTERNET_IP_PORT" && (ip.IsPrivate() || ip.IsLoopback()) {
			return nil, invalid("Invalid value for field '%s.ipAddress': '%s'. An internet network endpoint must be a public address.", field, ip)
		}
		if !hasPort {
			return nil, invalid("Invalid value for field '%s.port': ''. The endpoint needs a port, or the group a defaultPort.", field)
		}
		out["ipAddress"], out["port"] = ip.String(), port
	case "INTERNET_FQDN_PORT":
		fq := strings.ToLower(str(e, "fqdn"))
		if !domainRE.MatchString(fq) {
			return nil, invalid("Invalid value for field '%s.fqdn': '%s'. Must be a fully qualified domain name.", field, str(e, "fqdn"))
		}
		out["fqdn"] = fq
		if hasPort {
			out["port"] = port
		} else {
			out["port"] = 443.0
		}
	default:
		return nil, invalid("Invalid value for field '%s': ''. A %s network endpoint group has no endpoints to attach.", field, typ)
	}
	return out, nil
}

func endpointKey(e obj) string {
	return str(e, "instance") + "|" + str(e, "ipAddress") + "|" + str(e, "fqdn") + "|" + strconv.Itoa(int(numOr(e, "port", 0)))
}

func negEndpoints(attach bool) func(s *Server, x lbCtx, o, b obj) (obj, error) {
	return func(s *Server, x lbCtx, o, b obj) (obj, error) {
		var eps []obj
		for i, e := range listOf(b["networkEndpoints"]) {
			ep, err := s.negEndpoint(x, o, "networkEndpoints["+strconv.Itoa(i)+"]", objOf(e))
			if err != nil {
				return nil, err
			}
			eps = append(eps, ep)
		}
		if len(eps) == 0 {
			return nil, required("networkEndpoints")
		}
		op := "detachNetworkEndpoints"
		if attach {
			op = "attachNetworkEndpoints"
		}
		return s.lbSet(x, o, op, func(g obj) error {
			have := listOf(g["_endpoints"])
			for _, ep := range eps {
				k, at := endpointKey(ep), -1
				for i, h := range have {
					if endpointKey(objOf(h)) == k {
						at = i
					}
				}
				switch {
				case attach && at < 0:
					have = append(have, ep)
				case !attach && at >= 0:
					have = append(have[:at], have[at+1:]...)
				case !attach:
					return invalid("Invalid value for field 'networkEndpoints': ''. The endpoint is not in the network endpoint group.")
				}
			}
			t := str(g, "networkEndpointType")
			if (t == "INTERNET_FQDN_PORT" || t == "INTERNET_IP_PORT") && len(have) > 1 {
				return invalid("Invalid value for field 'networkEndpoints': ''. An internet network endpoint group can have only one endpoint.")
			}
			g["_endpoints"] = have
			g["size"] = float64(len(have))
			return nil
		})
	}
}

// listNetworkEndpoints is listNetworkEndpoints: the endpoints, with their
// health when healthStatus is SHOW.
func listNetworkEndpoints(s *Server, x lbCtx, o, b obj) (obj, error) {
	show := str(b, "healthStatus") == "SHOW"
	var items []any
	for _, e := range listOf(o["_endpoints"]) {
		ep := objOf(e)
		it := obj{"networkEndpoint": deepCopy(ep)}
		if show {
			it["healths"] = s.endpointHealthsLocked(x.rel, ep)
		}
		items = append(items, it)
	}
	out := obj{"kind": "compute#networkEndpointGroupsListNetworkEndpoints"}
	if len(items) > 0 {
		out["items"] = items
	}
	return out, nil
}
