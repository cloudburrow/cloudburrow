// Package install tests scripts/install.sh and scripts/render-formula.sh by
// running them, against a local server that serves a fake release (#279).
//
// The installer's one job that matters is refusing a download it cannot
// verify, so that is tested with real corrupted bytes rather than argued.
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const tag = "v9.9.9"

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// archive builds a release tarball holding name/cloudburrow, a stand-in that
// prints the version the way the real binary does.
func archive(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := []byte("#!/bin/sh\necho 'cloudburrow " + tag + " (commit test)'\n")
	for _, h := range []*tar.Header{
		{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: name + "/cloudburrow", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write(bin)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// release serves a fake GitHub release: /latest redirects to the tag, as
// GitHub does, and /download/<tag>/<file> serves the assets. corrupt, when
// set, alters the archive after its checksum is computed.
func release(t *testing.T, corrupt func([]byte) []byte, sums func(string) string) *httptest.Server {
	t.Helper()
	name := fmt.Sprintf("cloudburrow_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	tarball := archive(t, name)
	sum := fmt.Sprintf("%x", sha256.Sum256(tarball))
	checksums := fmt.Sprintf("%s  %s.tar.gz\n%s  cloudburrow_%s_other_os.tar.gz\n", sum, name, strings.Repeat("0", 64), tag)
	if sums != nil {
		checksums = sums(checksums)
	}
	if corrupt != nil {
		tarball = corrupt(tarball)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/tag/"+tag, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("release page")) })
	mux.HandleFunc("/download/"+tag+"/"+name+".tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tarball) })
	mux.HandleFunc("/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(checksums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func install(t *testing.T, srv *httptest.Server, prefix string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{scriptPath(t, "install.sh"), "--prefix", prefix}, args...)...)
	cmd.Env = append(os.Environ(), "CLOUDBURROW_RELEASE_BASE="+srv.URL)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestTheInstallerInstallsAVerifiedRelease(t *testing.T) {
	srv := release(t, nil, nil)
	prefix := t.TempDir()
	out, err := install(t, srv, prefix)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "checksum verified") {
		t.Errorf("the installer did not report verifying the checksum:\n%s", out)
	}
	got, err := exec.Command(filepath.Join(prefix, "bin", "cloudburrow"), "version").Output()
	if err != nil || !strings.Contains(string(got), "cloudburrow "+tag) {
		t.Errorf("the installed binary printed %q (%v)", got, err)
	}
	// The latest release was found through the redirect; naming it works too.
	if out, err := install(t, srv, t.TempDir(), "--version", tag); err != nil {
		t.Errorf("install --version %s failed: %v\n%s", tag, err, out)
	}
}

// TestTheInstallerRefusesACorruptedDownload flips one byte of the archive
// after its checksum was published: the installer must refuse, say why, and
// leave nothing installed.
func TestTheInstallerRefusesACorruptedDownload(t *testing.T) {
	srv := release(t, func(b []byte) []byte {
		c := append([]byte(nil), b...)
		c[len(c)/2] ^= 0xff
		return c
	}, nil)
	prefix := t.TempDir()
	out, err := install(t, srv, prefix)
	if err == nil {
		t.Fatalf("a corrupted download was installed:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") || !strings.Contains(out, "refusing to install") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "cloudburrow")); !os.IsNotExist(err) {
		t.Error("a binary was installed despite the mismatch")
	}
}

// An archive with no checksum entry is unverifiable, and is refused the same
// way: a missing line must not read as "nothing to compare, so fine".
func TestTheInstallerRefusesAnArchiveWithNoChecksum(t *testing.T) {
	srv := release(t, nil, func(string) string {
		return strings.Repeat("0", 64) + "  cloudburrow_" + tag + "_other_os.tar.gz\n"
	})
	prefix := t.TempDir()
	out, err := install(t, srv, prefix)
	if err == nil || !strings.Contains(out, "no entry") {
		t.Fatalf("an archive missing from checksums.txt was not refused (%v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "cloudburrow")); !os.IsNotExist(err) {
		t.Error("a binary was installed without a checksum")
	}
}

func TestTheFormulaPinsEveryPlatformFromChecksums(t *testing.T) {
	dir := t.TempDir()
	var sums strings.Builder
	want := map[string]string{}
	for i, p := range []string{"darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64"} {
		s := strings.Repeat(fmt.Sprint(i+1), 64)
		want[p] = s
		fmt.Fprintf(&sums, "%s  cloudburrow_%s_%s.tar.gz\n", s, tag, p)
	}
	path := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(path, []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", scriptPath(t, "render-formula.sh"), tag, path).Output()
	if err != nil {
		t.Fatalf("render-formula.sh: %v", err)
	}
	f := string(out)
	for p, s := range want {
		u := fmt.Sprintf("https://github.com/cloudburrow/cloudburrow/releases/download/%s/cloudburrow_%s_%s.tar.gz", tag, tag, p)
		// Each URL is followed directly by its own checksum, not another's.
		if !strings.Contains(f, fmt.Sprintf("url %q\n      sha256 %q", u, s)) {
			t.Errorf("the formula does not pin %s to its checksum:\n%s", p, f)
		}
	}
	if !strings.Contains(f, `version "9.9.9"`) || !strings.Contains(f, `"cloudburrow `+tag+`"`) {
		t.Errorf("version or test block wrong:\n%s", f)
	}

	// A release missing a platform must not render a formula that points at
	// a download that does not exist.
	if err := os.WriteFile(path, []byte(strings.SplitN(sums.String(), "\n", 2)[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", scriptPath(t, "render-formula.sh"), tag, path).Run(); err == nil {
		t.Error("a formula was rendered with a platform's checksum missing")
	}
}
