//go:build compat

package compat

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// arRepoWithImage makes a DOCKER repository and pushes a random image to
// it as app:v1, and returns the client, the repository and the version.
func arRepoWithImage(ctx context.Context, t *testing.T, h *Harness, repoID string) (*artifactregistry.Client, string, string) {
	t.Helper()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repo := runParent(h) + "/repositories/" + repoID
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repo}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: repoID,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+h.Project()+"/"+repoID+"/app:v1", registryOptions(h)...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	d, _ := img.Digest()
	return ar, repo, repo + "/packages/app/versions/" + d.String()
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/UpdatePackage, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateFile, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteFile, google.devtools.artifactregistry.v1.ArtifactRegistry/BatchDeleteVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRule, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRule, google.devtools.artifactregistry.v1.ArtifactRegistry/ListRules, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRule, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRule, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateAttachment, google.devtools.artifactregistry.v1.ArtifactRegistry/GetAttachment, google.devtools.artifactregistry.v1.ArtifactRegistry/ListAttachments, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteAttachment, google.devtools.artifactregistry.v1.ArtifactRegistry/GetProjectSettings, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateProjectSettings, google.devtools.artifactregistry.v1.ArtifactRegistry/GetVPCSCConfig, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateVPCSCConfig
//
// TestArtifactRegistryConfigAndMetadata (#1449), with the official client
// over gRPC on the Run port, over an image crane pushed:
//   - UpdatePackage sets a display name and annotations, UpdateVersion and
//     UpdateFile annotations, which Get reads back; DeleteFile in a DOCKER
//     repository is FAILED_PRECONDITION, as on Google;
//   - a rule is created, read, listed, updated and deleted;
//   - an attachment on the version, naming one of its files, is created,
//     read, listed by target filter and deleted;
//   - project settings read with gcr.io redirection off, take pull_percent,
//     and refuse turning redirection on, with the reason;
//   - the location's VPC-SC config reads DENY and changes to ALLOW;
//   - BatchDeleteVersions with validate_only deletes nothing, then deletes
//     the tagged version; ListMavenArtifacts is UNIMPLEMENTED with the
//     reason.
func TestArtifactRegistryConfigAndMetadata(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	ar, repo, version := arRepoWithImage(ctx, t, h, "cfg-"+uuid.NewString()[:8])
	pkg := repo + "/packages/app"

	p, err := ar.UpdatePackage(ctx, &arpb.UpdatePackageRequest{Package: &arpb.Package{Name: pkg, DisplayName: "The App",
		Annotations: map[string]string{"team": "core"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "annotations"}}})
	if err != nil || p.GetDisplayName() != "The App" || p.GetAnnotations()["team"] != "core" {
		t.Errorf("UpdatePackage = %v, %v", p, err)
	}
	if got, _ := ar.GetPackage(ctx, &arpb.GetPackageRequest{Name: pkg}); got.GetDisplayName() != "The App" {
		t.Errorf("GetPackage after update = %v", got)
	}
	if _, err := ar.UpdatePackage(ctx, &arpb.UpdatePackageRequest{Package: &arpb.Package{Name: pkg},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"create_time"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdatePackage create_time = %v; want INVALID_ARGUMENT", err)
	}
	v, err := ar.UpdateVersion(ctx, &arpb.UpdateVersionRequest{Version: &arpb.Version{Name: version,
		Annotations: map[string]string{"example.com/approved": "yes"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}}})
	if err != nil || v.GetAnnotations()["example.com/approved"] != "yes" {
		t.Errorf("UpdateVersion = %v, %v", v, err)
	}
	if got, _ := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); got.GetAnnotations()["example.com/approved"] != "yes" {
		t.Errorf("GetVersion annotations = %v", got.GetAnnotations())
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: repo, Filter: `owner="` + version + `"`}).Next)
	if len(files) < 2 {
		t.Fatalf("ListFiles = %v", files)
	}
	file := files[0].GetName()
	f, err := ar.UpdateFile(ctx, &arpb.UpdateFileRequest{File: &arpb.File{Name: file, Annotations: map[string]string{"scan": "clean"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}}})
	if err != nil || f.GetAnnotations()["scan"] != "clean" {
		t.Errorf("UpdateFile = %v, %v", f, err)
	}
	if _, err := ar.DeleteFile(ctx, &arpb.DeleteFileRequest{Name: file}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "generic repositories") {
		t.Errorf("DeleteFile in a DOCKER repository = %v; want FAILED_PRECONDITION", err)
	}

	rule, err := ar.CreateRule(ctx, &arpb.CreateRuleRequest{Parent: repo, RuleId: "deny-old", Rule: &arpb.Rule{
		Action: arpb.Rule_DENY, Operation: arpb.Rule_DOWNLOAD, PackageId: "app",
		Condition: &expr.Expr{Expression: `resource.name.startsWith("old")`}}})
	if err != nil || rule.GetName() != repo+"/rules/deny-old" {
		t.Fatalf("CreateRule = %v, %v", rule, err)
	}
	if _, err := ar.CreateRule(ctx, &arpb.CreateRuleRequest{Parent: repo, RuleId: "deny-old", Rule: rule}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateRule twice = %v", err)
	}
	if got, err := ar.GetRule(ctx, &arpb.GetRuleRequest{Name: rule.GetName()}); err != nil || got.GetCondition().GetExpression() == "" {
		t.Errorf("GetRule = %v, %v", got, err)
	}
	if rules := collectAll(t, ar.ListRules(ctx, &arpb.ListRulesRequest{Parent: repo}).Next); len(rules) != 1 {
		t.Errorf("ListRules = %v", rules)
	}
	if got, err := ar.UpdateRule(ctx, &arpb.UpdateRuleRequest{Rule: &arpb.Rule{Name: rule.GetName(), Action: arpb.Rule_ALLOW},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"action"}}}); err != nil || got.GetAction() != arpb.Rule_ALLOW || got.GetPackageId() != "app" {
		t.Errorf("UpdateRule = %v, %v", got, err)
	}
	if err := ar.DeleteRule(ctx, &arpb.DeleteRuleRequest{Name: rule.GetName()}); err != nil {
		t.Errorf("DeleteRule: %v", err)
	}
	if _, err := ar.GetRule(ctx, &arpb.GetRuleRequest{Name: rule.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRule after delete = %v", err)
	}

	aop, err := ar.CreateAttachment(ctx, &arpb.CreateAttachmentRequest{Parent: repo, AttachmentId: "sbom", Attachment: &arpb.Attachment{
		Target: version, Type: "application/vnd.spdx+json", Files: []string{file}}})
	if err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	at, err := aop.Wait(ctx)
	if err != nil || at.GetName() != repo+"/attachments/sbom" || at.GetCreateTime() == nil {
		t.Fatalf("CreateAttachment.Wait = %v, %v", at, err)
	}
	if _, err := ar.CreateAttachment(ctx, &arpb.CreateAttachmentRequest{Parent: repo, AttachmentId: "bad", Attachment: &arpb.Attachment{
		Target: version, Type: "x", Files: []string{repo + "/files/sha256:" + strings.Repeat("0", 64)}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an attachment naming no file = %v; want INVALID_ARGUMENT", err)
	}
	if got, err := ar.GetAttachment(ctx, &arpb.GetAttachmentRequest{Name: at.GetName()}); err != nil || got.GetTarget() != version {
		t.Errorf("GetAttachment = %v, %v", got, err)
	}
	if l := collectAll(t, ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repo, Filter: `target="` + version + `"`}).Next); len(l) != 1 {
		t.Errorf("ListAttachments by target = %v", l)
	}
	if l := collectAll(t, ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repo, Filter: `type="other"`}).Next); len(l) != 0 {
		t.Errorf("ListAttachments by another type = %v", l)
	}
	dop, err := ar.DeleteAttachment(ctx, &arpb.DeleteAttachmentRequest{Name: at.GetName()})
	if err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteAttachment.Wait: %v", err)
	}

	settings := "projects/" + h.Project() + "/projectSettings"
	if s, err := ar.GetProjectSettings(ctx, &arpb.GetProjectSettingsRequest{Name: settings}); err != nil ||
		s.GetLegacyRedirectionState() != arpb.ProjectSettings_REDIRECTION_FROM_GCR_IO_DISABLED {
		t.Errorf("GetProjectSettings = %v, %v", s, err)
	}
	if s, err := ar.UpdateProjectSettings(ctx, &arpb.UpdateProjectSettingsRequest{ProjectSettings: &arpb.ProjectSettings{Name: settings, PullPercent: 25},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pull_percent"}}}); err != nil || s.GetPullPercent() != 25 {
		t.Errorf("UpdateProjectSettings = %v, %v", s, err)
	}
	if _, err := ar.UpdateProjectSettings(ctx, &arpb.UpdateProjectSettingsRequest{ProjectSettings: &arpb.ProjectSettings{Name: settings,
		LegacyRedirectionState: arpb.ProjectSettings_REDIRECTION_FROM_GCR_IO_ENABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"legacy_redirection_state"}}}); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "gcr.io") {
		t.Errorf("turning gcr.io redirection on = %v; want UNIMPLEMENTED with the reason", err)
	}

	vpcsc := runParent(h) + "/vpcscConfig"
	if c, err := ar.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: vpcsc}); err != nil || c.GetVpcscPolicy() != arpb.VPCSCConfig_DENY {
		t.Errorf("GetVPCSCConfig = %v, %v", c, err)
	}
	if c, err := ar.UpdateVPCSCConfig(ctx, &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{Name: vpcsc, VpcscPolicy: arpb.VPCSCConfig_ALLOW}}); err != nil ||
		c.GetVpcscPolicy() != arpb.VPCSCConfig_ALLOW {
		t.Errorf("UpdateVPCSCConfig = %v, %v", c, err)
	}
	t.Cleanup(func() {
		_, _ = ar.UpdateVPCSCConfig(context.Background(), &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{Name: vpcsc, VpcscPolicy: arpb.VPCSCConfig_DENY}})
	})

	bop, err := ar.BatchDeleteVersions(ctx, &arpb.BatchDeleteVersionsRequest{Parent: pkg, Names: []string{version}, ValidateOnly: true})
	if err != nil {
		t.Fatalf("BatchDeleteVersions(validate_only): %v", err)
	}
	if err := bop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); err != nil {
		t.Errorf("validate_only deleted the version: %v", err)
	}
	bop, err = ar.BatchDeleteVersions(ctx, &arpb.BatchDeleteVersionsRequest{Parent: pkg, Names: []string{version}})
	if err != nil {
		t.Fatalf("BatchDeleteVersions: %v", err)
	}
	if err := bop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); status.Code(err) != codes.NotFound {
		t.Errorf("GetVersion after BatchDeleteVersions = %v; want NotFound", err)
	}
	if _, err := ar.ListMavenArtifacts(ctx, &arpb.ListMavenArtifactsRequest{Parent: repo}).Next(); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "only DOCKER repositories") {
		t.Errorf("ListMavenArtifacts = %v; want UNIMPLEMENTED with the reason", err)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/GetVPCSCConfig, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateVPCSCConfig, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteVersion
//
// TestGcloudArtifactsVPCSCAndVersions (#1449): `gcloud artifacts
// vpcsc-config describe`, `allow` and `deny`, `gcloud artifacts versions
// update --update-annotations` and `gcloud artifacts versions delete`,
// through the configuration `cloudburrow gcloud-setup` writes, egress
// blocked.
func TestGcloudArtifactsVPCSCAndVersions(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	repoID := "gv-" + uuid.NewString()[:8]
	ar, _, version := arRepoWithImage(ctx, t, h, repoID)
	g := newGcloudSession(t, h)
	loc := "--location=us-central1"
	if out := g.must("artifacts", "vpcsc-config", "describe", loc, "--format=value(vpcscPolicy)"); lastLine(out) != "DENY" {
		t.Errorf("vpcsc-config describe = %q", out)
	}
	g.must("artifacts", "vpcsc-config", "allow", loc, "--quiet")
	if c, err := ar.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: runParent(h) + "/vpcscConfig"}); err != nil || c.GetVpcscPolicy() != arpb.VPCSCConfig_ALLOW {
		t.Errorf("after allow, the config = %v, %v", c, err)
	}
	g.must("artifacts", "vpcsc-config", "deny", loc, "--quiet")

	digest := version[strings.LastIndex(version, "/")+1:]
	common := []string{loc, "--repository=" + repoID, "--package=app"}
	g.must(append([]string{"artifacts", "versions", "update", digest, "--update-annotations=reviewed=yes"}, common...)...)
	if v, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); err != nil || v.GetAnnotations()["reviewed"] != "yes" {
		t.Errorf("after versions update, the version = %v, %v", v, err)
	}
	g.must(append([]string{"artifacts", "versions", "delete", digest, "--delete-tags", "--quiet"}, common...)...)
	if _, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); status.Code(err) != codes.NotFound {
		t.Errorf("after versions delete, GetVersion = %v; want NotFound", err)
	}
}
