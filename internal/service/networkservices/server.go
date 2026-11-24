// Package networkservices serves the parts of the Network Services API v1
// (networkservices.googleapis.com, #2160) that Secure Web Proxy, Service
// Extensions and proxyless gRPC servers need, in the CLI process, on the Compute Engine API's
// port, over JSON as Google serves it and over gRPC: gateways (a
// SECURE_WEB_GATEWAY gateway is a real forward proxy here, proxy.go; an
// OPEN_MESH gateway is kept for Traffic Director), authorization
// extensions (authzExtensions, which an authorization policy's CUSTOM
// action names), endpoint policies (endpointpolicies.go), their
// long-running operations and the locations. The
// API's other collections answer UNIMPLEMENTED.
//
// It follows https://docs.cloud.google.com/secure-web-proxy/docs/quickstart,
// .../secure-web-proxy/docs/initial-setup-steps,
// .../secure-web-proxy/docs/quotas, .../service-extensions/docs/overview and
// the REST reference (read 2026-11-22).
//
// Every create, update and delete is a long-running operation, done at
// once.
package networkservices

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Options configure a Server.
type Options struct {
	// State is the JSON file the resources are kept in; empty keeps them in
	// memory.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
	// Bind is the host address the gateways' listeners are on; "" is
	// 127.0.0.1.
	Bind string
}

// Hooks are what the API reads from the services beside it; each may be
// nil, and then accepts everything.
type Hooks struct {
	// GatewaySecurityPolicyExists is whether a Network Security gateway
	// security policy exists (projects/P/locations/L/gatewaySecurityPolicies/X).
	GatewaySecurityPolicyExists func(name string) bool
	// ServerTLSPolicyExists is whether a Network Security server TLS
	// policy exists; NetSecPolicyExists whether any Network Security
	// policy (server TLS, client TLS, authorization) exists.
	ServerTLSPolicyExists func(name string) bool
	NetSecPolicyExists    func(name string) bool
	// CertificateExists is whether a Certificate Manager certificate
	// exists (projects/P/locations/L/certificates/C).
	CertificateExists func(name string) bool
	// Network is whether a VPC network exists (projects/P/global/networks/N);
	// Subnetwork is a subnetwork's network and whether it exists
	// (projects/P/regions/R/subnetworks/S).
	Network    func(rel string) bool
	Subnetwork func(rel string) (network string, ok bool)
	// ProxyOnlySubnet is whether network has an ACTIVE proxy-only subnet
	// (purpose REGIONAL_MANAGED_PROXY) in region.
	ProxyOnlySubnet func(network, region string) bool
	// BackendService is a backend service's load balancing scheme, by its
	// relative name; ok is false when there is none.
	BackendService func(rel string) (scheme string, ok bool)
}

// Server is the Network Services API.
type Server struct {
	nsvpb.UnimplementedNetworkServicesServer
	nsvpb.UnimplementedDepServiceServer
	opts Options

	mu    sync.Mutex
	res   map[string]proto.Message
	ops   map[string]*longrunningpb.Operation
	hooks Hooks
	proxy *proxyPlane
	// subnetRange allocates a gateway's address (UseSubnetRange).
	subnetRange SubnetRange
}

// New returns a Server with its state restored. Start opens the
// gateways' listeners.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Bind == "" {
		opts.Bind = "127.0.0.1"
	}
	s := &Server{opts: opts, res: map[string]proto.Message{}, ops: map[string]*longrunningpb.Operation{}}
	s.proxy = newProxyPlane(s)
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

// UseHooks sets the hooks after New.
func (s *Server) UseHooks(h Hooks) {
	s.mu.Lock()
	s.hooks = h
	s.mu.Unlock()
}

func (s *Server) hooksNow() Hooks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hooks
}

// The collections served, by their path segment.
const (
	collGateways = "gateways"
	collAuthzExt = "authzExtensions"
)

// colls are every collection of the API: the two served, and the rest,
// which answer UNIMPLEMENTED on this port rather than Compute Engine's
// 404.
var colls = map[string]bool{collGateways: true, collAuthzExt: true, collEndpointPolicies: true, "wasmPlugins": true, "grpcRoutes": true, "httpRoutes": true, "tcpRoutes": true,
	"tlsRoutes": true, "serviceBindings": true, "meshes": true, "serviceLbPolicies": true, "agentGateways": true,
	"lbTrafficExtensions": true, "lbRouteExtensions": true, "lbEdgeExtensions": true, "routeViews": true}

var noun = map[string]string{collGateways: "Gateway", collAuthzExt: "AuthzExtension", collEndpointPolicies: "EndpointPolicy"}

var (
	parentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	// idRE is the IDs' rule: "1-63 characters long, containing only
	// letters, numbers, hyphens, and underscores, and should not start
	// with a number".
	idRE = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]{0,62}$`)
)

// Locations are the locations served: global and Compute Engine's regions.
func Locations() []string { return append([]string{"global"}, locations.ComputeRegions()...) }

func validLocation(l string) bool { return l == "global" || locations.IsComputeRegion(l) }

func parseParent(parent string) (project, location string, err error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("Invalid resource field value in the request: parent %q must be projects/{project}/locations/{location}.", parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("Location '%s' is not supported.", m[2])
	}
	return m[1], m[2], nil
}

func checkName(name, coll string) error {
	p := strings.Split(name, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != "locations" || p[4] != coll || p[5] == "" {
		return apierror.InvalidArgument("Invalid resource field value in the request: name %q is not a %s name.", name, noun[coll])
	}
	return nil
}

func checkID(field, id string) error {
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("Invalid value for field '%s': '%s'. It should be 1-63 characters long, containing only letters, numbers, hyphens, and underscores, and should not start with a number.", field, id)
	}
	return nil
}

func projectOf(name string) string  { return strings.Split(name, "/")[1] }
func locationOf(name string) string { return strings.Split(name, "/")[3] }

func notFound(coll, name string) error {
	return apierror.NotFound("%s '%s' not found.", noun[coll], name)
}

func stamp(m proto.Message, now time.Time, create bool) {
	r := m.ProtoReflect()
	fd := r.Descriptor().Fields()
	if create {
		if f := fd.ByName("create_time"); f != nil {
			r.Set(f, protoreflect.ValueOfMessage(timestamppb.New(now).ProtoReflect()))
		}
	}
	if f := fd.ByName("update_time"); f != nil {
		r.Set(f, protoreflect.ValueOfMessage(timestamppb.New(now).ProtoReflect()))
	}
}

func setName(m proto.Message, name string) {
	r := m.ProtoReflect()
	r.Set(r.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString(name))
}

func (s *Server) create(parent, coll, idField, id string, m proto.Message, check func(m proto.Message, name string) error) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent(parent); err != nil {
		return nil, err
	}
	if err := checkID(idField, id); err != nil {
		return nil, err
	}
	name := parent + "/" + coll + "/" + id
	setName(m, name)
	if err := check(m, name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("%s '%s' already exists.", noun[coll], name)
	}
	stamp(m, s.opts.Now(), true)
	s.res[name] = m
	op, err := s.opLocked(name, "create", m)
	s.changedLocked()
	return op, err
}

func (s *Server) get(coll, name string) (proto.Message, error) {
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.res[name]
	if !ok {
		return nil, notFound(coll, name)
	}
	return proto.Clone(m), nil
}

func (s *Server) list(parent, coll string) ([]proto.Message, error) {
	all := strings.HasSuffix(parent, "/locations/-")
	if all {
		if p := strings.TrimSuffix(parent, "/locations/-"); !strings.HasPrefix(p, "projects/") || strings.Contains(p[len("projects/"):], "/") {
			return nil, apierror.InvalidArgument("Invalid resource field value in the request: parent %q must be projects/{project}/locations/{location}.", parent)
		}
	} else if _, _, err := parseParent(parent); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.res {
		p := strings.Split(k, "/")
		if p[4] != coll {
			continue
		}
		if all && strings.HasPrefix(k, strings.TrimSuffix(parent, "/locations/-")+"/locations/") || !all && strings.Join(p[:4], "/") == parent {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]proto.Message, 0, len(keys))
	for _, k := range keys {
		out = append(out, proto.Clone(s.res[k]))
	}
	return out, nil
}

func (s *Server) update(coll string, in proto.Message, mask *fieldmaskpb.FieldMask, allowed []string, check func(m proto.Message, name string) error) (*longrunningpb.Operation, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("Invalid argument: the %s is required.", noun[coll])
	}
	r := in.ProtoReflect()
	name := r.Get(r.Descriptor().Fields().ByName("name")).String()
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	s.mu.Lock()
	cur, ok := s.res[name]
	s.mu.Unlock()
	if !ok {
		return nil, notFound(coll, name)
	}
	next := proto.Clone(cur)
	paths := allowed
	if mask != nil && len(mask.GetPaths()) > 0 {
		paths = nil
		for _, raw := range mask.GetPaths() {
			for _, p := range strings.Split(raw, ",") {
				paths = append(paths, camelToSnake(strings.TrimSpace(p)))
			}
		}
	}
	ok2 := map[string]bool{}
	for _, a := range allowed {
		ok2[a] = true
	}
	d := next.ProtoReflect()
	for _, p := range paths {
		if !ok2[p] {
			return nil, apierror.InvalidArgument("Invalid update_mask path '%s': the field cannot be updated. Updatable fields are: %s.", p, strings.Join(allowed, ", "))
		}
		f := d.Descriptor().Fields().ByName(protoreflect.Name(p))
		if r.Has(f) {
			d.Set(f, r.Get(f))
		} else {
			d.Clear(f)
		}
	}
	if err := check(next, name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp(next, s.opts.Now(), false)
	s.res[name] = next
	op, err := s.opLocked(name, "update", next)
	s.changedLocked()
	return op, err
}

func (s *Server) delete(coll, name string) (*longrunningpb.Operation, error) {
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; !ok {
		return nil, notFound(coll, name)
	}
	delete(s.res, name)
	op, err := s.opLocked(name, "delete", nil)
	s.changedLocked()
	return op, err
}

func camelToSnake(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) opLocked(target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := s.opts.Now()
	ts := timestamppb.New(now)
	md, err := anypb.New(&nsvpb.OperationMetadata{CreateTime: ts, EndTime: ts, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Internal(err, "operation metadata")
	}
	if resp == nil {
		resp = &emptypb.Empty{}
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Internal(err, "operation response")
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("projects/%s/locations/%s/operations/operation-%d-%s", projectOf(target), locationOf(target), now.UnixMilli(), hex.EncodeToString(b[:]))
	op := &longrunningpb.Operation{Name: name, Metadata: md, Done: true, Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[name] = op
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

// HasOperation is whether name is one of this API's operations.
func (s *Server) HasOperation(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ops[name]
	return ok
}

// ResetProject forgets a project's resources and operations, or every
// project's when project is "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.res {
		if project == "" || projectOf(k) == project {
			delete(s.res, k)
		}
	}
	for k := range s.ops {
		if project == "" || projectOf(k) == project {
			delete(s.ops, k)
		}
	}
	s.changedLocked()
	return s.saveLocked()
}

type state struct {
	Resources  map[string]json.RawMessage `json:"resources"`
	Operations map[string]json.RawMessage `json:"operations"`
	// Listeners are each gateway port's host address, kept across
	// restarts.
	Listeners map[string]string `json:"listeners,omitempty"`
}

func newOf(name string) proto.Message {
	p := strings.Split(name, "/")
	if len(p) != 6 {
		return nil
	}
	switch p[4] {
	case collGateways:
		return &nsvpb.Gateway{}
	case collAuthzExt:
		return &nsvpb.AuthzExtension{}
	case collEndpointPolicies:
		return &nsvpb.EndpointPolicy{}
	}
	return nil
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("networkservices: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("networkservices: %s: %w", s.opts.State, err)
	}
	for k, raw := range st.Resources {
		m := newOf(k)
		if m == nil {
			continue
		}
		if err := protojson.Unmarshal(raw, m); err != nil {
			return fmt.Errorf("networkservices: %s: %w", k, err)
		}
		s.res[k] = m
	}
	for k, raw := range st.Operations {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("networkservices: operation %s: %w", k, err)
		}
		s.ops[k] = op
	}
	for k, v := range st.Listeners {
		s.proxy.prev[k] = v
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	st := state{Resources: map[string]json.RawMessage{}, Operations: map[string]json.RawMessage{}, Listeners: s.proxy.addrsSnapshot()}
	for k, m := range s.res {
		b, err := protojson.Marshal(m)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Resources[k] = b
	}
	for k, op := range s.ops {
		b, err := protojson.Marshal(op)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Operations[k] = b
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o755); err != nil {
		return apierror.Internal(err, "save")
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.Rename(tmp, s.opts.State); err != nil {
		return apierror.Internal(err, "save")
	}
	return nil
}
