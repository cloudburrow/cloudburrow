package main

// The Permissions tab on an Artifact Registry repository's page (#1439):
// the repository's stored policy (#1304, ADR-0006), through the same
// GetIamPolicy, SetIamPolicy and TestIamPermissions the ArtifactRegistry
// API serves, as the repository's page reads the same repositories. An
// image and a version have no policy of their own.

import (
	"context"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// PolicyOn is the tab on a repository's page alone.
func (artifactRegistryProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: runPermissionsLink, RoleHelp: roleHelp("Artifact Registry", "roles/artifactregistry.reader")}
}

// withRepositoryIAM calls fn with the API's IAM methods and the full name
// of the repository at path, found as the page finds it.
func (p artifactRegistryProvider) withRepositoryIAM(project string, path []string, fn func(iamServer, string) error) error {
	r, _, err := p.repository(project, path[0])
	if err != nil {
		return err
	}
	return fn(&runadapter.ArtifactRegistryServer{}, r.GetName())
}

func (p artifactRegistryProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withRepositoryIAM(project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p artifactRegistryProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withRepositoryIAM(project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p artifactRegistryProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withRepositoryIAM(project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = artifactRegistryProvider{}
