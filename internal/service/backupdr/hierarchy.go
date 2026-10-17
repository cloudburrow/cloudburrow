package backupdr

// A folder's and the organization's resourceBackupConfigs.fetch (#1536):
// "the resource backup configs of the resources in the folder or
// organization", which are every project's under it, through its
// subfolders. Backup and DR runs in the CLI's process, beside Resource
// Manager, so the CLI hands it the registry's hierarchy as it hands Cloud
// Logging and Autokey theirs (cmd/cloudburrow/backupdr.go): the projects
// under a folder or the organization. A server given none (unit tests, an
// instance without Resource Manager) refuses both methods with
// hierarchyWhy, as before.

import (
	"errors"
	"net/http"
	"sort"
)

// Hierarchy is Resource Manager's folders and organization as Backup and
// DR sees them.
type Hierarchy interface {
	// ProjectsUnder is the ID of every active project under parent
	// ("folders/{id}" or "organizations/{id}"), directly or through
	// folders. It returns ErrNoParent when there is no such folder or
	// organization.
	ProjectsUnder(parent string) ([]string, error)
}

// ErrNoParent is what ProjectsUnder returns for a folder or an
// organization Resource Manager does not have.
var ErrNoParent = errors.New("no such folder or organization")

// UseHierarchy makes a folder's and the organization's fetch see h's
// projects. Call it before Start.
func (s *Server) UseHierarchy(h Hierarchy) {
	s.mu.Lock()
	s.hierarchy = h
	s.mu.Unlock()
}

// projectsUnder is the sorted IDs of the projects under kind/id.
func (s *Server) projectsUnder(kind, id string) ([]string, error) {
	method := kind + ".locations.resourceBackupConfigs.fetch"
	s.mu.Lock()
	h := s.hierarchy
	s.mu.Unlock()
	if h == nil {
		return nil, unimplemented(method, hierarchyWhy)
	}
	parent := kind + "/" + id
	ids, err := h.ProjectsUnder(parent)
	if errors.Is(err, ErrNoParent) {
		// Google answers a folder or organization the caller cannot see,
		// or that does not exist, with 403.
		return nil, errorf(http.StatusForbidden, "PERMISSION_DENIED", "Permission denied on resource '%s' (or it may not exist).", parent)
	}
	if err != nil {
		return nil, errorf(http.StatusServiceUnavailable, "UNAVAILABLE", "cloudburrow: could not read the projects under %s from Resource Manager: %v", parent, err)
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	return ids, nil
}
