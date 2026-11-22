package compute

import (
	"regexp"
	"sort"
	"strings"
)

// Backend services and backend buckets (#2153), as
// https://docs.cloud.google.com/load-balancing/docs/backend-service and
// the backendServices, regionBackendServices and backendBuckets REST
// references describe them (read 2026-11-22): a backend service's
// backends are instance groups (zonal or regional, managed or not) or
// network endpoint groups (zonal, serverless, internet, Private Service
// Connect), each with its balancing mode and capacity, health checked by
// one health check; a backend bucket serves a Cloud Storage bucket.

var bsSchemes = []string{"EXTERNAL", "EXTERNAL_MANAGED", "INTERNAL", "INTERNAL_MANAGED", "INTERNAL_SELF_MANAGED"}

var bsProtocols = []string{"HTTP", "HTTPS", "HTTP2", "TCP", "SSL", "UDP", "GRPC", "H2C", "UNSPECIFIED"}

var bsAffinities = []string{"NONE", "CLIENT_IP", "CLIENT_IP_PROTO", "CLIENT_IP_PORT_PROTO", "CLIENT_IP_NO_DESTINATION",
	"GENERATED_COOKIE", "HEADER_FIELD", "HTTP_COOKIE", "STRONG_COOKIE_AFFINITY"}

var bsLocality = []string{"ROUND_ROBIN", "LEAST_REQUEST", "RING_HASH", "RANDOM", "ORIGINAL_DESTINATION", "MAGLEV",
	"WEIGHTED_MAGLEV", "WEIGHTED_ROUND_ROBIN", "WEIGHTED_GCP_RENDEZVOUS"}

func init() {
	registerLB(&lbColl{coll: "backendServices", kind: "compute#backendService", global: true, regional: true, patch: true, update: true,
		fingerprint: true, iam: true, testIam: true, build: buildBackendService, decorate: withUsedBy, noun: "backendService",
		post: map[string]func(*Server, lbCtx, obj, obj) (obj, error){"getHealth": backendServiceHealth,
			"setSecurityPolicy":     setSecurityPolicyOn("securityPolicy", "CLOUD_ARMOR"),
			"setEdgeSecurityPolicy": setSecurityPolicyOn("edgeSecurityPolicy", "CLOUD_ARMOR_EDGE"),
			"addSignedUrlKey":       addSignedURLKey, "deleteSignedUrlKey": deleteSignedURLKey},
		get:     map[string]func(*Server, lbCtx, obj, call) (obj, error){"getEffectiveSecurityPolicies": effectiveSecurityPolicies},
		collGet: map[string]func(*Server, lbCtx, call) (obj, error){"listUsable": listUsable("backendServices", "compute#usableBackendServiceList")}})
	registerLB(&lbColl{coll: "backendBuckets", kind: "compute#backendBucket", global: true, patch: true, update: true,
		iam: true, testIam: true, build: buildBackendBucket, decorate: withUsedBy, noun: "backendBucket",
		post: map[string]func(*Server, lbCtx, obj, obj) (obj, error){
			"setEdgeSecurityPolicy": setSecurityPolicyOn("edgeSecurityPolicy", "CLOUD_ARMOR_EDGE"),
			"addSignedUrlKey":       addSignedURLKey, "deleteSignedUrlKey": deleteSignedURLKey},
		collGet: map[string]func(*Server, lbCtx, call) (obj, error){"listUsable": listUsable("backendBuckets", "compute#usableBackendBucketList")}})
}

// listUsable is listUsable: the collection's records, as list returns them.
func listUsable(coll, kind string) func(*Server, lbCtx, call) (obj, error) {
	return func(s *Server, x lbCtx, c call) (obj, error) {
		out, err := s.listOut(kind, x.collRel(), s.res, c.query)
		if err != nil {
			return nil, err
		}
		items := out["items"].([]obj)
		for i, it := range items {
			items[i] = withUsedBy(s, deepCopy(it).(obj))
		}
		return out, nil
	}
}

// withUsedBy adds usedBy: the URL maps, proxies and forwarding rules that
// name the record.
func withUsedBy(s *Server, o obj) obj {
	var refs []any
	for _, u := range s.lbUsersLocked(str(o, "selfLink")) {
		refs = append(refs, map[string]any{"reference": u})
	}
	if len(refs) > 0 {
		o["usedBy"] = refs
	}
	return o
}

// groupKind is what a backend's group link names: "ig" (an instance
// group), "zonalNEG", "serverless", "psc", "internet" or "hybrid".
func groupKind(rel string, g obj) string {
	if strings.Contains(rel, "/instanceGroups/") {
		return "ig"
	}
	switch str(g, "networkEndpointType") {
	case "SERVERLESS":
		return "serverless"
	case "PRIVATE_SERVICE_CONNECT":
		return "psc"
	case "INTERNET_FQDN_PORT", "INTERNET_IP_PORT":
		return "internet"
	case "NON_GCP_PRIVATE_IP_PORT":
		return "hybrid"
	}
	return "zonalNEG"
}

func isHTTPProtocol(p string) bool {
	return p == "HTTP" || p == "HTTPS" || p == "HTTP2" || p == "GRPC" || p == "H2C"
}

func buildBackendService(s *Server, x lbCtx, b, old obj) (obj, error) {
	o := obj{}
	scheme, err := oneOf("resource.loadBalancingScheme", str(b, "loadBalancingScheme"), "EXTERNAL", bsSchemes...)
	if err != nil {
		return nil, err
	}
	if x.region == "" && scheme == "INTERNAL" {
		return nil, invalid("Invalid value for field 'resource.loadBalancingScheme': 'INTERNAL'. A global backend service cannot have the INTERNAL load balancing scheme.")
	}
	if x.region != "" && scheme == "INTERNAL_SELF_MANAGED" {
		return nil, invalid("Invalid value for field 'resource.loadBalancingScheme': 'INTERNAL_SELF_MANAGED'. A regional backend service cannot have the INTERNAL_SELF_MANAGED load balancing scheme.")
	}
	passthrough := x.region != "" && (scheme == "INTERNAL" || scheme == "EXTERNAL")
	defProto := "HTTP"
	if passthrough {
		defProto = "TCP"
	}
	proto, err := oneOf("resource.protocol", str(b, "protocol"), defProto, bsProtocols...)
	if err != nil {
		return nil, err
	}
	if passthrough && proto != "TCP" && proto != "UDP" && proto != "UNSPECIFIED" {
		return nil, invalid("Invalid value for field 'resource.protocol': '%s'. A backend service of a passthrough Network Load Balancer (%s) must be TCP, UDP or UNSPECIFIED.", proto, scheme)
	}
	if !passthrough && (proto == "UDP" || proto == "UNSPECIFIED") {
		return nil, invalid("Invalid value for field 'resource.protocol': '%s'. Protocol %s is supported only by passthrough Network Load Balancers.", proto, proto)
	}
	o["loadBalancingScheme"], o["protocol"] = scheme, proto
	aff, err := oneOf("resource.sessionAffinity", str(b, "sessionAffinity"), "NONE", bsAffinities...)
	if err != nil {
		return nil, err
	}
	o["sessionAffinity"] = aff
	if aff == "GENERATED_COOKIE" && !isHTTPProtocol(proto) {
		return nil, invalid("Invalid value for field 'resource.sessionAffinity': 'GENERATED_COOKIE'. GENERATED_COOKIE requires the HTTP, HTTPS or HTTP/2 protocol.")
	}
	loc := str(b, "localityLbPolicy")
	if loc != "" {
		if loc, err = oneOf("resource.localityLbPolicy", loc, "", bsLocality...); err != nil {
			return nil, err
		}
		if scheme == "EXTERNAL" && !passthrough {
			return nil, invalid("Invalid value for field 'resource.localityLbPolicy': '%s'. localityLbPolicy is not supported by the classic Application Load Balancer (load balancing scheme EXTERNAL).", loc)
		}
		o["localityLbPolicy"] = loc
	}
	if (aff == "HEADER_FIELD" || aff == "HTTP_COOKIE") && loc != "RING_HASH" && loc != "MAGLEV" {
		return nil, invalid("Invalid value for field 'resource.sessionAffinity': '%s'. Session affinity %s requires localityLbPolicy RING_HASH or MAGLEV.", aff, aff)
	}
	if ch := objOf(b["consistentHash"]); ch != nil {
		if loc != "RING_HASH" && loc != "MAGLEV" {
			return nil, invalid("Invalid value for field 'resource.consistentHash': ''. consistentHash requires localityLbPolicy RING_HASH or MAGLEV.")
		}
		if aff == "HEADER_FIELD" && str(ch, "httpHeaderName") == "" {
			return nil, invalid("Invalid value for field 'resource.consistentHash.httpHeaderName': ''. HEADER_FIELD session affinity needs a header name.")
		}
		o["consistentHash"] = deepCopy(ch)
	} else if aff == "HEADER_FIELD" {
		return nil, invalid("Invalid value for field 'resource.consistentHash.httpHeaderName': ''. HEADER_FIELD session affinity needs a header name.")
	}
	if v, ok := num(b, "affinityCookieTtlSec"); ok {
		if v < 0 || v > 1209600 {
			return nil, invalid("Invalid value for field 'resource.affinityCookieTtlSec': '%v'. Must be between 0 and 1209600.", v)
		}
		o["affinityCookieTtlSec"] = v
	}
	if !passthrough {
		o["timeoutSec"] = numOr(b, "timeoutSec", 30)
		pn := str(b, "portName")
		if pn == "" {
			pn = "http"
		}
		o["portName"], o["port"] = pn, numOr(b, "port", 80)
	} else if v, ok := num(b, "timeoutSec"); ok {
		o["timeoutSec"] = v
	}
	cd := objOf(b["connectionDraining"])
	o["connectionDraining"] = obj{"drainingTimeoutSec": numOr(cd, "drainingTimeoutSec", 0)}
	if err := s.buildPolicyFields(x, b, o, true); err != nil {
		return nil, err
	}
	iap, err := buildIAP(b, old, scheme)
	if err != nil {
		return nil, err
	}
	if iap != nil {
		o["iap"] = iap
	}
	if b["enableCDN"] == true && (passthrough || scheme == "INTERNAL_MANAGED" || scheme == "INTERNAL_SELF_MANAGED" || !isHTTPProtocol(proto)) {
		return nil, invalid("Invalid value for field 'resource.enableCDN': 'true'. Cloud CDN is supported only by external Application Load Balancers.")
	}
	if b["enableCDN"] == true && iap != nil && iap["enabled"] == true {
		return nil, invalid("Invalid value for field 'resource.iap.enabled': 'true'. Identity-Aware Proxy and Cloud CDN cannot both be enabled on a backend service.")
	}
	cdn, err := buildCdnPolicy(objOf(b["cdnPolicy"]), b["enableCDN"] == true, false, old)
	if err != nil {
		return nil, err
	}
	if cdn != nil {
		o["cdnPolicy"] = cdn
	}
	o["enableCDN"] = b["enableCDN"] == true
	if old != nil {
		if k := old["_signedUrlKeys"]; k != nil {
			o["_signedUrlKeys"] = deepCopy(k)
		}
	}
	copyFields(o, b, "customRequestHeaders", "customResponseHeaders", "logConfig", "outlierDetection",
		"circuitBreakers", "compressionMode", "failoverPolicy", "connectionTrackingPolicy", "maxStreamDuration",
		"securitySettings", "subsetting", "ipAddressSelectionPolicy", "strongSessionAffinityCookie")
	for _, h := range append(strs(b, "customRequestHeaders"), strs(b, "customResponseHeaders")...) {
		if i := strings.Index(h, ":"); i <= 0 {
			return nil, invalid("Invalid value for field 'resource.customRequestHeaders': '%s'. A custom header is 'NAME:VALUE'.", h)
		}
	}
	// The network, for an internal passthrough backend service.
	if nw := str(b, "network"); nw != "" {
		rel, _ := NetworkName(nw, x.project)
		if _, ok := s.res[rel]; !ok {
			return nil, notFound(rel)
		}
		o["network"] = rel
	}
	// Health checks: at most one, of this scope (a global backend service
	// may use the legacy HTTP(S) health checks).
	var hcs []any
	for i, h := range strs(b, "healthChecks") {
		colls := []string{"healthChecks", "httpHealthChecks", "httpsHealthChecks"}
		rel, _, err := s.lbRef(x, "resource.healthChecks["+itoa(i)+"]", h, colls...)
		if err != nil {
			return nil, err
		}
		hscope := scopeOf(rel)
		if hscope != "global" && hscope != x.scope {
			return nil, invalid("Invalid value for field 'resource.healthChecks[%d]': '%s'. The health check must be global or in region '%s'.", i, h, x.region)
		}
		if x.region != "" && hscope == "global" && !passthrough {
			return nil, invalid("Invalid value for field 'resource.healthChecks[%d]': '%s'. A regional backend service of a proxy load balancer needs a regional health check.", i, h)
		}
		if strings.Contains(rel, "HealthChecks/") && strings.Contains(rel, "/http") && (scheme != "EXTERNAL" || x.region != "") {
			return nil, invalid("Invalid value for field 'resource.healthChecks[%d]': '%s'. Legacy health checks are supported only by the classic Application Load Balancer.", i, h)
		}
		hcs = append(hcs, rel)
	}
	if len(hcs) > 1 {
		return nil, invalid("Invalid value for field 'resource.healthChecks': '%v'. A backend service can have at most one health check.", hcs)
	}
	if len(hcs) > 0 {
		o["healthChecks"] = hcs
	}
	backends, err := s.checkBackends(x, b, proto, scheme, passthrough, len(hcs) > 0)
	if err != nil {
		return nil, err
	}
	if len(backends) > 0 {
		o["backends"] = backends
	}
	return o, nil
}

// checkBackends validates and defaults a backend service's backends.
func (s *Server) checkBackends(x lbCtx, b obj, proto, scheme string, passthrough, hasHC bool) ([]any, error) {
	var out []any
	seen := map[string]bool{}
	kinds := map[string]bool{}
	for i, e := range listOf(b["backends"]) {
		be := objOf(e)
		field := "resource.backends[" + itoa(i) + "]"
		rel, g, err := s.lbRef(x, field+".group", str(be, "group"), "instanceGroups", "networkEndpointGroups")
		if err != nil {
			return nil, err
		}
		if seen[rel] {
			return nil, invalid("Invalid value for field '%s.group': '%s'. Backend group is used twice in the backend service.", field, rel)
		}
		seen[rel] = true
		gk := groupKind(rel, g)
		kinds[gk] = true
		gscope := scopeOf(rel)
		if x.region != "" && regionOfScope(gscope) != x.region && gk != "internet" {
			return nil, invalid("Invalid value for field '%s.group': '%s'. The backend must be in region '%s'.", field, rel, x.region)
		}
		switch gk {
		case "serverless":
			if passthrough || (!isHTTPProtocol(proto)) {
				return nil, invalid("Invalid value for field '%s.group': '%s'. A serverless network endpoint group needs a backend service of protocol HTTP, HTTPS or HTTP2.", field, rel)
			}
			if hasHC {
				return nil, invalid("Invalid value for field 'resource.healthChecks': ''. A backend service with a serverless network endpoint group backend cannot have health checks.")
			}
		case "internet":
			if x.region == "" && gscope != "global" {
				return nil, invalid("Invalid value for field '%s.group': '%s'. A global backend service needs a global internet network endpoint group.", field, rel)
			}
			if hasHC && x.region == "" {
				return nil, invalid("Invalid value for field 'resource.healthChecks': ''. A global backend service with an internet network endpoint group backend cannot have health checks.")
			}
		case "psc":
			if hasHC {
				return nil, invalid("Invalid value for field 'resource.healthChecks': ''. A backend service with a Private Service Connect network endpoint group backend cannot have health checks.")
			}
		case "ig", "zonalNEG", "hybrid":
			if !hasHC {
				return nil, invalid("Invalid value for field 'resource.healthChecks': ''. A backend service with %s backends must have a health check.",
					map[string]string{"ig": "instance group", "zonalNEG": "zonal network endpoint group", "hybrid": "hybrid network endpoint group"}[gk])
			}
		}
		nb := obj{"group": rel}
		copyFields(nb, be, "description", "failover", "preference")
		capacity := numOr(be, "capacityScaler", 1)
		if capacity < 0 || capacity > 1 {
			return nil, invalid("Invalid value for field '%s.capacityScaler': '%v'. Must be between 0.0 and 1.0.", field, capacity)
		}
		if capacity != 0 && capacity < 0.1 {
			return nil, invalid("Invalid value for field '%s.capacityScaler': '%v'. Must be 0 or between 0.1 and 1.0.", field, capacity)
		}
		if gk != "serverless" && gk != "internet" && gk != "psc" {
			mode, err := checkBalancing(field, be, nb, gk, proto, passthrough)
			if err != nil {
				return nil, err
			}
			nb["balancingMode"] = mode
			nb["capacityScaler"] = capacity
		} else {
			if v, ok := be["capacityScaler"]; ok {
				nb["capacityScaler"] = v
			}
		}
		out = append(out, nb)
	}
	if kinds["serverless"] && len(kinds) > 1 {
		return nil, invalid("Invalid value for field 'resource.backends': ''. A backend service with a serverless network endpoint group cannot have other kinds of backends.")
	}
	if kinds["ig"] && (kinds["zonalNEG"] || kinds["hybrid"]) && scheme == "EXTERNAL" {
		return nil, invalid("Invalid value for field 'resource.backends': ''. Instance groups and network endpoint groups cannot be mixed in one backend service of the classic Application Load Balancer.")
	}
	return out, nil
}

// checkBalancing is a backend's balancing mode and its capacity fields.
func checkBalancing(field string, be, nb obj, gk, proto string, passthrough bool) (string, error) {
	def := "UTILIZATION"
	switch {
	case passthrough:
		def = "CONNECTION"
	case gk != "ig" && isHTTPProtocol(proto):
		def = "RATE"
	case gk != "ig":
		def = "CONNECTION"
	}
	mode, err := oneOf(field+".balancingMode", str(be, "balancingMode"), def, "UTILIZATION", "RATE", "CONNECTION", "CUSTOM_METRICS")
	if err != nil {
		return "", err
	}
	if mode == "CUSTOM_METRICS" {
		return "", unimplemented("the CUSTOM_METRICS balancing mode is not served: backends report no custom metrics here (#2153)")
	}
	switch {
	case passthrough && mode != "CONNECTION":
		return "", invalid("Invalid value for field '%s.balancingMode': '%s'. A passthrough Network Load Balancer's backends use the CONNECTION balancing mode.", field, mode)
	case mode == "CONNECTION" && isHTTPProtocol(proto):
		return "", invalid("Invalid value for field '%s.balancingMode': 'CONNECTION'. Balancing mode CONNECTION is not supported for protocol %s.", field, proto)
	case mode == "RATE" && !isHTTPProtocol(proto):
		return "", invalid("Invalid value for field '%s.balancingMode': 'RATE'. Balancing mode RATE is not supported for protocol %s.", field, proto)
	case mode == "UTILIZATION" && gk != "ig":
		return "", invalid("Invalid value for field '%s.balancingMode': 'UTILIZATION'. Balancing mode UTILIZATION is supported only by instance group backends.", field)
	}
	rateKeys := []string{"maxRate", "maxRatePerInstance", "maxRatePerEndpoint"}
	connKeys := []string{"maxConnections", "maxConnectionsPerInstance", "maxConnectionsPerEndpoint"}
	for _, k := range append(append([]string{"maxUtilization"}, rateKeys...), connKeys...) {
		if v, ok := num(be, k); ok {
			if v < 0 {
				return "", invalid("Invalid value for field '%s.%s': '%v'. Must be greater than or equal to 0", field, k, v)
			}
			nb[k] = v
		}
	}
	has := func(keys []string) bool {
		for _, k := range keys {
			if _, ok := nb[k]; ok {
				return true
			}
		}
		return false
	}
	switch mode {
	case "UTILIZATION":
		if v, ok := num(nb, "maxUtilization"); ok && v > 1 {
			return "", invalid("Invalid value for field '%s.maxUtilization': '%v'. Must be between 0.0 and 1.0.", field, v)
		}
		if _, ok := nb["maxUtilization"]; !ok {
			nb["maxUtilization"] = 0.8
		}
	case "RATE":
		if !has(rateKeys) {
			return "", invalid("Invalid value for field '%s': ''. Balancing mode RATE needs one of maxRate, maxRatePerInstance or maxRatePerEndpoint.", field)
		}
	case "CONNECTION":
		if !passthrough && !has(connKeys) {
			return "", invalid("Invalid value for field '%s': ''. Balancing mode CONNECTION needs one of maxConnections, maxConnectionsPerInstance or maxConnectionsPerEndpoint.", field)
		}
	}
	return mode, nil
}

// backendServiceHealth is getHealth: each endpoint of a backend group, as
// the backend service's health check last found it.
func backendServiceHealth(s *Server, x lbCtx, o, b obj) (obj, error) {
	grp := str(b, "group")
	if grp == "" {
		return nil, required("resourceGroupReference.group")
	}
	rel := Rel(grp, x.project)
	found := false
	for _, e := range listOf(o["backends"]) {
		if str(objOf(e), "group") == rel {
			found = true
		}
	}
	if !found {
		return nil, invalid("Invalid value for field 'resourceGroupReference.group': '%s'. The group is not a backend of '%s'.", grp, x.rel)
	}
	var out []any
	for _, h := range s.lbHealthLocked(x.rel, rel) {
		out = append(out, h)
	}
	res := obj{"kind": "compute#backendServiceGroupHealth"}
	if len(out) > 0 {
		res["healthStatus"] = out
	}
	return res, nil
}

func buildBackendBucket(s *Server, x lbCtx, b, old obj) (obj, error) {
	bucket := str(b, "bucketName")
	if bucket == "" {
		return nil, required("resource.bucketName")
	}
	if !bucketNameRE.MatchString(bucket) {
		return nil, invalid("Invalid value for field 'resource.bucketName': '%s'. Must be a valid Cloud Storage bucket name.", bucket)
	}
	o := obj{"bucketName": bucket}
	if err := s.buildPolicyFields(x, b, o, false); err != nil {
		return nil, err
	}
	cdn, err := buildCdnPolicy(objOf(b["cdnPolicy"]), b["enableCdn"] == true, true, old)
	if err != nil {
		return nil, err
	}
	if cdn != nil {
		o["cdnPolicy"] = cdn
	}
	if old != nil {
		if k := old["_signedUrlKeys"]; k != nil {
			o["_signedUrlKeys"] = deepCopy(k)
		}
	}
	copyFields(o, b, "enableCdn", "customResponseHeaders", "compressionMode", "loadBalancingScheme", "params")
	for _, h := range strs(b, "customResponseHeaders") {
		if i := strings.Index(h, ":"); i <= 0 {
			return nil, invalid("Invalid value for field 'resource.customResponseHeaders': '%s'. A custom header is 'NAME:VALUE'.", h)
		}
	}
	if o["enableCdn"] == nil {
		o["enableCdn"] = false
	}
	return o, nil
}

// backendsOf are a backend service's backend group links, sorted.
func backendsOf(bs obj) []string {
	var out []string
	for _, e := range listOf(bs["backends"]) {
		out = append(out, str(objOf(e), "group"))
	}
	sort.Strings(out)
	return out
}

// bucketNameRE is a Cloud Storage bucket name (storage/docs/buckets#naming).
var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// statusPrecondition is http.StatusPreconditionFailed's reason.
