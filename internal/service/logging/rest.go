package logging

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Logging JSON API shares the gRPC port (#591), as
// logging.googleapis.com serves both. Its routes are Google's own: the
// google.api.http bindings of every google.logging.v2 service, read from the
// generated descriptors. The shared transcoder calls the same Server gRPC
// does, so REST has no logic of its own. LoggingServiceV2, MetricsServiceV2
// and ConfigServiceV2 are registered (#1226); the ConfigServiceV2 methods
// Admin does not implement (the Async bucket calls, links, settings) answer 501
// UNIMPLEMENTED. The transcoder serves unary calls; the tail, a
// bidirectional stream, is served over gRPC (#1215) and, as a streamed JSON
// array, at its JSON binding by tailjson.go (#1410). A path Google does not bind answers 404.
// Of logging_v2.yaml's mixins, Operations' bindings are, for CopyLogEntries'
// operations: projects' (#1394), and organizations', folders' and billing
// accounts' (#1411). IAMPolicy's are, on log views (#1519). Locations is not
// bound.

// Transcoder returns the Cloud Logging JSON API's configuration, with nothing
// registered: register the same server with it as with gRPC.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.logging.v2"},
		Mixins: append(append([]*annotations.HttpRule(nil), operationsMixins...), viewIAMMixins...)}
}

// Routes returns every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for what register adds, which is called
// with the transcoder as it is with the gRPC server, so both transports serve
// the same set.
//
// POST /v2/entries:tail is served beside the transcoder (tailjson.go), and
// sink requests carry interceptChildren around it (intercept.go); the
// settings answers are completed by serveSettingsJSON.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	rec := &tailRecorder{next: t}
	register(rec)
	if rec.srv == nil && rec.admin == nil {
		return t
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec.srv != nil && r.Method == http.MethodPost && r.URL.Path == tailJSONPath {
			rec.srv.serveTailJSON(w, r)
			return
		}
		if settingsPathRe.MatchString(r.URL.Path) && (r.Method == http.MethodGet || r.Method == http.MethodPatch) {
			serveSettingsJSON(t, w, r)
			return
		}
		if serveSinkJSON(rec.admin, t, w, r) { // interceptChildren, intercept.go
			return
		}
		t.ServeHTTP(w, r)
	})
}

// settingsPathRe is GetSettings' and UpdateSettings' binding,
// /v2/{name=*/*}/settings.
var settingsPathRe = regexp.MustCompile(`^/v2/(projects|organizations|folders|billingAccounts)/[^/]+/settings$`)

// serveSettingsJSON adds loggingServiceAccountId to a Settings answer
// (#1304). Google's JSON API publishes the field (the discovery document's
// Settings: "the service account for the given resource container ... Log
// sinks use this service account as their writer_identity if no custom
// service account is provided"), and Terraform's
// google_logging_project_settings reads it, but the google.logging.v2
// proto the server is built on has no such field, so gRPC cannot carry it.
// It is the container's Log Router account, kmsServiceAccountId's
// (UNVERIFIED: that Google names the same account in both).
func serveSettingsJSON(next http.Handler, w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	if rec.Code == http.StatusOK {
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) == nil {
			if sa, ok := m["kmsServiceAccountId"]; ok {
				if _, set := m["loggingServiceAccountId"]; !set {
					m["loggingServiceAccountId"] = sa
					if b, err := json.Marshal(m); err == nil {
						body = b
					}
				}
			}
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(rec.Code)
	_, _ = bytes.NewReader(body).WriteTo(w)
}

// operationsMixins are logging_v2.yaml's Operations bindings, one set per
// parent collection.
var operationsMixins = func() []*annotations.HttpRule {
	var out []*annotations.HttpRule
	for _, coll := range []string{"projects", "organizations", "folders", "billingAccounts"} {
		out = append(out,
			&annotations.HttpRule{Selector: "google.longrunning.Operations.GetOperation",
				Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=" + coll + "/*/locations/*/operations/*}"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.ListOperations",
				Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=" + coll + "/*/locations/*}/operations"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.CancelOperation",
				Pattern: &annotations.HttpRule_Post{Post: "/v2/{name=" + coll + "/*/locations/*/operations/*}:cancel"}, Body: "*"})
	}
	return out
}()
