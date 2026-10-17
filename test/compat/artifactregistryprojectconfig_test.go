//go:build compat

package compat

import (
	"net/http"
	"testing"

	arapi "google.golang.org/api/artifactregistry/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestArtifactRegistryProjectConfig (#1519), with Google's generated
// google.golang.org/api/artifactregistry/v1 client (the Go apiv1 client
// has no projectConfig): a location's projectConfig reads back with its
// name and no platform logs config until one is set; updateProjectConfig
// with an updateMask sets loggingState and severityLevel, a later one of
// severityLevel alone keeps loggingState, an unknown enum value is
// INVALID_ARGUMENT, and another location's stays unset.
func TestArtifactRegistryProjectConfig(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	ctx := h.Context()
	svc, err := arapi.NewService(ctx, option.WithEndpoint("http://"+endpoint+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	locs := svc.Projects.Locations
	name := "projects/" + h.Project() + "/locations/us-east1/projectConfig"
	t.Cleanup(func() {
		_, _ = locs.UpdateProjectConfig(name, &arapi.ProjectConfig{Name: name}).Context(h.Context()).Do()
	})
	got, err := locs.GetProjectConfig(name).Context(ctx).Do()
	if err != nil || got.Name != name || (got.PlatformLogsConfig != nil && got.PlatformLogsConfig.LoggingState != "") {
		t.Fatalf("getProjectConfig = %+v, %v; want its name and no platform logs config", got, err)
	}
	set, err := locs.UpdateProjectConfig(name, &arapi.ProjectConfig{Name: name,
		PlatformLogsConfig: &arapi.PlatformLogsConfig{LoggingState: "ENABLED", SeverityLevel: "WARNING"}}).
		UpdateMask("platformLogsConfig").Context(ctx).Do()
	if err != nil || set.PlatformLogsConfig == nil || set.PlatformLogsConfig.LoggingState != "ENABLED" || set.PlatformLogsConfig.SeverityLevel != "WARNING" {
		t.Fatalf("updateProjectConfig = %+v, %v", set, err)
	}
	set, err = locs.UpdateProjectConfig(name, &arapi.ProjectConfig{Name: name,
		PlatformLogsConfig: &arapi.PlatformLogsConfig{SeverityLevel: "ERROR"}}).
		UpdateMask("platformLogsConfig.severityLevel").Context(ctx).Do()
	if err != nil || set.PlatformLogsConfig.LoggingState != "ENABLED" || set.PlatformLogsConfig.SeverityLevel != "ERROR" {
		t.Errorf("updateProjectConfig of severityLevel = %+v, %v; want loggingState kept", set, err)
	}
	if got, err = locs.GetProjectConfig(name).Context(ctx).Do(); err != nil || got.PlatformLogsConfig == nil || got.PlatformLogsConfig.SeverityLevel != "ERROR" {
		t.Errorf("getProjectConfig after the update = %+v, %v", got, err)
	}
	_, err = locs.UpdateProjectConfig(name, &arapi.ProjectConfig{PlatformLogsConfig: &arapi.PlatformLogsConfig{LoggingState: "LOUD"}}).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != http.StatusBadRequest {
		t.Errorf("updateProjectConfig with an unknown loggingState = %v, want 400", err)
	}
	other := "projects/" + h.Project() + "/locations/europe-west1/projectConfig"
	if got, err := locs.GetProjectConfig(other).Context(ctx).Do(); err != nil || got.PlatformLogsConfig != nil {
		t.Errorf("another location's projectConfig = %+v, %v; want unset", got, err)
	}
}
