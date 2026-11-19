package iameval

import (
	"context"
	"encoding/json"
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
		if strings.HasSuffix(info.FullMethod, "/TestIamPermissions") {
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
		resource := field(msg.ProtoReflect(), m.Field)
		if err := e.Authorize(ctx, svc, bearer(ctx), m.PermissionOf(resource), resource); err != nil {
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
	if err != nil || c.Owner {
		return resp, err
	}
	out, ok := resp.(*iampb.TestIamPermissionsResponse)
	if !ok {
		return resp, err
	}
	var held []string
	for _, p := range out.GetPermissions() {
		if e.Eval.Check(e.Request(ctx, svc, c.Email, p, in.GetResource())).Granted {
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
	if c.Owner {
		return nil
	}
	d := e.Eval.Check(e.Request(ctx, svc, c.Email, permission, resource))
	if d.Granted {
		return nil
	}
	return svc.Deny(permission, resource)
}

// Request is the check of permission on resource in svc for email, with
// the service's own policies on the resource and its parents.
func (e *Enforcer) Request(ctx context.Context, svc *Service, email, permission, resource string) Request {
	req := Request{Email: email, Permission: permission, Resource: resource, Type: svc.Type(resource),
		Service: svc.Name, Project: Project(resource), Time: e.now()}
	if get := e.Policies[svc.Name]; get != nil {
		for _, r := range svc.PolicyResources(resource) {
			req.Own = append(req.Own, Attached{Resource: "//" + svc.Name + "/" + r, Policy: get(ctx, r)})
		}
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
		full, fields, ok := m.Match(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(full, "/TestIamPermissions") {
			e.testPermissionsJSON(w, r, svc, fields["resource"], next)
			return
		}
		_, row := ForMethod(svc, full)
		if row == nil {
			next.ServeHTTP(w, r)
			return
		}
		resource := fields[row.Field]
		if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), row.PermissionOf(resource), resource); err != nil {
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
	if c.Owner {
		next.ServeHTTP(w, r)
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
			if ps, ok := p.(string); ok && e.Eval.Check(e.Request(r.Context(), svc, c.Email, ps, resource)).Granted {
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
