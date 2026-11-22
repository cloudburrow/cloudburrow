package compute

import (
	"strconv"
	"strings"
)

// Target proxies, target pools and target instances (#2153), as the
// targetHttpProxies, targetHttpsProxies, targetTcpProxies,
// targetSslProxies, targetPools and targetInstances REST references and
// https://docs.cloud.google.com/load-balancing/docs/target-proxies,
// .../network/target-pools and .../protocol-forwarding describe them (read
// 2026-11-22). A forwarding rule's target: the HTTP(S) proxies apply a URL
// map; the TCP and SSL proxies send to one backend service; a target pool
// is a legacy passthrough Network Load Balancer's pool of instances; a
// target instance is one instance (protocol forwarding).

type lbPost = map[string]func(*Server, lbCtx, obj, obj) (obj, error)

func init() {
	registerLB(&lbColl{coll: "targetHttpProxies", kind: "compute#targetHttpProxy", global: true, regional: true, patch: true,
		fingerprint: true, build: buildHTTPProxy(false), noun: "targetHttpProxy",
		post: lbPost{"setUrlMap": setURLMap}})
	registerLB(&lbColl{coll: "targetHttpsProxies", kind: "compute#targetHttpsProxy", global: true, regional: true, patch: true,
		fingerprint: true, build: buildHTTPProxy(true), noun: "targetHttpsProxy",
		post: lbPost{"setUrlMap": setURLMap, "setSslCertificates": setSslCertificates, "setSslPolicy": setSslPolicy,
			"setQuicOverride": setQuicOverride, "setCertificateMap": noCertificateMap}})
	registerLB(&lbColl{coll: "targetTcpProxies", kind: "compute#targetTcpProxy", global: true, regional: true,
		testIam: true, build: buildStreamProxy(false), noun: "targetTcpProxy",
		post: lbPost{"setBackendService": setProxyService, "setProxyHeader": setProxyHeader}})
	registerLB(&lbColl{coll: "targetSslProxies", kind: "compute#targetSslProxy", global: true,
		testIam: true, build: buildStreamProxy(true), noun: "targetSslProxy",
		post: lbPost{"setBackendService": setProxyService, "setProxyHeader": setProxyHeader, "setSslCertificates": setSslCertificates,
			"setSslPolicy": setSslPolicy, "setCertificateMap": noCertificateMap}})
	registerLB(&lbColl{coll: "targetPools", kind: "compute#targetPool", regional: true, testIam: true, build: buildTargetPool,
		noun: "targetPool",
		post: lbPost{"addInstance": poolInstances(true), "removeInstance": poolInstances(false), "addHealthCheck": poolHealthCheck(true),
			"removeHealthCheck": poolHealthCheck(false), "setBackup": setBackup, "getHealth": targetPoolHealth,
			"setSecurityPolicy": noSecurityPolicy}})
	registerLB(&lbColl{coll: "targetInstances", kind: "compute#targetInstance", zonal: true, testIam: true, build: buildTargetInstance,
		noun: "targetInstance", post: lbPost{"setSecurityPolicy": noSecurityPolicy}})
}

func noCertificateMap(*Server, lbCtx, obj, obj) (obj, error) {
	return nil, unimplemented("Certificate Manager certificate maps are not served: attach SSL certificates (sslCertificates) instead (#2153)")
}

// lbSet changes one record in place and answers its operation.
func (s *Server) lbSet(x lbCtx, o obj, opType string, change func(o obj) error) (obj, error) {
	nw := deepCopy(o).(obj)
	if err := change(nw); err != nil {
		return nil, err
	}
	if _, ok := nw["fingerprint"]; ok {
		nw["fingerprint"] = newFingerprint()
	}
	s.res[x.rel] = norm(nw)
	s.lbChangedLocked()
	return s.done(x.project, x.scope, opType, x.rel, o["id"])
}

// urlMapRef is a proxy's URL map: of its own scope.
func (s *Server) urlMapRef(x lbCtx, field, ref string) (string, error) {
	rel, _, err := s.lbRef(x, field, ref, "urlMaps")
	if err != nil {
		return "", err
	}
	if scopeOf(rel) != x.scope {
		return "", invalid("Invalid value for field '%s': '%s'. The URL map must be in the proxy's scope (%s).", field, ref, x.scope)
	}
	return rel, nil
}

// certRefs are a proxy's SSL certificates: 1 to 15 of its scope (a
// global proxy's are global).
func (s *Server) certRefs(x lbCtx, field string, refs []string) ([]any, error) {
	if len(refs) == 0 {
		return nil, required(field)
	}
	if len(refs) > 15 {
		return nil, invalid("Invalid value for field '%s': ''. A target proxy can have at most 15 SSL certificates.", field)
	}
	var out []any
	for i, r := range refs {
		rel, _, err := s.lbRef(x, field+"["+itoa(i)+"]", r, "sslCertificates")
		if err != nil {
			return nil, err
		}
		if scopeOf(rel) != x.scope {
			return nil, invalid("Invalid value for field '%s[%d]': '%s'. The SSL certificate must be in the proxy's scope (%s).", field, i, r, x.scope)
		}
		out = append(out, rel)
	}
	return out, nil
}

func (s *Server) sslPolicyRef(x lbCtx, field, ref string) (string, error) {
	rel, _, err := s.lbRef(x, field, ref, "sslPolicies")
	if err != nil {
		return "", err
	}
	if scopeOf(rel) != x.scope {
		return "", invalid("Invalid value for field '%s': '%s'. The SSL policy must be in the proxy's scope (%s).", field, ref, x.scope)
	}
	return rel, nil
}

func buildHTTPProxy(tls bool) func(s *Server, x lbCtx, b, old obj) (obj, error) {
	return func(s *Server, x lbCtx, b, old obj) (obj, error) {
		um, err := s.urlMapRef(x, "resource.urlMap", str(b, "urlMap"))
		if err != nil {
			return nil, err
		}
		o := obj{"urlMap": um}
		if v, ok := num(b, "httpKeepAliveTimeoutSec"); ok {
			if v < 5 || v > 1200 {
				return nil, invalid("Invalid value for field 'resource.httpKeepAliveTimeoutSec': '%v'. Must be between 5 and 1200.", v)
			}
			o["httpKeepAliveTimeoutSec"] = v
		}
		copyFields(o, b, "proxyBind")
		if !tls {
			return o, nil
		}
		if str(b, "certificateMap") != "" {
			return nil, unimplemented("Certificate Manager certificate maps are not served: attach SSL certificates (sslCertificates) instead (#2153)")
		}
		if str(b, "serverTlsPolicy") != "" || str(b, "authorizationPolicy") != "" {
			return nil, unimplemented("mutual TLS (serverTlsPolicy) and authorization policies are not served (#2153)")
		}
		certs, err := s.certRefs(x, "resource.sslCertificates", strs(b, "sslCertificates"))
		if err != nil {
			return nil, err
		}
		o["sslCertificates"] = certs
		if sp := str(b, "sslPolicy"); sp != "" {
			if o["sslPolicy"], err = s.sslPolicyRef(x, "resource.sslPolicy", sp); err != nil {
				return nil, err
			}
		}
		q, err := oneOf("resource.quicOverride", str(b, "quicOverride"), "NONE", "NONE", "ENABLE", "DISABLE")
		if err != nil {
			return nil, err
		}
		if x.region == "" {
			o["quicOverride"] = q
		}
		if te := str(b, "tlsEarlyData"); te != "" {
			if o["tlsEarlyData"], err = oneOf("resource.tlsEarlyData", te, "", "DISABLED", "STRICT", "PERMISSIVE", "UNRESTRICTED"); err != nil {
				return nil, err
			}
		}
		return o, nil
	}
}

func setURLMap(s *Server, x lbCtx, o, b obj) (obj, error) {
	um, err := s.urlMapRef(x, "urlMapReference.urlMap", str(b, "urlMap"))
	if err != nil {
		return nil, err
	}
	if err := s.checkRuleSchemesLocked(x.rel, "", um); err != nil {
		return nil, err
	}
	return s.lbSet(x, o, "setUrlMap", func(o obj) error { o["urlMap"] = um; return nil })
}

func setSslCertificates(s *Server, x lbCtx, o, b obj) (obj, error) {
	certs, err := s.certRefs(x, "request.sslCertificates", strs(b, "sslCertificates"))
	if err != nil {
		return nil, err
	}
	return s.lbSet(x, o, "setSslCertificates", func(o obj) error { o["sslCertificates"] = certs; return nil })
}

func setSslPolicy(s *Server, x lbCtx, o, b obj) (obj, error) {
	ref := str(b, "sslPolicy")
	rel := ""
	if ref != "" {
		var err error
		if rel, err = s.sslPolicyRef(x, "sslPolicyReference.sslPolicy", ref); err != nil {
			return nil, err
		}
	}
	return s.lbSet(x, o, "setSslPolicy", func(o obj) error {
		if rel == "" {
			delete(o, "sslPolicy")
		} else {
			o["sslPolicy"] = rel
		}
		return nil
	})
}

func setQuicOverride(s *Server, x lbCtx, o, b obj) (obj, error) {
	q, err := oneOf("request.quicOverride", str(b, "quicOverride"), "NONE", "NONE", "ENABLE", "DISABLE")
	if err != nil {
		return nil, err
	}
	return s.lbSet(x, o, "setQuicOverride", func(o obj) error { o["quicOverride"] = q; return nil })
}

// streamService is a TCP or SSL proxy's backend service: of its scope and
// protocol.
func (s *Server) streamService(x lbCtx, field, ref string, tls bool) (string, error) {
	rel, bs, err := s.lbRef(x, field, ref, "backendServices")
	if err != nil {
		return "", err
	}
	if scopeOf(rel) != x.scope {
		return "", invalid("Invalid value for field '%s': '%s'. The backend service must be in the proxy's scope (%s).", field, ref, x.scope)
	}
	want := "TCP"
	if tls {
		want = "SSL"
	}
	if p := str(bs, "protocol"); p != want && !(tls && p == "TCP") {
		return "", invalid("Invalid value for field '%s': '%s'. The backend service of a target %s proxy must use protocol %s; it uses %s.", field, ref, strings.ToLower(want), want, p)
	}
	return rel, nil
}

func buildStreamProxy(tls bool) func(s *Server, x lbCtx, b, old obj) (obj, error) {
	return func(s *Server, x lbCtx, b, old obj) (obj, error) {
		svc, err := s.streamService(x, "resource.service", str(b, "service"), tls)
		if err != nil {
			return nil, err
		}
		o := obj{"service": svc}
		ph, err := oneOf("resource.proxyHeader", str(b, "proxyHeader"), "NONE", "NONE", "PROXY_V1")
		if err != nil {
			return nil, err
		}
		o["proxyHeader"] = ph
		copyFields(o, b, "proxyBind")
		if x.region != "" {
			o["loadBalancingScheme"] = str(b, "loadBalancingScheme")
			if o["loadBalancingScheme"] == "" {
				o["loadBalancingScheme"] = "INTERNAL_MANAGED"
			}
		}
		if !tls {
			return o, nil
		}
		if str(b, "certificateMap") != "" {
			return nil, unimplemented("Certificate Manager certificate maps are not served: attach SSL certificates (sslCertificates) instead (#2153)")
		}
		certs, err := s.certRefs(x, "resource.sslCertificates", strs(b, "sslCertificates"))
		if err != nil {
			return nil, err
		}
		o["sslCertificates"] = certs
		if sp := str(b, "sslPolicy"); sp != "" {
			if o["sslPolicy"], err = s.sslPolicyRef(x, "resource.sslPolicy", sp); err != nil {
				return nil, err
			}
		}
		return o, nil
	}
}

func setProxyService(s *Server, x lbCtx, o, b obj) (obj, error) {
	svc, err := s.streamService(x, "request.service", str(b, "service"), str(o, "kind") == "compute#targetSslProxy")
	if err != nil {
		return nil, err
	}
	return s.lbSet(x, o, "setBackendService", func(o obj) error { o["service"] = svc; return nil })
}

func setProxyHeader(s *Server, x lbCtx, o, b obj) (obj, error) {
	ph, err := oneOf("request.proxyHeader", str(b, "proxyHeader"), "NONE", "NONE", "PROXY_V1")
	if err != nil {
		return nil, err
	}
	return s.lbSet(x, o, "setProxyHeader", func(o obj) error { o["proxyHeader"] = ph; return nil })
}

// poolInstanceRef is an instance of a target pool's region.
func (s *Server) poolInstanceRef(x lbCtx, field, ref string) (string, error) {
	rel := Rel(ref, x.project)
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[2] != "zones" || p[4] != "instances" {
		return "", invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
	}
	if regionOfZone(p[3]) != x.region {
		return "", invalid("Invalid value for field '%s': '%s'. The instance must be in region '%s'.", field, ref, x.region)
	}
	if _, ok := s.res[rel]; !ok {
		return "", notFound(rel)
	}
	return rel, nil
}

func (s *Server) poolHealthRef(x lbCtx, field, ref string) (string, error) {
	rel, _, err := s.lbRef(lbCtx{project: x.project, scope: "global"}, field, ref, "httpHealthChecks", "httpsHealthChecks")
	return rel, err
}

func buildTargetPool(s *Server, x lbCtx, b, old obj) (obj, error) {
	o := obj{}
	var insts []any
	for i, r := range strs(b, "instances") {
		rel, err := s.poolInstanceRef(x, "resource.instances["+itoa(i)+"]", r)
		if err != nil {
			return nil, err
		}
		insts = append(insts, rel)
	}
	if len(insts) > 0 {
		o["instances"] = insts
	}
	hcs := strs(b, "healthChecks")
	if len(hcs) > 1 {
		return nil, invalid("Invalid value for field 'resource.healthChecks': ''. A target pool can have at most one health check.")
	}
	if len(hcs) == 1 {
		rel, err := s.poolHealthRef(x, "resource.healthChecks[0]", hcs[0])
		if err != nil {
			return nil, err
		}
		o["healthChecks"] = []any{rel}
	}
	aff, err := oneOf("resource.sessionAffinity", str(b, "sessionAffinity"), "NONE", "NONE", "CLIENT_IP", "CLIENT_IP_PROTO")
	if err != nil {
		return nil, err
	}
	o["sessionAffinity"] = aff
	if bp := str(b, "backupPool"); bp != "" {
		rel, _, err := s.lbRef(x, "resource.backupPool", bp, "targetPools")
		if err != nil {
			return nil, err
		}
		o["backupPool"] = rel
	}
	if v, ok := num(b, "failoverRatio"); ok {
		if v < 0 || v > 1 {
			return nil, invalid("Invalid value for field 'resource.failoverRatio': '%v'. Must be between 0 and 1.", v)
		}
		if o["backupPool"] == nil {
			return nil, invalid("Invalid value for field 'resource.failoverRatio': '%v'. A failover ratio needs a backup pool.", v)
		}
		o["failoverRatio"] = v
	}
	if str(b, "securityPolicy") != "" {
		return nil, unimplemented("Google Cloud Armor security policies are not served (#2153)")
	}
	return o, nil
}

func poolInstances(add bool) func(s *Server, x lbCtx, o, b obj) (obj, error) {
	return func(s *Server, x lbCtx, o, b obj) (obj, error) {
		var refs []string
		for i, e := range listOf(b["instances"]) {
			rel, err := s.poolInstanceRef(x, "request.instances["+itoa(i)+"].instance", str(objOf(e), "instance"))
			if err != nil {
				return nil, err
			}
			refs = append(refs, rel)
		}
		op := "removeInstance"
		if add {
			op = "addInstance"
		}
		return s.lbSet(x, o, op, func(o obj) error {
			have := strs(o, "instances")
			for _, r := range refs {
				i := indexOf(have, r)
				switch {
				case add && i < 0:
					have = append(have, r)
				case !add && i >= 0:
					have = append(have[:i], have[i+1:]...)
				case !add:
					return invalid("Invalid value for field 'request.instances': '%s'. The instance is not in the target pool.", r)
				}
			}
			if len(have) == 0 {
				delete(o, "instances")
			} else {
				o["instances"] = toAny(have)
			}
			return nil
		})
	}
}

func indexOf(l []string, v string) int {
	for i, e := range l {
		if e == v {
			return i
		}
	}
	return -1
}

func poolHealthCheck(add bool) func(s *Server, x lbCtx, o, b obj) (obj, error) {
	return func(s *Server, x lbCtx, o, b obj) (obj, error) {
		var refs []string
		for i, e := range listOf(b["healthChecks"]) {
			rel, err := s.poolHealthRef(x, "request.healthChecks["+itoa(i)+"].healthCheck", str(objOf(e), "healthCheck"))
			if err != nil {
				return nil, err
			}
			refs = append(refs, rel)
		}
		op := "removeHealthCheck"
		if add {
			op = "addHealthCheck"
		}
		return s.lbSet(x, o, op, func(o obj) error {
			have := strs(o, "healthChecks")
			for _, r := range refs {
				i := indexOf(have, r)
				switch {
				case add && i < 0:
					have = append(have, r)
				case !add && i >= 0:
					have = append(have[:i], have[i+1:]...)
				}
			}
			if len(have) > 1 {
				return invalid("Invalid value for field 'request.healthChecks': ''. A target pool can have at most one health check.")
			}
			if len(have) == 0 {
				delete(o, "healthChecks")
			} else {
				o["healthChecks"] = toAny(have)
			}
			return nil
		})
	}
}

func setBackup(s *Server, x lbCtx, o, b obj) (obj, error) {
	ref := str(b, "target")
	rel := ""
	if ref != "" {
		var err error
		if rel, _, err = s.lbRef(x, "targetReference.target", ref, "targetPools"); err != nil {
			return nil, err
		}
		if rel == x.rel {
			return nil, invalid("Invalid value for field 'targetReference.target': '%s'. A target pool cannot be its own backup.", ref)
		}
	}
	return s.lbSet(x, o, "setBackup", func(o obj) error {
		if rel == "" {
			delete(o, "backupPool")
			delete(o, "failoverRatio")
			return nil
		}
		o["backupPool"] = rel
		if v := str(b, "_failoverRatio"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f > 1 {
				return invalid("Invalid value for field 'failoverRatio': '%s'. Must be between 0 and 1.", v)
			}
			o["failoverRatio"] = f
		}
		return nil
	})
}

func targetPoolHealth(s *Server, x lbCtx, o, b obj) (obj, error) {
	rel, err := s.poolInstanceRef(x, "instanceReference.instance", str(b, "instance"))
	if err != nil {
		return nil, err
	}
	if indexOf(strs(o, "instances"), rel) < 0 {
		return nil, invalid("Invalid value for field 'instanceReference.instance': '%s'. The instance is not in the target pool.", str(b, "instance"))
	}
	return obj{"kind": "compute#targetPoolInstanceHealth", "healthStatus": []any{s.poolHealthLocked(x.rel, o, rel)}}, nil
}

func buildTargetInstance(s *Server, x lbCtx, b, old obj) (obj, error) {
	ref := str(b, "instance")
	if ref == "" {
		return nil, required("resource.instance")
	}
	rel := Rel(ref, x.project)
	if !strings.Contains(rel, "/") {
		rel = zoneRel(x.project, x.zone, "instances", rel)
	}
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[4] != "instances" {
		return nil, invalid("Invalid value for field 'resource.instance': '%s'. The URL is malformed.", ref)
	}
	if p[3] != x.zone {
		return nil, invalid("Invalid value for field 'resource.instance': '%s'. The instance must be in zone '%s'.", ref, x.zone)
	}
	if _, ok := s.res[rel]; !ok {
		return nil, notFound(rel)
	}
	np, err := oneOf("resource.natPolicy", str(b, "natPolicy"), "NO_NAT", "NO_NAT")
	if err != nil {
		return nil, err
	}
	o := obj{"instance": rel, "natPolicy": np}
	if nw := str(b, "network"); nw != "" {
		n, _ := NetworkName(nw, x.project)
		if _, ok := s.res[n]; !ok {
			return nil, notFound(n)
		}
		o["network"] = n
	}
	if str(b, "securityPolicy") != "" {
		return nil, unimplemented("Google Cloud Armor security policies are not served (#2153)")
	}
	return o, nil
}
