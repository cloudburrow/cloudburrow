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
	"runtime"
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
const SnapshotDate = "2026-11-19"

// Snapshots are the ecosystems Artifact Analysis scans that the pin
// covers: the OS packages of Alpine, Debian and Ubuntu images, and Go,
// Java (Maven), Node.js (npm) and Python (PyPI) packages.
var Snapshots = []Snapshot{
	{"Alpine", "1795080781716943", "82c1e6b3a170cdc7e663061fc3a766a6c90ec73ac071aa34403bebaed0cca457", 9276935},
	{"Debian", "1795096995130005", "6005ba6323158727283dac884ff01b5693fc57861afb046c85ce2bffe79bc576", 82263485},
	{"Go", "1795050195126703", "525f77fa536859d59215442f6ba98caf100ca7f1bef2dd9a242d9f3fb0f89757", 12956844},
	{"Maven", "1795051974553992", "fd1c96321a41c535c3f4cab7fc4db24f46c659e143eb62493c1a634a917f1c97", 10547334},
	{"PyPI", "1795082587433272", "51d4295bbe14768b99f0b8b91b01b708d8230ac296c8722cc5e29e66d28e8ee4", 38075455},
	{"npm", "1795095210470062", "0601339e91c58525433117f107327a55c96a060534c5614ee6375f6f7bf366f9", 221817800},
	{"Ubuntu", "1795077206037621", "7490ea001180541e8bb10acae93aff85ca368842986cd4d22e68b9ce82d7fb6a", 819766997},
}

// SnapshotURL is the OSV data dump's archive of one ecosystem at the pinned
// generation.
func SnapshotURL(s Snapshot) string {
	return "https://osv-vulnerabilities.storage.googleapis.com/" + s.Ecosystem + "/all.zip?generation=" + s.Generation
}
