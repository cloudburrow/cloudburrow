// Package networkconnectivity serves the Network Connectivity API v1's
// service connectivity automation (networkconnectivity.googleapis.com,
// #2141): service connection policies, service connection maps, service
// classes and service connection tokens (google.cloud.networkconnectivity.v1
// CrossNetworkAutomationService), with their operations and locations,
// over gRPC and JSON as Google serves them.
//
// A service connection policy lets a producer service make Private
// Service Connect endpoints in a consumer's VPC network; a service
// connection map is the producer's side, naming its service attachments
// and the consumer networks it should reach. When a map's consumer
// network has a policy for the map's service class in the map's region,
// an endpoint (a Compute Engine forwarding rule targeting the attachment)
// is made in one of the policy's subnets, and listed on both: the map's
// consumerPscConnections and the policy's pscConnections. Cloud SQL's
// automatic endpoints (pscAutoConnections) are such a map, made by the
// Cloud SQL Admin API for an instance (Producer). The endpoints are real:
// a VM of the network reaches the producer through them (internal/service/compute).
//
// Network Connectivity Center (hubs and spokes), policy-based routes,
// internal ranges and data transfer are not served: their methods answer
// UNIMPLEMENTED.
//
// Sources (re-read 2026-11-22): the REST reference
// (https://docs.cloud.google.com/network-connectivity/docs/reference/networkconnectivity/rest/v1/projects.locations.serviceConnectionPolicies,
// .../serviceConnectionMaps, .../serviceClasses), and
// https://docs.cloud.google.com/vpc/docs/about-service-connection-policies and
// https://docs.cloud.google.com/vpc/docs/configure-service-connection-policies.
package networkconnectivity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Compute is the Compute Engine API the endpoints are made in. It is
// called with the server's lock held, and must not call back into it.
type Compute interface {
	// HasNetwork reports whether the network ref exists, in project when
	// ref is a bare name.
	HasNetwork(ref, project string) bool
	// Subnetwork is the network of subnetwork ref in region, and false
	// when there is none.
	Subnetwork(ref, project, region string) (network string, ok bool)
	// CreateAutoEndpoint makes a Private Service Connect endpoint.
	CreateAutoEndpoint(e Endpoint) (EndpointResult, error)
	// DeleteAutoEndpoint removes one CreateAutoEndpoint made.
	DeleteAutoEndpoint(forwardingRule string) error
}

// Endpoint is an endpoint automation asks Compute Engine for.
type Endpoint struct {
	Project, Region, Name, Network string
	Subnetworks                    []string
	Target                         string
	GlobalAccess                   bool
	Description                    string
}

// EndpointResult is the endpoint made.
type EndpointResult struct {
	ForwardingRule, IP, Subnetwork, PSCConnectionID, Operation string
}

// ErrNoAddress is the error CreateAutoEndpoint answers when none of the
// policy's subnets has an address free.
var ErrNoAddress = errors.New("no subnetwork of the policy has a free address")

// Server is the Network Connectivity API's service connectivity
// automation.
type Server struct {
	ncpb.UnimplementedCrossNetworkAutomationServiceServer

	state   string
	now     func() time.Time
	compute func() Compute

	mu       sync.Mutex
	policies map[string]*ncpb.ServiceConnectionPolicy
	maps     map[string]*ncpb.ServiceConnectionMap
	classes  map[string]*ncpb.ServiceClass
	tokens   map[string]*ncpb.ServiceConnectionToken
	ops      map[string]*longrunningpb.Operation
}

// Options configure a Server.
type Options struct {
	// State is the JSON file the resources are kept in; "" keeps them in
	// memory.
	State string
	// Compute is the Compute Engine API; nil (or a nil answer) checks no
	// network and makes no endpoint.
	Compute func() Compute
	// Now is time.Now when nil.
	Now func() time.Time
}

// NewServer returns a server with its state restored.
func NewServer(o Options) (*Server, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{state: o.State, now: o.Now, compute: o.Compute}
	s.clear()
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) clear() {
	s.policies = map[string]*ncpb.ServiceConnectionPolicy{}
	s.maps = map[string]*ncpb.ServiceConnectionMap{}
	s.classes = map[string]*ncpb.ServiceClass{}
	s.tokens = map[string]*ncpb.ServiceConnectionToken{}
	s.ops = map[string]*longrunningpb.Operation{}
}

// computeAPI is Compute Engine, or nil.
func (s *Server) computeAPI() Compute {
	if s.compute == nil {
		return nil
	}
	return s.compute()
}

// disk is the state file: each resource in its JSON form.
type disk struct {
	Policies map[string]json.RawMessage `json:"policies,omitempty"`
	Maps     map[string]json.RawMessage `json:"maps,omitempty"`
	Classes  map[string]json.RawMessage `json:"classes,omitempty"`
	Tokens   map[string]json.RawMessage `json:"tokens,omitempty"`
	Ops      map[string]json.RawMessage `json:"operations,omitempty"`
}

func encodeAll[T proto.Message](m map[string]T) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		b, err := protojson.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, nil
}

func decodeAll[T proto.Message](in map[string]json.RawMessage, mk func() T, out map[string]T) error {
	for k, b := range in {
		v := mk()
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return nil
}

func (s *Server) restore() error {
	if s.state == "" {
		return nil
	}
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Network Connectivity state: %w", err)
	}
	var d disk
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("networkconnectivity: %s: %w", s.state, err)
	}
	for _, step := range []error{
		decodeAll(d.Policies, func() *ncpb.ServiceConnectionPolicy { return &ncpb.ServiceConnectionPolicy{} }, s.policies),
		decodeAll(d.Maps, func() *ncpb.ServiceConnectionMap { return &ncpb.ServiceConnectionMap{} }, s.maps),
		decodeAll(d.Classes, func() *ncpb.ServiceClass { return &ncpb.ServiceClass{} }, s.classes),
		decodeAll(d.Tokens, func() *ncpb.ServiceConnectionToken { return &ncpb.ServiceConnectionToken{} }, s.tokens),
		decodeAll(d.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} }, s.ops),
	} {
		if step != nil {
			return fmt.Errorf("networkconnectivity: %s: %w", s.state, step)
		}
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.state == "" {
		return nil
	}
	var d disk
	var err error
	if d.Policies, err = encodeAll(s.policies); err != nil {
		return err
	}
	if d.Maps, err = encodeAll(s.maps); err != nil {
		return err
	}
	if d.Classes, err = encodeAll(s.classes); err != nil {
		return err
	}
	if d.Tokens, err = encodeAll(s.tokens); err != nil {
		return err
	}
	if d.Ops, err = encodeAll(s.ops); err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// ResetProject forgets one project's resources, or every project's for
// "", removing the endpoints its maps made.
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "projects/" + project + "/"
	in := func(k string) bool { return project == "" || strings.HasPrefix(k, prefix) }
	c := s.computeAPI()
	for k, m := range s.maps {
		if in(k) {
			for _, cn := range m.GetConsumerPscConnections() {
				if c != nil && cn.GetForwardingRule() != "" {
					_ = c.DeleteAutoEndpoint(cn.GetForwardingRule())
				}
			}
			delete(s.maps, k)
		}
	}
	for k := range s.policies {
		if in(k) {
			delete(s.policies, k)
		}
	}
	for k := range s.classes {
		if in(k) {
			delete(s.classes, k)
		}
	}
	for k := range s.tokens {
		if in(k) {
			delete(s.tokens, k)
		}
	}
	for k := range s.ops {
		if in(k) {
			delete(s.ops, k)
		}
	}
	s.reconcileLocked()
	return s.saveLocked()
}

// Names.

var (
	parentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	idRE     = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// Locations are the locations served: global and Google Cloud's regions.
func Locations() []string { return append([]string{"global"}, locations.Regions...) }

func validRegion(l string) bool {
	for _, r := range locations.Regions {
		if r == l {
			return true
		}
	}
	return false
}

// parent splits projects/P/locations/L, refusing a location that is not a
// region: service connectivity automation is regional.
func parent(field, name string) (project, location string, err error) {
	m := parentRE.FindStringSubmatch(name)
	if m == nil {
		return "", "", apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, name)
	}
	if !validRegion(m[2]) {
		return "", "", apierror.InvalidArgument("location %q is not a valid region for this resource: service connectivity automation resources are regional", m[2])
	}
	return m[1], m[2], nil
}

// child checks name is {parent}/{collection}/{id}.
func child(name, collection string) (parentName, id string, err error) {
	i := strings.LastIndex(name, "/"+collection+"/")
	if i < 0 {
		return "", "", apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/%s/{id}", name, collection)
	}
	parentName, id = name[:i], name[i+len(collection)+2:]
	if _, _, err := parent("name", parentName); err != nil {
		return "", "", err
	}
	if id == "" || strings.Contains(id, "/") {
		return "", "", apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/%s/{id}", name, collection)
	}
	return parentName, id, nil
}

func checkID(field, id string) error {
	if id == "" {
		return apierror.InvalidArgument("%s is required", field)
	}
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be 1 to 63 characters: a lowercase letter, then lowercase letters, digits or hyphens, not ending in a hyphen (RFC 1035)", field, id)
	}
	return nil
}

// computeRel is a Compute Engine reference as a relative name: a full URL
// (https://www.googleapis.com/compute/v1/projects/...) or
// //compute.googleapis.com/projects/... loses its prefix.
func computeRel(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "projects/"); i > 0 {
		return ref[i:]
	}
	return ref
}

// networkRel is a network reference's relative name, and false when it is
// not one.
func networkRel(ref string) (string, bool) {
	rel := computeRel(ref)
	p := strings.Split(rel, "/")
	if len(p) != 5 || p[0] != "projects" || p[2] != "global" || p[3] != "networks" || p[1] == "" || p[4] == "" {
		return "", false
	}
	return rel, true
}

func newEtag() *string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	e := hex.EncodeToString(b[:])
	return &e
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// checkEtag refuses a stale etag, as AIP-154 has it.
func checkEtag(sent *string, have *string) error {
	if sent == nil || *sent == "" || have == nil {
		return nil
	}
	if *sent != *have {
		return apierror.Aborted("the etag %q is not the resource's current etag: read it again and retry", *sent)
	}
	return nil
}

func sortedKeys[T any](m map[string]T, prefix string) []string {
	out := []string{}
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }
