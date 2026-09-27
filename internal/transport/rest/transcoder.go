package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"google.golang.org/genproto/googleapis/api/annotations"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Transcoder serves gRPC methods as Google's JSON/HTTP API (#591), by the
// google.api.http bindings of their protos, so a service that implements a
// gRPC server has its REST surface without writing one.
//
// Its routes are every google.api.http binding of every service in Packages,
// read from the generated descriptors, then Mixins: the rules a service's
// yaml adds (Locations, IAMPolicy, Operations), which no descriptor of the
// package carries. A request matching a route whose service is registered
// with the Transcoder calls that implementation, the same method gRPC calls;
// a route whose service or method is not registered answers 501
// UNIMPLEMENTED; a path matching no route answers 404.
//
// It is a grpc.ServiceRegistrar: register a server with it as with a
// *grpc.Server, e.g. kmspb.RegisterKeyManagementServiceServer(t, impl), and
// mount it with grpctransport.Server.ServeHTTP to share the gRPC port (h2c).
// The call does not pass through the gRPC server's interceptors.
//
// The mapping follows google/api/http.proto: path variables set the request
// fields they name; body "*" is the whole request, body "<field>" that field;
// every other leaf field is a query parameter, named by its JSON or proto
// field path. The query policy is in query.go. Errors are the AIP-193
// envelope, {"error": {"code", "message", "status"}}.
//
// Fields are read on first use, so set them before serving.
type Transcoder struct {
	// Packages are the proto packages whose bindings are served, e.g.
	// "google.cloud.kms.v1".
	Packages []string
	// Mixins are extra bindings, each naming its method in Selector, e.g.
	// "google.iam.v1.IAMPolicy.GetIamPolicy". A selector need not resolve:
	// a binding to a method that exists nowhere answers 501, which says the
	// API has the method where a 404 would say it has not.
	Mixins []*annotations.HttpRule
	// MaxBodyBytes bounds a request body; 0 is MaxRequestBytes.
	MaxBodyBytes int64
	// RefuseBodyConflicts names the methods, by selector, for which a field
	// the path binds that the body also sets to another value is
	// INVALID_ARGUMENT. For any other method the path's value wins. What
	// Google does is UNVERIFIED and may differ by method.
	RefuseBodyConflicts []string

	once     sync.Once
	routes   []Route
	refuse   map[string]bool
	mu       sync.RWMutex
	services map[string]registered // by full service name
}

type registered struct {
	impl    any
	methods map[string]grpc.MethodDesc
}

// Route is one bound HTTP method and path.
type Route struct {
	// RPC is the method it transcodes to, with the short service name, e.g.
	// KeyManagementService/Encrypt or IAMPolicy/GetIamPolicy.
	RPC string
	// Service and MethodName are the full service name and the method, e.g.
	// google.cloud.kms.v1.KeyManagementService and Encrypt.
	Service, MethodName string
	Method              string // HTTP method
	Pattern             string // path template
	Body                string // "", "*" or a request field
	ResponseBody        string

	re   *regexp.Regexp
	vars []string // the field path each capture group sets
}

// String names a route for tests and errors.
func (r Route) String() string { return fmt.Sprintf("%s %s (%s)", r.Method, r.Pattern, r.RPC) }

// RegisterService makes impl serve desc's methods over JSON, as
// grpc.Server.RegisterService does over gRPC.
func (t *Transcoder) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if impl != nil && desc.HandlerType != nil {
		ht := reflect.TypeOf(desc.HandlerType).Elem()
		if st := reflect.TypeOf(impl); !st.Implements(ht) {
			panic(fmt.Sprintf("rest: RegisterService: %v does not implement %v", st, ht))
		}
	}
	// Streams are left out: a streaming method has no JSON form here, so it
	// answers 501 as an unregistered one does.
	reg := registered{impl: impl, methods: map[string]grpc.MethodDesc{}}
	for _, m := range desc.Methods {
		reg.methods[m.MethodName] = m
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.services == nil {
		t.services = map[string]registered{}
	}
	if _, dup := t.services[desc.ServiceName]; dup {
		panic(fmt.Sprintf("rest: RegisterService: %s is already registered", desc.ServiceName))
	}
	t.services[desc.ServiceName] = reg
}

// Routes returns every route: the bindings of Packages, sorted by RPC and
// HTTP method, then Mixins in order. A request is served by the first route
// it matches.
func (t *Transcoder) Routes() []Route {
	t.init()
	return append([]Route(nil), t.routes...)
}

func (t *Transcoder) init() {
	t.once.Do(func() {
		t.refuse = map[string]bool{}
		for _, s := range t.RefuseBodyConflicts {
			t.refuse[s] = true
		}
		var out []Route
		for _, pkg := range t.Packages {
			protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(pkg), func(f protoreflect.FileDescriptor) bool {
				for i := 0; i < f.Services().Len(); i++ {
					sd := f.Services().Get(i)
					for j := 0; j < sd.Methods().Len(); j++ {
						md := sd.Methods().Get(j)
						rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule)
						if rule != nil {
							out = append(out, routesOf(string(sd.FullName()), string(md.Name()), rule)...)
						}
					}
				}
				return true
			})
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].RPC+out[i].Method < out[j].RPC+out[j].Method })
		for _, m := range t.Mixins {
			i := strings.LastIndexByte(m.GetSelector(), '.')
			if i <= 0 {
				panic(fmt.Sprintf("rest: mixin selector %q does not name a method", m.GetSelector()))
			}
			out = append(out, routesOf(m.GetSelector()[:i], m.GetSelector()[i+1:], m)...)
		}
		for i := range out {
			out[i].re, out[i].vars = compileTemplate(out[i].Pattern)
		}
		t.routes = out
	})
}

// routesOf is a rule and its additional bindings, which take the rule's
// method but their own body.
func routesOf(service, method string, rule *annotations.HttpRule) []Route {
	short := service
	if i := strings.LastIndexByte(service, '.'); i >= 0 {
		short = service[i+1:]
	}
	var out []Route
	for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
		if m, p := httpOf(r); m != "" {
			out = append(out, Route{RPC: short + "/" + method, Service: service, MethodName: method,
				Method: m, Pattern: p, Body: r.GetBody(), ResponseBody: r.GetResponseBody()})
		}
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

// compileTemplate compiles an HTTP rule path template, matched against the
// escaped path, with a capture group per variable. `*` is one segment and
// `**` one or more; neither crosses the `:verb` that ends a path, so a
// resource ID containing a colon does not match.
func compileTemplate(t string) (*regexp.Regexp, []string) {
	var b strings.Builder
	var vars []string
	segments := func(s string) {
		for k, seg := range strings.Split(s, "/") {
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
	}
	b.WriteString("^")
	for i := 0; i < len(t); {
		if t[i] != '{' {
			j := strings.IndexByte(t[i:], '{')
			if j < 0 {
				j = len(t) - i
			}
			segments(t[i : i+j])
			i += j
			continue
		}
		end := strings.IndexByte(t[i:], '}')
		if end < 0 {
			panic(fmt.Sprintf("rest: path template %q has an unclosed variable", t))
		}
		end += i
		field, pattern, ok := strings.Cut(t[i+1:end], "=")
		if !ok {
			pattern = "*"
		}
		vars = append(vars, field)
		b.WriteString("(")
		segments(pattern)
		b.WriteString(")")
		i = end + 1
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()), vars
}

func (t *Transcoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.init()
	path := r.URL.EscapedPath()
	for i := range t.routes {
		rt := &t.routes[i]
		if rt.Method != r.Method {
			continue
		}
		m := rt.re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		t.serve(w, r, rt, m[1:])
		return
	}
	writeStatusError(w, apierror.NotFound("no method is bound to %s %s", r.Method, r.URL.Path))
}

func (t *Transcoder) serve(w http.ResponseWriter, r *http.Request, rt *Route, captures []string) {
	t.mu.RLock()
	svc, ok := t.services[rt.Service]
	t.mu.RUnlock()
	md, bound := svc.methods[rt.MethodName]
	switch {
	case !ok || !bound: // a streaming method is not in methods
		writeStatusError(w, apierror.Unimplemented("%s is not implemented over JSON", rt.RPC))
		return
	case rt.ResponseBody != "":
		writeStatusError(w, apierror.Unimplemented("%s: a response_body binding is not implemented over JSON", rt.RPC))
		return
	}
	var q query
	dec := func(in any) error {
		msg, ok := in.(proto.Message)
		if !ok {
			return apierror.Internal(nil, "%s: the request is not a proto message", rt.RPC)
		}
		var err error
		q, err = t.decode(r, rt, captures, msg)
		return err
	}
	resp, err := md.Handler(svc.impl, r.Context(), dec, nil)
	if err != nil {
		writeStatusError(w, err)
		return
	}
	msg, ok := resp.(proto.Message)
	if !ok {
		writeStatusError(w, apierror.Internal(nil, "%s: the response is not a proto message", rt.RPC))
		return
	}
	b, err := q.marshal().Marshal(msg)
	if err != nil {
		writeStatusError(w, apierror.Internal(err, "encode the response"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// decode builds the request: the query, then the body, then the path.
func (t *Transcoder) decode(r *http.Request, rt *Route, captures []string, req proto.Message) (query, error) {
	root := req.ProtoReflect()
	q, err := parseQuery(r.URL.RawQuery, root, rt)
	if err != nil {
		return q, err
	}
	switch rt.Body {
	case "":
	case "*":
		if err := t.decodeBody(r, req); err != nil {
			return q, err
		}
	default:
		fd, parent, err := resolve(root, rt.Body, true)
		if err != nil {
			return q, err
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return q, apierror.Unimplemented("%s: a body bound to the non-message field %s is not implemented over JSON", rt.RPC, rt.Body)
		}
		// Mutable, so an empty body is an empty message and not an absent one.
		if err := t.decodeBody(r, parent.Mutable(fd).Message().Interface()); err != nil {
			return q, err
		}
	}
	for i, field := range rt.vars {
		v, err := url.PathUnescape(captures[i])
		if err != nil {
			return q, apierror.InvalidArgument("the path is not validly escaped")
		}
		fd, parent, err := resolve(root, field, true)
		if err != nil {
			return q, err
		}
		val, err := scalar(fd, field, v, func() protoreflect.Value { return parent.NewField(fd) })
		if err != nil {
			return q, err
		}
		if t.refuse[rt.Service+"."+rt.MethodName] && parent.Has(fd) && !parent.Get(fd).Equal(val) {
			leaf := string(fd.Name())
			return q, apierror.InvalidArgument("the %s in the body, %q, differs from the %s in the path, %q", leaf, fmt.Sprint(parent.Get(fd).Interface()), leaf, v)
		}
		parent.Set(fd, val)
	}
	return q, nil
}

// resolve finds the field a proto field path (a.b.c) names, returning it and
// the message holding it, made mutable when create is set.
func resolve(root protoreflect.Message, path string, create bool) (protoreflect.FieldDescriptor, protoreflect.Message, error) {
	m := root
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return nil, nil, apierror.Internal(nil, "%s has no field %s", root.Descriptor().FullName(), path)
		}
		if i == len(parts)-1 {
			return fd, m, nil
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return nil, nil, apierror.Internal(nil, "%s: %s is not a message", root.Descriptor().FullName(), path)
		}
		if create {
			m = m.Mutable(fd).Message()
		} else {
			m = m.Get(fd).Message()
		}
	}
	return nil, nil, apierror.Internal(nil, "empty field path")
}

// decodeBody reads a JSON request into m. Unknown fields are refused; an
// empty body is an empty message; a malformed body is reported without
// quoting it, since protojson's syntax errors can quote input and the input
// may be a secret.
func (t *Transcoder) decodeBody(r *http.Request, m proto.Message) error {
	limit := t.MaxBodyBytes
	if limit <= 0 {
		limit = MaxRequestBytes
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return apierror.InvalidArgument("the request body could not be read")
	}
	if int64(len(b)) > limit {
		return apierror.InvalidArgument("the request body exceeds %d bytes", limit)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("the request body is not a valid %s", m.ProtoReflect().Descriptor().Name())
	}
	return nil
}

// writeStatusError writes the AIP-193 error envelope, with status set to the
// canonical code name, which the Cloud Storage-style WriteError omits.
func writeStatusError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	body := map[string]any{"error": map[string]any{
		"code":    e.HTTPStatus(),
		"message": e.Message,
		"status":  rpccode.Code(e.Code).String(),
	}}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body)
}
