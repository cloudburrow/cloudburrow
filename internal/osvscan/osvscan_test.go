package osvscan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	pb "google.golang.org/genproto/googleapis/grafeas/v1"
)

// result is OSV-Scanner v2.7.1's output for an Alpine 3.13 image with
// zlib 1.2.11-r3, cut down to what Convert reads.
const result = `{"results":[{"source":{"path":"/lib/apk/db/installed","type":"os"},"packages":[{
 "package":{"name":"zlib","os_package_name":"zlib","version":"1.2.11-r3","ecosystem":"Alpine:v3.13"},
 "vulnerabilities":[{"id":"ALPINE-CVE-2022-37434","upstream":["CVE-2022-37434"],"details":"zlib through 1.2.12 has a heap-based buffer over-read",
  "severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
  "affected":[{"package":{"name":"zlib","ecosystem":"Alpine:v3.12"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.2.12-r9"}]}]},
   {"package":{"name":"zlib","ecosystem":"Alpine:v3.13"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.2.12-r2"}]}]}],
  "references":[{"type":"ADVISORY","url":"https://security.alpinelinux.org/vuln/CVE-2022-37434"}]},
  {"id":"GHSA-xxxx-yyyy-zzzz","details":"no fix","affected":[{"package":{"name":"zlib","ecosystem":"Alpine:v3.13"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"}]}]}]}],
 "groups":[{"ids":["ALPINE-CVE-2022-37434"],"max_severity":"9.8"}]}]},
 {"source":{"path":"/app/go.mod","type":"lockfile"},"packages":[{"package":{"name":"golang.org/x/net","version":"0.1.0","ecosystem":"Go"},"vulnerabilities":[]}]}],
 "image_metadata":{"os":"Alpine Linux v3.13"}}`

func TestConvert(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatal(err)
	}
	f := Convert(&r)
	if f.CPE != "cpe:/o:alpine:alpine_linux:3.13" || strings.Join(f.AnalysisTypes, ",") != "GO,OS" {
		t.Errorf("CPE %q, analysis types %v", f.CPE, f.AnalysisTypes)
	}
	if len(f.Packages) != 2 || f.Packages[0].GetPackage().GetVersion().GetRevision() != "r3" || f.Packages[1].GetPackage().GetPackageType() != "GO" {
		t.Errorf("packages: %v", f.Packages)
	}
	if len(f.Vulnerabilities) != 2 {
		t.Fatalf("vulnerabilities: %v", f.Vulnerabilities)
	}
	v := f.Vulnerabilities[0]
	vo := v.GetVulnerability()
	pi := vo.GetPackageIssue()[0]
	if v.GetNoteName() != "projects/goog-vulnz/notes/CVE-2022-37434" || vo.GetSeverity() != pb.Severity_CRITICAL || vo.GetCvssScore() != 9.8 ||
		!vo.GetFixAvailable() || pi.GetFixedVersion().GetFullName() != "1.2.12-r2" || pi.GetAffectedVersion().GetName() != "1.2.11" ||
		vo.GetCvssv3().GetBaseScore() != 9.8 || vo.GetCvssv3().GetScope() != pb.CVSS_SCOPE_UNCHANGED {
		t.Errorf("CVE-2022-37434: %v", v)
	}
	nf := f.Vulnerabilities[1].GetVulnerability()
	if f.Vulnerabilities[1].GetNoteName() != "projects/goog-vulnz/notes/GHSA-xxxx-yyyy-zzzz" || nf.GetFixAvailable() ||
		nf.GetPackageIssue()[0].GetFixedVersion().GetKind() != pb.Version_MAXIMUM || nf.GetSeverity() != pb.Severity_SEVERITY_UNSPECIFIED {
		t.Errorf("unfixed: %v", f.Vulnerabilities[1])
	}
	names := map[string]bool{}
	for _, n := range f.Notes {
		names[n.GetName()] = true
	}
	for _, want := range []string{"projects/goog-vulnz/notes/CVE-2022-37434", "projects/goog-analysis/notes/PACKAGE_OS", "projects/goog-analysis/notes/PACKAGE_GO"} {
		if !names[want] {
			t.Errorf("no note %s in %v", want, names)
		}
	}
}

// layered is OSV-Scanner v2.7.1's shape for an image scan run with
// --licenses (#2066): each package's licenses and the index of the layer
// it came from, and the image's layers.
const layered = `{"results":[{"source":{"path":"/lib/apk/db/installed","type":"os"},"packages":[
 {"package":{"name":"musl","version":"1.2.5-r0","ecosystem":"Alpine:v3.20","image_origin_details":{"index":1}},
  "licenses":["MIT","MIT AND BSD-2-Clause"],
  "vulnerabilities":[{"id":"ALPINE-CVE-2025-26519","upstream":["CVE-2025-26519"],"affected":[{"package":{"name":"musl","ecosystem":"Alpine:v3.20"},
   "ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.2.5-r1"}]}]}]}]},
 {"package":{"name":"zlib","version":"1.3.1-r1","ecosystem":"Alpine:v3.20","image_origin_details":{"index":0}},"licenses":["Zlib"]},
 {"package":{"name":"busybox","version":"1.36.1-r29","ecosystem":"Alpine:v3.20"},"licenses":["unknown"]}]}],
 "image_metadata":{"os":"Alpine Linux v3.20","layer_metadata":[
  {"diff_id":"sha256:88b4","command":"ADD alpine-minirootfs.tar.gz / # buildkit","base_image_index":0},
  {"diff_id":"sha256:5f1a","command":"RUN apk add musl","base_image_index":1},
  {"diff_id":"","command":"CMD [\"/bin/sh\"]","is_empty":true}],
  "base_images":[[],[{"name":"alpine:3.20"}]]}}`

func TestConvertRecordsLicensesAndLayers(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(layered), &r); err != nil {
		t.Fatal(err)
	}
	f := Convert(&r)
	lic := map[string]string{}
	for _, o := range f.Packages {
		lic[o.GetPackage().GetName()] = o.GetPackage().GetLicense().GetExpression()
	}
	if lic["musl"] != "MIT AND (MIT AND BSD-2-Clause)" || lic["zlib"] != "Zlib" || lic["busybox"] != "" {
		t.Errorf("licenses: %v", lic)
	}
	if len(f.Vulnerabilities) != 1 {
		t.Fatalf("vulnerabilities: %v", f.Vulnerabilities)
	}
	l := f.Vulnerabilities[0].GetVulnerability().GetPackageIssue()[0].GetFileLocation()[0].GetLayerDetails()
	if l.GetIndex() != 1 || l.GetDiffId() != "sha256:5f1a" || l.GetCommand() != "RUN apk add musl" ||
		len(l.GetBaseImages()) != 1 || l.GetBaseImages()[0].GetName() != "alpine:3.20" {
		t.Errorf("layer: %v", l)
	}
	// A directory scan names no layer.
	var plain Result
	if err := json.Unmarshal([]byte(result), &plain); err != nil {
		t.Fatal(err)
	}
	if l := Convert(&plain).Vulnerabilities[0].GetVulnerability().GetPackageIssue()[0].GetFileLocation()[0].GetLayerDetails(); l != nil {
		t.Errorf("layer outside an image scan: %v", l)
	}
}

// The CVSS v3.1 specification's examples.
func TestCVSS3BaseScore(t *testing.T) {
	for vec, want := range map[string]float32{
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H": 9.8,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N": 6.1,
		"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N": 5.5,
		"CVSS:3.0/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:N/A:N": 5.9,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N": 0,
	} {
		if got := parseCVSS3(vec).GetBaseScore(); got != want {
			t.Errorf("%s: %v, want %v", vec, got, want)
		}
	}
	if parseCVSS3("AV:N") != nil || parseCVSS3("CVSS:3.1/AV:X") != nil {
		t.Error("an invalid vector parsed")
	}
}

func TestVersionSplit(t *testing.T) {
	v := version("1:2.3.4-5+deb12u1", true)
	if v.GetEpoch() != 1 || v.GetName() != "2.3.4" || v.GetRevision() != "5+deb12u1" || v.GetFullName() != "1:2.3.4-5+deb12u1" {
		t.Errorf("%v", v)
	}
	if v := version("v1.2.3-pre", false); v.GetName() != "v1.2.3-pre" || v.GetRevision() != "" {
		t.Errorf("%v", v)
	}
}

func TestPrefetchChecksAndSkips(t *testing.T) {
	bin := []byte("scanner")
	zip := []byte("alpine db")
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "osv-scanner_"):
			_, _ = w.Write(bin)
		case strings.Contains(r.URL.Path, "Alpine"):
			_, _ = w.Write(zip)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host, err := HostBinary()
	if err != nil {
		t.Skip(err)
	}
	key := ""
	for k, b := range Binaries {
		if b == host {
			key = k
		}
	}
	oldB, oldS := Binaries[key], Snapshots
	t.Cleanup(func() { Binaries[key], Snapshots = oldB, oldS })
	Binaries[key] = Binary{Name: host.Name, SHA256: sum(bin), Size: int64(len(bin))}
	Snapshots = []Snapshot{{"Alpine", "1", sum(zip), int64(len(zip))}, {"Debian", "1", sum([]byte("other")), 5}}
	c := srv.Client()
	rt := c.Transport
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return rt.RoundTrip(r)
	})
	d := Dir(t.TempDir())
	n, err := Fetcher{Client: c}.Prefetch(context.Background(), d, "alpine")
	if err != nil || n != int64(len(bin)+len(zip)) {
		t.Fatalf("Prefetch: %d, %v", n, err)
	}
	if err := Ready(d); err != nil {
		t.Errorf("Ready: %v", err)
	}
	if n, err := (Fetcher{Client: c}).Prefetch(context.Background(), d, "Alpine"); err != nil || n != 0 {
		t.Errorf("a second Prefetch downloaded %d, %v", n, err)
	}
	if _, err := (Fetcher{Client: c}).Prefetch(context.Background(), d, "Debian"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing generation: %v", err)
	}
	if _, err := (Fetcher{Client: c}).Prefetch(context.Background(), d, "Nope"); err == nil {
		t.Error("an unpinned ecosystem was accepted")
	}
	// A tampered snapshot is not ready.
	d2 := Dir(t.TempDir())
	if _, err := (Fetcher{Client: c}).Prefetch(context.Background(), d2, "Alpine"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d2.SnapshotPath(Snapshots[0]), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ready(d2); err == nil {
		t.Error("a tampered snapshot is ready")
	}
	c.CloseIdleConnections()
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestScanWithThePinnedScanner runs the real scanner when a prefetch cache
// holding it is named by CLOUDBURROW_TEST_OSV_CACHE, over an archive named
// by CLOUDBURROW_TEST_OSV_ARCHIVE.
func TestScanWithThePinnedScanner(t *testing.T) {
	dir, archive := os.Getenv("CLOUDBURROW_TEST_OSV_CACHE"), os.Getenv("CLOUDBURROW_TEST_OSV_ARCHIVE")
	if dir == "" || archive == "" {
		t.Skip("set CLOUDBURROW_TEST_OSV_CACHE and CLOUDBURROW_TEST_OSV_ARCHIVE to scan with the prefetched OSV-Scanner")
	}
	r, err := Scan(context.Background(), Dir(dir), archive)
	if err != nil {
		t.Fatal(err)
	}
	f := Convert(r)
	if len(f.Vulnerabilities) == 0 {
		t.Errorf("no vulnerabilities: %+v", r)
	}
	// --licenses reads each OS package's license from its database, offline
	// (#2066), and an image scan names each package's layer.
	licensed := 0
	for _, o := range f.Packages {
		if o.GetPackage().GetLicense().GetExpression() != "" {
			licensed++
		}
	}
	if licensed == 0 {
		t.Errorf("no package has a license: %v", f.Packages)
	}
	for _, o := range f.Vulnerabilities {
		if o.GetVulnerability().GetPackageIssue()[0].GetFileLocation()[0].GetLayerDetails() == nil {
			t.Errorf("%s names no layer", o.GetNoteName())
		}
	}
}

// The pins here are dependencies.json's scanning entries.
func TestPinsMatchDependenciesJSON(t *testing.T) {
	b, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		Components struct {
			Scanning struct {
				OSVScanner struct {
					Version   string            `json:"version"`
					Checksums map[string]string `json:"checksums"`
				} `json:"osvScanner"`
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
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	sc := inv.Components.Scanning
	if sc.OSVScanner.Version != Version || len(sc.OSVScanner.Checksums) != len(Binaries) {
		t.Errorf("osvScanner %s with %d checksums; pins.go has %s with %d", sc.OSVScanner.Version, len(sc.OSVScanner.Checksums), Version, len(Binaries))
	}
	for _, bin := range Binaries {
		if sc.OSVScanner.Checksums[bin.Name] != "sha256:"+bin.SHA256 {
			t.Errorf("%s: dependencies.json %q, pins.go %s", bin.Name, sc.OSVScanner.Checksums[bin.Name], bin.SHA256)
		}
	}
	if sc.OSVDatabase.Version != SnapshotDate || len(sc.OSVDatabase.Snapshots) != len(Snapshots) {
		t.Errorf("osvDatabase %s with %d snapshots; pins.go has %s with %d", sc.OSVDatabase.Version, len(sc.OSVDatabase.Snapshots), SnapshotDate, len(Snapshots))
	}
	for _, s := range Snapshots {
		j := sc.OSVDatabase.Snapshots[s.Ecosystem]
		if j.URL != SnapshotURL(s) || j.SHA256 != "sha256:"+s.SHA256 || j.Size != s.Size {
			t.Errorf("%s: dependencies.json %+v, pins.go %+v", s.Ecosystem, j, s)
		}
	}
}

// TestPrefetchTriesTheMirrorFirst (#1917): each file comes from the first
// mirror base serving its pinned bytes, then upstream; a base serving
// other bytes is passed over; an unmirrored ecosystem whose generation is
// gone is left out of a prefetch that names none, and fails one that
// names it.
func TestPrefetchTriesTheMirrorFirst(t *testing.T) {
	bin, alpine, ubuntu := []byte("scanner"), []byte("alpine db"), []byte("ubuntu db")
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	host, err := HostBinary()
	if err != nil {
		t.Skip(err)
	}
	key := runtime.GOOS + "/" + runtime.GOARCH
	oldB, oldS := Binaries[key], Snapshots
	t.Cleanup(func() { Binaries[key], Snapshots = oldB, oldS })
	Binaries[key] = Binary{Name: host.Name, SHA256: sum(bin), Size: int64(len(bin))}
	Snapshots = []Snapshot{{"Alpine", "1", sum(alpine), int64(len(alpine))}, {"Ubuntu", "1", sum(ubuntu), int64(len(ubuntu))}}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/bad/" + MirrorSnapshotAsset(Snapshots[0]):
			_, _ = w.Write([]byte("not the pinned bytes"))
		case "/good/" + MirrorSnapshotAsset(Snapshots[0]):
			_, _ = w.Write(alpine)
		case "/good/" + MirrorBinaryAsset(Binaries[key]):
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := Fetcher{Client: srv.Client(), Mirrors: []string{srv.URL + "/bad", srv.URL + "/good/"}}
	if got := f.SnapshotSources(Snapshots[1]); len(got) != 1 || got[0] != SnapshotURL(Snapshots[1]) {
		t.Errorf("Ubuntu, unmirrored, is fetched from %v", got)
	}
	var log strings.Builder
	f.Logf = func(format string, a ...any) { fmt.Fprintf(&log, format, a...) }
	d := Dir(t.TempDir())
	// Every other host answers 404, as OSV's bucket does once a pinned
	// generation is gone; nothing leaves the machine.
	f.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != strings.TrimPrefix(srv.URL, "http://") {
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Request: r}, nil
		}
		return srv.Client().Transport.RoundTrip(r)
	})}
	if _, err := f.Prefetch(context.Background(), d); err != nil {
		t.Fatalf("Prefetch: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "WARNING: Ubuntu is not mirrored") {
		t.Errorf("no warning for the missing Ubuntu snapshot:\n%s", log.String())
	}
	if got := strings.Join(asked, " "); got != "/bad/"+MirrorBinaryAsset(Binaries[key])+" /good/"+MirrorBinaryAsset(Binaries[key])+
		" /bad/"+MirrorSnapshotAsset(Snapshots[0])+" /good/"+MirrorSnapshotAsset(Snapshots[0]) {
		t.Errorf("asked %s", got)
	}
	if err := Ready(d); err != nil {
		t.Error(err)
	}
	if _, err := f.Prefetch(context.Background(), d, "Ubuntu"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a named Ubuntu whose generation is gone: %v", err)
	}
}

func TestDefaultMirrors(t *testing.T) {
	if m := DefaultMirrors("v0.31.2"); len(m) != 2 || m[0] != "https://github.com/cloudburrow/cloudburrow/releases/download/v0.31.2" ||
		m[1] != "https://github.com/cloudburrow/cloudburrow/releases/latest/download" {
		t.Errorf("a release: %v", m)
	}
	if m := DefaultMirrors("v0.31.2-0.20261119-abcdef"); len(m) != 1 {
		t.Errorf("a pseudo-version: %v", m)
	}
	for _, s := range Snapshots {
		if a := MirrorSnapshotAsset(s); !strings.HasPrefix(a, "osv-db-"+s.Ecosystem+"-"+s.SHA256[:16]) {
			t.Errorf("%s: %s", s.Ecosystem, a)
		}
	}
	if IsMirrored("Ubuntu") || !IsMirrored("pypi") {
		t.Error("IsMirrored")
	}
}

// An offline scan missing an ecosystem's database names the ecosystems,
// each once, so the scan is reported as not prefetched (#2072).
func TestMissingDatabases(t *testing.T) {
	out := "Error during extraction: (extracting as vulnmatch/osvlocal) could not load db for Go ecosystem: unable to fetch OSV database: no offline version of the OSV database is available\n" +
		"could not load db for Maven ecosystem: unable to fetch OSV database: no offline version of the OSV database is available\n" +
		"could not load db for Go ecosystem: unable to fetch OSV database: no offline version of the OSV database is available\n"
	if got := strings.Join(missingDatabases(out), ","); got != "Go,Maven" {
		t.Errorf("missingDatabases = %q; want Go,Maven", got)
	}
	if got := missingDatabases("osv-scanner: some other failure"); len(got) != 0 {
		t.Errorf("missingDatabases of an unrelated failure = %q", got)
	}
}
