package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// prefixKMS wraps by prefixing the key's name.
type prefixKMS struct{}

func (prefixKMS) Encrypt(_ context.Context, key string, p []byte) ([]byte, error) {
	return append([]byte(key+":"), p...), nil
}
func (prefixKMS) Decrypt(_ context.Context, key string, c []byte) ([]byte, error) {
	return c[len(key)+1:], nil
}

// TestConsoleBackupDRVaultEncryptionKey (#1501): Create backup vault's
// customer-managed encryption key is sent as encryptionConfig.kmsKeyName,
// and the vault's page shows it.
func TestConsoleBackupDRVaultEncryptionKey(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	srv.UseKMS(prefixKMS{})
	p := backupDRProvider{bdr: srv}
	ctx := context.Background()
	const project = "bdr-cmek"
	key := "projects/bdr-cmek/locations/us-central1/keyRings/r/cryptoKeys/k"
	_, fields := p.CreateForm()
	found := false
	for _, f := range fields {
		found = found || f.Name == "kmsKeyName"
	}
	if !found {
		t.Fatal("Create backup vault has no encryption key field")
	}
	if _, err := p.Create(ctx, project, map[string]string{"id": "vault-k", "location": "us-central1", "retention": "86400s",
		"kmsKeyName": key}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, project, []string{"vault-k"})
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, pr := range d.Summary {
		if pr.Label == "Encryption key" {
			got = pr.Value
		}
	}
	if got != key {
		t.Errorf("the vault's Encryption key is %q, want %s", got, key)
	}
}
