package compute

// A disk with a customer-managed (Cloud KMS) or customer-supplied key is
// encrypted at all times (#2206): its storage holds only CryptFile, a
// block device image encrypted with dm-crypt (aes-xts-plain64, AES-256 in
// XTS mode, Linux's own cipher), whether or not a VM has it open. While a
// running VM uses the disk, the kernel of the cluster's node decrypts its
// blocks as they are read and encrypts them as they are written: the bytes
// on the storage stay ciphertext, as a Compute Engine disk's do on Google's
// hosts. The disk's 256-bit data key is kept only wrapped: by the Cloud
// KMS key version (Encrypt) for a CMEK disk, by the customer's own key for
// a CSEK disk, so with the key version disabled or destroyed, or without
// the customer's key, the disk can't be opened.
//
// CryptFile's first CryptHeaderSize bytes are its header: one line,
// "CBCRYPT1 aes-xts-plain64 CHECK", where CHECK is the hex HMAC-SHA256 of
// "cloudburrow disk key check" under the data key, so a wrong key is
// refused before the device is opened; then zeros. The encrypted device
// follows, its key the 512-bit XTS key HKDF-SHA256 derives from the data
// key (info "cloudburrow disk aes-xts-plain64"). Only Go's standard library
// is used here; the node's dmsetup and losetup open the device.

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// CryptFile is the name, in the root of a disk's storage, of its
	// encrypted image.
	CryptFile = ".cloudburrow-crypt"
	// CryptHeaderSize is the size of CryptFile's header, before the
	// encrypted device (8 sectors).
	CryptHeaderSize = 4096
	// CryptCipher is dm-crypt's cipher.
	CryptCipher = "aes-xts-plain64"

	cryptMagic = "CBCRYPT1"
)

// ErrSealed means an encrypted disk could not be opened with the key
// given: the wrong key, or a damaged header.
var ErrSealed = errors.New("the disk's encrypted contents can't be opened with its data key")

func checkDataKey(key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("a disk data key is 32 bytes, not %d", len(key))
	}
	return nil
}

// CryptKey is the hex dm-crypt key (512 bits, for AES-256-XTS) of a disk's
// data key.
func CryptKey(dataKey []byte) (string, error) {
	if err := checkDataKey(dataKey); err != nil {
		return "", err
	}
	k, err := hkdf.Key(sha256.New, dataKey, nil, "cloudburrow disk "+CryptCipher, 64)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

func cryptCheck(dataKey []byte) string {
	m := hmac.New(sha256.New, dataKey)
	m.Write([]byte("cloudburrow disk key check"))
	return hex.EncodeToString(m.Sum(nil))
}

// CryptHeader is the header line of a disk encrypted with dataKey.
func CryptHeader(dataKey []byte) (string, error) {
	if err := checkDataKey(dataKey); err != nil {
		return "", err
	}
	return cryptMagic + " " + CryptCipher + " " + cryptCheck(dataKey), nil
}

// CheckCryptHeader returns ErrSealed unless line is the header of a disk
// encrypted with dataKey.
func CheckCryptHeader(line string, dataKey []byte) error {
	if err := checkDataKey(dataKey); err != nil {
		return err
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != cryptMagic || f[1] != CryptCipher ||
		!hmac.Equal([]byte(f[2]), []byte(cryptCheck(dataKey))) {
		return ErrSealed
	}
	return nil
}
