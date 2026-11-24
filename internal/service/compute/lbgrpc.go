package compute

// Target gRPC proxies (#2160), as
// https://docs.cloud.google.com/service-mesh/docs/service-routing/proxyless-overview,
// .../traffic-director/docs/proxyless-overview and the targetGrpcProxies
// REST reference describe them (read 2026-11-22): a global proxy naming a
// URL map, which a global forwarding rule of the INTERNAL_SELF_MANAGED
// scheme (address 0.0.0.0, a port and a network) targets; proxyless gRPC
// clients (xds:///HOST:PORT) learn the URL map's routes and the backend
// services' endpoints from the xDS API (lbxds.go) and call the endpoints
// directly, so the rule itself listens on nothing. validateForProxyless
// refuses URL map features proxyless gRPC does not support.

func init() {
	registerLB(&lbColl{coll: "targetGrpcProxies", kind: "compute#targetGrpcProxy", global: true, patch: true, fingerprint: true,
		build: buildGrpcProxy, noun: "targetGrpcProxy"})
}

func buildGrpcProxy(s *Server, x lbCtx, b, old obj) (obj, error) {
	um, err := s.urlMapRef(x, "resource.urlMap", str(b, "urlMap"))
	if err != nil {
		return nil, err
	}
	o := obj{"urlMap": um, "validateForProxyless": b["validateForProxyless"] == true}
	if o["validateForProxyless"] == true {
		if err := proxylessURLMap(s.res[um]); err != nil {
			return nil, err
		}
	}
	for _, svc := range urlMapServices(s.res[um]) {
		bo := s.res[svc]
		if str(bo, "kind") == "compute#backendBucket" {
			return nil, invalid("Invalid value for field 'resource.urlMap': '%s'. A target gRPC proxy's URL map cannot use backend buckets.", um)
		}
		if str(bo, "loadBalancingScheme") != "INTERNAL_SELF_MANAGED" {
			return nil, invalid("Invalid value for field 'resource.urlMap': '%s'. Backend service '%s' must have the INTERNAL_SELF_MANAGED load balancing scheme to be used by a target gRPC proxy.", um, svc)
		}
		if str(bo, "protocol") != "GRPC" {
			return nil, invalid("Invalid value for field 'resource.urlMap': '%s'. Backend service '%s' must use the GRPC protocol to be used by a target gRPC proxy.", um, svc)
		}
	}
	return o, nil
}

// proxylessURLMap checks a URL map against what proxyless gRPC supports
// (proxyless-overview, "Limitations"): no URL rewrites, redirects, CORS,
// request mirroring, or custom error responses.
func proxylessURLMap(m obj) error {
	var bad string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				switch k {
				case "urlRewrite", "urlRedirect", "defaultUrlRedirect", "corsPolicy", "requestMirrorPolicy",
					"defaultCustomErrorResponsePolicy", "customErrorResponsePolicy":
					if bad == "" {
						bad = path + "." + k
					}
				}
				walk(e, path+"."+k)
			}
		case []any:
			for _, e := range t {
				walk(e, path)
			}
		}
	}
	walk(m, "resource")
	if bad != "" {
		return invalid("Invalid value for field '%s': ''. The URL map uses a feature proxyless gRPC does not support (validateForProxyless).", bad)
	}
	return nil
}
