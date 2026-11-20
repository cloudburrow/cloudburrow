package run

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/cloudburrow/cloudburrow/internal/grafeas"
	"github.com/cloudburrow/cloudburrow/internal/osvscan"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Artifact Registry's vulnerability scanning (#1912), as Artifact Analysis
// documents it (https://docs.cloud.google.com/artifact-analysis/docs/container-scanning-overview):
//
//   - a DOCKER repository scans what is pushed to it while its
//     vulnerability_scanning_config is INHERITED (or unset) and its
//     project has the Container Scanning API, containerscanning.googleapis.com,
//     enabled in Service Usage, which is off until enabled, as on Google;
//   - each image manifest is scanned once per digest: a DISCOVERY
//     occurrence (note projects/goog-analysis/notes/PACKAGE_VULNERABILITY)
//     goes PENDING, SCANNING and FINISHED_SUCCESS (FINISHED_UNSUPPORTED when
//     nothing in it can be scanned, FINISHED_FAILED with the reason when
//     the scan cannot run), with PACKAGE occurrences for what it holds and
//     VULNERABILITY occurrences (notes projects/goog-vulnz/notes/{CVE})
//     for what OSV's database says affects them;
//   - continuous analysis: an image pushed or pulled within 30 days stays
//     ACTIVE and is scanned again when the database snapshot changes
//     (a newer `cloudburrow prefetch`); after 30 days without a pull it is
//     INACTIVE and its metadata no longer updated, after 90 it is archived
//     (archiveTime), and a pull makes it active again and rescans it.
//
// The scan runs OSV-Scanner on this machine (internal/osvscan), offline.

// ContainerScanningAPI is the API whose state turns scanning on.
const ContainerScanningAPI = "containerscanning.googleapis.com"

var scanState = struct {
	mu      sync.Mutex
	store   *grafeas.Store
	dir     osvscan.Dir
	enabled func(project, api string) bool
	// images are each scanned image's last push or pull and the snapshot
	// it was last scanned with, by resource URI.
	images map[string]*scannedImage
	file   string
	sem    chan struct{}
	wg     sync.WaitGroup
	now    func() time.Time
}{images: map[string]*scannedImage{}, sem: make(chan struct{}, 1), now: time.Now}

type scannedImage struct {
	Repository string    `json:"repository"`
	Image      string    `json:"image"`
	Digest     string    `json:"digest"`
	LastUse    time.Time `json:"lastUse"`
	Snapshot   string    `json:"snapshot"`
}

// UseContainerAnalysis turns scanning on for this instance: st is the
// Container Analysis store the occurrences go to, dir the prefetch cache
// holding OSV-Scanner and its database, enabled reports a project's API
// state, and file keeps the scanned images' continuous-analysis state.
func UseContainerAnalysis(st *grafeas.Store, dir osvscan.Dir, enabled func(project, api string) bool, file string) {
	scanState.mu.Lock()
	scanState.store, scanState.dir, scanState.enabled, scanState.file = st, dir, enabled, file
	scanState.images = map[string]*scannedImage{}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &scanState.images)
	}
	scanState.mu.Unlock()
	if rs := RegistryStore(); rs != nil {
		rs.Pushed = imagePushed
		rs.Pulled = imagePulled
	}
}

func saveScannedLocked() {
	if scanState.file == "" {
		return
	}
	b, err := json.Marshal(scanState.images)
	if err == nil {
		_ = os.WriteFile(scanState.file, b, 0o600)
	}
}

// scanningConfig is a repository's vulnerability_scanning_config as
// Google answers it: the stored enablement_config with the state it
// leads to and why.
func scanningConfig(r *arpb.Repository) *arpb.Repository_VulnerabilityScanningConfig {
	in := r.GetVulnerabilityScanningConfig()
	out := &arpb.Repository_VulnerabilityScanningConfig{EnablementConfig: in.GetEnablementConfig(), LastEnableTime: in.GetLastEnableTime()}
	project := ""
	if p := strings.Split(r.GetName(), "/"); len(p) > 1 {
		project = p[1]
	}
	switch {
	case r.GetFormat() != arpb.Repository_DOCKER || r.GetMode() == arpb.Repository_VIRTUAL_REPOSITORY:
		out.EnablementState = arpb.Repository_VulnerabilityScanningConfig_SCANNING_UNSUPPORTED
		out.EnablementStateReason = "Vulnerability scanning is not supported for this repository's format or mode."
	case in.GetEnablementConfig() == arpb.Repository_VulnerabilityScanningConfig_DISABLED:
		out.EnablementState = arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED
		out.EnablementStateReason = "Repository has vulnerability scanning disabled."
	case !apiEnabled(project, ContainerScanningAPI):
		out.EnablementState = arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED
		out.EnablementStateReason = "API " + ContainerScanningAPI + " is not enabled."
	default:
		out.EnablementState = arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE
		out.EnablementStateReason = "Repository has vulnerability scanning enabled."
	}
	return out
}

func apiEnabled(project, api string) bool {
	scanState.mu.Lock()
	f := scanState.enabled
	scanState.mu.Unlock()
	return f != nil && f(project, api)
}

// withScanning is a repository as a read answers it.
func withScanning(r *arpb.Repository) *arpb.Repository {
	r.VulnerabilityScanningConfig = scanningConfig(r)
	return r
}

// setScanningConfig stores what a create or update gave: only
// enablement_config is the caller's; last_enable_time is when it was last
// set to anything but DISABLED.
func setScanningConfig(r *arpb.Repository, in *arpb.Repository_VulnerabilityScanningConfig, now time.Time) {
	prev := r.GetVulnerabilityScanningConfig()
	cfg := &arpb.Repository_VulnerabilityScanningConfig{EnablementConfig: in.GetEnablementConfig(), LastEnableTime: prev.GetLastEnableTime()}
	if cfg.EnablementConfig != arpb.Repository_VulnerabilityScanningConfig_DISABLED &&
		(prev == nil || prev.GetEnablementConfig() == arpb.Repository_VulnerabilityScanningConfig_DISABLED || prev.GetLastEnableTime() == nil) {
		cfg.LastEnableTime = timestamppb.New(now)
	}
	r.VulnerabilityScanningConfig = cfg
}

// ImageResourceURI is an image version's resource URI in Container
// Analysis: https://{location}-docker.pkg.dev/{project}/{repository}/{image}@{digest}.
func ImageResourceURI(r registry.Repo, image, digest string) string {
	return "https://" + registry.ImageURL(r, image) + "@" + digest
}

func repositoryName(r registry.Repo) string {
	return "projects/" + r.Project + "/locations/" + r.Location + "/repositories/" + r.Repository
}

// scanActive reports whether pushes to r are scanned.
func scanActive(r registry.Repo) bool {
	repositories.mu.Lock()
	repo, ok := repositories.repos[repositoryName(r)]
	repositories.mu.Unlock()
	if !ok {
		return false
	}
	return scanningConfig(repo).GetEnablementState() == arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE
}

func imagePushed(r registry.Repo, image, digest, mediaType string) {
	if registry.IsIndex(mediaType) || !scanActive(r) {
		return
	}
	uri := ImageResourceURI(r, image, digest)
	scanState.mu.Lock()
	if scanState.store == nil {
		scanState.mu.Unlock()
		return
	}
	img, seen := scanState.images[uri]
	if !seen {
		img = &scannedImage{Repository: repositoryName(r), Image: image, Digest: digest}
		scanState.images[uri] = img
	}
	img.LastUse = scanState.now()
	saveScannedLocked()
	scanState.mu.Unlock()
	// Once per digest: a second push of the same manifest (a new tag)
	// is not scanned again.
	if !seen {
		startScan(r, image, digest)
	}
}

func imagePulled(r registry.Repo, image, digest string) {
	uri := ImageResourceURI(r, image, digest)
	scanState.mu.Lock()
	img, ok := scanState.images[uri]
	if !ok {
		scanState.mu.Unlock()
		return
	}
	stale := scanState.now().Sub(img.LastUse) > staleAfter
	img.LastUse = scanState.now()
	saveScannedLocked()
	scanState.mu.Unlock()
	// Pulling an image with stale or archived metadata rescans it.
	if stale && scanActive(r) {
		startScan(r, image, digest)
	}
}

const (
	staleAfter   = 30 * 24 * time.Hour
	archiveAfter = 90 * 24 * time.Hour
)

func startScan(r registry.Repo, image, digest string) {
	uri := ImageResourceURI(r, image, digest)
	parent := "projects/" + r.Project
	st := scanState.store
	st.PutNote(&gpb.Note{Name: osvscan.DiscoveryNote, ShortDescription: "Package vulnerability scanning",
		LongDescription: "Artifact Analysis's container scanning of OS and language packages.",
		Type:            &gpb.Note_Discovery{Discovery: &gpb.DiscoveryNote{AnalysisKind: gpb.NoteKind_VULNERABILITY}}})
	st.ReplaceOccurrences(parent, uri, isDiscovery, []*gpb.Occurrence{discovery(gpb.DiscoveryOccurrence_PENDING, nil, "", nil)})
	scanState.wg.Add(1)
	go func() {
		defer scanState.wg.Done()
		scanState.sem <- struct{}{}
		defer func() { <-scanState.sem }()
		scan(r, image, digest)
	}()
}

// WaitScans waits for the scans started so far, for tests.
func WaitScans() { scanState.wg.Wait() }

func isDiscovery(o *gpb.Occurrence) bool { return o.GetKind() == gpb.NoteKind_DISCOVERY }

func discovery(s gpb.DiscoveryOccurrence_AnalysisStatus, completed []string, cpe string, failure error) *gpb.Occurrence {
	d := &gpb.DiscoveryOccurrence{AnalysisStatus: s, ContinuousAnalysis: gpb.DiscoveryOccurrence_ACTIVE, Cpe: cpe}
	if s == gpb.DiscoveryOccurrence_FINISHED_SUCCESS || s == gpb.DiscoveryOccurrence_FINISHED_UNSUPPORTED || s == gpb.DiscoveryOccurrence_FINISHED_FAILED {
		d.LastScanTime = timestamppb.New(scanState.now())
	}
	if len(completed) > 0 {
		d.AnalysisCompleted = &gpb.DiscoveryOccurrence_AnalysisCompleted{AnalysisType: completed}
	}
	if failure != nil {
		code := codes.Internal
		if errors.Is(failure, osvscan.ErrNotPrefetched) {
			code = codes.FailedPrecondition
		}
		if s == gpb.DiscoveryOccurrence_FINISHED_UNSUPPORTED {
			code = codes.Unimplemented
		}
		d.AnalysisStatusError = &spb.Status{Code: int32(code), Message: failure.Error()}
		d.AnalysisError = []*spb.Status{d.AnalysisStatusError}
	}
	return &gpb.Occurrence{NoteName: osvscan.DiscoveryNote, Details: &gpb.Occurrence_Discovery{Discovery: d}}
}

func scan(r registry.Repo, image, digest string) {
	uri := ImageResourceURI(r, image, digest)
	parent := "projects/" + r.Project
	st := scanState.store
	st.ReplaceOccurrences(parent, uri, isDiscovery, []*gpb.Occurrence{discovery(gpb.DiscoveryOccurrence_SCANNING, nil, "", nil)})
	res, err := scanImage(r, image, digest)
	all := func(*gpb.Occurrence) bool { return true }
	switch {
	case errors.Is(err, osvscan.ErrNoPackages):
		st.ReplaceOccurrences(parent, uri, all, []*gpb.Occurrence{discovery(gpb.DiscoveryOccurrence_FINISHED_UNSUPPORTED, nil, "",
			errors.New("the image's operating system and packages are not supported for scanning"))})
		return
	case err != nil:
		slog.Warn("Artifact Analysis could not scan an image", "image", uri, "err", err)
		st.ReplaceOccurrences(parent, uri, isDiscovery, []*gpb.Occurrence{discovery(gpb.DiscoveryOccurrence_FINISHED_FAILED, nil, "", err)})
		return
	}
	f := osvscan.Convert(res)
	for _, n := range f.Notes {
		st.PutNote(n)
	}
	occs := []*gpb.Occurrence{discovery(gpb.DiscoveryOccurrence_FINISHED_SUCCESS, f.AnalysisTypes, f.CPE, nil)}
	occs = append(occs, f.Packages...)
	occs = append(occs, f.Vulnerabilities...)
	st.ReplaceOccurrences(parent, uri, all, occs)
	scanState.mu.Lock()
	if img := scanState.images[uri]; img != nil {
		img.Snapshot = osvscan.SnapshotDate
		saveScannedLocked()
	}
	scanState.mu.Unlock()
}

// scanImage writes the image as a `docker save` archive and scans it.
func scanImage(r registry.Repo, image, digest string) (*osvscan.Result, error) {
	rs := RegistryStore()
	if rs == nil {
		return nil, errors.New("no registry store")
	}
	scanState.mu.Lock()
	dir := scanState.dir
	scanState.mu.Unlock()
	if err := osvscan.Ready(dir); err != nil {
		return nil, err
	}
	work := filepath.Join(string(dir), "osv-scanner", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	fh, err := os.CreateTemp(work, "image-*.tar")
	if err != nil {
		return nil, err
	}
	defer os.Remove(fh.Name())
	err = writeImageArchive(fh, rs, r, image, digest)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("export the image for scanning: %w", err)
	}
	return osvscan.Scan(context.Background(), dir, fh.Name())
}

// writeImageArchive writes an image manifest's config and layers in the
// `docker save` layout (manifest.json naming the config and the layers in
// order), which OSV-Scanner's --archive reads.
func writeImageArchive(w *os.File, rs *registry.Store, r registry.Repo, image, digest string) error {
	b, _, mt, err := rs.GetManifest(r, image, digest)
	if err != nil {
		return err
	}
	m, _, err := registry.ParseManifest(mt, b)
	if err != nil {
		return err
	}
	if m.Config == nil {
		return errors.New("the manifest names no config")
	}
	tw := tar.NewWriter(w)
	add := func(name, d string) error {
		blob, err := rs.ReadRepoBlob(r, d)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(blob)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(blob)
		return err
	}
	hex := func(d string) string { return strings.TrimPrefix(d, "sha256:") }
	cfgName := hex(m.Config.Digest) + ".json"
	if err := add(cfgName, m.Config.Digest); err != nil {
		return err
	}
	var layers []string
	for _, l := range m.Layers {
		name := hex(l.Digest) + "/layer.tar"
		if err := add(name, l.Digest); err != nil {
			return err
		}
		layers = append(layers, name)
	}
	man, _ := json.Marshal([]map[string]any{{"Config": cfgName, "RepoTags": []string{registry.ImageURL(r, image) + ":scan"}, "Layers": layers}})
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(man); err != nil {
		return err
	}
	return tw.Close()
}

// ContinuousAnalysis runs the continuous analysis once: an active image
// last scanned with another snapshot is scanned again; one unused for 30
// days goes INACTIVE, and after 90 days is archived.
func ContinuousAnalysis() {
	scanState.mu.Lock()
	st := scanState.store
	if st == nil {
		scanState.mu.Unlock()
		return
	}
	now := scanState.now()
	type job struct {
		uri string
		img scannedImage
	}
	var rescan, stale []job
	for uri, img := range scanState.images {
		age := now.Sub(img.LastUse)
		switch {
		case age > staleAfter:
			stale = append(stale, job{uri, *img})
		case img.Snapshot != "" && img.Snapshot != osvscan.SnapshotDate:
			rescan = append(rescan, job{uri, *img})
		}
	}
	scanState.mu.Unlock()
	for _, j := range stale {
		archive := now.Sub(j.img.LastUse) > archiveAfter
		for _, o := range st.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == j.uri && isDiscovery(o) }) {
			st.UpdateOccurrenceFunc(o.GetName(), func(o *gpb.Occurrence) {
				d := o.GetDiscovery()
				d.ContinuousAnalysis = gpb.DiscoveryOccurrence_INACTIVE
				if archive && d.ArchiveTime == nil {
					d.ArchiveTime = timestamppb.New(j.img.LastUse.Add(archiveAfter))
				}
			})
		}
	}
	for _, j := range rescan {
		p := strings.Split(j.img.Repository, "/")
		if len(p) != 6 {
			continue
		}
		r := registry.Repo{Project: p[1], Location: p[3], Repository: p[5]}
		if scanActive(r) {
			startScan(r, j.img.Image, j.img.Digest)
		}
	}
}

// RunContinuousAnalysis runs ContinuousAnalysis now and then every
// interval until ctx ends.
func RunContinuousAnalysis(ctx context.Context, every time.Duration) {
	ContinuousAnalysis()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ContinuousAnalysis()
		}
	}
}

// ImageOccurrences are the Container Analysis occurrences of an image
// version, for the console's Vulnerabilities tab; nil without scanning.
func ImageOccurrences(r registry.Repo, image, digest string) []*gpb.Occurrence {
	scanState.mu.Lock()
	st := scanState.store
	scanState.mu.Unlock()
	if st == nil {
		return nil
	}
	uri := ImageResourceURI(r, image, digest)
	return st.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == uri })
}

// RepositoryScanning is a repository's vulnerability_scanning_config as a
// read answers it, for the console.
func RepositoryScanning(r registry.Repo) *arpb.Repository_VulnerabilityScanningConfig {
	repositories.mu.Lock()
	repo, ok := repositories.repos[repositoryName(r)]
	repositories.mu.Unlock()
	if !ok {
		return nil
	}
	return scanningConfig(repo)
}

// ResetContainerAnalysisProject forgets a project's Container Analysis
// notes, occurrences and policies and its scanned images, as Google drops
// them with a deleted project (#1540).
func ResetContainerAnalysisProject(project string) {
	scanState.mu.Lock()
	st := scanState.store
	pre := "https://"
	for uri := range scanState.images {
		// https://{location}-docker.pkg.dev/{project}/...
		rest := strings.TrimPrefix(uri, pre)
		if i := strings.IndexByte(rest, '/'); i >= 0 && strings.HasPrefix(rest[i+1:], project+"/") {
			delete(scanState.images, uri)
		}
	}
	saveScannedLocked()
	scanState.mu.Unlock()
	if st != nil {
		st.ResetProject(project)
	}
}
