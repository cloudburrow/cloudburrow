package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/storageserver"
)

// An RSA signed URL made by the official client with the credentials
// `cloudburrow env` hands out verifies against the storage server `up`
// starts (#577): the fixture's public key goes into the Deployment's own
// arguments, and a server run with exactly those arguments accepts the URL
// and still refuses an account nobody registered.
func TestUpRegistersTheFixtureKeyForSignedURLs(t *testing.T) {
	dir := t.TempDir()
	creds, err := metadata.LoadOrCreate(dir, "proj", "http://127.0.0.1:1/token")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := storageSigningKeys(config.Config{}, creds)
	if err != nil {
		t.Fatal(err)
	}
	b := components.BuiltinStorageBackend("cloudburrow", "img", false, false, keys)
	var signing []string
	for i, a := range b.Args {
		if a == "--signing-key" && i+1 < len(b.Args) {
			signing = append(signing, a, b.Args[i+1])
		}
	}
	if len(signing) != 2 || !strings.HasPrefix(signing[1], creds.Email+"=") {
		t.Fatalf("the Deployment's signing arguments are %q; want one --signing-key for %s", signing, creds.Email)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- storageserver.Run(ctx, append([]string{"--listen", addr}, signing...), io.Discard, io.Discard)
	}()
	t.Cleanup(func() { cancel(); <-done })
	base := "http://" + addr
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/storage/v1/b?project=proj"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 200 {
			t.Fatal("the storage server did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	post := func(url, ctype, body string) {
		t.Helper()
		resp, err := http.Post(url, ctype, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d", url, resp.StatusCode)
		}
	}
	post(base+"/storage/v1/b?project=proj", "application/json", `{"name":"signed"}`)
	post(base+"/upload/storage/v1/b/signed/o?uploadType=media&name=secret.txt", "text/plain", "classified")

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(creds.PrivateKey)})
	get := func(email string, key []byte, scheme gcs.SigningScheme) (int, string) {
		t.Helper()
		u, err := gcs.SignedURL("signed", "secret.txt", &gcs.SignedURLOptions{GoogleAccessID: email, PrivateKey: key,
			Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: scheme, Hostname: addr, Insecure: true})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(strings.Replace(u, "https://", "http://", 1))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for _, sc := range []gcs.SigningScheme{gcs.SigningSchemeV4, gcs.SigningSchemeV2} {
		if code, body := get(creds.Email, keyPEM, sc); code != http.StatusOK || body != "classified" {
			t.Errorf("scheme %d, the fixture's own key: %d %s; want 200", sc, code, body)
		}
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(other)})
	if code, _ := get("stranger@proj.iam.gserviceaccount.com", otherPEM, gcs.SigningSchemeV4); code != http.StatusForbidden {
		t.Errorf("an unregistered account = %d; want 403", code)
	}
	if code, _ := get(creds.Email, otherPEM, gcs.SigningSchemeV4); code != http.StatusForbidden {
		t.Errorf("the fixture's account with the wrong key = %d; want 403", code)
	}
}

// storage.signingCerts was declared and read by nothing (#577). Each entry is
// registered alongside the fixture, and one that is not a key fails `up`
// with its account and path before anything is created.
func TestSigningCertsFromTheConfigAreRegistered(t *testing.T) {
	dir := t.TempDir()
	creds, err := metadata.LoadOrCreate(dir, "proj", "http://127.0.0.1:1/token")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	good := filepath.Join(dir, "signer.pem")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Storage.SigningCerts = map[string]string{"signer@proj.iam.gserviceaccount.com": good}
	keys, err := storageSigningKeys(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["signer@proj.iam.gserviceaccount.com"] == nil || keys[creds.Email] == nil {
		t.Errorf("registered %d keys; want the fixture's and signer's", len(keys))
	}
	b := components.BuiltinStorageBackend("cloudburrow", "img", false, false, keys)
	if got := strings.Count(strings.Join(b.Args, " "), "--signing-key"); got != 2 {
		t.Errorf("%d --signing-key arguments; want 2: %q", got, b.Args)
	}

	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("not a key"), 0o600)
	cfg.Storage.SigningCerts = map[string]string{"signer@proj.iam.gserviceaccount.com": bad}
	if _, err := storageSigningKeys(cfg, creds); err == nil || !strings.Contains(err.Error(), "signer@proj") || !strings.Contains(err.Error(), bad) {
		t.Errorf("a file that is not a key = %v; want an error naming the account and path", err)
	}
	cfg.Storage.SigningCerts = map[string]string{"signer@proj.iam.gserviceaccount.com": filepath.Join(dir, "missing.pem")}
	if _, err := storageSigningKeys(cfg, creds); err == nil || !strings.Contains(err.Error(), "signingCerts") {
		t.Errorf("a missing file = %v; want an error naming storage.signingCerts", err)
	}
}
