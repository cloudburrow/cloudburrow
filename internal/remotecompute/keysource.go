package remotecompute

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// KeySource supplies CloudBurrow's private key for a remote (#2209). Two
// kinds exist: FileKey, the key kept under the instance's state directory
// (mode 0600) for `--compute ssh://`, which CloudBurrow installs on the
// host itself; and StoredKey, a key held in a secret store such as the OS
// keychain, for a VM CloudBurrow provisioned with the key's public half
// already in place.
type KeySource interface {
	// PrivateKey is the key in OpenSSH PEM form. A stored key is read for
	// each connection and never written to disk: ssh reaches it through an
	// agent this process serves on a socket only its owner can open.
	PrivateKey() ([]byte, error)
}

// FileKey is the key EnsureKey keeps for a target under an instance's
// state directory.
type FileKey struct {
	StateDir string
	Target   Target
	Comment  string
}

// PrivateKey reads the key file, generating it on first use.
func (f FileKey) PrivateKey() ([]byte, error) {
	k, err := EnsureKey(f.StateDir, f.Target, f.Comment)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(k.Path)
}

// SecretStore is a store of named secrets: the provisioner's keychain
// (internal/byoc/keychain's Store) satisfies it.
type SecretStore interface {
	Get(name string) ([]byte, error)
}

// StoredKey is a key held as the item Name of Store.
type StoredKey struct {
	Store SecretStore
	Name  string
}

// PrivateKey reads the item.
func (s StoredKey) PrivateKey() ([]byte, error) {
	if s.Store == nil {
		return nil, errors.New("no key store")
	}
	return s.Store.Get(s.Name)
}

// PEMKey is a key already in hand.
type PEMKey []byte

// PrivateKey returns a copy of the key.
func (p PEMKey) PrivateKey() ([]byte, error) {
	if len(p) == 0 {
		return nil, errors.New("no private key")
	}
	return append([]byte(nil), p...), nil
}

// keyAgent is an SSH agent holding one key, served on a Unix socket in a
// directory only this user can open, for the system ssh to sign with.
type keyAgent struct {
	sock, pubFile, public string
	ln                    net.Listener
	wg                    sync.WaitGroup
}

func startAgent(dir string, pemKey []byte) (*keyAgent, error) {
	raw, err := ssh.ParseRawPrivateKey(pemKey)
	if err != nil {
		return nil, fmt.Errorf("CloudBurrow's SSH key could not be read: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, fmt.Errorf("CloudBurrow's SSH key could not be read: %w", err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: raw, Comment: "cloudburrow"}); err != nil {
		return nil, err
	}
	a := &keyAgent{sock: filepath.Join(dir, "agent.sock"), pubFile: filepath.Join(dir, "id.pub"),
		public: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
	// ssh signs with the agent's key that matches IdentityFile's public
	// half, and offers no other.
	if err := os.WriteFile(a.pubFile, []byte(a.public+"\n"), 0o600); err != nil {
		return nil, err
	}
	_ = os.Remove(a.sock)
	if a.ln, err = net.Listen("unix", a.sock); err != nil {
		return nil, err
	}
	if err := os.Chmod(a.sock, 0o600); err != nil {
		_ = a.ln.Close()
		return nil, err
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			c, err := a.ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = agent.ServeAgent(ring, c)
			}()
		}
	}()
	return a, nil
}

func (a *keyAgent) Close() error {
	if a == nil {
		return nil
	}
	err := a.ln.Close()
	a.wg.Wait()
	_ = os.Remove(a.sock)
	return err
}

// pinnedHosts writes a known_hosts naming only the host key the
// provisioner made for the VM, under alias, and returns its path.
func pinnedHosts(dir, alias, hostKey string) (string, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return "", fmt.Errorf("the host's key %q is not an SSH public key: %w", hostKey, err)
	}
	path := filepath.Join(dir, "known_hosts")
	line := alias + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + "\n"
	return path, os.WriteFile(path, []byte(line), 0o600)
}
