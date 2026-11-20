package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A whole-file replacement is written only when its hash is the pinned one
// and the file it replaces exists (#2008).
func TestReplaceFile(t *testing.T) {
	dir, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, "new.dat"), "\x00new\xff")
	write(t, filepath.Join(dst, "a.dat"), "old")
	sum := sha256.Sum256([]byte("\x00new\xff"))
	good := File{Path: "a.dat", From: "new.dat", SHA256: hex.EncodeToString(sum[:])}
	if err := replaceFile(dir, dst, good); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.dat")); string(b) != "\x00new\xff" {
		t.Errorf("replaced file = %q", b)
	}
	bad := good
	bad.SHA256 = strings.Repeat("0", 64)
	if err := replaceFile(dir, dst, bad); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("a wrong hash: %v", err)
	}
	missing := good
	missing.Path = "b.dat"
	if err := replaceFile(dir, dst, missing); err == nil {
		t.Error("a replacement of a missing file was written")
	}
}

// Every pinned replacement in sources.json hashes to its pin.
func TestRepositoryFiles(t *testing.T) {
	src, err := loadSources("../../third_party/bigquery-emulator")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range src.Patched {
		for _, f := range p.Files {
			b, err := os.ReadFile(filepath.Join("../../third_party/bigquery-emulator", f.From))
			if err != nil {
				t.Fatal(err)
			}
			if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.SHA256 {
				t.Errorf("%s hashes to %x, sources.json pins %s", f.From, sum, f.SHA256)
			}
		}
	}
}
