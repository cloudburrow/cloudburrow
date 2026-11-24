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
// The disk's storage itself is a volume of the VM backend; it is not
// encrypted with the data key (docs/compute.md).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
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

// applyDiskKeyLocked gives a new disk d the customer-managed key b names
// (b.diskEncryptionKey.kmsKeyName), wrapping a new data key with it. A
// disk cloned from a CMEK disk unwraps the source's data key first.
func (s *Server) applyDiskKeyLocked(d, b obj, field string) error {
	for _, src := range []string{str(d, "sourceDisk"), str(d, "sourceSnapshot")} {
		if so, ok := s.res[src]; ok && src != "" {
			if err := s.checkDiskKeyLocked(so); err != nil {
				return err
			}
		}
	}
	ek, _ := b["diskEncryptionKey"].(map[string]any)
	key := str(ek, "kmsKeyName")
	if key == "" {
		return nil
	}
	if str(ek, "rawKey") != "" || str(ek, "rsaEncryptedKey") != "" {
		return invalid("Invalid value for field '%s.diskEncryptionKey': at most one of kmsKeyName, rawKey and rsaEncryptedKey may be set.", field)
	}
	if err := checkDiskKeyName(key, diskRegion(d), field+".diskEncryptionKey.kmsKeyName"); err != nil {
		return err
	}
	k, err := s.kmsLocked(key)
	if err != nil {
		return err
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
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
	d["diskEncryptionKey"] = out
	d[wrappedKeyKey] = base64.StdEncoding.EncodeToString(wrapped)
	return nil
}

// checkDiskKeyLocked unwraps a CMEK disk's data key: an error when its key
// version is disabled or destroyed.
func (s *Server) checkDiskKeyLocked(d obj) error {
	w := str(d, wrappedKeyKey)
	if w == "" {
		return nil
	}
	ek, _ := d["diskEncryptionKey"].(map[string]any)
	if ek == nil {
		ek, _ = d["snapshotEncryptionKey"].(map[string]any) // a snapshot's
	}
	key := cryptoKeyOf(str(ek, "kmsKeyName"))
	k, err := s.kmsLocked(key)
	if err != nil {
		return err
	}
	ct, err := base64.StdEncoding.DecodeString(w)
	if err != nil {
		return err
	}
	ctx, cancel := kmsCtx()
	defer cancel()
	if _, err := k.Decrypt(ctx, key, ct); err != nil {
		return kmsError(key, err)
	}
	return nil
}

// checkInstanceKeysLocked checks the keys of an instance's disks.
func (s *Server) checkInstanceKeysLocked(o obj) error {
	l, _ := o["disks"].([]any)
	for _, e := range l {
		ad, _ := e.(map[string]any)
		if d, ok := s.res[Rel(str(ad, "source"), "")]; ok {
			if err := s.checkDiskKeyLocked(d); err != nil {
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
	d[wrappedKeyKey] = base64.StdEncoding.EncodeToString(wrapped)
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
