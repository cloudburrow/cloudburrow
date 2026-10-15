package sqladmin

// Point-in-time recovery (#1330). A created instance with
// settings.backupConfiguration.pointInTimeRecoveryEnabled (PostgreSQL) or
// binaryLogEnabled (MySQL) keeps the engine's own transaction log beside
// its data: PostgreSQL archives its write-ahead log, MySQL keeps its
// binary log. Each base, a pg_basebackup or a mysqldump taken at the
// binary log position it is consistent with, is taken when the instance
// starts and with each of its backups. A clone to cloneContext.pointInTime
// starts from the newest base taken before that time and replays the log
// to it: PostgreSQL to recovery_target_time in a scratch server, MySQL up
// to the first transaction whose commit timestamp is later, both to the
// microsecond. See docs/cloudsql.md.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

// PITRDumper is a Dumper that can recover a server to a point in time.
type PITRDumper interface {
	// PITRBase takes a base that a recovery can start from.
	PITRBase(ctx context.Context, t DumpTarget) error
	// RecoveryWindow is the earliest and latest times the target can be
	// recovered to; ok is false when it has no base yet.
	RecoveryWindow(ctx context.Context, t DumpTarget) (earliest, latest time.Time, ok bool, err error)
	// CopyAt loads src's databases as they were at `at` into dst, a
	// created instance's fresh server, using dir as scratch space.
	CopyAt(ctx context.Context, src, dst DumpTarget, at time.Time, dir string) error
}

// PITREnabled is whether spec keeps its transaction log for point-in-time
// recovery: pointInTimeRecoveryEnabled for PostgreSQL, binaryLogEnabled
// for MySQL, as Cloud SQL names them.
func (s Spec) PITREnabled() bool {
	bc, _ := s.Settings["backupConfiguration"].(map[string]any)
	key := "pointInTimeRecoveryEnabled"
	if s.MySQL() {
		key = "binaryLogEnabled"
	}
	on, _ := bc[key].(bool)
	return on
}

func (s *Server) pitr() PITRDumper {
	if s.backups == nil {
		return nil
	}
	p, _ := s.backups.dumper.(PITRDumper)
	return p
}

// pitrSpec is a created instance's spec when it keeps its log.
func (s *Server) pitrSpec(name string) (Spec, bool) {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return Spec{}, false
	}
	c := s.writes.byName[name]
	if c == nil || !c.spec.PITREnabled() {
		return Spec{}, false
	}
	return c.spec, true
}

// takePITRBase takes a base for a created instance that keeps its log; it
// is a no-op for any other. Errors are the operation's caller's to report.
func (s *Server) takePITRBase(name string) error {
	p := s.pitr()
	spec, ok := s.pitrSpec(name)
	if p == nil || !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	return p.PITRBase(ctx, DumpTarget{Instance: name, MySQL: spec.MySQL(), Created: true})
}

// pitrUnavailable is why an instance cannot be recovered to a time, or
// nil when it can.
func (s *Server) pitrUnavailable(e Engine) error {
	if s.pitr() == nil {
		return unimplemented("point-in-time recovery", "this server was started without a backup store (docs/cloudsql.md)")
	}
	if _, ok := s.pitrSpec(e.Instance()); !ok {
		t := s.target(e)
		key := "pointInTimeRecoveryEnabled"
		if t.MySQL {
			key = "binaryLogEnabled"
		}
		why := ""
		if !t.Created {
			why = "; it is the server `cloudburrow up` runs, which keeps no transaction log: create an instance with it enabled"
		}
		return Errorf(http.StatusBadRequest, "Invalid request: point-in-time recovery is not enabled on instance %s "+
			"(settings.backupConfiguration.%s)%s.", e.Instance(), key, why)
	}
	return nil
}

// latestRecoveryTime is projects.instances.getLatestRecoveryTime.
func (s *Server) latestRecoveryTime(project string, e Engine) (any, error) {
	if err := s.pitrUnavailable(e); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	earliest, latest, ok, err := s.pitr().RecoveryWindow(ctx, s.target(e))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance %s has no base backup to recover from yet.", e.Instance())
	}
	return map[string]any{"kind": "sql#getLatestRecoveryTime",
		"earliestRecoveryTime": earliest.UTC().Format(time.RFC3339Nano),
		"latestRecoveryTime":   latest.UTC().Format(time.RFC3339Nano)}, nil
}

// checkPointInTime parses a clone's pointInTime and checks it is within
// the source's recovery window.
func (s *Server) checkPointInTime(e Engine, raw string) (time.Time, error) {
	if err := s.pitrUnavailable(e); err != nil {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, Errorf(http.StatusBadRequest, "Invalid request: cloneContext.pointInTime %q is not an RFC 3339 time.", raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	earliest, latest, ok, err := s.pitr().RecoveryWindow(ctx, s.target(e))
	if err != nil {
		return time.Time{}, err
	}
	if !ok || at.Before(earliest) || at.After(latest) {
		window := "none yet"
		if ok {
			window = earliest.UTC().Format(time.RFC3339Nano) + " to " + latest.UTC().Format(time.RFC3339Nano)
		}
		return time.Time{}, Errorf(http.StatusBadRequest, "Invalid request: the point in time %s is outside instance %s's "+
			"recovery window (%s).", at.UTC().Format(time.RFC3339Nano), e.Instance(), window)
	}
	return at, nil
}

// copyAt is copyData to a point in time.
func (s *Server) copyAt(src, dst DumpTarget, at time.Time) error {
	dir, err := os.MkdirTemp(s.backups.dir, "pitr-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	if err := s.pitr().CopyAt(ctx, src, dst, at, dir); err != nil {
		return fmt.Errorf("recover %s to %s: %w", src.Instance, at.UTC().Format(time.RFC3339Nano), err)
	}
	return nil
}
