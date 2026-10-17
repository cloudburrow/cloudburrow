package sqladmin

// Resetting a project (#1541): what /admin/reset?project= and a project's
// purge (#1537) delete. Google deletes a purged project's Cloud SQL
// instances, with their data, users and databases, and their backups,
// client certificates and operations; a project created again under its
// ID lists none of them.

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// ResetProject deletes what the Admin API keeps for project, or for every
// project when project is "": the instances created with instances.insert,
// each with its server, standby and volume, which hold its users and
// databases; the source representation instances; the backups (final ones
// too) and their dumps; the client certificates; and the operations. The
// servers `up` runs are every project's, and stay, with their users and
// databases; their client certificates issued in project go. A Backup and
// DR backup is that service's, and its reset deletes it.
func (s *Server) ResetProject(ctx context.Context, project string) error {
	in := func(p string) bool { return project == "" || p == project }
	var errs []error

	// Instances. Each is marked PENDING_DELETE first, so a server still
	// starting does not mark it RUNNABLE while it goes.
	var doomed []*created
	s.emu.Lock()
	w := s.writes
	if w != nil {
		for _, c := range w.byName {
			if in(c.spec.Project) {
				c.state = "PENDING_DELETE"
				doomed = append(doomed, c)
			}
		}
		changed := false
		for name, r := range w.sources {
			if in(r.Project) {
				delete(w.sources, name)
				changed = true
			}
		}
		if changed {
			if err := s.saveSourcesLocked(); err != nil {
				errs = append(errs, fmt.Errorf("save the source representation instances: %w", err))
			}
		}
	}
	s.emu.Unlock()
	for _, c := range doomed {
		name := c.spec.Name
		if err := w.prov.Remove(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("remove instance %s: %w", name, err))
			continue
		}
		s.emu.Lock()
		if w.byName[name] == c {
			delete(w.byName, name)
		}
		s.emu.Unlock()
		if s.conn != nil {
			s.conn.removeInstance(name)
		}
	}
	if len(doomed) > 0 {
		s.emu.Lock()
		err := s.saveLocked()
		s.emu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("save the instances: %w", err))
		}
	}

	// Backups, the final ones of deleted instances too.
	if b := s.backups; b != nil {
		b.mu.Lock()
		changed := false
		for id, r := range b.runs {
			if in(r.Project) && !r.BackupDR {
				delete(b.runs, id)
				_ = os.RemoveAll(b.runDir(id))
				changed = true
			}
		}
		if changed {
			if err := b.saveLocked(); err != nil {
				errs = append(errs, fmt.Errorf("save the backups: %w", err))
			}
		}
		b.mu.Unlock()
	}

	// The project's client certificates on the servers `up` runs; a
	// created instance's went with it.
	if s.certs != nil {
		s.certs.mu.Lock()
		for _, e := range s.engines {
			certs, err := s.certs.load(e.Instance())
			if err != nil {
				errs = append(errs, fmt.Errorf("read the client certificates of %s: %w", e.Instance(), err))
				continue
			}
			kept := certs[:0]
			for _, c := range certs {
				if !in(c.Project) {
					kept = append(kept, c)
				}
			}
			if len(kept) != len(certs) {
				if err := s.certs.save(e.Instance(), kept); err != nil {
					errs = append(errs, fmt.Errorf("save the client certificates of %s: %w", e.Instance(), err))
				}
			}
		}
		s.certs.mu.Unlock()
	}

	// Operations, and the imports and exports still running under them.
	var gone []string
	s.mu.Lock()
	order := s.opOrder[:0]
	for _, id := range s.opOrder {
		if p, _ := s.ops[id]["targetProject"].(string); in(p) {
			delete(s.ops, id)
			gone = append(gone, id)
			continue
		}
		order = append(order, id)
	}
	s.opOrder = order
	s.mu.Unlock()
	s.emu.RLock()
	t := s.transfers
	s.emu.RUnlock()
	if t != nil {
		t.mu.Lock()
		for _, id := range gone {
			if cancel := t.cancels[id]; cancel != nil {
				cancel()
				delete(t.cancels, id)
			}
		}
		t.mu.Unlock()
	}
	return errors.Join(errs...)
}
