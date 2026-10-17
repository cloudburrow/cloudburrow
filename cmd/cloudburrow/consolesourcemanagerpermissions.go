package main

// The Permissions tab on a Secure Source Manager instance's and
// repository's pages (#1527): the stored policy (ADR-0006), through the
// same IAM methods the API serves: the IAMPolicy mixin on an instance,
// GetIamPolicyRepo, SetIamPolicyRepo and TestIamPermissionsRepo on a
// repository.

import (
	"context"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// sourceManagerPermissionsLink: the Secure Source Manager row is in the
// Cloud Run section, as it is served on the Run port.
const sourceManagerPermissionsLink = runPermissionsLink

// PolicyOn is the tab on an instance's page and a repository's.
func (sourceManagerProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	switch len(path) {
	case 1:
		return &console.PolicyTarget{Link: sourceManagerPermissionsLink, RoleHelp: roleHelp("Secure Source Manager", "roles/securesourcemanager.instanceAccessor")}
	case 2:
		return &console.PolicyTarget{Link: sourceManagerPermissionsLink, RoleHelp: roleHelp("Secure Source Manager", "roles/securesourcemanager.repoReader")}
	}
	return nil
}

// withIAM calls fn with the IAM methods and the full name of the instance
// or repository at path, found as the page finds it.
func (p sourceManagerProvider) withIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return err
	}
	if len(path) == 1 {
		return fn(runadapter.SourceManagerInstanceIAM(), in.GetName())
	}
	r, err := p.repository(ctx, in, path[1])
	if err != nil {
		return err
	}
	return fn(runadapter.SourceManagerRepositoryIAM(), r.GetName())
}

func (p sourceManagerProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p sourceManagerProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p sourceManagerProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = sourceManagerProvider{}
