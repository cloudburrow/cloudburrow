package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/osvscan"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// local sends every request, whatever its host, to h, which sees the
// original host in r.Host; nothing leaves the machine.
func local(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rt := srv.Client().Transport
	old := client
	client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return rt.RoundTrip(r)
	})}
	t.Cleanup(func() { client = old })
}

// fakePins replaces the pins with small files for the test's duration.
func fakePins(t *testing.T, bin []byte, snaps map[string][]byte) {
	t.Helper()
	oldB, oldS := osvscan.Binaries, osvscan.Snapshots
	t.Cleanup(func() { osvscan.Binaries, osvscan.Snapshots = oldB, oldS })
	osvscan.Binaries = map[string]osvscan.Binary{}
	for k, b := range oldB {
		osvscan.Binaries[k] = osvscan.Binary{Name: b.Name, SHA256: sum(append([]byte(b.Name), bin...)), Size: int64(len(b.Name) + len(bin))}
	}
	osvscan.Snapshots = nil
	for _, e := range []string{"Alpine", "Ubuntu"} {
		if b, ok := snaps[e]; ok {
			osvscan.Snapshots = append(osvscan.Snapshots, osvscan.Snapshot{Ecosystem: e, Generation: "7", SHA256: sum(b), Size: int64(len(b))})
		}
	}
}

func TestStageFallsBackToAnEarlierRelease(t *testing.T) {
	bin := []byte("-scanner")
	alpine, ubuntu := []byte("alpine db"), []byte("ubuntu db")
	fakePins(t, bin, map[string][]byte{"Alpine": alpine, "Ubuntu": ubuntu})
	var asked []string
	local(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Host+r.URL.Path)
		switch {
		case r.Host == "github.com" && strings.HasPrefix(r.URL.Path, "/google/osv-scanner/releases/download/"):
			name := filepath.Base(r.URL.Path)
			_, _ = w.Write(append([]byte(name), bin...))
		case r.Host == "mirror.test" && r.URL.Path == "/old/"+osvscan.MirrorSnapshotAsset(osvscan.Snapshots[0]):
			_, _ = w.Write(alpine)
		default: // OSV no longer serves the generation
			http.NotFound(w, r)
		}
	})
	out := t.TempDir()
	var log bytes.Buffer
	if err := runStage(context.Background(), []string{"-out", out, "-mirror", "https://mirror.test/old"}, &log); err != nil {
		t.Fatalf("stage: %v\n%s", err, log.String())
	}
	sums, err := os.ReadFile(filepath.Join(out, "osv-mirror-SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(sums), "\n"); n != len(osvscan.Binaries)+1 {
		t.Errorf("%d sums, want %d binaries and Alpine:\n%s", n, len(osvscan.Binaries), sums)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, _ := strings.Cut(line, "  ")
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || sum(b) != want {
			t.Errorf("%s: %v, sha256 %s, want %s", name, err, sum(b), want)
		}
	}
	if strings.Contains(string(sums), "Ubuntu") {
		t.Error("Ubuntu, which is not mirrored, was staged")
	}
	if n, err := os.ReadFile(filepath.Join(out, "osv-mirror-NOTICE.txt")); err != nil || !strings.Contains(string(n), "Apache License") {
		t.Errorf("NOTICE: %v %s", err, n)
	}
	// Upstream first, then the mirror.
	got := strings.Join(asked, " ")
	if !strings.Contains(got, "osv-vulnerabilities.storage.googleapis.com/Alpine/all.zip mirror.test/old/osv-db-Alpine-") {
		t.Errorf("sources asked, in order: %s", got)
	}

	// No source with the bytes: stage fails, naming each attempt.
	if err := runStage(context.Background(), []string{"-out", t.TempDir()}, &log); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("stage without a source: %v", err)
	}
}

func TestFetchPrefersTheMirror(t *testing.T) {
	bin := []byte("-scanner")
	alpine := []byte("alpine db")
	fakePins(t, bin, map[string][]byte{"Alpine": alpine})
	host, err := osvscan.HostBinary()
	if err != nil {
		t.Skip(err)
	}
	var upstream int
	local(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "mirror.test" && r.URL.Path == "/r/"+osvscan.MirrorBinaryAsset(host):
			_, _ = w.Write(append([]byte(host.Name), bin...))
		case r.Host == "mirror.test" && r.URL.Path == "/r/"+osvscan.MirrorSnapshotAsset(osvscan.Snapshots[0]):
			_, _ = w.Write(alpine)
		default:
			upstream++
			http.NotFound(w, r)
		}
	})
	dir := t.TempDir()
	var log bytes.Buffer
	if err := runFetch(context.Background(), []string{"-dir", dir, "-ecosystems", "Alpine", "-mirror", "https://mirror.test/r"}, &log); err != nil {
		t.Fatalf("fetch: %v\n%s", err, log.String())
	}
	if upstream != 0 {
		t.Errorf("%d upstream requests with the mirror serving everything", upstream)
	}
	if err := osvscan.Ready(osvscan.Dir(dir)); err != nil {
		t.Error(err)
	}
}

func TestBump(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"internal/osvscan/pins.go", "dependencies.json"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pinned, _ := time.Parse("2006-01-02", osvscan.SnapshotDate)
	alpine := []byte("the new Alpine archive")
	local(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "storage.googleapis.com" && strings.HasPrefix(r.URL.Path, "/storage/v1/b/osv-vulnerabilities/o/"):
			eco := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/storage/v1/b/osv-vulnerabilities/o/"), "/all.zip")
			for _, s := range osvscan.Snapshots {
				if s.Ecosystem == eco {
					g, size := s.Generation, s.Size
					if eco == "Alpine" {
						g, size = "1795999999999999", int64(len(alpine))
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"generation": g, "size": jsonInt(size)})
					return
				}
			}
			http.NotFound(w, r)
		case r.Host == "osv-vulnerabilities.storage.googleapis.com" && r.URL.Path == "/Alpine/all.zip" && r.URL.Query().Get("generation") == "1795999999999999":
			_, _ = w.Write(alpine)
		default:
			t.Errorf("unexpected request %s%s", r.Host, r.URL)
			http.NotFound(w, r)
		}
	})
	var log bytes.Buffer
	// Young pins are left alone.
	if err := runBump(context.Background(), []string{"-min-age", "3"}, root, pinned.Add(48*time.Hour), &log); err != nil || !strings.Contains(log.String(), "nothing to do") {
		t.Fatalf("young pins: %v %s", err, log.String())
	}
	if err := runBump(context.Background(), []string{"-min-age", "3"}, root, pinned.Add(4*24*time.Hour), &log); err != nil {
		t.Fatalf("bump: %v\n%s", err, log.String())
	}
	date := pinned.Add(4 * 24 * time.Hour).Format("2006-01-02")
	pins, _ := os.ReadFile(filepath.Join(root, "internal/osvscan/pins.go"))
	want := `{"Alpine", "1795999999999999", "` + sum(alpine) + `", 22}`
	if !strings.Contains(string(pins), want) || !strings.Contains(string(pins), `const SnapshotDate = "`+date+`"`) {
		t.Errorf("pins.go has no %s dated %s", want, date)
	}
	deps, _ := os.ReadFile(filepath.Join(root, "dependencies.json"))
	var inv struct {
		Components struct {
			Scanning struct {
				OSVDatabase struct {
					Version   string `json:"version"`
					Snapshots map[string]struct {
						URL    string `json:"url"`
						SHA256 string `json:"sha256"`
						Size   int64  `json:"size"`
					} `json:"snapshots"`
				} `json:"osvDatabase"`
			} `json:"scanning"`
		} `json:"components"`
	}
	if err := json.Unmarshal(deps, &inv); err != nil {
		t.Fatal(err)
	}
	db := inv.Components.Scanning.OSVDatabase
	a := db.Snapshots["Alpine"]
	if db.Version != date || a.SHA256 != "sha256:"+sum(alpine) || a.Size != 22 || !strings.HasSuffix(a.URL, "generation=1795999999999999") {
		t.Errorf("dependencies.json: %s %+v", db.Version, a)
	}
	for _, s := range osvscan.Snapshots {
		if s.Ecosystem != "Alpine" && db.Snapshots[s.Ecosystem].SHA256 != "sha256:"+s.SHA256 {
			t.Errorf("%s changed: %+v", s.Ecosystem, db.Snapshots[s.Ecosystem])
		}
	}
	// Only the pins moved: the files differ from the originals in those lines alone.
	orig, _ := os.ReadFile("../../dependencies.json")
	if d := diffLines(string(orig), string(deps)); d != 4 {
		t.Errorf("dependencies.json: %d lines changed, want 4 (version, url, sha256, size)", d)
	}
	origPins, _ := os.ReadFile("../../internal/osvscan/pins.go")
	if d := diffLines(string(origPins), string(pins)); d != 2 {
		t.Errorf("pins.go: %d lines changed, want 2", d)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func diffLines(a, b string) int {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(al) != len(bl) {
		return -1
	}
	n := 0
	for i := range al {
		if al[i] != bl[i] {
			n++
		}
	}
	return n
}
