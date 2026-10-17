package kms

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// The EKM service (google.cloud.kms.v1.EkmService, #1452) is CRUD over
// stored records: EkmConnections and each location's EkmConfig are kept and
// returned as written, and nothing dials the external key manager they name:
// the instance acts as the EKM of every connection (external.go, #1469), so
// an EXTERNAL_VPC key's versions keep their key material locally.
// VerifyConnectivity checks a connection against that local EKM, which is
// always reachable.
// IAM policies on a connection and on a location's EkmConfig are stored on
// their records and never enforced (iam.go, #1469).
// Server certificates are parsed as Google documents (Certificate.parsed and
// the fields only present when it is true). The codes for refused requests
// are UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/ekm_service.proto

const (
	ekmConnectionPrefix = "kms/ekmconnection/"
	ekmConfigPrefix     = "kms/ekmconfig/"
	// maxServerCertificates is "a maximum of 10" (ServiceResolver).
	maxServerCertificates = 10
)

// sdServiceRE is a Service Directory service name.
var sdServiceRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/namespaces/[^/]+/services/[^/]+$`)

type ekmConnection struct {
	Name        string        `json:"name"`
	Created     time.Time     `json:"created"`
	Resolvers   []ekmResolver `json:"serviceResolvers,omitempty"`
	Mode        string        `json:"keyManagementMode,omitempty"`
	CryptoSpace string        `json:"cryptoSpacePath,omitempty"`
	// IAMPolicy is stored, never enforced (#1469), as a key ring's is.
	IAMPolicy *iampolicy.Stored `json:"iamPolicy,omitempty"`
}

type ekmResolver struct {
	Service  string   `json:"serviceDirectoryService"`
	Filter   string   `json:"endpointFilter,omitempty"`
	Hostname string   `json:"hostname"`
	Certs    [][]byte `json:"serverCertificates"`
}

type ekmConfig struct {
	Name    string `json:"name"`
	Default string `json:"defaultEkmConnection,omitempty"`
	// IAMPolicy is stored, never enforced (#1469).
	IAMPolicy *iampolicy.Stored `json:"iamPolicy,omitempty"`
}

// etag is a digest of what is stored, so any change makes a new one. The
// IAM policy is left out: it has its own etag, and setting it changes
// nothing UpdateEkmConnection compares against (#1469).
func (c ekmConnection) etag() string {
	c.IAMPolicy = nil
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (c ekmConnection) mode() kmspb.EkmConnection_KeyManagementMode {
	if c.Mode == "" {
		// "If unset, this defaults to MANUAL."
		return kmspb.EkmConnection_MANUAL
	}
	return kmspb.EkmConnection_KeyManagementMode(kmspb.EkmConnection_KeyManagementMode_value[c.Mode])
}

func certificate(der []byte) *kmspb.Certificate {
	out := &kmspb.Certificate{RawDer: der}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return out
	}
	sum := sha256.Sum256(der)
	out.Parsed = true
	out.Issuer = c.Issuer.String()
	out.Subject = c.Subject.String()
	out.SubjectAlternativeDnsNames = c.DNSNames
	out.NotBeforeTime = timestamppb.New(c.NotBefore)
	out.NotAfterTime = timestamppb.New(c.NotAfter)
	out.SerialNumber = strings.ToUpper(c.SerialNumber.Text(16))
	out.Sha256Fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
	return out
}

func (c ekmConnection) proto() *kmspb.EkmConnection {
	out := &kmspb.EkmConnection{Name: c.Name, CreateTime: timestamppb.New(c.Created), Etag: c.etag(),
		KeyManagementMode: c.mode(), CryptoSpacePath: c.CryptoSpace}
	for _, r := range c.Resolvers {
		pr := &kmspb.EkmConnection_ServiceResolver{ServiceDirectoryService: r.Service, EndpointFilter: r.Filter, Hostname: r.Hostname}
		for _, der := range r.Certs {
			pr.ServerCertificates = append(pr.ServerCertificates, certificate(der))
		}
		out.ServiceResolvers = append(out.ServiceResolvers, pr)
	}
	return out
}

func resolversOf(in []*kmspb.EkmConnection_ServiceResolver) ([]ekmResolver, error) {
	// "Currently, only a single ServiceResolver is supported."
	if len(in) > 1 {
		return nil, apierror.InvalidArgument("ekm_connection.service_resolvers has %d entries: only one is supported", len(in))
	}
	var out []ekmResolver
	for _, r := range in {
		if !sdServiceRE.MatchString(r.GetServiceDirectoryService()) {
			return nil, apierror.InvalidArgument("ekm_connection.service_resolvers.service_directory_service %q must be projects/*/locations/*/namespaces/*/services/*", r.GetServiceDirectoryService())
		}
		if r.GetHostname() == "" {
			return nil, apierror.InvalidArgument("ekm_connection.service_resolvers.hostname is required")
		}
		certs := r.GetServerCertificates()
		if len(certs) == 0 || len(certs) > maxServerCertificates {
			return nil, apierror.InvalidArgument("ekm_connection.service_resolvers.server_certificates must have 1 to %d certificates, not %d", maxServerCertificates, len(certs))
		}
		res := ekmResolver{Service: r.GetServiceDirectoryService(), Filter: r.GetEndpointFilter(), Hostname: r.GetHostname()}
		for _, c := range certs {
			if len(c.GetRawDer()) == 0 {
				return nil, apierror.InvalidArgument("ekm_connection.service_resolvers.server_certificates.raw_der is required")
			}
			res.Certs = append(res.Certs, c.GetRawDer())
		}
		out = append(out, res)
	}
	return out, nil
}

func checkCryptoSpace(c ekmConnection) error {
	if c.mode() == kmspb.EkmConnection_CLOUD_KMS && c.CryptoSpace == "" {
		return apierror.InvalidArgument("ekm_connection.crypto_space_path is required when key_management_mode is CLOUD_KMS")
	}
	return nil
}

func parseEkmConnection(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "ekmConnections" {
		return badName(field, name, "EkmConnection", "want projects/{project}/locations/{location}/ekmConnections/{ekm_connection}")
	}
	if err := parseLocation(field, strings.Join(parts[:4], "/")); err != nil {
		return err
	}
	if !idRE.MatchString(parts[5]) {
		return badName(field, name, "EkmConnection", "the ID must be 1-63 letters, digits, - or _")
	}
	return nil
}

func parseEkmConfig(field, name string) (string, error) {
	loc, ok := strings.CutSuffix(name, "/ekmConfig")
	if !ok {
		return "", badName(field, name, "EkmConfig", "want projects/{project}/locations/{location}/ekmConfig")
	}
	if err := parseLocation(field, loc); err != nil {
		return "", err
	}
	return loc, nil
}

// EkmServer serves google.cloud.kms.v1.EkmService over the KMS store.
type EkmServer struct {
	kmspb.UnimplementedEkmServiceServer
	s *Server
}

// Ekm returns the EKM service over s's store.
func (s *Server) Ekm() *EkmServer { return &EkmServer{s: s} }

var ekmFilterFields = fieldSupport{"name": {"=", "!=", ":"}}

func (e *EkmServer) CreateEkmConnection(_ context.Context, req *kmspb.CreateEkmConnectionRequest) (*kmspb.EkmConnection, error) {
	s := e.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if !idRE.MatchString(req.GetEkmConnectionId()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("ekm_connection_id %q must be 1-63 letters, digits, - or _", req.GetEkmConnectionId()))
	}
	in := req.GetEkmConnection()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("ekm_connection is required"))
	}
	resolvers, err := resolversOf(in.GetServiceResolvers())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	name := req.GetParent() + "/ekmConnections/" + req.GetEkmConnectionId()
	c := ekmConnection{Name: name, Created: s.now().UTC(), Resolvers: resolvers, CryptoSpace: in.GetCryptoSpacePath()}
	if m := in.GetKeyManagementMode(); m != kmspb.EkmConnection_KEY_MANAGEMENT_MODE_UNSPECIFIED {
		c.Mode = m.String()
	}
	if err := checkCryptoSpace(c); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing ekmConnection
	if found, err := s.get(dbKey(ekmConnectionPrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("EkmConnection %s already exists", name))
	}
	if err := s.put(dbKey(ekmConnectionPrefix, name), c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return c.proto(), nil
}

func (e *EkmServer) GetEkmConnection(_ context.Context, req *kmspb.GetEkmConnectionRequest) (*kmspb.EkmConnection, error) {
	if err := parseEkmConnection("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var c ekmConnection
	if err := e.s.load(dbKey(ekmConnectionPrefix, req.GetName()), &c, "EkmConnection", req.GetName()); err != nil {
		return nil, err
	}
	return c.proto(), nil
}

func (e *EkmServer) ListEkmConnections(_ context.Context, req *kmspb.ListEkmConnectionsRequest) (*kmspb.ListEkmConnectionsResponse, error) {
	s := e.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), ekmFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(ekmConnectionPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if filter != nil {
		var matched []string
		for _, n := range names {
			if filter.match(func(field string) (string, bool) { return n, field == "name" }) {
				matched = append(matched, n)
			}
		}
		names = matched
	}
	page, next, err := orderedPage("kms-ekm:"+req.GetParent()+"|filter="+req.GetFilter(), names, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListEkmConnectionsResponse{NextPageToken: next, TotalSize: int32(len(names))}
	for _, n := range page {
		var c ekmConnection
		found, err := s.get(dbKey(ekmConnectionPrefix, n), &c)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			resp.EkmConnections = append(resp.EkmConnections, c.proto())
		}
	}
	return resp, nil
}

// UpdateEkmConnection changes service_resolvers, key_management_mode and
// crypto_space_path. An etag that is not the stored one is ABORTED, as
// Google's etags are elsewhere (UNVERIFIED here).
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/ekm_service.proto
func (e *EkmServer) UpdateEkmConnection(_ context.Context, req *kmspb.UpdateEkmConnectionRequest) (*kmspb.EkmConnection, error) {
	s := e.s
	in := req.GetEkmConnection()
	if err := parseEkmConnection("ekm_connection.name", in.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required"))
	}
	var resolvers []ekmResolver
	var setResolvers, setMode, setSpace bool
	for _, p := range paths {
		switch p {
		case "service_resolvers":
			r, err := resolversOf(in.GetServiceResolvers())
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			resolvers, setResolvers = r, true
		case "key_management_mode":
			setMode = true
		case "crypto_space_path":
			setSpace = true
		case "name", "create_time":
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q names an output-only field", p))
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not an EkmConnection field that can be updated", p))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var c ekmConnection
	if err := s.load(dbKey(ekmConnectionPrefix, in.GetName()), &c, "EkmConnection", in.GetName()); err != nil {
		return nil, err
	}
	if tag := in.GetEtag(); tag != "" && tag != c.etag() {
		return nil, apierror.Wrap(apierror.Aborted("etag %q does not match EkmConnection %s: it has changed since it was read", tag, c.Name))
	}
	if setResolvers {
		c.Resolvers = resolvers
	}
	if setMode {
		c.Mode = ""
		if m := in.GetKeyManagementMode(); m != kmspb.EkmConnection_KEY_MANAGEMENT_MODE_UNSPECIFIED {
			c.Mode = m.String()
		}
	}
	if setSpace {
		c.CryptoSpace = in.GetCryptoSpacePath()
	}
	if err := checkCryptoSpace(c); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.put(dbKey(ekmConnectionPrefix, c.Name), c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return c.proto(), nil
}

// GetEkmConfig returns a location's EkmConfig. Every location has one, with
// no default connection until one is set ("the EkmConfig singleton").
func (e *EkmServer) GetEkmConfig(_ context.Context, req *kmspb.GetEkmConfigRequest) (*kmspb.EkmConfig, error) {
	if _, err := parseEkmConfig("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	c := ekmConfig{Name: req.GetName()}
	if _, err := e.s.get(dbKey(ekmConfigPrefix, req.GetName()), &c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &kmspb.EkmConfig{Name: c.Name, DefaultEkmConnection: c.Default}, nil
}

// UpdateEkmConfig sets or clears default_ekm_connection, which must name an
// EkmConnection in the same location.
func (e *EkmServer) UpdateEkmConfig(_ context.Context, req *kmspb.UpdateEkmConfigRequest) (*kmspb.EkmConfig, error) {
	s := e.s
	in := req.GetEkmConfig()
	loc, err := parseEkmConfig("ekm_config.name", in.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required"))
	}
	for _, p := range paths {
		if p != "default_ekm_connection" {
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not an EkmConfig field that can be updated: only default_ekm_connection is", p))
		}
	}
	def := in.GetDefaultEkmConnection()
	s.mu.Lock()
	defer s.mu.Unlock()
	if def != "" {
		if err := parseEkmConnection("ekm_config.default_ekm_connection", def); err != nil {
			return nil, apierror.Wrap(err)
		}
		if !strings.HasPrefix(def, loc+"/") {
			return nil, apierror.Wrap(apierror.InvalidArgument("ekm_config.default_ekm_connection %s is not in %s", def, loc))
		}
		var c ekmConnection
		if found, err := s.get(dbKey(ekmConnectionPrefix, def), &c); err != nil {
			return nil, apierror.Wrap(err)
		} else if !found {
			return nil, apierror.Wrap(apierror.InvalidArgument("ekm_config.default_ekm_connection %s does not exist", def))
		}
	}
	// The record is read again under the lock, so the IAM policy stored on
	// it (#1469) is kept.
	c := ekmConfig{Name: in.GetName()}
	if _, err := s.get(dbKey(ekmConfigPrefix, c.Name), &c); err != nil {
		return nil, apierror.Wrap(err)
	}
	c.Name, c.Default = in.GetName(), def
	if err := s.put(dbKey(ekmConfigPrefix, c.Name), c); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &kmspb.EkmConfig{Name: c.Name, DefaultEkmConnection: c.Default}, nil
}

// VerifyConnectivity "performs checks to ensure that the EkmConnection is
// configured correctly and can be reached" (ekm_service.proto). The EKM of
// every connection is the instance itself (external.go, #1469), which is
// always reachable, so a connection that exists verifies: the response is
// empty, as Google's is on success. An absent one is NOT_FOUND. Nothing
// dials the connection's hostname or Service Directory service.
func (e *EkmServer) VerifyConnectivity(_ context.Context, req *kmspb.VerifyConnectivityRequest) (*kmspb.VerifyConnectivityResponse, error) {
	if err := parseEkmConnection("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var c ekmConnection
	if err := e.s.load(dbKey(ekmConnectionPrefix, req.GetName()), &c, "EkmConnection", req.GetName()); err != nil {
		return nil, err
	}
	return &kmspb.VerifyConnectivityResponse{}, nil
}

// EkmConnectionsInProject returns every EkmConnection of project, in every
// location, ordered by name, for the console, which lists a project's
// connections on one page as KeyRingsInProject lists its rings.
func (s *Server) EkmConnectionsInProject(project string) ([]*kmspb.EkmConnection, error) {
	if err := parseLocation("project", "projects/"+project+"/locations/global"); err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(ekmConnectionPrefix, "projects/"+project+"/locations")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var out []*kmspb.EkmConnection
	for _, n := range names {
		var c ekmConnection
		found, err := s.get(dbKey(ekmConnectionPrefix, n), &c)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			out = append(out, c.proto())
		}
	}
	return out, nil
}
