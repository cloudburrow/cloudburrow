package backupdr

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AlloyDB clusters and Filestore instances (#1524). A backup plan of
// either type applies to a resource another service of this instance
// serves (Workload): its backup is that service's own copy of the data,
// the cluster's databases or the share's files, kept under the run's ID
// out of the project's own backups. Each is restored by its own service,
// as Google's guides do: AlloyDB's RestoreCluster with
// backupdrBackupSource or backupdrPitrSource
// (docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/alloydb/alloydb-restore),
// and Filestore's CreateInstance with a share's sourceBackupdrBackup
// (docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/filestore/filestore-instance-restore).

const (
	// AlloyDBResourceType is an AlloyDB for PostgreSQL cluster.
	AlloyDBResourceType = "alloydb.googleapis.com/Cluster"
	// FilestoreResourceType is a Filestore instance.
	FilestoreResourceType = "file.googleapis.com/Instance"
	// workloadRunPrefix starts a workload backup's run ID.
	workloadRunPrefix = "wl-"
)

// WorkloadResource is what a Workload answers of a resource.
type WorkloadResource struct {
	// Name is its canonical name, projects/P/locations/L/clusters/C or
	// projects/P/locations/L/instances/I.
	Name string
	// Props are its dataSourceGcpResource properties
	// (alloyDbClusterDatasourceProperties,
	// filestoreInstanceDatasourceProperties).
	Props map[string]any
	// AssociationProps are its association's properties
	// (alloydbClusterBackupPlanAssociationProperties,
	// filestoreInstanceBackupPlanAssociationProperties).
	AssociationProps map[string]any
}

// Workload is the service that serves a resource type's resources.
type Workload interface {
	// Resource is the resource named, or an error (NOT_FOUND, or a
	// resource of a kind Backup and DR does not protect).
	Resource(name string) (WorkloadResource, error)
	// Protect starts protection by association, whose backups go in
	// dataSource, with the plan's log retention; Unprotect ends it.
	Protect(ctx context.Context, name, association, dataSource string, logRetentionDays int64) error
	Unprotect(ctx context.Context, name string) error
	// Backup takes a backup of the resource kept as runID, and answers
	// when it is taken, with its size and backup properties
	// (alloyDbBackupProperties, filestoreInstanceBackupProperties).
	Backup(ctx context.Context, name, runID string) (int64, map[string]any, error)
	// DeleteBackup deletes the backup kept as runID.
	DeleteBackup(ctx context.Context, runID string) error
}

// workloadRun is an AlloyDB or Filestore backup's run.
type workloadRun struct {
	Kind, Resource, Status, Error, EndTime string
	SizeBytes                              int64
	Props                                  map[string]any `json:",omitempty"`
}

// SetWorkload makes typ's resources protectable through api, called for
// each use; it may answer nil while the service is not served.
func (s *Server) SetWorkload(typ string, api func() Workload) {
	s.mu.Lock()
	if s.workloads == nil {
		s.workloads = map[string]func() Workload{}
	}
	s.workloads[typ] = api
	s.mu.Unlock()
}

func isWorkload(typ string) bool { return typ == AlloyDBResourceType || typ == FilestoreResourceType }

func isCompute(typ string) bool {
	return typ == ComputeInstanceResourceType || typ == ComputeDiskResourceType
}

// workloadName is the service a type's resources belong to, for messages.
func workloadName(typ string) string {
	if typ == AlloyDBResourceType {
		return "AlloyDB for PostgreSQL"
	}
	return "Filestore"
}

func (s *Server) workload(typ string) (Workload, error) {
	s.mu.Lock()
	f := s.workloads[typ]
	s.mu.Unlock()
	if f != nil {
		if w := f(); w != nil {
			return w, nil
		}
	}
	svc := "alloydb"
	if typ == FilestoreResourceType {
		svc = "filestore"
	}
	return nil, precondition("%s is not served by this instance: run up with %s in --services to protect its resources.", workloadName(typ), svc)
}

// workloadResourceName parses an association's resource: the resource name
// or the full one (//alloydb.googleapis.com/…, //file.googleapis.com/…), a
// Filestore zonal instance's also as projects/P/zones/Z/instances/I, as
// the Filestore backup guide writes it.
func workloadResourceName(res, typ string) (string, bool) {
	res = strings.TrimPrefix(strings.TrimPrefix(res, "//alloydb.googleapis.com/"), "//file.googleapis.com/")
	p := strings.Split(res, "/")
	coll := "clusters"
	if typ == FilestoreResourceType {
		coll = "instances"
		if len(p) == 6 && p[2] == "zones" {
			p[2] = "locations"
		}
	}
	if len(p) != 6 || p[0] != "projects" || p[2] != "locations" || p[4] != coll || p[1] == "" || p[3] == "" || p[5] == "" {
		return "", false
	}
	return strings.Join(p, "/"), true
}

// workloadRegion is the region of a resource's location (a Filestore zonal
// instance's zone is in its region).
func workloadRegion(name string) string {
	loc := strings.Split(name, "/")[3]
	if strings.Count(loc, "-") >= 2 {
		return regionOfZone(loc)
	}
	return loc
}

func (s *Server) createWorkloadAssociation(loc, id, planName, resource, typ string) (any, error) {
	rel, ok := workloadResourceName(resource, typ)
	if !ok {
		what, format := "an AlloyDB cluster", "projects/{project}/locations/{location}/clusters/{cluster}"
		if typ == FilestoreResourceType {
			what, format = "a Filestore instance", "projects/{project}/locations/{location}/instances/{instance}"
		}
		return nil, invalid("backupPlanAssociation.resource %q is not %s: the format is %s.", resource, what, format)
	}
	// "The backup plan that you want to use must be in the same region as
	// your AlloyDB cluster"; "For a zonal resource, --resource must be in
	// the same region as that of the --location" (Filestore).
	if l, r := locationOfName(loc+"/x"), workloadRegion(rel); l != r {
		return nil, invalid("backupPlanAssociation %s is in %s: %s is protected by a backup plan association in its region, %s.",
			loc+"/backupPlanAssociations/"+id, l, rel, r)
	}
	w, err := s.workload(typ)
	if err != nil {
		return nil, err
	}
	res, err := w.Resource(rel)
	if err != nil {
		return nil, err
	}
	name := loc + "/backupPlanAssociations/" + id
	project := strings.Split(rel, "/")[1]
	s.mu.Lock()
	p := s.st.Plans[planName]
	switch {
	case s.st.Associations[name] != nil:
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Backup plan association %s already exists.", name)
	case p == nil:
		s.mu.Unlock()
		return nil, invalid("backupPlanAssociation.backupPlan %s does not exist.", planName)
	case p.ResourceType != typ:
		s.mu.Unlock()
		return nil, invalid("Backup plan %s protects %s, not %s.", p.Name, p.ResourceType, typ)
	}
	for _, a := range s.st.Associations {
		if a.ResourceType == typ && a.Resource == rel {
			s.mu.Unlock()
			return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "%s is already protected by %s.", rel, a.Name)
		}
	}
	now := s.stamp()
	var ds *dataSource
	for _, d := range s.st.DataSources {
		if d.Kind == typ && d.Instance == rel && strings.HasPrefix(d.Name, p.Vault+"/") {
			ds = d
		}
	}
	fresh := ds == nil
	if fresh {
		ds = &dataSource{Name: p.Vault + "/dataSources/ds-" + newID()[:20], Project: project, Instance: rel, Kind: typ, CreateTime: now}
	}
	logDays := p.LogRetentionDays
	s.mu.Unlock()
	// The service's side first, outside the lock: it calls back into this
	// one's resolvers.
	if err := w.Protect(context.Background(), rel, name, ds.Name, logDays); err != nil {
		return nil, err
	}
	if res2, err := w.Resource(rel); err == nil {
		res = res2
	}
	s.mu.Lock()
	if s.st.Associations[name] != nil {
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Backup plan association %s already exists.", name)
	}
	if fresh {
		s.st.DataSources[ds.Name] = ds
	}
	ds.Association, ds.UpdateTime = name, now
	ds.Props = res.Props
	ds.Info = Instance{Region: workloadRegion(rel), CreateTime: str(res.Props, "instanceCreateTime")}
	a := &association{Name: name, Plan: p.Name, Resource: rel, ResourceType: typ, DataSource: ds.Name, CreateTime: now, UpdateTime: now,
		LastBackup: map[string]lastBackup{}, Props: res.AssociationProps}
	s.st.Associations[name] = a
	s.planJobLocked("CREATE_BACKUP_PLAN_ASSOCIATION", name, nil, p, a, nil)
	err = s.saveLocked()
	out := s.associationJSON(a)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "create", name, out, "google.cloud.backupdr.v1.BackupPlanAssociation", nil), nil
}

// unprotectWorkload ends a workload resource's protection, after its
// association is deleted. Without s.mu held.
func (s *Server) unprotectWorkload(typ, rel string) {
	w, err := s.workload(typ)
	if err != nil {
		return
	}
	_ = w.Unprotect(context.Background(), rel)
}

// workloadGcpResource is a workload data source's dataSourceGcpResource.
func (d *dataSource) workloadGcpResource() map[string]any {
	host, key := "alloydb.googleapis.com", "alloyDbClusterDatasourceProperties"
	if d.Kind == FilestoreResourceType {
		host, key = "file.googleapis.com", "filestoreInstanceDatasourceProperties"
	}
	return map[string]any{"gcpResourcename": "//" + host + "/" + d.Instance, "location": strings.Split(d.Instance, "/")[3],
		"type": d.Kind, key: d.Props}
}

// gcpResource is any non-Cloud SQL data source's dataSourceGcpResource.
func (d *dataSource) gcpResource() map[string]any {
	if isWorkload(d.Kind) {
		return d.workloadGcpResource()
	}
	return d.computeGcpResource()
}

// sourceRel is the protected resource's relative name.
func (d *dataSource) sourceRel() string {
	switch {
	case isWorkload(d.Kind):
		return d.Instance
	case isCompute(d.Kind):
		return d.computeRel()
	}
	return "projects/" + d.Project + "/instances/" + d.Instance
}

// workloadBackup starts a backup of an AlloyDB or Filestore data source.
// It returns the triggerBackup operation, done when the backup is taken.
func (s *Server) workloadBackup(assocName, planName, ruleID string, onDemand bool, retentionDays int64, labels map[string]string,
	rev string, d *dataSource, vlt *vault) (string, map[string]any, error) {
	loc := assocName[:strings.Index(assocName, "/backupPlanAssociations/")]
	cat := "SCHEDULED_BACKUP"
	if onDemand {
		cat = "ON_DEMAND_BACKUP"
	}
	jobPayload := map[string]any{"jobCategory": cat, "resourceType": jobResourceType(d.Kind), "backupRule": ruleID,
		"sourceResourceName": d.Instance, "sourceResourceId": lastSeg(d.Instance), "sourceResourceLocation": strings.Split(d.Instance, "/")[3],
		"backupVaultName": vaultOf(d.Name), "backupPlanName": planName, "dataSourceName": d.Name}
	w, err := s.workload(d.Kind)
	if err != nil {
		s.mu.Lock()
		s.jobDoneLocked(BackupRestoreJobsLog, d.Project, locationOfName(d.Name), jobPayload, err)
		_ = s.saveLocked()
		s.mu.Unlock()
		return "", nil, err
	}
	s.mu.Lock()
	// "You can only run one backup job at a time": a second one "fails
	// with the Unable to queue the operation error" (AlloyDB's and
	// Filestore's backup guides).
	for _, b := range s.st.Backups {
		if r := s.st.WorkloadRuns[b.RunID]; b.DataSource == d.Name && r != nil && r.Status == "RUNNING" {
			s.mu.Unlock()
			return "", nil, precondition("Unable to queue the operation: backup %s of %s is still running, and only one backup job runs at a time.",
				b.Name, d.Instance)
		}
	}
	s.mu.Unlock()
	now := s.now().UTC()
	runID := workloadRunPrefix + newID()[:20]
	b := &backup{Name: d.Name + "/backups/" + strconv.FormatInt(now.UnixNano(), 10), DataSource: d.Name, RunID: runID,
		Plan: planName, RuleID: ruleID, CreateTime: now.Format(time.RFC3339Nano), Labels: labels, Revision: rev,
		ExpireTime: now.Add(time.Duration(retentionDays) * 24 * time.Hour).Format(time.RFC3339Nano)}
	if vlt != nil && vlt.Inheritance != "MATCH_BACKUP_EXPIRE_TIME" {
		min, _ := parseDuration(vlt.MinRetention)
		if e := now.Add(min); e.Before(now.Add(time.Duration(retentionDays) * 24 * time.Hour)) {
			b.EnforcedEnd = e.Format(time.RFC3339Nano)
		}
	}
	if onDemand {
		b.RuleID = ""
		b.Description = "on demand, rule " + ruleID
		if ruleID == "" {
			b.Description = "on demand, custom retention " + strconv.FormatInt(retentionDays, 10) + " days"
		}
	}
	jobPayload["backupName"] = b.Name
	opName, op := s.newOp(loc, "triggerBackup", assocName)
	s.mu.Lock()
	job := s.startJobLocked(BackupRestoreJobsLog, d.Project, locationOfName(d.Name), jobPayload)
	if s.st.WorkloadRuns == nil {
		s.st.WorkloadRuns = map[string]*workloadRun{}
	}
	s.st.WorkloadRuns[runID] = &workloadRun{Kind: d.Kind, Resource: d.Instance, Status: "RUNNING"}
	s.st.Backups[b.Name] = b
	if ruleID != "" {
		if a := s.st.Associations[assocName]; a != nil {
			a.LastBackup[ruleID] = lastBackup{State: "PENDING", Time: a.LastBackup[ruleID].Time}
		}
	}
	s.ops[opName] = op
	_ = s.saveLocked()
	out := copyMap(op)
	rel, dsName := d.Instance, d.Name
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		size, props, err := w.Backup(ctx, rel, runID)
		var res WorkloadResource
		if err == nil {
			res, _ = w.Resource(rel)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		end := s.stamp()
		if r := s.st.WorkloadRuns[runID]; r != nil {
			r.EndTime, r.Props = end, props
			if err != nil {
				r.Status, r.Error = "FAILED", err.Error()
			} else {
				r.Status, r.SizeBytes = "SUCCESSFUL", size
			}
			job.Payload["incrementalBackupSizeGib"] = float64(r.SizeBytes) / (1 << 30)
		}
		if ds := s.st.DataSources[dsName]; ds != nil && res.Props != nil {
			ds.Props = res.Props
		}
		var jerr error
		if err != nil {
			jerr = errorf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
		}
		s.finishJobLocked(job, jerr)
		if aa := s.st.Associations[assocName]; aa != nil && ruleID != "" {
			lb := aa.LastBackup[ruleID]
			if err != nil {
				lb.State, lb.Error = "FAILED", err.Error()
			} else {
				lb.State, lb.Time, lb.Error = "SUCCEEDED", end, ""
			}
			aa.LastBackup[ruleID] = lb
		}
		var resp map[string]any
		if aa := s.st.Associations[assocName]; aa != nil {
			resp = s.associationJSON(aa)
		}
		s.finishOp(op, resp, "google.cloud.backupdr.v1.BackupPlanAssociation", err)
		_ = s.saveLocked()
	}()
	return opName, out, nil
}

// workloadBackupJSON sets an AlloyDB or Filestore backup's fields on out.
// With s.mu held.
func (s *Server) workloadBackupJSON(b *backup, d *dataSource, out map[string]any) {
	delete(out, "cloudSqlInstanceBackupProperties")
	g := d.workloadGcpResource()
	out["gcpResource"] = map[string]any{"gcpResourcename": g["gcpResourcename"], "location": g["location"], "type": d.Kind}
	r := s.st.WorkloadRuns[b.RunID]
	props := map[string]any{}
	if r != nil {
		for k, v := range r.Props {
			props[k] = v
		}
	}
	if d.Kind == AlloyDBResourceType {
		if r != nil {
			props["storedBytes"] = strconv.FormatInt(r.SizeBytes, 10)
		}
		// One chain per data source: each backup is a full dump here.
		props["chainId"] = lastSeg(d.Name)
		if b.Description != "" {
			props["description"] = b.Description
		}
		out["alloyDbBackupProperties"] = props
		return
	}
	props["sourceInstance"] = d.Instance
	out["filestoreInstanceBackupProperties"] = props
}

// workloadRunLocked is a workload backup's run state. With s.mu held.
func (s *Server) workloadRunLocked(id string) (Run, bool) {
	r := s.st.WorkloadRuns[id]
	if r == nil {
		return Run{}, false
	}
	return Run{Status: r.Status, Error: r.Error, EndTime: r.EndTime, SizeBytes: r.SizeBytes}, true
}

// deleteWorkloadRun deletes a workload backup's data. Without s.mu held.
func (s *Server) deleteWorkloadRun(id string) error {
	s.mu.Lock()
	r := s.st.WorkloadRuns[id]
	delete(s.st.WorkloadRuns, id)
	_ = s.saveLocked()
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	w, err := s.workload(r.Kind)
	if err != nil {
		return nil
	}
	return w.DeleteBackup(context.Background(), id)
}

// workloadRestoreHint is why backups.restore does not restore a workload's
// backup, and how it is restored.
func workloadRestoreHint(name, typ string) error {
	if typ == AlloyDBResourceType {
		return invalid("Backup %s is an AlloyDB cluster's backup, which backups.restore does not restore: AlloyDB restores it into a new cluster, "+
			"with alloydb projects.locations.clusters.restore and backupdrBackupSource.backup %s (gcloud beta alloydb clusters restore --backupdr-backup).", name, name)
	}
	return invalid("Backup %s is a Filestore instance's backup, which backups.restore does not restore: Filestore restores it into a new instance, "+
		"with file projects.locations.instances.create and fileShares[].sourceBackupdrBackup %s (gcloud filestore instances create --file-share=...,source-backupdr-backup=...).", name, name)
}

// workloadBackupRun resolves a backup of typ for its service's restore:
// its run ID and the resource it was taken of.
func (s *Server) workloadBackupRun(name, typ string) (string, string, error) {
	if err := s.checkVaultKeyFor(name); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Backups[name]
	if b == nil {
		return "", "", notFound("The Backup and DR backup %s does not exist.", name)
	}
	d := s.st.DataSources[b.DataSource]
	if d == nil || d.Kind != typ {
		kind := CloudSQLResourceType
		if d != nil && d.Kind != "" {
			kind = d.Kind
		}
		return "", "", invalid("The Backup and DR backup %s is a backup of a %s, not of a %s.", name, kind, typ)
	}
	r := s.st.WorkloadRuns[b.RunID]
	if r == nil || r.Status != "SUCCESSFUL" {
		return "", "", precondition("Backup %s is not ACTIVE: only a backup that was taken can be restored.", name)
	}
	return b.RunID, d.Instance, nil
}

// AlloyDBBackup is the run an AlloyDB cluster's backup holds and the
// cluster it was taken of, for RestoreCluster's backupdrBackupSource.
func (s *Server) AlloyDBBackup(name string) (string, string, error) {
	return s.workloadBackupRun(name, AlloyDBResourceType)
}

// FilestoreBackup is the run a Filestore instance's backup holds, for a
// share's sourceBackupdrBackup.
func (s *Server) FilestoreBackup(name string) (string, error) {
	id, _, err := s.workloadBackupRun(name, FilestoreResourceType)
	return id, err
}

// AlloyDBDataSource is the cluster a data source protects, for
// RestoreCluster's backupdrPitrSource.
func (s *Server) AlloyDBDataSource(name string) (string, error) {
	if err := s.checkVaultKeyFor(name + "/x"); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.st.DataSources[name]
	if d == nil {
		return "", notFound("The Backup and DR data source %s does not exist.", name)
	}
	if d.Kind != AlloyDBResourceType {
		return "", invalid("The Backup and DR data source %s protects a %s, not an AlloyDB cluster.", name, kindOrSQL(d.Kind))
	}
	return d.Instance, nil
}

func kindOrSQL(k string) string {
	if k == "" {
		return CloudSQLResourceType
	}
	return k
}

// HTTPStatus and GoogleStatus are the error's statuses, for the services
// that restore through this one.
func (e *apiError) HTTPStatus() int      { return e.code }
func (e *apiError) GoogleStatus() string { return e.status }

// planLimits are the retention ranges, in days, of each resource type's
// scheduled rules by recurrence, as the backup plans overview lists them
// (docs.cloud.google.com/backup-disaster-recovery/docs/concepts/backup-plan,
// read 2026-11-24); YEARLY is not listed there, and is held to the
// rules' general bound.
var planLimits = map[string]map[string][2]int64{
	ComputeInstanceResourceType: {"HOURLY": {1, 366}, "DAILY": {1, 1827}, "WEEKLY": {7, 1827}, "MONTHLY": {30, 3653}},
	ComputeDiskResourceType:     {"HOURLY": {1, 90}, "DAILY": {1, 186}, "WEEKLY": {7, 366}, "MONTHLY": {30, 732}},
	CloudSQLResourceType:        {"HOURLY": {1, 90}, "DAILY": {1, 186}, "WEEKLY": {7, 366}, "MONTHLY": {30, 732}},
	AlloyDBResourceType:         {"HOURLY": {1, 90}, "DAILY": {1, 186}, "WEEKLY": {7, 365}, "MONTHLY": {30, 365}},
	FilestoreResourceType:       {"HOURLY": {1, 90}, "DAILY": {1, 186}, "WEEKLY": {7, 366}, "MONTHLY": {30, 732}},
}

// checkRuleRetention checks a rule's retention against its type's range.
func checkRuleRetention(i int, ru rule, typ string) error {
	rt, _ := ru.Schedule["recurrenceType"].(string)
	lim, ok := planLimits[typ][rt]
	if !ok {
		return nil
	}
	if ru.RetentionDays < lim[0] || ru.RetentionDays > lim[1] {
		return invalid("backupPlan.backupRules[%d].backupRetentionDays %d is not supported: an %s rule for %s keeps backups %d to %d days.",
			i, ru.RetentionDays, rt, typ, lim[0], lim[1])
	}
	return nil
}

// logRetentionTypes are the types a plan's logRetentionDays applies to:
// "valid only for AlloyDB and Cloud SQL workloads".
func logRetentionType(typ string) bool {
	return typ == CloudSQLResourceType || typ == AlloyDBResourceType
}

// reprotectLocked gives the resources plan's associations protect the
// plan's current log retention, in the background: their services call
// back into this one. With s.mu held.
func (s *Server) reprotectLocked(planName string) {
	p := s.st.Plans[planName]
	if p == nil || !isWorkload(p.ResourceType) {
		return
	}
	type job struct{ typ, rel, assoc, ds string }
	var jobs []job
	for _, a := range s.st.Associations {
		if a.Plan == planName {
			jobs = append(jobs, job{a.ResourceType, a.Resource, a.Name, a.DataSource})
		}
	}
	logDays := p.LogRetentionDays
	go func() {
		for _, j := range jobs {
			if w, err := s.workload(j.typ); err == nil {
				_ = w.Protect(context.Background(), j.rel, j.assoc, j.ds, logDays)
			}
		}
	}()
}
