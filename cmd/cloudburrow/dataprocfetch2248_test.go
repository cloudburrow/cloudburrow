package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchPinnedFromFallsBack (#2248): a mirror that fails is noted and
// the next one tried; the file is the pinned one whichever served it, and
// a mirror serving other bytes is refused.
func TestFetchPinnedFromFallsBack(t *testing.T) {
	body := []byte("pig release")
	h := sha256.Sum256(body)
	sum := hex.EncodeToString(h[:])
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", http.StatusServiceUnavailable) }))
	defer down.Close()
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("other")) }))
	defer wrong.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer good.Close()

	p := filepath.Join(t.TempDir(), "pig.tar.gz")
	var log strings.Builder
	got, err := fetchPinnedFrom(context.Background(), p, []string{down.URL + "/p", wrong.URL + "/p", good.URL + "/p"}, sum, "Pig", &log)
	if err != nil {
		t.Fatalf("fetchPinnedFrom: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(got); string(b) != string(body) {
		t.Errorf("fetched %q; want %q", b, body)
	}
	if !strings.Contains(log.String(), down.URL) || !strings.Contains(log.String(), wrong.URL) {
		t.Errorf("the failed mirrors are not noted:\n%s", log.String())
	}
	if _, err := fetchPinnedFrom(context.Background(), filepath.Join(t.TempDir(), "x"), []string{down.URL, wrong.URL}, sum, "Pig", io.Discard); err == nil {
		t.Error("no mirror served the pinned file, but fetchPinnedFrom succeeded")
	}
}
