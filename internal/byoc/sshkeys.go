package byoc

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KeyPair is an ed25519 key in OpenSSH's forms.
type KeyPair struct {
	PrivatePEM string // OPENSSH PRIVATE KEY
	Public     string // authorized_keys line
}

// NewKeyPair makes an ed25519 key on this machine. comment names it in the
// authorized_keys line.
func NewKeyPair(comment string) (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generating an SSH key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, fmt.Errorf("encoding an SSH key: %w", err)
	}
	spub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, fmt.Errorf("encoding an SSH key: %w", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spub)))
	if comment != "" {
		line += " " + comment
	}
	return KeyPair{PrivatePEM: string(pem.EncodeToMemory(block)), Public: line}, nil
}
