package main

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

// Backup and DR of AlloyDB clusters and Filestore instances (#1524): the
// Backup and DR Service reaches each through a backupdr.Workload, and each
// resolves the vault's backups through the Backup and DR Service, in
// process, as Cloud SQL's and Compute Engine's integrations do. The
// services never import one another (TestServicesDoNotImportOtherServices).

// statusErr is another service's error as backupdr reads it: its HTTP and
// Google statuses.
type statusErr struct {
	code   int
	status string
	msg    string
}

func (e statusErr) Error() string        { return e.msg }
func (e statusErr) HTTPStatus() int      { return e.code }
func (e statusErr) GoogleStatus() string { return e.status }

// googleStatusName is a gRPC code's google.rpc.Code name, NOT_FOUND for
// NotFound.
func googleStatusName(c codes.Code) string {
	var b strings.Builder
	for i, r := range c.String() {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// toBackupDR is a service's error as the Backup and DR Service answers it.
func toBackupDR(err error) error {
	if err == nil {
		return nil
	}
	var e *apierror.Error
	if errors.As(err, &e) {
		return statusErr{code: e.HTTPStatus(), status: googleStatusName(e.Code), msg: e.Message}
	}
	return err
}

// fromBackupDR is a Backup and DR error as the restoring service answers it.
func fromBackupDR(err error) error {
	if err == nil {
		return nil
	}
	var se interface{ GoogleStatus() string }
	if !errors.As(err, &se) {
		return apierror.FailedPrecondition("%v", err)
	}
	switch se.GoogleStatus() {
	case "NOT_FOUND":
		return apierror.NotFound("%v", err)
	case "INVALID_ARGUMENT":
		return apierror.InvalidArgument("%v", err)
	}
	return apierror.FailedPrecondition("%v", err)
}

// backupDRAlloyDB is AlloyDB as a Backup and DR workload.
type backupDRAlloyDB struct{ api *alloydb.Server }

func (w backupDRAlloyDB) Resource(name string) (backupdr.WorkloadResource, error) {
	c, err := w.api.BackupDRCluster(name)
	if err != nil {
		return backupdr.WorkloadResource{}, toBackupDR(err)
	}
	props := map[string]any{"name": c.Name, "clusterUid": c.UID}
	if len(c.PITRWindows) > 0 {
		var ws []any
		for _, pw := range c.PITRWindows {
			ws = append(ws, pw)
		}
		props["pitrWindows"] = ws
	}
	return backupdr.WorkloadResource{Name: c.Name, Props: props, AssociationProps: map[string]any{"clusterUid": c.UID}}, nil
}

func (w backupDRAlloyDB) Protect(ctx context.Context, name, association, dataSource string, logDays int64) error {
	return toBackupDR(w.api.ProtectCluster(ctx, name, association, dataSource, logDays))
}

func (w backupDRAlloyDB) Unprotect(ctx context.Context, name string) error {
	return toBackupDR(w.api.UnprotectCluster(ctx, name))
}

func (w backupDRAlloyDB) Backup(ctx context.Context, name, runID string) (int64, map[string]any, error) {
	size, version, err := w.api.BackupDRBackup(ctx, name, runID)
	if err != nil {
		return 0, nil, toBackupDR(err)
	}
	return size, map[string]any{"databaseVersion": version}, nil
}

func (w backupDRAlloyDB) DeleteBackup(_ context.Context, runID string) error {
	return w.api.DeleteBackupDRBackup(runID)
}

// backupDRFilestore is Filestore as a Backup and DR workload.
type backupDRFilestore struct{ api *filestore.Server }

func (w backupDRFilestore) Resource(name string) (backupdr.WorkloadResource, error) {
	in, err := w.api.BackupDRInstance(name)
	if err != nil {
		return backupdr.WorkloadResource{}, toBackupDR(err)
	}
	return backupdr.WorkloadResource{Name: in.Name, Props: map[string]any{"name": in.Name, "instanceCreateTime": in.CreateTime},
		AssociationProps: map[string]any{"instanceCreateTime": in.CreateTime}}, nil
}

func (w backupDRFilestore) Protect(context.Context, string, string, string, int64) error { return nil }

func (w backupDRFilestore) Unprotect(context.Context, string) error { return nil }

func (w backupDRFilestore) Backup(ctx context.Context, name, runID string) (int64, map[string]any, error) {
	size, err := w.api.BackupDRBackup(ctx, name, runID)
	if err != nil {
		return 0, nil, toBackupDR(err)
	}
	return size, nil, nil
}

func (w backupDRFilestore) DeleteBackup(ctx context.Context, runID string) error {
	return w.api.DeleteBackupDRBackup(ctx, runID)
}

// backupDRRestores is the Backup and DR Service as AlloyDB's and
// Filestore's restores see it.
type backupDRRestores struct{ s *backupdr.Server }

func (b backupDRRestores) AlloyDBBackup(name string) (string, string, error) {
	id, cluster, err := b.s.AlloyDBBackup(name)
	return id, cluster, fromBackupDR(err)
}

func (b backupDRRestores) AlloyDBDataSource(name string) (string, error) {
	c, err := b.s.AlloyDBDataSource(name)
	return c, fromBackupDR(err)
}

func (b backupDRRestores) FilestoreBackup(name string) (string, error) {
	id, err := b.s.FilestoreBackup(name)
	return id, fromBackupDR(err)
}

func (b backupDRRestores) RestoreStarted(category, source, restoreProject, restoreResource, recoveryPoint string) string {
	return b.s.RestoreStarted(category, source, restoreProject, restoreResource, recoveryPoint)
}

func (b backupDRRestores) RestoreFinished(jobID string, err error) { b.s.RestoreFinished(jobID, err) }

// useBackupDRWorkloads makes AlloyDB's clusters and Filestore's instances
// protectable by srv, and lets them restore its backups, once each is
// started.
func useBackupDRWorkloads(srv *backupdr.Server, adb *alloyDBService, fs *filestoreService) {
	if srv == nil {
		return
	}
	restores := backupDRRestores{s: srv}
	if adb != nil {
		adb.backupDR = restores
		srv.SetWorkload(backupdr.AlloyDBResourceType, func() backupdr.Workload {
			if api := adb.API(); api != nil {
				return backupDRAlloyDB{api: api}
			}
			return nil
		})
	}
	if fs != nil {
		fs.backupDR = restores
		srv.SetWorkload(backupdr.FilestoreResourceType, func() backupdr.Workload {
			if api := fs.API(); api != nil {
				return backupDRFilestore{api: api}
			}
			return nil
		})
	}
}
