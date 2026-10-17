package backupdr

// Resetting a project (#1542): what /admin/reset?project= and a project's
// purge (#1537) delete. Google deletes a purged project's backup vaults,
// with their data sources and backups, its backup plans and their
// associations, its trial and its operations; a project created again
// under its ID has none of them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ResetProject deletes every record named under project, or every record
// when project is "": vaults, data sources, backups, with the Cloud SQL
// backup runs that hold them, plans, associations, trials and operations.
// A vault elsewhere that backs up one of project's instances keeps its
// backups, as on Google, where a vault's backups outlive what they protect.
func (s *Server) ResetProject(_ context.Context, project string) error {
	in := func(name string) bool {
		return project == "" || strings.HasPrefix(name, "projects/"+project+"/")
	}
	var runs []string
	s.mu.Lock()
	for n, b := range s.st.Backups {
		if in(n) {
			runs = append(runs, b.RunID)
			delete(s.st.Backups, n)
		}
	}
	dropIn(s.st.Vaults, in)
	dropIn(s.st.DataSources, in)
	dropIn(s.st.Plans, in)
	dropIn(s.st.Associations, in)
	// A trial is by its location's name, projects/{p}/locations/{l}.
	dropIn(s.st.Trials, func(n string) bool { return in(n + "/") })
	dropIn(s.ops, in)
	err := s.saveLocked()
	s.mu.Unlock()
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("save the Backup and DR records: %w", err))
	}
	for _, id := range runs {
		if id == "" || s.sql == nil {
			continue
		}
		if err := s.sql.DeleteRun(id); err != nil {
			errs = append(errs, fmt.Errorf("delete backup run %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func dropIn[V any](m map[string]V, in func(string) bool) {
	for k := range m {
		if in(k) {
			delete(m, k)
		}
	}
}
