package backupdr

// Updates (patch on vaults, plans, associations, data sources and
// backups), plan revisions, the fetch and reference views, operation
// cancel and delete (#1462), and the reasons the rest is refused.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Why the methods that need something CloudBurrow does not run are
// refused, each naming what is missing.
const (
	managementServerWhy = "a management server is the Backup and DR appliance's console: a Google-managed VM " +
		"on Compute Engine reached over VPC peering with your network, and CloudBurrow runs neither Compute Engine " +
		"nor Google-managed networking; backup vaults and plans protect Cloud SQL instances without one (docs/backupdr.md)"
	applianceWhy = "this is the backup/recovery appliance's and agent's protocol for workloads on Compute Engine " +
		"(the appliance streams a backup in and finalizes it); CloudBurrow runs no appliance and no Compute Engine, " +
		"and a Cloud SQL data source's backups are taken by the Cloud SQL Admin API (docs/backupdr.md)"
	trialWhy = "a Backup and DR trial is billing-account state (a subscription with its end date), and CloudBurrow " +
		"has no billing accounts; every method it serves is available without one (docs/backupdr.md)"
	hierarchyWhy = "fetching resource backup configs for a folder or an organization inventories every project " +
		"under it through Cloud Asset Inventory, which CloudBurrow does not run (docs/backupdr.md)"
	inventoryWhy = "resource backup configs inventory every resource of a project (Compute Engine instances and " +
		"disks, Cloud Storage buckets, Cloud SQL instances) through Cloud Asset Inventory, which CloudBurrow does not " +
		"run; a Cloud SQL instance's protection is its backup plan association and data source (docs/backupdr.md)"
	initializeWhy = "initializing the service creates Google's default backup vault and backup plan for the resource " +
		"type, whose names, rules and retention Google chooses and does not document; create them with " +
		"backupVaults.create and backupPlans.create (docs/backupdr.md)"
)

// --- revisions --------------------------------------------------------

func (p *plan) revisionID() string { return "v" + strconv.Itoa(p.Revision) }

func (p *plan) revisionName() string { return p.Name + "/revisions/" + p.revisionID() }

// planRevision is the current revision of the plan named name, with s.mu
// held; v0 for a plan that is gone.
func (s *Server) planRevision(name string) string {
	if p := s.st.Plans[name]; p != nil {
		return p.revisionID()
	}
	return "v0"
}

func (b *backup) revision() string {
	if b.Revision == "" {
		return "v0"
	}
	return b.Revision
}

func (b *backup) enforcedEnd() string {
	if b.EnforcedEnd == "" {
		return b.ExpireTime
	}
	return b.EnforcedEnd
}

// addRevisionLocked records the plan as it now is as its current
// revision, with s.mu held.
func (s *Server) addRevisionLocked(p *plan) {
	s.backfillRevisionsLocked(p)
	if n := len(p.Revisions); n > 0 && p.Revisions[n-1].ID == p.revisionID() {
		return
	}
	p.Revisions = append(p.Revisions, planRevision{ID: p.revisionID(), CreateTime: p.UpdateTime, Snapshot: s.planJSON(p)})
}

// backfillRevisionsLocked gives a plan recorded before revisions were
// kept its v0.
func (s *Server) backfillRevisionsLocked(p *plan) {
	if len(p.Revisions) == 0 && p.Revision == 0 {
		p.Revisions = []planRevision{{ID: "v0", CreateTime: p.CreateTime, Snapshot: s.planJSON(p)}}
	}
}

func (s *Server) revisionJSON(p *plan, r planRevision) map[string]any {
	state := "INACTIVE"
	if r.ID == p.revisionID() {
		state = "ACTIVE"
	}
	return map[string]any{"name": p.Name + "/revisions/" + r.ID, "revisionId": r.ID, "state": state,
		"createTime": r.CreateTime, "backupPlanSnapshot": r.Snapshot}
}

func (s *Server) listRevisions(planName string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Plans[planName]
	if p == nil {
		return nil, notFound("Backup plan %s does not exist.", planName)
	}
	s.backfillRevisionsLocked(p)
	out := []any{}
	// Newest first, as Google lists them.
	for i := len(p.Revisions) - 1; i >= 0; i-- {
		out = append(out, s.revisionJSON(p, p.Revisions[i]))
	}
	return map[string]any{"backupPlanRevisions": out}, nil
}

func (s *Server) getRevision(planName, id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Plans[planName]
	if p == nil {
		return nil, notFound("Backup plan %s does not exist.", planName)
	}
	s.backfillRevisionsLocked(p)
	for _, r := range p.Revisions {
		if r.ID == id {
			return s.revisionJSON(p, r), nil
		}
	}
	return nil, notFound("Backup plan revision %s/revisions/%s does not exist.", planName, id)
}

// --- patch ------------------------------------------------------------

// updateMask is the request's updateMask as camelCase field names; it is
// required on every patch.
func updateMask(r *http.Request, allowed ...string) ([]string, error) {
	raw := param(r, "updateMask")
	if raw == "" {
		return nil, invalid("updateMask is required.")
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = camel(strings.TrimSpace(f))
		// labels.key masks the one label; it overwrites labels whole here.
		if top, _, found := strings.Cut(f, "."); found && top == "labels" {
			f = top
		}
		if !ok[f] {
			return nil, invalid("updateMask field %q cannot be updated; the fields that can are %s.", f, strings.Join(allowed, ", "))
		}
		out = append(out, f)
	}
	return out, nil
}

// camel turns backup_rules into backupRules.
func camel(s string) string {
	var b strings.Builder
	up := false
	for _, c := range s {
		switch {
		case c == '_':
			up = true
		case up:
			b.WriteString(strings.ToUpper(string(c)))
			up = false
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (s *Server) patchVault(r *http.Request, name string) (any, error) {
	mask, err := updateMask(r, "description", "labels", "backupMinimumEnforcedRetentionDuration", "accessRestriction", "effectiveTime")
	if err != nil {
		return nil, err
	}
	var body struct {
		Description       string            `json:"description"`
		Labels            map[string]string `json:"labels"`
		MinRetention      string            `json:"backupMinimumEnforcedRetentionDuration"`
		AccessRestriction string            `json:"accessRestriction"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	loc := name[:strings.Index(name, "/backupVaults/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.st.Vaults[name]
	if v == nil {
		return nil, notFound("Backup vault %s does not exist.", name)
	}
	next := *v
	for _, f := range mask {
		switch f {
		case "description":
			next.Description = body.Description
		case "labels":
			next.Labels = body.Labels
		case "backupMinimumEnforcedRetentionDuration":
			if d, err := parseDuration(body.MinRetention); err != nil || d < 0 {
				return nil, invalid("backupVault.backupMinimumEnforcedRetentionDuration %q is not a duration such as \"100000s\".", body.MinRetention)
			}
			next.MinRetention = body.MinRetention
		case "accessRestriction":
			switch body.AccessRestriction {
			case "WITHIN_PROJECT", "WITHIN_ORGANIZATION", "UNRESTRICTED", "WITHIN_ORG_BUT_UNRESTRICTED_FOR_BA":
			default:
				return nil, invalid("backupVault.accessRestriction %q is not one of WITHIN_PROJECT, WITHIN_ORGANIZATION, UNRESTRICTED.", body.AccessRestriction)
			}
			next.AccessRestriction = body.AccessRestriction
		case "effectiveTime":
			// The time the retention takes effect; it is applied at once.
		}
	}
	if param(r, "validateOnly") == "true" {
		return s.opDoneLocked(loc, "update", name, s.vaultJSON(&next), "google.cloud.backupdr.v1.BackupVault", nil), nil
	}
	next.UpdateTime = s.stamp()
	*v = next
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.opDoneLocked(loc, "update", name, s.vaultJSON(v), "google.cloud.backupdr.v1.BackupVault", nil), nil
}

func (s *Server) patchPlan(r *http.Request, name string) (any, error) {
	mask, err := updateMask(r, "description", "labels", "backupRules", "logRetentionDays")
	if err != nil {
		return nil, err
	}
	var body struct {
		Description      string            `json:"description"`
		Labels           map[string]string `json:"labels"`
		Rules            []rule            `json:"backupRules"`
		LogRetentionDays json.RawMessage   `json:"logRetentionDays"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	loc := name[:strings.Index(name, "/backupPlans/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Plans[name]
	if p == nil {
		return nil, notFound("Backup plan %s does not exist.", name)
	}
	s.backfillRevisionsLocked(p)
	next := *p
	for _, f := range mask {
		switch f {
		case "description":
			next.Description = body.Description
		case "labels":
			next.Labels = body.Labels
		case "backupRules":
			if err := checkRules(body.Rules); err != nil {
				return nil, err
			}
			next.Rules = body.Rules
		case "logRetentionDays":
			n, err := parseLogDays(body.LogRetentionDays)
			if err != nil {
				return nil, err
			}
			next.LogRetentionDays = n
		}
	}
	next.Revision++
	next.UpdateTime = s.stamp()
	*p = next
	s.addRevisionLocked(p)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.opDoneLocked(loc, "update", name, s.planJSON(p), "google.cloud.backupdr.v1.BackupPlan", nil), nil
}

// patchAssociation moves an association to another plan (backupPlan is
// the one field Google lets a patch change). A plan in another vault
// gives the instance a data source there, as a new association would.
func (s *Server) patchAssociation(r *http.Request, name string) (any, error) {
	if _, err := updateMask(r, "backupPlan"); err != nil {
		return nil, err
	}
	var body struct {
		Plan string `json:"backupPlan"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	loc := name[:strings.Index(name, "/backupPlanAssociations/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Associations[name]
	if a == nil {
		return nil, notFound("Backup plan association %s does not exist.", name)
	}
	p := s.st.Plans[body.Plan]
	switch {
	case body.Plan == "":
		return nil, invalid("backupPlanAssociation.backupPlan is required.")
	case p == nil:
		return nil, invalid("backupPlanAssociation.backupPlan %s does not exist.", body.Plan)
	case p.ResourceType != a.ResourceType:
		return nil, invalid("Backup plan %s protects %s, not %s.", p.Name, p.ResourceType, a.ResourceType)
	}
	now := s.stamp()
	if old := s.st.DataSources[a.DataSource]; old == nil || !strings.HasPrefix(old.Name, p.Vault+"/") {
		project, instance, _ := sqlResource(a.Resource)
		var ds *dataSource
		for _, d := range s.st.DataSources {
			if d.Project == project && d.Instance == instance && strings.HasPrefix(d.Name, p.Vault+"/") {
				ds = d
			}
		}
		if ds == nil {
			ds = &dataSource{Name: p.Vault + "/dataSources/ds-" + newID()[:20], Project: project, Instance: instance, CreateTime: now}
			if old != nil {
				ds.Info = old.Info
			}
			s.st.DataSources[ds.Name] = ds
		}
		if old != nil {
			old.Association, old.UpdateTime = "", now
		}
		ds.Association, ds.UpdateTime = name, now
		a.DataSource = ds.Name
	}
	a.Plan, a.UpdateTime = p.Name, now
	// The new plan's rules start over.
	a.LastBackup = map[string]lastBackup{}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.opDoneLocked(loc, "update", name, s.associationJSON(a), "google.cloud.backupdr.v1.BackupPlanAssociation", nil), nil
}

func (s *Server) patchDataSource(r *http.Request, name string) (any, error) {
	mask, err := updateMask(r, "labels")
	if err != nil {
		return nil, err
	}
	var body struct {
		Labels map[string]string `json:"labels"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	loc := name[:strings.Index(name, "/backupVaults/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.st.DataSources[name]
	if d == nil {
		if param(r, "allowMissing") == "true" {
			return nil, unimplemented("dataSources.patch with allowMissing", "a data source is made by a backup plan association "+
				"or by the backup appliance's protocol on Compute Engine, which CloudBurrow does not run, not by an upsert")
		}
		return nil, notFound("Data source %s does not exist.", name)
	}
	for range mask {
		d.Labels = body.Labels
	}
	d.UpdateTime = s.stamp()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.opDoneLocked(loc, "update", name, s.dataSourceJSON(d), "google.cloud.backupdr.v1.DataSource", nil), nil
}

// patchBackup changes a backup's expiry and enforced retention: the
// enforced retention can be extended, never shortened, and the backup
// cannot expire before it ends.
func (s *Server) patchBackup(r *http.Request, name string) (any, error) {
	mask, err := updateMask(r, "expireTime", "enforcedRetentionEndTime", "labels", "description")
	if err != nil {
		return nil, err
	}
	var body struct {
		ExpireTime  string            `json:"expireTime"`
		EnforcedEnd string            `json:"enforcedRetentionEndTime"`
		Labels      map[string]string `json:"labels"`
		Description string            `json:"description"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	loc := name[:strings.Index(name, "/backupVaults/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Backups[name]
	if b == nil {
		return nil, notFound("Backup %s does not exist.", name)
	}
	next := *b
	enforced, expire := b.enforcedEnd(), b.ExpireTime
	for _, f := range mask {
		switch f {
		case "expireTime":
			expire = body.ExpireTime
		case "enforcedRetentionEndTime":
			enforced = body.EnforcedEnd
		case "labels":
			next.Labels = body.Labels
		case "description":
			next.Description = body.Description
		}
	}
	te, err1 := time.Parse(time.RFC3339Nano, expire)
	tf, err2 := time.Parse(time.RFC3339Nano, enforced)
	old, _ := time.Parse(time.RFC3339Nano, b.enforcedEnd())
	switch {
	case err1 != nil:
		return nil, invalid("backup.expireTime %q is not an RFC 3339 time.", expire)
	case err2 != nil:
		return nil, invalid("backup.enforcedRetentionEndTime %q is not an RFC 3339 time.", enforced)
	case tf.Before(old):
		return nil, invalid("backup.enforcedRetentionEndTime %s is before the current one, %s: enforced retention can be extended, not reduced.", enforced, b.enforcedEnd())
	case te.Before(tf):
		return nil, invalid("backup.expireTime %s is before its enforcedRetentionEndTime %s.", expire, enforced)
	}
	next.ExpireTime = te.UTC().Format(time.RFC3339Nano)
	next.EnforcedEnd = tf.UTC().Format(time.RFC3339Nano)
	if next.EnforcedEnd == next.ExpireTime {
		next.EnforcedEnd = ""
	}
	next.UpdateTime = s.stamp()
	*b = next
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.opDoneLocked(loc, "update", name, s.backupJSON(b), "google.cloud.backupdr.v1.Backup", nil), nil
}

// opDoneLocked is opDone with s.mu held.
func (s *Server) opDoneLocked(loc, verb, target string, resp map[string]any, typ string, err error) map[string]any {
	name, op := s.newOp(loc, verb, target)
	s.finishOp(op, resp, typ, err)
	s.ops[name] = op
	return copyMap(op)
}

// --- fetch views ------------------------------------------------------

func resourceType(r *http.Request) (string, error) {
	t := param(r, "resourceType")
	if t == "" {
		return "", invalid("resourceType is required.")
	}
	return t, nil
}

func (s *Server) fetchAssociations(r *http.Request, loc string) (any, error) {
	typ, err := resourceType(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, n := range sortedKeys(s.st.Associations) {
		if a := s.st.Associations[n]; under(n, loc, "/backupPlanAssociations/") && a.ResourceType == typ {
			out = append(out, s.associationJSON(a))
		}
	}
	return map[string]any{"backupPlanAssociations": out}, nil
}

func (s *Server) fetchBackups(r *http.Request, dsName string) (any, error) {
	typ, err := resourceType(r)
	if err != nil {
		return nil, err
	}
	all, err := s.listBackups(dsName)
	if err != nil {
		return nil, err
	}
	out := []any{}
	if typ == CloudSQLResourceType {
		out = all.(map[string]any)["backups"].([]any)
	}
	return map[string]any{"backups": out}, nil
}

// A data source reference is a project's location-level view of a data
// source, named by the data source's ID.
func (s *Server) referenceJSON(loc string, d *dataSource) map[string]any {
	ds := s.dataSourceJSON(d)
	cfg, _ := ds["backupConfigInfo"].(map[string]any)
	info := map[string]any{"lastBackupState": cfg["lastBackupState"]}
	if t, ok := cfg["lastSuccessfulBackupConsistencyTime"]; ok {
		info["lastSuccessfulBackupConsistencyTime"] = t
	}
	res := "projects/" + d.Project + "/instances/" + d.Instance
	return map[string]any{"name": loc + "/dataSourceReferences/" + d.Name[strings.LastIndex(d.Name, "/")+1:],
		"dataSource": d.Name, "createTime": d.CreateTime, "dataSourceBackupConfigState": ds["configState"],
		"dataSourceBackupCount": ds["backupCount"], "dataSourceBackupConfigInfo": info, "totalStoredBytes": "0",
		"dataSourceGcpResourceInfo": map[string]any{"gcpResourcename": "//sqladmin.googleapis.com/" + res,
			"location": d.Info.Region, "type": CloudSQLResourceType, "cloudSqlInstanceProperties": map[string]any{
				"name": res, "databaseInstalledVersion": d.Info.DatabaseVersion, "instanceCreateTime": d.Info.CreateTime,
				"instanceTier": d.Info.Tier}}}
}

// referencesLocked are the location's data source references, by name.
func (s *Server) referencesLocked(loc string) []map[string]any {
	var out []map[string]any
	for _, n := range sortedKeys(s.st.DataSources) {
		vaultName := n[:strings.Index(n, "/dataSources/")]
		if match(loc, vaultName[:strings.Index(vaultName, "/backupVaults/")]) {
			vloc := vaultName[:strings.Index(vaultName, "/backupVaults/")]
			out = append(out, s.referenceJSON(vloc, s.st.DataSources[n]))
		}
	}
	return out
}

func (s *Server) listReferences(loc string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, r := range s.referencesLocked(loc) {
		out = append(out, r)
	}
	return map[string]any{"dataSourceReferences": out}
}

func (s *Server) getReference(name string) (any, error) {
	loc := name[:strings.Index(name, "/dataSourceReferences/")]
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.referencesLocked(loc) {
		if r["name"] == name {
			return r, nil
		}
	}
	return nil, notFound("Data source reference %s does not exist.", name)
}

func (s *Server) fetchReferences(r *http.Request, loc string) (any, error) {
	typ, err := resourceType(r)
	if err != nil {
		return nil, err
	}
	out := []any{}
	if typ == CloudSQLResourceType {
		out = s.listReferences(loc).(map[string]any)["dataSourceReferences"].([]any)
	}
	return map[string]any{"dataSourceReferences": out}, nil
}

// testVaultPermissions answers every requested permission, as every
// CloudBurrow service does: nothing is enforced, so nothing is denied
// (internal/iampolicy).
func (s *Server) testVaultPermissions(r *http.Request, name string) (any, error) {
	var body struct {
		Permissions []string `json:"permissions"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	s.mu.Lock()
	v := s.st.Vaults[name]
	s.mu.Unlock()
	if v == nil {
		return nil, notFound("Backup vault %s does not exist.", name)
	}
	out := map[string]any{}
	if len(body.Permissions) > 0 {
		out["permissions"] = body.Permissions
	}
	return out, nil
}

// --- operations -------------------------------------------------------

// cancelOperation asks for an operation's cancellation: a running one is
// marked requestedCancellation and runs to its end, since a backup the
// Cloud SQL Admin API is taking cannot be stopped; a finished one is left
// as it is. Both answer google.protobuf.Empty, as google.longrunning's
// best-effort cancel does.
func (s *Server) cancelOperation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok {
		return nil, notFound("Operation %s does not exist.", name)
	}
	if op["done"] != true {
		op["metadata"].(map[string]any)["requestedCancellation"] = true
	}
	return map[string]any{}, nil
}

// deleteOperation forgets an operation; its work is not affected.
func (s *Server) deleteOperation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[name]; !ok {
		return nil, notFound("Operation %s does not exist.", name)
	}
	delete(s.ops, name)
	return map[string]any{}, nil
}
