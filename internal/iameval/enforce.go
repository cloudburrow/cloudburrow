package iameval

import (
	"context"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
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
