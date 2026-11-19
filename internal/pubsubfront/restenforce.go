package pubsubfront

import (
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// IAM enforcement over Pub/Sub's REST transport (#1887): a request is
// bound to its gRPC method by pubsub.proto's HTTP rules, its request
// message is built from the path's fields and the JSON body, and it is
// checked as that method is over gRPC (psChecks), with the same refusal in
// Google's JSON error envelope. The IAMPolicy paths
// (/v1/{resource}:getIamPolicy, :setIamPolicy) are checked as their mixin
// methods. The request's bearer token travels in its context as gRPC
// metadata, so testIamPermissions answers what the caller holds.

var (
	restMatcherOnce sync.Once
	restMatcher     *rest.Matcher
)

func pubsubMatcher() *rest.Matcher {
	restMatcherOnce.Do(func() {
		restMatcher = rest.NewMatcher([]string{"google.pubsub.v1.Publisher", "google.pubsub.v1.Subscriber", "google.pubsub.v1.SchemaService"}, nil)
	})
	return restMatcher
}

// withBearer is r with its Authorization header as incoming gRPC metadata.
func withBearer(r *http.Request) *http.Request {
	h := r.Header.Get("Authorization")
	if h == "" {
		return r
	}
	md, _ := metadata.FromIncomingContext(r.Context())
	md = md.Copy()
	md.Set("authorization", h)
	return r.WithContext(metadata.NewIncomingContext(r.Context(), md))
}

// restAuthorize checks a REST request; nil when it may go on.
func (f *Front) restAuthorize(r *http.Request) error {
	if !f.iamCheck.Enabled() {
		return nil
	}
	if name, verb, ok := iamPath(r.URL.Path); ok {
		switch verb {
		case "getIamPolicy", "setIamPolicy":
			m := iamService + strings.ToUpper(verb[:1]) + verb[1:]
			fr, _ := proto.Marshal(iamRequest(m, name))
			return f.authorize(r.Context(), m, fr)
		}
		return nil
	}
	full, fields, ok := pubsubMatcher().Match(r)
	if !ok {
		return nil
	}
	if _, mapped := psChecks[full]; !mapped {
		return nil
	}
	msg := inputOf(full)
	if msg == nil {
		return nil
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		body, err := readBody(r)
		if err != nil {
			return err
		}
		if len(body) > 0 {
			// The rules' body is "*", the whole request, for every
			// mapped method with one.
			_ = restJSON.Unmarshal(body, msg.Interface())
		}
	}
	for path, v := range fields {
		setString(msg, path, v)
	}
	fr, err := proto.Marshal(msg.Interface())
	if err != nil {
		return nil
	}
	return f.authorize(r.Context(), full, fr)
}

// iamRequest is the IAMPolicy request m names for resource.
func iamRequest(m, resource string) proto.Message {
	msg := inputOf(m)
	if msg == nil {
		return nil
	}
	setString(msg, "resource", resource)
	return msg.Interface()
}

// inputOf is a new request message of the full method ("/pkg.Svc/M").
func inputOf(full string) protoreflect.Message {
	svc, name, ok := strings.Cut(strings.TrimPrefix(full, "/"), "/")
	if !ok {
		return nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return nil
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
	if err != nil {
		return nil
	}
	return mt.New()
}

// setString sets a dotted string field of m, making its parents.
func setString(m protoreflect.Message, path, v string) {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return
		}
		if i == len(parts)-1 {
			if fd.Kind() == protoreflect.StringKind && !fd.IsList() {
				m.Set(fd, protoreflect.ValueOfString(v))
			}
			return
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return
		}
		m = m.Mutable(fd).Message()
	}
}
