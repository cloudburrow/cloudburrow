package run

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/v1/random"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/grafeas"
	"github.com/cloudburrow/cloudburrow/internal/osvscan"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// A DOCKER repository's vulnerability_scanning_config state follows the
// project's Container Scanning API and its own enablement_config; a push
// to an active repository is scanned once per digest, and without a
// prefetched scanner its DISCOVERY occurrence is FINISHED_FAILED with
// FAILED_PRECONDITION; one to a DISABLED repository is not scanned; a
// continuous analysis turns an image unused for 30 days INACTIVE and
// archives it after 90; a project's reset forgets its scans.
func TestRepositoryVulnerabilityScanning(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	var enabled atomic.Bool
	gs := grafeas.NewStore(nil)
	UseContainerAnalysis(gs, osvscan.Dir(t.TempDir()), func(project, api string) bool {
		return api == ContainerScanningAPI && enabled.Load()
	}, filepath.Join(t.TempDir(), "scanned.json"))
	t.Cleanup(func() {
		WaitScans()
		scanState.mu.Lock()
		scanState.store, scanState.enabled = nil, nil
		scanState.mu.Unlock()
	})
	s := NewServer(&Knative{}, "t", time.Minute)
	a := s.ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/scan-unit/locations/us-central1"
	create := func(id string, r *arpb.Repository) *arpb.Repository {
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
		if _, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: r}); err != nil {
			t.Fatal(err)
		}
		got, err := a.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/" + id})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	on := create("on", &arpb.Repository{Format: arpb.Repository_DOCKER})
	if c := on.GetVulnerabilityScanningConfig(); c.GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED ||
		c.GetEnablementStateReason() != "API containerscanning.googleapis.com is not enabled." {
		t.Errorf("with the API off: %v", c)
	}
	enabled.Store(true)
	off := create("off", &arpb.Repository{Format: arpb.Repository_DOCKER, VulnerabilityScanningConfig: &arpb.Repository_VulnerabilityScanningConfig{
		EnablementConfig: arpb.Repository_VulnerabilityScanningConfig_DISABLED,
		EnablementState:  arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE}})
	if c := off.GetVulnerabilityScanningConfig(); c.GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED || c.GetLastEnableTime() != nil {
		t.Errorf("DISABLED: %v", c)
	}
	inh := create("inherited", &arpb.Repository{Format: arpb.Repository_DOCKER, VulnerabilityScanningConfig: &arpb.Repository_VulnerabilityScanningConfig{
		EnablementConfig: arpb.Repository_VulnerabilityScanningConfig_INHERITED}})
	if c := inh.GetVulnerabilityScanningConfig(); c.GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE || c.GetLastEnableTime() == nil {
		t.Errorf("INHERITED: %v", c)
	}
	if c := create("npm", &arpb.Repository{Format: arpb.Repository_NPM}).GetVulnerabilityScanningConfig(); c.GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_UNSUPPORTED {
		t.Errorf("NPM: %v", c)
	}

	img, _ := random.Image(64, 1)
	d, _ := img.Digest()
	for _, id := range []string{"on", "off"} {
		if _, err := st.WriteImage(registry.Repo{Location: "us-central1", Project: "scan-unit", Repository: id}, "app", "v1", img); err != nil {
			t.Fatal(err)
		}
	}
	WaitScans()
	onURI := "https://us-central1-docker.pkg.dev/scan-unit/on/app@" + d.String()
	disc := gs.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == onURI })
	if len(disc) != 1 || disc[0].GetDiscovery().GetAnalysisStatus() != gpb.DiscoveryOccurrence_FINISHED_FAILED ||
		codes.Code(disc[0].GetDiscovery().GetAnalysisStatusError().GetCode()) != codes.FailedPrecondition {
		t.Fatalf("the scan without a scanner: %v", disc)
	}
	if n := gs.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() != onURI }); len(n) != 0 {
		t.Errorf("the DISABLED repository's push was scanned: %v", n)
	}
	// A second tag of the same digest is not scanned again.
	before := disc[0].GetName()
	if _, err := st.WriteImage(registry.Repo{Location: "us-central1", Project: "scan-unit", Repository: "on"}, "app", "v2", img); err != nil {
		t.Fatal(err)
	}
	WaitScans()
	if again := gs.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == onURI }); len(again) != 1 || again[0].GetName() != before {
		t.Errorf("a second push of the digest rescanned: %v", again)
	}

	scanState.mu.Lock()
	scanState.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	scanState.mu.Unlock()
	t.Cleanup(func() { scanState.mu.Lock(); scanState.now = time.Now; scanState.mu.Unlock() })
	ContinuousAnalysis()
	o := gs.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == onURI })[0]
	if o.GetDiscovery().GetContinuousAnalysis() != gpb.DiscoveryOccurrence_INACTIVE || o.GetDiscovery().GetArchiveTime() != nil {
		t.Errorf("after 31 days: %v", o.GetDiscovery())
	}
	scanState.mu.Lock()
	scanState.now = func() time.Time { return time.Now().Add(91 * 24 * time.Hour) }
	scanState.mu.Unlock()
	ContinuousAnalysis()
	o = gs.Occurrences(func(o *gpb.Occurrence) bool { return o.GetResourceUri() == onURI })[0]
	if o.GetDiscovery().GetArchiveTime() == nil {
		t.Errorf("after 91 days: %v", o.GetDiscovery())
	}
	// A project's reset forgets its occurrences and scanned images.
	ResetContainerAnalysisProject("scan-unit")
	if left := gs.Occurrences(func(*gpb.Occurrence) bool { return true }); len(left) != 0 {
		t.Errorf("after the project's reset: %v", left)
	}
	scanState.mu.Lock()
	n := len(scanState.images)
	scanState.mu.Unlock()
	if n != 0 {
		t.Errorf("%d scanned images kept after the project's reset", n)
	}
}
