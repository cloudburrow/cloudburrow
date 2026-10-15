package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tgz(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	return &buf
}

func TestUntarGzUnpacksSource(t *testing.T) {
	dir := t.TempDir()
	if err := untarGz(tgz(t, map[string]string{"Dockerfile": "FROM scratch\n", "app/main.go": "package main\n"}), dir); err != nil {
		t.Fatalf("untarGz: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "app", "main.go"))
	if err != nil || string(b) != "package main\n" {
		t.Errorf("app/main.go = %q, %v", b, err)
	}
}

func TestUntarGzRefusesEscape(t *testing.T) {
	err := untarGz(tgz(t, map[string]string{"../evil": "x"}), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("untarGz(../evil) = %v; want refused", err)
	}
	if err := untarGz(strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Error("untarGz accepted a non-gzip source")
	}
}
