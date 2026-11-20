package iameval

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Caller is who sent a request.
type Caller struct {
	// Email is the account its bearer token was minted for.
	Email string
	// Owner is the instance's own account (the ADC fixture's), or a
	// request with no bearer token, which is served as that account:
	// every permission, as Google gives a project's creator roles/owner.
	Owner bool
	// Invalid is a bearer token this instance did not mint.
	Invalid bool
}

// PolicyGetter reads a service resource's own policy, nil for none (or
// for a resource that does not exist).
type PolicyGetter func(ctx context.Context, resource string) *iampolicy.Stored

// Enforcer checks served calls when On reports true.
type Enforcer struct {
	Eval *Evaluator
	// On reports whether enforcement is on (--iam-enforcement).
	On func() bool
	// CallerOf names a request's caller from its bearer token, "" for
	// none.
	CallerOf func(bearer string) Caller
	// Policies are each service's own policy readers, by service host.
	Policies map[string]PolicyGetter
	// Now is the clock request.time reads; nil is time.Now.
	Now func() time.Time
	// OwnerEmail is the instance's own account, whom a request with no
	// bearer token is served as: deny policies apply to it too (#1886).
	OwnerEmail string
	// Scopes resolve a service's resource names, by service host, where
	// the name alone does not say what policies apply (round 11).
	Scopes map[string]func(resource string) Scope
	// ParentOf is a project's or folder's current parent ("folders/N",
	// "organizations/N"), "" for none or an unknown one.
	ParentOf func(resource string) string
}

// Scope is what applies to a resource whose name does not say.
type Scope struct {
	// Resource is the name its policies are kept under, "" for the name
	// as given.
	Resource string
	// Own, when set, are the service's own policy resources that apply,
	// the deepest first, in place of the service's PolicyCollections'.
	Own []string
	// Parent is the hierarchy resource ("projects/P", "folders/N",
	// "organizations/N") whose ancestry applies above it, "" for the one
	// its name shows.
	Parent string
}

// email is the account c is checked as.
func (e *Enforcer) email(c Caller) string {
	if c.Owner && c.Email == "" {
		return strings.ToLower(e.OwnerEmail)
	}
	return c.Email
}

// holds is whether c holds permission on resource in svc: the owner every
// permission no deny policy denies it, anyone else what the evaluator
// grants.
func (e *Enforcer) holds(ctx context.Context, svc *Service, c Caller, permission, resource string) bool {
	req := e.Request(ctx, svc, e.email(c), permission, resource)
	if c.Owner {
		return !e.Eval.Denies(req)
	}
	return e.Eval.Check(req).Granted
}

func (e *Enforcer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Unauthenticated is Google's 401 for a bearer token it does not accept.
func Unauthenticated() error {
	return status.Error(codes.Unauthenticated, "Request had invalid authentication credentials. Expected OAuth 2 access token, "+
		"login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project.")
}

// Interceptor checks svc's mapped methods before they run.
func (e *Enforcer) Interceptor(svc *Service) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		if strings.HasSuffix(info.FullMethod, "/TestIamPermissions") && svc.answersTestIAM(info.FullMethod) {
			return e.testPermissions(ctx, svc, req, handler)
		}
		_, m := ForMethod(svc, info.FullMethod)
		if m == nil {
			return handler(ctx, req)
		}
		msg, ok := req.(proto.Message)
		if !ok {
			return handler(ctx, req)
		}
		if m.Filter != "" {
			return e.filterGRPC(ctx, svc, m, req, handler)
		}
		resources := fieldValues(msg.ProtoReflect(), m.Field)
		for _, resource := range resources {
			if resource == "" && m.Optional {
				continue
			}
			if err := e.Authorize(ctx, svc, bearer(ctx), m.PermissionOf(resource), resource); err != nil {
				return nil, err
			}
		}
		resource := resources[0]
		if m.IfSet != "" && hasField(msg.ProtoReflect(), m.IfSet) {
			if err := e.Authorize(ctx, svc, bearer(ctx), m.IfSetPermission, resource); err != nil {
				return nil, err
			}
		}
		for _, a := range m.Also {
			for _, r := range actAsFields(msg.ProtoReflect(), []string{a.Field}) {
				if err := e.Authorize(ctx, svc, bearer(ctx), a.Permission, r); err != nil {
					return nil, err
				}
			}
		}
		for _, account := range actAsFields(msg.ProtoReflect(), m.ActAs) {
			if err := e.AuthorizeActAs(ctx, svc, bearer(ctx), Project(resource), account); err != nil {
				return nil, err
			}
		}
		if err := e.extra(ctx, svc, m, bearer(ctx), resource); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// testPermissions answers TestIamPermissions as Google does: the
// requested permissions the caller holds on the resource. The instance's
// own account holds every one.
func (e *Enforcer) testPermissions(ctx context.Context, svc *Service, req any, handler grpc.UnaryHandler) (any, error) {
	in, ok := req.(*iampb.TestIamPermissionsRequest)
	if !ok {
		return handler(ctx, req)
	}
	c := Caller{Owner: true}
	if b := bearer(ctx); b != "" && e.CallerOf != nil {
		c = e.CallerOf(b)
	}
	if c.Invalid {
		return nil, Unauthenticated()
	}
	resp, err := handler(ctx, req)
	if err != nil {
		return resp, err
	}
	out, ok := resp.(*iampb.TestIamPermissionsResponse)
	if !ok {
		return resp, err
	}
	var held []string
	for _, p := range out.GetPermissions() {
		if e.holds(ctx, svc, c, p, in.GetResource()) {
			held = append(held, p)
		}
	}
	return &iampb.TestIamPermissionsResponse{Permissions: held}, nil
}

// Authorize checks that the caller of bearer has permission on resource
// in svc, nil when it does (or enforcement is off), else svc's denial.
func (e *Enforcer) Authorize(ctx context.Context, svc *Service, bearer, permission, resource string) error {
	if e == nil || e.On == nil || !e.On() || permission == "" {
		return nil
	}
	c := Caller{Owner: true}
	if bearer != "" && e.CallerOf != nil {
		c = e.CallerOf(bearer)
	}
	if c.Invalid {
		return Unauthenticated()
	}
	if e.holds(ctx, svc, c, permission, resource) {
		return nil
	}
	return svc.Deny(permission, resource)
}

// Request is the check of permission on resource in svc for email, with
// the service's own policies on the resource and its parents.
func (e *Enforcer) Request(ctx context.Context, svc *Service, email, permission, resource string) Request {
	own, parent := svc.PolicyResources(resource), ""
	if f := e.Scopes[svc.Name]; f != nil {
		// A name its policies are not kept under (a service account by
		// "projects/-", a tag value), or whose ancestry its name does
		// not show (#1879, round 11).
		sc := f(resource)
		if sc.Resource != "" {
			resource = sc.Resource
			own = svc.PolicyResources(resource)
		}
		if sc.Own != nil {
			own = sc.Own
		}
		parent = sc.Parent
	}
	req := Request{Email: email, Permission: permission, Resource: resource, Type: svc.Type(resource),
		Service: svc.Name, Project: Project(resource), Time: e.now()}
	if parent != "" {
		req.Project = Project(parent)
	}
	if get := e.Policies[svc.Name]; get != nil {
		for _, r := range own {
			req.Own = append(req.Own, Attached{Resource: "//" + svc.Name + "/" + r, Policy: get(ctx, r)})
		}
	}
	if req.Project == "" && e.Eval != nil {
		// A folder's, the organization's or a billing account's resource:
		// its root's policy and the root's ancestors' apply (#1879).
		root := resource
		if parent != "" {
			root = parent
		}
		req.Own = append(req.Own, e.Eval.hierarchyPolicies(root)...)
	}
	return req
}

// FromProto is a policy as the evaluator reads it.
func FromProto(p *iampb.Policy) *iampolicy.Stored {
	if p == nil {
		return nil
	}
	s := &iampolicy.Stored{Etag: p.GetEtag()}
	for _, b := range p.GetBindings() {
		bd := iampolicy.Binding{Role: b.GetRole(), Members: b.GetMembers()}
		if c := b.GetCondition(); c != nil {
			bd.Condition = &iampolicy.Condition{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		}
		s.Bindings = append(s.Bindings, bd)
	}
	return s
}

// GRPCPolicyGetter reads policies through a service's GetIamPolicy, at
// version 3 so conditions come back.
func GRPCPolicyGetter(get func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)) PolicyGetter {
	return func(ctx context.Context, resource string) *iampolicy.Stored {
		f := get()
		if f == nil {
			return nil
		}
		p, err := f(ctx, &iampb.GetIamPolicyRequest{Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			return nil
		}
		return FromProto(p)
	}
}

func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t, ok := cutBearer(v); ok {
			return t
		}
	}
	return ""
}

// IncomingBearer is the bearer token of a call's incoming gRPC metadata,
// "" for none.
func IncomingBearer(ctx context.Context) string { return bearer(ctx) }

// Bearer is an Authorization header's bearer token, "" for none.
func Bearer(header string) string {
	t, _ := cutBearer(header)
	return t
}

func cutBearer(v string) (string, bool) {
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:]), true
	}
	return "", false
}

// field reads a dotted string field of m, "" when absent.
func field(m protoreflect.Message, path string) string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return ""
		}
		if i == len(parts)-1 {
			if fd.Kind() != protoreflect.StringKind {
				return ""
			}
			return m.Get(fd).String()
		}
		if fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
			return ""
		}
		m = m.Get(fd).Message()
	}
	return ""
}

// HTTP checks svc's JSON requests as Interceptor checks its gRPC calls:
// m names the method a request is bound to and the resource its path
// names. A denial is the same status, as Google's JSON error envelope.
func (e *Enforcer) HTTP(svc *Service, m *rest.Matcher, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || m == nil {
			next.ServeHTTP(w, r)
			return
		}
		route, fields, ok := m.MatchRoute(r)
		full := "/" + route.Service + "/" + route.MethodName
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(full, "/TestIamPermissions") && svc.answersTestIAM(full) {
			e.testPermissionsJSON(w, r, svc, fields["resource"], next)
			return
		}
		_, row := ForMethod(svc, full)
		if row == nil {
			next.ServeHTTP(w, r)
			return
		}
		if row.Filter != "" {
			e.filterJSON(w, r, svc, row, next)
			return
		}
		var raw []byte
		if route.Body != "" && r.Body != nil {
			b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			if err == nil {
				raw = b
			}
		}
		resources := jsonResources(fields, raw, route.Body, row.Field)
		for _, resource := range resources {
			if resource == "" && row.Optional {
				continue
			}
			if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), row.PermissionOf(resource), resource); err != nil {
				WriteJSONStatus(w, err)
				return
			}
		}
		resource := resources[0]
		if row.IfSet != "" && jsonHas(raw, route.Body, row.IfSet) {
			if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), row.IfSetPermission, resource); err != nil {
				WriteJSONStatus(w, err)
				return
			}
		}
		for _, a := range row.Also {
			for _, res := range jsonActAs(raw, route.Body, []string{a.Field}) {
				if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), a.Permission, res); err != nil {
					WriteJSONStatus(w, err)
					return
				}
			}
		}
		accounts := jsonActAs(raw, route.Body, row.ActAs)
		for _, q := range row.ActAsQuery {
			if v := strings.TrimPrefix(r.URL.Query().Get(q), "serviceAccount:"); v != "" {
				accounts = append(accounts, v)
			}
		}
		for _, account := range accounts {
			if err := e.AuthorizeActAs(r.Context(), svc, Bearer(r.Header.Get("Authorization")), Project(resource), account); err != nil {
				WriteJSONStatus(w, err)
				return
			}
		}
		if err := e.extra(r.Context(), svc, row, Bearer(r.Header.Get("Authorization")), resource); err != nil {
			WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteJSONStatus writes a gRPC status as Google's JSON error envelope,
// details included ({"error": {"code", "message", "status", "details"}}).
func WriteJSONStatus(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	code := runtimeHTTPStatus(st.Code())
	details := []json.RawMessage{}
	for _, d := range st.Proto().GetDetails() {
		if b, err := protojson.Marshal(d); err == nil {
			details = append(details, b)
		}
	}
	body := map[string]any{"error": map[string]any{"code": code, "message": st.Message(), "status": st.Code().String(), "details": details}}
	if st.Code() == codes.PermissionDenied {
		body["error"].(map[string]any)["status"] = "PERMISSION_DENIED"
	}
	if st.Code() == codes.Unauthenticated {
		body["error"].(map[string]any)["status"] = "UNAUTHENTICATED"
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func runtimeHTTPStatus(c codes.Code) int {
	switch c {
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

// testPermissionsJSON is testPermissions over JSON: the handler's answer,
// {"permissions": [...]}, keeps only those the caller holds on resource.
func (e *Enforcer) testPermissionsJSON(w http.ResponseWriter, r *http.Request, svc *Service, resource string, next http.Handler) {
	c := Caller{Owner: true}
	if b := Bearer(r.Header.Get("Authorization")); b != "" && e.CallerOf != nil {
		c = e.CallerOf(b)
	}
	if c.Invalid {
		WriteJSONStatus(w, Unauthenticated())
		return
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var out map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	held := []any{}
	if perms, ok := out["permissions"].([]any); ok {
		for _, p := range perms {
			if ps, ok := p.(string); ok && e.holds(r.Context(), svc, c, ps, resource) {
				held = append(held, ps)
			}
		}
	}
	if len(held) == 0 {
		delete(out, "permissions")
	} else {
		out["permissions"] = held
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// HTTPPaths checks svc's methods served over JSON alone, matched by their
// paths (WorkflowsJSON), as HTTP checks the bound ones.
func (e *Enforcer) HTTPPaths(svc *Service, methods []JSONMethod, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		permission, resource, ok := MatchJSON(methods, r.Method, r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), permission, resource); err != nil {
			WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checksPolicies is whether svc's table checks its IAM policy methods,
// and so answers TestIamPermissions with what the caller holds: a table
// that checks only actAs (Cloud Run's, Cloud Functions') leaves its IAM
// methods as they were.
func (s *Service) checksPolicies() bool {
	for full := range s.Methods {
		if strings.HasSuffix(full, "/GetIamPolicy") {
			return true
		}
	}
	return false
}

// answersTestIAM is whether svc's table answers a TestIamPermissions call
// of full with what the caller holds: its own service's method, or the
// IAMPolicy mixin's when the table checks that mixin's methods. Several
// APIs share a port (Cloud Run's serves Cloud Functions, Artifact Registry
// and Cloud Build), so each table answers only its own.
func (s *Service) answersTestIAM(full string) bool {
	if !s.checksPolicies() {
		return false
	}
	service, _, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
	if service == "google.iam.v1.IAMPolicy" {
		_, ok := s.Methods["/google.iam.v1.IAMPolicy/GetIamPolicy"]
		return ok
	}
	return Services[service] == s
}

// hasField is whether m sets the message field at the dotted path.
func hasField(m protoreflect.Message, path string) bool {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil || !m.Has(fd) {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return false
		}
		m = m.Get(fd).Message()
	}
	return false
}

// jsonHas is whether a JSON request's body sets the field at path, a
// request field path as jsonActAs reads it.
func jsonHas(raw []byte, body, path string) bool {
	if body == "" || len(raw) == 0 {
		return false
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	parts := strings.Split(path, ".")
	if body != "*" {
		if parts[0] != body {
			return false
		}
		parts = parts[1:]
	}
	for _, p := range parts {
		m, ok := doc.(map[string]any)
		if !ok {
			return false
		}
		v, ok := m[lowerCamel(p)]
		if !ok {
			v, ok = m[p]
		}
		if !ok || v == nil {
			return false
		}
		doc = v
	}
	return true
}
