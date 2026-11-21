package kmsinventory

import (
	"net/http"
	"regexp"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Inventory API's JSON form, at Google's own paths (the
// google.api.http bindings of google.cloud.kms.inventory.v1): served on
// Cloud KMS's port, beside cloudkms's, through the shared transcoder, so
// REST calls the same methods gRPC does.

// paths are the Inventory API's paths, which no cloudkms binding shares.
var paths = regexp.MustCompile(`^/v1/(projects/[^/]+/cryptoKeys|.+/protectedResourcesSummary|(projects|organizations)/[^/]+/protectedResources:search)$`)

// Owns reports whether r is an Inventory API request.
func Owns(r *http.Request) bool { return paths.MatchString(r.URL.Path) }

// Routes are the bound routes.
func Routes() []rest.Route { return transcoder().Routes() }

func transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.kms.inventory.v1"}}
}

// NewRESTHandler is the JSON API for s.
func NewRESTHandler(s *Server) http.Handler {
	t := transcoder()
	s.Register(t)
	return t
}
