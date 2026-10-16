package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// Customer-supplied encryption keys (#1345), as
// docs.cloud.google.com/storage/docs/encryption/customer-supplied-keys
// describes them. A request names a key with three headers, the same in the
// JSON and XML APIs (and, over gRPC, CommonObjectRequestParams):
//
//	x-goog-encryption-algorithm: AES256
//	x-goog-encryption-key: the base64 of a 256-bit key
//	x-goog-encryption-key-sha256: the base64 of the key's SHA-256
//
// and a copy or rewrite names its source's key under
// x-goog-copy-source-encryption-*. Cloud Storage keeps only the key's
// SHA-256 to check a later request against, and so does CloudBurrow: the
// object records it (objectRecord.KeySHA256), and the key itself is never
// stored. The bytes are held as written; what a client sees is Google's
// behaviour: a read without the key, or with another key, is refused, and
// metadata read without the key leaves out crc32c and md5Hash.

const (
	csekHeader       = "X-Goog-Encryption-"
	csekSourceHeader = "X-Goog-Copy-Source-Encryption-"
	csekAlgorithm    = "AES256"
)

// parseCSEK reads the key a request names under prefix, and returns its
// SHA-256, or nil when the request names none.
func parseCSEK(h http.Header, prefix string) ([]byte, error) {
	alg, key, sum := h.Get(prefix+"Algorithm"), h.Get(prefix+"Key"), h.Get(prefix+"Key-Sha256")
	if alg == "" && key == "" && sum == "" {
		return nil, nil
	}
	if alg != csekAlgorithm {
		return nil, errorf(http.StatusBadRequest, "customerEncryptionAlgorithmIsInvalid",
			"Missing an encryption algorithm, or the provided algorithm is not \"AES256\".")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errorf(http.StatusBadRequest, "customerEncryptionKeyFormatIsInvalid",
			"Missing an encryption key, or it is not base64 encoded, or it does not meet the required length of the encryption algorithm.")
	}
	got := sha256.Sum256(raw)
	want, err := base64.StdEncoding.DecodeString(sum)
	if err != nil || !bytes.Equal(want, got[:]) {
		return nil, errorf(http.StatusBadRequest, "customerEncryptionKeySha256IsInvalid",
			"Missing a SHA256 hash of the encryption key, or it is not base64 encoded, or it does not match the encryption key.")
	}
	return got[:], nil
}

// checkCSEK says whether key (a SHA-256, or nil) opens o.
func checkCSEK(o objectRecord, key []byte) error {
	switch {
	case o.KeySHA256 == nil && key == nil:
		return nil
	case o.KeySHA256 == nil:
		return errorf(http.StatusBadRequest, "resourceNotEncryptedWithCustomerEncryptionKey",
			"The target object is not encrypted by a customer-supplied encryption key.")
	case key == nil:
		return errorf(http.StatusBadRequest, "resourceIsEncryptedWithCustomerEncryptionKey",
			"The target object is encrypted by a customer-supplied encryption key.")
	case !bytes.Equal(o.KeySHA256, key):
		return errorf(http.StatusBadRequest, "customerEncryptionKeyIsIncorrect",
			"The provided encryption key is incorrect.")
	}
	return nil
}

// requestOpens reports whether r carries the key o is encrypted with: the
// metadata of an encrypted object read without it has no checksums.
func requestOpens(r *http.Request, o objectRecord) bool {
	if o.KeySHA256 == nil {
		return true
	}
	if r == nil {
		return false
	}
	key, err := parseCSEK(r.Header, csekHeader)
	return err == nil && bytes.Equal(key, o.KeySHA256)
}

// csekJSON is the Object resource's customerEncryption.
func csekJSON(o objectRecord) map[string]any {
	return map[string]any{"encryptionAlgorithm": csekAlgorithm, "keySha256": base64.StdEncoding.EncodeToString(o.KeySHA256)}
}

// csekResponseHeaders echoes the key's algorithm and hash, as Cloud Storage
// does on a write or read with a customer-supplied key.
func csekResponseHeaders(h http.Header, o objectRecord) {
	if o.KeySHA256 != nil {
		h.Set(csekHeader+"Algorithm", csekAlgorithm)
		h.Set(csekHeader+"Key-Sha256", base64.StdEncoding.EncodeToString(o.KeySHA256))
	}
}

// checkReadKey checks the key a read of o carries: a read of the bytes needs
// the object's own key, and a metadata read that names a key needs the
// right one.
func checkReadKey(r *http.Request, o objectRecord) error {
	key, err := parseCSEK(r.Header, csekHeader)
	if err != nil {
		return err
	}
	media := r.URL.Query().Get("alt") == "media" || !strings.HasPrefix(r.URL.Path, jsonPrefix)
	if key != nil || media {
		return checkCSEK(o, key)
	}
	return nil
}

// copyKeys checks a copy's or rewrite's source key against src, and returns
// the destination's key: a rewrite is how a key is added, rotated or removed.
func copyKeys(r *http.Request, src objectRecord) ([]byte, error) {
	srcKey, err := parseCSEK(r.Header, csekSourceHeader)
	if err != nil {
		return nil, err
	}
	if err := checkCSEK(src, srcKey); err != nil {
		return nil, err
	}
	return parseCSEK(r.Header, csekHeader)
}

// sameCSEK reports whether a patch or update body's customerEncryption
// restates o's: AES256 and o's key SHA-256, either field left out meaning
// unchanged. Naming a key on an object with none is a change.
func sameCSEK(v any, o objectRecord) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, f := range m {
		s, isStr := f.(string)
		switch {
		case !isStr && f != nil:
			return false
		case k == "encryptionAlgorithm":
			if s != "" && s != csekAlgorithm {
				return false
			}
		case k == "keySha256":
			if s != "" && s != base64.StdEncoding.EncodeToString(o.KeySHA256) {
				return false
			}
			if s != "" && o.KeySHA256 == nil {
				return false
			}
		default:
			return false
		}
	}
	return o.KeySHA256 != nil
}
