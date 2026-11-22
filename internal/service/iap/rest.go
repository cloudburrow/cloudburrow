package iap

import (
	"net/http"
	"strings"

	"cloud.google.com/go/iap/apiv1/iappb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves both services on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	iappb.RegisterIdentityAwareProxyAdminServiceServer(g, s)
	iappb.RegisterIdentityAwareProxyOAuthServiceServer(g, s)
}

// Transcoder is the JSON API's configuration, with nothing registered:
// iap.proto binds every method itself, the IAM ones at
// /v1/{resource=**}:getIamPolicy and the rest.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.iap.v1"}, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}

// Handles is whether path is one of this API's, which the Compute Engine
// port serves beside /compute/v1/: an IAP resource (iap_web, iap_tunnel)
// or the OAuth Admin API's brands, under /v1/projects/P/.
func Handles(path string) bool {
	if !strings.HasPrefix(path, "/v1/projects/") {
		return false
	}
	p := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if len(p) < 3 {
		return false
	}
	seg := strings.SplitN(p[2], ":", 2)[0]
	return seg == "iap_web" || seg == "iap_tunnel" || seg == "brands"
}
