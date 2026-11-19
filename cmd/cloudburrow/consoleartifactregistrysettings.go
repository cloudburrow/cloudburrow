package main

// Artifact Registry's Settings page (#1855): the project's settings
// (GetProjectSettings: gcr.io redirection and its pull percent), and each
// location's VPC Service Controls policy (GetVPCSCConfig) and platform logs
// (the projectConfig of #1519), with Set project pull percent, Set VPC-SC
// policy and Set platform logs, which were actions of a repository's page
// before; Google's console has them on no repository.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

type artifactRegistrySettingsProvider struct{ defaultProject string }

func (artifactRegistrySettingsProvider) ID() string    { return "artifact-registry-settings" }
func (artifactRegistrySettingsProvider) Title() string { return "Settings" }

func (p artifactRegistrySettingsProvider) project(project string) string {
	if project == "" {
		return p.defaultProject
	}
	return project
}

// List is one row per setting: the project's, then each location's where
// the project has a repository or has set one.
func (p artifactRegistrySettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	project = p.project(project)
	out := console.Listing{Columns: []string{"Scope", "Value"}, NameColumn: "Setting", Noun: "settings",
		Note: "Settings are stored and read back: this instance serves no gcr.io, enforces no VPC Service Controls perimeter " +
			"and writes no platform logs."}
	cs := runadapter.ConfigServer()
	if ps, err := cs.GetProjectSettings(ctx, &arpb.GetProjectSettingsRequest{Name: "projects/" + project + "/projectSettings"}); err == nil {
		out.Items = append(out.Items,
			console.Resource{Name: "Container Registry (gcr.io) redirection", Fields: map[string]string{"Scope": project, "Value": ps.GetLegacyRedirectionState().String()}},
			console.Resource{Name: "Pull percent", Fields: map[string]string{"Scope": project, "Value": strconv.Itoa(int(ps.GetPullPercent()))}})
	}
	locs := map[string]bool{}
	for _, r := range runadapter.Repositories() {
		parts := strings.Split(r.GetName(), "/")
		if len(parts) == 6 && parts[1] == project {
			locs[parts[3]] = true
		}
	}
	for _, l := range runadapter.ArtifactRegistryLocations {
		if st, sev := runadapter.ProjectPlatformLogs(project, l); st != "" || sev != "" {
			locs[l] = true
		}
		if c, err := cs.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: "projects/" + project + "/locations/" + l + "/vpcscConfig"}); err == nil &&
			c.GetVpcscPolicy() == arpb.VPCSCConfig_ALLOW {
			locs[l] = true
		}
	}
	names := make([]string, 0, len(locs))
	for l := range locs {
		names = append(names, l)
	}
	sort.Strings(names)
	for _, l := range names {
		vpcsc := "-"
		if c, err := cs.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: "projects/" + project + "/locations/" + l + "/vpcscConfig"}); err == nil {
			vpcsc = c.GetVpcscPolicy().String()
		}
		logs := "-"
		if st, sev := runadapter.ProjectPlatformLogs(project, l); st != "" || sev != "" {
			logs = strings.Trim(st+", severity "+orDash(sev), ", ")
		}
		out.Items = append(out.Items,
			console.Resource{Name: "VPC-SC policy (" + l + ")", Fields: map[string]string{"Scope": l, "Value": vpcsc}},
			console.Resource{Name: "Platform logs (" + l + ")", Fields: map[string]string{"Scope": l, "Value": logs}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// ListActions are the settings' changes, drawn on the page.
func (p artifactRegistrySettingsProvider) ListActions(context.Context, string) []console.Action {
	locs := runadapter.ArtifactRegistryLocations
	return []console.Action{
		{ID: "pull-percent", Label: "Set project pull percent", Fields: []console.Field{
			{Name: "percent", Label: "Pull percent", Type: "number", Default: "0",
				Help: "UpdateProjectSettings' pull_percent; gcr.io redirection stays off, as nothing here serves gcr.io"}}},
		{ID: "vpcsc", Label: "Set VPC-SC policy", Fields: []console.Field{
			{Name: "location", Label: "Location", Type: "select", Options: locs, Default: "us-central1"},
			{Name: "policy", Label: "Policy", Type: "select", Options: []string{"DENY", "ALLOW"}, Default: "DENY",
				Help: "the location's policy for remote repositories' upstream fetches, as UpdateVPCSCConfig sets it"}}},
		{ID: "platform-logs", Label: "Set platform logs", Fields: []console.Field{
			{Name: "location", Label: "Location", Type: "select", Options: locs, Default: "us-central1"},
			{Name: "state", Label: "Logging state", Type: "select", Options: []string{"ENABLED", "DISABLED", "LOGGING_STATE_UNSPECIFIED"}, Default: "ENABLED",
				Help: "the location's projectConfig platformLogsConfig, as updateProjectConfig sets it (#1519); stored: this instance writes no platform logs"},
			{Name: "severity", Label: "Severity level", Type: "select", Options: []string{"SEVERITY_LEVEL_UNSPECIFIED", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"},
				Default: "SEVERITY_LEVEL_UNSPECIFIED"}}},
	}
}

// DetailActions: a setting has no page of its own.
func (artifactRegistrySettingsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

// ActAt performs a list action (an empty path).
func (p artifactRegistrySettingsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	return arSetSetting(ctx, p.project(project), action, v)
}

// arSetSetting is one of the Settings page's changes.
func arSetSetting(ctx context.Context, project, action string, v map[string]string) error {
	cs := runadapter.ConfigServer()
	loc := strings.TrimSpace(v["location"])
	switch action {
	case "pull-percent":
		n, err := strconv.Atoi(strings.TrimSpace(v["percent"]))
		if err != nil {
			return fmt.Errorf("pull percent %q is not a number", v["percent"])
		}
		_, err = cs.UpdateProjectSettings(ctx, &arpb.UpdateProjectSettingsRequest{ProjectSettings: &arpb.ProjectSettings{
			Name: "projects/" + project + "/projectSettings", PullPercent: int32(n)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pull_percent"}}})
		return err
	case "vpcsc":
		_, err := cs.UpdateVPCSCConfig(ctx, &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{
			Name:        "projects/" + project + "/locations/" + loc + "/vpcscConfig",
			VpcscPolicy: arpb.VPCSCConfig_VPCSCPolicy(arpb.VPCSCConfig_VPCSCPolicy_value[v["policy"]])}})
		return err
	case "platform-logs":
		return runadapter.SetProjectPlatformLogs(project, loc, v["state"], v["severity"])
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Provider  = artifactRegistrySettingsProvider{}
	_ console.ListActor = artifactRegistrySettingsProvider{}
)
