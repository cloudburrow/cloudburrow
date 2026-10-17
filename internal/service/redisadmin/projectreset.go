package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// ResetProject deletes every instance CreateInstance made in project, its
// servers and its kept record, at once (#1540): what Google drops with a
// purged project. An instance with an operation in progress is deleted too,
// and the operation fails. The instance `cloudburrow up` runs is not one
// CreateInstance made, and stays.
func (s *Server) ResetProject(ctx context.Context, project string) error {
	c := s.created
	if c == nil || project == "" {
		return nil
	}
	prefix := "projects/" + project + "/"
	c.mu.Lock()
	var gone []*record
	var ops []string
	for name, r := range c.byID {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		gone = append(gone, r)
		delete(c.byID, name)
		if op, ok := c.busy[name]; ok {
			ops = append(ops, op)
			delete(c.busy, name)
			if cancel, ok := c.cancels[op]; ok {
				cancel()
				delete(c.cancels, op)
			}
		}
	}
	c.mu.Unlock()
	for _, op := range ops {
		if c.ops != nil {
			_ = c.ops.Fail(op, apierror.Aborted("the instance's project was deleted"))
		}
	}
	var errs []error
	for _, r := range gone {
		if err := c.removeServers(ctx, *r); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", r.Name, err))
		}
		if c.db != nil {
			if err := c.db.Delete(recordPrefix + r.Server); err != nil && !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, fmt.Errorf("forget %s: %w", r.Name, err))
			}
		}
		c.mu.Lock()
		r.closeEndpoints()
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}
