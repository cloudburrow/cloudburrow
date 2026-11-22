package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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
			serveSettingsJSON(rec.admin, t, w, r)
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
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/Settings
//
// It also carries defaultSinkConfig (#2108, defaults.go), the JSON API's
// Settings field the proto lacks: UpdateSettings takes it when the
// updateMask names default_sink_config (or is empty and the body has it),
// and both methods answer it.
func serveSettingsJSON(a *Admin, next http.Handler, w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v2/")
	var sinkConfig *SinkConfig
	setSinkConfig := false
	fwd := r
	if a != nil && r.Method == http.MethodPatch {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			rest.WriteError(w, apierror.InvalidArgument("reading the request: %v", err))
			return
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		raw, has := m["defaultSinkConfig"]
		var paths, rest0 []string
		named := false
		for _, mask := range r.URL.Query()["updateMask"] {
			for _, p := range strings.Split(mask, ",") {
				switch p = strings.TrimSpace(p); p {
				case "":
				case "default_sink_config", "defaultSinkConfig":
					named = true
					paths = append(paths, p)
				default:
					paths = append(paths, p)
					rest0 = append(rest0, p)
				}
			}
		}
		setSinkConfig = named || (len(paths) == 0 && has)
		if len(paths) == 0 && has {
			// An empty mask with a defaultSinkConfig: the other fields
			// the body sends, as the mask.
			named = true
			for _, f := range []string{"kmsKeyName", "storageLocation", "disableDefaultSink"} {
				if _, ok := m[f]; ok {
					rest0 = append(rest0, f)
				}
			}
		}
		if setSinkConfig && has && string(raw) != "null" {
			sinkConfig = &SinkConfig{}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(sinkConfig); err != nil {
				rest.WriteError(w, apierror.InvalidArgument("defaultSinkConfig: %v", err))
				return
			}
			if err := a.CheckSinkConfig(sinkConfig); err != nil {
				rest.WriteError(w, err)
				return
			}
		}
		if has {
			delete(m, "defaultSinkConfig")
			if b, err := json.Marshal(m); err == nil {
				body = b
			}
		}
		q := r.URL.Query()
		q.Del("updateMask")
		if len(rest0) > 0 {
			q.Set("updateMask", strings.Join(rest0, ","))
		}
		if named && len(rest0) == 0 {
			// Only the defaultSinkConfig changes: the rest is read.
			fwd = r.Clone(r.Context())
			fwd.Method, fwd.Body, fwd.ContentLength = http.MethodGet, http.NoBody, 0
		} else {
			fwd = r.Clone(r.Context())
			fwd.Body, fwd.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		}
		fwd.URL.RawQuery = q.Encode()
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, fwd)
	body := rec.Body.Bytes()
	if rec.Code == http.StatusOK && a != nil && setSinkConfig {
		if err := a.SetSinkConfig(name, sinkConfig); err != nil {
			rest.WriteError(w, err)
			return
		}
	}
	if rec.Code == http.StatusOK {
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) == nil {
			changed := false
			if sa, ok := m["kmsServiceAccountId"]; ok {
				if _, set := m["loggingServiceAccountId"]; !set {
					m["loggingServiceAccountId"] = sa
					changed = true
				}
			}
			if a != nil {
				if c, err := a.SinkConfigOf(name); err == nil && c != nil {
					if raw, err := json.Marshal(c); err == nil {
						m["defaultSinkConfig"] = raw
						changed = true
					}
				}
			}
			if changed {
				if b, err := json.Marshal(m); err == nil {
					body = b
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
