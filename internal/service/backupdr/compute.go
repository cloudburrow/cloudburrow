package backupdr

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Compute Engine instances and disks (#2075). A backup plan for either
// type applies to a VM or a disk the instance's Compute Engine API serves;
// its backup is a snapshot of each disk (the VM's every attached disk, or
// the one disk) taken through that API and kept out of the project's own
// snapshots, with the VM's configuration as it was. backups.restore makes
// a new VM or disk from it, in the target project and zone, with the
// overrides Google documents
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/compute/compute-instance-restore,
// https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/compute/disk-restore).
// Neither page documents an in-place restore of a vaulted backup: a
// restore always makes a new resource.

const (
	// ComputeInstanceResourceType is a Compute Engine VM instance.
	ComputeInstanceResourceType = "compute.googleapis.com/Instance"
	// ComputeDiskResourceType is a zonal Persistent Disk.
	ComputeDiskResourceType = "compute.googleapis.com/Disk"
	// ComputeRegionDiskResourceType is a regional Persistent Disk or
	// Hyperdisk Balanced High Availability volume (#2206): "Regional
	// Persistent Disk and Hyperdisk volumes (compute.googleapis.com/RegionDisk)"
	// (restore-data/automated-restore-verification).
	ComputeRegionDiskResourceType = "compute.googleapis.com/RegionDisk"
	// computeRunPrefix starts a Compute Engine backup's run ID, so the
	// runs it shares with Cloud SQL's are told apart.
	computeRunPrefix = "compute-"
	// VaultSnapshotQuery is the query parameter that shows the compute API
	// the snapshots a vault holds, which its own snapshots.list leaves out.
	VaultSnapshotQuery = "_backupdr"
)

// Compute is the Compute Engine API, called in process: method, the path
// after /compute/v1/projects/{project}/, query and JSON body, answered
// with relative links (compute.Server.Call).
type Compute interface {
	Call(method, project, path string, query map[string][]string, body map[string]any) (map[string]any, error)
}

// SetCompute makes Compute Engine instances and disks protectable; api is
// called for each use, and may answer nil while Compute is not served.
func (s *Server) SetCompute(api func() Compute) {
	s.mu.Lock()
	s.compute = api
	s.mu.Unlock()
}

// servedType is whether a plan or association of typ is served.
func servedType(typ string) bool {
	return typ == CloudSQLResourceType || isCompute(typ) || isWorkload(typ)
}

func (s *Server) computeAPI() (Compute, error) {
	s.mu.Lock()
	f := s.compute
	s.mu.Unlock()
	if f != nil {
		if c := f(); c != nil {
			return c, nil
		}
	}
	return nil, precondition("Compute Engine is not served by this instance: run up with compute in --services to protect its VMs and disks.")
}

// computeError is a Compute Engine error with its HTTP status kept.
func computeError(err error) error {
	var st interface{ Status() (int, string) }
	if errors.As(err, &st) {
		code, _ := st.Status()
		return &apiError{code: code, status: googleStatus(code), msg: err.Error()}
	}
	return err
}

func googleStatus(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "ALREADY_EXISTS"
	case http.StatusPreconditionFailed:
		return "FAILED_PRECONDITION"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusNotImplemented:
		return "UNIMPLEMENTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

// collection is the compute collection of a resource type.
func collection(typ string) string {
	if isDiskType(typ) {
		return "disks"
	}
	return "instances"
}

// isDiskType is whether typ is a zonal or a regional disk.
func isDiskType(typ string) bool {
	return typ == ComputeDiskResourceType || typ == ComputeRegionDiskResourceType
}

// scopeKind is the compute scope collection of a resource type: zones, or
// regions for a regional disk.
func scopeKind(typ string) string {
	if typ == ComputeRegionDiskResourceType {
		return "regions"
	}
	return "zones"
}

// computeResource parses an association's resource, a resource name
// ("projects/my-project/zones/us-central1-a/instances/my-instance",
// projects/P/regions/R/disks/D for a regional disk) or a full one
// (//compute.googleapis.com/... or https://www.googleapis.com/compute/v1/...);
// zone is the region of a regional disk.
func computeResource(res, typ string) (project, zone, name string, ok bool) {
	for _, p := range []string{"//compute.googleapis.com/", "https://www.googleapis.com/compute/v1/", "https://compute.googleapis.com/compute/v1/"} {
		res = strings.TrimPrefix(res, p)
	}
	p := strings.Split(res, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != scopeKind(typ) || p[4] != collection(typ) || p[1] == "" || p[3] == "" || p[5] == "" {
		return "", "", "", false
	}
	return p[1], p[3], p[5], true
}

// regionOfZone is us-central1 for us-central1-a, and a region itself.
func regionOfZone(zone string) string {
	if strings.Count(zone, "-") < 2 {
		return zone
	}
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

func lastSeg(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func gb(v any) int64 {
	switch t := v.(type) {
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case float64:
		return int64(t)
	}
	return 0
}

// computeProps are a VM's or disk's dataSourceGcpResource properties
// (ComputeInstanceDataSourceProperties, DiskDataSourceProperties).
func computeProps(typ, rel string, o map[string]any) map[string]any {
	if isDiskType(typ) {
		return map[string]any{"name": rel, "description": str(o, "description"), "type": str(o, "type"),
			"sizeGb": strconv.FormatInt(gb(o["sizeGb"]), 10)}
	}
	disks, _ := o["disks"].([]any)
	var total int64
	for _, e := range disks {
		ad, _ := e.(map[string]any)
		total += gb(ad["diskSizeGb"])
	}
	return map[string]any{"name": rel, "description": str(o, "description"), "machineType": str(o, "machineType"),
		"totalDiskCount": strconv.Itoa(len(disks)), "totalDiskSizeGb": strconv.FormatInt(total, 10)}
}

func (d *dataSource) computeRel() string {
	return "projects/" + d.Project + "/" + d.computePath()
}

// computePath is the resource's path under its project:
// zones/Z/instances/I, zones/Z/disks/D or regions/R/disks/D.
func (d *dataSource) computePath() string {
	return scopeKind(d.Kind) + "/" + d.Zone + "/" + collection(d.Kind) + "/" + d.Instance
}

// computeGcpResource is a Compute Engine data source's dataSourceGcpResource.
func (d *dataSource) computeGcpResource() map[string]any {
	out := map[string]any{"gcpResourcename": "//compute.googleapis.com/" + d.computeRel(), "location": d.Zone, "type": d.Kind}
	key := "computeInstanceDatasourceProperties"
	if isDiskType(d.Kind) {
		key = "diskDatasourceProperties"
	}
	out[key] = d.Props
	return out
}

// computeRun is a Compute Engine backup: its disks' snapshots and, for a
// VM, its configuration when it was taken.
type computeRun struct {
	Status, Error, EndTime string
	SizeBytes              int64
	Project, Zone          string
	// Instance is the VM as Compute Engine answered it (relative links);
	// nil for a disk's backup.
	Instance map[string]any `json:",omitempty"`
	Disks    []computeDisk
}

// computeDisk is one disk of a backup.
type computeDisk struct {
	DeviceName, Snapshot, Source, Type, Description, Mode string
	SizeGb                                                int64
	Boot, AutoDelete                                      bool
	Labels                                                map[string]any `json:",omitempty"`
	Licenses                                              []any          `json:",omitempty"`
	// ReplicaZones are a regional disk's.
	ReplicaZones []string `json:",omitempty"`
}

// runLocked is a backup run's state: a Compute Engine backup's, or Cloud
// SQL's. With s.mu held.
func (s *Server) runLocked(id string) (Run, bool) {
	if strings.HasPrefix(id, workloadRunPrefix) {
		return s.workloadRunLocked(id)
	}
	if strings.HasPrefix(id, computeRunPrefix) {
		r := s.st.ComputeRuns[id]
		if r == nil {
			return Run{}, false
		}
		return Run{Status: r.Status, Error: r.Error, EndTime: r.EndTime, SizeBytes: r.SizeBytes}, true
	}
	if s.sql == nil {
		return Run{}, false
	}
	return s.sql.Run(id)
}

// deleteRun deletes a backup run: a Compute Engine backup's snapshots, or
// Cloud SQL's run. Without s.mu held.
func (s *Server) deleteRun(id string) error {
	if strings.HasPrefix(id, workloadRunPrefix) {
		return s.deleteWorkloadRun(id)
	}
	if !strings.HasPrefix(id, computeRunPrefix) {
		if s.sql == nil {
			return nil
		}
		return s.sql.DeleteRun(id)
	}
	s.mu.Lock()
	r := s.st.ComputeRuns[id]
	delete(s.st.ComputeRuns, id)
	_ = s.saveLocked()
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	c, err := s.computeAPI()
	if err != nil {
		return nil
	}
	for _, d := range r.Disks {
		if d.Snapshot == "" {
			continue
		}
		if _, err := c.Call(http.MethodDelete, r.Project, "global/snapshots/"+d.Snapshot, vaultQuery(), nil); err != nil && !isNotFound(err) {
			return computeError(err)
		}
	}
	return nil
}

func vaultQuery() map[string][]string { return map[string][]string{VaultSnapshotQuery: {"1"}} }

func isNotFound(err error) bool { return asAPIError(computeError(err)).code == http.StatusNotFound }

func (s *Server) createComputeAssociation(loc, id, planName, resource, typ string) (any, error) {
	project, zone, rname, ok := computeResource(resource, typ)
	if !ok {
		return nil, invalid("backupPlanAssociation.resource %q is not a Compute Engine %s: the format is projects/{project}/zones/{zone}/%s/{name}.",
			resource, strings.TrimSuffix(collection(typ), "s"), collection(typ))
	}
	// "The backup plan ... must be in the same region as the resource":
	// a zonal resource is protected from its zone's region, a regional
	// disk from its region.
	if l := locationOfName(loc + "/x"); l != regionOfZone(zone) {
		return nil, invalid("backupPlanAssociation %s is in %s: a resource in zone %s is protected by a backup plan association in its region, %s.",
			loc+"/backupPlanAssociations/"+id, l, zone, regionOfZone(zone))
	}
	c, err := s.computeAPI()
	if err != nil {
		return nil, err
	}
	path := scopeKind(typ) + "/" + zone + "/" + collection(typ) + "/" + rname
	rel := "projects/" + project + "/" + path
	o, err := c.Call(http.MethodGet, project, path, nil, nil)
	if err != nil {
		return nil, computeError(err)
	}
	name := loc + "/backupPlanAssociations/" + id
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
		if d.Kind == typ && d.Project == project && d.Zone == zone && d.Instance == rname && strings.HasPrefix(d.Name, p.Vault+"/") {
			ds = d
		}
	}
	if ds == nil {
		ds = &dataSource{Name: p.Vault + "/dataSources/ds-" + newID()[:20], Project: project, Instance: rname, Zone: zone, Kind: typ, CreateTime: now}
		s.st.DataSources[ds.Name] = ds
	}
	ds.Association, ds.UpdateTime = name, now
	ds.Props = computeProps(typ, rel, o)
	ds.Info = Instance{Region: zone, CreateTime: str(o, "creationTimestamp")}
	a := &association{Name: name, Plan: p.Name, Resource: rel, ResourceType: typ, DataSource: ds.Name, CreateTime: now, UpdateTime: now,
		LastBackup: map[string]lastBackup{}}
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

// jobResourceType is the resourceType of a job's log entry.
func jobResourceType(kind string) string {
	switch kind {
	case ComputeInstanceResourceType:
		return "Compute Engine"
	case ComputeDiskResourceType:
		return "Disk"
	case ComputeRegionDiskResourceType:
		return "Regional Disk"
	case AlloyDBResourceType:
		return "AlloyDB"
	case FilestoreResourceType:
		return "Filestore"
	}
	return "Cloud SQL"
}

// computeBackup starts a backup of a Compute Engine data source: each disk
// snapshotted in turn, and the VM's configuration kept. It returns the
// triggerBackup operation, done when the backup is taken.
func (s *Server) computeBackup(assocName, planName, ruleID string, onDemand bool, retentionDays int64, labels map[string]string,
	rev string, d *dataSource, vlt *vault) (string, map[string]any, error) {
	loc := assocName[:strings.Index(assocName, "/backupPlanAssociations/")]
	cat := "SCHEDULED_BACKUP"
	if onDemand {
		cat = "ON_DEMAND_BACKUP"
	}
	jobPayload := map[string]any{"jobCategory": cat, "resourceType": jobResourceType(d.Kind), "backupRule": ruleID,
		"sourceResourceName": d.computeRel(), "sourceResourceId": d.Instance, "sourceResourceLocation": d.Zone,
		"backupVaultName": vaultOf(d.Name), "backupPlanName": planName, "dataSourceName": d.Name}
	c, err := s.computeAPI()
	if err != nil {
		s.mu.Lock()
		s.jobDoneLocked(BackupRestoreJobsLog, d.Project, locationOfName(d.Name), jobPayload, err)
		_ = s.saveLocked()
		s.mu.Unlock()
		return "", nil, err
	}
	now := s.now().UTC()
	runID := computeRunPrefix + newID()[:20]
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
	run := &computeRun{Status: "RUNNING", Project: d.Project, Zone: d.Zone}
	s.mu.Lock()
	job := s.startJobLocked(BackupRestoreJobsLog, d.Project, locationOfName(d.Name), jobPayload)
	if s.st.ComputeRuns == nil {
		s.st.ComputeRuns = map[string]*computeRun{}
	}
	s.st.ComputeRuns[runID] = run
	s.st.Backups[b.Name] = b
	if ruleID != "" {
		if a := s.st.Associations[assocName]; a != nil {
			a.LastBackup[ruleID] = lastBackup{State: "PENDING", Time: a.LastBackup[ruleID].Time}
		}
	}
	s.ops[opName] = op
	_ = s.saveLocked()
	out := copyMap(op)
	kind, project, zone, rname := d.Kind, d.Project, d.Zone, d.Instance
	s.mu.Unlock()
	go func() {
		inst, disks, props, err := takeComputeBackup(c, kind, project, zone, rname, runID)
		s.mu.Lock()
		defer s.mu.Unlock()
		end := s.stamp()
		if r := s.st.ComputeRuns[runID]; r != nil {
			r.Instance, r.Disks, r.EndTime = inst, disks, end
			if err != nil {
				r.Status, r.Error = "FAILED", err.Error()
			} else {
				r.Status = "SUCCESSFUL"
				for _, dk := range disks {
					r.SizeBytes += dk.SizeGb << 30
				}
			}
			job.Payload["incrementalBackupSizeGib"] = float64(r.SizeBytes) / (1 << 30)
		}
		if ds := s.st.DataSources[d.Name]; ds != nil && props != nil {
			ds.Props = props
		}
		s.finishJobLocked(job, err)
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

// waitCompute waits for a compute zonal (or, scope regions/R, regional)
// operation to finish, and returns its error.
func waitCompute(c Compute, project, zone string, op map[string]any) error {
	scope := "zones/" + zone
	if strings.Count(zone, "-") < 2 {
		scope = "regions/" + zone
	}
	name := str(op, "name")
	deadline := time.Now().Add(30 * time.Minute)
	for {
		if str(op, "status") == "DONE" {
			if e, ok := op["error"].(map[string]any); ok {
				msg := "the Compute Engine operation " + name + " failed"
				if l, _ := e["errors"].([]any); len(l) > 0 {
					if m, _ := l[0].(map[string]any); str(m, "message") != "" {
						msg = str(m, "message")
					}
				}
				return errorf(http.StatusInternalServerError, "INTERNAL", "%s", msg)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errorf(http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "the Compute Engine operation %s did not finish", name)
		}
		next, err := c.Call(http.MethodPost, project, scope+"/operations/"+name+"/wait", nil, nil)
		if err != nil {
			return computeError(err)
		}
		op = next
	}
}

// takeComputeBackup snapshots the disk, or each disk attached to the VM,
// into snapshots only the vault lists. On failure the snapshots made so far
// are deleted.
func takeComputeBackup(c Compute, kind, project, zone, name, runID string) (map[string]any, []computeDisk, map[string]any, error) {
	path := scopeKind(kind) + "/" + zone + "/" + collection(kind) + "/" + name
	rel := "projects/" + project + "/" + path
	o, err := c.Call(http.MethodGet, project, path, nil, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the %s %s cannot be backed up: %w", jobResourceType(kind), rel, computeError(err))
	}
	type src struct {
		disk string
		ad   map[string]any
	}
	var srcs []src
	var inst map[string]any
	if isDiskType(kind) {
		srcs = []src{{path, map[string]any{"deviceName": name}}}
	} else {
		inst = o
		l, _ := o["disks"].([]any)
		for _, e := range l {
			ad, _ := e.(map[string]any)
			// A VM's disk may be regional (regions/R/disks/D).
			srcs = append(srcs, src{diskPath(str(ad, "source"), zone), ad})
		}
	}
	var out []computeDisk
	cleanup := func() {
		for _, d := range out {
			_, _ = c.Call(http.MethodDelete, project, "global/snapshots/"+d.Snapshot, vaultQuery(), nil)
		}
	}
	for i, sd := range srcs {
		disk, err := c.Call(http.MethodGet, project, sd.disk, nil, nil)
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("disk %s cannot be backed up: %w", sd.disk, computeError(err))
		}
		snap := "bdr-" + strings.TrimPrefix(runID, computeRunPrefix) + "-" + strconv.Itoa(i)
		op, err := c.Call(http.MethodPost, project, sd.disk+"/createSnapshot", nil,
			map[string]any{"name": snap, "description": "Backup and DR backup of " + rel, VaultSnapshotQuery: true})
		if err == nil {
			err = waitCompute(c, project, strings.Split(sd.disk, "/")[1], op)
		}
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("the snapshot of disk %s failed: %w", sd.disk, computeError(err))
		}
		boot, _ := sd.ad["boot"].(bool)
		auto, _ := sd.ad["autoDelete"].(bool)
		lic, _ := disk["licenses"].([]any)
		lab, _ := disk["labels"].(map[string]any)
		var rz []string
		if l, ok := disk["replicaZones"].([]any); ok {
			for _, z := range l {
				if zs, ok := z.(string); ok {
					rz = append(rz, lastSeg(zs))
				}
			}
		}
		out = append(out, computeDisk{DeviceName: str(sd.ad, "deviceName"), Snapshot: snap, Source: str(disk, "selfLink"),
			Type: lastSeg(str(disk, "type")), Description: str(disk, "description"), Mode: str(sd.ad, "mode"), SizeGb: gb(disk["sizeGb"]),
			Boot: boot, AutoDelete: auto, Labels: lab, Licenses: lic, ReplicaZones: rz})
	}
	return inst, out, computeProps(kind, rel, o), nil
}

// diskPath is the path, under its project, of an attached disk's source
// (zones/Z/disks/D or regions/R/disks/D); a bare name is in zone.
func diskPath(source, zone string) string {
	p := strings.Split(source, "/")
	for i := 0; i+3 < len(p); i++ {
		if (p[i] == "zones" || p[i] == "regions") && p[i+2] == "disks" {
			return strings.Join(p[i:i+4], "/")
		}
	}
	return "zones/" + zone + "/disks/" + lastSeg(source)
}

// computeBackupJSON sets a Compute Engine backup's fields on out:
// computeInstanceBackupProperties or diskBackupProperties, and its
// gcpResource. With s.mu held.
func (s *Server) computeBackupJSON(b *backup, d *dataSource, out map[string]any) {
	delete(out, "cloudSqlInstanceBackupProperties")
	out["gcpResource"] = map[string]any{"gcpResourcename": "//compute.googleapis.com/" + d.computeRel(), "location": d.Zone, "type": d.Kind}
	r := s.st.ComputeRuns[b.RunID]
	if r == nil {
		return
	}
	if isDiskType(d.Kind) {
		p := map[string]any{"sourceDisk": d.computeRel(), "architecture": "X86_64", "physicalBlockSizeBytes": "4096"}
		// "Region and zone are mutually exclusive fields" (DiskBackupProperties).
		scope := "projects/" + d.Project + "/" + scopeKind(d.Kind) + "/" + d.Zone
		if d.Kind == ComputeRegionDiskResourceType {
			p["region"] = scope
		} else {
			p["zone"] = scope
		}
		if len(r.Disks) == 1 {
			dk := r.Disks[0]
			p["sizeGb"], p["type"], p["description"] = strconv.FormatInt(dk.SizeGb, 10), scope+"/diskTypes/"+dk.Type, dk.Description
			if len(dk.ReplicaZones) > 0 {
				var rz []any
				for _, z := range dk.ReplicaZones {
					rz = append(rz, "projects/"+d.Project+"/zones/"+z)
				}
				p["replicaZones"] = rz
			}
			if len(dk.Licenses) > 0 {
				p["licenses"] = dk.Licenses
			}
			if len(dk.Labels) > 0 {
				p["labels"] = dk.Labels
			}
		}
		out["diskBackupProperties"] = p
		return
	}
	i := r.Instance
	if i == nil {
		out["computeInstanceBackupProperties"] = map[string]any{"sourceInstance": d.computeRel()}
		return
	}
	var disks []any
	for _, dk := range r.Disks {
		disks = append(disks, map[string]any{"deviceName": dk.DeviceName, "boot": dk.Boot, "autoDelete": dk.AutoDelete,
			"mode": dk.Mode, "diskSizeGb": strconv.FormatInt(dk.SizeGb, 10), "type": "PERSISTENT",
			"diskType": diskTypeLink(d.Project, d.Zone, dk),
			"source":   dk.Source})
	}
	p := map[string]any{"sourceInstance": d.computeRel(), "machineType": str(i, "machineType"), "description": str(i, "description"),
		"canIpForward": i["canIpForward"] == true, "disk": disks, "keyRevocationActionType": "NONE"}
	for k, to := range map[string]string{"networkInterfaces": "networkInterface", "serviceAccounts": "serviceAccount",
		"labels": "labels", "tags": "tags", "scheduling": "scheduling"} {
		if v, ok := i[k]; ok {
			p[to] = v
		}
	}
	if md, ok := i["metadata"].(map[string]any); ok {
		p["metadata"] = map[string]any{"items": md["items"]}
	}
	out["computeInstanceBackupProperties"] = p
}

// knownKeys refuses, as unimplemented, the fields of a restore's
// properties CloudBurrow does not apply.
func knownKeys(field string, m map[string]any, known ...string) error {
	var extra []string
	for k, v := range m {
		if v == nil || v == "" || v == false {
			continue
		}
		if l, ok := v.([]any); ok && len(l) == 0 {
			continue
		}
		ok := false
		for _, kk := range known {
			ok = ok || k == kk
		}
		if !ok {
			extra = append(extra, field+"."+k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return unimplemented("restoring with "+strings.Join(extra, ", "),
			"Compute Engine here does not serve those settings (customer-managed disk keys, storage pools, provisioned IOPS, reservations, "+
				"confidential VMs, GPUs and the like)")
	}
	return nil
}

// restoreCompute is backups.restore of a Compute Engine backup: a new VM
// from a VM's backup, or a new disk from a disk's backup or from one disk
// of a VM's.
func (s *Server) restoreCompute(name string, body map[string]any) (any, error) {
	if err := s.checkVaultKeyFor(name); err != nil {
		return nil, err
	}
	mapOf := func(k string) (map[string]any, bool) { m, ok := body[k].(map[string]any); return m, ok }
	if m := str(body, "clearOverridesFieldMask"); m != "" {
		return nil, unimplemented("clearOverridesFieldMask", "every restored field is the backup's or its override")
	}
	s.mu.Lock()
	b := s.st.Backups[name]
	if b == nil {
		s.mu.Unlock()
		return nil, notFound("Backup %s does not exist.", name)
	}
	d := s.st.DataSources[b.DataSource]
	r := s.st.ComputeRuns[b.RunID]
	if d == nil || r == nil || r.Status != "SUCCESSFUL" {
		s.mu.Unlock()
		return nil, precondition("Backup %s is not ACTIVE: only a backup that was taken can be restored.", name)
	}
	run := *r
	kind := d.Kind
	s.mu.Unlock()
	instEnv, hasInst := mapOf("computeInstanceTargetEnvironment")
	diskEnv, hasZonal := mapOf("diskTargetEnvironment")
	regionEnv, hasRegional := mapOf("regionDiskTargetEnvironment")
	hasDisk := hasZonal || hasRegional
	targets := 0
	for _, h := range []bool{hasInst, hasZonal, hasRegional} {
		if h {
			targets++
		}
	}
	switch {
	case targets > 1:
		return nil, invalid("Give one of computeInstanceTargetEnvironment, diskTargetEnvironment and regionDiskTargetEnvironment.")
	case isDiskType(kind) && hasInst:
		return nil, invalid("Backup %s is a disk's backup: restore it with diskTargetEnvironment or regionDiskTargetEnvironment, and diskRestoreProperties.", name)
	case kind == ComputeInstanceResourceType && !hasInst && !hasDisk:
		return nil, invalid("computeInstanceTargetEnvironment is required to restore a Compute Engine instance's backup (or diskTargetEnvironment or regionDiskTargetEnvironment, for one of its disks).")
	case isDiskType(kind) && !hasDisk:
		return nil, invalid("diskTargetEnvironment or regionDiskTargetEnvironment is required to restore a disk's backup.")
	}
	env, envField, locField := instEnv, "computeInstanceTargetEnvironment", "zone"
	switch {
	case hasZonal:
		env, envField = diskEnv, "diskTargetEnvironment"
	case hasRegional:
		env, envField, locField = regionEnv, "regionDiskTargetEnvironment", "region"
	}
	project, zone := str(env, "project"), str(env, locField)
	if project == "" {
		return nil, invalid("%s.project is required.", envField)
	}
	if zone == "" {
		return nil, invalid("%s.%s is required.", envField, locField)
	}
	var replicaZones []any
	if hasRegional {
		// "Target URLs of the replica zones for the disk" (required): two
		// zones of the region, as gcloud's --replica-zones takes them.
		l, _ := env["replicaZones"].([]any)
		if len(l) == 0 {
			return nil, invalid("regionDiskTargetEnvironment.replicaZones is required.")
		}
		if len(l) != 2 {
			return nil, invalid("regionDiskTargetEnvironment.replicaZones must list exactly 2 zones, not %d.", len(l))
		}
		for i, z := range l {
			zs, _ := z.(string)
			if regionOfZone(lastSeg(zs)) != zone {
				return nil, invalid("regionDiskTargetEnvironment.replicaZones[%d] %q is not a zone of region %s.", i, zs, zone)
			}
			replicaZones = append(replicaZones, "zones/"+lastSeg(zs))
		}
	}
	c, err := s.computeAPI()
	if err != nil {
		return nil, err
	}
	var target, typ, path string
	var insert map[string]any
	switch {
	case hasRegional:
		props, _ := mapOf("diskRestoreProperties")
		insert, err = diskInsert(name, run, props, kind, "regions/"+zone)
		if err == nil {
			insert["replicaZones"] = replicaZones
		}
		typ, path = ComputeRegionDiskResourceType, "regions/"+zone+"/disks"
		target = "projects/" + project + "/regions/" + zone + "/disks/" + str(insert, "name")
	case hasZonal:
		props, _ := mapOf("diskRestoreProperties")
		insert, err = diskInsert(name, run, props, kind, "zones/"+zone)
		typ, path = ComputeDiskResourceType, "zones/"+zone+"/disks"
		target = "projects/" + project + "/zones/" + zone + "/disks/" + str(insert, "name")
	default:
		props, _ := mapOf("computeInstanceRestoreProperties")
		insert, err = instanceInsert(run, props, project, zone)
		typ, path = ComputeInstanceResourceType, "zones/"+zone+"/instances"
		target = "projects/" + project + "/zones/" + zone + "/instances/" + str(insert, "name")
	}
	if err != nil {
		return nil, err
	}
	cop, err := c.Call(http.MethodPost, project, path, nil, insert)
	if err != nil {
		return nil, computeError(err)
	}
	loc := name[:strings.Index(name, "/backupVaults/")]
	opName, op := s.newOp(loc, "restore", name)
	s.mu.Lock()
	payload := map[string]any{"jobCategory": "RESTORE", "resourceType": jobResourceType(kind), "restoreResourceName": target,
		"dataSourceName": d.Name, "backupName": name, "backupVaultName": vaultOf(d.Name), "sourceResourceName": d.computeRel(),
		"sourceResourceId": d.Instance, "sourceResourceLocation": d.Zone, "recoveryPointTime": run.EndTime}
	job := s.startJobLocked(BackupRestoreJobsLog, project, locationOfName(name), payload)
	s.ops[opName] = op
	_ = s.saveLocked()
	out := copyMap(op)
	s.mu.Unlock()
	go func() {
		err := waitCompute(c, project, zone, cop)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.finishJobLocked(job, err)
		resp := map[string]any{"targetResource": map[string]any{"gcpResource": map[string]any{
			"gcpResourcename": "//compute.googleapis.com/" + target, "location": zone, "type": typ}}}
		s.finishOp(op, resp, "google.cloud.backupdr.v1.RestoreBackupResponse", err)
		_ = s.saveLocked()
	}()
	return out, nil
}

func snapshotRel(run computeRun, d computeDisk) string {
	return "projects/" + run.Project + "/global/snapshots/" + d.Snapshot
}

// diskInsert is the disks.insert (or regionDisks.insert) body of a disk
// restore into scope, zones/Z or regions/R.
func diskInsert(name string, run computeRun, props map[string]any, kind, scope string) (map[string]any, error) {
	if err := knownKeys("diskRestoreProperties", props, "name", "sizeGb", "type", "description", "labels", "architecture",
		"licenses", "guestOsFeature", "physicalBlockSizeBytes", "instanceBackupSource"); err != nil {
		return nil, err
	}
	if str(props, "name") == "" {
		return nil, invalid("diskRestoreProperties.name is required.")
	}
	if a := str(props, "architecture"); a != "" && a != "X86_64" && a != "ARM64" {
		return nil, invalid("diskRestoreProperties.architecture %q is not ARM64 or X86_64.", a)
	}
	if p := str(props, "physicalBlockSizeBytes"); p != "" && p != "4096" {
		return nil, invalid("diskRestoreProperties.physicalBlockSizeBytes %s is not supported: it is 4096.", p)
	}
	var src *computeDisk
	if isDiskType(kind) {
		if _, ok := props["instanceBackupSource"]; ok {
			return nil, invalid("Backup %s is a disk's backup: diskRestoreProperties.instanceBackupSource applies to a Compute Engine instance's backup.", name)
		}
		if len(run.Disks) == 1 {
			src = &run.Disks[0]
		}
	} else {
		ibs, _ := props["instanceBackupSource"].(map[string]any)
		boot, _ := ibs["bootDisk"].(bool)
		dev := str(ibs, "sourceDeviceName")
		if boot == (dev != "") {
			return nil, invalid("diskRestoreProperties.instanceBackupSource needs bootDisk or sourceDeviceName, one of them, to restore one disk of an instance's backup.")
		}
		for i := range run.Disks {
			if (boot && run.Disks[i].Boot) || (dev != "" && run.Disks[i].DeviceName == dev) {
				src = &run.Disks[i]
			}
		}
		if src == nil {
			return nil, invalid("Backup %s holds no disk with device name %q.", name, dev)
		}
	}
	if src == nil {
		return nil, precondition("Backup %s holds no disk.", name)
	}
	size := src.SizeGb
	if v, ok := props["sizeGb"]; ok && gb(v) != 0 {
		if gb(v) < src.SizeGb {
			return nil, invalid("diskRestoreProperties.sizeGb %d is smaller than the backed up disk, %d GB.", gb(v), src.SizeGb)
		}
		size = gb(v)
	}
	typ := src.Type
	if t := str(props, "type"); t != "" {
		typ = lastSeg(t)
	}
	ins := map[string]any{"name": str(props, "name"), "sourceSnapshot": snapshotRel(run, *src), "sizeGb": strconv.FormatInt(size, 10),
		"type": scope + "/diskTypes/" + typ}
	if v := str(props, "description"); v != "" {
		ins["description"] = v
	} else if src.Description != "" {
		ins["description"] = src.Description
	}
	if l, ok := props["labels"].(map[string]any); ok {
		ins["labels"] = l
	} else if len(src.Labels) > 0 {
		ins["labels"] = src.Labels
	}
	return ins, nil
}

// instanceInsert is the instances.insert body of a VM restore: the
// backed-up configuration with the restore's overrides, each disk made
// from its snapshot.
func instanceInsert(run computeRun, props map[string]any, project, zone string) (map[string]any, error) {
	if err := knownKeys("computeInstanceRestoreProperties", props, "name", "machineType", "description", "labels", "metadata",
		"tags", "networkInterfaces", "serviceAccounts", "scheduling", "canIpForward", "deletionProtection", "hostname",
		"disks", "keyRevocationActionType"); err != nil {
		return nil, err
	}
	name := str(props, "name")
	if name == "" {
		return nil, invalid("computeInstanceRestoreProperties.name is required.")
	}
	if k := str(props, "keyRevocationActionType"); k != "" && k != "NONE" && k != "STOP" && k != "KEY_REVOCATION_ACTION_TYPE_UNSPECIFIED" {
		return nil, invalid("computeInstanceRestoreProperties.keyRevocationActionType %q is not NONE or STOP.", k)
	}
	src := run.Instance
	region := regionOfZone(zone)
	sameProject := project == run.Project
	ins := map[string]any{"name": name}
	mt := lastSeg(str(src, "machineType"))
	if v := str(props, "machineType"); v != "" {
		mt = lastSeg(v)
	}
	ins["machineType"] = "zones/" + zone + "/machineTypes/" + mt
	for _, k := range []string{"description", "labels", "tags", "scheduling", "canIpForward", "deletionProtection", "hostname"} {
		if v, ok := props[k]; ok {
			ins[k] = v
		} else if v, ok := src[k]; ok {
			ins[k] = v
		}
	}
	if t, ok := ins["tags"].(map[string]any); ok {
		ins["tags"] = map[string]any{"items": t["items"]}
	}
	if m, ok := props["metadata"].(map[string]any); ok {
		ins["metadata"] = map[string]any{"items": m["items"]}
	} else if m, ok := src["metadata"].(map[string]any); ok {
		ins["metadata"] = map[string]any{"items": m["items"]}
	}
	if v, ok := props["serviceAccounts"]; ok {
		ins["serviceAccounts"] = v
	} else if l, ok := src["serviceAccounts"].([]any); ok {
		var out []any
		for _, e := range l {
			sa, _ := e.(map[string]any)
			email := str(sa, "email")
			if !sameProject && strings.HasSuffix(email, "-compute@developer.gserviceaccount.com") {
				email = "default" // the target project's default service account
			}
			out = append(out, map[string]any{"email": email, "scopes": sa["scopes"]})
		}
		ins["serviceAccounts"] = out
	}
	if v, ok := props["networkInterfaces"]; ok {
		ins["networkInterfaces"] = v
	} else {
		l, _ := src["networkInterfaces"].([]any)
		var out []any
		for _, e := range l {
			nic, _ := e.(map[string]any)
			n := map[string]any{"network": "global/networks/" + lastSeg(str(nic, "network"))}
			if sub := str(nic, "subnetwork"); sub != "" {
				n["subnetwork"] = "regions/" + region + "/subnetworks/" + lastSeg(sub)
			}
			if acs, ok := nic["accessConfigs"].([]any); ok {
				var a []any
				for _, ac := range acs {
					m, _ := ac.(map[string]any)
					a = append(a, map[string]any{"name": str(m, "name"), "networkTier": str(m, "networkTier")})
				}
				n["accessConfigs"] = a
			}
			out = append(out, n)
		}
		ins["networkInterfaces"] = out
	}
	over := map[string]map[string]any{}
	if l, ok := props["disks"].([]any); ok {
		for i, e := range l {
			m, _ := e.(map[string]any)
			dev := str(m, "deviceName")
			if dev == "" {
				return nil, invalid("computeInstanceRestoreProperties.disks[%d].deviceName is required: it names the backed up disk to override.", i)
			}
			found := false
			for _, dk := range run.Disks {
				found = found || dk.DeviceName == dev
			}
			if !found {
				return nil, invalid("computeInstanceRestoreProperties.disks[%d].deviceName %q is not a disk of the backup.", i, dev)
			}
			if err := knownKeys(fmt.Sprintf("computeInstanceRestoreProperties.disks[%d]", i), m, "deviceName", "autoDelete", "mode",
				"diskSizeGb", "initializeParams", "boot", "type", "kind", "index", "diskType", "source"); err != nil {
				return nil, err
			}
			over[dev] = m
		}
	}
	var disks []any
	for i, dk := range run.Disks {
		dname := name
		if !dk.Boot {
			dname = name + "-" + strconv.Itoa(i)
		}
		ip := map[string]any{"diskName": dname, "sourceSnapshot": snapshotRel(run, dk), "diskType": "zones/" + zone + "/diskTypes/" + dk.Type,
			"diskSizeGb": strconv.FormatInt(dk.SizeGb, 10)}
		if len(dk.ReplicaZones) > 0 {
			// "Source regional persistent disks will be restored with
			// default replica zones if not specified": the VM's zone and
			// the region's next.
			ip["replicaZones"] = defaultReplicaZones(zone)
		}
		ad := map[string]any{"boot": dk.Boot, "autoDelete": dk.AutoDelete, "deviceName": dk.DeviceName}
		if dk.Mode != "" {
			ad["mode"] = dk.Mode
		}
		if o := over[dk.DeviceName]; o != nil {
			if v, ok := o["autoDelete"].(bool); ok {
				ad["autoDelete"] = v
			}
			if v := str(o, "mode"); v != "" {
				ad["mode"] = v
			}
			if v := gb(o["diskSizeGb"]); v != 0 {
				ip["diskSizeGb"] = strconv.FormatInt(v, 10)
			}
			if p, _ := o["initializeParams"].(map[string]any); str(p, "diskName") != "" {
				ip["diskName"] = str(p, "diskName")
			}
		}
		ad["initializeParams"] = ip
		disks = append(disks, ad)
	}
	ins["disks"] = disks
	return ins, nil
}

// diskTypeLink is a backed-up disk's type, under its zone or, for a
// regional disk, its region.
func diskTypeLink(project, zone string, dk computeDisk) string {
	if len(dk.ReplicaZones) > 0 {
		return "projects/" + project + "/regions/" + regionOfZone(zone) + "/diskTypes/" + dk.Type
	}
	return "projects/" + project + "/zones/" + zone + "/diskTypes/" + dk.Type
}

// defaultReplicaZones are a regional disk's replica zones restored with a
// VM in zone: zone, and the first other zone of its region.
func defaultReplicaZones(zone string) []any {
	out := []any{"zones/" + zone}
	for _, z := range locations.Zones(regionOfZone(zone)) {
		if z != zone {
			return append(out, "zones/"+z)
		}
	}
	return out
}
