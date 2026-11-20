package main

import (
	"bytes"
	"io"
	"net/http"
	"regexp"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
	"google.golang.org/grpc"
)

// The eighth round's checks (#1879): Compute Engine's field-level
// permissions, API Keys' lookupKey and the operations of API Keys and
// Service Usage.
type iamEnforced8 struct {
	compute      *computeService
	apiKeys      *apikeys.Server
	serviceUsage *serviceusage.Server
	// localAI serves Vertex AI, Document AI, Speech-to-Text and
	// Translation (and Vision and Natural Language, which name no
	// permission).
	localAI *vertexai.Server
}

func (s iamEnforced8) use(e *iameval.Enforcer) {
	if s.localAI != nil {
		svcs := []*iameval.Service{iameval.VertexAI, iameval.DocumentAI, iameval.Speech}
		var unary []grpc.UnaryServerInterceptor
		var wraps []func(http.Handler) http.Handler
		for _, svc := range svcs {
			unary = append(unary, e.Interceptor(svc))
			wraps = append(wraps, jsonEnforcer(e, svc, nil))
		}
		s.localAI.UseIAM(unary, []grpc.StreamServerInterceptor{e.StreamInterceptor(svcs...)}, func(next http.Handler) http.Handler {
			h := e.HTTPPaths(iameval.VertexAI, iameval.AIJSON,
				e.HTTPPaths(iameval.Speech, iameval.SpeechJSON,
					e.HTTPPaths(iameval.Translation, iameval.TranslationJSON, next)))
			for _, w := range wraps {
				h = w(h)
			}
			return h
		})
	}
	if s.compute != nil {
		prev := s.compute.wrapJSON
		s.compute.wrapJSON = func(next http.Handler) http.Handler {
			h := computeFieldChecks(e, next)
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
	if s.apiKeys != nil {
		prev := s.apiKeys.Wrap
		keys := s.apiKeys
		s.apiKeys.Wrap = func(next http.Handler) http.Handler {
			h := apiKeysLookups(e, keys, next)
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
	if s.serviceUsage != nil {
		prev := s.serviceUsage.Wrap
		su := s.serviceUsage
		s.serviceUsage.Wrap = func(next http.Handler) http.Handler {
			h := operationGet(e, iameval.ServiceUsage, serviceUsageOpPath, su.OperationProject, next)
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
}

// computeFieldChecks checks a Compute request's field-level permissions
// (iameval.ComputeFieldChecks) after its method's own.
func computeFieldChecks(e *iameval.Enforcer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		bearer := iameval.Bearer(r.Header.Get("Authorization"))
		for _, c := range iameval.ComputeFieldChecks(r.Method, r.URL.Path, body, publicComputeProject) {
			if c.ActAs {
				err = e.AuthorizeActAs(r.Context(), iameval.Compute, bearer, c.Project, c.Resource)
			} else {
				err = e.Authorize(r.Context(), iameval.Compute, bearer, c.Permission, c.Resource)
			}
			if err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// publicComputeProject is whether project is a public image project.
func publicComputeProject(project string) bool {
	return publicImageProject("/compute/v1/projects/" + project + "/")
}

var (
	apiKeysLookupPath  = regexp.MustCompile(`^/v2/keys:lookupKey$`)
	apiKeysOpPath      = regexp.MustCompile(`^/v2/(operations/[^/:]+)$`)
	serviceUsageOpPath = regexp.MustCompile(`^/v1/(operations/[^:]+)$`)
)

// apiKeysLookups checks keys.lookupKey, apikeys.keys.lookup on the key's
// project (iameval.APIKeysLookup), and operations.get.
func apiKeysLookups(e *iameval.Enforcer, keys *apikeys.Server, next http.Handler) http.Handler {
	ops := operationGet(e, iameval.APIKeys, apiKeysOpPath, keys.OperationProject, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || r.Method != http.MethodGet || !apiKeysLookupPath.MatchString(r.URL.Path) {
			ops.ServeHTTP(w, r)
			return
		}
		if project, ok := keys.KeyProject(r.URL.Query().Get("keyString")); ok {
			if err := e.Authorize(r.Context(), iameval.APIKeys, iameval.Bearer(r.Header.Get("Authorization")),
				iameval.APIKeysLookup, "projects/"+project); err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// operationGet checks a GET of an operation path matches, by
// serviceusage.operations.get on the project the operation was made in
// (projectOf; an operation not known passes to the handler's NOT_FOUND).
func operationGet(e *iameval.Enforcer, svc *iameval.Service, path *regexp.Regexp, projectOf func(string) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		g := path.FindStringSubmatch(r.URL.Path)
		if g == nil {
			next.ServeHTTP(w, r)
			return
		}
		if project := projectOf(g[1]); project != "" {
			if err := e.Authorize(r.Context(), svc, iameval.Bearer(r.Header.Get("Authorization")),
				iameval.ServiceUsageOperationsGet, "projects/"+project); err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
