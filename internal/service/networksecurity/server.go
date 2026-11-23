// Package networksecurity serves the parts of the Network Security API v1
// (networksecurity.googleapis.com, #2160) that Cloud Load Balancing reads,
// in the CLI process, on the Compute Engine API's port, over JSON as
// Google serves it: server TLS policies (frontend mutual TLS), client TLS
// policies, the classic authorization policies and the authorization
// policies of Application Load Balancers (authzPolicies), their
// long-running operations and the locations.
//
// It follows https://docs.cloud.google.com/network-security/docs/reference/rest,
// .../load-balancing/docs/mtls, .../load-balancing/docs/https/setting-up-mtls-global,
// .../load-balancing/docs/auth-policy/auth-policy-overview and the
// targetHttpsProxies and backendServices references (read 2026-11-22):
//
//   - a server TLS policy in the Application Load Balancers' form has an
//     mtlsPolicy with clientValidationMode (ALLOW_INVALID_OR_MISSING_CLIENT_CERT
//     or REJECT_INVALID) and, optionally, clientValidationTrustConfig (a
//     Certificate Manager trust config in the policy's location); it has
//     neither allowOpen nor serverCertificate nor clientValidationCa, which
//     are Traffic Director's form; a target HTTPS proxy's serverTlsPolicy
//     names it, and the load balancers here then ask each client for its
//     certificate and check it against the trust config;
//   - a client TLS policy is kept for a backend service's
//     securitySettings.clientTlsPolicy, which Google applies to
//     INTERNAL_SELF_MANAGED backend services only;
//   - a classic authorization policy (authorizationPolicies) is kept and
//     checked; a target HTTPS proxy's authorizationPolicy "currently has no
//     impact" on Google, and has none here;
//   - an authorization policy of the Application Load Balancers
//     (authzPolicies) targets forwarding rules (EXTERNAL_MANAGED or
//     INTERNAL_MANAGED) and is enforced on each request through them: the
//     CUSTOM policies first (Cloud IAP, or a Service Extensions
//     authorization extension the load balancer calls over ext_proc or
//     ext_authz; a CONTENT_AUTHZ policy's last, with the body), then the
//     DENY policies, then the ALLOW ones (ForwardingRulePolicies); a
//     rule's source resources match the instance whose internal address a
//     request comes from, by its service account and its tags' value IDs.
//
// Every create, update and delete is a long-running operation, done at
// once.
package networksecurity

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
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Options configure a Server.
type Options struct {
	// State is the JSON file the resources are kept in; empty keeps them in
	// memory.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
}

// Hooks are what the API reads from the services beside it; each may be
// nil, and then accepts everything (or finds no user).
type Hooks struct {
	// TrustConfigExists is whether a Certificate Manager trust config
	// exists (projects/P/locations/L/trustConfigs/T).
	TrustConfigExists func(name string) bool
	// ForwardingRule is a forwarding rule's load balancing scheme, by its
	// relative name (projects/P/global/forwardingRules/R or
	// projects/P/regions/R/forwardingRules/F); ok is false when there is
	// none.
	ForwardingRule func(rel string) (scheme string, ok bool)
	// Users are the Compute Engine resources that name a policy (target
	// HTTPS proxies, backend services), which keep it from being deleted.
	Users func(name string) []string
	// AuthzExtension is a Service Extensions authorization extension's
	// load balancing scheme, by its relative name
	// (projects/P/locations/L/authzExtensions/E); ok is false when there is
	// none.
	AuthzExtension func(rel string) (scheme string, ok bool)
}

// Server is the Network Security API.
type Server struct {
	nspb.UnimplementedNetworkSecurityServer
	opts Options

	mu    sync.Mutex
	res   map[string]proto.Message
	ops   map[string]*longrunningpb.Operation
	hooks Hooks
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, res: map[string]proto.Message{}, ops: map[string]*longrunningpb.Operation{}}
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

// The collections, by their path segment.
const (
	collServerTLS = "serverTlsPolicies"
	collClientTLS = "clientTlsPolicies"
	collAuthz     = "authorizationPolicies"
	collAuthzALB  = "authzPolicies"
)

var colls = map[string]bool{collServerTLS: true, collClientTLS: true, collAuthz: true, collAuthzALB: true}

// noun is a collection's resource in Google's messages.
var noun = map[string]string{collServerTLS: "ServerTlsPolicy", collClientTLS: "ClientTlsPolicy",
	collAuthz: "AuthorizationPolicy", collAuthzALB: "AuthzPolicy"}

var (
	parentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	// idRE is the IDs' rule: "1-63 characters long, containing only
	// letters, numbers, hyphens, and underscores, and should not start
	// with a number".
	idRE = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]{0,62}$`)
)

// Locations are the locations served: global and Compute Engine's regions.
func Locations() []string { return append([]string{"global"}, compute.Regions()...) }

func validLocation(l string) bool { return l == "global" || compute.IsRegion(l) }

// parseParent checks a collection's parent, projects/P/locations/L.
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

// checkName checks an item's name, projects/P/locations/L/<coll>/ID.
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

// stamp sets a resource's create and update times.
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

// create adds m (a clone) as parent/coll/id after check.
func (s *Server) create(parent, coll, idField, id string, m proto.Message, check func(m proto.Message, location string) error) (*longrunningpb.Operation, error) {
	_, loc, err := parseParent(parent)
	if err != nil {
		return nil, err
	}
	if err := checkID(idField, id); err != nil {
		return nil, err
	}
	if err := check(m, loc); err != nil {
		return nil, err
	}
	name := parent + "/" + coll + "/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("%s '%s' already exists.", noun[coll], name)
	}
	setName(m, name)
	stamp(m, s.opts.Now(), true)
	s.res[name] = m
	return s.opLocked(name, "create", m)
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

// list are parent's coll items (every location's for "-"), sorted, copies.
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

// update applies in's masked fields to its stored resource after check;
// a missing mask replaces every updatable field, as Google's "If the user
// does not provide a mask then all fields will be overwritten".
func (s *Server) update(coll string, in proto.Message, mask *fieldmaskpb.FieldMask, allowed []string, check func(m proto.Message, location string) error) (*longrunningpb.Operation, error) {
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
	if err := check(next, locationOf(name)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp(next, s.opts.Now(), false)
	s.res[name] = next
	return s.opLocked(name, "update", next)
}

func (s *Server) delete(coll, name string) (*longrunningpb.Operation, error) {
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, ok := s.res[name]
	users := s.hooks.Users
	s.mu.Unlock()
	if !ok {
		return nil, notFound(coll, name)
	}
	if users != nil {
		if u := users(name); len(u) > 0 {
			return nil, apierror.FailedPrecondition("%s '%s' is in use by '%s'.", noun[coll], name, u[0])
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.res, name)
	return s.opLocked(name, "delete", nil)
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

// opLocked records a done operation of verb on target, answering resp,
// and saves.
func (s *Server) opLocked(target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := s.opts.Now()
	ts := timestamppb.New(now)
	md, err := anypb.New(&nspb.OperationMetadata{CreateTime: ts, EndTime: ts, Target: target, Verb: verb, ApiVersion: "v1"})
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

// HasOperation is whether name is one of this API's operations: the
// Compute Engine port serves Certificate Manager's and Serverless VPC
// Access's beside them, under the same paths.
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
	return s.saveLocked()
}

// state is the saved file: each message as its protojson.
type state struct {
	Resources  map[string]json.RawMessage `json:"resources"`
	Operations map[string]json.RawMessage `json:"operations"`
}

func newOf(name string) proto.Message {
	p := strings.Split(name, "/")
	if len(p) != 6 {
		return nil
	}
	switch p[4] {
	case collServerTLS:
		return &nspb.ServerTlsPolicy{}
	case collClientTLS:
		return &nspb.ClientTlsPolicy{}
	case collAuthz:
		return &nspb.AuthorizationPolicy{}
	case collAuthzALB:
		return &nspb.AuthzPolicy{}
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
		return fmt.Errorf("networksecurity: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("networksecurity: %s: %w", s.opts.State, err)
	}
	for k, raw := range st.Resources {
		m := newOf(k)
		if m == nil {
			continue
		}
		if err := protojson.Unmarshal(raw, m); err != nil {
			return fmt.Errorf("networksecurity: %s: %w", k, err)
		}
		s.res[k] = m
	}
	for k, raw := range st.Operations {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("networksecurity: operation %s: %w", k, err)
		}
		s.ops[k] = op
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	st := state{Resources: map[string]json.RawMessage{}, Operations: map[string]json.RawMessage{}}
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
