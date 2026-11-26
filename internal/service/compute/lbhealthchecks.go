package compute

import "strconv"

// Health checks (#2153): healthChecks and regionHealthChecks of every type
// (https://docs.cloud.google.com/load-balancing/docs/health-check-concepts
// and the healthChecks REST reference, read 2026-11-22), and the legacy
// httpHealthChecks and httpsHealthChecks target pools use
// (load-balancing/docs/health-checks#legacy-health-checks). The data plane
// (lbhealth.go) probes each backend with them, from the documented probe
// ranges: a backend whose network's firewall does not allow them is
// unhealthy, as on Google.

// hcTypes are a health check's types and the field of each one's settings.
var hcTypes = map[string]string{"HTTP": "httpHealthCheck", "HTTPS": "httpsHealthCheck", "HTTP2": "http2HealthCheck",
	"TCP": "tcpHealthCheck", "SSL": "sslHealthCheck", "GRPC": "grpcHealthCheck", "GRPC_WITH_TLS": "grpcTlsHealthCheck"}

// hcDefaultPort is each type's port when none is given.
var hcDefaultPort = map[string]float64{"HTTP": 80, "HTTPS": 443, "HTTP2": 443, "TCP": 80, "SSL": 443, "GRPC": 443, "GRPC_WITH_TLS": 443}

func init() {
	registerLB(&lbColl{coll: "healthChecks", kind: "compute#healthCheck", global: true, regional: true, patch: true, update: true,
		testIam: true, build: buildHealthCheck, noun: "healthCheck"})
	registerLB(&lbColl{coll: "httpHealthChecks", kind: "compute#httpHealthCheck", global: true, patch: true, update: true,
		testIam: true, build: legacyHealthCheck(80), noun: "httpHealthCheck"})
	registerLB(&lbColl{coll: "httpsHealthChecks", kind: "compute#httpsHealthCheck", global: true, patch: true, update: true,
		testIam: true, build: legacyHealthCheck(443), noun: "httpsHealthCheck"})
}

// hcTiming checks and defaults a health check's interval, timeout and
// thresholds into o.
func hcTiming(b, o obj) error {
	interval := numOr(b, "checkIntervalSec", 5)
	timeout := numOr(b, "timeoutSec", 5)
	if interval < 1 || interval > 300 {
		return invalid("Invalid value for field 'resource.checkIntervalSec': '%v'. Must be greater than or equal to 1 and less than or equal to 300", interval)
	}
	if timeout < 1 || timeout > 300 {
		return invalid("Invalid value for field 'resource.timeoutSec': '%v'. Must be greater than or equal to 1 and less than or equal to 300", timeout)
	}
	if timeout > interval {
		return invalid("Invalid value for field 'resource.timeoutSec': '%v'. TimeoutSec should be less than checkIntervalSec.", timeout)
	}
	for _, k := range []string{"healthyThreshold", "unhealthyThreshold"} {
		v := numOr(b, k, 2)
		if v < 1 || v > 10 {
			return invalid("Invalid value for field 'resource.%s': '%v'. Must be greater than or equal to 1 and less than or equal to 10", k, v)
		}
		o[k] = v
	}
	o["checkIntervalSec"], o["timeoutSec"] = interval, timeout
	return nil
}

func buildHealthCheck(s *Server, x lbCtx, b, old obj) (obj, error) {
	o := obj{}
	if err := hcTiming(b, o); err != nil {
		return nil, err
	}
	typ := str(b, "type")
	if typ == "" {
		for t, f := range hcTypes {
			if b[f] != nil {
				typ = t
			}
		}
	}
	field, ok := hcTypes[typ]
	if !ok {
		return nil, invalid("Invalid value for field 'resource.type': '%s'. Must be one of [GRPC, GRPC_WITH_TLS, HTTP, HTTP2, HTTPS, SSL, TCP]", typ)
	}
	for t, f := range hcTypes {
		if t != typ && b[f] != nil {
			return nil, invalid("Invalid value for field 'resource.%s': ''. Health check of type %s cannot have %s.", f, typ, f)
		}
	}
	in := objOf(b[field])
	if in == nil {
		in = obj{}
	}
	sub := obj{}
	spec := str(in, "portSpecification")
	if spec == "" {
		switch {
		case in["port"] != nil:
			spec = "USE_FIXED_PORT"
		case str(in, "portName") != "":
			spec = "USE_NAMED_PORT"
		default:
			spec = "USE_FIXED_PORT"
		}
	}
	spec, err := oneOf("resource."+field+".portSpecification", spec, "", "USE_FIXED_PORT", "USE_NAMED_PORT", "USE_SERVING_PORT")
	if err != nil {
		return nil, err
	}
	if str(in, "portSpecification") != "" {
		// Google keeps portSpecification only when it is given: with port
		// or portName alone, the probe follows them (as the Terraform
		// provider, whose port_specification is not computed, expects).
		sub["portSpecification"] = spec
	}
	switch spec {
	case "USE_FIXED_PORT":
		port := numOr(in, "port", hcDefaultPort[typ])
		if port < 1 || port > 65535 {
			return nil, invalid("Invalid value for field 'resource.%s.port': '%v'. Must be greater than or equal to 1 and less than or equal to 65535", field, port)
		}
		sub["port"] = port
	case "USE_NAMED_PORT":
		if str(in, "portName") == "" {
			return nil, invalid("Invalid value for field 'resource.%s.portName': ''. A port name is required with USE_NAMED_PORT.", field)
		}
		if in["port"] != nil {
			return nil, invalid("Invalid value for field 'resource.%s.port': '%v'. A port cannot be given with USE_NAMED_PORT.", field, in["port"])
		}
	case "USE_SERVING_PORT":
		if in["port"] != nil || str(in, "portName") != "" {
			return nil, invalid("Invalid value for field 'resource.%s.portSpecification': 'USE_SERVING_PORT'. Neither port nor portName can be given with USE_SERVING_PORT.", field)
		}
	}
	if pn := str(in, "portName"); pn != "" {
		if !namedPortRE.MatchString(pn) {
			return nil, invalid("Invalid value for field 'resource.%s.portName': '%s'. Must be a match of regex '[a-z]([-a-z0-9]{0,61}[a-z0-9])?'", field, pn)
		}
		sub["portName"] = pn
	}
	switch typ {
	case "HTTP", "HTTPS", "HTTP2":
		rp := str(in, "requestPath")
		if rp == "" {
			rp = "/"
		}
		if rp[0] != '/' {
			return nil, invalid("Invalid value for field 'resource.%s.requestPath': '%s'. The request path must start with '/'.", field, rp)
		}
		sub["requestPath"] = rp
		copyFields(sub, in, "host", "response")
	case "TCP", "SSL":
		copyFields(sub, in, "request", "response")
	case "GRPC", "GRPC_WITH_TLS":
		copyFields(sub, in, "grpcServiceName")
	}
	if typ != "GRPC" && typ != "GRPC_WITH_TLS" {
		ph, err := oneOf("resource."+field+".proxyHeader", str(in, "proxyHeader"), "NONE", "NONE", "PROXY_V1")
		if err != nil {
			return nil, err
		}
		sub["proxyHeader"] = ph
	}
	o["type"], o[field] = typ, sub
	if lc := objOf(b["logConfig"]); lc != nil {
		o["logConfig"] = obj{"enable": lc["enable"] == true}
	}
	if sr := strs(b, "sourceRegions"); len(sr) > 0 {
		if x.region != "" {
			return nil, invalid("Invalid value for field 'resource.sourceRegions': '%v'. Source regions are set only on global health checks.", sr)
		}
		o["sourceRegions"] = toAny(sr)
	}
	return o, nil
}

// legacyHealthCheck builds an httpHealthCheck or httpsHealthCheck.
func legacyHealthCheck(defPort float64) func(s *Server, x lbCtx, b, old obj) (obj, error) {
	return func(s *Server, x lbCtx, b, old obj) (obj, error) {
		o := obj{}
		if err := hcTiming(b, o); err != nil {
			return nil, err
		}
		port := numOr(b, "port", defPort)
		if port < 1 || port > 65535 {
			return nil, invalid("Invalid value for field 'resource.port': '%v'. Must be greater than or equal to 1 and less than or equal to 65535", port)
		}
		rp := str(b, "requestPath")
		if rp == "" {
			rp = "/"
		}
		if rp[0] != '/' {
			return nil, invalid("Invalid value for field 'resource.requestPath': '%s'. The request path must start with '/'.", rp)
		}
		o["port"], o["requestPath"] = port, rp
		copyFields(o, b, "host")
		return o, nil
	}
}

// hcProbe is how a health check probes one endpoint.
type hcProbe struct {
	typ        string // HTTP, HTTPS, HTTP2, TCP, SSL, GRPC, GRPC_WITH_TLS
	port       int    // 0: the serving port
	portName   string // with USE_NAMED_PORT
	path, host string
	request    string
	response   string
	proxyV1    bool
	grpcName   string
	interval   int
	timeout    int
	healthy    int
	unhealthy  int
}

// probeOf is a health check record's probe.
func probeOf(o obj) hcProbe {
	p := hcProbe{interval: int(numOr(o, "checkIntervalSec", 5)), timeout: int(numOr(o, "timeoutSec", 5)),
		healthy: int(numOr(o, "healthyThreshold", 2)), unhealthy: int(numOr(o, "unhealthyThreshold", 2))}
	switch str(o, "kind") {
	case "compute#httpHealthCheck", "compute#httpsHealthCheck":
		p.typ = "HTTP"
		if str(o, "kind") == "compute#httpsHealthCheck" {
			p.typ = "HTTPS"
		}
		p.port, p.path, p.host = int(numOr(o, "port", 80)), str(o, "requestPath"), str(o, "host")
		return p
	}
	p.typ = str(o, "type")
	sub := objOf(o[hcTypes[p.typ]])
	spec := str(sub, "portSpecification")
	if spec == "" && sub["port"] == nil && str(sub, "portName") != "" {
		spec = "USE_NAMED_PORT"
	}
	switch spec {
	case "USE_NAMED_PORT":
		p.portName = str(sub, "portName")
	case "USE_SERVING_PORT":
	default:
		p.port = int(numOr(sub, "port", hcDefaultPort[p.typ]))
	}
	p.path, p.host, p.request, p.response = str(sub, "requestPath"), str(sub, "host"), str(sub, "request"), str(sub, "response")
	p.proxyV1 = str(sub, "proxyHeader") == "PROXY_V1"
	p.grpcName = str(sub, "grpcServiceName")
	return p
}

// key identifies a probe's settings, so a changed health check starts
// over.
func (p hcProbe) key() string {
	return p.typ + "|" + strconv.Itoa(p.port) + "|" + p.portName + "|" + p.path + "|" + p.host + "|" + p.request + "|" + p.response + "|" + p.grpcName
}
