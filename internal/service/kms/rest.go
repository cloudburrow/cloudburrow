package kms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The Cloud KMS JSON API shares the gRPC port, as cloudkms.googleapis.com
// does (#414). Its routes are Google's own: the google.api.http bindings of
// every google.cloud.kms.v1 service, read from the generated descriptors,
// plus the mixins cloudkms_v1.yaml binds (Locations, IAMPolicy, Operations).
// A bound path that is not transcoded yet answers 501 UNIMPLEMENTED; a path
// Google does not bind answers 404.

// Route is one bound HTTP method and path.
type Route struct {
	// RPC is the method it transcodes to, e.g. KeyManagementService/Encrypt,
	// or a mixin such as IAMPolicy/GetIamPolicy.
	RPC     string
	Method  string
	Pattern string
	re      *regexp.Regexp
}

// mixins are the bindings cloudkms_v1.yaml adds (googleapis
// cloudkms_v1.yaml@5b03e5ec:71-107), which no kmspb descriptor carries.
var mixins = func() []Route {
	rs := []Route{
		{RPC: "Locations/GetLocation", Method: http.MethodGet, Pattern: "/v1/{name=projects/*/locations/*}"},
		{RPC: "Locations/ListLocations", Method: http.MethodGet, Pattern: "/v1/{name=projects/*}/locations"},
		{RPC: "Operations/GetOperation", Method: http.MethodGet, Pattern: "/v1/{name=projects/*/locations/*/operations/*}"},
		// Not bound by Google's protos, but gcloud's GA `kms keyrings delete`
		// calls it; 501 rather than 404, which would say no such method
		// exists. Whether Google serves it is UNVERIFIED.
		{RPC: "KeyManagementService/DeleteKeyRing", Method: http.MethodDelete, Pattern: "/v1/{name=projects/*/locations/*/keyRings/*}"},
	}
	for _, res := range []string{"projects/*/locations/*/keyRings/*", "projects/*/locations/*/keyRings/*/cryptoKeys/*",
		"projects/*/locations/*/keyRings/*/importJobs/*", "projects/*/locations/*/ekmConfig", "projects/*/locations/*/ekmConnections/*"} {
		rs = append(rs,
			// GET only: cloudkms_v1.yaml binds getIamPolicy to GET.
			Route{RPC: "IAMPolicy/GetIamPolicy", Method: http.MethodGet, Pattern: "/v1/{resource=" + res + "}:getIamPolicy"},
			Route{RPC: "IAMPolicy/SetIamPolicy", Method: http.MethodPost, Pattern: "/v1/{resource=" + res + "}:setIamPolicy"},
			Route{RPC: "IAMPolicy/TestIamPermissions", Method: http.MethodPost, Pattern: "/v1/{resource=" + res + "}:testIamPermissions"},
		)
	}
	return rs
}()

// Routes returns every bound route: the proto bindings, then the mixins.
func Routes() []Route {
	var out []Route
	protoregistry.GlobalFiles.RangeFilesByPackage("google.cloud.kms.v1", func(f protoreflect.FileDescriptor) bool {
		for i := 0; i < f.Services().Len(); i++ {
			sd := f.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				md := sd.Methods().Get(j)
				rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule)
				if rule == nil {
					continue
				}
				name := string(sd.Name()) + "/" + string(md.Name())
				for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
					if m, p := httpOf(r); m != "" {
						out = append(out, Route{RPC: name, Method: m, Pattern: p})
					}
				}
			}
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].RPC+out[i].Method < out[j].RPC+out[j].Method })
	out = append(out, mixins...)
	for i := range out {
		out[i].re = templateRE(out[i].Pattern)
	}
	return out
}

func httpOf(r *annotations.HttpRule) (string, string) {
	switch p := r.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, p.Post
	case *annotations.HttpRule_Patch:
		return http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		return http.MethodDelete, p.Delete
	case *annotations.HttpRule_Put:
		return http.MethodPut, p.Put
	}
	return "", ""
}

// templateRE compiles an HTTP rule path template. `*` is one segment and
// `**` one or more; neither crosses the `:verb` that ends a path, since no
// KMS resource ID contains a colon.
func templateRE(t string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(t); {
		if t[i] != '{' {
			j := strings.IndexByte(t[i:], '{')
			if j < 0 {
				j = len(t) - i
			}
			b.WriteString(regexp.QuoteMeta(t[i : i+j]))
			i += j
			continue
		}
		end := strings.IndexByte(t[i:], '}') + i
		pattern := "*"
		if eq := strings.IndexByte(t[i:end], '='); eq >= 0 {
			pattern = t[i+eq+1 : end]
		}
		for k, seg := range strings.Split(pattern, "/") {
			if k > 0 {
				b.WriteString("/")
			}
			switch seg {
			case "*":
				b.WriteString("[^/:]+")
			case "**":
				b.WriteString("[^:]+")
			default:
				b.WriteString(regexp.QuoteMeta(seg))
			}
		}
		i = end + 1
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// RESTHandler serves the JSON API. handlers maps an RPC (Route.RPC) to its
// transcoding; a bound route without one answers UNIMPLEMENTED.
type RESTHandler struct {
	routes   []Route
	handlers map[string]http.HandlerFunc
}

// NewRESTHandler returns the JSON API for s. Each transcoding calls the
// same Server method gRPC does; REST has no logic of its own.
func NewRESTHandler(s *Server) *RESTHandler {
	h := &RESTHandler{routes: Routes(), handlers: map[string]http.HandlerFunc{}}
	// S:405 and S:417, body "*" (#415).
	h.handlers["KeyManagementService/Encrypt"] = transcode(func(r *http.Request, _ query) (proto.Message, error) {
		req := &kmspb.EncryptRequest{}
		if err := decodeBody(r, req); err != nil {
			return nil, err
		}
		req.Name = nameBeforeVerb(r)
		return s.Encrypt(r.Context(), req)
	})
	h.handlers["KeyManagementService/Decrypt"] = transcode(func(r *http.Request, _ query) (proto.Message, error) {
		req := &kmspb.DecryptRequest{}
		if err := decodeBody(r, req); err != nil {
			return nil, err
		}
		req.Name = nameBeforeVerb(r)
		return s.Decrypt(r.Context(), req)
	})

	// The reads (#422): S:55, S:63, S:71, S:99, S:109, S:118. The path gives
	// name or parent, which the same parsers gRPC uses then check.
	list := []string{"pageSize", "pageToken", "filter", "orderBy"}
	h.handlers["KeyManagementService/GetKeyRing"] = transcode(func(r *http.Request, _ query) (proto.Message, error) {
		return s.GetKeyRing(r.Context(), &kmspb.GetKeyRingRequest{Name: pathName(r)})
	})
	h.handlers["KeyManagementService/GetCryptoKey"] = transcode(func(r *http.Request, _ query) (proto.Message, error) {
		return s.GetCryptoKey(r.Context(), &kmspb.GetCryptoKeyRequest{Name: pathName(r)})
	})
	h.handlers["KeyManagementService/GetCryptoKeyVersion"] = transcode(func(r *http.Request, _ query) (proto.Message, error) {
		return s.GetCryptoKeyVersion(r.Context(), &kmspb.GetCryptoKeyVersionRequest{Name: pathName(r)})
	})
	h.handlers["KeyManagementService/ListKeyRings"] = transcodeWith(list, func(r *http.Request, q query) (proto.Message, error) {
		size, err := q.int32Field("pageSize")
		if err != nil {
			return nil, err
		}
		return s.ListKeyRings(r.Context(), &kmspb.ListKeyRingsRequest{Parent: pathParent(r, "keyRings"), PageSize: size,
			PageToken: q.fields["pageToken"], Filter: q.fields["filter"], OrderBy: q.fields["orderBy"]})
	})
	h.handlers["KeyManagementService/ListCryptoKeys"] = transcodeWith(append(list, "versionView"), func(r *http.Request, q query) (proto.Message, error) {
		size, err := q.int32Field("pageSize")
		if err != nil {
			return nil, err
		}
		view, err := q.viewField("versionView")
		if err != nil {
			return nil, err
		}
		return s.ListCryptoKeys(r.Context(), &kmspb.ListCryptoKeysRequest{Parent: pathParent(r, "cryptoKeys"), PageSize: size,
			PageToken: q.fields["pageToken"], Filter: q.fields["filter"], OrderBy: q.fields["orderBy"], VersionView: view})
	})
	h.handlers["KeyManagementService/ListCryptoKeyVersions"] = transcodeWith(append(list, "view"), func(r *http.Request, q query) (proto.Message, error) {
		size, err := q.int32Field("pageSize")
		if err != nil {
			return nil, err
		}
		view, err := q.viewField("view")
		if err != nil {
			return nil, err
		}
		return s.ListCryptoKeyVersions(r.Context(), &kmspb.ListCryptoKeyVersionsRequest{Parent: pathParent(r, "cryptoKeyVersions"), PageSize: size,
			PageToken: q.fields["pageToken"], Filter: q.fields["filter"], OrderBy: q.fields["orderBy"], View: view})
	})
	return h
}

// pathName is the resource name in /v1/{name}.
func pathName(r *http.Request) string { return strings.TrimPrefix(r.URL.Path, "/v1/") }

// pathParent is the parent in /v1/{parent}/<collection>.
func pathParent(r *http.Request, collection string) string {
	return strings.TrimSuffix(pathName(r), "/"+collection)
}

// maxBodyBytes bounds a JSON request: 64KiB of plaintext and of AAD, base64
// encoded, with room for the rest.
const maxBodyBytes = 1 << 20

// decodeBody reads a JSON request into m. Unknown fields are refused, and a
// malformed body is reported without quoting it: protojson's syntax errors
// can quote input, and the input may be plaintext.
func decodeBody(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return apierror.InvalidArgument("the request body could not be read")
	}
	if len(b) > maxBodyBytes {
		return apierror.InvalidArgument("the request body exceeds %d bytes", maxBodyBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("the request body is not a valid %s", m.ProtoReflect().Descriptor().Name())
	}
	return nil
}

// nameBeforeVerb is the resource name in /v1/{name}:verb.
func nameBeforeVerb(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	if i := strings.LastIndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	return p
}

// transcode writes call's response as protojson, or its error in the KMS
// envelope with the same code gRPC gives. The method takes no query
// parameters beyond the system ones.
func transcode(call func(*http.Request, query) (proto.Message, error)) http.HandlerFunc {
	return transcodeWith(nil, call)
}

// transcodeWith is transcode for a method that takes the named request
// fields as query parameters.
func transcodeWith(fields []string, call func(*http.Request, query) (proto.Message, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := parseQuery(r, fields...)
		if err != nil {
			writeError(w, err)
			return
		}
		resp, err := call(r, q)
		if err != nil {
			writeError(w, err)
			return
		}
		b, err := q.marshal().Marshal(resp)
		if err != nil {
			writeError(w, apierror.Internal(err, "encode the response"))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}

func (h *RESTHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	for _, rt := range h.routes {
		if rt.Method != r.Method || !rt.re.MatchString(path) {
			continue
		}
		if fn, ok := h.handlers[rt.RPC]; ok {
			fn(w, r)
			return
		}
		writeError(w, apierror.Unimplemented("%s is not implemented over JSON", rt.RPC))
		return
	}
	writeError(w, apierror.NotFound("no method is bound to %s %s", r.Method, r.URL.Path))
}

// writeError writes the AIP-193 error envelope, with status set to the
// canonical code name, which the shared Cloud Storage-style writer omits.
func writeError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	code := codes.Code(e.Code)
	body := map[string]any{"error": map[string]any{
		"code":    e.HTTPStatus(),
		"message": e.Message,
		"status":  rpccode.Code(code).String(),
	}}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body)
}

// String names a route for tests and errors.
func (r Route) String() string { return fmt.Sprintf("%s %s (%s)", r.Method, r.Pattern, r.RPC) }
