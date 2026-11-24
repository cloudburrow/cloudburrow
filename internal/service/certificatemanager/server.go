// Package certificatemanager serves the Certificate Manager API v1
// (certificatemanager.googleapis.com, #2160) in the CLI process, on the
// Compute Engine API's port, over JSON as Google serves it: certificates,
// certificate maps and their entries, DNS authorizations, certificate
// issuance configs and trust configs, their long-running operations and
// the locations.
//
// Each resource is checked as Google's REST reference and limits describe
// it (https://docs.cloud.google.com/certificate-manager/docs/reference/certificate-manager/rest,
// .../certificate-manager/docs/quotas, .../dns-authorizations, read
// 2026-11-22):
//
//   - a self-managed certificate is its PEM chain and key, checked as a
//     pair of one of the documented key types (RSA-2048, RSA-3072,
//     RSA-4096, ECDSA P-256, ECDSA P-384); the key is input only;
//   - a Google-managed certificate names its domains and is authorized by
//     DNS authorizations (up to 100 domains, wildcards allowed), by the
//     load balancer (up to 5, no wildcards, global only) or issued through
//     a certificate issuance config (private PKI). No public CA can issue a
//     certificate for this machine, so each is issued by CloudBurrow's own
//     load-balancing CA (compute.Server.IssueLocalCertificate, the CA
//     served at /cloudburrow/lb/ca.pem), an issuance config's CA Service
//     pool too: it is ACTIVE at once with DNS authorization (no DNS zone is
//     looked up for the CNAME) or an issuance config, and with load
//     balancer authorization once a target proxy that a forwarding rule
//     serves uses a map that names it, as Google provisions it only then;
//   - a certificate map's entries choose the certificates a TLS handshake
//     gets by its SNI (hostname, exact or wildcard) or, failing that, the
//     PRIMARY entry; the load balancers' listeners read them through
//     MapCertificates (compute.CertificateMaps).
//
// Every create, update and delete is a long-running operation, done at
// once.
package certificatemanager

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

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
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

// Issuer issues a certificate for domains from the local CA:
// compute.Server.IssueLocalCertificate.
type Issuer interface {
	IssueLocalCertificate(domains []string) (chainPEM, keyPEM string, expire time.Time, err error)
}

// Target is a target HTTPS or SSL proxy that names a certificate map, and
// the addresses its forwarding rules serve it on: a map's gclbTargets.
type Target struct {
	// Map is the map's name (projects/P/locations/global/certificateMaps/M).
	Map string
	// HTTPSProxy or SSLProxy is the proxy's relative name
	// (projects/P/global/targetHttpsProxies/X).
	HTTPSProxy, SSLProxy string
	// IPs are its forwarding rules' addresses and ports.
	IPs []IPConfig
}

// IPConfig is one address a target is served on.
type IPConfig struct {
	IP    string
	Ports []uint32
}

// Options configure a Server.
type Options struct {
	// State is the JSON file the resources are kept in; empty keeps them in
	// memory.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
}

// Server is the Certificate Manager API.
type Server struct {
	cmpb.UnimplementedCertificateManagerServer
	opts Options

	mu sync.Mutex
	// res are every resource, by name; keys the self-managed and issued
	// certificates' private keys (input only), by certificate.
	res  map[string]proto.Message
	keys map[string]string
	ops  map[string]*longrunningpb.Operation

	issuer  Issuer
	targets func(project string) []Target
	// issuing are the certificates being issued, outside the lock.
	issuing map[string]bool
	// trustUsers names what uses a trust config (UseTrustUsers).
	trustUsers func(name string) []string
	// certUsers names what uses a certificate beside the target proxies
	// (a backend authentication config; UseCertificateUsers).
	certUsers func(name string) []string
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, res: map[string]proto.Message{}, keys: map[string]string{}, ops: map[string]*longrunningpb.Operation{}}
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

// UseIssuer sets the CA that Google-managed certificates are issued by.
func (s *Server) UseIssuer(i Issuer) {
	s.mu.Lock()
	s.issuer = i
	s.mu.Unlock()
}

// UseTargets sets what lists the target proxies that name maps.
func (s *Server) UseTargets(f func(project string) []Target) {
	s.mu.Lock()
	s.targets = f
	s.mu.Unlock()
}

// The collections, by their path segment.
const (
	collCerts   = "certificates"
	collMaps    = "certificateMaps"
	collEntries = "certificateMapEntries"
	collDNS     = "dnsAuthorizations"
	collIssue   = "certificateIssuanceConfigs"
	collTrust   = "trustConfigs"
)

var (
	parentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	// idRE is the resource IDs' rule: a lowercase letter, then up to 62
	// lowercase letters, digits and hyphens, not ending with a hyphen.
	idRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// Locations are the locations served: global and Compute Engine's regions.
func Locations() []string { return append([]string{"global"}, compute.Regions()...) }

func validLocation(l string) bool {
	if l == "global" {
		return true
	}
	return compute.IsRegion(l)
}

// parseListParent is parseParent for a list, which also takes the
// location "-", every location (AIP-159), as the console lists them.
func parseListParent(parent string) (project, location string, err error) {
	if p := strings.TrimSuffix(parent, "/locations/-"); p != parent && strings.HasPrefix(p, "projects/") && !strings.Contains(p[len("projects/"):], "/") {
		return p[len("projects/"):], "-", nil
	}
	return parseParent(parent, false)
}

// parseParent checks a collection's parent, projects/P/locations/L.
func parseParent(parent string, globalOnly bool) (project, location string, err error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("Invalid resource field value in the request: parent %q must be projects/{project}/locations/{location}.", parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("Location '%s' is not supported.", m[2])
	}
	if globalOnly && m[2] != "global" {
		return "", "", apierror.InvalidArgument("Location '%s' is not supported for this resource: only 'global' is.", m[2])
	}
	return m[1], m[2], nil
}

// checkName checks an item's name: projects/P/locations/L/<coll>/ID (and,
// for a map entry, projects/P/locations/L/certificateMaps/M/certificateMapEntries/ID).
func checkName(name, coll string) error {
	p := strings.Split(name, "/")
	want := 6
	if coll == collEntries {
		want = 8
	}
	if len(p) != want || p[0] != "projects" || p[2] != "locations" || p[len(p)-2] != coll || p[len(p)-1] == "" {
		return apierror.InvalidArgument("Invalid resource field value in the request: name %q is not a %s name.", name, strings.TrimSuffix(coll, "s"))
	}
	if coll == collEntries && (p[4] != collMaps || p[5] == "") {
		return apierror.InvalidArgument("Invalid resource field value in the request: name %q is not a certificate map entry name.", name)
	}
	return nil
}

func checkID(field, id string) error {
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("Invalid value for field '%s': '%s'. It must be 1 to 63 characters long, start with a lowercase letter, and contain only lowercase letters, digits and hyphens, not ending with a hyphen.", field, id)
	}
	return nil
}

func projectOf(name string) string  { return strings.Split(name, "/")[1] }
func locationOf(name string) string { return strings.Split(name, "/")[3] }

// noun is a collection's resource in Google's messages.
var noun = map[string]string{collCerts: "Certificate", collMaps: "CertificateMap", collEntries: "CertificateMapEntry",
	collDNS: "DnsAuthorization", collIssue: "CertificateIssuanceConfig", collTrust: "TrustConfig"}

func notFound(coll, name string) error {
	return apierror.NotFound("%s '%s' not found.", noun[coll], name)
}

// getLocked is a resource of coll, a copy.
func (s *Server) getLocked(coll, name string) (proto.Message, error) {
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	m, ok := s.res[name]
	if !ok {
		return nil, notFound(coll, name)
	}
	return proto.Clone(m), nil
}

// childrenLocked are the names of parent's coll items, sorted.
func (s *Server) childrenLocked(parent, coll string) []string {
	prefix := parent + "/" + coll + "/"
	var out []string
	for k := range s.res {
		if p := strings.TrimSuffix(parent, "/locations/-"); p != parent {
			// Every location's: projects/P/locations/L/coll/ID.
			rest, ok := strings.CutPrefix(k, p+"/locations/")
			if parts := strings.Split(rest, "/"); ok && len(parts) == 3 && parts[1] == coll {
				out = append(out, k)
			}
			continue
		}
		if strings.HasPrefix(k, prefix) && !strings.Contains(k[len(prefix):], "/") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// stamp sets a new resource's create and update times.
func stamp(m proto.Message, now time.Time, create bool) {
	r := m.ProtoReflect()
	fd := r.Descriptor().Fields()
	ts := timestamppb.New(now).ProtoReflect()
	if create {
		if f := fd.ByName("create_time"); f != nil {
			r.Set(f, protoreflect.ValueOfMessage(ts))
		}
	}
	if f := fd.ByName("update_time"); f != nil {
		r.Set(f, protoreflect.ValueOfMessage(timestamppb.New(now).ProtoReflect()))
	}
}

// applyMask copies the fields that mask names from src into dst; allowed
// are the paths update may change.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask, allowed ...string) error {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return apierror.InvalidArgument("Invalid argument: update_mask is required.")
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	d, sr := dst.ProtoReflect(), src.ProtoReflect()
	for _, raw := range mask.GetPaths() {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			snake := camelToSnake(p)
			if !ok[snake] {
				return apierror.InvalidArgument("Invalid update_mask path '%s': the field cannot be updated. Updatable fields are: %s.", p, strings.Join(allowed, ", "))
			}
			f := d.Descriptor().Fields().ByName(protoreflect.Name(snake))
			if f == nil {
				return apierror.InvalidArgument("Invalid update_mask path '%s'.", p)
			}
			if sr.Has(f) {
				d.Set(f, sr.Get(f))
			} else {
				d.Clear(f)
			}
		}
	}
	return nil
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
	md, err := anypb.New(&cmpb.OperationMetadata{CreateTime: ts, EndTime: ts, Target: target, Verb: verb, ApiVersion: "v1"})
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
	p := strings.Split(target, "/")
	name := fmt.Sprintf("projects/%s/locations/%s/operations/operation-%d-%s", p[1], p[3], now.UnixMilli(), hex.EncodeToString(b[:]))
	op := &longrunningpb.Operation{Name: name, Metadata: md, Done: true, Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[name] = op
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

// HasOperation is whether name is one of this API's operations: the
// Compute Engine port serves Serverless VPC Access's beside them, under the
// same paths.
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
			delete(s.keys, k)
		}
	}
	for k := range s.ops {
		if project == "" || projectOf(k) == project {
			delete(s.ops, k)
		}
	}
	return s.saveLocked()
}

// state is the saved file: each message as its protojson, by collection.
type state struct {
	Resources  map[string]map[string]json.RawMessage `json:"resources"`
	Keys       map[string]string                     `json:"keys,omitempty"`
	Operations map[string]json.RawMessage            `json:"operations"`
}

// newOf is an empty message of a resource name's collection.
func newOf(name string) proto.Message {
	p := strings.Split(name, "/")
	if len(p) < 2 {
		return nil
	}
	switch p[len(p)-2] {
	case collCerts:
		return &cmpb.Certificate{}
	case collMaps:
		return &cmpb.CertificateMap{}
	case collEntries:
		return &cmpb.CertificateMapEntry{}
	case collDNS:
		return &cmpb.DnsAuthorization{}
	case collIssue:
		return &cmpb.CertificateIssuanceConfig{}
	case collTrust:
		return &cmpb.TrustConfig{}
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
		return fmt.Errorf("certificatemanager: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("certificatemanager: %s: %w", s.opts.State, err)
	}
	for _, items := range st.Resources {
		for k, raw := range items {
			m := newOf(k)
			if m == nil {
				continue
			}
			if err := protojson.Unmarshal(raw, m); err != nil {
				return fmt.Errorf("certificatemanager: %s: %w", k, err)
			}
			s.res[k] = m
		}
	}
	for k, v := range st.Keys {
		s.keys[k] = v
	}
	for k, raw := range st.Operations {
		op := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, op); err != nil {
			return fmt.Errorf("certificatemanager: operation %s: %w", k, err)
		}
		s.ops[k] = op
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	st := state{Resources: map[string]map[string]json.RawMessage{}, Keys: s.keys, Operations: map[string]json.RawMessage{}}
	for k, m := range s.res {
		b, err := protojson.Marshal(m)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		p := strings.Split(k, "/")
		coll := p[len(p)-2]
		if st.Resources[coll] == nil {
			st.Resources[coll] = map[string]json.RawMessage{}
		}
		st.Resources[coll][k] = b
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
