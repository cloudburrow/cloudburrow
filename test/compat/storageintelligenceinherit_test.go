//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestStorageIntelligenceInheritsThroughFolders (#1532): with the
// organization's configuration STANDARD, a folder made with Resource
// Manager's official v3 client set to STANDARD with its own filter, and a
// project created in that folder left to INHERIT, the project's effective
// configuration, read with the official Storage Control client, is the
// folder's; with the folder set to INHERIT, the project's is the
// organization's. The organization's configuration is put back as it was.
//
// covers: google.storage.control.v2.StorageControl/GetProjectIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateFolderIntelligenceConfig
// covers: google.storage.control.v2.StorageControl/GetOrganizationIntelligenceConfig, google.storage.control.v2.StorageControl/UpdateOrganizationIntelligenceConfig
func TestStorageIntelligenceInheritsThroughFolders(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	fc, _, pc := foldersClients(t, h)
	ctx := h.Context()
	g := storageControlGRPC(t, h)

	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "sii-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folderID := strings.TrimPrefix(f.GetName(), "folders/")
	project := v1ID(h, "sii-")
	cop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: f.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject in %s: %v", f.GetName(), err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatalf("CreateProject in %s: %v", f.GetName(), err)
	}

	orgName := intelligenceName("organizations", strings.TrimPrefix(localOrg, "organizations/"))
	folderName := intelligenceName("folders", folderID)
	before, err := g.GetOrganizationIntelligenceConfig(ctx, &controlpb.GetOrganizationIntelligenceConfigRequest{Name: orgName})
	if err != nil {
		t.Fatalf("GetOrganizationIntelligenceConfig: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = g.UpdateFolderIntelligenceConfig(ctx, &controlpb.UpdateFolderIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folderName, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
		_, _ = g.UpdateOrganizationIntelligenceConfig(ctx, &controlpb.UpdateOrganizationIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: orgName, EditionConfig: before.GetEditionConfig(), Filter: before.GetFilter()},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}})
		if op, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(ctx)
		}
		_, _ = fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: f.GetName()})
	})

	if _, err := g.UpdateOrganizationIntelligenceConfig(ctx, &controlpb.UpdateOrganizationIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: orgName, EditionConfig: controlpb.IntelligenceConfig_STANDARD},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}}); err != nil {
		t.Fatalf("organization STANDARD: %v", err)
	}
	filter := &controlpb.IntelligenceConfig_Filter{
		CloudStorageLocations: &controlpb.IntelligenceConfig_Filter_IncludedCloudStorageLocations{
			IncludedCloudStorageLocations: &controlpb.IntelligenceConfig_Filter_CloudStorageLocations{Locations: []string{"europe-west1"}}}}
	if _, err := g.UpdateFolderIntelligenceConfig(ctx, &controlpb.UpdateFolderIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folderName, EditionConfig: controlpb.IntelligenceConfig_STANDARD, Filter: filter},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}}); err != nil {
		t.Fatalf("folder STANDARD: %v", err)
	}

	effective := func() *controlpb.IntelligenceConfig_EffectiveIntelligenceConfig {
		t.Helper()
		p, err := g.GetProjectIntelligenceConfig(ctx, &controlpb.GetProjectIntelligenceConfigRequest{Name: intelligenceName("projects", project)})
		if err != nil || p.GetEditionConfig() != controlpb.IntelligenceConfig_INHERIT {
			t.Fatalf("GetProjectIntelligenceConfig = %v, %v; want INHERIT", p, err)
		}
		return p.GetEffectiveIntelligenceConfig()
	}
	if eff := effective(); eff.GetIntelligenceConfig() != folderName ||
		eff.GetEffectiveEdition() != controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_STANDARD {
		t.Errorf("project under a STANDARD folder: effective %v; want %s", eff, folderName)
	}

	if _, err := g.UpdateFolderIntelligenceConfig(ctx, &controlpb.UpdateFolderIntelligenceConfigRequest{
		IntelligenceConfig: &controlpb.IntelligenceConfig{Name: folderName, EditionConfig: controlpb.IntelligenceConfig_INHERIT},
		UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config", "filter"}}}); err != nil {
		t.Fatalf("folder INHERIT: %v", err)
	}
	if eff := effective(); eff.GetIntelligenceConfig() != orgName ||
		eff.GetEffectiveEdition() != controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_STANDARD {
		t.Errorf("project under an inheriting folder: effective %v; want %s", eff, orgName)
	}
	fo, err := g.GetFolderIntelligenceConfig(ctx, &controlpb.GetFolderIntelligenceConfigRequest{Name: folderName})
	if err != nil || fo.GetEffectiveIntelligenceConfig().GetIntelligenceConfig() != orgName {
		t.Errorf("inheriting folder = %v, %v; want the organization's effective", fo, err)
	}
}
