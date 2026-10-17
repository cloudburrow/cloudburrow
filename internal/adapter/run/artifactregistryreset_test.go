package run

import (
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// A project's reset (#1543) deletes its repositories, with their images,
// files, rules and policies, and its settings and configs, saved state
// too, and leaves another project's alone.
func TestResetArtifactRegistryDropsOnlyThatProject(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	defer UseRegistry(nil)

	seed := func(p string) string {
		repo := "projects/" + p + "/locations/us-central1/repositories/r"
		repositories.mu.Lock()
		repositories.repos[repo] = &arpb.Repository{Name: repo, Format: arpb.Repository_DOCKER}
		repositories.policies[repo] = iampolicy.Stored{}
		saveRepositoriesLocked()
		repositories.mu.Unlock()
		arConfig.mu.Lock()
		arConfig.rules[repo+"/rules/keep"] = &arpb.Rule{Name: repo + "/rules/keep"}
		arConfig.settings["projects/"+p+"/projectSettings"] = &arpb.ProjectSettings{Name: "projects/" + p + "/projectSettings"}
		arConfig.vpcsc["projects/"+p+"/locations/us-central1/vpcscConfig"] = &arpb.VPCSCConfig{}
		arConfig.projectConfigs["projects/"+p+"/locations/us-central1/projectConfig"] = &arProjectConfig{}
		saveARConfigLocked()
		arConfig.mu.Unlock()
		if _, err := addFile(repo, "f", "pkg", "1", []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	gone, kept := seed("ar-reset-gone"), seed("ar-reset-kept")
	defer func() { _ = ResetArtifactRegistry("ar-reset-kept") }()

	if err := ResetArtifactRegistry("ar-reset-gone"); err != nil {
		t.Fatal(err)
	}
	has := func(p, repo string) []string {
		var got []string
		repositories.mu.Lock()
		if _, ok := repositories.repos[repo]; ok {
			got = append(got, "repository")
		}
		if _, ok := repositories.policies[repo]; ok {
			got = append(got, "policy")
		}
		repositories.mu.Unlock()
		arConfig.mu.Lock()
		if _, ok := arConfig.rules[repo+"/rules/keep"]; ok {
			got = append(got, "rule")
		}
		if _, ok := arConfig.settings["projects/"+p+"/projectSettings"]; ok {
			got = append(got, "settings")
		}
		if _, ok := arConfig.vpcsc["projects/"+p+"/locations/us-central1/vpcscConfig"]; ok {
			got = append(got, "vpcsc")
		}
		if _, ok := arConfig.projectConfigs["projects/"+p+"/locations/us-central1/projectConfig"]; ok {
			got = append(got, "projectConfig")
		}
		arConfig.mu.Unlock()
		if len(repoFilesOf(repo)) > 0 {
			got = append(got, "files")
		}
		return got
	}
	if got := has("ar-reset-gone", gone); len(got) != 0 {
		t.Errorf("the reset project kept its %v", got)
	}
	if got := has("ar-reset-kept", kept); len(got) != 7 {
		t.Errorf("the other project has only %v", got)
	}

	// What was saved is gone too: a restart reads only the other project's.
	repositories.mu.Lock()
	repositories.repos = map[string]*arpb.Repository{}
	repositories.policies = map[string]iampolicy.Stored{}
	repositories.mu.Unlock()
	arConfig.mu.Lock()
	arConfig.rules, arConfig.settings = map[string]*arpb.Rule{}, map[string]*arpb.ProjectSettings{}
	arConfig.vpcsc, arConfig.projectConfigs = map[string]*arpb.VPCSCConfig{}, map[string]*arProjectConfig{}
	arConfig.mu.Unlock()
	UseRegistry(st)
	if got := has("ar-reset-gone", gone); len(got) != 0 {
		t.Errorf("after a restart the reset project has its %v", got)
	}
	if got := has("ar-reset-kept", kept); len(got) != 7 {
		t.Errorf("after a restart the other project has only %v", got)
	}
}
