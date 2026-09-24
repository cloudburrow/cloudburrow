// Package kms serves the Cloud KMS resource-management API
// (google.cloud.kms.v1.KeyManagementService) for local development (#309).
//
// Google publishes no KMS emulator and no maintained third-party one exists
// (docs/upstream-evaluation.md), so this is built. It manages key rings,
// symmetric encryption keys and their versions — the resources an
// application or Terraform creates before it encrypts anything. Encrypt and
// Decrypt, asymmetric and MAC purposes, import jobs, HSM and EKM protection,
// rotation schedules and IAM are UNIMPLEMENTED.
package kms

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type keyRing struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

type cryptoKey struct {
	Name    string            `json:"name"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels,omitempty"`
	Primary int               `json:"primary"`
	// Next is the number the next version gets.
	Next int `json:"next"`
}

type keyVersion struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// Material is the 256-bit AES key. It never leaves the store through
	// this API: no RPC returns it.
	Material []byte `json:"material"`
}

const (
	ringPrefix    = "kms/ring/"
	keyPrefix     = "kms/key/"
	versionPrefix = "kms/version/"
)

var (
	locationRE = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]+$`)
	idRE       = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
)

// Server serves google.cloud.kms.v1.KeyManagementService.
type Server struct {
	kmspb.UnimplementedKeyManagementServiceServer
	db  store.Store
	mu  sync.Mutex
	now func() time.Time
}

// NewServer returns the API over db.
func NewServer(db store.Store) *Server { return &Server{db: db, now: time.Now} }

// Register adds the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { kmspb.RegisterKeyManagementServiceServer(g, s) }

func (s *Server) put(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encode")
	}
	if err := s.db.Put(key, b); err != nil {
		return apierror.Internal(err, "store")
	}
	return nil
}

func (s *Server) get(key string, v any) bool {
	b, err := s.db.Get(key)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func dbKey(prefix, name string) string { return prefix + strings.ReplaceAll(name, "/", "~") }

func (s *Server) list(prefix, parent string) ([]string, error) {
	keys, err := s.db.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "list")
	}
	var names []string
	for _, k := range keys {
		n := strings.ReplaceAll(strings.TrimPrefix(k, prefix), "~", "/")
		if strings.HasPrefix(n, parent+"/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// --- key rings ---------------------------------------------------------

func toRing(r keyRing) *kmspb.KeyRing {
	return &kmspb.KeyRing{Name: r.Name, CreateTime: timestamppb.New(r.Created)}
}

func (s *Server) CreateKeyRing(_ context.Context, req *kmspb.CreateKeyRingRequest) (*kmspb.KeyRing, error) {
	if !locationRE.MatchString(req.GetParent()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent()))
	}
	if !idRE.MatchString(req.GetKeyRingId()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("key_ring_id %q must be 1-63 letters, digits, - or _", req.GetKeyRingId()))
	}
	name := req.GetParent() + "/keyRings/" + req.GetKeyRingId()
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing keyRing
	if s.get(dbKey(ringPrefix, name), &existing) {
		return nil, apierror.Wrap(apierror.AlreadyExists("KeyRing %s already exists", name))
	}
	r := keyRing{Name: name, Created: s.now().UTC()}
	if err := s.put(dbKey(ringPrefix, name), r); err != nil {
		return nil, apierror.Wrap(err)
	}
	return toRing(r), nil
}

func (s *Server) GetKeyRing(_ context.Context, req *kmspb.GetKeyRingRequest) (*kmspb.KeyRing, error) {
	var r keyRing
	if !s.get(dbKey(ringPrefix, req.GetName()), &r) {
		return nil, apierror.Wrap(apierror.NotFound("KeyRing %s not found", req.GetName()))
	}
	return toRing(r), nil
}

func (s *Server) ListKeyRings(_ context.Context, req *kmspb.ListKeyRingsRequest) (*kmspb.ListKeyRingsResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("filter and order_by are not implemented"))
	}
	names, err := s.list(ringPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := paging.Page("kms-rings:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListKeyRingsResponse{NextPageToken: next, TotalSize: int32(len(names))}
	for _, n := range page {
		var r keyRing
		if s.get(dbKey(ringPrefix, n), &r) {
			resp.KeyRings = append(resp.KeyRings, toRing(r))
		}
	}
	return resp, nil
}

// --- crypto keys and versions ------------------------------------------

func (s *Server) toVersion(v keyVersion) *kmspb.CryptoKeyVersion {
	return &kmspb.CryptoKeyVersion{
		Name:            v.Name,
		State:           kmspb.CryptoKeyVersion_ENABLED,
		ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
		Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		CreateTime:      timestamppb.New(v.Created),
		GenerateTime:    timestamppb.New(v.Created),
	}
}

func (s *Server) toKey(k cryptoKey) *kmspb.CryptoKey {
	out := &kmspb.CryptoKey{
		Name:       k.Name,
		Purpose:    kmspb.CryptoKey_ENCRYPT_DECRYPT,
		CreateTime: timestamppb.New(k.Created),
		Labels:     k.Labels,
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
			Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		},
	}
	if k.Primary > 0 {
		var v keyVersion
		if s.get(dbKey(versionPrefix, versionName(k.Name, k.Primary)), &v) {
			out.Primary = s.toVersion(v)
		}
	}
	return out
}

func versionName(key string, n int) string { return key + "/cryptoKeyVersions/" + strconv.Itoa(n) }

// unsupportedKey names what a CryptoKey asks for that is not implemented.
func unsupportedKey(k *kmspb.CryptoKey) error {
	if p := k.GetPurpose(); p != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return apierror.Unimplemented("purpose %s is not implemented: only ENCRYPT_DECRYPT (symmetric) keys are", p)
	}
	if t := k.GetVersionTemplate(); t != nil {
		if a := t.GetAlgorithm(); a != kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED &&
			a != kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
			return apierror.Unimplemented("algorithm %s is not implemented: only GOOGLE_SYMMETRIC_ENCRYPTION is", a)
		}
		if pl := t.GetProtectionLevel(); pl != kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED && pl != kmspb.ProtectionLevel_SOFTWARE {
			return apierror.Unimplemented("protection level %s is not implemented: only SOFTWARE keys exist locally", pl)
		}
	}
	if k.GetRotationPeriod() != nil || k.GetNextRotationTime() != nil {
		return apierror.Unimplemented("automatic rotation is not implemented: create versions with CreateCryptoKeyVersion")
	}
	if k.GetCryptoKeyBackend() != "" || k.GetImportOnly() || k.GetDestroyScheduledDuration() != nil {
		return apierror.Unimplemented("crypto_key_backend, import_only and destroy_scheduled_duration are not implemented")
	}
	return nil
}

func newMaterial() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, apierror.Internal(err, "generate key material")
	}
	return b, nil
}

func (s *Server) CreateCryptoKey(_ context.Context, req *kmspb.CreateCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	var ring keyRing
	if !s.get(dbKey(ringPrefix, req.GetParent()), &ring) {
		return nil, apierror.Wrap(apierror.NotFound("KeyRing %s not found", req.GetParent()))
	}
	if !idRE.MatchString(req.GetCryptoKeyId()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("crypto_key_id %q must be 1-63 letters, digits, - or _", req.GetCryptoKeyId()))
	}
	in := req.GetCryptoKey()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("crypto_key is required"))
	}
	if err := unsupportedKey(in); err != nil {
		return nil, apierror.Wrap(err)
	}
	name := req.GetParent() + "/cryptoKeys/" + req.GetCryptoKeyId()
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing cryptoKey
	if s.get(dbKey(keyPrefix, name), &existing) {
		return nil, apierror.Wrap(apierror.AlreadyExists("CryptoKey %s already exists", name))
	}
	now := s.now().UTC()
	k := cryptoKey{Name: name, Created: now, Labels: in.GetLabels(), Next: 1}
	if !req.GetSkipInitialVersionCreation() {
		mat, err := newMaterial()
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if err := s.put(dbKey(versionPrefix, versionName(name, 1)), keyVersion{Name: versionName(name, 1), Created: now, Material: mat}); err != nil {
			return nil, apierror.Wrap(err)
		}
		k.Primary, k.Next = 1, 2
	}
	if err := s.put(dbKey(keyPrefix, name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toKey(k), nil
}

func (s *Server) GetCryptoKey(_ context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	var k cryptoKey
	if !s.get(dbKey(keyPrefix, req.GetName()), &k) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKey %s not found", req.GetName()))
	}
	return s.toKey(k), nil
}

func (s *Server) ListCryptoKeys(_ context.Context, req *kmspb.ListCryptoKeysRequest) (*kmspb.ListCryptoKeysResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("filter and order_by are not implemented"))
	}
	var ring keyRing
	if !s.get(dbKey(ringPrefix, req.GetParent()), &ring) {
		return nil, apierror.Wrap(apierror.NotFound("KeyRing %s not found", req.GetParent()))
	}
	names, err := s.list(keyPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := paging.Page("kms-keys:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListCryptoKeysResponse{NextPageToken: next, TotalSize: int32(len(names))}
	for _, n := range page {
		var k cryptoKey
		if s.get(dbKey(keyPrefix, n), &k) {
			resp.CryptoKeys = append(resp.CryptoKeys, s.toKey(k))
		}
	}
	return resp, nil
}

func (s *Server) CreateCryptoKeyVersion(_ context.Context, req *kmspb.CreateCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if v := req.GetCryptoKeyVersion(); v != nil && v.GetState() != kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_STATE_UNSPECIFIED &&
		v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, apierror.Wrap(apierror.Unimplemented("creating a version in state %s is not implemented", v.GetState()))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if !s.get(dbKey(keyPrefix, req.GetParent()), &k) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKey %s not found", req.GetParent()))
	}
	mat, err := newMaterial()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	n := k.Next
	v := keyVersion{Name: versionName(k.Name, n), Created: s.now().UTC(), Material: mat}
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	// A new version is not made primary, as on Google: that is
	// UpdateCryptoKeyPrimaryVersion's job.
	k.Next = n + 1
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toVersion(v), nil
}

func (s *Server) GetCryptoKeyVersion(_ context.Context, req *kmspb.GetCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	var v keyVersion
	if !s.get(dbKey(versionPrefix, req.GetName()), &v) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKeyVersion %s not found", req.GetName()))
	}
	return s.toVersion(v), nil
}

func (s *Server) ListCryptoKeyVersions(_ context.Context, req *kmspb.ListCryptoKeyVersionsRequest) (*kmspb.ListCryptoKeyVersionsResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("filter and order_by are not implemented"))
	}
	var k cryptoKey
	if !s.get(dbKey(keyPrefix, req.GetParent()), &k) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKey %s not found", req.GetParent()))
	}
	names, err := s.list(versionPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// By number, not lexically: version 10 comes after 9.
	sort.Slice(names, func(i, j int) bool {
		a, _ := strconv.Atoi(names[i][strings.LastIndex(names[i], "/")+1:])
		b, _ := strconv.Atoi(names[j][strings.LastIndex(names[j], "/")+1:])
		return a < b
	})
	keys := make([]string, len(names))
	byKey := map[string]string{}
	for i, n := range names {
		keys[i] = fmt.Sprintf("%010d", i)
		byKey[keys[i]] = n
	}
	page, next, err := paging.Page("kms-versions:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListCryptoKeyVersionsResponse{NextPageToken: next, TotalSize: int32(len(names))}
	for _, pk := range page {
		var v keyVersion
		if s.get(dbKey(versionPrefix, byKey[pk]), &v) {
			resp.CryptoKeyVersions = append(resp.CryptoKeyVersions, s.toVersion(v))
		}
	}
	return resp, nil
}

func (s *Server) UpdateCryptoKeyPrimaryVersion(_ context.Context, req *kmspb.UpdateCryptoKeyPrimaryVersionRequest) (*kmspb.CryptoKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if !s.get(dbKey(keyPrefix, req.GetName()), &k) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKey %s not found", req.GetName()))
	}
	n, err := strconv.Atoi(req.GetCryptoKeyVersionId())
	if err != nil || n < 1 {
		return nil, apierror.Wrap(apierror.InvalidArgument("crypto_key_version_id %q is not a version number", req.GetCryptoKeyVersionId()))
	}
	var v keyVersion
	if !s.get(dbKey(versionPrefix, versionName(k.Name, n)), &v) {
		return nil, apierror.Wrap(apierror.NotFound("CryptoKeyVersion %s not found", versionName(k.Name, n)))
	}
	k.Primary = n
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toKey(k), nil
}
