package kms

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"testing"
)

func material(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnvelopeRoundTrips(t *testing.T) {
	key := material(t)
	for name, aad := range map[string][]byte{"empty": nil, "one byte": {7}, "64KiB": bytes.Repeat([]byte{'a'}, 64*1024)} {
		ct, err := seal(key, 3, []byte("hello"), aad)
		if err != nil {
			t.Fatal(err)
		}
		pt, err := open(key, 3, ct, aad)
		if err != nil || string(pt) != "hello" {
			t.Errorf("%s AAD: open = %q, %v", name, pt, err)
		}
		if v, err := parseVersion(ct); err != nil || v != 3 {
			t.Errorf("%s AAD: parseVersion = %d, %v; want 3", name, v, err)
		}
	}
}

func TestEnvelopeUsesAFreshNonce(t *testing.T) {
	key := material(t)
	a, _ := seal(key, 1, []byte("same"), nil)
	b, _ := seal(key, 1, []byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext are identical")
	}
}

func TestEnvelopeRefusesWhatItDidNotSeal(t *testing.T) {
	key, other := material(t), material(t)
	ct, err := seal(key, 2, []byte("pt"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(key, 2, ct, []byte("other aad")); err != errOpen {
		t.Errorf("another AAD: %v", err)
	}
	if _, err := open(other, 2, ct, []byte("aad")); err != errOpen {
		t.Errorf("another version's material: %v", err)
	}
	for i := range ct {
		for bit := 0; bit < 8; bit++ {
			flipped := bytes.Clone(ct)
			flipped[i] ^= 1 << bit
			if _, err := open(key, 2, flipped, []byte("aad")); err == nil {
				t.Fatalf("a flip of bit %d at byte %d still opens", bit, i)
			}
		}
	}
	rewritten := bytes.Clone(ct)
	binary.BigEndian.PutUint32(rewritten[1:5], 9)
	if _, err := open(key, 9, rewritten, []byte("aad")); err != errOpen {
		t.Errorf("a rewritten version number: %v", err)
	}
}

func TestEnvelopeRefusesMalformedInputWithoutPanicking(t *testing.T) {
	key := material(t)
	for name, ct := range map[string][]byte{
		"empty":          nil,
		"too short":      make([]byte, headerLen+nonceLen+tagLen-1),
		"unknown format": append([]byte{0x02, 0, 0, 0, 1}, make([]byte, nonceLen+tagLen)...),
		"version zero":   append([]byte{envelopeFormat, 0, 0, 0, 0}, make([]byte, nonceLen+tagLen)...),
	} {
		if _, err := open(key, 1, ct, nil); err != errOpen {
			t.Errorf("%s: open = %v", name, err)
		}
		if _, err := parseVersion(ct); err != errOpen {
			t.Errorf("%s: parseVersion = %v", name, err)
		}
	}
}

// No failure's message carries the plaintext, the AAD, the ciphertext or the
// key.
func TestEnvelopeErrorsCarryNoInput(t *testing.T) {
	key := []byte("KEYMARKER-KEYMARKER-KEYMARKER-32")
	ct, _ := seal(key, 1, []byte("PLAINMARKER"), []byte("AADMARKER"))
	_, err := open(key, 1, ct, []byte("WRONGAADMARKER"))
	for _, marker := range []string{"KEYMARKER", "PLAINMARKER", "AADMARKER", string(ct)} {
		if err == nil || strings.Contains(err.Error(), marker) {
			t.Errorf("the error %v carries %q", err, marker)
		}
	}
}
