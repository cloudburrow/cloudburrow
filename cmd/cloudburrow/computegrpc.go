package main

import (
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
)

// The gRPC surface of the APIs on the Compute Engine port (#2160): Google
// serves Network Security, Network Services, Certificate Manager,
// Identity-Aware Proxy and Serverless VPC Access over gRPC as well as
// JSON, so their Go clients' default (gRPC) transport works here too,
// over HTTP/2 without TLS (h2c) or over this port's TLS. Each API keeps a
// gRPC server of its own, since each registers google.longrunning.Operations
// and google.cloud.location.Locations; a call to one of those goes to the
// API whose operation (or location parent) its request names, from the
// x-goog-request-params header Google's clients send with every call.
// Everything else (the xDS API of proxyless gRPC) goes to next.

// computeGRPCAPI is one API's gRPC server and the operations it owns.
type computeGRPCAPI struct {
	pkg   string // the proto package, e.g. google.cloud.networksecurity.v1
	g     *grpc.Server
	owns  func(op string) bool
	isLoc bool // answers google.cloud.location.Locations
}

type computeGRPC struct {
	apis []computeGRPCAPI
	next http.Handler
}

// newComputeGRPC routes the APIs' calls, and the rest to next; the last
// API with owns takes the operations no other owns.
func newComputeGRPC(next http.Handler, apis ...computeGRPCAPI) *computeGRPC {
	return &computeGRPC{apis: apis, next: next}
}

// computeGRPCServer is an API's server with its services registered.
func computeGRPCServer(register func(grpc.ServiceRegistrar)) *grpc.Server {
	g := grpchttp.NewServer()
	register(g)
	return g
}

// requestName is the name= (or parent=) of a call's routing header.
func requestName(r *http.Request) string {
	for _, v := range r.Header.Values("x-goog-request-params") {
		for _, kv := range strings.Split(v, "&") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || k != "name" && k != "parent" {
				continue
			}
			if u, err := url.QueryUnescape(val); err == nil {
				return u
			}
			return val
		}
	}
	return ""
}

func (c *computeGRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	for _, a := range c.apis {
		if strings.HasPrefix(path, "/"+a.pkg+".") {
			a.g.ServeHTTP(w, r)
			return
		}
	}
	switch {
	case strings.HasPrefix(path, "/google.longrunning.Operations/"):
		name := requestName(r)
		for _, a := range c.apis {
			if a.owns != nil && a.owns(name) {
				a.g.ServeHTTP(w, r)
				return
			}
		}

	case strings.HasPrefix(path, "/google.cloud.location.Locations/"):
		for _, a := range c.apis {
			if a.isLoc {
				a.g.ServeHTTP(w, r)
				return
			}
		}
	}
	c.next.ServeHTTP(w, r)
}

// Stop stops every API's server.
func (c *computeGRPC) Stop() {
	for _, a := range c.apis {
		a.g.Stop()
	}
}
