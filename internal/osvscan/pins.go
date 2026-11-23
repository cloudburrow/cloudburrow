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
const SnapshotDate = "2026-11-23"

// Snapshots are the ecosystems Artifact Analysis scans that the pin
// covers: the OS packages of Alpine, Debian and Ubuntu images, and Go,
// Java (Maven), Node.js (npm) and Python (PyPI) packages.
var Snapshots = []Snapshot{
	{"Alpine", "1795419221904360", "ebd5580be18f3a6e7539da588f3598e5211eae1231bc8930e4dd84ede1b35d3e", 9335526},
	{"Debian", "1795450702350583", "9d3ca9f27dc8dbd3ce0279197c473615bb29e3c8f04170ff758c7f99d18aedf6", 84597617},
	{"Go", "1795364312272971", "91598a9cbf2224b4e4073d81a8b06ea657bdd2b9a55164fb5bf90ed1868dabd3", 13013663},
	{"Maven", "1795449793821021", "7728a025e8f370f12de255fc34076a9c0d1d5db589304945e5e8730483667e40", 10565847},
	{"PyPI", "1795428217478387", "7bac0d0f11b32343f51df2242110831954aada8f87021db95954414363fc8e37", 38293799},
	{"npm", "1795444417986711", "2154a12f56474607be8f9e725ce3f5d5e83d795eaad461b5dc8c68ad191857fe", 222104165},
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
