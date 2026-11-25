package compute

// Customer-managed encryption keys on disks (#2206), as
// https://docs.cloud.google.com/compute/docs/disks/customer-managed-encryption
// documents them (read 2026-11-24):
//
//   - diskEncryptionKey.kmsKeyName on disks.insert and regionDisks.insert
//     (and an instance's attachedDisks[].diskEncryptionKey) names a Cloud
//     KMS CryptoKey of this instance's Cloud KMS: "A key in the same region
//     as the disk", "A multi-regional key in the same geographical location
//     as the disk" or "A key in the global location";
//   - the disk gets a data key of its own, 256-bit AES, wrapped (Encrypt)
//     by the CryptoKey's primary version and kept only wrapped; the disk
//     answers kmsKeyName with that version (…/cryptoKeyVersions/N), as
//     Google's does (the Terraform provider's decoder strips the suffix);
//   - "You can't encrypt existing resources with CMEKs", and a CMEK can't
//     be removed: disks.updateKmsKey and regionDisks.updateKmsKey rotate a
//     CMEK disk to its key's primary version, or change it to the key the
//     body names ("changing keys doesn't alter the data encryption key"),
//     re-wrapping the data key; a disabled or destroyed version (the one
//     that wrapped the data key, or the new primary) refuses it;
//   - "You can't attach a disk with a revoked key to a VM or create a
//     snapshot from the disk", "You can't boot a VM if any of its attached
//     disks have revoked keys", and a disk can't be cloned from one: each
//     unwraps the data key first;
//   - "Keys can't be changed for Hyperdisk volumes created by cloning,
//     unless the source disk and all other clones have been deleted."
//
// The disk's storage is encrypted at rest with the data key: sealed
// (diskseal.go) whenever no running instance has it open, so a disabled or
// destroyed key version leaves nothing readable.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// KeyService is Cloud KMS: Encrypt with a CryptoKey's primary version,
// answering the version, and Decrypt.
type KeyService interface {
	Encrypt(ctx context.Context, key string, plaintext []byte) (ciphertext []byte, version string, err error)
	Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error)
}

// UseKMS makes diskEncryptionKey.kmsKeyName work, over k: without it, a
// disk with a key is refused, as Cloud KMS is not enabled.
func (s *Server) UseKMS(k KeyService) {
	s.mu.Lock()
	s.kms = k
	s.mu.Unlock()
}

// wrappedKeyKey is a CMEK disk's wrapped data key (private).
const wrappedKeyKey = "_wrappedKey"

var diskCryptoKeyRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)$`)

// kmsMultiRegions are Cloud KMS's multi-regional locations, by the prefix
// of the regions in them.
var kmsMultiRegions = map[string]string{"us": "us-", "europe": "europe-", "asia": "asia-"}

// cryptoKeyOf is a kmsKeyName without its /cryptoKeyVersions/N suffix.
func cryptoKeyOf(name string) string {
	if i := strings.Index(name, "/cryptoKeyVersions/"); i >= 0 {
		return name[:i]
	}
	return name
}

// checkDiskKeyName validates a disk's kmsKeyName: its form and location.
func checkDiskKeyName(key, region, field string) error {
	m := diskCryptoKeyRE.FindStringSubmatch(key)
	if m == nil {
		return invalid("Invalid value for field '%s': '%s'. The key name must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}.", field, key)
	}
	loc := m[2]
	if loc == "global" || loc == region || (kmsMultiRegions[loc] != "" && strings.HasPrefix(region, kmsMultiRegions[loc])) {
		return nil
	}
	return invalid("Invalid value for field '%s': '%s'. The Cloud KMS key must be in the same region as the disk (%s), in a multi-region that contains it, or global.", field, key, region)
}

// kmsLocked is Cloud KMS, or Google's refusal when it is not enabled.
func (s *Server) kmsLocked(key string) (KeyService, error) {
	if s.kms == nil {
		return nil, errf(http.StatusBadRequest, "invalid",
			"Cloud KMS error when using key %s: Cloud Key Management Service (KMS) API has not been used in this project before or it is disabled: run up with kms in --services.", key)
	}
	return s.kms, nil
}

func kmsError(key string, err error) error {
	return errf(http.StatusBadRequest, "invalid", "Cloud KMS error when using key %s: %s", key, err.Error())
}

func kmsCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// applyDiskKeyLocked gives a new disk d the key b names: a
// customer-managed key (b.diskEncryptionKey.kmsKeyName), wrapping a new
// data key with it, or a customer-supplied one (rawKey, rsaEncryptedKey,
// diskkeys.go). A disk made from a disk or snapshot with a key unwraps the
// source's data key first: a customer-supplied one needs
// sourceSnapshotEncryptionKey.
func (s *Server) applyDiskKeyLocked(d, b obj, field string) error {
	if src := str(d, "sourceDisk"); src != "" {
		if so, ok := s.res[src]; ok {
			if isCSEK(so) {
				return invalid("Invalid value for field '%s.sourceDisk': '%s'. A disk encrypted with a customer-supplied encryption key can't be cloned.", field, src)
			}
			if _, err := s.diskKeyLocked(so, nil, field+".sourceDisk"); err != nil {
				return err
			}
		}
	}
	if src := str(d, "sourceSnapshot"); src != "" {
		if so, ok := s.res[src]; ok {
			sk, _ := b["sourceSnapshotEncryptionKey"].(map[string]any)
			if _, err := s.diskKeyLocked(so, sk, field+".sourceSnapshotEncryptionKey"); err != nil {
				return err
			}
		}
	}
	ek, _ := b["diskEncryptionKey"].(map[string]any)
	key := str(ek, "kmsKeyName")
	n := 0
	for _, f := range []string{"kmsKeyName", "rawKey", "rsaEncryptedKey"} {
		if str(ek, f) != "" {
			n++
		}
	}
	if n > 1 {
		return invalid("Invalid value for field '%s.diskEncryptionKey': at most one of kmsKeyName, rawKey and rsaEncryptedKey may be set.", field)
	}
	if n == 0 {
		return nil
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	if key == "" {
		ck, err := s.customerKeyLocked(ek, field+".diskEncryptionKey")
		if err != nil {
			return err
		}
		w, err := wrapCustomer(ck, dek)
		if err != nil {
			return err
		}
		d["diskEncryptionKey"] = obj{"sha256": csekSHA256(ck)}
		d[wrappedKeyKey] = w
		d[csekKey] = true
		s.rememberKeyLocked(w, dek)
		return nil
	}
	if err := checkDiskKeyName(key, diskRegion(d), field+".diskEncryptionKey.kmsKeyName"); err != nil {
		return err
	}
	k, err := s.kmsLocked(key)
	if err != nil {
		return err
	}
	ctx, cancel := kmsCtx()
	defer cancel()
	wrapped, version, err := k.Encrypt(ctx, key, dek)
	if err != nil {
		return kmsError(key, err)
	}
	out := obj{"kmsKeyName": version}
	if sa := str(ek, "kmsKeyServiceAccount"); sa != "" {
		out["kmsKeyServiceAccount"] = sa
	}
	w := base64.StdEncoding.EncodeToString(wrapped)
	d["diskEncryptionKey"] = out
	d[wrappedKeyKey] = w
	s.rememberKeyLocked(w, dek)
	return nil
}

// checkDiskKeyLocked checks that a disk's (or snapshot's) data key can be
// unwrapped: an error when its Cloud KMS key version is disabled or
// destroyed, or, for a customer-supplied key, when ck (the
// CustomerEncryptionKey the request gave, field) is missing or wrong.
func (s *Server) checkDiskKeyLocked(d obj, ck map[string]any, field string) error {
	_, err := s.diskKeyLocked(d, ck, field)
	return err
}

// diskKeyLocked unwraps d's data key, nil for a disk without one: with
// Cloud KMS (Decrypt, so a disabled or destroyed version refuses it) or
// the customer's key ck. The key is remembered (openKeys) so the disk can
// be sealed again when the instance that opened it stops.
func (s *Server) diskKeyLocked(d obj, ck map[string]any, field string) ([]byte, error) {
	w := str(d, wrappedKeyKey)
	if w == "" {
		if gk := str(d, googleKeyKey); gk != "" {
			return base64.StdEncoding.DecodeString(gk)
		}
		return nil, nil
	}
	if isCSEK(d) {
		return s.openCustomerLocked(d, ck, field)
	}
	ek, _ := d["diskEncryptionKey"].(map[string]any)
	if ek == nil {
		ek, _ = d["snapshotEncryptionKey"].(map[string]any) // a snapshot's
	}
	key := cryptoKeyOf(str(ek, "kmsKeyName"))
	k, err := s.kmsLocked(key)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(w)
	if err != nil {
		return nil, err
	}
	ctx, cancel := kmsCtx()
	defer cancel()
	dek, err := k.Decrypt(ctx, key, ct)
	if err != nil {
		return nil, kmsError(key, err)
	}
	s.rememberKeyLocked(w, dek)
	return dek, nil
}

// checkInstanceKeysLocked checks the keys of an instance's disks; keys are
// the customer-supplied keys startWithEncryptionKey gave, by disk.
func (s *Server) checkInstanceKeysLocked(o obj, keys map[string]map[string]any) error {
	l, _ := o["disks"].([]any)
	for i, e := range l {
		ad, _ := e.(map[string]any)
		src := Rel(str(ad, "source"), "")
		if d, ok := s.res[src]; ok {
			if err := s.checkDiskKeyLocked(d, keys[src], "disks["+strconv.Itoa(i)+"].diskEncryptionKey"); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateDiskKmsKeyLocked is disks.updateKmsKey and
// regionDisks.updateKmsKey.
func (s *Server) updateDiskKmsKeyLocked(project, scope, rel string, d, b obj) (obj, error) {
	w := str(d, wrappedKeyKey)
	if isCSEK(d) {
		// KMS_UPDATE_NOT_SUPPORTED_FOR_CSEK_RESOURCE (the error catalog).
		return nil, invalid("Can't update the KMS key for 'disk' resource '%s' because it is protected by a Customer-Supplied Encryption Key (CSEK). To use CMEK for a 'disk', create a new resource and provide a KMS key during creation.", str(d, "name"))
	}
	if w == "" {
		// KMS_UPDATE_NOT_SUPPORTED_FOR_NON_KMS_PROTECTED_RESOURCE
		// (https://docs.cloud.google.com/compute/docs/reference/rest/v1/errors).
		name := str(d, "name")
		return nil, invalid("Cannot update the KMS key for the 'disk' '%s'. KMS key updates are only supported for resources already protected by Customer-Managed Encryption Keys (CMEK). The resource '%s' is not CMEK protected. To use CMEK for a 'disk', create a new resource and provide a KMS key during creation.", name, name)
	}
	ek, _ := d["diskEncryptionKey"].(map[string]any)
	cur := cryptoKeyOf(str(ek, "kmsKeyName"))
	key := cur
	if n := str(b, "kmsKeyName"); n != "" {
		key = n
		if err := checkDiskKeyName(key, diskRegion(d), "kmsKeyName"); err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(lastSeg(str(d, "type")), "hyperdisk-") && key != cur {
		if clones := s.cloneFamilyLocked(rel, d); len(clones) > 0 {
			return nil, invalid("Invalid resource usage: 'The key of Hyperdisk volume '%s' can't be changed: it was created by cloning, or was cloned, and '%s' still exists.'.", rel, clones[0])
		}
	}
	k, err := s.kmsLocked(key)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(w)
	if err != nil {
		return nil, err
	}
	ctx, cancel := kmsCtx()
	defer cancel()
	dek, err := k.Decrypt(ctx, cur, ct)
	if err != nil {
		return nil, kmsError(cur, err)
	}
	wrapped, version, err := k.Encrypt(ctx, key, dek)
	if err != nil {
		return nil, kmsError(key, err)
	}
	out := obj{"kmsKeyName": version}
	if sa := str(ek, "kmsKeyServiceAccount"); sa != "" {
		out["kmsKeyServiceAccount"] = sa
	}
	d["diskEncryptionKey"] = out
	nw := base64.StdEncoding.EncodeToString(wrapped)
	d[wrappedKeyKey] = nw
	s.rememberKeyLocked(nw, dek)
	return s.done(project, scope, "updateKmsKey", rel, d["id"])
}

// cloneFamilyLocked are the disks related to rel by cloning that still
// exist: its source disk, and the other disks cloned from that source or
// from rel.
func (s *Server) cloneFamilyLocked(rel string, d obj) []string {
	var out []string
	src := str(d, "sourceDisk")
	if _, ok := s.res[src]; ok && src != "" {
		out = append(out, src)
	}
	for k, o := range s.res {
		if k == rel || !strings.Contains(k, "/disks/") {
			continue
		}
		if sd := str(o, "sourceDisk"); sd == rel || (src != "" && sd == src) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
