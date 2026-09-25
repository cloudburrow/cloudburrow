// Package kms serves the Cloud KMS resource-management API
// (google.cloud.kms.v1.KeyManagementService) for local development (#309).
//
// It is built to Google's spec rather than reused: Google publishes no Cloud
// KMS emulator, and each third-party one departs from Google's documented
// behaviour (docs/upstream-evaluation.md, the Cloud KMS amendment). It manages key rings,
// symmetric encryption keys and their versions — the resources an
// application or Terraform creates before it encrypts anything. Encrypt and
// Decrypt, asymmetric and MAC purposes, import jobs, HSM and EKM protection,
// rotation schedules and IAM are UNIMPLEMENTED.
package kms

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/sched"
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
	// DestroyScheduled is how long a destroyed version waits in
	// DESTROY_SCHEDULED (#399). Zero reads as the 30-day default, for keys
	// created before it was stored.
	DestroyScheduled time.Duration `json:"destroyScheduled,omitempty"`
}

// Destroy scheduling (resources.proto destroy_scheduled_duration; the range
// is from the key-states documentation).
const (
	defaultDestroyScheduled = 30 * 24 * time.Hour
	minDestroyScheduled     = 24 * time.Hour
	maxDestroyScheduled     = 120 * 24 * time.Hour
)

func (k cryptoKey) destroyScheduled() time.Duration {
	if k.DestroyScheduled == 0 {
		return defaultDestroyScheduled
	}
	return k.DestroyScheduled
}

type keyVersion struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// Material is the 256-bit AES key. It never leaves the store through
	// this API: no RPC returns it.
	Material []byte `json:"material"`
	// State is the version's lifecycle state (#398). Empty reads as
	// ENABLED, so records written before states were stored stay readable.
	State string `json:"state,omitempty"`
	// DestroyTime is when a DESTROY_SCHEDULED version is destroyed, and
	// DestroyEventTime when a DESTROYED one was.
	DestroyTime      time.Time `json:"destroyTime,omitempty"`
	DestroyEventTime time.Time `json:"destroyEventTime,omitempty"`
}

// effective returns the version as it is at now: a DESTROY_SCHEDULED version
// whose destroy_time has come is DESTROYED, with destroy_event_time set and
// its material gone, whether or not the sweep has written that yet (#403).
func (v keyVersion) effective(now time.Time) keyVersion {
	if v.state() == kmspb.CryptoKeyVersion_DESTROY_SCHEDULED && !now.Before(v.DestroyTime) {
		v.State = kmspb.CryptoKeyVersion_DESTROYED.String()
		v.DestroyEventTime = v.DestroyTime
		v.DestroyTime = time.Time{}
		v.Material = nil
	}
	return v
}

// state returns the version's lifecycle state.
func (v keyVersion) state() kmspb.CryptoKeyVersion_CryptoKeyVersionState {
	if v.State == "" {
		return kmspb.CryptoKeyVersion_ENABLED
	}
	return kmspb.CryptoKeyVersion_CryptoKeyVersionState(kmspb.CryptoKeyVersion_CryptoKeyVersionState_value[v.State])
}

const (
	ringPrefix    = "kms/ring/"
	keyPrefix     = "kms/key/"
	versionPrefix = "kms/version/"
)

// Server serves google.cloud.kms.v1.KeyManagementService.
type Server struct {
	kmspb.UnimplementedKeyManagementServiceServer
	db    store.Store
	mu    sync.Mutex
	clock sched.Clock
	// rearm wakes Run when a destroy or restore changes the next due time.
	rearm chan struct{}
}

// NewServer returns the API over db.
func NewServer(db store.Store) *Server { return NewServerWithClock(db, sched.RealClock{}) }

// NewServerWithClock returns a Server on clock, so tests can move time for
// the lifecycle (destruction is scheduled 24 hours or more ahead).
func NewServerWithClock(db store.Store, clock sched.Clock) *Server {
	return &Server{db: db, clock: clock, rearm: make(chan struct{}, 1)}
}

func (s *Server) now() time.Time { return s.clock.Now() }

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

// get reads a record. Only a missing record is reported as not found: a
// failed read or an undecodable record is INTERNAL, because treating it as
// absent would let a caller overwrite, or silently lose, what is there. The
// message names the key, never the record's contents.
func (s *Server) get(key string, v any) (bool, error) {
	b, err := s.db.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, apierror.Internal(err, "read %s", key)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, apierror.Internal(err, "decode %s", key)
	}
	// Every read of a version sees its effective state (#403).
	if kv, ok := v.(*keyVersion); ok {
		*kv = kv.effective(s.now())
	}
	return true, nil
}

// load reads a record that must exist: absence is NOT_FOUND for the named
// resource, and any other failure INTERNAL.
func (s *Server) load(key string, v any, kind, name string) error {
	found, err := s.get(key, v)
	if err != nil {
		return apierror.Wrap(err)
	}
	if !found {
		return apierror.Wrap(apierror.NotFound("%s %s not found", kind, name))
	}
	return nil
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
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if !idRE.MatchString(req.GetKeyRingId()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("key_ring_id %q must be 1-63 letters, digits, - or _", req.GetKeyRingId()))
	}
	name := req.GetParent() + "/keyRings/" + req.GetKeyRingId()
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing keyRing
	if found, err := s.get(dbKey(ringPrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("KeyRing %s already exists", name))
	}
	r := keyRing{Name: name, Created: s.now().UTC()}
	if err := s.put(dbKey(ringPrefix, name), r); err != nil {
		return nil, apierror.Wrap(err)
	}
	return toRing(r), nil
}

func (s *Server) GetKeyRing(_ context.Context, req *kmspb.GetKeyRingRequest) (*kmspb.KeyRing, error) {
	if err := parseKeyRing("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var r keyRing
	if err := s.load(dbKey(ringPrefix, req.GetName()), &r, "KeyRing", req.GetName()); err != nil {
		return nil, err
	}
	return toRing(r), nil
}

func (s *Server) ListKeyRings(_ context.Context, req *kmspb.ListKeyRingsRequest) (*kmspb.ListKeyRingsResponse, error) {
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
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
		// A record listed but gone is a concurrent delete; one that cannot be
		// read fails the page, which must not come back short.
		found, err := s.get(dbKey(ringPrefix, n), &r)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			resp.KeyRings = append(resp.KeyRings, toRing(r))
		}
	}
	return resp, nil
}

// --- crypto keys and versions ------------------------------------------

func (s *Server) toVersion(v keyVersion) *kmspb.CryptoKeyVersion {
	out := &kmspb.CryptoKeyVersion{
		Name:            v.Name,
		State:           v.state(),
		ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
		Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		CreateTime:      timestamppb.New(v.Created),
		GenerateTime:    timestamppb.New(v.Created),
	}
	// As the proto says: destroy_time only while DESTROY_SCHEDULED,
	// destroy_event_time only once DESTROYED.
	switch out.State {
	case kmspb.CryptoKeyVersion_DESTROY_SCHEDULED:
		out.DestroyTime = timestamppb.New(v.DestroyTime)
	case kmspb.CryptoKeyVersion_DESTROYED:
		out.DestroyEventTime = timestamppb.New(v.DestroyEventTime)
	}
	return out
}

// toKey renders a key with its primary version. A primary that cannot be read
// is INTERNAL, not omitted: a key without its primary would look like a key
// that has none.
func (s *Server) toKey(k cryptoKey) (*kmspb.CryptoKey, error) {
	out := &kmspb.CryptoKey{
		Name:       k.Name,
		Purpose:    kmspb.CryptoKey_ENCRYPT_DECRYPT,
		CreateTime: timestamppb.New(k.Created),
		Labels:     k.Labels,
		// Echoed even when defaulted; whether Google echoes the default is
		// UNVERIFIED.
		DestroyScheduledDuration: durationpb.New(k.destroyScheduled()),
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
			Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		},
	}
	if k.Primary > 0 {
		var v keyVersion
		name := versionName(k.Name, k.Primary)
		found, err := s.get(dbKey(versionPrefix, name), &v)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !found {
			return nil, apierror.Wrap(apierror.Internal(nil, "primary version %s of %s is missing", name, k.Name))
		}
		out.Primary = s.toVersion(v)
	}
	return out, nil
}

func versionName(key string, n int) string { return key + "/cryptoKeyVersions/" + strconv.Itoa(n) }

// unsupportedKey names what a CryptoKey asks for that is not implemented.
func unsupportedKey(k *kmspb.CryptoKey) error {
	if k.GetPurpose() == kmspb.CryptoKey_CRYPTO_KEY_PURPOSE_UNSPECIFIED {
		// Required (service.proto); the code is UNVERIFIED.
		return apierror.InvalidArgument("crypto_key.purpose is required")
	}
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
	if k.GetCryptoKeyBackend() != "" {
		return apierror.Unimplemented("crypto_key_backend is not implemented")
	}
	if k.GetImportOnly() {
		return apierror.Unimplemented("import_only is not implemented")
	}
	return nil
}

// destroyScheduledOf validates crypto_key.destroy_scheduled_duration: unset
// is the 30-day default; otherwise whole seconds from 24h to 120d. The code
// for a value out of range is UNVERIFIED.
func destroyScheduledOf(k *kmspb.CryptoKey) (time.Duration, error) {
	d := k.GetDestroyScheduledDuration()
	if d == nil {
		return defaultDestroyScheduled, nil
	}
	if err := d.CheckValid(); err != nil || d.GetNanos() != 0 {
		return 0, apierror.InvalidArgument("crypto_key.destroy_scheduled_duration must be whole seconds from 24h to 120d")
	}
	v := d.AsDuration()
	if v < minDestroyScheduled || v > maxDestroyScheduled {
		return 0, apierror.InvalidArgument("crypto_key.destroy_scheduled_duration %s is out of range: it must be from 24h to 120d", v)
	}
	return v, nil
}

func newMaterial() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, apierror.Internal(err, "generate key material")
	}
	return b, nil
}

func (s *Server) CreateCryptoKey(_ context.Context, req *kmspb.CreateCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if err := parseKeyRing("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
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
	destroyAfter, err := destroyScheduledOf(in)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	name := req.GetParent() + "/cryptoKeys/" + req.GetCryptoKeyId()
	s.mu.Lock()
	defer s.mu.Unlock()
	// The request is validated first, then the parent looked up, so a bad
	// ID under a missing ring is INVALID_ARGUMENT, not NOT_FOUND.
	var ring keyRing
	if err := s.load(dbKey(ringPrefix, req.GetParent()), &ring, "KeyRing", req.GetParent()); err != nil {
		return nil, err
	}
	var existing cryptoKey
	if found, err := s.get(dbKey(keyPrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("CryptoKey %s already exists", name))
	}
	now := s.now().UTC()
	k := cryptoKey{Name: name, Created: now, Labels: in.GetLabels(), Next: 1, DestroyScheduled: destroyAfter}
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
	return s.toKey(k)
}

func (s *Server) GetCryptoKey(_ context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if err := parseCryptoKey("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetName()), &k, "CryptoKey", req.GetName()); err != nil {
		return nil, err
	}
	return s.toKey(k)
}

func (s *Server) ListCryptoKeys(_ context.Context, req *kmspb.ListCryptoKeysRequest) (*kmspb.ListCryptoKeysResponse, error) {
	if err := parseKeyRing("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("filter and order_by are not implemented"))
	}
	var ring keyRing
	if err := s.load(dbKey(ringPrefix, req.GetParent()), &ring, "KeyRing", req.GetParent()); err != nil {
		return nil, err
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
		found, err := s.get(dbKey(keyPrefix, n), &k)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !found {
			continue
		}
		pk, err := s.toKey(k)
		if err != nil {
			return nil, err
		}
		resp.CryptoKeys = append(resp.CryptoKeys, pk)
	}
	return resp, nil
}

func (s *Server) CreateCryptoKeyVersion(_ context.Context, req *kmspb.CreateCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if err := parseCryptoKey("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	// A version starts ENABLED, or DISABLED when asked (resources.proto
	// state; service.proto CreateCryptoKeyVersion). No other initial state
	// can be asked for; Google's code for one is UNVERIFIED.
	initial := kmspb.CryptoKeyVersion_ENABLED
	switch st := req.GetCryptoKeyVersion().GetState(); st {
	case kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_STATE_UNSPECIFIED, kmspb.CryptoKeyVersion_ENABLED:
	case kmspb.CryptoKeyVersion_DISABLED:
		initial = st
	default:
		return nil, apierror.Wrap(apierror.InvalidArgument("crypto_key_version.state %s is not a valid initial state: use ENABLED or DISABLED", st))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetParent()), &k, "CryptoKey", req.GetParent()); err != nil {
		return nil, err
	}
	mat, err := newMaterial()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	n := k.Next
	v := keyVersion{Name: versionName(k.Name, n), Created: s.now().UTC(), Material: mat, State: initial.String()}
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
	if _, _, err := parseCryptoKeyVersion("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, req.GetName()), &v, "CryptoKeyVersion", req.GetName()); err != nil {
		return nil, err
	}
	return s.toVersion(v), nil
}

func (s *Server) ListCryptoKeyVersions(_ context.Context, req *kmspb.ListCryptoKeyVersionsRequest) (*kmspb.ListCryptoKeyVersionsResponse, error) {
	if err := parseCryptoKey("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("filter and order_by are not implemented"))
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetParent()), &k, "CryptoKey", req.GetParent()); err != nil {
		return nil, err
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
		// A record listed but gone is a concurrent delete; one that cannot be
		// read fails the page, which must not come back short.
		found, err := s.get(dbKey(versionPrefix, byKey[pk]), &v)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			resp.CryptoKeyVersions = append(resp.CryptoKeyVersions, s.toVersion(v))
		}
	}
	return resp, nil
}

func (s *Server) UpdateCryptoKeyPrimaryVersion(_ context.Context, req *kmspb.UpdateCryptoKeyPrimaryVersionRequest) (*kmspb.CryptoKey, error) {
	if err := parseCryptoKey("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	n, err := versionNumber(req.GetCryptoKeyVersionId())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetName()), &k, "CryptoKey", req.GetName()); err != nil {
		return nil, err
	}
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, versionName(k.Name, n)), &v, "CryptoKeyVersion", versionName(k.Name, n)); err != nil {
		return nil, err
	}
	// Only an ENABLED version can become the primary: Encrypt by key name
	// would otherwise pick a version it must refuse (#401). The code is
	// UNVERIFIED.
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKeyVersion %s is %s: only an ENABLED version can be made primary", v.Name, st))
	}
	// service.proto: this "returns an error if called on a key whose purpose
	// is not ENCRYPT_DECRYPT". Every key here is ENCRYPT_DECRYPT, because
	// CreateCryptoKey refuses every other purpose (unsupportedKey), so the
	// check cannot be reached. It belongs here with the first other purpose.
	k.Primary = n
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toKey(k)
}

// UpdateCryptoKeyVersion moves a version between ENABLED and DISABLED, the
// only change this method makes (service.proto: DestroyCryptoKeyVersion and
// RestoreCryptoKeyVersion move between the others). The codes for a bad mask,
// a bad target and a version past disabling are UNVERIFIED.
func (s *Server) UpdateCryptoKeyVersion(_ context.Context, req *kmspb.UpdateCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	in := req.GetCryptoKeyVersion()
	if _, _, err := parseCryptoKeyVersion("crypto_key_version.name", in.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required and must contain state"))
	}
	for _, p := range paths {
		switch p {
		case "state":
		case "external_protection_level_options":
			return nil, apierror.Wrap(apierror.Unimplemented("update_mask path %q is not implemented: EXTERNAL protection is not supported", p))
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not a field that can be updated: only state is", p))
		}
	}
	target := in.GetState()
	if target != kmspb.CryptoKeyVersion_ENABLED && target != kmspb.CryptoKeyVersion_DISABLED {
		return nil, apierror.Wrap(apierror.InvalidArgument(
			"crypto_key_version.state %s is not allowed: UpdateCryptoKeyVersion moves between ENABLED and DISABLED only; use DestroyCryptoKeyVersion or RestoreCryptoKeyVersion", target))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, in.GetName()), &v, "CryptoKeyVersion", in.GetName()); err != nil {
		return nil, err
	}
	if cur := v.state(); cur != kmspb.CryptoKeyVersion_ENABLED && cur != kmspb.CryptoKeyVersion_DISABLED {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKeyVersion %s is %s: only an ENABLED or DISABLED version can be updated; restore it first", in.GetName(), cur))
	}
	// Disabling the primary is allowed: the key keeps it as its primary,
	// now DISABLED, and Encrypt by key name refuses it.
	v.State = target.String()
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toVersion(v), nil
}

// DestroyCryptoKeyVersion schedules a version for destruction: it becomes
// DESTROY_SCHEDULED with destroy_time the key's destroy_scheduled_duration
// from now (service.proto). The material is kept until then, so
// RestoreCryptoKeyVersion can still bring it back; #403 moves it to
// DESTROYED. The code for a version already scheduled or destroyed is
// UNVERIFIED.
func (s *Server) DestroyCryptoKeyVersion(_ context.Context, req *kmspb.DestroyCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	key, _, err := parseCryptoKeyVersion("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, req.GetName()), &v, "CryptoKeyVersion", req.GetName()); err != nil {
		return nil, err
	}
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED && st != kmspb.CryptoKeyVersion_DISABLED {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKeyVersion %s is %s: only an ENABLED or DISABLED version can be destroyed", v.Name, st))
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, key), &k, "CryptoKey", key); err != nil {
		return nil, err
	}
	// The primary may be destroyed too; whether Google refuses that is
	// UNVERIFIED, and nothing here refuses without evidence.
	v.State = kmspb.CryptoKeyVersion_DESTROY_SCHEDULED.String()
	v.DestroyTime = s.now().UTC().Add(k.destroyScheduled())
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.wake()
	return s.toVersion(v), nil
}

// wake re-arms Run after a change to what is due.
func (s *Server) wake() {
	select {
	case s.rearm <- struct{}{}:
	default:
	}
}

// Sweep writes DESTROYED for every version whose destroy_time has come, with
// destroy_event_time set and its key material removed from the record, and
// returns the earliest destroy_time still to come (zero if none). Reads never
// wait for it: they compute the effective state themselves.
func (s *Server) Sweep() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.db.List(versionPrefix)
	if err != nil {
		return time.Time{}, fmt.Errorf("list versions: %w", err)
	}
	now := s.now()
	var next time.Time
	for _, key := range keys {
		b, err := s.db.Get(key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("read %s: %w", key, err)
		}
		var stored keyVersion
		if err := json.Unmarshal(b, &stored); err != nil {
			return time.Time{}, fmt.Errorf("decode %s: %w", key, err)
		}
		if stored.state() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			continue
		}
		if eff := stored.effective(now); eff.state() == kmspb.CryptoKeyVersion_DESTROYED {
			if err := s.put(key, eff); err != nil {
				return time.Time{}, err
			}
			continue
		}
		if next.IsZero() || stored.DestroyTime.Before(next) {
			next = stored.DestroyTime
		}
	}
	return next, nil
}

// Run sweeps now, so versions that fell due while nothing was running are
// DESTROYED at once, then again at each next destroy_time, until ctx ends.
// A sweep that fails, such as before the cluster holding the store exists,
// is retried with backoff from a second up to a minute.
func (s *Server) Run(ctx context.Context) {
	retry := time.Second
	for {
		next, err := s.Sweep()
		var wait <-chan time.Time
		switch {
		case err != nil:
			wait = s.clock.After(retry)
			if retry *= 2; retry > time.Minute {
				retry = time.Minute
			}
		case !next.IsZero():
			retry = time.Second
			wait = s.clock.After(next.Sub(s.now()))
		default:
			retry = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.rearm:
		case <-wait:
		}
	}
}
