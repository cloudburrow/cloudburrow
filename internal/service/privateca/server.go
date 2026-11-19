// Package privateca serves Certificate Authority Service v1
// (privateca.googleapis.com, #1834) in the CLI process: CA pools, root
// certificate authorities with keys generated locally, and the certificates
// they issue from a CSR or a CertificateConfig, signed for real with
// crypto/x509 so any TLS stack can verify them against the CA.
//
// Every mutation the API makes long-running answers a google.longrunning
// Operation that is already done, with privateca's OperationMetadata and
// the resource as its response; operations.get answers it afterwards.
//
// Subordinate CAs, certificate templates and CRLs (published, with each
// CA's certificate, at the access URLs this port serves) are phase 2.
// A CA's key may be a Cloud KMS CryptoKeyVersion of the instance's Cloud
// KMS, which signs everything the CA issues; a pool's encryption_spec wraps
// its certificates' data at rest with a Cloud KMS key; a CA's gcs_bucket
// has its CA certificate and CRLs published to the instance's Cloud
// Storage.
// docs/privateca.md is the user's guide.
package privateca

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Resource limits Google documents (quotas page, "Resource limits"):
// https://docs.cloud.google.com/certificate-authority-service/quotas
const (
	// MaxCAsPerLocation is "CAs: 1,000 per location per project".
	MaxCAsPerLocation = 1000
	// MaxRevokedPerCA is the unexpired revoked certificates a CA with a
	// software signing key may have, 500,000; a revocation past it fails.
	MaxRevokedPerCA = 500000
	// GracePeriod is how long a deleted CA can be undeleted: "a 30-day
	// grace period before it is deleted" (managing-ca-state).
	GracePeriod = 30 * 24 * time.Hour
)

// idRE is a resource ID: ca_pool_id, certificate_authority_id and
// certificate_id are "[a-zA-Z0-9_-]{1,63}" (the field comments of
// CreateCaPoolRequest, CreateCertificateAuthorityRequest and
// CreateCertificateRequest).
var idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

var (
	projectRE  = regexp.MustCompile(`^projects/[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)
	locationRE = regexp.MustCompile(`^projects/[a-z0-9][a-z0-9-]{0,61}[a-z0-9]/locations/[a-z0-9-]+$`)
	poolRE     = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+)/caPools/([^/]+)$`)
	caRE       = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/caPools/[^/]+)/certificateAuthorities/([^/]+)$`)
	certRE     = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/caPools/[^/]+)/certificates/([^/]+)$`)
)

// caRecord is a certificate authority and its signing key, which is the
// instance's own product data, kept beside it.
type caRecord struct {
	CA   *privatecapb.CertificateAuthority
	Key  crypto.Signer
	Cert *x509.Certificate
	// CSR is the PEM CSR of a CA awaiting activation.
	CSR string
}

// poolRecord is a CA pool and its IAM policy.
type poolRecord struct {
	Pool   *privatecapb.CaPool
	Policy *iampolicy.Stored
}

// Server is the Certificate Authority Service.
type Server struct {
	state string // the records' file; "" keeps them in memory

	mu    sync.Mutex
	pools map[string]*poolRecord
	cas   map[string]*caRecord
	certs map[string]*privatecapb.Certificate
	ops   map[string]*longrunningpb.Operation
	// templates are the certificate templates, crls each CA's CRL.
	templates map[string]*templateRecord
	crls      map[string]*privatecapb.CertificateRevocationList
	// crlPolicies are the CRLs' IAM policies.
	crlPolicies map[string]**iampolicy.Stored

	// addr is the address published content is served at.
	addr func() string
	// kms is the instance's Cloud KMS, or nil (kmskey.go).
	kms KMS
	// objects writes a CA's published content to its gcs_bucket, and
	// storageAddr is where the instance's Cloud Storage serves it
	// (publish.go); nil without Cloud Storage.
	objects     ObjectWriter
	storageAddr func() string
	// sealed are certificates of CMEK pools loaded encrypted that Cloud
	// KMS has not decrypted yet; sealedCache each certificate's last
	// ciphertext (cmek.go).
	sealed      map[string]*sealedCert
	sealedCache map[string]*sealedCert

	// now is the clock, for tests.
	now func() time.Time
}

// NewServer returns a server whose records are kept in state ("" keeps
// them in memory only). A state file that cannot be read is an error.
func NewServer(state string) (*Server, error) {
	s := &Server{state: state, now: time.Now,
		pools: map[string]*poolRecord{}, cas: map[string]*caRecord{},
		certs: map[string]*privatecapb.Certificate{}, ops: map[string]*longrunningpb.Operation{},
		templates: map[string]*templateRecord{}, crls: map[string]*privatecapb.CertificateRevocationList{},
		crlPolicies: map[string]**iampolicy.Stored{}, sealed: map[string]*sealedCert{}, sealedCache: map[string]*sealedCert{}}
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// disk is the state file's form: each resource in its JSON API form, and
// each CA's key as PKCS #8 PEM.
type disk struct {
	Pools    map[string]json.RawMessage `json:"pools"`
	Policies map[string]json.RawMessage `json:"policies,omitempty"`
	CAs      map[string]diskCA          `json:"cas"`
	Certs    map[string]json.RawMessage `json:"certificates"`
	Ops      map[string]json.RawMessage `json:"operations"`
	// Templates, their policies and the CRLs (#1834 phase 2).
	Templates        map[string]json.RawMessage `json:"templates,omitempty"`
	TemplatePolicies map[string]json.RawMessage `json:"templatePolicies,omitempty"`
	CRLs             map[string]json.RawMessage `json:"crls,omitempty"`
	// SealedCerts are the certificates of CMEK pools, encrypted.
	SealedCerts map[string]*sealedCert `json:"sealedCertificates,omitempty"`
}

type diskCA struct {
	CA  json.RawMessage `json:"ca"`
	Key string          `json:"key,omitempty"`
	// PublicKey and Algorithm are a Cloud KMS key's, whose private part
	// stays in Cloud KMS.
	PublicKey string `json:"publicKey,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Certificate Authority Service state: %w", err)
	}
	var d disk
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("parse %s: %w", s.state, err)
	}
	for n, raw := range d.Pools {
		p := &privatecapb.CaPool{}
		if err := protojson.Unmarshal(raw, p); err != nil {
			return fmt.Errorf("parse pool %s: %w", n, err)
		}
		rec := &poolRecord{Pool: p}
		if pol, ok := d.Policies[n]; ok {
			rec.Policy = &iampolicy.Stored{}
			if err := json.Unmarshal(pol, rec.Policy); err != nil {
				return fmt.Errorf("parse pool %s policy: %w", n, err)
			}
		}
		s.pools[n] = rec
	}
	for n, dc := range d.CAs {
		ca := &privatecapb.CertificateAuthority{}
		if err := protojson.Unmarshal(dc.CA, ca); err != nil {
			return fmt.Errorf("parse CA %s: %w", n, err)
		}
		var key crypto.Signer
		if v := ca.GetKeySpec().GetCloudKmsKeyVersion(); v != "" {
			pub, err := parsePublicPEM(dc.PublicKey)
			if err != nil {
				return fmt.Errorf("CA %s public key: %w", n, err)
			}
			key = &kmsSigner{s: s, version: v, pub: pub,
				alg: privatecapb.CertificateAuthority_SignHashAlgorithm(privatecapb.CertificateAuthority_SignHashAlgorithm_value[dc.Algorithm])}
		} else {
			blk, _ := pem.Decode([]byte(dc.Key))
			if blk == nil {
				return fmt.Errorf("CA %s: no key", n)
			}
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return fmt.Errorf("CA %s key: %w", n, err)
			}
			key = k.(crypto.Signer)
		}
		rec := &caRecord{CA: ca, Key: key}
		if len(ca.GetPemCaCertificates()) > 0 {
			if c, err := parsePEMCert(ca.GetPemCaCertificates()[0]); err == nil {
				rec.Cert = c
			}
		}
		if _, inKMS := key.(*kmsSigner); !inKMS && ca.GetState() == privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION {
			// A Cloud KMS key's CSR is made when it is fetched, once Cloud
			// KMS is up.
			if rec.CSR, err = makeCSR(rec); err != nil {
				return fmt.Errorf("CA %s CSR: %w", n, err)
			}
		}
		s.cas[n] = rec
	}
	for n, raw := range d.Templates {
		t := &privatecapb.CertificateTemplate{}
		if err := protojson.Unmarshal(raw, t); err != nil {
			return fmt.Errorf("parse template %s: %w", n, err)
		}
		rec := &templateRecord{Template: t}
		if pol, ok := d.TemplatePolicies[n]; ok {
			rec.Policy = &iampolicy.Stored{}
			if err := json.Unmarshal(pol, rec.Policy); err != nil {
				return fmt.Errorf("parse template %s policy: %w", n, err)
			}
		}
		s.templates[n] = rec
	}
	for n, raw := range d.CRLs {
		c := &privatecapb.CertificateRevocationList{}
		if err := protojson.Unmarshal(raw, c); err != nil {
			return fmt.Errorf("parse CRL %s: %w", n, err)
		}
		s.crls[n] = c
	}
	for n, raw := range d.Certs {
		c := &privatecapb.Certificate{}
		if err := protojson.Unmarshal(raw, c); err != nil {
			return fmt.Errorf("parse certificate %s: %w", n, err)
		}
		s.certs[n] = c
	}
	for n, sc := range d.SealedCerts {
		s.sealed[n] = sc
	}
	for n, raw := range d.Ops {
		o := &longrunningpb.Operation{}
		if err := protojson.Unmarshal(raw, o); err != nil {
			return fmt.Errorf("parse operation %s: %w", n, err)
		}
		s.ops[n] = o
	}
	return nil
}

// save writes the records; the caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	d := disk{Pools: map[string]json.RawMessage{}, Policies: map[string]json.RawMessage{}, CAs: map[string]diskCA{},
		Certs: map[string]json.RawMessage{}, Ops: map[string]json.RawMessage{},
		Templates: map[string]json.RawMessage{}, TemplatePolicies: map[string]json.RawMessage{}, CRLs: map[string]json.RawMessage{}}
	for n, t := range s.templates {
		b, err := protojson.Marshal(t.Template)
		if err != nil {
			return err
		}
		d.Templates[n] = b
		if t.Policy != nil {
			pb, err := json.Marshal(t.Policy)
			if err != nil {
				return err
			}
			d.TemplatePolicies[n] = pb
		}
	}
	for n, c := range s.crls {
		b, err := protojson.Marshal(c)
		if err != nil {
			return err
		}
		d.CRLs[n] = b
	}
	for n, p := range s.pools {
		b, err := protojson.Marshal(p.Pool)
		if err != nil {
			return err
		}
		d.Pools[n] = b
		if p.Policy != nil {
			pb, err := json.Marshal(p.Policy)
			if err != nil {
				return err
			}
			d.Policies[n] = pb
		}
	}
	for n, c := range s.cas {
		b, err := protojson.Marshal(c.CA)
		if err != nil {
			return err
		}
		if k, ok := c.Key.(*kmsSigner); ok {
			p, err := publicPEM(k.pub)
			if err != nil {
				return err
			}
			d.CAs[n] = diskCA{CA: b, PublicKey: p, Algorithm: k.alg.String()}
			continue
		}
		der, err := x509.MarshalPKCS8PrivateKey(c.Key)
		if err != nil {
			return err
		}
		d.CAs[n] = diskCA{CA: b, Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
	}
	d.SealedCerts = map[string]*sealedCert{}
	for n, sc := range s.sealed {
		d.SealedCerts[n] = sc
	}
	for n, c := range s.certs {
		if key := s.cmekKey(n); key != "" {
			sc, err := s.seal(n, c, key)
			if err != nil {
				return err
			}
			d.SealedCerts[n] = sc
			continue
		}
		b, err := protojson.Marshal(c)
		if err != nil {
			return err
		}
		d.Certs[n] = b
	}
	for n, o := range s.ops {
		b, err := protojson.Marshal(o)
		if err != nil {
			return err
		}
		d.Ops[n] = b
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

// ResetProject deletes every record of project, or of every project when
// project is "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	match := func(name string) bool { return project == "" || strings.HasPrefix(name, "projects/"+project+"/") }
	for n := range s.pools {
		if match(n) {
			delete(s.pools, n)
		}
	}
	for n := range s.cas {
		if match(n) {
			delete(s.cas, n)
		}
	}
	for n := range s.certs {
		if match(n) {
			delete(s.certs, n)
		}
	}
	for n := range s.sealed {
		if match(n) {
			delete(s.sealed, n)
			delete(s.sealedCache, n)
		}
	}
	for n := range s.ops {
		if match(n) {
			delete(s.ops, n)
		}
	}
	for n := range s.templates {
		if match(n) {
			delete(s.templates, n)
		}
	}
	for n := range s.crls {
		if match(n) {
			delete(s.crls, n)
		}
	}
	return s.save()
}

// sortedKeys is m's keys under prefix, sorted.
func sortedKeys[V any](m map[string]V, prefix string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clone[M proto.Message](m M) M { return proto.Clone(m).(M) }

func parsePEMCert(s string) (*x509.Certificate, error) {
	blk, _ := pem.Decode([]byte(s))
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// PoolsInProject is every CA pool of a project, in every location, for the
// console's list page, which Google's console draws across locations.
func (s *Server) PoolsInProject(project string) []*privatecapb.CaPool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*privatecapb.CaPool
	for _, k := range sortedKeys(s.pools, "projects/"+project+"/locations/") {
		if poolRE.MatchString(k) {
			out = append(out, clone(s.pools[k].Pool))
		}
	}
	return out
}

// TemplatesInProject is every certificate template of a project, in every
// location, for the console's templates page.
func (s *Server) TemplatesInProject(project string) []*privatecapb.CertificateTemplate {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*privatecapb.CertificateTemplate
	for _, k := range sortedKeys(s.templates, "projects/"+project+"/locations/") {
		out = append(out, clone(s.templates[k].Template))
	}
	return out
}
