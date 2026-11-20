package main

// The resource the toolbar's resource selector chose (#2023). Google's
// console scopes the pages that work at that level (IAM, Organization
// Policies, Tags) to the organization or a folder once it is chosen: its
// address carries ?organizationId=N or ?folder=N, as console.cloud.google.com
// does, beside the project the per-project pages keep reading. Each console
// request carries them (console.ScopeValue), and these pages read and write
// that resource through the same served APIs: Folders' and Organizations'
// GetIamPolicy and SetIamPolicy, the organization policy registry the v1 and
// v2 APIs serve, and TagKeys.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// The query parameters Google's console addresses a folder and the
// organization by.
const (
	scopeFolderParam = "folder"
	scopeOrgParam    = "organizationId"
)

// selectedResource is the folder or organization chosen in the toolbar.
type selectedResource struct {
	// Name is folders/N or organizations/N.
	Name string
	// Kind is Folder or Organization; Display its name in the toolbar.
	Kind, Display string
}

// consoleResourceScope is the folder or organization ctx's request names,
// ok false when it names neither (the project is the scope), and an error
// when it names one that does not exist.
func consoleResourceScope(ctx context.Context, reg *resourcemanager.Registry) (selectedResource, bool, error) {
	if id := console.ScopeValue(ctx, scopeFolderParam); id != "" {
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return selectedResource{}, true, fmt.Errorf("folder %q must be a folder number", id)
		}
		f, err := reg.Folder("folders/" + id)
		if err != nil {
			return selectedResource{}, true, fmt.Errorf("folder %s: %w", id, err)
		}
		if f.State != resourcemanager.StateActive {
			return selectedResource{}, true, fmt.Errorf("folder %s is pending deletion", id)
		}
		d := f.DisplayName
		if d == "" {
			d = f.Name
		}
		return selectedResource{Name: f.Name, Kind: "Folder", Display: d}, true, nil
	}
	if id := console.ScopeValue(ctx, scopeOrgParam); id != "" {
		if "organizations/"+id != resourcemanager.OrganizationName {
			return selectedResource{}, true, fmt.Errorf("organization %s not found; the local organization is %s", id, resourcemanager.OrganizationName)
		}
		return selectedResource{Name: resourcemanager.OrganizationName, Kind: "Organization", Display: resourcemanager.OrganizationDomain}, true, nil
	}
	return selectedResource{}, false, nil
}

// iamServerOf is the API whose IAM methods serve name (folders/N or
// organizations/N).
func iamServerOf(reg *resourcemanager.Registry, name string) iamServer {
	if name == resourcemanager.OrganizationName {
		return resourcemanager.NewOrganizationsServer(reg)
	}
	return resourcemanager.NewFoldersServer(reg)
}

// ancestorsOf are the folders above name, then the organization, nearest
// first; none for the organization.
func ancestorsOf(reg *resourcemanager.Registry, name string) []selectedResource {
	var out []selectedResource
	if name == resourcemanager.OrganizationName {
		return nil
	}
	for i := 0; name != resourcemanager.OrganizationName && i <= resourcemanager.MaxFolderDepth+1; i++ {
		f, err := reg.Folder(name)
		if err != nil {
			return out
		}
		name = f.Parent
		if name == resourcemanager.OrganizationName {
			break
		}
		pf, err := reg.Folder(name)
		d := name
		if err == nil && pf.DisplayName != "" {
			d = pf.DisplayName
		}
		out = append(out, selectedResource{Name: name, Kind: "Folder", Display: d})
	}
	if name == resourcemanager.OrganizationName {
		out = append(out, selectedResource{Name: resourcemanager.OrganizationName, Kind: "Organization", Display: resourcemanager.OrganizationName})
	}
	return out
}
