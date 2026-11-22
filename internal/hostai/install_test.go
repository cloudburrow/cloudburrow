package hostai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetal(t *testing.T) {
	defer func(o, a string) { goos, goarch = o, a }(goos, goarch)
	for _, c := range []struct {
		os, arch string
		ok       bool
		why      string
	}{
		{"darwin", "arm64", true, ""},
		{"darwin", "amd64", false, "Apple silicon"},
		{"linux", "arm64", false, "macOS only"},
	} {
		goos, goarch = c.os, c.arch
		ok, why := Metal()
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s/%s: Metal() = %v, %q", c.os, c.arch, ok, why)
		}
	}
}

type entry struct {
	name, body, link string
	typ              byte
}

func tarball(t *testing.T, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func serve(t *testing.T, b []byte) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) }))
	t.Cleanup(srv.Close)
	h := sha256.Sum256(b)
	return srv.URL + "/llama.tar.gz", hex.EncodeToString(h[:])
}

func TestLlamaServerInstallsTheVerifiedArchive(t *testing.T) {
	b := tarball(t, []entry{{name: "llama-b1/", typ: tar.TypeDir}, {name: "llama-b1/llama-server", body: "#!/bin/sh\n", typ: tar.TypeReg},
		{name: "llama-b1/libggml.0.dylib", body: "lib", typ: tar.TypeReg},
		{name: "llama-b1/libggml.dylib", link: "libggml.0.dylib", typ: tar.TypeSymlink}})
	u, sum := serve(t, b)
	in := &Installer{Dir: t.TempDir(), ArchiveURL: u, ArchiveSHA256: sum}
	bin, err := in.LlamaServer(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("llama-server %s: %v %v", bin, st, err)
	}
	if l, err := os.Readlink(filepath.Join(filepath.Dir(bin), "libggml.dylib")); err != nil || l != "libggml.0.dylib" {
		t.Errorf("dylib link = %q, %v", l, err)
	}
	// Installed once: a second call does not download again.
	in.ArchiveURL = u + "-gone"
	in.ArchiveSHA256 = sum
	if again, err := in.LlamaServer(context.Background(), nil); err != nil || again != bin {
		t.Errorf("second LlamaServer = %q, %v", again, err)
	}
}

func TestLlamaServerRefusesAWrongDigest(t *testing.T) {
	b := tarball(t, []entry{{name: "llama-b1/llama-server", body: "x", typ: tar.TypeReg}})
	u, _ := serve(t, b)
	dir := t.TempDir()
	in := &Installer{Dir: dir, ArchiveURL: u, ArchiveSHA256: strings.Repeat("0", 64)}
	if _, err := in.LlamaServer(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("LlamaServer = %v, want a digest refusal", err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Errorf("a refused archive left %v", es)
	}
}

func TestUntarRefusesEscapes(t *testing.T) {
	for name, es := range map[string][]entry{
		"dotdot":       {{name: "top/../../evil", body: "x", typ: tar.TypeReg}},
		"absolute":     {{name: "top/x", link: "/etc/passwd", typ: tar.TypeSymlink}},
		"link escapes": {{name: "top/x", link: "../../y", typ: tar.TypeSymlink}},
		"device":       {{name: "top/dev", typ: tar.TypeChar}},
	} {
		dir := t.TempDir()
		arc := filepath.Join(dir, "a.tgz")
		_ = os.WriteFile(arc, tarball(t, es), 0o644)
		if err := untar(arc, filepath.Join(dir, "out")); err == nil {
			t.Errorf("%s: untar accepted it", name)
		}
	}
}

func TestMLXPythonWithUv(t *testing.T) {
	dir := t.TempDir()
	var ran []string
	in := &Installer{Dir: dir,
		LookPath: func(n string) (string, error) {
			if n == "uv" {
				return "/usr/local/bin/uv", nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, filepath.Base(name)+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "-c" {
				return "True\n", nil
			}
			return "", nil
		}}
	py, err := in.MLXPython(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(py, dir) || !strings.HasSuffix(py, "/venv/bin/python") {
		t.Errorf("python = %s", py)
	}
	all := strings.Join(ran, "\n")
	for _, want := range []string{"uv venv --quiet --python 3.12 ", "uv pip install --quiet --python " + py + " --require-hashes --no-deps -r "} {
		if !strings.Contains(all, want) {
			t.Errorf("ran %q, want %q", all, want)
		}
	}
	lock, _ := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(py))), "requirements.txt"))
	if !strings.Contains(string(lock), "mlx-lm==0.32.2") || !strings.Contains(string(lock), "--hash=sha256:") {
		t.Errorf("the requirements written are not the hash lock")
	}
	ran = nil
	if again, err := in.MLXPython(context.Background(), nil); err != nil || again != py || len(ran) != 0 {
		t.Errorf("a made venv was made again: %v %v", ran, err)
	}
}

func TestMLXPythonNeedsAnOfficialPython(t *testing.T) {
	in := &Installer{Dir: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
	if _, err := in.MLXPython(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "needs uv, or Python 3.12") {
		t.Errorf("MLXPython = %v", err)
	}
	if !officialPython("/opt/homebrew/Cellar/python@3.12/3.12.16/Frameworks/Python.framework/Versions/3.12/bin/python3.12") ||
		!officialPython("/Library/Frameworks/Python.framework/Versions/3.12/bin/python3.12") || officialPython("/Users/x/miniconda3/bin/python3.12") {
		t.Errorf("officialPython misjudges a path")
	}
}

func TestMLXPythonRefusesNoMetal(t *testing.T) {
	in := &Installer{Dir: t.TempDir(), LookPath: func(n string) (string, error) { return "/bin/" + n, nil },
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "-c" {
				return "False\n", nil
			}
			return "", nil
		}}
	if _, err := in.MLXPython(context.Background(), nil); !errors.Is(err, ErrNoMetal) {
		t.Errorf("MLXPython = %v, want ErrNoMetal", err)
	}
}
