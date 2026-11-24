// Package iamgate is IAM enforcement (#1879) in the fronts CloudBurrow runs
// beside the Google emulators in the cluster: Firestore's and Spanner's
// call fronts (internal/callfront) and Bigtable's (internal/bigtablefront).
// Each call is bound to its gRPC method (over gRPC, or over REST by the
// method's HTTP rule), its request is decoded, and the permissions Google's
// reference names for the method are checked on the resource its request
// names, by the host's one evaluator (internal/iamcheck): the caller named
// from the bearer token the instance minted, the resource's own policies
// in the front (a Spanner database's, a Bigtable table's) and its
// project's, folders' and organization's. Only `up --iam-enforcement=on`
// gives a front the host's address; without it nothing is checked. A
// method no table row names is not checked (docs/compatibility.md lists
// what is).
package iamgate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Check is one permission on one resource.
type Check struct {
	Permission, Resource string
}

// Rule is a method's checks for its decoded request; none is a call that
// needs nothing (or names no resource, which the method refuses itself).
type Rule func(m protoreflect.Message) []Check

// Service is one API's table.
type Service struct {
	// Name is the API's host ("firestore.googleapis.com"), resource.service.
	Name string
	// Methods are the rules by gRPC full method.
	Methods map[string]Rule
	// Type is a checked resource's resource.type.
	Type func(resource string) string
	// Deny is the error a denied call gets, in the API's words.
	Deny func(permission, resource string) error
	// TestIAM are the API's TestIamPermissions methods (full names),
	// which the gate answers for a caller with a token from the host's
	// evaluator.
	TestIAM map[string]bool
}

// Gate checks the calls of its services.
type Gate struct {
	Client   *iamcheck.Client
	Services []*Service
	// Own are the resource's own policies the front keeps (a Spanner
	// database's and its instance's), the resource's first; nil for none.
	Own func(resource string) []iamcheck.Policy
	// PassAs, when set, is the bearer token a call with one is passed to
	// the emulator with once checked: the Firestore emulator reads a
	// bearer token as a Firebase JWT, and refuses an access token the
	// instance minted ("invalid jwt"); "owner" is its own account.
	PassAs string

	once    sync.Once
	matcher *rest.Matcher
}

// Enabled reports whether g checks anything.
func (g *Gate) Enabled() bool { return g != nil && g.Client.Enabled() }

// rule is the service and rule of a full method.
func (g *Gate) rule(full string) (*Service, Rule) {
	for _, s := range g.Services {
		if r, ok := s.Methods[full]; ok {
			return s, r
		}
	}
	return nil, nil
}

// Checked reports whether a full method has a rule, or is a
// TestIamPermissions the gate answers.
func (g *Gate) Checked(full string) bool {
	_, r := g.rule(full)
	return r != nil || g.testIAM(full) != nil
}

// Authorize checks a call of full with request m, sent with bearer; nil
// when it may go on.
func (g *Gate) Authorize(ctx context.Context, bearer, full string, m protoreflect.Message) error {
	if !g.Enabled() || m == nil {
		return nil
	}
	svc, r := g.rule(full)
	if r == nil {
		return nil
	}
	// The checks on one resource go in one request, in order.
	var order []string
	byRes := map[string][]string{}
	for _, c := range r(m) {
		if c.Resource == "" || c.Permission == "" {
			continue
		}
		c.Resource = strings.ReplaceAll(c.Resource, Routed, routedDatabase(ctx))
		if _, ok := byRes[c.Resource]; !ok {
			order = append(order, c.Resource)
		}
		byRes[c.Resource] = append(byRes[c.Resource], c.Permission)
	}
	for _, res := range order {
		req := iamcheck.Request{Bearer: bearer, Service: svc.Name, Permissions: byRes[res], Resource: res, Project: Project(res)}
		if svc.Type != nil {
			req.Type = svc.Type(res)
		}
		if g.Own != nil {
			req.Own = g.Own(res)
		}
		d, err := g.Client.Check(ctx, req)
		if err != nil {
			return status.Errorf(codes.Unavailable, "IAM check unavailable: %v", err)
		}
		if d.Unauthenticated {
			return Unauthenticated()
		}
		if d.ScopeInsufficient {
			return iamcheck.InsufficientScopes(svc.Name, strings.TrimPrefix(full, "/"))
		}
		for _, p := range byRes[res] {
			if !d.Holds(p) {
				return svc.Deny(p, res)
			}
		}
	}
	return nil
}

// Unauthenticated is Google's refusal of a bearer token it did not issue.
func Unauthenticated() error {
	return status.Error(codes.Unauthenticated, "Request had invalid authentication credentials. Expected OAuth 2 access token, "+
		"login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project.")
}

// Project is the project segment of a resource name, "" for none.
func Project(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "projects" {
			return parts[i+1]
		}
	}
	return ""
}

// Prefix is the first n collection/ID pairs of name ("projects/p/instances/i"
// for n 2), "" when name has fewer.
func Prefix(name string, n int) string {
	parts := strings.Split(name, "/")
	if len(parts) < 2*n {
		return ""
	}
	for i := 1; i < 2*n; i += 2 {
		if parts[i] == "" {
			return ""
		}
	}
	return strings.Join(parts[:2*n], "/")
}

// Input is a new request message of a full method ("/pkg.Svc/M").
func Input(full string) protoreflect.Message {
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

// String reads a dotted string field of m, "" when absent.
func String(m protoreflect.Message, path string) string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil || fd.IsList() || fd.IsMap() {
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

// Has is whether m sets the field at the dotted path.
func Has(m protoreflect.Message, path string) bool {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return false
		}
		if fd.IsList() {
			return i == len(parts)-1 && m.Get(fd).List().Len() > 0
		}
		if !m.Has(fd) {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
			return false
		}
		m = m.Get(fd).Message()
	}
	return false
}

// List is the messages of a repeated message field of m.
func List(m protoreflect.Message, name string) []protoreflect.Message {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || !fd.IsList() || fd.Kind() != protoreflect.MessageKind {
		return nil
	}
	l := m.Get(fd).List()
	out := make([]protoreflect.Message, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		out = append(out, l.Get(i).Message())
	}
	return out
}

// On is a rule of fixed permissions on the resource field names (via of,
// when set, maps it to the resource checked).
func On(field string, of func(string) string, perms ...string) Rule {
	return func(m protoreflect.Message) []Check {
		res := String(m, field)
		if of != nil && res != "" {
			res = of(res)
		}
		if res == "" {
			return nil
		}
		out := make([]Check, len(perms))
		for i, p := range perms {
			out[i] = Check{p, res}
		}
		return out
	}
}

// bearerOf is the Authorization header's bearer token. "owner", the
// token the Google clients send an emulator (FIRESTORE_EMULATOR_HOST), is
// none: the instance's own account, as a call with no token is.
func bearerOf(h string) string {
	b := iamcheck.Bearer(h)
	if b == "owner" {
		return ""
	}
	return b
}

// BearerOf is a gRPC call's bearer token, from its incoming metadata.
func BearerOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if b := bearerOf(v); b != "" {
			return b
		}
	}
	return ""
}

func (g *Gate) restMatcher() *rest.Matcher {
	g.once.Do(func() {
		seen := map[string]bool{}
		var names []string
		for _, s := range g.Services {
			for full := range s.Methods {
				svc, _, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
				if !seen[svc] {
					seen[svc] = true
					names = append(names, svc)
				}
			}
		}
		g.matcher = rest.NewMatcher(names, nil)
	})
	return g.matcher
}

// maxBody is the largest request the HTTP gate reads to check it.
const maxBody = 64 << 20

// Wrap checks the gRPC (HTTP/2) and REST calls next serves, answering a
// denied one with the API's refusal: a gRPC status, or Google's JSON
// error envelope.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	if !g.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := bearerOf(r.Header.Get("Authorization"))
		if g.PassAs != "" && bearer != "" {
			// After the checks below, which read bearer.
			r.Header.Set("Authorization", "Bearer "+g.PassAs)
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			if !g.Checked(r.URL.Path) || r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := withRouting(r.Context(), r.Header.Get("X-Goog-Request-Params"))
			msg, err := g.firstFrame(r)
			if err == nil && msg != nil {
				err = g.Authorize(ctx, bearer, r.URL.Path, msg)
			}
			if err != nil {
				st := status.Convert(err)
				netfwd.WriteGRPCStatus(w, int(st.Code()), st.Message())
				return
			}
			if resp, ok, err := g.answerTestIAM(ctx, bearer, r.URL.Path, msg); ok {
				if err != nil {
					st := status.Convert(err)
					netfwd.WriteGRPCStatus(w, int(st.Code()), st.Message())
					return
				}
				writeGRPCMessage(w, resp)
				return
			}
			if msg != nil && clientStreaming(r.URL.Path) {
				// Each later message of a client stream (a Firestore
				// Write or Listen) is checked as it arrives.
				g.serveStream(ctx, w, r, next, bearer, msg)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		route, fields, ok := g.restMatcher().MatchRoute(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		full := "/" + route.Service + "/" + route.MethodName
		msg := Input(full)
		if !g.Checked(full) || msg == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if db := r.URL.Query().Get("databaseId"); db != "" {
			ctx = withRouting(ctx, "database_id="+url.QueryEscape(db))
		}
		if route.Body != "" && r.Body != nil {
			b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			var named struct {
				DatabaseID string `json:"databaseId"`
			}
			if json.Unmarshal(b, &named) == nil && named.DatabaseID != "" {
				ctx = withRouting(ctx, "database_id="+url.QueryEscape(named.DatabaseID))
			}
			if err == nil && len(b) > 0 {
				target := msg
				if route.Body != "*" {
					if fd := msg.Descriptor().Fields().ByName(protoreflect.Name(route.Body)); fd != nil && fd.Kind() == protoreflect.MessageKind {
						target = msg.Mutable(fd).Message()
					}
				}
				_ = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, target.Interface())
			}
		}
		for path, v := range fields {
			setString(msg, path, v)
		}
		if resp, ok, err := g.answerTestIAM(ctx, bearer, full, msg); ok {
			if err != nil {
				writeJSON(w, err)
				return
			}
			writeJSONMessage(w, resp)
			return
		}
		if err := g.Authorize(ctx, bearer, full, msg); err != nil {
			writeJSON(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// firstFrame reads a gRPC request's first message, putting it back.
func (g *Gate) firstFrame(r *http.Request) (protoreflect.Message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r.Body, hdr[:]); err != nil {
		// No message (or a broken stream): the emulator answers it.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(hdr[:0]), r.Body), r.Body}
		return nil, nil
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxBody {
		return nil, status.Errorf(codes.ResourceExhausted, "request message of %d bytes is too large to check", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return nil, status.Errorf(codes.Internal, "read the request: %v", err)
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(hdr[:]), bytes.NewReader(body), r.Body), r.Body}
	return decodeFrame(r.URL.Path, r.Header.Get("Grpc-Encoding"), hdr[0], body)
}

// decodeFrame is a gRPC message of full method's request type, from its
// frame's flag and bytes; nil for a message it cannot decode, which the
// emulator answers.
func decodeFrame(full, enc string, flag byte, body []byte) (protoreflect.Message, error) {
	if flag == 1 {
		if enc != "gzip" {
			return nil, status.Errorf(codes.Unimplemented, "a request compressed with %q cannot be checked", enc)
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decompress the request: %v", err)
		}
		if body, err = io.ReadAll(io.LimitReader(zr, maxBody)); err != nil {
			return nil, status.Errorf(codes.Internal, "decompress the request: %v", err)
		}
	}
	msg := Input(full)
	if msg == nil {
		return nil, nil
	}
	if err := proto.Unmarshal(body, msg.Interface()); err != nil {
		return nil, nil
	}
	return msg, nil
}

// StreamInterceptor checks the first request of each call on a gRPC
// server whose codec passes raw frames (decode turns a received frame
// into the method's request; the frame is replayed to the handler).
func (g *Gate) StreamInterceptor(newFrame func() any, decode func(frame any) []byte, replay func(frame []byte, v any) error) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !g.Enabled() || !g.Checked(info.FullMethod) {
			return handler(srv, ss)
		}
		fr := newFrame()
		if err := ss.RecvMsg(fr); err != nil {
			if err == io.EOF {
				return handler(srv, &replayed{ServerStream: ss, eof: true})
			}
			return err
		}
		raw := decode(fr)
		msg := Input(info.FullMethod)
		if msg != nil && proto.Unmarshal(raw, msg.Interface()) == nil {
			if resp, ok, err := g.answerTestIAM(ss.Context(), BearerOf(ss.Context()), info.FullMethod, msg); ok {
				if err != nil {
					return err
				}
				return ss.SendMsg(resp)
			}
			if err := g.Authorize(ss.Context(), BearerOf(ss.Context()), info.FullMethod, msg); err != nil {
				return err
			}
		}
		return handler(srv, &replayed{ServerStream: ss, first: raw, has: true, replay: replay})
	}
}

// replayed is a server stream whose first RecvMsg answers the frame the
// gate read.
type replayed struct {
	grpc.ServerStream
	first  []byte
	has    bool
	eof    bool
	replay func([]byte, any) error
}

func (s *replayed) RecvMsg(v any) error {
	if s.eof {
		return io.EOF
	}
	if s.has {
		s.has = false
		return s.replay(s.first, v)
	}
	return s.ServerStream.RecvMsg(v)
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

// writeJSON writes err as Google's JSON error envelope.
func writeJSON(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.PermissionDenied:
		code = http.StatusForbidden
	case codes.Unauthenticated:
		code = http.StatusUnauthorized
	case codes.Unavailable:
		code = http.StatusServiceUnavailable
	}
	details := []json.RawMessage{}
	for _, d := range st.Proto().GetDetails() {
		if b, err := protojson.Marshal(d); err == nil {
			details = append(details, b)
		}
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": st.Message(),
		"status": rpccode.Code(st.Code()).String(), "details": details}})
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
