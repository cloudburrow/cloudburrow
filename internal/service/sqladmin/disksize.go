package sqladmin

// Storage shrink (#1535): getDiskShrinkConfig, performDiskShrink and
// resetReplicaSize, as Cloud SQL's "Decrease storage size" documents them.
// An instance's settings.dataDiskSizeGb is recorded as given (Spec.Settings:
// the size of the disk Cloud SQL would provision; the server's data lives
// in its volume in the cluster, which has no fixed size). The smallest size
// it can shrink to is measured from what its server holds: its databases'
// size on disk, rounded up to a whole GB, and never below Cloud SQL's
// minimum of 10 GB.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// minDiskGB is the smallest data disk Cloud SQL provisions.
const minDiskGB = 10

// diskSizeGB is a created instance's settings.dataDiskSizeGb, Cloud SQL's
// default of 10 when it was given none.
func diskSizeGB(spec Spec) int64 {
	switch v := spec.Settings["dataDiskSizeGb"].(type) {
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	case float64:
		if v > 0 {
			return int64(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return n
		}
	}
	return minDiskGB
}

// dataBytes is how much the instance's server holds on disk: the sum of
// pg_database_size over its databases, or of MySQL's tables' data and
// indexes.
func dataBytes(ctx context.Context, e Engine) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	switch e := e.(type) {
	case *Postgres:
		c, err := e.conn(ctx)
		if err != nil {
			return 0, err
		}
		defer c.Close(context.Background())
		var n int64
		if err := c.QueryRow(ctx, "SELECT COALESCE(sum(pg_database_size(datname)), 0)::bigint FROM pg_database").Scan(&n); err != nil {
			return 0, fmt.Errorf("measure %s's databases: %w", e.Instance(), err)
		}
		return n, nil
	case *MySQL:
		db, err := e.db()
		if err != nil {
			return 0, err
		}
		defer db.Close()
		var n sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT CAST(COALESCE(SUM(data_length + index_length), 0) AS SIGNED) FROM information_schema.tables").Scan(&n); err != nil {
			return 0, fmt.Errorf("measure %s's databases: %w", e.Instance(), err)
		}
		return n.Int64, nil
	}
	return 0, Errorf(http.StatusServiceUnavailable, "instance %s has no server to measure", e.Instance())
}

// minimalShrinkGB is the smallest size the instance's disk can shrink to.
func minimalShrinkGB(ctx context.Context, e Engine) (int64, error) {
	n, err := dataBytes(ctx, e)
	if err != nil {
		return 0, err
	}
	const gb = 1 << 30
	return max(minDiskGB, (n+gb-1)/gb), nil
}

// shrinkable is the created primary a shrink is asked of.
func (s *Server) shrinkable(project, name, what string) (*created, error) {
	c, err := s.createdFor(project, name, what)
	if err != nil {
		return nil, err
	}
	s.emu.RLock()
	replica := c.spec.MasterInstanceName != ""
	s.emu.RUnlock()
	if replica {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s is a read replica; shrink its primary's storage, "+
			"then match the replica's to it with resetReplicaSize.", name)
	}
	return c, nil
}

// getDiskShrinkConfig is projects.instances.getDiskShrinkConfig.
func (s *Server) getDiskShrinkConfig(ctx context.Context, project, name string) (any, error) {
	c, err := s.shrinkable(project, name, "projects.instances.getDiskShrinkConfig")
	if err != nil {
		return nil, err
	}
	if err := s.busy(name); err != nil {
		return nil, err
	}
	min, err := minimalShrinkGB(ctx, c.engine)
	if err != nil {
		return nil, err
	}
	s.emu.RLock()
	size := diskSizeGB(c.spec)
	s.emu.RUnlock()
	out := map[string]any{"kind": "sql#getDiskShrinkConfig", "minimalTargetSizeGb": strconv.FormatInt(min, 10)}
	if size <= min {
		out["message"] = fmt.Sprintf("The instance's storage, %d GB, is already the smallest it can be.", size)
	}
	return out, nil
}

// performDiskShrink is projects.instances.performDiskShrink: the
// instance's dataDiskSizeGb becomes targetSizeGb, which must be smaller
// than it and no smaller than getDiskShrinkConfig's minimalTargetSizeGb.
func (s *Server) performDiskShrink(r *http.Request, base, project, name string) (any, error) {
	var body struct {
		TargetSizeGb json.Number `json:"targetSizeGb"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	c, err := s.shrinkable(project, name, "projects.instances.performDiskShrink")
	if err != nil {
		return nil, err
	}
	target, err := body.TargetSizeGb.Int64()
	if body.TargetSizeGb == "" || err != nil || target <= 0 {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: targetSizeGb must be a positive number of GB, got %q.", body.TargetSizeGb)
	}
	if err := s.busy(name); err != nil {
		return nil, err
	}
	min, err := minimalShrinkGB(r.Context(), c.engine)
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	defer s.emu.Unlock()
	if c.state != "RUNNABLE" {
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", c.state)
	}
	size := diskSizeGB(c.spec)
	switch {
	case target >= size:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: targetSizeGb %d is not smaller than the instance's storage, %d GB.", target, size)
	case target < min:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: targetSizeGb %d is smaller than the minimal target size, %d GB, "+
			"that the instance's data needs (getDiskShrinkConfig).", target, min)
	}
	if err := s.setDiskSizeLocked(c, target); err != nil {
		return nil, err
	}
	return s.done(base, project, c.engine, "UPDATE", name), nil
}

// resetReplicaSize is projects.instances.resetReplicaSize: a read
// replica's dataDiskSizeGb becomes its primary's, as after its primary's
// storage was shrunk.
func (s *Server) resetReplicaSize(r *http.Request, base, project, name string) (any, error) {
	if err := decode(r, &struct{}{}); err != nil {
		return nil, err
	}
	c, err := s.createdFor(project, name, "projects.instances.resetReplicaSize")
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	defer s.emu.Unlock()
	master := c.spec.MasterInstanceName
	if master == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s is not a read replica; resetReplicaSize sets a replica's storage to its primary's.", name)
	}
	if c.spec.External != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s replicates from %s, a source representation instance, which has no Cloud SQL storage to match.", name, master)
	}
	if c.state != "RUNNABLE" {
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", c.state)
	}
	size := int64(minDiskGB)
	if p := s.writes.byName[master]; p != nil {
		size = diskSizeGB(p.spec)
	}
	if err := s.setDiskSizeLocked(c, size); err != nil {
		return nil, err
	}
	return s.done(base, project, c.engine, "UPDATE", name), nil
}

// setDiskSizeLocked records a created instance's new dataDiskSizeGb.
func (s *Server) setDiskSizeLocked(c *created, gb int64) error {
	st := make(map[string]any, len(c.spec.Settings)+1)
	for k, v := range c.spec.Settings {
		st[k] = v
	}
	st["dataDiskSizeGb"] = strconv.FormatInt(gb, 10)
	c.spec.Settings = st
	c.spec.SettingsVersion++
	return s.saveLocked()
}
