package main

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// newBackupDRServer serves the Backup and DR Service (#1433) over the
// Cloud SQL Admin API's backups, and lets that API resolve its data
// sources and backups. Nil unless backupdr is enabled with a Cloud SQL
// service.
func newBackupDRServer(cfg config.Config, sql *sqladmin.Server) *backupdr.Server {
	if !serviceEnabled(cfg, config.ServiceBackupDR) || cfg.Endpoints.BackupDR < 0 || sql == nil {
		return nil
	}
	state := ""
	if cfg.Mode == config.ModePersistent {
		state = filepath.Join(cfg.InstanceDir(), "backupdr.json")
	}
	srv := backupdr.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.BackupDR)), backupDRSQL{sql}, state)
	sql.SetBackupDR(srv)
	return srv
}

// backupDRSQL is the Cloud SQL Admin API as the Backup and DR Service
// sees it.
type backupDRSQL struct{ s *sqladmin.Server }

func (b backupDRSQL) Instance(project, instance string) (backupdr.Instance, bool) {
	i, ok := b.s.LookupInstance(project, instance)
	return backupdr.Instance{DatabaseVersion: i.DatabaseVersion, Tier: i.Tier, CreateTime: i.CreateTime, Region: i.Region,
		BackupEnabled: i.BackupEnabled}, ok
}

func (b backupDRSQL) Instances(project string) []string { return b.s.InstanceNames(project) }

func (b backupDRSQL) Backup(project, instance, description string) (string, <-chan struct{}, error) {
	return b.s.TakeBackupDRBackup(project, instance, description)
}

func (b backupDRSQL) Run(id string) (backupdr.Run, bool) {
	r, ok := b.s.BackupDRRun(id)
	return backupdr.Run{Status: r.Status, Error: r.Error, EndTime: r.EndTime}, ok
}

func (b backupDRSQL) DeleteRun(id string) error { return b.s.DeleteBackupDRRun(id) }

// backupDREnabled is whether `up` binds the Backup and DR port.
func backupDREnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBackupDR) && sqlAdminEnabled(cfg) && cfg.Endpoints.BackupDR >= 0
}

// backupDRHierarchy is backupdr.Hierarchy over Resource Manager's registry
// (#1536): the active projects whose ancestry (Registry.Ancestry, as Cloud
// Logging and Autokey read it) passes through the folder or reaches the
// organization. A project with no parent is under neither.
type backupDRHierarchy struct{ reg *resourcemanager.Registry }

func (h backupDRHierarchy) ProjectsUnder(parent string) ([]string, error) {
	kind, id, _ := strings.Cut(parent, "/")
	switch {
	case parent == resourcemanager.OrganizationName:
	case kind == "folders" && id != "":
		f, err := h.reg.Folder(parent)
		if err != nil || f.State != resourcemanager.StateActive {
			return nil, backupdr.ErrNoParent
		}
	default:
		return nil, backupdr.ErrNoParent
	}
	want := strings.TrimSuffix(kind, "s")
	projects, err := h.reg.Active()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range projects {
		chain, err := h.reg.Ancestry(p.ProjectID)
		if err != nil {
			return nil, err
		}
		for _, a := range chain[1:] {
			if a.Type == want && a.ID == id {
				out = append(out, p.ProjectID)
				break
			}
		}
	}
	return out, nil
}
