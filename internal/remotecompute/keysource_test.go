package remotecompute

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type memStore map[string][]byte

func (m memStore) Get(name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return nil, errors.New(name + ": not in the keychain")
}

func newPEM(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	spub, _ := ssh.NewPublicKey(pub)
	return pem.EncodeToMemory(block), spub
}

// A stored key (#2209) is read from its store and served to ssh by an
// agent on an owner-only socket, holding that key alone; nothing of the
// private key is written to disk.
func TestStoredKeyIsServedByAnAgent(t *testing.T) {
	key, pub := newPEM(t)
	src := StoredKey{Store: memStore{"vm-key": key}, Name: "vm-key"}
	got, err := src.PrivateKey()
	if err != nil || string(got) != string(key) {
		t.Fatalf("PrivateKey = %v", err)
	}
	if _, err := (StoredKey{Store: memStore{}, Name: "gone"}).PrivateKey(); err == nil {
		t.Fatal("a missing item was not refused")
	}
	dir, err := shortDir(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/id.pub"); _ = os.Remove(dir) })
	a, err := startAgent(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(a.sock); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("agent socket %v, %v: want owner-only", fi.Mode(), err)
	}
	pubFile, _ := os.ReadFile(a.pubFile)
	if !strings.HasPrefix(string(pubFile), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))) {
		t.Fatalf("id.pub = %q", pubFile)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(dir + "/" + e.Name())
		if strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("%s holds the private key", e.Name())
		}
	}
	c, err := net.Dial("unix", a.sock)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := agent.NewClient(c).List()
	_ = c.Close()
	if err != nil || len(keys) != 1 || string(keys[0].Blob) != string(pub.Marshal()) {
		t.Fatalf("the agent holds %v, %v", keys, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("unix", a.sock); err == nil {
		t.Fatal("the agent still answers after Close")
	}
}

// A pinned host key is the known_hosts' only entry, under the alias.
func TestPinnedHostKey(t *testing.T) {
	_, pub := newPEM(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " host-comment"
	path, err := pinnedHosts(t.TempDir(), "cloudburrow-x", line)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if want := "cloudburrow-x " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + "\n"; string(b) != want {
		t.Fatalf("known_hosts = %q, want %q", b, want)
	}
	if _, err := pinnedHosts(t.TempDir(), "a", "not a key"); err == nil {
		t.Fatal("a malformed host key was accepted")
	}
}
