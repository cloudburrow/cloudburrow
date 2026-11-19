package backupdr

// Job logs (#1924). Google's Backup and DR Service writes each backup and
// restore job to Cloud Logging as backupdr.googleapis.com/bdr_backup_restore_jobs
// on the monitored resource backupdr.googleapis.com/BackupDRProject, and each
// backup plan and backup plan association job as
// backupdr.googleapis.com/bdr_backup_plan_jobs on backupdr.googleapis.com/BackupPlan
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/monitor-reports/backupvault-backup-recovery-jobs,
// https://docs.cloud.google.com/logging/docs/api/platform-logs). A job is
// logged when it starts (RUNNING) and again when it ends (SUCCESSFUL or
// FAILED), with the same jobId. The service keeps its jobs too, 14 days
// of them, the longest time range of the console's Jobs page.

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Log IDs and monitored resource types of the job logs.
const (
	BackupRestoreJobsLog = "backupdr.googleapis.com/bdr_backup_restore_jobs"
	BackupPlanJobsLog    = "backupdr.googleapis.com/bdr_backup_plan_jobs"
	projectResourceType  = "backupdr.googleapis.com/BackupDRProject"
	planResourceType     = "backupdr.googleapis.com/BackupPlan"
	backupRestorePayload = "type.googleapis.com/google.cloud.backupdr.logging.v1.BDRBackupRestoreJobLog"
	backupPlanPayload    = "type.googleapis.com/google.cloud.backupdr.logging.v1.BDRBackupPlanJobLog"
	// jobsKept is how long the service keeps a job: the Jobs page's
	// longest time range.
	jobsKept = 14 * 24 * time.Hour
)

// Job is one backup, restore or system job, as it is logged.
type Job struct {
	// Log is BackupRestoreJobsLog or BackupPlanJobsLog.
	Log      string `json:"log"`
	Project  string `json:"project"`
	Location string `json:"location"`
	// Payload is the log entry's jsonPayload, in Google's field names.
	Payload map[string]any `json:"payload"`
	// Start and End are RFC 3339; End is "" while it runs.
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

// ID is the job's jobId.
func (j Job) ID() string { s, _ := j.Payload["jobId"].(string); return s }

// Status is the job's jobStatus: RUNNING, SUCCESSFUL, FAILED or SKIPPED.
func (j Job) Status() string { s, _ := j.Payload["jobStatus"].(string); return s }

// Field is a payload field as a string.
func (j Job) Field(name string) string { s, _ := j.Payload[name].(string); return s }

// JobLog is a job's log entry, for Cloud Logging.
type JobLog struct {
	Project, LogID, ResourceType string
	ResourceLabels               map[string]string
	Severity                     string
	Time                         time.Time
	Payload                      map[string]any
	// InsertID is the same for a job's RUNNING and final entries' halves:
	// jobId plus its status.
	InsertID string
}

// UseJobLogs writes each job's log entries through write, the instance's
// Cloud Logging, when it is served.
func (s *Server) UseJobLogs(write func(JobLog)) {
	s.mu.Lock()
	s.jobLog = write
	s.mu.Unlock()
}

// startJob records a job as RUNNING and logs it, with s.mu held. payload
// holds the job's own fields; jobId, jobStatus and startTime are added.
func (s *Server) startJobLocked(log, project, location string, payload map[string]any) *Job {
	if s.st.Jobs == nil {
		s.st.Jobs = map[string]*Job{}
	}
	now := s.now().UTC()
	j := &Job{Log: log, Project: project, Location: location, Payload: payload, Start: now.Format(time.RFC3339Nano)}
	payload["jobId"] = newUUID()
	payload["jobStatus"] = "RUNNING"
	payload["startTime"] = j.Start
	s.st.Jobs[j.ID()] = j
	s.pruneJobsLocked(now)
	s.emitLocked(j)
	return j
}

// finishJobLocked ends a job, SUCCESSFUL or FAILED with err, and logs it,
// with s.mu held.
func (s *Server) finishJobLocked(j *Job, err error) {
	if j == nil {
		return
	}
	j.End = s.now().UTC().Format(time.RFC3339Nano)
	j.Payload["endTime"] = j.End
	j.Payload["jobStatus"] = "SUCCESSFUL"
	if err != nil {
		code, typ := 13, "INTERNAL"
		var ae *apiError
		if errors.As(err, &ae) {
			code, typ = grpcCode(ae.code), ae.status
		}
		j.Payload["jobStatus"] = "FAILED"
		j.Payload["errorCode"] = float64(code)
		j.Payload["errorType"] = typ
		j.Payload["errorMessage"] = err.Error()
	}
	if t, e := time.Parse(time.RFC3339Nano, j.Start); e == nil {
		j.Payload["duration"] = durationJSON(s.now().Sub(t))
	}
	s.emitLocked(j)
}

// jobDone records a job that ran to its end at once.
func (s *Server) jobDoneLocked(log, project, location string, payload map[string]any, err error) {
	s.finishJobLocked(s.startJobLocked(log, project, location, payload), err)
}

func durationJSON(d time.Duration) string {
	return strconv.FormatFloat(d.Round(time.Millisecond).Seconds(), 'f', -1, 64) + "s"
}

func (s *Server) emitLocked(j *Job) {
	if s.jobLog == nil {
		return
	}
	labels := map[string]string{"resource_container": "projects/" + j.Project, "location": j.Location}
	typ, at := projectResourceType, backupRestorePayload
	if j.Log == BackupPlanJobsLog {
		typ, at = planResourceType, backupPlanPayload
		labels["backup_plan_id"] = lastSegment(j.Field("backupPlanName"))
	}
	p := copyMap(j.Payload)
	p["@type"] = at
	sev := "INFO"
	if j.Status() == "FAILED" {
		sev = "ERROR"
	}
	when, _ := time.Parse(time.RFC3339Nano, j.Start)
	if j.End != "" {
		when, _ = time.Parse(time.RFC3339Nano, j.End)
	}
	s.jobLog(JobLog{Project: j.Project, LogID: j.Log, ResourceType: typ, ResourceLabels: labels, Severity: sev, Time: when,
		Payload: p, InsertID: j.ID() + "-" + strings.ToLower(j.Status())})
}

// pruneJobsLocked drops the jobs older than jobsKept.
func (s *Server) pruneJobsLocked(now time.Time) {
	for id, j := range s.st.Jobs {
		if t, err := time.Parse(time.RFC3339Nano, j.Start); err == nil && now.Sub(t) > jobsKept {
			delete(s.st.Jobs, id)
		}
	}
}

// Jobs are project's jobs that started at or after since, newest first.
func (s *Server) Jobs(project string, since time.Time) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Job
	for _, j := range s.st.Jobs {
		if j.Project != project {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, j.Start); err != nil || t.Before(since) {
			continue
		}
		cp := *j
		cp.Payload = copyMap(j.Payload)
		out = append(out, cp)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Start > out[b].Start })
	return out
}

// Failure is a row of the Jobs page's Current failures: the resources
// whose last backup job failed, by error type.
type Failure struct {
	ErrorType, Project, ErrorMessage string
	ErrorCode                        int
	Count                            int
	ResourceIDs                      []string
}

// CurrentFailures counts, per error type, project's resources whose last
// backup job failed: Google's "total number of resources in the source
// project for which the last backup job failed due to a particular error
// type".
func (s *Server) CurrentFailures(project string) []Failure {
	last := map[string]Job{}
	for _, j := range s.Jobs(project, time.Time{}) {
		switch j.Field("jobCategory") {
		case "SCHEDULED_BACKUP", "ON_DEMAND_BACKUP":
		default:
			continue
		}
		if j.Status() == "RUNNING" {
			continue
		}
		res := j.Field("sourceResourceName")
		if _, seen := last[res]; !seen {
			last[res] = j // newest first
		}
	}
	by := map[string]*Failure{}
	for _, res := range sortedKeys(last) {
		j := last[res]
		if j.Status() != "FAILED" {
			continue
		}
		f := by[j.Field("errorType")]
		if f == nil {
			code, _ := j.Payload["errorCode"].(float64)
			f = &Failure{ErrorType: j.Field("errorType"), Project: project, ErrorMessage: j.Field("errorMessage"), ErrorCode: int(code)}
			by[f.ErrorType] = f
		}
		f.Count++
		f.ResourceIDs = append(f.ResourceIDs, j.Field("sourceResourceId"))
	}
	out := []Failure{}
	for _, k := range sortedKeys(by) {
		out = append(out, *by[k])
	}
	return out
}

// RestoreStarted logs a restore job (Cloud SQL's instances.restoreBackup
// with backupdrBackup, category RESTORE) or a point-in-time restore
// (instances.pointInTimeRestore of a data source, category PITR) as
// RUNNING, and returns its job ID.
func (s *Server) RestoreStarted(category, source, restoreProject, restoreInstance, recoveryPoint string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dsName, backupName := source, ""
	if i := strings.Index(source, "/backups/"); i >= 0 {
		dsName, backupName = source[:i], source
	}
	payload := map[string]any{"jobCategory": category, "resourceType": "Cloud SQL",
		"restoreResourceName": "projects/" + restoreProject + "/instances/" + restoreInstance, "dataSourceName": dsName}
	if backupName != "" {
		payload["backupName"] = backupName
	}
	if recoveryPoint != "" {
		payload["recoveryPointTime"] = recoveryPoint
	}
	loc := ""
	if p := strings.Split(source, "/"); len(p) > 5 {
		loc = p[3]
		payload["backupVaultName"] = strings.Join(p[:6], "/")
	}
	if d := s.st.DataSources[dsName]; d != nil {
		payload["sourceResourceName"] = "projects/" + d.Project + "/instances/" + d.Instance
		payload["sourceResourceId"] = d.Instance
		payload["sourceResourceLocation"] = d.Info.Region
	}
	// The job is the restore's project's, where it runs.
	j := s.startJobLocked(BackupRestoreJobsLog, restoreProject, loc, payload)
	_ = s.saveLocked()
	return j.ID()
}

// RestoreFinished logs a restore job's end.
func (s *Server) RestoreFinished(jobID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.st.Jobs[jobID]
	if j == nil {
		return
	}
	if err != nil {
		err = errorf(500, "INTERNAL", "%s", err.Error())
	}
	s.finishJobLocked(j, err)
	_ = s.saveLocked()
}

// planJobLocked logs a backup plan or backup plan association job (create,
// update or delete), with s.mu held.
func (s *Server) planJobLocked(category, name string, prev, next *plan, a *association, err error) {
	p := strings.Split(name, "/")
	if len(p) < 4 {
		return
	}
	payload := map[string]any{"jobCategory": category, "resourceType": "Cloud SQL"}
	pl := next
	if pl == nil {
		pl = prev
	}
	if a != nil {
		project, instance, _ := sqlResource(a.Resource)
		payload["backupPlanAssociationName"] = a.Name
		payload["sourceResourceName"] = "projects/" + project + "/instances/" + instance
		payload["sourceResourceId"] = instance
		if d := s.st.DataSources[a.DataSource]; d != nil {
			payload["sourceResourceLocation"] = d.Info.Region
		}
	}
	if prev != nil {
		payload["previousBackupPlanName"] = prev.Name
		payload["previousBackupPlanRevisionId"] = prev.revisionID()
		payload["previousBackupPlanRevisionName"] = prev.revisionName()
		if a == nil {
			payload["previousBackupPlanRules"] = rulesJSON(prev.Rules)
		}
	}
	if next != nil {
		payload["newBackupPlanName"] = next.Name
		payload["newBackupPlanRevisionId"] = next.revisionID()
		payload["newBackupPlanRevisionName"] = next.revisionName()
		if a == nil {
			payload["revisedBackupPlanRules"] = rulesJSON(next.Rules)
		}
	}
	if pl != nil {
		payload["backupPlanName"] = pl.Name
		if a == nil {
			n := 0
			for _, x := range s.st.Associations {
				if x.Plan == pl.Name {
					n++
				}
			}
			payload["workloadsAffected"] = float64(n)
		}
	}
	s.jobDoneLocked(BackupPlanJobsLog, p[1], p[3], payload, err)
}

func rulesJSON(rules []rule) []any {
	out := []any{}
	for _, r := range rules {
		out = append(out, map[string]any{"ruleId": r.RuleID, "backupRetentionDays": float64(r.RetentionDays), "standardSchedule": r.Schedule})
	}
	return out
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// newUUID is a random version 4 UUID, as Google's jobIds are.
func newUUID() string {
	h := newID()
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
