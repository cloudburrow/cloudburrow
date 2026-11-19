//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	containeranalysis "cloud.google.com/go/containeranalysis/apiv1"
	grafeas "cloud.google.com/go/grafeas/apiv1"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
	capb "google.golang.org/genproto/googleapis/devtools/containeranalysis/v1"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
	iampb "google.golang.org/genproto/googleapis/iam/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// vulnerableImage is an Alpine 3.13 image whose apk database lists zlib
// 1.2.11-r3, which OSV's Alpine records say CVE-2022-37434 (fixed in
// 1.2.12-r2, CVSS 3.1 base 9.8) and CVE-2018-25032 affect. It is built
// here, layer by layer, so nothing is pulled.
func vulnerableImage(t *testing.T, label string) v1.Image {
	t.Helper()
	img, err := crane.Image(map[string][]byte{
		"etc/os-release":       []byte("NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.13.5\nPRETTY_NAME=\"Alpine Linux v3.13\"\n"),
		"etc/alpine-release":   []byte("3.13.5\n"),
		"etc/apk/arch":         []byte("x86_64\n"),
		"etc/apk/repositories": []byte("https://dl-cdn.alpinelinux.org/alpine/v3.13/main\n"),
		"lib/apk/db/installed": []byte("C:Q1\nP:zlib\nV:1.2.11-r3\nA:x86_64\nS:1\nI:1\nT:zlib\nU:https://zlib.net/\nL:Zlib\no:zlib\nm:x\nt:1\n\n"),
		"label":                []byte(label),
	})
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	cf.History = []v1.History{{CreatedBy: "compat test layer"}}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// scanningAPI turns the project's Container Scanning API on, and off
// again when the test ends.
func scanningAPI(t *testing.T, h *Harness, project string) {
	t.Helper()
	addr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SERVICEUSAGE", EnvServiceUsage)
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := su.NewService(h.Context(), option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	name := "projects/" + project + "/services/containerscanning.googleapis.com"
	if got, err := c.Services.Get(name).Do(); err != nil || got.State != "DISABLED" {
		t.Fatalf("the Container Scanning API before it is enabled: %v, %v; want DISABLED, as on Google", got, err)
	}
	if _, err := c.Services.Enable(name, &su.EnableServiceRequest{}).Do(); err != nil {
		t.Fatalf("enable containerscanning.googleapis.com: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Services.Disable(name, &su.DisableServiceRequest{}).Do() })
}

// waitDiscovery waits for the image's DISCOVERY occurrence to finish, and
// skips when the instance has no prefetched scanner.
func waitDiscovery(t *testing.T, ctx context.Context, g *grafeas.Client, project, uri string) *gpb.Occurrence {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		it := g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + project,
			Filter: `kind="DISCOVERY" AND resourceUrl="` + uri + `"`})
		o, err := it.Next()
		if err == nil {
			d := o.GetDiscovery()
			switch d.GetAnalysisStatus() {
			case gpb.DiscoveryOccurrence_FINISHED_SUCCESS:
				return o
			case gpb.DiscoveryOccurrence_FINISHED_FAILED:
				if codes.Code(d.GetAnalysisStatusError().GetCode()) == codes.FailedPrecondition {
					t.Skipf("the instance's scanner is not prefetched (cloudburrow prefetch --vulnerability-scanning): %s", d.GetAnalysisStatusError().GetMessage())
				}
				t.Fatalf("the scan failed: %v", d.GetAnalysisStatusError())
			case gpb.DiscoveryOccurrence_FINISHED_UNSUPPORTED:
				t.Fatalf("the scan found the image unsupported: %v", d.GetAnalysisStatusError())
			}
		} else if err != iterator.Done {
			t.Fatalf("ListOccurrences: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no finished DISCOVERY occurrence for %s: %v", uri, o)
		}
		time.Sleep(time.Second)
	}
}

func allOccurrences(t *testing.T, it *grafeas.OccurrenceIterator) []*gpb.Occurrence {
	t.Helper()
	var out []*gpb.Occurrence
	for {
		o, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
}

// covers: grafeas.v1.Grafeas/ListOccurrences, grafeas.v1.Grafeas/GetOccurrence, grafeas.v1.Grafeas/GetOccurrenceNote, grafeas.v1.Grafeas/GetNote, grafeas.v1.Grafeas/ListNoteOccurrences, google.devtools.containeranalysis.v1.ContainerAnalysis/GetVulnerabilityOccurrencesSummary, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository
//
// TestArtifactRegistryVulnerabilityScanning (#1912): Artifact Registry's
// automatic scanning, as Artifact Analysis documents it. With the
// project's Container Scanning API off (its default) a DOCKER repository's
// vulnerability_scanning_config is SCANNING_DISABLED and a push is not
// scanned; enabled, the repository is SCANNING_ACTIVE and a pushed image
// gets a DISCOVERY occurrence (FINISHED_SUCCESS, continuous analysis
// ACTIVE, the OS CPE), PACKAGE occurrences and VULNERABILITY occurrences
// in Google's shape, read with the official containeranalysis and grafeas
// clients: note projects/goog-vulnz/notes/CVE-2022-37434, severity
// CRITICAL, CVSS 9.8 with its v3 vector's metrics, the affected package and
// version, and the fix (1.2.12-r2). The summary counts them by severity
// with what is fixable. A repository with scanning DISABLED is not scanned.
func TestArtifactRegistryVulnerabilityScanning(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	scanningAPI(t, h, h.Project())
	repo := formatRepo(t, ctx, ar, parent, "scan-docker", &arpb.Repository{Format: arpb.Repository_DOCKER})
	if s := repo.GetVulnerabilityScanningConfig().GetEnablementState(); s != arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE {
		t.Fatalf("enablement_state %v (%q); want SCANNING_ACTIVE with the API enabled", s, repo.GetVulnerabilityScanningConfig().GetEnablementStateReason())
	}
	off := formatRepo(t, ctx, ar, parent, "scan-off", &arpb.Repository{Format: arpb.Repository_DOCKER,
		VulnerabilityScanningConfig: &arpb.Repository_VulnerabilityScanningConfig{EnablementConfig: arpb.Repository_VulnerabilityScanningConfig_DISABLED}})
	if s := off.GetVulnerabilityScanningConfig().GetEnablementState(); s != arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED {
		t.Errorf("a DISABLED repository's enablement_state is %v", s)
	}
	py := formatRepo(t, ctx, ar, parent, "scan-python", &arpb.Repository{Format: arpb.Repository_PYTHON})
	if s := py.GetVulnerabilityScanningConfig().GetEnablementState(); s != arpb.Repository_VulnerabilityScanningConfig_SCANNING_UNSUPPORTED {
		t.Errorf("a PYTHON repository's enablement_state is %v", s)
	}

	ca, err := containeranalysis.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })
	g := ca.GetGrafeasClient()

	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	push := func(repoID, label string) string {
		img := vulnerableImage(t, label)
		ref := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/" + repoID + "/zlib-app:v1"
		if err := crane.Push(img, ref, registryOptions(h)...); err != nil {
			t.Fatalf("crane push %s: %v", ref, err)
		}
		d, _ := img.Digest()
		return "https://us-central1-docker.pkg.dev/" + h.Project() + "/" + repoID + "/zlib-app@" + d.String()
	}
	offURI := push("scan-off", "off")
	uri := push("scan-docker", "on")

	disc := waitDiscovery(t, ctx, g, h.Project(), uri)
	if disc.GetNoteName() != "projects/goog-analysis/notes/PACKAGE_VULNERABILITY" ||
		disc.GetDiscovery().GetContinuousAnalysis() != gpb.DiscoveryOccurrence_ACTIVE ||
		disc.GetDiscovery().GetCpe() != "cpe:/o:alpine:alpine_linux:3.13" || disc.GetDiscovery().GetLastScanTime() == nil {
		t.Errorf("DISCOVERY occurrence: %v", disc)
	}
	if got, err := g.GetOccurrence(ctx, &gpb.GetOccurrenceRequest{Name: disc.GetName()}); err != nil || got.GetResourceUri() != uri {
		t.Errorf("GetOccurrence %s: %v, %v", disc.GetName(), got, err)
	}
	if n, err := g.GetOccurrenceNote(ctx, &gpb.GetOccurrenceNoteRequest{Name: disc.GetName()}); err != nil || n.GetKind() != gpb.NoteKind_DISCOVERY {
		t.Errorf("GetOccurrenceNote: %v, %v", n, err)
	}

	vulns := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `kind="VULNERABILITY" AND resourceUrl="` + uri + `"`}))
	var cve *gpb.Occurrence
	for _, o := range vulns {
		if o.GetNoteName() == "projects/goog-vulnz/notes/CVE-2022-37434" {
			cve = o
		}
	}
	if cve == nil {
		t.Fatalf("no VULNERABILITY occurrence of CVE-2022-37434 among %d", len(vulns))
	}
	v := cve.GetVulnerability()
	pi := v.GetPackageIssue()
	if v.GetSeverity() != gpb.Severity_CRITICAL || v.GetCvssScore() < 9.7 || v.GetCvssScore() > 9.9 || !v.GetFixAvailable() ||
		v.GetCvssVersion() != gpb.CVSSVersion_CVSS_VERSION_3 || v.GetCvssv3().GetAttackVector() != gpb.CVSS_ATTACK_VECTOR_NETWORK ||
		len(pi) != 1 || pi[0].GetAffectedPackage() != "zlib" || pi[0].GetAffectedVersion().GetFullName() != "1.2.11-r3" ||
		pi[0].GetAffectedVersion().GetName() != "1.2.11" || pi[0].GetAffectedVersion().GetRevision() != "r3" ||
		pi[0].GetFixedVersion().GetFullName() != "1.2.12-r2" || pi[0].GetPackageType() != "OS" ||
		pi[0].GetAffectedCpeUri() != "cpe:/o:alpine:alpine_linux:3.13" {
		t.Errorf("CVE-2022-37434's occurrence: %v", cve)
	}
	pkgs := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `kind="PACKAGE" AND resourceUrl="` + uri + `"`}))
	if len(pkgs) != 1 || pkgs[0].GetPackage().GetName() != "zlib" || pkgs[0].GetPackage().GetVersion().GetFullName() != "1.2.11-r3" {
		t.Errorf("PACKAGE occurrences: %v", pkgs)
	}
	if by := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `dependencyPackageName="zlib" AND kind="PACKAGE" AND has_prefix(resourceUrl, "https://us-central1-docker.pkg.dev/` + h.Project() + `/scan-docker/")`})); len(by) != 1 {
		t.Errorf("dependencyPackageName filter: %d occurrences", len(by))
	}

	note, err := g.GetNote(ctx, &gpb.GetNoteRequest{Name: "projects/goog-vulnz/notes/CVE-2022-37434"})
	if err != nil || note.GetKind() != gpb.NoteKind_VULNERABILITY || note.GetVulnerability().GetSeverity() != gpb.Severity_CRITICAL {
		t.Errorf("GetNote CVE-2022-37434: %v, %v", note, err)
	}
	affected := allOccurrences(t, g.ListNoteOccurrences(ctx, &gpb.ListNoteOccurrencesRequest{Name: "projects/goog-vulnz/notes/CVE-2022-37434"}))
	found := false
	for _, o := range affected {
		found = found || o.GetResourceUri() == uri
	}
	if !found {
		t.Errorf("ListNoteOccurrences of CVE-2022-37434 does not name %s", uri)
	}
	if byNote := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `noteProjectId="goog-vulnz" AND noteId="CVE-2022-37434" AND resourceUrl="` + uri + `"`})); len(byNote) != 1 {
		t.Errorf("noteProjectId/noteId filter: %d occurrences", len(byNote))
	}

	sum, err := ca.GetVulnerabilityOccurrencesSummary(ctx, &capb.GetVulnerabilityOccurrencesSummaryRequest{
		Parent: "projects/" + h.Project(), Filter: `resourceUrl="` + uri + `"`})
	if err != nil {
		t.Fatal(err)
	}
	var total, fixable int64
	for _, c := range sum.GetCounts() {
		if c.GetResourceUri() != uri {
			t.Errorf("summary names %s", c.GetResourceUri())
		}
		total += c.GetTotalCount()
		fixable += c.GetFixableCount()
	}
	if total != int64(len(vulns)) || fixable == 0 {
		t.Errorf("summary %v: total %d, want %d, and some fixable", sum, total, len(vulns))
	}

	if none := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `resourceUrl="` + offURI + `"`})); len(none) != 0 {
		t.Errorf("the repository with scanning DISABLED has %d occurrences", len(none))
	}

	// Turning scanning off on the repository shows as SCANNING_DISABLED.
	upd, err := ar.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: repo.GetName(),
		VulnerabilityScanningConfig: &arpb.Repository_VulnerabilityScanningConfig{EnablementConfig: arpb.Repository_VulnerabilityScanningConfig_DISABLED}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"vulnerability_scanning_config"}}})
	if err != nil || upd.GetVulnerabilityScanningConfig().GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED {
		t.Errorf("UpdateRepository vulnerability_scanning_config DISABLED: %v, %v", upd.GetVulnerabilityScanningConfig(), err)
	}
}

// covers: grafeas.v1.Grafeas/CreateNote, grafeas.v1.Grafeas/BatchCreateNotes, grafeas.v1.Grafeas/ListNotes, grafeas.v1.Grafeas/UpdateNote, grafeas.v1.Grafeas/DeleteNote, grafeas.v1.Grafeas/CreateOccurrence, grafeas.v1.Grafeas/BatchCreateOccurrences, grafeas.v1.Grafeas/UpdateOccurrence, grafeas.v1.Grafeas/DeleteOccurrence, google.devtools.containeranalysis.v1.ContainerAnalysis/SetIamPolicy, google.devtools.containeranalysis.v1.ContainerAnalysis/GetIamPolicy, google.devtools.containeranalysis.v1.ContainerAnalysis/TestIamPermissions
//
// TestContainerAnalysisNotesAndOccurrences (#1912): a project's own notes
// and occurrences, as "Create custom notes and occurrences" documents
// them, with the official grafeas and containeranalysis clients: a note is
// created, listed, updated and deleted; an existing note ID is
// ALREADY_EXISTS; an occurrence must name an existing note of its kind;
// occurrences are created one at a time and in batch, filtered, updated
// and deleted; a note's IAM policy is set, read and tested.
func TestContainerAnalysisNotesAndOccurrences(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	ca, err := containeranalysis.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })
	g := ca.GetGrafeasClient()
	parent := "projects/" + h.Project()
	id := "compat-attestor-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	note := &gpb.Note{ShortDescription: "a compat note", Type: &gpb.Note_Attestation{Attestation: &gpb.AttestationNote{
		Hint: &gpb.AttestationNote_Hint{HumanReadableName: "compat"}}}}
	n, err := g.CreateNote(ctx, &gpb.CreateNoteRequest{Parent: parent, NoteId: id, Note: note})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	t.Cleanup(func() { _ = g.DeleteNote(context.Background(), &gpb.DeleteNoteRequest{Name: n.GetName()}) })
	if n.GetName() != parent+"/notes/"+id || n.GetKind() != gpb.NoteKind_ATTESTATION || n.GetCreateTime() == nil {
		t.Errorf("CreateNote: %v", n)
	}
	if _, err := g.CreateNote(ctx, &gpb.CreateNoteRequest{Parent: parent, NoteId: id, Note: note}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateNote: %v; want ALREADY_EXISTS", err)
	}
	if _, err := g.CreateNote(ctx, &gpb.CreateNoteRequest{Parent: parent, NoteId: id + "-x", Note: &gpb.Note{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a note with no type: %v; want INVALID_ARGUMENT", err)
	}
	batch, err := g.BatchCreateNotes(ctx, &gpb.BatchCreateNotesRequest{Parent: parent, Notes: map[string]*gpb.Note{
		id + "-b1": {Type: &gpb.Note_Build{Build: &gpb.BuildNote{BuilderVersion: "1"}}},
		id + "-b2": {Type: &gpb.Note_Build{Build: &gpb.BuildNote{BuilderVersion: "2"}}},
	}})
	if err != nil || len(batch.GetNotes()) != 2 {
		t.Fatalf("BatchCreateNotes: %v, %v", batch, err)
	}
	for _, b := range batch.GetNotes() {
		name := b.GetName()
		t.Cleanup(func() { _ = g.DeleteNote(context.Background(), &gpb.DeleteNoteRequest{Name: name}) })
	}
	listed := 0
	it := g.ListNotes(ctx, &gpb.ListNotesRequest{Parent: parent, Filter: `kind="BUILD"`, PageSize: 1})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(x.GetName(), parent+"/notes/"+id) {
			listed++
		}
	}
	if listed != 2 {
		t.Errorf("ListNotes kind=BUILD, a page at a time, found %d of the batch", listed)
	}
	if u, err := g.UpdateNote(ctx, &gpb.UpdateNoteRequest{Name: n.GetName(), Note: &gpb.Note{ShortDescription: "updated"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"short_description"}}}); err != nil || u.GetShortDescription() != "updated" || u.GetAttestation() == nil {
		t.Errorf("UpdateNote: %v, %v", u, err)
	}

	att := &gpb.Occurrence{ResourceUri: "https://us-central1-docker.pkg.dev/" + h.Project() + "/r/img@sha256:" + strings.Repeat("a", 64), NoteName: n.GetName(),
		Details: &gpb.Occurrence_Attestation{Attestation: &gpb.AttestationOccurrence{SerializedPayload: []byte("payload")}}}
	o, err := g.CreateOccurrence(ctx, &gpb.CreateOccurrenceRequest{Parent: parent, Occurrence: att})
	if err != nil || o.GetKind() != gpb.NoteKind_ATTESTATION || !strings.HasPrefix(o.GetName(), parent+"/occurrences/") {
		t.Fatalf("CreateOccurrence: %v, %v", o, err)
	}
	if _, err := g.CreateOccurrence(ctx, &gpb.CreateOccurrenceRequest{Parent: parent, Occurrence: &gpb.Occurrence{ResourceUri: att.ResourceUri,
		NoteName: parent + "/notes/missing-" + id, Details: att.Details}}); status.Code(err) != codes.NotFound {
		t.Errorf("an occurrence of a missing note: %v; want NOT_FOUND", err)
	}
	if _, err := g.CreateOccurrence(ctx, &gpb.CreateOccurrenceRequest{Parent: parent, Occurrence: &gpb.Occurrence{ResourceUri: att.ResourceUri,
		NoteName: batch.GetNotes()[0].GetName(), Details: att.Details}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an ATTESTATION occurrence of a BUILD note: %v; want INVALID_ARGUMENT", err)
	}
	bo, err := g.BatchCreateOccurrences(ctx, &gpb.BatchCreateOccurrencesRequest{Parent: parent, Occurrences: []*gpb.Occurrence{att, att}})
	if err != nil || len(bo.GetOccurrences()) != 2 {
		t.Fatalf("BatchCreateOccurrences: %v, %v", bo, err)
	}
	if got := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: parent,
		Filter: `kind="ATTESTATION" AND noteId="` + id + `"`})); len(got) != 3 {
		t.Errorf("ListOccurrences of the note: %d, want 3", len(got))
	}
	if u, err := g.UpdateOccurrence(ctx, &gpb.UpdateOccurrenceRequest{Name: o.GetName(), Occurrence: &gpb.Occurrence{Remediation: "rebuild"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"remediation"}}}); err != nil || u.GetRemediation() != "rebuild" || u.GetAttestation() == nil {
		t.Errorf("UpdateOccurrence: %v, %v", u, err)
	}
	for _, x := range append(bo.GetOccurrences(), o) {
		if err := g.DeleteOccurrence(ctx, &gpb.DeleteOccurrenceRequest{Name: x.GetName()}); err != nil {
			t.Errorf("DeleteOccurrence: %v", err)
		}
	}
	if _, err := g.GetOccurrence(ctx, &gpb.GetOccurrenceRequest{Name: o.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted occurrence: %v; want NOT_FOUND", err)
	}

	pol, err := ca.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: n.GetName(), Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/containeranalysis.notes.occurrences.viewer", Members: []string{"user:dev@example.com"}}}}})
	if err != nil || len(pol.GetEtag()) == 0 {
		t.Fatalf("SetIamPolicy: %v, %v", pol, err)
	}
	if got, err := ca.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: n.GetName()}); err != nil || len(got.GetBindings()) != 1 {
		t.Errorf("GetIamPolicy: %v, %v", got, err)
	}
	if tp, err := ca.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: n.GetName(),
		Permissions: []string{"containeranalysis.notes.get"}}); err != nil || len(tp.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions: %v, %v", tp, err)
	}
	if err := g.DeleteNote(ctx, &gpb.DeleteNoteRequest{Name: n.GetName()}); err != nil {
		t.Errorf("DeleteNote: %v", err)
	}
	if _, err := g.GetNote(ctx, &gpb.GetNoteRequest{Name: n.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted note: %v; want NOT_FOUND", err)
	}
}

// covers: grafeas.v1.Grafeas/ListOccurrences, google.devtools.containeranalysis.v1.ContainerAnalysis/GetVulnerabilityOccurrencesSummary
//
// TestGcloudArtifactsVulnerabilityScanning (#1912): `gcloud artifacts
// docker images list --show-occurrences` and `gcloud artifacts docker
// images describe --show-package-vulnerability`, through the
// configuration `cloudburrow gcloud-setup` writes (its containeranalysis
// override), egress blocked, show a scanned image's discovery and
// vulnerabilities.
func TestGcloudArtifactsVulnerabilityScanning(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SERVICEUSAGE", EnvServiceUsage)
	ar, ctx := arClient(t, h)
	scanningAPI(t, h, h.Project())
	formatRepo(t, ctx, ar, runParent(h), "scan-gcloud", &arpb.Repository{Format: arpb.Repository_DOCKER})
	ca, err := containeranalysis.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	img := vulnerableImage(t, "gcloud")
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+h.Project()+"/scan-gcloud/zlib-app:v1", registryOptions(h)...); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	pkg := "us-central1-docker.pkg.dev/" + h.Project() + "/scan-gcloud/zlib-app"
	waitDiscovery(t, ctx, ca.GetGrafeasClient(), h.Project(), "https://"+pkg+"@"+d.String())

	g := newGcloudSession(t, h)
	out := g.must("artifacts", "docker", "images", "list", "us-central1-docker.pkg.dev/"+h.Project()+"/scan-gcloud", "--show-occurrences", "--format=json")
	// gcloud's notes ("Listing items under ...") come before the JSON.
	if i := strings.Index(out, "\n["); i >= 0 {
		out = out[i+1:]
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 1 {
		t.Fatalf("images list --show-occurrences: %v\n%s", err, out)
	}
	if list[0]["package"] != pkg || list[0]["version"] != d.String() {
		t.Errorf("images list: %v", list[0])
	}
	raw, _ := json.Marshal(list[0])
	if !strings.Contains(string(raw), "FINISHED_SUCCESS") || !strings.Contains(string(raw), "CRITICAL") {
		t.Errorf("images list --show-occurrences shows no finished discovery or CRITICAL count:\n%s", out)
	}

	out = g.must("artifacts", "docker", "images", "describe", pkg+"@"+d.String(), "--show-package-vulnerability", "--format=json")
	if !strings.Contains(out, "CVE-2022-37434") || !strings.Contains(out, "package_vulnerability_summary") || !strings.Contains(out, "discovery_summary") {
		t.Errorf("images describe --show-package-vulnerability:\n%s", out)
	}
}
