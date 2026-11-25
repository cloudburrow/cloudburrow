// Package keychain keeps bring-your-own-cloud credentials (#2209) in the OS
// keychain: the macOS Keychain, through /usr/bin/security with the secret
// on its standard input rather than its command line, or the Secret
// Service (GNOME Keyring, KWallet) over D-Bus on Linux, through
// github.com/zalando/go-keyring, which is pure Go, so the Linux CLI stays
// static. Nothing here writes a secret to disk or to a log, but Dir, the
// store of the local fakes' own test credentials.
package keychain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service every item is filed under.
const Service = "CloudBurrow"

// ErrNotFound is an item the keychain does not hold.
var ErrNotFound = errors.New("not in the keychain")

// Store holds named secrets.
type Store interface {
	Get(name string) ([]byte, error)
	Set(name string, secret []byte) error
	Delete(name string) error
}

// OS is the operating system's keychain. Each item's account is
// "<namespace>/<name>", so two state directories never share an item.
type OS struct{ Namespace string }

func (o OS) account(name string) string {
	if o.Namespace == "" {
		return name
	}
	return o.Namespace + "/" + name
}

// Get reads an item.
func (o OS) Get(name string) ([]byte, error) {
	v, err := keyring.Get(Service, o.account(name))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s from the keychain: %w", name, unavailable(err))
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("reading %s from the keychain: the item is not CloudBurrow's", name)
	}
	return b, nil
}

// Set writes an item, replacing one of the same name. The secret is base64,
// so the keychain stores it as printable text.
func (o OS) Set(name string, secret []byte) error {
	if err := keyring.Set(Service, o.account(name), base64.StdEncoding.EncodeToString(secret)); err != nil {
		return fmt.Errorf("writing %s to the keychain: %w", name, unavailable(err))
	}
	return nil
}

// Delete removes an item; one that is not there is not an error.
func (o OS) Delete(name string) error {
	err := keyring.Delete(Service, o.account(name))
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("removing %s from the keychain: %w", name, unavailable(err))
}

// unavailable says what to do when there is no keychain to use, without
// relaying anything the keychain tool printed beyond its error.
func unavailable(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "org.freedesktop.secrets") || strings.Contains(msg, "dbus") || strings.Contains(msg, "DBUS") {
		return fmt.Errorf("no Secret Service is running (start GNOME Keyring or KWallet in this session): %w", err)
	}
	return err
}

// Memory is an in-process Store, for tests.
type Memory struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemory is an empty Memory.
func NewMemory() *Memory { return &Memory{items: map[string][]byte{}} }

// Get reads an item.
func (m *Memory) Get(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return append([]byte(nil), v...), nil
}

// Set writes an item.
func (m *Memory) Set(name string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[name] = append([]byte(nil), secret...)
	return nil
}

// Delete removes an item.
func (m *Memory) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, name)
	return nil
}

// Names lists the items held, sorted.
func (m *Memory) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.items))
	for k := range m.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Dir is a Store of owner-only files in a directory, shared between
// processes: only for the local fakes of AWS and Azure that the tests and
// the first-run walk-through run against (CLOUDBURROW_BYOC_FAKES), whose
// credentials are the fakes' own. Real credentials always go to OS.
type Dir struct{ Path string }

func (d Dir) file(name string) string {
	return filepath.Join(d.Path, base64.RawURLEncoding.EncodeToString([]byte(name)))
}

// Get reads an item.
func (d Dir) Get(name string) ([]byte, error) {
	b, err := os.ReadFile(d.file(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return b, err
}

// Set writes an item, owner-only.
func (d Dir) Set(name string, secret []byte) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	tmp := d.file(name) + ".tmp"
	if err := os.WriteFile(tmp, secret, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.file(name))
}

// Delete removes an item.
func (d Dir) Delete(name string) error {
	err := os.Remove(d.file(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
