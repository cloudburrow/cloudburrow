package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The admin token (#553): minted by `up` for the life of the instance,
// kept owner-only in the instance's state directory, required on every
// /admin route, and never written into the runtime file or a diagnose
// bundle. A workload in the cluster that reaches the control port through
// Docker Desktop's host.docker.internal gets 401 without it.

func adminTokenPath(cfg config.Config) string { return filepath.Join(cfg.InstanceDir(), "admin-token") }

// newAdminToken mints a token: 32 random bytes, hex.
func newAdminToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint the admin token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// readAdminToken is the running instance's token, for the CLI's own admin
// calls.
func readAdminToken(cfg config.Config) (string, error) {
	b, err := os.ReadFile(adminTokenPath(cfg))
	if err != nil {
		return "", fmt.Errorf("the admin token is not readable (%w); is `cloudburrow up` running?", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// adminRequest builds a request to the instance's admin API carrying its
// token.
func adminRequest(cfg config.Config, method, url string, body io.Reader) (*http.Request, error) {
	token, err := readAdminToken(cfg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}
