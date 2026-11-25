package main

import (
	"context"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newBackupDRServer serves the Backup and DR Service (#1433) over the
// Cloud SQL Admin API's backups, and lets that API resolve its data
// sources and backups. Nil unless backupdr is enabled with a Cloud SQL
// service.
func newBackupDRServer(cfg config.Config, sql *sqladmin.Server) *backupdr.Server {
	if !backupDREnabled(cfg) || (sql == nil && !backupDRWorkloadsEnabled(cfg)) {
		return nil
	}
	state := ""
	if cfg.Mode == config.ModePersistent {
		state = filepath.Join(cfg.InstanceDir(), "backupdr.json")
	}
	// Without Cloud SQL it protects the other workloads alone (#1524).
	var csql backupdr.CloudSQL
	if sql != nil {
		csql = backupDRSQL{sql}
	}
	srv := backupdr.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.BackupDR)), csql, state)
	if sql != nil {
		sql.SetBackupDR(srv)
	}
	return srv
}

// backupDRWorkloadsEnabled is whether a workload Backup and DR protects
// other than Cloud SQL's is served: Compute Engine, AlloyDB or Filestore.
func backupDRWorkloadsEnabled(cfg config.Config) bool {
	return computeEnabled(cfg) || alloyDBEnabled(cfg) || filestoreEnabled(cfg)
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
	out := backupdr.Run{Status: r.Status, Error: r.Error, EndTime: r.EndTime}
	if ok && r.Status == "SUCCESSFUL" {
		out.SizeBytes = b.s.BackupDRRunSize(id)
	}
	return out, ok
}

func (b backupDRSQL) DeleteRun(id string) error { return b.s.DeleteBackupDRRun(id) }

// backupDREnabled is whether `up` binds the Backup and DR port.
func backupDREnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBackupDR) && (sqlAdminEnabled(cfg) || backupDRWorkloadsEnabled(cfg)) && cfg.Endpoints.BackupDR >= 0
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

// backupDRJobLogger writes the Backup and DR Service's job logs (#1924)
// through api's own WriteLogEntries, so exclusions and sinks apply to
// them as to any write.
func backupDRJobLogger(api *logging.Server) func(backupdr.JobLog) {
	return func(j backupdr.JobLog) {
		payload, err := structpb.NewStruct(j.Payload)
		if err != nil {
			log.Printf("backupdr: job log: %v", err)
			return
		}
		sev := ltype.LogSeverity_INFO
		if j.Severity == "ERROR" {
			sev = ltype.LogSeverity_ERROR
		}
		e := &loggingpb.LogEntry{
			LogName:   "projects/" + j.Project + "/logs/" + strings.ReplaceAll(j.LogID, "/", "%2F"),
			Resource:  &monitoredres.MonitoredResource{Type: j.ResourceType, Labels: j.ResourceLabels},
			Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
			Timestamp: timestamppb.New(j.Time), Severity: sev, InsertId: j.InsertID,
		}
		// The write runs beside the job, not under the service's lock.
		go func() {
			if _, err := api.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}}); err != nil {
				log.Printf("backupdr: job log: %v", err)
			}
		}()
	}
}
