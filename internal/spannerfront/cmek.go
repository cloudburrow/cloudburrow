package spannerfront

// Customer-managed encryption keys (#1950), as
// https://docs.cloud.google.com/spanner/docs/cmek documents them, through
// the shared helper internal/cmek and the instance's Cloud KMS (--kms):
//
//   - CreateDatabase's encryption_config names the key (kms_key_name, or
//     kms_key_names, one a region of a multi-region configuration); the key
//     must exist in Cloud KMS with an ENABLED primary version, and the
//     Spanner service agent,
//     service-PROJECT_NUMBER@gcp-sa-spanner.iam.gserviceaccount.com, needs
//     roles/cloudkms.cryptoKeyEncrypterDecrypter on it (checked by the
//     host's evaluator with `up --iam-enforcement=on`). The database gets a
//     data key wrapped by the key's primary version, which
//     encryption_info.kms_key_version reports. "If the key is disabled or
//     destroyed, Spanner immediately begins making the database
//     inaccessible": while the version cannot unwrap the data key, the
//     database's sessions and the calls made on them are refused
//     FAILED_PRECONDITION "KMS key required by the Spanner resource is not
//     accessible.", and they work again once it is enabled. The
//     emulator's own storage of the rows is not encrypted with the key.
//   - A backup "uses its database's encryption configuration by default"
//     (USE_DATABASE_ENCRYPTION), or Google's (GOOGLE_DEFAULT_ENCRYPTION), or
//     a key of its own (CUSTOMER_MANAGED_ENCRYPTION); a copy and a restored
//     database use the backup's unless told otherwise
//     (USE_CONFIG_DEFAULT_OR_BACKUP_ENCRYPTION). A customer-managed backup
//     is really encrypted: its schema and rows are sealed with a fresh data
//     key wrapped by the key's primary version, in memory and in the
//     front's state, and "restoring a CMEK backup requires the key and key
//     version used to encrypt it to be available": a restore or copy asks
//     Cloud KMS to unwrap the data key, and is refused
//     FAILED_PRECONDITION with Google's text while the version is disabled
//     or destroyed. The version is fixed at creation, even after rotation.
//   - A key must be in its instance configuration's location: the region
//     of a regional configuration, and for a multi-region one either the
//     multi-region itself (nam3) or kms_key_names with one key in each of
//     its read-write and witness regions. The emulator's own configuration
//     has no location, so any key is accepted.
//
// Choices Google does not document, pinned by unit tests (UNVERIFIED): the
// texts after Google's sentence, a bad key name or location
// INVALID_ARGUMENT, and a service agent without the role
// FAILED_PRECONDITION with Google's sentence. Docs read 2026-11-19:
// https://docs.cloud.google.com/spanner/docs/cmek

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// spannerAgentDomain is the Spanner service agent's domain.
const spannerAgentDomain = "gcp-sa-spanner.iam.gserviceaccount.com"

// errKeyText is Google's documented message for a key Spanner cannot use.
const errKeyText = "KMS key required by the Spanner resource is not accessible."

// dbKey is a customer-managed database's keys and the data key each
// wrapped.
type dbKey struct {
	Keys   []string      `json:"keys"`
	Sealed []cmek.Sealed `json:"sealed"`
}

// sealedSnap is a customer-managed backup's snapshot: Sealed[0] holds the
// encoded snapshot, sealed; the others wrap a data key of their own, so
// every key must be usable to open it.
type sealedSnap struct {
	Keys   []string      `json:"keys"`
	Sealed []cmek.Sealed `json:"sealed"`
}

// UseKeys makes customer-managed keys work through k.
func (s *Instances) UseKeys(k cmek.Keys) {
	s.mu.Lock()
	s.keys = &k
	s.mu.Unlock()
}

func (s *Instances) keyring() (cmek.Keys, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		return cmek.Keys{}, false
	}
	return *s.keys, true
}

// projectOf is a resource name's project.
func projectOf(name string) string {
	p := strings.Split(name, "/")
	if len(p) > 1 {
		return p[1]
	}
	return ""
}

func spannerAgent(name string) string { return cmek.ServiceAgent(projectOf(name), spannerAgentDomain) }

// keyStatus is a key refusal as Spanner answers it.
func keyStatus(method string, err error) error {
	e := cmek.As(err)
	if e == nil {
		return err
	}
	switch e.Kind {
	case cmek.Invalid:
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: encryption_config: %s", method, e.Error())
	case cmek.NoKMS:
		return status.Errorf(codes.FailedPrecondition, "%s %s", errKeyText, e.Error())
	case cmek.Unavailable:
		return status.Errorf(codes.Unavailable, "%s %s", errKeyText, e.Error())
	}
	return status.Errorf(codes.FailedPrecondition, "%s %s", errKeyText, e.Error())
}

// keyNames is a config's keys: kms_key_name, or kms_key_names.
func keyNames(method, one string, many []string) ([]string, error) {
	if one != "" && len(many) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: encryption_config sets both kms_key_name and kms_key_names.", method)
	}
	if one != "" {
		return []string{one}, nil
	}
	return append([]string(nil), many...), nil
}

// resolveKeys is the keys a new resource is encrypted with, given the
// config's type (its enum name), its keys and the keys it inherits.
func resolveKeys(method, typ string, keys, inherited []string) ([]string, error) {
	switch {
	case strings.HasSuffix(typ, "GOOGLE_DEFAULT_ENCRYPTION"):
		if len(keys) > 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: kms_key_name must not be set unless encryption_type is CUSTOMER_MANAGED_ENCRYPTION.", method)
		}
		return nil, nil
	case strings.HasSuffix(typ, "CUSTOMER_MANAGED_ENCRYPTION"):
		if len(keys) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: kms_key_name is required when encryption_type is CUSTOMER_MANAGED_ENCRYPTION.", method)
		}
		return keys, nil
	}
	if len(keys) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: kms_key_name must not be set unless encryption_type is CUSTOMER_MANAGED_ENCRYPTION.", method)
	}
	return inherited, nil
}

// checkKeyNames checks each key names a CryptoKey.
func checkKeyNames(method string, keys []string) error {
	for _, k := range keys {
		if err := cmek.CheckName("kms_key_name", k, ""); err != nil {
			return keyStatus(method, err)
		}
	}
	return nil
}

// checkKeyLocations checks keys against the location of instance's
// configuration.
func (s *Instances) checkKeyLocations(ctx context.Context, method, instance string, keys []string) error {
	if err := checkKeyNames(method, keys); err != nil || len(keys) == 0 {
		return err
	}
	inst, err := s.GetInstance(ctx, instanceReq(instance))
	if err != nil {
		return err
	}
	loc := s.configLocation(inst.GetConfig())
	if loc == "" {
		return nil
	}
	locs := make([]string, 0, len(keys))
	for _, k := range keys {
		l, _ := cmek.Location(k)
		locs = append(locs, l)
	}
	if mr, ok := multiRegions[loc]; ok {
		if len(keys) == 1 && locs[0] == loc {
			return nil
		}
		want := append(append([]string{}, mr.rw...), mr.witness)
		sort.Strings(want)
		got := append([]string{}, locs...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return status.Errorf(codes.InvalidArgument, "Invalid %s request: encryption_config: the instance's configuration is %s: name one key in %s, or kms_key_names with one key in each of %s (got %s).",
				method, loc, loc, strings.Join(want, ", "), strings.Join(locs, ", "))
		}
		return nil
	}
	if len(keys) != 1 || locs[0] != loc {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: encryption_config: the key must be in the instance configuration's region, %s (got %s).",
			method, loc, strings.Join(locs, ", "))
	}
	return nil
}

// wrapKeys checks each key and wraps a data key with it for name: the
// first seals plaintext when it is not nil.
func (s *Instances) wrapKeys(ctx context.Context, method, name string, keys []string, plaintext []byte) ([]cmek.Sealed, error) {
	k, ok := s.keyring()
	if !ok {
		k = cmek.Keys{}
	}
	project, agent := projectOf(name), spannerAgent(name)
	out := make([]cmek.Sealed, 0, len(keys))
	for i, key := range keys {
		if err := k.Usable(ctx, project, agent, key); err != nil {
			return nil, keyStatus(method, err)
		}
		var pt []byte
		if i == 0 {
			pt = plaintext
		}
		sl, err := k.Seal(ctx, project, agent, key, pt, []byte(name))
		if err != nil {
			return nil, keyStatus(method, err)
		}
		out = append(out, sl)
	}
	return out, nil
}

// openKeys unwraps each sealed data key of name; the first's plaintext is
// returned.
func (s *Instances) openKeys(ctx context.Context, method, name string, sealed []cmek.Sealed) ([]byte, error) {
	k, _ := s.keyring()
	var first []byte
	for i, sl := range sealed {
		pt, err := k.Open(ctx, projectOf(name), spannerAgent(name), sl, []byte(name))
		if err != nil {
			return nil, keyStatus(method, err)
		}
		if i == 0 {
			first = pt
		}
	}
	return first, nil
}

// encryptionInfos are the encryption_info of sealed values.
func encryptionInfos(sealed []cmek.Sealed) []*databasepb.EncryptionInfo {
	out := make([]*databasepb.EncryptionInfo, 0, len(sealed))
	for _, sl := range sealed {
		out = append(out, &databasepb.EncryptionInfo{EncryptionType: databasepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION, KmsKeyVersion: sl.KeyVersion})
	}
	return out
}

// databaseKeys are database's keys, nil for Google-managed encryption.
func (s *Instances) databaseKeys(database string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.dbKeys[database]; k != nil {
		return append([]string(nil), k.Keys...)
	}
	return nil
}

// recordDatabaseKeys wraps a data key for database with keys, before it is
// created.
func (s *Instances) databaseKeyRecord(ctx context.Context, method, database string, keys []string) (*dbKey, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	sealed, err := s.wrapKeys(ctx, method, database, keys, nil)
	if err != nil {
		return nil, err
	}
	return &dbKey{Keys: keys, Sealed: sealed}, nil
}

// overlayKeys sets a customer-managed database's encryption_config and
// encryption_info. s.mu is held.
func (s *Instances) overlayKeys(db *databasepb.Database) {
	k := s.dbKeys[db.GetName()]
	if k == nil {
		return
	}
	c := &databasepb.EncryptionConfig{}
	if len(k.Keys) == 1 {
		c.KmsKeyName = k.Keys[0]
	} else {
		c.KmsKeyNames = append([]string(nil), k.Keys...)
	}
	db.EncryptionConfig = c
	db.EncryptionInfo = encryptionInfos(k.Sealed)
}

// databaseAccessible refuses a call on a customer-managed database whose
// key cannot unwrap its data key.
func (s *Instances) databaseAccessible(ctx context.Context, database string) error {
	s.mu.Lock()
	k := s.dbKeys[database]
	s.mu.Unlock()
	if k == nil {
		return nil
	}
	_, err := s.openKeys(ctx, "the call", database, k.Sealed)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
			return status.Error(codes.FailedPrecondition, errKeyText+" "+st.Message())
		}
	}
	return err
}

// hasDatabaseKeys reports whether any database has a customer-managed key.
func (s *Instances) hasDatabaseKeys() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dbKeys) > 0
}

// sealSnapshot seals sn for backup name with keys.
func (s *Instances) sealSnapshot(ctx context.Context, method, name string, keys []string, sn *snapshot) (*sealedSnap, error) {
	raw, err := encodeSnapshot(sn)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding the backup: %v", err)
	}
	sealed, err := s.wrapKeys(ctx, method, name, keys, raw)
	if err != nil {
		return nil, err
	}
	return &sealedSnap{Keys: keys, Sealed: sealed}, nil
}

// openSnapshot is backup b's snapshot: a customer-managed one is opened
// through Cloud KMS.
func (s *Instances) openSnapshot(ctx context.Context, method, name string, b *backup) (*snapshot, error) {
	if b.sealed == nil {
		return b.snap, nil
	}
	raw, err := s.openKeys(ctx, method, name, b.sealed.Sealed)
	if err != nil {
		return nil, err
	}
	sn, err := decodeSnapshot(raw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decoding the backup %s: %v", name, err)
	}
	return sn, nil
}

// backupKeys are backup b's keys, nil for Google-managed encryption.
func (b *backup) keys() []string {
	if b == nil || b.sealed == nil {
		return nil
	}
	return append([]string(nil), b.sealed.Keys...)
}

// setBackupEncryption fills pb's encryption_info for b.
func setBackupEncryption(pb *databasepb.Backup, b *sealedSnap) {
	if b == nil {
		pb.EncryptionInfo = &databasepb.EncryptionInfo{EncryptionType: databasepb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION}
		pb.EncryptionInformation = nil
		return
	}
	infos := encryptionInfos(b.Sealed)
	pb.EncryptionInfo = infos[0]
	pb.EncryptionInformation = infos
}

// sealedMagic starts a customer-managed backup's copy file.
const sealedMagic = "cloudburrow-spanner-cmek-backup/1\n"

func encodeSealed(b *sealedSnap) ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	return append([]byte(sealedMagic), raw...), nil
}

// decodeSealed is a copy file's sealed snapshot, or nil when it holds a
// plain one.
func decodeSealed(raw []byte) (*sealedSnap, error) {
	rest, ok := strings.CutPrefix(string(raw), sealedMagic)
	if !ok {
		return nil, nil
	}
	var b sealedSnap
	if err := json.Unmarshal([]byte(rest), &b); err != nil {
		return nil, err
	}
	return &b, nil
}
