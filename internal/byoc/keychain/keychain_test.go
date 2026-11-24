package keychain

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// The OS store's round trip runs on go-keyring's mock provider: a test
// never reads or writes the real keychain.
func TestOSStoreRoundTripOnTheMockKeyring(t *testing.T) {
	keyring.MockInit()
	s := OS{Namespace: "test"}
	if _, err := s.Get("connection/aws-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a missing item = %v", err)
	}
	secret := []byte("{\"k\":\"v with spaces, quotes \\\" and \\n newlines\"}\x00\xff")
	if err := s.Set("connection/aws-1", secret); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("connection/aws-1")
	if err != nil || string(got) != string(secret) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	// The item is filed under the namespace, base64.
	raw, err := keyring.Get(Service, "test/connection/aws-1")
	if err != nil || raw == string(secret) {
		t.Fatalf("raw item %q, %v", raw, err)
	}
	if err := s.Delete("connection/aws-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("connection/aws-1"); err != nil {
		t.Fatalf("a second Delete = %v", err)
	}
	if _, err := s.Get("connection/aws-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
}

func TestOSStoreReportsAKeychainError(t *testing.T) {
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)
	err := OS{}.Set("x", []byte("y"))
	if err == nil || !strings.Contains(err.Error(), "no Secret Service") {
		t.Fatalf("Set = %v", err)
	}
}

func TestMemory(t *testing.T) {
	m := NewMemory()
	_ = m.Set("b", []byte("2"))
	_ = m.Set("a", []byte("1"))
	if n := m.Names(); len(n) != 2 || n[0] != "a" {
		t.Fatalf("Names = %v", n)
	}
	_ = m.Delete("a")
	if _, err := m.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
