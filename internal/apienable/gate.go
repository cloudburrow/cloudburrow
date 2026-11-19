package apienable

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Options tune a Gate.
type Options struct {
	// ServiceOf names the API a request calls when a port serves more
	// than one (Bigtable's data and admin APIs); "" is the Gate's own.
	ServiceOf func(r *http.Request) string
	// ProjectOf places a request the default ProjectOf cannot (a Cloud
	// Storage bucket's project); "" falls back to ProjectOf.
	ProjectOf func(r *http.Request) string
	// WriteJSON answers a REST denial in the API's own format; nil is
	// Denial.WriteJSON.
	WriteJSON func(w http.ResponseWriter, r *http.Request, d Denial)
	// Skip exempts a request (a front's own call log or readiness
	// endpoint, which no client of the API calls).
	Skip func(r *http.Request) bool
}

// Gate refuses next's requests to service in a project where it is
// disabled. A nil decider (or one Use left unset, for global) passes
// every request.
func Gate(d Decider, service string, next http.Handler, opts ...Options) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec := d
		if dec == nil {
			dec = global()
		}
		if dec == nil || (o.Skip != nil && o.Skip(r)) || strings.HasPrefix(r.URL.Path, "/cloudburrow/") || strings.HasPrefix(r.URL.Path, "/_cloudburrow/") {
			next.ServeHTTP(w, r)
			return
		}
		svc := service
		if o.ServiceOf != nil {
			if s := o.ServiceOf(r); s != "" {
				svc = s
			}
		}
		project := ""
		if o.ProjectOf != nil {
			project = o.ProjectOf(r)
		}
		if project == "" {
			project = ProjectOf(r)
		}
		if project == "" {
			next.ServeHTTP(w, r)
			return
		}
		denial, off := dec.Disabled(r.Context(), project, svc)
		if !off {
			next.ServeHTTP(w, r)
			return
		}
		if IsGRPC(r) {
			denial.WriteGRPC(w)
			return
		}
		if o.WriteJSON != nil {
			o.WriteJSON(w, r, denial)
			return
		}
		denial.WriteJSON(w)
	})
}

// globalDecider is the decider Use sets, for the servers in the cluster,
// as orgpolicy.Use sets theirs.
var globalDecider atomic.Pointer[deciderBox]

type deciderBox struct{ d Decider }

// Use makes d the decider of every Gate made with a nil one; nil turns
// them off.
func Use(d Decider) {
	if d == nil {
		globalDecider.Store(nil)
		return
	}
	globalDecider.Store(&deciderBox{d: d})
}

func global() Decider {
	if b := globalDecider.Load(); b != nil {
		return b.d
	}
	return nil
}

// UnaryInterceptor checks a call by the first project its request message
// names, for the calls whose routing headers name none. A nil decider is
// the one Use set, when the call is made.
func UnaryInterceptor(dec Decider, service string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		d := dec
		if d == nil {
			d = global()
		}
		if d != nil {
			if m, ok := req.(proto.Message); ok {
				if p := ProjectOfMessage(m); p != "" {
					if denial, off := d.Disabled(ctx, p, service); off {
						return nil, denial.Err()
					}
				}
			}
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor checks a stream by its metadata, else its first
// request message. A nil decider is the one Use set.
func StreamInterceptor(dec Decider, service string) grpc.StreamServerInterceptor {
	return StreamInterceptorFor(dec, func(string) string { return service })
}

// StreamInterceptorFor is StreamInterceptor for a server of several APIs:
// serviceOf names the API of a call's full method.
func StreamInterceptorFor(dec Decider, serviceOf func(fullMethod string) string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		d := dec
		if d == nil {
			d = global()
		}
		if d == nil {
			return handler(srv, ss)
		}
		service := serviceOf(info.FullMethod)
		if p := projectOfContext(ss.Context()); p != "" {
			if denial, off := d.Disabled(ss.Context(), p, service); off {
				return denial.Err()
			}
			return handler(srv, ss)
		}
		return handler(srv, &checkedStream{ServerStream: ss, d: d, service: service})
	}
}

type checkedStream struct {
	grpc.ServerStream
	d       Decider
	service string
	checked bool
}

func (s *checkedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if s.checked {
		return nil
	}
	s.checked = true
	if pm, ok := m.(proto.Message); ok {
		if p := ProjectOfMessage(pm); p != "" {
			if denial, off := s.d.Disabled(s.Context(), p, s.service); off {
				return denial.Err()
			}
		}
	}
	return nil
}

// projectOfContext is the project a call's metadata names.
func projectOfContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	return ProjectOfHeaders(func(k string) []string { return md.Get(k) })
}

// ProjectOfMessage is the project the first "projects/..." string field
// of m names, searching nested messages breadth first, two levels deep.
func ProjectOfMessage(m proto.Message) string {
	return projectIn(m.ProtoReflect(), 0)
}

func projectIn(m protoreflect.Message, depth int) string {
	var found string
	var nested []protoreflect.Message
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap():
			if p := ProjectOfName(v.String()); p != "" && strings.HasPrefix(strings.TrimPrefix(v.String(), "/"), "projects/") {
				found = p
				return false
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap():
			nested = append(nested, v.Message())
		}
		return true
	})
	if found != "" || depth >= 2 {
		return found
	}
	for _, n := range nested {
		if p := projectIn(n, depth+1); p != "" {
			return p
		}
	}
	return ""
}
