package firestoreadmin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// ResetProject deletes one project's Firestore state, as Google does when
// the project is purged (#1540): the documents of each of its databases,
// `(default)` whether or not the admin API has named it, through the
// emulator's own endpoint, and the databases, indexes, field overrides,
// backup schedules, backups and operations the front keeps for it. The
// project is unseeded too, so one created again under the ID gets a new
// `(default)`.
func (a *Admin) ResetProject(ctx context.Context, project string) error {
	if project == "" {
		return errors.New("a project is required")
	}
	prefix := "projects/" + project + "/databases/"
	a.mu.Lock()
	dbs := map[string]bool{prefix + DefaultDatabase: true}
	for name := range a.databases {
		if strings.HasPrefix(name, prefix) {
			dbs[name] = true
		}
	}
	frontstate.DropProject(a.databases, project)
	delete(a.seeded, project)
	frontstate.DropProject(a.indexes, project)
	frontstate.DropProject(a.fields, project)
	frontstate.DropProject(a.schedules, project)
	frontstate.DropProject(a.ops, project)
	a.opOrder = frontstate.KeepOthers(a.opOrder, project)
	frontstate.DropProject(a.backups, project)
	frontstate.DropProject(a.lastBackup, project)
	a.mu.Unlock()
	names := make([]string, 0, len(dbs))
	for n := range dbs {
		names = append(names, n)
	}
	sort.Strings(names)
	var errs []error
	for _, n := range names {
		if err := a.clearDocuments(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("clear %s: %w", n, err))
		}
	}
	if err := a.save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
