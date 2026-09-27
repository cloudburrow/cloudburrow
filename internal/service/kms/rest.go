package kms

import (
	"net/http"

	"google.golang.org/genproto/googleapis/api/annotations"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud KMS JSON API shares the gRPC port, as cloudkms.googleapis.com
// does (#414). Its routes are Google's own: the google.api.http bindings of
// every google.cloud.kms.v1 service, read from the generated descriptors,
// plus the mixins cloudkms_v1.yaml binds (Locations, IAMPolicy, Operations).
// The shared transcoder (#591) calls the same Server methods gRPC does, so
// REST has no logic of its own; a bound path the Server does not implement
// answers 501 UNIMPLEMENTED, and a path Google does not bind answers 404.

// Route is one bound HTTP method and path.
type Route = rest.Route

// maxBodyBytes bounds a JSON request: 64KiB of plaintext and of AAD, base64
// encoded, with room for the rest.
const maxBodyBytes = 1 << 20

// mixins are the bindings cloudkms_v1.yaml adds (googleapis
// cloudkms_v1.yaml@5b03e5ec:71-107), which no kmspb descriptor carries.
var mixins = func() []*annotations.HttpRule {
	get := func(sel, path string) *annotations.HttpRule {
		return &annotations.HttpRule{Selector: sel, Pattern: &annotations.HttpRule_Get{Get: path}}
	}
	post := func(sel, path string) *annotations.HttpRule {
		return &annotations.HttpRule{Selector: sel, Pattern: &annotations.HttpRule_Post{Post: path}, Body: "*"}
	}
	rs := []*annotations.HttpRule{
		get("google.cloud.location.Locations.GetLocation", "/v1/{name=projects/*/locations/*}"),
		get("google.cloud.location.Locations.ListLocations", "/v1/{name=projects/*}/locations"),
		get("google.longrunning.Operations.GetOperation", "/v1/{name=projects/*/locations/*/operations/*}"),
		// Not bound by Google's protos, but gcloud's GA `kms keyrings delete`
		// calls it; 501 rather than 404, which would say no such method
		// exists. Whether Google serves it is UNVERIFIED.
		{Selector: "google.cloud.kms.v1.KeyManagementService.DeleteKeyRing",
			Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/keyRings/*}"}},
	}
	for _, res := range []string{"projects/*/locations/*/keyRings/*", "projects/*/locations/*/keyRings/*/cryptoKeys/*",
		"projects/*/locations/*/keyRings/*/importJobs/*", "projects/*/locations/*/ekmConfig", "projects/*/locations/*/ekmConnections/*"} {
		rs = append(rs,
			// GET only: cloudkms_v1.yaml binds getIamPolicy to GET.
			get("google.iam.v1.IAMPolicy.GetIamPolicy", "/v1/{resource="+res+"}:getIamPolicy"),
			post("google.iam.v1.IAMPolicy.SetIamPolicy", "/v1/{resource="+res+"}:setIamPolicy"),
			post("google.iam.v1.IAMPolicy.TestIamPermissions", "/v1/{resource="+res+"}:testIamPermissions"),
		)
	}
	return rs
}()

// transcoder is the KMS JSON API's configuration. The path's name wins over
// one in the body for Encrypt, Decrypt and the IAMPolicy mixin; the methods
// below refuse a body that names another resource instead. What Google does
// then is UNVERIFIED.
func transcoder() *rest.Transcoder {
	return &rest.Transcoder{
		Packages:     []string{"google.cloud.kms.v1"},
		Mixins:       mixins,
		MaxBodyBytes: maxBodyBytes,
		RefuseBodyConflicts: []string{
			"google.cloud.kms.v1.KeyManagementService.UpdateCryptoKey",
			"google.cloud.kms.v1.KeyManagementService.UpdateCryptoKeyVersion",
			"google.cloud.kms.v1.KeyManagementService.UpdateCryptoKeyPrimaryVersion",
			"google.cloud.kms.v1.KeyManagementService.DestroyCryptoKeyVersion",
			"google.cloud.kms.v1.KeyManagementService.RestoreCryptoKeyVersion",
		},
	}
}

// Routes returns every bound route: the proto bindings, then the mixins.
func Routes() []Route { return transcoder().Routes() }

// NewRESTHandler returns the JSON API for s: the same services Register
// adds to gRPC, transcoded.
func NewRESTHandler(s *Server) http.Handler {
	t := transcoder()
	s.Register(t)
	return t
}
