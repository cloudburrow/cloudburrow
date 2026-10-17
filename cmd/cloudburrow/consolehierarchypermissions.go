package main

// The Permissions tab on a folder's page (#1552) and on the organization's
// (#1554): the stored IAM policy (ADR-0006, #1368), read and written through
// the v3 Folders and Organizations APIs' GetIamPolicy, SetIamPolicy and
// TestIamPermissions, as Google's console offers IAM for a folder or the
// organization once it is selected in the resource picker.

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// iamTarget is the API whose policy the page at path shows, and the
// resource name it takes: the organization's through Organizations, a
// folder's through Folders.
func (p hierarchyProvider) iamTarget(path []string) (iamServer, string, error) {
	if len(path) != 1 {
		return nil, "", errors.New("only a folder or the organization has a Permissions tab")
	}
	if isOrganizationPath(path) {
		return resourcemanager.NewOrganizationsServer(p.registry), resourcemanager.OrganizationName, nil
	}
	name, err := resourcemanager.FolderName(strings.TrimPrefix(path[0], "folders/"))
	return resourcemanager.NewFoldersServer(p.registry), name, err
}

// PolicyOn draws the tab on an active folder's page.
func (p hierarchyProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	if isOrganizationPath(path) {
		return &console.PolicyTarget{Link: projectIAMLink, RoleHelp: roleHelp("Resource Manager", "roles/resourcemanager.organizationViewer")}
	}
	if f, err := p.registry.Folder(path[0]); err != nil || f.State != resourcemanager.StateActive {
		return nil
	}
	return &console.PolicyTarget{Link: projectIAMLink, RoleHelp: roleHelp("Resource Manager", "roles/resourcemanager.folderViewer")}
}

func (p hierarchyProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	api, name, err := p.iamTarget(path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p hierarchyProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	api, name, err := p.iamTarget(path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p hierarchyProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) ([]string, error) {
	api, name, err := p.iamTarget(path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}

var _ console.PolicyEditor = hierarchyProvider{}
