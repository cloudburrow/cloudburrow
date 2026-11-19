// Command osvmirror keeps Artifact Registry's vulnerability scanning
// prefetchable (#1917). OSV keeps a pinned database generation for only
// about a week, so:
//
//	osvmirror stage -out DIR [-mirror BASE,...]
//	    downloads the scanner's four binaries and the Mirrored ecosystems'
//	    pinned snapshots (upstream first, then each mirror base, such as an
//	    earlier release's downloads), checks each against its pinned
//	    SHA-256, and writes them under DIR with their release asset names,
//	    an osv-mirror-SHA256SUMS list and an osv-mirror-NOTICE.txt.
//	    release.yml's osv-mirror job uploads DIR to the release.
//
//	osvmirror fetch -dir CACHE [-ecosystems Alpine,...] [-mirror BASE,...]
//	    what `cloudburrow prefetch --vulnerability-scanning` stores, into a
//	    prefetch cache directory, without the rest of prefetch: CI's run
//	    shard uses it.
//
//	osvmirror bump [-min-age DAYS] [-date YYYY-MM-DD]
//	    moves every snapshot pin to OSV's current generation: reads each
//	    archive's generation and size from the bucket's metadata, downloads
//	    that generation, hashes it, and rewrites internal/osvscan/pins.go
//	    and dependencies.json together. With -min-age it does nothing while
//	    the pins are younger than that many days. osv-pins.yml runs it.
//
// The scanner binaries are not bumped here: a new OSV-Scanner release is a
// dependency update (tools/depcheck reports it).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/osvscan"
)

// Where bump reads OSV's bucket: the Cloud Storage JSON API's object
// metadata, which names the live generation and size, and the bucket's
// public download host. Variables so a test serves both locally.
var (
	metadataBase = "https://storage.googleapis.com/storage/v1/b/osv-vulnerabilities/o/"
	downloadBase = "https://osv-vulnerabilities.storage.googleapis.com/"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "stage":
		err = runStage(ctx, os.Args[2:], os.Stdout)
	case "fetch":
		err = runFetch(ctx, os.Args[2:], os.Stdout)
	case "bump":
		err = runBump(ctx, os.Args[2:], ".", time.Now().UTC(), os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "osvmirror:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: osvmirror stage -out DIR [-mirror BASE,...] | fetch -dir CACHE [-ecosystems E,...] [-mirror BASE,...] | bump [-min-age DAYS] [-date YYYY-MM-DD]")
	os.Exit(2)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func logTo(w io.Writer) func(string, ...any) {
	return func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
}

var client = &http.Client{Timeout: 30 * time.Minute}

func runFetch(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	dir := fs.String("dir", "", "the prefetch cache directory (<state dir>/cache)")
	ecosystems := fs.String("ecosystems", "", "comma-separated ecosystems (default: as prefetch)")
	mirrors := fs.String("mirror", "", "comma-separated release download bases tried first (default: the latest release)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("fetch: -dir is required")
	}
	m := splitList(*mirrors)
	if *mirrors == "" {
		m = osvscan.DefaultMirrors("")
	}
	f := osvscan.Fetcher{Client: client, Logf: logTo(out), Mirrors: m}
	n, err := f.Prefetch(ctx, osvscan.Dir(*dir), splitList(*ecosystems)...)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "OSV-Scanner %s and the OSV database of %s are in %s (%d bytes downloaded)\n", osvscan.Version, osvscan.SnapshotDate, osvscan.Dir(*dir).DBDir(), n)
	return osvscan.Ready(osvscan.Dir(*dir))
}

// stageItem is one release asset the mirror holds.
type stageItem struct {
	asset   string
	sources []string
	sha256  string
	size    int64
}

// stageItems are the mirror's assets, each with its upstream URL first and
// then the mirror bases: at release time the upstream generation is
// usually still served, and an earlier release holds it when it is not.
func stageItems(mirrors []string) []stageItem {
	var items []stageItem
	keys := make([]string, 0, len(osvscan.Binaries))
	for k := range osvscan.Binaries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := osvscan.Binaries[k]
		items = append(items, stageItem{osvscan.MirrorBinaryAsset(b), withMirrors(osvscan.BinaryURL(b), osvscan.MirrorBinaryAsset(b), mirrors), b.SHA256, b.Size})
	}
	for _, s := range osvscan.Snapshots {
		if !osvscan.IsMirrored(s.Ecosystem) || s.SHA256 == "" {
			continue
		}
		items = append(items, stageItem{osvscan.MirrorSnapshotAsset(s), withMirrors(osvscan.SnapshotURL(s), osvscan.MirrorSnapshotAsset(s), mirrors), s.SHA256, s.Size})
	}
	return items
}

func withMirrors(upstream, asset string, mirrors []string) []string {
	urls := []string{upstream}
	for _, m := range mirrors {
		urls = append(urls, strings.TrimSuffix(m, "/")+"/"+asset)
	}
	return urls
}

func runStage(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	dir := fs.String("out", "", "the directory to write the release assets to")
	mirrors := fs.String("mirror", "", "comma-separated release download bases tried after upstream")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("stage: -out is required")
	}
	f := osvscan.Fetcher{Client: client, Logf: logTo(out)}
	var sums strings.Builder
	var total int64
	for _, it := range stageItems(splitList(*mirrors)) {
		if _, err := f.FetchFile(ctx, it.sources, filepath.Join(*dir, it.asset), it.sha256, it.size, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", it.sha256, it.asset)
		total += it.size
	}
	if err := os.WriteFile(filepath.Join(*dir, "osv-mirror-SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "osv-mirror-NOTICE.txt"), []byte(notice()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "staged %d bytes of OSV-Scanner %s and the OSV database of %s (%s) in %s\n",
		total, osvscan.Version, osvscan.SnapshotDate, strings.Join(osvscan.Mirrored, ", "), *dir)
	return nil
}

// notice is the attribution shipped beside the mirrored files.
func notice() string {
	var b strings.Builder
	fmt.Fprintf(&b, `CloudBurrow's mirror of Artifact Registry vulnerability scanning's downloads (#1917)

These files are unmodified copies, kept so that `+"`cloudburrow prefetch --vulnerability-scanning`"+`
keeps working after the upstream copies are gone. The CLI checks every file against the SHA-256
pinned in internal/osvscan/pins.go and dependencies.json whichever copy it downloads.

OSV-Scanner %s (osv-scanner-%s-*)
  Copyright Google LLC. Licensed under the Apache License, Version 2.0; a copy is in this
  repository's LICENSE file and at https://www.apache.org/licenses/LICENSE-2.0
  Source: https://github.com/google/osv-scanner/releases/tag/%s

OSV database snapshot of %s (osv-db-<ecosystem>-<sha256 prefix>.zip)
  OSV's data dumps, https://google.github.io/osv.dev/data/#data-dumps, as served by
  https://osv-vulnerabilities.storage.googleapis.com/<ecosystem>/all.zip at these generations:
`, osvscan.Version, osvscan.Version, osvscan.Version, osvscan.SnapshotDate)
	for _, s := range osvscan.Snapshots {
		if osvscan.IsMirrored(s.Ecosystem) {
			fmt.Fprintf(&b, "    %-8s generation %s, sha256 %s\n", s.Ecosystem, s.Generation, s.SHA256)
		}
	}
	b.WriteString(`  Each record keeps its source database's licence and attribution, which the record names
  (for example CC-BY-4.0 for the GitHub Advisory Database, the Go vulnerability database and
  PyPA's advisory database); the distributions' trackers (Alpine secdb, the Debian security
  tracker) set their own terms. See https://google.github.io/osv.dev/data/ for the sources.
`)
	return b.String()
}

// generation is the metadata bump reads for one archive.
type generation struct {
	Generation string `json:"generation"`
	Size       string `json:"size"`
}

func currentGeneration(ctx context.Context, ecosystem string) (generation, error) {
	var g generation
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+url.PathEscape(ecosystem+"/all.zip"), nil)
	if err != nil {
		return g, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return g, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g, fmt.Errorf("%s/all.zip metadata: %s", ecosystem, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return g, fmt.Errorf("%s/all.zip metadata: %w", ecosystem, err)
	}
	if _, err := strconv.ParseUint(g.Generation, 10, 64); err != nil {
		return g, fmt.Errorf("%s/all.zip metadata: generation %q", ecosystem, g.Generation)
	}
	return g, nil
}

// hashGeneration downloads one generation and returns its SHA-256 and
// size, without keeping it.
func hashGeneration(ctx context.Context, ecosystem, gen string) (string, int64, error) {
	u := downloadBase + ecosystem + "/all.zip?generation=" + gen
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: %s", u, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", n, fmt.Errorf("download %s: %w", u, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func runBump(ctx context.Context, args []string, root string, now time.Time, out io.Writer) error {
	fs := flag.NewFlagSet("bump", flag.ContinueOnError)
	minAge := fs.Int("min-age", 0, "do nothing while the pins are younger than this many days")
	date := fs.String("date", now.Format("2006-01-02"), "the new pins' date")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pinned, err := time.Parse("2006-01-02", osvscan.SnapshotDate)
	if err != nil {
		return err
	}
	if age := int(now.Sub(pinned).Hours() / 24); age < *minAge {
		fmt.Fprintf(out, "the pins of %s are %d days old (under %d): nothing to do\n", osvscan.SnapshotDate, age, *minAge)
		return nil
	}
	if _, err := time.Parse("2006-01-02", *date); err != nil {
		return fmt.Errorf("-date: %w", err)
	}
	var fresh []osvscan.Snapshot
	for _, s := range osvscan.Snapshots {
		g, err := currentGeneration(ctx, s.Ecosystem)
		if err != nil {
			return err
		}
		if g.Generation == s.Generation {
			fresh = append(fresh, s)
			fmt.Fprintf(out, "%s: generation %s unchanged\n", s.Ecosystem, s.Generation)
			continue
		}
		sum, n, err := hashGeneration(ctx, s.Ecosystem, g.Generation)
		if err != nil {
			return err
		}
		if want, err := strconv.ParseInt(g.Size, 10, 64); err == nil && want != n {
			return fmt.Errorf("%s generation %s: downloaded %d bytes, the metadata says %d", s.Ecosystem, g.Generation, n, want)
		}
		fresh = append(fresh, osvscan.Snapshot{Ecosystem: s.Ecosystem, Generation: g.Generation, SHA256: sum, Size: n})
		fmt.Fprintf(out, "%s: generation %s -> %s, sha256 %s, %d bytes\n", s.Ecosystem, s.Generation, g.Generation, sum, n)
	}
	pinsPath := filepath.Join(root, "internal", "osvscan", "pins.go")
	depsPath := filepath.Join(root, "dependencies.json")
	pins, err := os.ReadFile(pinsPath)
	if err != nil {
		return err
	}
	deps, err := os.ReadFile(depsPath)
	if err != nil {
		return err
	}
	newPins, err := rewritePins(string(pins), *date, fresh)
	if err != nil {
		return err
	}
	newDeps, err := rewriteDependencies(string(deps), *date, fresh)
	if err != nil {
		return err
	}
	if err := os.WriteFile(pinsPath, []byte(newPins), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(depsPath, []byte(newDeps), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "pins moved to %s in %s and %s\n", *date, pinsPath, depsPath)
	return nil
}

var snapshotDateRE = regexp.MustCompile(`(?m)^const SnapshotDate = "[0-9-]+"$`)

// rewritePins replaces SnapshotDate and each snapshot's line in pins.go.
func rewritePins(src, date string, snaps []osvscan.Snapshot) (string, error) {
	if !snapshotDateRE.MatchString(src) {
		return "", errors.New("pins.go: no SnapshotDate line")
	}
	src = snapshotDateRE.ReplaceAllString(src, `const SnapshotDate = "`+date+`"`)
	for _, s := range snaps {
		re := regexp.MustCompile(`\{"` + regexp.QuoteMeta(s.Ecosystem) + `", "[0-9]+", "[0-9a-f]{64}", [0-9]+\}`)
		if len(re.FindAllString(src, -1)) != 1 {
			return "", fmt.Errorf("pins.go: not exactly one %s snapshot line", s.Ecosystem)
		}
		src = re.ReplaceAllLiteralString(src, fmt.Sprintf(`{%q, %q, %q, %d}`, s.Ecosystem, s.Generation, s.SHA256, s.Size))
	}
	return src, nil
}

// rewriteDependencies replaces osvDatabase's version and each snapshot's
// url, sha256 and size in dependencies.json, leaving every other byte.
func rewriteDependencies(src, date string, snaps []osvscan.Snapshot) (string, error) {
	i := strings.Index(src, `"osvDatabase": {`)
	if i < 0 {
		return "", errors.New(`dependencies.json: no "osvDatabase"`)
	}
	head, db := src[:i], src[i:]
	ver := regexp.MustCompile(`"version": "[0-9-]+"`)
	loc := ver.FindStringIndex(db)
	if loc == nil {
		return "", errors.New("dependencies.json: osvDatabase has no version")
	}
	db = db[:loc[0]] + `"version": "` + date + `"` + db[loc[1]:]
	for _, s := range snaps {
		re := regexp.MustCompile(`("` + regexp.QuoteMeta(s.Ecosystem) + `": \{\s*"url": )"[^"]*"(,\s*"sha256": )"[^"]*"(,\s*"size": )[0-9]+`)
		if len(re.FindAllString(db, -1)) != 1 {
			return "", fmt.Errorf("dependencies.json: not exactly one %s snapshot", s.Ecosystem)
		}
		db = re.ReplaceAllString(db, `${1}"`+osvscan.SnapshotURL(s)+`"${2}"sha256:`+s.SHA256+`"${3}`+strconv.FormatInt(s.Size, 10))
	}
	return head + db, nil
}
