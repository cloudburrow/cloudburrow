// Package osvscan runs Artifact Registry's vulnerability scanning on this
// machine (#1912): Google's OSV-Scanner (https://github.com/google/osv-scanner,
// Apache-2.0) over an image archive, in its offline mode, against a pinned
// snapshot of the OSV database, and turns its findings into Grafeas
// occurrences in the shape Artifact Analysis writes them.
//
// Nothing here reaches the network while an instance runs. The scanner's
// release binary and the database's ecosystem archives are downloaded by
// `cloudburrow prefetch`, each checked against the SHA-256 pinned here and
// in dependencies.json (scanning.osvScanner, scanning.osvDatabase), and the
// scanner is run with --offline (no osv.dev, no deps.dev, no registry
// pulls), its local database directory pointed at the prefetched snapshot,
// and every proxy variable pointed at a closed port, so a call it might
// still try fails on the machine.
package osvscan

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// Version is the pinned OSV-Scanner release.
const Version = "v2.7.1"

// Binary is one platform's release asset.
type Binary struct {
	Name   string
	SHA256 string
	Size   int64
}

// Binaries are the release's assets, by GOOS/GOARCH, with the SHA-256 the
// release's osv-scanner_SHA256SUMS lists.
var Binaries = map[string]Binary{
	"darwin/amd64": {"osv-scanner_darwin_amd64", "b8420d5485ba792626315f1bf9a5a2046e971d38eea75bf9090c38852dbc3b1b", 59352784},
	"darwin/arm64": {"osv-scanner_darwin_arm64", "b22c02877a811c8f78956e8643efa554932b7378f590d13f8e97d4e9448a29f3", 55745714},
	"linux/amd64":  {"osv-scanner_linux_amd64", "fb7dab19a8bd9bbe5c3189f09d4b7ee1820c9553a23bac202648a56efd2f6ee8", 58187936},
	"linux/arm64":  {"osv-scanner_linux_arm64", "983d22fbad9724514e3fcc6acc83b5fa64c5ffa6b0d9f510028f22c1b223ff81", 54198432},
}

// BinaryURL is where a release asset is downloaded from: the project's own
// GitHub release.
func BinaryURL(b Binary) string {
	return "https://github.com/google/osv-scanner/releases/download/" + Version + "/" + b.Name
}

// HostBinary is the asset for this machine.
func HostBinary() (Binary, error) {
	b, ok := Binaries[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return Binary{}, fmt.Errorf("OSV-Scanner %s publishes no binary for %s/%s", Version, runtime.GOOS, runtime.GOARCH)
	}
	return b, nil
}

// Snapshot is one ecosystem's archive of the OSV database, as OSV
// publishes it (https://google.github.io/osv.dev/data/#data-dumps), pinned
// to one object generation so every prefetch gets the same bytes.
type Snapshot struct {
	Ecosystem  string
	Generation string
	SHA256     string
	Size       int64
}

// SnapshotDate is when the pinned generations were published.
const SnapshotDate = "2026-11-22"

// Snapshots are the ecosystems Artifact Analysis scans that the pin
// covers: the OS packages of Alpine, Debian and Ubuntu images, and Go,
// Java (Maven), Node.js (npm) and Python (PyPI) packages.
var Snapshots = []Snapshot{
	{"Alpine", "1795278833833204", "512eeca33f6bc9a3a7bbfde33208ac28eaee1ab8af4e7c718590ae36e0a5a5f8", 9332090},
	{"Debian", "1795341799208230", "c50c6fbb104e5ceae918f81377abc253203bf48af97fda6daab2324310abc20a", 84452448},
	{"Go", "1795285104654618", "58569ce71c642873b1d222b9feb844a01ebd1833fcfb8c13988acec343cf04b9", 13013636},
	{"Maven", "1795192410682818", "75fb59af0d2c7bbae5ef447ca1093b371168b0ec0704c77e9bb1271f83a0aff8", 10561066},
	{"PyPI", "1795303098236874", "88f6042e2648caaa8a961ca8b4a79bae7a9aa5797064e1814b9279e516624db6", 38288593},
	{"npm", "1795332791486905", "64021a51406b00b341f9b5b4e8428cec210404db3a857d4f9ff8cb2767091e24", 222050720},
	{"Ubuntu", "1795157303811323", "c2d95e70fcf080f2eb731986b8099142ed343a0f9e6d0f123ddde5943ac17f6c", 821653609},
}

// SnapshotURL is the OSV data dump's archive of one ecosystem at the pinned
// generation.
func SnapshotURL(s Snapshot) string {
	return "https://osv-vulnerabilities.storage.googleapis.com/" + s.Ecosystem + "/all.zip?generation=" + s.Generation
}

// Mirrored are the ecosystems each CloudBurrow release mirrors (#1917):
// release.yml's osv-mirror job uploads their pinned snapshots, with the
// scanner's four binaries, as release assets, because OSV keeps a pinned
// generation for only about a week and a release's assets stay. They are
// about 375 MB together. Ubuntu (about 820 MB, more than twice the rest
// together, and over a gigabyte of release assets a day with the scanner)
// is not mirrored: it is fetched from OSV's live bucket only, so it is
// prefetched while its pin is fresh, and a prefetch that names no
// ecosystem goes on without it when its generation is gone.
var Mirrored = []string{"Alpine", "Debian", "Go", "Maven", "PyPI", "npm"}

// IsMirrored reports whether a release mirrors the ecosystem's snapshot.
func IsMirrored(ecosystem string) bool {
	for _, e := range Mirrored {
		if strings.EqualFold(e, ecosystem) {
			return true
		}
	}
	return false
}

// MirrorRepo is the GitHub repository whose releases carry the mirror.
const MirrorRepo = "cloudburrow/cloudburrow"

// MirrorSnapshotAsset is a snapshot's release asset name. It names the
// archive's SHA-256, so the same bytes have the same name in every release
// and a mirror base can be any release that holds them.
func MirrorSnapshotAsset(s Snapshot) string {
	return "osv-db-" + s.Ecosystem + "-" + s.SHA256[:16] + ".zip"
}

// MirrorBinaryAsset is a scanner binary's release asset name.
func MirrorBinaryAsset(b Binary) string {
	return "osv-scanner-" + Version + "-" + strings.TrimPrefix(b.Name, "osv-scanner_")
}

// releaseTag is a release version this CLI may carry.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// DefaultMirrors are the release download bases prefetch tries before
// OSV's bucket and the scanner's own release: this CLI's own release when
// it is one (a build from source is not), then the latest release, which
// holds the same files whenever its pins are this CLI's. Every file is
// checked against the SHA-256 pinned here whichever base serves it.
func DefaultMirrors(cliVersion string) []string {
	base := "https://github.com/" + MirrorRepo + "/releases/"
	var m []string
	if releaseTag.MatchString(cliVersion) {
		m = append(m, base+"download/"+cliVersion)
	}
	return append(m, base+"latest/download")
}
