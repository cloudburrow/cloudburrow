package firestoreadmin

// Customer-managed encryption keys (#1881).
//
// A database created with cmek_config names a Cloud KMS key, which is
// checked against the instance's own Cloud KMS: the CLI serves it on the
// host, and the front reaches it over the cluster's host route
// (cloudburrow-host, --kms). As on Google
// (https://docs.cloud.google.com/firestore/native/docs/use-cmek), the key
// must be in the database's location (Cloud KMS's `us` for nam5, `europe`
// for eur3, the same name for a region), must be a symmetric
// ENCRYPT_DECRYPT key, and its primary version must be enabled; the
// database reports the version it was created with as
// cmek_config.active_key_version, with the primary of a rotated key added
// once it is in use.
//
// The emulator does not encrypt, but the key's state is enforced: while a
// version in use is disabled, scheduled for destruction or destroyed, every
// data call of the database, through the Firestore front or the Datastore
// front, is refused with Google's FAILED_PRECONDITION, "The
// customer-managed encryption key required by the requested resource is not
// accessible." (https://docs.cloud.google.com/firestore/native/docs/cmek),
// and served again once it is enabled. Google polls Cloud KMS every five
// minutes; the front asks it at most every keyCheckInterval.

import (
	"context"
	"regexp"
	"slices"
	"sync"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// keyInaccessible is Google's message for a database whose key cannot be
// used.
const keyInaccessible = "The customer-managed encryption key required by the requested resource is not accessible."

// keyCheckInterval is how long a key's state is trusted before Cloud KMS
// is asked again.
const keyCheckInterval = time.Second

var kmsKeyNameRe = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)$`)

// keyService is the part of Cloud KMS the API reads.
type keyService interface {
	GetCryptoKey(ctx context.Context, in *kmspb.GetCryptoKeyRequest, opts ...grpc.CallOption) (*kmspb.CryptoKey, error)
	GetCryptoKeyVersion(ctx context.Context, in *kmspb.GetCryptoKeyVersionRequest, opts ...grpc.CallOption) (*kmspb.CryptoKeyVersion, error)
}

// keyChecks is the API's link to Cloud KMS and what it last learned.
type keyChecks struct {
	mu   sync.Mutex
	kms  keyService
	conn *grpc.ClientConn
	// checked is each database's last verdict, by name.
	checked map[string]keyVerdict
}

type keyVerdict struct {
	at  time.Time
	err error
}

// SetKMS has the API check keys against the Cloud KMS at addr
// (host:port, gRPC); "" leaves cmek_config refused.
func (a *Admin) SetKMS(addr string) error {
	if addr == "" {
		return nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	a.keys.mu.Lock()
	defer a.keys.mu.Unlock()
	a.keys.conn, a.keys.kms = conn, kmspb.NewKeyManagementServiceClient(conn)
	return nil
}

// useKeys is SetKMS with a client of the caller's (tests).
func (a *Admin) useKeys(k keyService) {
	a.keys.mu.Lock()
	defer a.keys.mu.Unlock()
	a.keys.kms = k
}

func (a *Admin) keyService() keyService {
	a.keys.mu.Lock()
	defer a.keys.mu.Unlock()
	return a.keys.kms
}

// kmsLocation is the Cloud KMS location a database location's keys are in.
func kmsLocation(databaseLocation string) string {
	switch databaseLocation {
	case "nam5":
		return "us"
	case "eur3":
		return "europe"
	}
	return databaseLocation
}

// checkKey checks the key a new database in location is to be encrypted
// with, and returns its config as the database reports it: the key and its
// primary version.
func (a *Admin) checkKey(ctx context.Context, keyName, location string) (*adminpb.Database_CmekConfig, error) {
	m := kmsKeyNameRe.FindStringSubmatch(keyName)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid kms_key_name %q: the expected format is projects/{project_id}/locations/{kms_location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}.", keyName)
	}
	if want := kmsLocation(location); m[2] != want {
		return nil, status.Errorf(codes.InvalidArgument, "The KMS key %s is in location %s, but a database in %s must use a key in %s: only keys in the same location as the database are allowed to be used for encryption.", keyName, m[2], location, want)
	}
	k := a.keyService()
	if k == nil {
		return nil, status.Error(codes.FailedPrecondition, "Cloud KMS is not served by this instance: enable the kms service to create a database with cmek_config.")
	}
	key, err := k.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: keyName})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.FailedPrecondition, "%s The KMS key %s does not exist.", keyInaccessible, keyName)
		}
		return nil, status.Errorf(codes.Unavailable, "Cloud KMS did not answer for %s: %v", keyName, err)
	}
	if key.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return nil, status.Errorf(codes.InvalidArgument, "The KMS key %s has purpose %s: a database's key must be a symmetric ENCRYPT_DECRYPT key.", keyName, key.GetPurpose())
	}
	primary := key.GetPrimary()
	if primary.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition, "%s The primary version of %s is %s.", keyInaccessible, keyName, orNone(primary.GetState().String(), primary == nil))
	}
	return &adminpb.Database_CmekConfig{KmsKeyName: keyName, ActiveKeyVersion: []string{primary.GetName()}}, nil
}

func orNone(s string, none bool) string {
	if none {
		return "missing"
	}
	return s
}

// encryptionFor is the cmek_config of a database restored or cloned with
// cfg from a source encrypted with source (nil: Google's default
// encryption), in location.
func (a *Admin) encryptionFor(ctx context.Context, cfg *adminpb.Database_EncryptionConfig, source *adminpb.Database_CmekConfig, location string) (*adminpb.Database_CmekConfig, error) {
	switch {
	case cfg.GetCustomerManagedEncryption() != nil:
		return a.checkKey(ctx, cfg.GetCustomerManagedEncryption().GetKmsKeyName(), location)
	case cfg.GetGoogleDefaultEncryption() != nil:
		return nil, nil
	}
	// use_source_encryption, the default.
	if source == nil {
		return nil, nil
	}
	return proto.Clone(source).(*adminpb.Database_CmekConfig), nil
}

// keyAccess is the refusal a data call of the database gets for its key,
// or nil: a version in use that is not enabled makes it inaccessible. It
// asks Cloud KMS at most every keyCheckInterval.
func (a *Admin) keyAccess(ctx context.Context, name string) error {
	a.mu.Lock()
	db, ok := a.databases[name]
	var cfg *adminpb.Database_CmekConfig
	if ok && db.DeleteTime == nil && db.GetCmekConfig() != nil {
		cfg = proto.Clone(db.GetCmekConfig()).(*adminpb.Database_CmekConfig)
	}
	a.mu.Unlock()
	if cfg == nil {
		return nil
	}
	now := a.now()
	a.keys.mu.Lock()
	if v, ok := a.keys.checked[name]; ok && now.Sub(v.at) < keyCheckInterval {
		a.keys.mu.Unlock()
		return v.err
	}
	a.keys.mu.Unlock()
	rotated, err := a.askKey(ctx, cfg)
	a.keys.mu.Lock()
	if a.keys.checked == nil {
		a.keys.checked = map[string]keyVerdict{}
	}
	a.keys.checked[name] = keyVerdict{at: now, err: err}
	a.keys.mu.Unlock()
	if rotated != "" {
		a.mu.Lock()
		if db, ok := a.databases[name]; ok && db.GetCmekConfig() != nil && !slices.Contains(db.CmekConfig.ActiveKeyVersion, rotated) {
			db.CmekConfig.ActiveKeyVersion = append(db.CmekConfig.ActiveKeyVersion, rotated)
		}
		a.mu.Unlock()
		a.persist()
	}
	return err
}

// askKey asks Cloud KMS for cfg's versions: the key's primary version when
// it is enabled and not yet in use, and the refusal, or nil.
func (a *Admin) askKey(ctx context.Context, cfg *adminpb.Database_CmekConfig) (string, error) {
	k := a.keyService()
	if k == nil {
		return "", status.Error(codes.FailedPrecondition, keyInaccessible)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, v := range cfg.GetActiveKeyVersion() {
		ver, err := k.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v})
		if err != nil && status.Code(err) != codes.NotFound {
			return "", status.Errorf(codes.Unavailable, "Cloud KMS did not answer for %s: %v", v, err)
		}
		if err != nil || ver.GetState() != kmspb.CryptoKeyVersion_ENABLED {
			return "", status.Error(codes.FailedPrecondition, keyInaccessible)
		}
	}
	key, err := k.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: cfg.GetKmsKeyName()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", status.Error(codes.FailedPrecondition, keyInaccessible)
		}
		return "", status.Errorf(codes.Unavailable, "Cloud KMS did not answer for %s: %v", cfg.GetKmsKeyName(), err)
	}
	if p := key.GetPrimary(); p.GetState() == kmspb.CryptoKeyVersion_ENABLED && !slices.Contains(cfg.GetActiveKeyVersion(), p.GetName()) {
		return p.GetName(), nil
	}
	return "", nil
}

// forgetKey drops the database's last key verdict.
func (a *Admin) forgetKey(name string) {
	a.keys.mu.Lock()
	defer a.keys.mu.Unlock()
	delete(a.keys.checked, name)
}
