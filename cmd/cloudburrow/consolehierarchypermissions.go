package main

// The Permissions tab on a folder's page (#1552): the folder's stored IAM
// policy (ADR-0006, #1368), read and written through the v3 Folders API's
// GetIamPolicy, SetIamPolicy and TestIamPermissions, as Google's console
// offers IAM for a folder once it is selected in the resource picker.

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

func (p hierarchyProvider) foldersIAM() iamServer {
	return resourcemanager.NewFoldersServer(p.registry)
}

// folderIAMName is the folder at path as the Folders API names it.
func folderIAMName(path []string) (string, error) {
	if len(path) != 1 {
		return "", errors.New("only a folder has a Permissions tab")
	}
	return resourcemanager.FolderName(strings.TrimPrefix(path[0], "folders/"))
}

// PolicyOn draws the tab on an active folder's page.
func (p hierarchyProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	if f, err := p.registry.Folder(path[0]); err != nil || f.State != resourcemanager.StateActive {
		return nil
	}
	return &console.PolicyTarget{Link: projectIAMLink, RoleHelp: roleHelp("Resource Manager", "roles/resourcemanager.folderViewer")}
}

func (p hierarchyProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	name, err := folderIAMName(path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, p.foldersIAM(), name)
}

func (p hierarchyProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	name, err := folderIAMName(path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, p.foldersIAM(), name, pol)
}

func (p hierarchyProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) ([]string, error) {
	name, err := folderIAMName(path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, p.foldersIAM(), name, perms)
}

var _ console.PolicyEditor = hierarchyProvider{}
