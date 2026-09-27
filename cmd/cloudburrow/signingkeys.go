package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// storageSigningKeys is what the storage server verifies RSA signed URLs
// against (#577): the ADC fixture's own public key, so a URL an official
// client signs with the credentials `cloudburrow env` hands out verifies,
// and every storage.signingCerts entry.
//
// The server refuses an RSA signed URL for any other account. Before this,
// `up` registered nothing and storage.signingCerts was read by nobody, so
// every such URL was 403 on a `cloudburrow up` instance while the docs said
// they verified.
//
// Each file is read and parsed here, before anything is created, so a bad
// entry fails `up` with its name rather than a pod that will not start.
func storageSigningKeys(cfg config.Config, creds *metadata.Credentials) (map[string][]byte, error) {
	keys := map[string][]byte{}
	der, err := x509.MarshalPKIXPublicKey(&creds.PrivateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("the ADC fixture's public key: %w", err)
	}
	keys[creds.Email] = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	for email, path := range cfg.Storage.SigningCerts {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("storage.signingCerts %s: %w", email, err)
		}
		if _, err := storage.ParseSigningKey(raw); err != nil {
			return nil, fmt.Errorf("storage.signingCerts %s=%s: %w", email, path, err)
		}
		keys[email] = raw
	}
	return keys, nil
}
