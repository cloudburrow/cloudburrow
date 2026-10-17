// Package kms serves the Cloud KMS resource-management API
// (google.cloud.kms.v1.KeyManagementService) for local development (#309).
//
// It is built to Google's spec rather than reused: Google publishes no Cloud
// KMS emulator, and each third-party one departs from Google's documented
// behaviour (docs/upstream-evaluation.md, the Cloud KMS amendment). It manages key rings,
// keys and their versions — the resources an application or Terraform
// creates before it encrypts anything — and serves the cryptographic methods:
// Encrypt and Decrypt (crypto.go); the asymmetric, MAC, raw and ML-KEM
// purposes (purposes.go, ops.go); key deletion (delete.go); and import jobs
// (import.go). IAM policies on key rings and crypto keys are stored and never
// enforced (iam.go, ADR-0006 as amended by #421). HSM keys are served as
// SOFTWARE ones are, without an HSM or an attestation (protection.go); EKM
// connections and configuration are stored (ekm.go), and Autokey creates
// keys on demand (autokey.go, #1452). EXTERNAL and EXTERNAL_VPC keys are
// served with the instance acting as their external key manager
// (external.go, #1469). Single-tenant HSM instances and their
// quorum-approved proposals are served by HsmManagement, and keys in them
// as HSM keys are (hsm.go, #1469). Trusted key wrapping and IAM on
// anything else are UNIMPLEMENTED. Automatic rotation (rotation_period, next_rotation_time) is
// served on the Server's clock (rotation.go, #816).
package kms

import (
	"context"

	"cloud.google.com/go/iam/apiv1/iampb"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type keyRing struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// IAMPolicy is stored, never enforced (#428). A record written before
	// it existed reads as having none.
	IAMPolicy *iampolicy.Stored `json:"iamPolicy,omitempty"`
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
	// IAMPolicy is stored, never enforced (#428).
	IAMPolicy *iampolicy.Stored `json:"iamPolicy,omitempty"`
	// RotationPeriod and NextRotation are the key's automatic rotation
	// schedule (#816); zero is unset. A record written before they were
	// stored reads as a key that does not rotate.
	RotationPeriod time.Duration `json:"rotationPeriod,omitempty"`
	NextRotation   time.Time     `json:"nextRotationTime,omitempty"`
	// Purpose and Algorithm (the version template's) are empty for
	// ENCRYPT_DECRYPT and GOOGLE_SYMMETRIC_ENCRYPTION (#1212), so records
	// written before other purposes existed read unchanged.
	Purpose   string `json:"purpose,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`
	// ProtectionLevel is the version template's, empty for SOFTWARE
	// (#1452); new versions get it.
	ProtectionLevel string `json:"protectionLevel,omitempty"`
	// KAJPolicy is key_access_justifications_policy, stored and returned,
	// enforced on encrypt, decrypt and sign (protection.go, #1506).
	KAJPolicy *kajPolicy `json:"keyAccessJustificationsPolicy,omitempty"`
	// Backend is crypto_key_backend, the EkmConnection of an EXTERNAL_VPC
	// key, and Coordinated whether that connection was CLOUD_KMS-mode when
	// the key was created (external.go, #1469).
	Backend     string `json:"cryptoKeyBackend,omitempty"`
	Coordinated bool   `json:"coordinated,omitempty"`
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
	// Material is the key: AES or HMAC bytes, or a PKCS #8 DER private key
	// (purposes.go). It never leaves the store through this API: no RPC
	// returns it.
	Material []byte `json:"material"`
	// Algorithm is empty for GOOGLE_SYMMETRIC_ENCRYPTION (#1212).
	Algorithm string `json:"algorithm,omitempty"`
	// ProtectionLevel is empty for SOFTWARE (#1452).
	ProtectionLevel string `json:"protectionLevel,omitempty"`
	// ImportJob and Imported are set on an imported version (import.go).
	ImportJob string    `json:"importJob,omitempty"`
	Imported  time.Time `json:"importTime,omitempty"`
	// State is the version's lifecycle state (#398). Empty reads as
	// ENABLED, so records written before states were stored stay readable.
	State string `json:"state,omitempty"`
	// DestroyTime is when a DESTROY_SCHEDULED version is destroyed, and
	// DestroyEventTime when a DESTROYED one was.
	DestroyTime      time.Time `json:"destroyTime,omitempty"`
	DestroyEventTime time.Time `json:"destroyEventTime,omitempty"`
	// ExternalKeyURI, KeyPath and Backend are an external version's
	// external_protection_level_options (external.go, #1469).
	ExternalKeyURI string `json:"externalKeyUri,omitempty"`
	KeyPath        string `json:"ekmConnectionKeyPath,omitempty"`
	Backend        string `json:"ekmConnectionBackendOverride,omitempty"`
	// TrustedWrapping is trusted_wrapping_enabled, and HsmTrusted an
	// AES_WRAPPING version's hsm_trusted, set by upgrade_key_trust
	// (trusted.go, #1521).
	TrustedWrapping bool `json:"trustedWrappingEnabled,omitempty"`
	HsmTrusted      bool `json:"hsmTrusted,omitempty"`
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
	// hierarchy resolves Autokey configs through folders (autokey.go).
	hierarchy Hierarchy
}

// NewServer returns the API over db.
func NewServer(db store.Store) *Server { return NewServerWithClock(db, sched.RealClock{}) }

// NewServerWithClock returns a Server on clock, so tests can move time for
// the lifecycle (destruction is scheduled 24 hours or more ahead).
func NewServerWithClock(db store.Store, clock sched.Clock) *Server {
	return &Server{db: db, clock: clock, rearm: make(chan struct{}, 1)}
}

func (s *Server) now() time.Time { return s.clock.Now() }

// Register adds the service to a gRPC server, or to the JSON transcoder,
// with the EKM, Autokey, AutokeyAdmin and HsmManagement services
// cloudkms.googleapis.com also serves (#1452), the IAMPolicy mixin it serves
// beside them (cloudkms_v1.yaml:13), and the Locations mixin (#1291). The
// Operations mixin is registered apart (Operations).
func (s *Server) Register(g grpc.ServiceRegistrar) {
	kmspb.RegisterKeyManagementServiceServer(g, s)
	kmspb.RegisterEkmServiceServer(g, s.Ekm())
	kmspb.RegisterAutokeyServer(g, s.Autokey())
	kmspb.RegisterAutokeyAdminServer(g, s.AutokeyAdmin())
	kmspb.RegisterHsmManagementServer(g, s.HsmManagement())
	iampb.RegisterIAMPolicyServer(g, s.IAMPolicy())
	locations.Register(g, s.Locations())
}

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
	filter, err := parseFilter(req.GetFilter(), ringFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(ringPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// name compares the full resource name (#409); UNVERIFIED against Google.
	if filter != nil {
		var matched []string
		for _, n := range names {
			if filter.match(func(field string) (string, bool) { return n, field == "name" }) {
				matched = append(matched, n)
			}
		}
		names = matched
	}
	page, next, err := orderedPage("kms-rings:"+req.GetParent()+"|filter="+req.GetFilter(), names, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
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

// KeyRingsInProject returns every key ring in project, in every location,
// ordered by name, for the console (#593). ListKeyRings takes one location,
// so a screen listing a project's rings through it would have to ask about
// every location ListLocations lists. The records are the ones ListKeyRings reads.
func (s *Server) KeyRingsInProject(project string) ([]*kmspb.KeyRing, error) {
	if err := parseLocation("project", "projects/"+project+"/locations/global"); err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(ringPrefix, "projects/"+project+"/locations")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var out []*kmspb.KeyRing
	for _, n := range names {
		var r keyRing
		found, err := s.get(dbKey(ringPrefix, n), &r)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			out = append(out, toRing(r))
		}
	}
	return out, nil
}

// --- crypto keys and versions ------------------------------------------

func (s *Server) toVersion(v keyVersion) *kmspb.CryptoKeyVersion {
	out := &kmspb.CryptoKeyVersion{
		Name:            v.Name,
		State:           v.state(),
		ProtectionLevel: v.protectionLevel(),
		Algorithm:       v.algorithm(),
		CreateTime:      timestamppb.New(v.Created),
		GenerateTime:    timestamppb.New(v.Created),

		ExternalProtectionLevelOptions: v.externalOptions(),
		TrustedWrappingEnabled:         v.TrustedWrapping,
		HsmTrusted:                     v.HsmTrusted,
	}
	// A trusted-wrapped import has an import_time but no import_job.
	if v.ImportJob != "" || !v.Imported.IsZero() {
		out.ImportJob = v.ImportJob
		out.ImportTime = timestamppb.New(v.Imported)
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
		Purpose:    k.purpose(),
		CreateTime: timestamppb.New(k.Created),
		Labels:     k.Labels,
		// Echoed even when defaulted; whether Google echoes the default is
		// UNVERIFIED.
		DestroyScheduledDuration: durationpb.New(k.destroyScheduled()),
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			ProtectionLevel: k.protectionLevel(),
			Algorithm:       k.algorithm(),
		},
		KeyAccessJustificationsPolicy: k.KAJPolicy.proto(),
		CryptoKeyBackend:              k.Backend,
	}
	if k.RotationPeriod > 0 {
		out.RotationSchedule = &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(k.RotationPeriod)}
	}
	if !k.NextRotation.IsZero() {
		out.NextRotationTime = timestamppb.New(k.NextRotation)
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
	if p := k.GetPurpose(); !purposes[p] {
		return apierror.Unimplemented("crypto_key.purpose %s is not implemented", p)
	}
	pl, err := protectionLevelOf("crypto_key.version_template.protection_level", k.GetVersionTemplate().GetProtectionLevel())
	if err != nil {
		return err
	}
	// crypto_key_backend is "only applicable" to EXTERNAL_VPC and
	// HSM_SINGLE_TENANT keys (resources.proto); Google's answer for another
	// level is not documented.
	if k.GetCryptoKeyBackend() != "" && pl != kmspb.ProtectionLevel_EXTERNAL_VPC && pl != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return apierror.Unimplemented("crypto_key.crypto_key_backend is not implemented for protection level %s: it applies to EXTERNAL_VPC keys, naming their EkmConnection, and HSM_SINGLE_TENANT keys, naming their SingleTenantHsmInstance", pl)
	}
	if k.GetImportOnly() {
		return apierror.Unimplemented("crypto_key.import_only is not implemented")
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

// newMaterial makes the material of a new version of k.
func newMaterial(k cryptoKey) ([]byte, error) { return generate(k.algorithm()) }

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
	level, _ := protectionLevelOf("", in.GetVersionTemplate().GetProtectionLevel())
	alg, err := levelAlgorithm(level, in.GetPurpose(), in.GetVersionTemplate().GetAlgorithm())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	destroyAfter, err := destroyScheduledOf(in)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	kaj, err := kajPolicyOf(in.GetKeyAccessJustificationsPolicy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	period, err := rotationPeriodOf(in)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	nextRotation, err := nextRotationOf(in)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// Only ENCRYPT_DECRYPT keys rotate automatically (resources.proto
	// rotation_period); the code for a schedule on another is UNVERIFIED.
	if (period > 0 || !nextRotation.IsZero()) && in.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return nil, apierror.Wrap(apierror.InvalidArgument("automatic rotation is only supported for ENCRYPT_DECRYPT keys, not %s", in.GetPurpose()))
	}
	if err := checkSchedule(period, nextRotation); err != nil {
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
	if retired, err := s.isRetired(name); err != nil {
		return nil, err
	} else if retired {
		// RetiredResources "prevent the reuse of these resource names after
		// deletion" (resources.proto); the code is UNVERIFIED.
		return nil, apierror.Wrap(apierror.AlreadyExists("CryptoKey %s was deleted, and its name cannot be reused", name))
	}
	k := cryptoKey{Name: name, Created: now, Labels: in.GetLabels(), Next: 1, DestroyScheduled: destroyAfter,
		RotationPeriod: period, NextRotation: nextRotation,
		Purpose: storedPurpose(in.GetPurpose()), Algorithm: storedAlgorithm(alg),
		ProtectionLevel: storedLevel(level), KAJPolicy: kaj}
	if level == kmspb.ProtectionLevel_EXTERNAL_VPC {
		c, err := s.keyBackend("crypto_key.crypto_key_backend", in.GetCryptoKeyBackend(), req.GetParent())
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		k.Backend, k.Coordinated = c.Name, c.mode() == kmspb.EkmConnection_CLOUD_KMS
	}
	if level == kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		if k.Backend, err = s.hsmBackend("crypto_key.crypto_key_backend", in.GetCryptoKeyBackend(), req.GetParent()); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	if k.manualExternal() {
		// create-external-key creates these keys with
		// --skip-initial-version-creation: each version names its external
		// key, which CreateCryptoKey cannot carry.
		if !req.GetSkipInitialVersionCreation() {
			return nil, apierror.Wrap(apierror.InvalidArgument("skip_initial_version_creation must be set for the %s key %s: each version names its external key in external_protection_level_options, which CreateCryptoKeyVersion takes", level, name))
		}
		if period > 0 || !nextRotation.IsZero() {
			return nil, apierror.Wrap(apierror.Unimplemented("automatic rotation is not implemented for %s keys whose versions name their own external key: Google documents it for symmetric coordinated external keys only", level))
		}
	}
	if req.GetTrustedWrappingEnabled() {
		if err := trustedWrappingAllowed("trusted_wrapping_enabled", k); err != nil {
			return nil, apierror.Wrap(err)
		}
		if req.GetSkipInitialVersionCreation() {
			return nil, apierror.Wrap(apierror.Unimplemented("trusted_wrapping_enabled with skip_initial_version_creation is not implemented: it applies to the first version created, and Google does not document which later version that is; set crypto_key_version.trusted_wrapping_enabled on CreateCryptoKeyVersion, or trusted_wrapping_enabled on ImportCryptoKeyVersion"))
		}
	}
	if !req.GetSkipInitialVersionCreation() {
		v, err := s.newVersion(k, 1, now, kmspb.CryptoKeyVersion_ENABLED, nil)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		v.TrustedWrapping = req.GetTrustedWrappingEnabled()
		if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
			return nil, apierror.Wrap(err)
		}
		k.Next = 2
		// Only an ENCRYPT_DECRYPT key has a primary (resources.proto
		// primary).
		if k.purpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT {
			k.Primary = 1
		}
	}
	if err := s.put(dbKey(keyPrefix, name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	if !k.NextRotation.IsZero() {
		s.wake()
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
	if err := checkView("version_view", req.GetVersionView()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := parseKeyRing("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), keyFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var ring keyRing
	if err := s.load(dbKey(ringPrefix, req.GetParent()), &ring, "KeyRing", req.GetParent()); err != nil {
		return nil, err
	}
	names, err := s.list(keyPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if filter != nil {
		var matched []string
		for _, n := range names {
			var k cryptoKey
			found, err := s.get(dbKey(keyPrefix, n), &k)
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			if found && filter.match(func(field string) (string, bool) {
				if field == "name" {
					return n, true
				}
				if label, ok := strings.CutPrefix(field, "labels."); ok {
					v, has := k.Labels[label]
					return v, has
				}
				return "", false
			}) {
				matched = append(matched, n)
			}
		}
		names = matched
	}
	page, next, err := orderedPage("kms-keys:"+req.GetParent()+"|filter="+req.GetFilter(), names, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
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
	if err := s.backendUsable(k); err != nil {
		return nil, err
	}
	trusted := req.GetCryptoKeyVersion().GetTrustedWrappingEnabled()
	if trusted {
		if err := trustedWrappingAllowed("crypto_key_version.trusted_wrapping_enabled", k); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	n := k.Next
	v, err := s.newVersion(k, n, s.now(), initial, req.GetCryptoKeyVersion().GetExternalProtectionLevelOptions())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v.TrustedWrapping = trusted
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
	if err := checkView("view", req.GetView()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := parseCryptoKey("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), versionFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetParent()), &k, "CryptoKey", req.GetParent()); err != nil {
		return nil, err
	}
	names, err := s.list(versionPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// Filter before paging, so total_size counts the matches and a page is
	// never short (#408).
	if filter != nil {
		var matched []string
		for _, n := range names {
			var v keyVersion
			found, err := s.get(dbKey(versionPrefix, n), &v)
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			if found && filter.match(func(field string) (string, bool) {
				if field == "state" {
					return v.state().String(), true
				}
				return "", false
			}) {
				matched = append(matched, n)
			}
		}
		names = matched
	}
	// By number, not lexically: version 10 comes after 9.
	page, next, err := orderedPage("kms-versions:"+req.GetParent()+"|filter="+req.GetFilter(), names, byVersionNumber, desc, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListCryptoKeyVersionsResponse{NextPageToken: next, TotalSize: int32(len(names))}
	for _, n := range page {
		var v keyVersion
		// A record listed but gone is a concurrent delete; one that cannot be
		// read fails the page, which must not come back short.
		found, err := s.get(dbKey(versionPrefix, n), &v)
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
	// service.proto: this "returns an error if called on a key whose purpose
	// is not ENCRYPT_DECRYPT". The code is UNVERIFIED.
	if p := k.purpose(); p != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s is %s: only an ENCRYPT_DECRYPT key has a primary version", k.Name, p))
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
	k.Primary = n
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toKey(k)
}

// UpdateCryptoKeyVersion moves a version between ENABLED and DISABLED
// (service.proto: DestroyCryptoKeyVersion and RestoreCryptoKeyVersion move
// between the others), and changes an external version's
// external_protection_level_options: the key URI or key path it names,
// without rotating the key (update-external-key-path, #1469). The codes for
// a bad mask, a bad target and a version past disabling are UNVERIFIED.
func (s *Server) UpdateCryptoKeyVersion(_ context.Context, req *kmspb.UpdateCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	in := req.GetCryptoKeyVersion()
	key, _, err := parseCryptoKeyVersion("crypto_key_version.name", in.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required and must contain state or external_protection_level_options"))
	}
	// external_protection_level_options is replaced whole, or one field of
	// it at a time, as gcloud kms keys versions update sends it.
	var setState bool
	external := map[string]bool{}
	for _, p := range paths {
		switch p {
		case "state":
			setState = true
		case "external_protection_level_options", "external_protection_level_options.external_key_uri",
			"external_protection_level_options.ekm_connection_key_path", "external_protection_level_options.ekm_connection_backend_override":
			external[strings.TrimPrefix(strings.TrimPrefix(p, "external_protection_level_options"), ".")] = true
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not a field that can be updated: only state and external_protection_level_options are", p))
		}
	}
	target := in.GetState()
	if setState && target != kmspb.CryptoKeyVersion_ENABLED && target != kmspb.CryptoKeyVersion_DISABLED {
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
	if len(external) > 0 {
		var k cryptoKey
		if err := s.load(dbKey(keyPrefix, key), &k, "CryptoKey", key); err != nil {
			return nil, err
		}
		if !isExternal(v.protectionLevel()) {
			return nil, apierror.Wrap(apierror.InvalidArgument("external_protection_level_options is only for EXTERNAL and EXTERNAL_VPC versions, not %s", v.protectionLevel()))
		}
		// The key material is the version's own, so the version keeps naming
		// the same key under its new reference.
		given, all := in.GetExternalProtectionLevelOptions(), external[""]
		opts := &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: v.ExternalKeyURI, EkmConnectionKeyPath: v.KeyPath, EkmConnectionBackendOverride: v.Backend}
		if all || external["external_key_uri"] {
			opts.ExternalKeyUri = given.GetExternalKeyUri()
		}
		if all || external["ekm_connection_key_path"] {
			opts.EkmConnectionKeyPath = given.GetEkmConnectionKeyPath()
		}
		if all || external["ekm_connection_backend_override"] {
			opts.EkmConnectionBackendOverride = given.GetEkmConnectionBackendOverride()
		}
		// A coordinated version's path is the EKM's (external.go), and Google
		// documents no change to it.
		if k.Coordinated && v.Backend == "" {
			return nil, apierror.Wrap(apierror.Unimplemented("external_protection_level_options is not implemented for %s, a coordinated external key's version: its key path is chosen by the EKM", v.Name))
		}
		updated := v
		updated.ExternalKeyURI, updated.KeyPath, updated.Backend = "", "", ""
		if err := s.externalRef(k, opts, &updated); err != nil {
			return nil, apierror.Wrap(err)
		}
		v = updated
	}
	// Disabling the primary is allowed: the key keeps it as its primary,
	// now DISABLED, and Encrypt by key name refuses it.
	if setState {
		v.State = target.String()
	}
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

// Exclusive runs fn while no request or sweep holds the server: /admin/reset
// deletes the store's records through it. Without it a sweep or a write in
// flight, holding a record it read before the delete, writes it back after,
// and the reset leaves it behind.
func (s *Server) Exclusive(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// wake re-arms Run after a change to what is due.
func (s *Server) wake() {
	select {
	case s.rearm <- struct{}{}:
	default:
	}
}

// Sweep writes DESTROYED for every version whose destroy_time has come, with
// destroy_event_time set and its key material removed from the record, rotates
// every key whose next_rotation_time has come (rotateDue, rotation.go), and
// returns the earliest destroy_time or next_rotation_time still to come (zero
// if none). Reads of versions never wait for it: they compute the effective
// state themselves. A rotation creates a version, so it happens here and not
// on a read.
//
// It reads every record without holding s.mu, with one request per prefix
// where the store can (scan), and takes s.mu only to write one record that is
// due, re-reading it first so a change made meanwhile (a restore, a schedule
// cleared, a reset) wins. Holding s.mu across one kubectl read per record made
// requests wait behind the sweep past their deadline once earlier runs had
// left a few hundred records (#1232). A change made meanwhile that moves what
// is due calls wake, so Run sweeps again.
func (s *Server) Sweep() (time.Time, error) {
	versions, err := s.scan(versionPrefix)
	if err != nil {
		return time.Time{}, fmt.Errorf("list versions: %w", err)
	}
	keys, err := s.scan(keyPrefix)
	if err != nil {
		return time.Time{}, fmt.Errorf("list keys: %w", err)
	}
	now := s.now()
	var next time.Time
	earliest := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, r := range versions {
		var stored keyVersion
		if err := json.Unmarshal(r.value, &stored); err != nil {
			return time.Time{}, fmt.Errorf("decode %s: %w", r.key, err)
		}
		if stored.state() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			continue
		}
		if stored.effective(now).state() != kmspb.CryptoKeyVersion_DESTROYED {
			earliest(stored.DestroyTime)
			continue
		}
		if err := s.destroyDue(r.key, now); err != nil {
			return time.Time{}, err
		}
	}
	for _, r := range keys {
		var k cryptoKey
		if err := json.Unmarshal(r.value, &k); err != nil {
			return time.Time{}, fmt.Errorf("decode %s: %w", r.key, err)
		}
		if k.NextRotation.IsZero() {
			continue
		}
		if now.Before(k.NextRotation) {
			earliest(k.NextRotation)
			continue
		}
		rotation, err := s.rotateDue(r.key, now)
		if err != nil {
			return time.Time{}, err
		}
		earliest(rotation)
	}
	return next, nil
}

// destroyDue writes DESTROYED for one version the sweep found due, under
// s.mu, if the stored record still is: it is read again, so a version
// restored or deleted since the sweep read it is left as it now is.
func (s *Server) destroyDue(key string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.db.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	var stored keyVersion
	if err := json.Unmarshal(b, &stored); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	if stored.state() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		return nil
	}
	if eff := stored.effective(now); eff.state() == kmspb.CryptoKeyVersion_DESTROYED {
		return s.put(key, eff)
	}
	return nil
}

// record is one stored record a scan read.
type record struct {
	key   string
	value []byte
}

// scanner is a store that reads every record under a prefix with one
// request; KubeStore does, with one kubectl list.
type scanner interface {
	Scan(prefix string) (map[string][]byte, error)
}

// scan returns the records under prefix, sorted by key, with one request
// when the store is a scanner and with List and a Get per record otherwise
// (the in-process stores, where a Get costs nothing). A record deleted
// between the List and its Get is left out.
func (s *Server) scan(prefix string) ([]record, error) {
	var out []record
	if sc, ok := s.db.(scanner); ok {
		all, err := sc.Scan(prefix)
		if err != nil {
			return nil, err
		}
		for k, v := range all {
			out = append(out, record{k, v})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
		return out, nil
	}
	keys, err := s.db.List(prefix)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		b, err := s.db.Get(key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", key, err)
		}
		out = append(out, record{key, b})
	}
	return out, nil
}

// Run sweeps now, so versions that fell due while nothing was running are
// DESTROYED, and keys due rotated, at once, then again at each next
// destroy_time or next_rotation_time, until ctx ends.
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

// RestoreCryptoKeyVersion brings a DESTROY_SCHEDULED version back before its
// destroy_time: it becomes DISABLED, with destroy_time cleared and its
// material untouched (service.proto). A version in any other state, including
// one whose destroy_time has passed, is FAILED_PRECONDITION (UNVERIFIED).
func (s *Server) RestoreCryptoKeyVersion(_ context.Context, req *kmspb.RestoreCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if _, _, err := parseCryptoKeyVersion("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, req.GetName()), &v, "CryptoKeyVersion", req.GetName()); err != nil {
		return nil, err
	}
	// load returns the effective state, so a version past destroy_time is
	// already DESTROYED here and is refused.
	if st := v.state(); st != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKeyVersion %s is %s: only a DESTROY_SCHEDULED version can be restored", v.Name, st))
	}
	v.State = kmspb.CryptoKeyVersion_DISABLED.String()
	v.DestroyTime = time.Time{}
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.wake()
	return s.toVersion(v), nil
}

// labelKeyRE and labelValueRE are Google's label rules
// (resource-manager labels overview): a key starts with a lowercase letter;
// keys and values use lowercase letters, digits, - and _, up to 63
// characters; at most 64 labels.
var (
	labelKeyRE   = regexp.MustCompile(`^\p{Ll}[\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValueRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

func validateLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierror.InvalidArgument("crypto_key.labels has %d labels: at most 64 are allowed", len(labels))
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return apierror.InvalidArgument("crypto_key.labels key %q is not a valid label key", k)
		}
		if !labelValueRE.MatchString(v) {
			return apierror.InvalidArgument("crypto_key.labels value for %q is not a valid label value", k)
		}
	}
	return nil
}

// UpdateCryptoKey changes the fields update_mask names: labels, the rotation
// schedule (rotation_period, next_rotation_time; #816), and a version
// template that stays GOOGLE_SYMMETRIC_ENCRYPTION at SOFTWARE. A path named
// with its field unset clears it.
// Immutable and output-only fields are refused naming the path; the codes
// are UNVERIFIED.
func (s *Server) UpdateCryptoKey(_ context.Context, req *kmspb.UpdateCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	in := req.GetCryptoKey()
	if err := parseCryptoKey("crypto_key.name", in.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required"))
	}
	var setLabels, setPeriod, setNext, setTemplate, setLevel, setKAJ bool
	var level kmspb.ProtectionLevel
	var kaj *kajPolicy
	var templateAlg algo
	var period time.Duration
	var nextRotation time.Time
	for _, p := range paths {
		switch p {
		case "labels":
			if err := validateLabels(in.GetLabels()); err != nil {
				return nil, apierror.Wrap(err)
			}
			setLabels = true
		case "version_template", "version_template.algorithm", "version_template.protection_level":
			t := in.GetVersionTemplate()
			if p != "version_template.protection_level" {
				templateAlg, setTemplate = t.GetAlgorithm(), true
			}
			if p != "version_template.algorithm" {
				pl, err := protectionLevelOf("version_template.protection_level", t.GetProtectionLevel())
				if err != nil {
					return nil, apierror.Wrap(err)
				}
				level, setLevel = pl, true
			}
		case "rotation_period":
			d, err := rotationPeriodOf(in)
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			period, setPeriod = d, true
		case "next_rotation_time":
			t, err := nextRotationOf(in)
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			nextRotation, setNext = t, true
		case "key_access_justifications_policy":
			pol, err := kajPolicyOf(in.GetKeyAccessJustificationsPolicy())
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			kaj, setKAJ = pol, true
		case "purpose", "destroy_scheduled_duration", "import_only", "crypto_key_backend":
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q names an immutable field", p))
		case "name", "primary", "create_time":
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q names an output-only field", p))
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q is not a CryptoKey field", p))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, in.GetName()), &k, "CryptoKey", in.GetName()); err != nil {
		return nil, err
	}
	if setLabels {
		k.Labels = in.GetLabels()
		if len(k.Labels) == 0 {
			k.Labels = nil
		}
	}
	if setPeriod {
		k.RotationPeriod = period
	}
	if setNext {
		k.NextRotation = nextRotation
	}
	// A key keeps the protection level it was created with: the template
	// may name it again, not another (the code is UNVERIFIED).
	if setLevel && level != k.protectionLevel() {
		return nil, apierror.Wrap(apierror.InvalidArgument("version_template.protection_level cannot change from %s to %s", k.protectionLevel(), level))
	}
	if setKAJ {
		k.KAJPolicy = kaj
	}
	if setTemplate {
		// A template names an algorithm of the key's own purpose; new
		// versions get it, existing ones keep theirs (resources.proto
		// version_template). The codes are UNVERIFIED.
		a, err := levelAlgorithm(k.protectionLevel(), k.purpose(), templateAlg)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		k.Algorithm = storedAlgorithm(a)
	}
	if (k.RotationPeriod > 0 || !k.NextRotation.IsZero()) && k.purpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return nil, apierror.Wrap(apierror.InvalidArgument("automatic rotation is only supported for ENCRYPT_DECRYPT keys, not %s", k.purpose()))
	}
	if (k.RotationPeriod > 0 || !k.NextRotation.IsZero()) && k.manualExternal() {
		return nil, apierror.Wrap(apierror.Unimplemented("automatic rotation is not implemented for %s keys whose versions name their own external key: Google documents it for symmetric coordinated external keys only", k.protectionLevel()))
	}
	// The schedule is checked as it will be stored, so a period set on a key
	// that already has a next_rotation_time needs no second path, and
	// clearing next_rotation_time under a period is refused. Every key here
	// is ENCRYPT_DECRYPT (CreateCryptoKey refuses the rest), the purpose that
	// supports automatic rotation.
	if err := checkSchedule(k.RotationPeriod, k.NextRotation); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	if setPeriod || setNext {
		s.wake()
	}
	return s.toKey(k)
}

// checkView accepts the defined views. FULL adds only attestation, which
// exists for HSM versions (resources.proto CryptoKeyVersionView); an HSM
// version here has no attestation to show, since no HSM made it
// (protection.go, #1452), so both views return the same fields. Another
// value's code is UNVERIFIED.
func checkView(field string, v kmspb.CryptoKeyVersion_CryptoKeyVersionView) error {
	switch v {
	case kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_VIEW_UNSPECIFIED, kmspb.CryptoKeyVersion_FULL:
		return nil
	}
	return apierror.InvalidArgument("%s %d is not a CryptoKeyVersionView", field, v)
}

// parseOrderBy accepts the orders the sorting-and-filtering documentation
// shows for these lists: name ascending (also the default) and "name desc".
// Anything else is INVALID_ARGUMENT naming it (UNVERIFIED).
func parseOrderBy(orderBy string) (desc bool, err error) {
	switch strings.Join(strings.Fields(orderBy), " ") {
	case "", "name", "name asc":
		return false, nil
	case "name desc":
		return true, nil
	}
	return false, apierror.InvalidArgument("order_by %q is not supported: use name or name desc", orderBy)
}

func byName(a, b string) bool { return a < b }

func byVersionNumber(a, b string) bool {
	x, _ := strconv.Atoi(a[strings.LastIndex(a, "/")+1:])
	y, _ := strconv.Atoi(b[strings.LastIndex(b, "/")+1:])
	return x < y
}

// orderedPage pages names in the given order. It pages over ordinal keys,
// since paging.Page sorts what it is given, and binds the page token to the
// order, so a token from one order cannot resume another and skip results.
func orderedPage(scope string, names []string, less func(a, b string) bool, desc bool, token string, size int) ([]string, string, error) {
	sorted := append([]string(nil), names...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if desc {
			return less(sorted[j], sorted[i])
		}
		return less(sorted[i], sorted[j])
	})
	keys := make([]string, len(sorted))
	byKey := make(map[string]string, len(sorted))
	for i, n := range sorted {
		keys[i] = fmt.Sprintf("%010d", i)
		byKey[keys[i]] = n
	}
	order := "asc"
	if desc {
		order = "desc"
	}
	page, next, err := paging.Page(scope+"|order="+order, keys, token, size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	out := make([]string, len(page))
	for i, k := range page {
		out[i] = byKey[k]
	}
	return out, next, nil
}

// The fields each list can filter on, and with which operators (#408).
var (
	ringFilterFields    = fieldSupport{"name": {"=", "!=", ":"}}
	keyFilterFields     = fieldSupport{"name": {"=", "!=", ":"}, "labels.": {"=", ":"}}
	versionFilterFields = fieldSupport{"state": {"=", "!="}}
)

// Locations returns the Locations mixin over s's key rings (#1291).
func (s *Server) Locations() locationpb.LocationsServer { return locationsServer{s: s} }
