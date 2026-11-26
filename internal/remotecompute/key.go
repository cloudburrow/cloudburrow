package remotecompute

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Key is CloudBurrow's own SSH key for one remote: generated here, kept
// under the instance's state directory with mode 0600, and never a key the
// user manages. Only its public half ever leaves the machine, into the
// remote account's authorized_keys.
type Key struct {
	// Path is the private key file (OpenSSH format, ed25519).
	Path string
	// Public is the authorized_keys line, with a comment naming the
	// instance.
	Public string
}

// keyFile is the private key's name in a remote's directory.
const keyFile = "id_ed25519"

// remoteDir is a target's directory under the instance's state.
func remoteDir(stateDir string, t Target) string {
	return filepath.Join(stateDir, "remote", t.keyName())
}

// EnsureKey returns the key for t under stateDir, generating it on first
// use. A key file that is not owner-only is refused, as ssh itself refuses
// it, rather than silently re-permissioned.
func EnsureKey(stateDir string, t Target, comment string) (Key, error) {
	dir := remoteDir(stateDir, t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Key{}, err
	}
	path := filepath.Join(dir, keyFile)
	if b, err := os.ReadFile(path); err == nil {
		info, err := os.Stat(path)
		if err != nil {
			return Key{}, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return Key{}, fmt.Errorf("CloudBurrow's SSH key %s is readable by others (mode %v); ssh refuses such a key: "+
				"run chmod 600 on it, or delete it to have a new one made", path, info.Mode().Perm())
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return Key{}, fmt.Errorf("read CloudBurrow's SSH key %s: %w", path, err)
		}
		return Key{Path: path, Public: authorizedLine(signer.PublicKey(), comment)}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Key{}, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return Key{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
		return Key{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Key{}, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return Key{}, err
	}
	line := authorizedLine(sp, comment)
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o600); err != nil {
		return Key{}, err
	}
	return Key{Path: path, Public: line}, nil
}

func authorizedLine(k ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if comment != "" {
		line += " " + comment
	}
	return line
}
