package alloydb

// The AlloyDB Omni engine, opt-in (#2207).
//
// Google publishes AlloyDB Omni, AlloyDB's own PostgreSQL-compatible
// engine (the columnar engine, google_ml_integration, alloydb_scann, the
// query plan cache and the index advisor), as the google/alloydbomni image
// (docs.cloud.google.com/alloydb/omni/containers/current/docs/quickstart,
// read 2026-11-24). Its use is governed by Google's AlloyDB Omni licence
// terms (OmniTermsURL), which only the end user may accept. So CloudBurrow
// never runs it by default: a cluster's instances are PostgreSQL servers
// (docs/alloydb.md), unless the user accepted the terms, either for every
// cluster `up` serves (`cloudburrow up --accept-alloydb-omni-terms`,
// CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y, or alloydb.acceptOmniTerms in a
// configuration file the user named) or for one cluster (the console's
// Create cluster checkbox, which sends OmniTermsHeader). Who accepted, when
// and how is recorded with the cluster and shown by `cloudburrow status`;
// the engine runs the Omni image only for a cluster with that record.
//
// What "an explicit Omni request" is. Google's AlloyDB API has no engine
// field: every Google cluster runs AlloyDB's engine, so nothing in a
// request Google documents asks for Omni rather than PostgreSQL. The
// database flags of AlloyDB's features (google_columnar_engine.enabled and
// the rest) are not taken as one: clusters and Terraform configurations
// made before #2207 carry them, and they keep being accepted and stored on
// PostgreSQL, as before. The explicit request is CloudBurrow's own request
// header, EngineHeader: OMNI (gRPC metadata x-cloudburrow-alloydb-engine),
// on CreateCluster or RestoreCluster; and OmniTermsHeader: Y, which both
// accepts the terms and asks for Omni. Without an acceptance an OMNI
// request is FAILED_PRECONDITION, saying how to accept; EngineHeader:
// POSTGRES keeps a cluster on PostgreSQL even when `up` accepted the terms.
// A secondary cluster runs its primary's engine, under its acceptance, as
// its servers replicate the primary's.
//
// Google publishes Omni of PostgreSQL 15, 16 and 17 on Docker Hub (15.18.0,
// 16.14.0 and 17.10.0, linux/amd64 and linux/arm64, arm64 in preview); its
// PostgreSQL 18 images are behind a registration form, and there is none
// of 14. A POSTGRES_14 cluster therefore runs PostgreSQL 14 even with the
// terms accepted, and an explicit Omni request for one is INVALID_ARGUMENT.

import (
	"context"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// OmniTermsURL is Google's AlloyDB Omni licence terms, as the image's
// Docker Hub page links them ("AlloyDB Omni License Terms").
const OmniTermsURL = "https://cloud.google.com/terms/alloydb-omni-marketplace-terms"

// OmniDocsURL is Google's guide to running AlloyDB Omni in a container.
const OmniDocsURL = "https://docs.cloud.google.com/alloydb/omni/containers/current/docs/quickstart"

// The request headers of the opt-in, CloudBurrow's own: Google's API has no
// such fields, as Google holds AlloyDB's licence there. Over gRPC they are
// the same names in lower case, as metadata.
const (
	// OmniTermsHeader: Y accepts the AlloyDB Omni terms, as the caller, for
	// the one cluster the request creates, which then runs Omni. The
	// console's Create cluster checkbox sends it.
	OmniTermsHeader = "X-CloudBurrow-Accept-AlloyDB-Omni-Terms"
	// EngineHeader names the engine a new cluster runs: OMNI or POSTGRES.
	EngineHeader = "X-CloudBurrow-AlloyDB-Engine"
)

// The engines EngineHeader names.
const (
	EngineOmni     = "OMNI"
	EnginePostgres = "POSTGRES"
)

// OmniAcceptance is who accepted the AlloyDB Omni terms, when, and how:
// recorded with each cluster that runs Omni.
type OmniAcceptance struct {
	AcceptedBy string `json:"acceptedBy"`
	AcceptedAt string `json:"acceptedAt"`
	Via        string `json:"via"`
}

// Accepted is whether a names who accepted and when.
func (a *OmniAcceptance) Accepted() bool {
	return a != nil && a.AcceptedBy != "" && a.AcceptedAt != ""
}

// The ways the terms are accepted, as recorded.
const (
	OmniViaFlag    = "cloudburrow up --accept-alloydb-omni-terms"
	OmniViaEnv     = "CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y"
	OmniViaFile    = "alloydb.acceptOmniTerms in the configuration file"
	OmniViaConsole = "the console's Create cluster checkbox (request header " + OmniTermsHeader + ": Y)"
)

// omniVersions are the major versions Google publishes Omni images of.
var omniVersions = map[alloydbpb.DatabaseVersion]bool{
	alloydbpb.DatabaseVersion_POSTGRES_15: true,
	alloydbpb.DatabaseVersion_POSTGRES_16: true,
	alloydbpb.DatabaseVersion_POSTGRES_17: true,
}

// OmniServes is whether Omni runs clusters of version.
func OmniServes(version alloydbpb.DatabaseVersion) bool { return omniVersions[version] }

// AcceptOmniTerms records the acceptance of the AlloyDB Omni terms for
// every cluster this server creates from now on: `up
// --accept-alloydb-omni-terms`, its environment variable or the named
// configuration file's alloydb.acceptOmniTerms. nil (the default) accepts
// nothing.
func (s *Server) AcceptOmniTerms(a *OmniAcceptance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a == nil {
		s.omniTerms = nil
		return
	}
	cp := *a
	s.omniTerms = &cp
}

// OmniOf is the acceptance cluster runs Omni under, nil when it runs
// PostgreSQL.
func (s *Server) OmniOf(cluster string) *OmniAcceptance {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.omni[cluster]; a != nil {
		cp := *a
		return &cp
	}
	return nil
}

type headersKey struct{}

// WithRequestHeaders carries h's opt-in headers to the API's methods, as
// the JSON API does with each request's.
func WithRequestHeaders(ctx context.Context, h http.Header) context.Context {
	m := map[string]string{}
	for _, k := range []string{OmniTermsHeader, EngineHeader} {
		if v := h.Get(k); v != "" {
			m[k] = v
		}
	}
	if len(m) == 0 {
		return ctx
	}
	return context.WithValue(ctx, headersKey{}, m)
}

// WithOmniTermsAccepted is ctx carrying OmniTermsHeader: Y, the console's
// checkbox.
func WithOmniTermsAccepted(ctx context.Context) context.Context {
	return WithRequestHeaders(ctx, http.Header{http.CanonicalHeaderKey(OmniTermsHeader): {"Y"}})
}

// requestHeader is the value of header name the request carried, in its
// JSON request or its gRPC metadata.
func requestHeader(ctx context.Context, name string) string {
	if m, ok := ctx.Value(headersKey{}).(map[string]string); ok {
		if v := m[name]; v != "" {
			return strings.TrimSpace(v)
		}
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(strings.ToLower(name)); len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
	}
	return ""
}

// restHeaders passes the JSON API's opt-in headers to the methods.
func restHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithRequestHeaders(r.Context(), r.Header)))
	})
}

// omniRefusal is the FAILED_PRECONDITION of an explicit Omni request with
// no acceptance of the terms. UNVERIFIED (re-read 2026-11-24,
// docs.cloud.google.com/alloydb/docs/cluster-create): Google documents no
// such refusal, as Google holds AlloyDB's licence; the code is the one this
// API answers for a missing precondition of the local server, and the
// wording is CloudBurrow's.
func omniRefusal() error {
	return apierror.FailedPrecondition("this cluster asks for the AlloyDB Omni engine (" + EngineHeader + ": " + EngineOmni +
		"), which runs Google's google/alloydbomni image, whose use is governed by Google's AlloyDB Omni licence terms (" +
		OmniTermsURL + "); CloudBurrow never accepts them on your behalf. To accept them yourself, start CloudBurrow with " +
		"`cloudburrow up --accept-alloydb-omni-terms` (or CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y, or alloydb.acceptOmniTerms " +
		"in a configuration file you name with --config), send " + OmniTermsHeader + ": Y with this request, or check " +
		"\"I accept the AlloyDB Omni licence terms\" on the console's Create cluster page; who accepted and when is recorded " +
		"with the cluster. Without " + EngineHeader + ", the cluster runs PostgreSQL")
}

// omniFor is the acceptance a new cluster of version runs Omni under: nil
// for PostgreSQL; or the request's refusal. The caller holds mu.
func (s *Server) omniFor(ctx context.Context, version alloydbpb.DatabaseVersion) (*OmniAcceptance, error) {
	accept := requestHeader(ctx, OmniTermsHeader)
	engine := strings.ToUpper(requestHeader(ctx, EngineHeader))
	if accept != "" && accept != "Y" {
		return nil, apierror.InvalidArgument("%s must be Y, which accepts the AlloyDB Omni licence terms (%s) for this cluster", OmniTermsHeader, OmniTermsURL)
	}
	switch engine {
	case "", EngineOmni:
	case EnginePostgres:
		if accept == "Y" {
			return nil, apierror.InvalidArgument("%s: Y asks for AlloyDB Omni, and %s: %s for PostgreSQL; send one", OmniTermsHeader, EngineHeader, EnginePostgres)
		}
		return nil, nil
	default:
		return nil, apierror.InvalidArgument("%s must be %s or %s", EngineHeader, EngineOmni, EnginePostgres)
	}
	explicit := engine == EngineOmni || accept == "Y"
	if !OmniServes(version) {
		if explicit {
			return nil, apierror.InvalidArgument("AlloyDB Omni is published for POSTGRES_15, POSTGRES_16 and POSTGRES_17, not %s; "+
				"choose one of those, or leave out %s and %s for PostgreSQL", version, EngineHeader, OmniTermsHeader)
		}
		return nil, nil
	}
	if accept == "Y" {
		return &OmniAcceptance{AcceptedBy: localUser(), AcceptedAt: s.now().UTC().Format(time.RFC3339), Via: OmniViaConsole}, nil
	}
	a := s.omniTerms
	if a == nil {
		if explicit {
			return nil, omniRefusal()
		}
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

// localUser is who runs this process, the person an acceptance by request
// header is recorded as made by: the request comes from this machine.
func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// recordOmni records that cluster runs Omni under a, or PostgreSQL when a
// is nil; the caller holds mu.
func (s *Server) recordOmni(cluster string, a *OmniAcceptance) {
	if a == nil {
		delete(s.omni, cluster)
		return
	}
	cp := *a
	s.omni[cluster] = &cp
}

// omniSpec is the acceptance a server of cluster runs Omni under, for its
// ServerSpec; the caller holds mu.
func (s *Server) omniSpec(cluster string) *OmniAcceptance {
	if a := s.omni[cluster]; a.Accepted() {
		cp := *a
		return &cp
	}
	return nil
}
