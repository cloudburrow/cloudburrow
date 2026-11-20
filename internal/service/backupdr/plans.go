package backupdr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// workloadWhy is why a resource type other than a Cloud SQL instance is
// refused.
func workloadWhy(typ string) string {
	return "CloudBurrow's Backup and DR Service protects Cloud SQL instances (" + CloudSQLResourceType + ") and Compute Engine " +
		"instances and disks (" + ComputeInstanceResourceType + ", " + ComputeDiskResourceType + "); " + typ +
		" is another workload type (AlloyDB, Filestore, regional disks), whose servers CloudBurrow does not run (docs/backupdr.md)"
}

var recurrences = map[string]bool{"HOURLY": true, "DAILY": true, "WEEKLY": true, "MONTHLY": true, "YEARLY": true}

func (s *Server) planJSON(p *plan) map[string]any {
	rules := []any{}
	for _, r := range p.Rules {
		rules = append(rules, map[string]any{"ruleId": r.RuleID, "backupRetentionDays": r.RetentionDays, "standardSchedule": r.Schedule})
	}
	out := map[string]any{"name": p.Name, "backupVault": p.Vault, "resourceType": p.ResourceType, "backupRules": rules,
		"createTime": p.CreateTime, "updateTime": p.UpdateTime, "state": "ACTIVE", "etag": strconv.Quote(p.UpdateTime),
		"backupVaultServiceAccount": "service-backupdr@cloudburrow.iam.gserviceaccount.com",
		"supportedResourceTypes":    []string{p.ResourceType}, "revisionId": p.revisionID(), "revisionName": p.revisionName()}
	if p.LogRetentionDays > 0 {
		out["logRetentionDays"] = strconv.FormatInt(p.LogRetentionDays, 10)
	}
	if p.MaxOnDemandDays > 0 {
		out["maxCustomOnDemandRetentionDays"] = p.MaxOnDemandDays
	}
	if p.Description != "" {
		out["description"] = p.Description
	}
	if len(p.Labels) > 0 {
		out["labels"] = p.Labels
	}
	return out
}

func (s *Server) createPlan(r *http.Request, loc string) (any, error) {
	id := param(r, "backupPlanId")
	if err := checkID("backupPlanId", id); err != nil {
		return nil, err
	}
	var body struct {
		Description      string            `json:"description"`
		Labels           map[string]string `json:"labels"`
		Vault            string            `json:"backupVault"`
		ResourceType     string            `json:"resourceType"`
		Rules            []rule            `json:"backupRules"`
		LogRetentionDays json.RawMessage   `json:"logRetentionDays"`
		MaxOnDemandDays  int64             `json:"maxCustomOnDemandRetentionDays"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	switch {
	case body.ResourceType == "":
		return nil, invalid("backupPlan.resourceType is required.")
	case !servedType(body.ResourceType):
		return nil, unimplemented("a backup plan for "+body.ResourceType, workloadWhy(body.ResourceType))
	case body.Vault == "":
		return nil, invalid("backupPlan.backupVault is required.")
	}
	logDays, err := parseLogDays(body.LogRetentionDays)
	if err != nil {
		return nil, err
	}
	name := loc + "/backupPlans/" + id
	s.mu.Lock()
	if s.st.Plans[name] != nil {
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Backup plan %s already exists.", name)
	}
	v, err := s.planVaultLocked(name, body.Vault)
	if err == nil {
		err = checkPlanRetention(body.Rules, logDays, body.MaxOnDemandDays, v, body.ResourceType)
	}
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	now := s.stamp()
	p := &plan{Name: name, Description: body.Description, Labels: body.Labels, Vault: body.Vault, ResourceType: body.ResourceType,
		Rules: body.Rules, LogRetentionDays: logDays, MaxOnDemandDays: body.MaxOnDemandDays, CreateTime: now, UpdateTime: now}
	s.st.Plans[name] = p
	s.addRevisionLocked(p)
	s.planJobLocked("CREATE_BACKUP_PLAN", name, nil, p, nil, nil)
	err = s.saveLocked()
	out := s.planJSON(p)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "create", name, out, "google.cloud.backupdr.v1.BackupPlan", nil), nil
}

// planVaultLocked is a plan's vault, which must exist in the plan's
// project, and in its region or the multi-region spanning it (#1923):
// "A backup plan and its linked backup vault must reside in the same
// project." With s.mu held.
func (s *Server) planVaultLocked(planName, vaultName string) (*vault, error) {
	v := s.st.Vaults[vaultName]
	if v == nil {
		return nil, invalid("backupPlan.backupVault %s does not exist.", vaultName)
	}
	pp, vp := strings.Split(planName, "/"), strings.Split(vaultName, "/")
	if pp[1] != vp[1] {
		return nil, invalid("backupPlan.backupVault %s is in project %s: a backup plan and its backup vault must be in the same project, %s.",
			vaultName, vp[1], pp[1])
	}
	if !vaultServes(vp[3], pp[3]) {
		return nil, invalid("backupPlan.backupVault %s is in %s: a backup plan in %s needs a backup vault in %s or in the multi-region that spans it.",
			vaultName, vp[3], pp[3], pp[3])
	}
	return v, nil
}

// maxRetentionDays is the longest custom on-demand retention: "Minimum
// value is 1 and maximum value is 36159 for custom retention on-demand
// backup" (BackupRule.backupRetentionDays).
const maxRetentionDays = 36159

// checkPlanRetention checks a plan's rules and retention against its
// vault (#1923): every rule's retention, and the log retention, at least
// the vault's minimum enforced retention; no rules only with a maximum
// custom on-demand retention.
func checkPlanRetention(rules []rule, logDays, maxOnDemand int64, v *vault, typ string) error {
	if maxOnDemand < 0 || maxOnDemand > maxRetentionDays {
		return invalid("backupPlan.maxCustomOnDemandRetentionDays %d must be 1 to %d.", maxOnDemand, maxRetentionDays)
	}
	if len(rules) == 0 && maxOnDemand == 0 {
		return invalid("backupPlan.backupRules must have at least one rule, unless backupPlan.maxCustomOnDemandRetentionDays is set.")
	}
	if len(rules) > 0 {
		if err := checkRules(rules, typ); err != nil {
			return err
		}
	}
	min := v.minRetentionDays()
	for i, r := range rules {
		if r.RetentionDays < min {
			return invalid("backupPlan.backupRules[%d].backupRetentionDays %d is less than the backup vault's minimum enforced retention, %d days.",
				i, r.RetentionDays, min)
		}
	}
	if logDays > 0 && logDays < min {
		return invalid("backupPlan.logRetentionDays %d is less than the backup vault's minimum enforced retention, %d days.", logDays, min)
	}
	if maxOnDemand > 0 && maxOnDemand < min {
		return invalid("backupPlan.maxCustomOnDemandRetentionDays %d is less than the backup vault's minimum enforced retention, %d days.", maxOnDemand, min)
	}
	return nil
}

// maxOnDemandLocked is the longest custom retention an on-demand backup
// under p may have: its maxCustomOnDemandRetentionDays, or "the vault's
// minimum enforced retention plus 30 days". With s.mu held.
func (s *Server) maxOnDemandLocked(p *plan) int64 {
	if p.MaxOnDemandDays > 0 {
		return p.MaxOnDemandDays
	}
	if v := s.st.Vaults[p.Vault]; v != nil {
		return v.minRetentionDays() + 30
	}
	return 30
}

// checkRules validates a plan's backup rules.
func checkRules(rules []rule, typ string) error {
	if len(rules) == 0 {
		return invalid("backupPlan.backupRules must have at least one rule.")
	}
	seen := map[string]bool{}
	for i, ru := range rules {
		if err := checkID(fmt.Sprintf("backupPlan.backupRules[%d].ruleId", i), ru.RuleID); err != nil {
			return err
		}
		if seen[ru.RuleID] {
			return invalid("backupPlan.backupRules: ruleId %q is repeated.", ru.RuleID)
		}
		seen[ru.RuleID] = true
		if ru.RetentionDays <= 0 || ru.RetentionDays > maxRetentionDays {
			return invalid("backupPlan.backupRules[%d].backupRetentionDays must be 1 to %d.", i, maxRetentionDays)
		}
		if err := checkSchedule(i, ru.Schedule, typ); err != nil {
			return err
		}
	}
	return nil
}

// parseLogDays parses logRetentionDays, an int64 as a JSON string or number.
func parseLogDays(raw json.RawMessage) (int64, error) {
	v := strings.Trim(string(raw), `"`)
	if v == "" || v == "null" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, invalid("backupPlan.logRetentionDays %q is not a number of days.", raw)
	}
	return n, nil
}

func checkSchedule(i int, sch map[string]any, typ string) error {
	if sch == nil {
		return invalid("backupPlan.backupRules[%d].standardSchedule is required.", i)
	}
	enumNames(sch)
	rt, _ := sch["recurrenceType"].(string)
	if !recurrences[rt] {
		return invalid("backupPlan.backupRules[%d].standardSchedule.recurrenceType %q is not one of HOURLY, DAILY, WEEKLY, MONTHLY, YEARLY.", i, rt)
	}
	tz, _ := sch["timeZone"].(string)
	if tz == "" {
		return invalid("backupPlan.backupRules[%d].standardSchedule.timeZone is required.", i)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return invalid("backupPlan.backupRules[%d].standardSchedule.timeZone %q is not an IANA time zone.", i, tz)
	}
	if err := checkScheduleDays(i, rt, sch, typ); err != nil {
		return err
	}
	if w, ok := sch["backupWindow"].(map[string]any); ok {
		st, _ := w["startHourOfDay"].(float64)
		en, _ := w["endHourOfDay"].(float64)
		if st < 0 || st > 23 || en < 1 || en > 24 || en <= st {
			return invalid("backupPlan.backupRules[%d].standardSchedule.backupWindow must start at hour 0 to 23 and end after it, by hour 24.", i)
		}
	}
	return nil
}

func (s *Server) getPlan(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Plans[name]
	if p == nil {
		return nil, notFound("Backup plan %s does not exist.", name)
	}
	return s.planJSON(p), nil
}

func (s *Server) listPlans(loc string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, n := range sortedKeys(s.st.Plans) {
		if under(n, loc, "/backupPlans/") {
			out = append(out, s.planJSON(s.st.Plans[n]))
		}
	}
	return map[string]any{"backupPlans": out}
}

func (s *Server) deletePlan(name string) (any, error) {
	loc := name[:strings.Index(name, "/backupPlans/")]
	s.mu.Lock()
	if s.st.Plans[name] == nil {
		s.mu.Unlock()
		return nil, notFound("Backup plan %s does not exist.", name)
	}
	for _, a := range s.st.Associations {
		if a.Plan == name {
			s.mu.Unlock()
			return nil, precondition("Backup plan %s is applied by backup plan association %s; delete the association first.", name, a.Name)
		}
	}
	s.planJobLocked("DELETE_BACKUP_PLAN", name, s.st.Plans[name], nil, nil, nil)
	delete(s.st.Plans, name)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "delete", name, nil, "", nil), nil
}

// --- associations and data sources ------------------------------------

// sqlResource parses a Cloud SQL instance's resource name,
// projects/{project}/instances/{instance}, with or without the
// //sqladmin.googleapis.com/ prefix.
func sqlResource(res string) (project, instance string, ok bool) {
	res = strings.TrimPrefix(res, "//sqladmin.googleapis.com/")
	p := strings.Split(res, "/")
	if len(p) != 4 || p[0] != "projects" || p[2] != "instances" || p[1] == "" || p[3] == "" {
		return "", "", false
	}
	return p[1], p[3], true
}

func (s *Server) associationJSON(a *association) map[string]any {
	infos := []any{}
	if p := s.st.Plans[a.Plan]; p != nil {
		for _, r := range p.Rules {
			lb := a.LastBackup[r.RuleID]
			info := map[string]any{"ruleId": r.RuleID, "lastBackupState": "FIRST_BACKUP_PENDING"}
			if lb.State != "" {
				info["lastBackupState"] = lb.State
			}
			if lb.Time != "" {
				info["lastSuccessfulBackupConsistencyTime"] = lb.Time
			}
			if lb.Error != "" {
				info["lastBackupError"] = map[string]any{"code": 13, "message": lb.Error}
			}
			infos = append(infos, info)
		}
	}
	out := map[string]any{"name": a.Name, "backupPlan": a.Plan, "resource": a.Resource, "resourceType": a.ResourceType,
		"dataSource": a.DataSource, "createTime": a.CreateTime, "updateTime": a.UpdateTime, "state": "ACTIVE",
		"rulesConfigInfo": infos, "backupPlanRevisionId": s.planRevision(a.Plan), "backupPlanRevisionName": a.Plan + "/revisions/" + s.planRevision(a.Plan)}
	if a.ResourceType == CloudSQLResourceType {
		out["cloudSqlInstanceBackupPlanAssociationProperties"] = map[string]any{
			"instanceCreateTime": s.st.DataSources[a.DataSource].info().CreateTime,
		}
	}
	return out
}

func (d *dataSource) info() Instance {
	if d == nil {
		return Instance{}
	}
	return d.Info
}

func (s *Server) createAssociation(r *http.Request, loc string) (any, error) {
	id := param(r, "backupPlanAssociationId")
	if err := checkID("backupPlanAssociationId", id); err != nil {
		return nil, err
	}
	var body struct {
		Plan         string `json:"backupPlan"`
		Resource     string `json:"resource"`
		ResourceType string `json:"resourceType"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	switch {
	case body.ResourceType == "":
		return nil, invalid("backupPlanAssociation.resourceType is required.")
	case !servedType(body.ResourceType):
		return nil, unimplemented("a backup plan association for "+body.ResourceType, workloadWhy(body.ResourceType))
	case body.Resource == "":
		return nil, invalid("backupPlanAssociation.resource is required.")
	case body.Plan == "":
		return nil, invalid("backupPlanAssociation.backupPlan is required.")
	case body.ResourceType != CloudSQLResourceType:
		return s.createComputeAssociation(loc, id, body.Plan, body.Resource, body.ResourceType)
	}
	project, instance, ok := sqlResource(body.Resource)
	if !ok {
		return nil, invalid("backupPlanAssociation.resource %q is not a Cloud SQL instance: the format is projects/{project}/instances/{instance}.", body.Resource)
	}
	info, ok := s.sql.Instance(project, instance)
	if !ok {
		return nil, notFound("The Cloud SQL instance %s does not exist in project %s.", instance, project)
	}
	name := loc + "/backupPlanAssociations/" + id
	s.mu.Lock()
	p := s.st.Plans[body.Plan]
	switch {
	case s.st.Associations[name] != nil:
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Backup plan association %s already exists.", name)
	case p == nil:
		s.mu.Unlock()
		return nil, invalid("backupPlanAssociation.backupPlan %s does not exist.", body.Plan)
	case p.ResourceType != body.ResourceType:
		s.mu.Unlock()
		return nil, invalid("Backup plan %s protects %s, not %s.", p.Name, p.ResourceType, body.ResourceType)
	}
	for _, a := range s.st.Associations {
		if ap, ai, _ := sqlResource(a.Resource); a.ResourceType == CloudSQLResourceType && ap == project && ai == instance {
			s.mu.Unlock()
			return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "The Cloud SQL instance %s is already protected by %s.", instance, a.Name)
		}
	}
	now := s.stamp()
	// A data source in the plan's vault, for this instance: the one an
	// earlier association made, if its backups are still there.
	var ds *dataSource
	for _, d := range s.st.DataSources {
		if d.Kind == "" && d.Project == project && d.Instance == instance && strings.HasPrefix(d.Name, p.Vault+"/") {
			ds = d
		}
	}
	if ds == nil {
		ds = &dataSource{Name: p.Vault + "/dataSources/ds-" + newID()[:20], Project: project, Instance: instance, CreateTime: now}
		s.st.DataSources[ds.Name] = ds
	}
	ds.Association, ds.Info, ds.UpdateTime = name, info, now
	a := &association{Name: name, Plan: p.Name, Resource: "projects/" + project + "/instances/" + instance,
		ResourceType: body.ResourceType, DataSource: ds.Name, CreateTime: now, UpdateTime: now, LastBackup: map[string]lastBackup{}}
	s.st.Associations[name] = a
	s.planJobLocked("CREATE_BACKUP_PLAN_ASSOCIATION", name, nil, p, a, nil)
	err := s.saveLocked()
	out := s.associationJSON(a)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "create", name, out, "google.cloud.backupdr.v1.BackupPlanAssociation", nil), nil
}

func (s *Server) getAssociation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Associations[name]
	if a == nil {
		return nil, notFound("Backup plan association %s does not exist.", name)
	}
	out := s.associationJSON(a)
	return out, nil
}

func (s *Server) listAssociations(loc string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, n := range sortedKeys(s.st.Associations) {
		if under(n, loc, "/backupPlanAssociations/") {
			out = append(out, s.associationJSON(s.st.Associations[n]))
		}
	}
	return map[string]any{"backupPlanAssociations": out}
}

// deleteAssociation stops protecting the instance. Its data source and the
// backups in it stay in the vault, as Google keeps them, until they
// expire or are deleted.
func (s *Server) deleteAssociation(name string) (any, error) {
	loc := name[:strings.Index(name, "/backupPlanAssociations/")]
	s.mu.Lock()
	a := s.st.Associations[name]
	if a == nil {
		s.mu.Unlock()
		return nil, notFound("Backup plan association %s does not exist.", name)
	}
	s.planJobLocked("DELETE_BACKUP_PLAN_ASSOCIATION", name, s.st.Plans[a.Plan], nil, a, nil)
	if d := s.st.DataSources[a.DataSource]; d != nil {
		d.Association, d.UpdateTime = "", s.stamp()
	}
	delete(s.st.Associations, name)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "delete", name, nil, "", nil), nil
}

func (s *Server) dataSourceJSON(d *dataSource) map[string]any {
	count := 0
	var last *backup
	for _, b := range s.st.Backups {
		if b.DataSource != d.Name {
			continue
		}
		count++
		if r, ok := s.runLocked(b.RunID); ok && r.Status == "SUCCESSFUL" && (last == nil || b.CreateTime > last.CreateTime) {
			last = b
		}
	}
	res := "projects/" + d.Project + "/instances/" + d.Instance
	cfg := map[string]any{"lastBackupState": "FIRST_BACKUP_PENDING"}
	if last != nil {
		r, _ := s.runLocked(last.RunID)
		cfg["lastBackupState"], cfg["lastSuccessfulBackupConsistencyTime"] = "SUCCEEDED", r.EndTime
	}
	state := "PASSIVE"
	if a := s.st.Associations[d.Association]; a != nil {
		state = "ACTIVE"
		rules := []any{}
		if p := s.st.Plans[a.Plan]; p != nil {
			for _, r := range p.Rules {
				rules = append(rules, r.RuleID)
			}
		}
		cfg["gcpBackupConfig"] = map[string]any{"backupPlan": a.Plan, "backupPlanAssociation": a.Name, "backupPlanRules": rules,
			"backupPlanRevisionId": s.planRevision(a.Plan), "backupPlanRevisionName": a.Plan + "/revisions/" + s.planRevision(a.Plan)}
	}
	gcp := map[string]any{"gcpResourcename": "//sqladmin.googleapis.com/" + res, "location": d.Info.Region,
		"type": CloudSQLResourceType, "cloudSqlInstanceDatasourceProperties": map[string]any{
			"name": res, "databaseInstalledVersion": d.Info.DatabaseVersion, "instanceCreateTime": d.Info.CreateTime,
			"instanceTier": d.Info.Tier}}
	if d.Kind != "" {
		gcp = d.computeGcpResource()
	}
	out := map[string]any{"name": d.Name, "state": "ACTIVE", "configState": state, "backupCount": strconv.Itoa(count),
		"createTime": d.CreateTime, "updateTime": d.UpdateTime, "etag": strconv.Quote(d.UpdateTime),
		"totalStoredBytes":      strconv.FormatInt(s.storedBytes(d.Name+"/"), 10),
		"backupConfigInfo":      cfg,
		"dataSourceGcpResource": gcp}
	if len(d.Labels) > 0 {
		out["labels"] = d.Labels
	}
	return out
}

func (s *Server) listDataSources(vaultName string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Vaults[vaultName] == nil && !strings.Contains(vaultName+"/", "/-/") {
		return nil, notFound("Backup vault %s does not exist.", vaultName)
	}
	out := []any{}
	for _, n := range sortedKeys(s.st.DataSources) {
		if under(n, vaultName, "/dataSources/") {
			out = append(out, s.dataSourceJSON(s.st.DataSources[n]))
		}
	}
	return map[string]any{"dataSources": out}, nil
}

func (s *Server) getDataSource(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.st.DataSources[name]
	if d == nil {
		return nil, notFound("Data source %s does not exist.", name)
	}
	return s.dataSourceJSON(d), nil
}

// --- backups ----------------------------------------------------------

func (s *Server) backupJSON(b *backup) map[string]any {
	d := s.st.DataSources[b.DataSource]
	info := d.info()
	state, consistency, size := "CREATING", "", int64(0)
	if r, ok := s.runLocked(b.RunID); !ok {
		state = "ERROR"
	} else {
		switch r.Status {
		case "SUCCESSFUL":
			state, consistency, size = "ACTIVE", r.EndTime, r.SizeBytes
		case "FAILED":
			state = "ERROR"
		}
	}
	typ := "SCHEDULED"
	if b.RuleID == "" {
		typ = "ON_DEMAND"
	}
	res := ""
	if d != nil {
		res = "projects/" + d.Project + "/instances/" + d.Instance
	}
	updated := b.CreateTime
	if b.UpdateTime != "" {
		updated = b.UpdateTime
	}
	out := map[string]any{"name": b.Name, "state": state, "backupType": typ, "createTime": b.CreateTime, "updateTime": updated,
		"expireTime": b.ExpireTime, "enforcedRetentionEndTime": b.enforcedEnd(), "etag": strconv.Quote(updated),
		"cloudSqlInstanceBackupProperties": map[string]any{"databaseInstalledVersion": info.DatabaseVersion, "sourceInstance": res,
			"instanceTier": info.Tier, "instanceCreateTime": info.CreateTime},
		"gcpBackupPlanInfo": map[string]any{"backupPlan": b.Plan, "backupPlanRuleId": b.RuleID,
			"backupPlanRevisionId": b.revision(), "backupPlanRevisionName": b.Plan + "/revisions/" + b.revision()},
		"gcpResource": map[string]any{"gcpResourcename": "//sqladmin.googleapis.com/" + res, "location": info.Region,
			"type": CloudSQLResourceType}, "resourceSizeBytes": strconv.FormatInt(size, 10)}
	if d != nil && d.Kind != "" {
		s.computeBackupJSON(b, d, out)
	}
	if consistency != "" {
		out["consistencyTime"] = consistency
	}
	if b.Description != "" {
		out["description"] = b.Description
	}
	if len(b.Labels) > 0 {
		out["labels"] = b.Labels
	}
	return out
}

func (s *Server) listBackups(dsName string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.DataSources[dsName] == nil && !strings.Contains(dsName+"/", "/-/") {
		return nil, notFound("Data source %s does not exist.", dsName)
	}
	out := []any{}
	for _, n := range sortedKeys(s.st.Backups) {
		b := s.st.Backups[n]
		if match(dsName, b.DataSource) {
			out = append(out, s.backupJSON(b))
		}
	}
	return map[string]any{"backups": out}, nil
}

func (s *Server) getBackup(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Backups[name]
	if b == nil {
		return nil, notFound("Backup %s does not exist.", name)
	}
	return s.backupJSON(b), nil
}

func (s *Server) deleteBackup(name string) (any, error) {
	loc := name[:strings.Index(name, "/backupVaults/")]
	s.mu.Lock()
	b := s.st.Backups[name]
	if b == nil {
		s.mu.Unlock()
		return nil, notFound("Backup %s does not exist.", name)
	}
	if r, ok := s.runLocked(b.RunID); ok && r.Status == "RUNNING" {
		s.mu.Unlock()
		return nil, precondition("Backup %s is still being taken.", name)
	}
	// "You can delete a backup only after it exceeds its minimum enforced
	// retention period" (bv-backup-manage, #1844).
	if end, err := time.Parse(time.RFC3339Nano, b.enforcedEnd()); err == nil && s.now().Before(end) {
		s.mu.Unlock()
		return nil, precondition("Backup %s cannot be deleted before its enforced retention ends, at %s.", name, b.enforcedEnd())
	}
	delete(s.st.Backups, name)
	err := s.saveLocked()
	s.mu.Unlock()
	if err == nil {
		err = s.deleteRun(b.RunID)
	}
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "delete", name, nil, "", nil), nil
}

// restoreBackup is backups.restore, whose targets are Compute Engine
// instances and disks: a Cloud SQL backup is restored by Cloud SQL.
func (s *Server) restoreBackup(r *http.Request, name string) (any, error) {
	var body map[string]any
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	s.mu.Lock()
	b := s.st.Backups[name]
	var d *dataSource
	if b != nil {
		d = s.st.DataSources[b.DataSource]
	}
	s.mu.Unlock()
	if b == nil {
		return nil, notFound("Backup %s does not exist.", name)
	}
	if d != nil && d.Kind != "" {
		return s.restoreCompute(name, body)
	}
	target := "a Compute Engine instance or disk"
	for k := range body {
		if strings.HasPrefix(k, "disk") || strings.HasPrefix(k, "regionDisk") {
			target = "a disk"
		}
	}
	return nil, invalid("Backup %s is a Cloud SQL backup, which backups.restore does not restore into %s: Cloud SQL restores it, "+
		"with sqladmin instances.restoreBackup and backupdrBackup %s, or instances.pointInTimeRestore with its data source.", name, target, name)
}

// triggerBackup takes an on-demand backup under one of the plan's rules.
func (s *Server) triggerBackup(r *http.Request, name string) (any, error) {
	var body struct {
		RuleID              string            `json:"ruleId"`
		CustomRetentionDays int64             `json:"customRetentionDays"`
		Labels              map[string]string `json:"labels"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	// customRetentionDays "is mutually exclusive with rule_id. This field
	// is required if rule_id is not provided" (TriggerBackupRequest).
	switch {
	case body.RuleID != "" && body.CustomRetentionDays != 0:
		return nil, invalid("ruleId and customRetentionDays are mutually exclusive: give one.")
	case body.RuleID == "" && body.CustomRetentionDays == 0:
		return nil, invalid("customRetentionDays is required when ruleId is not given.")
	case body.CustomRetentionDays < 0 || body.CustomRetentionDays > maxRetentionDays:
		return nil, invalid("customRetentionDays %d must be 1 to %d.", body.CustomRetentionDays, maxRetentionDays)
	}
	_, op, err := s.backup(name, body.RuleID, true, body.CustomRetentionDays, body.Labels)
	return op, err
}

// backup starts a backup of the association's instance under ruleID and
// returns its operation, which finishes when the backup is taken.
func (s *Server) backup(assocName, ruleID string, onDemand bool, retentionDays int64, labels map[string]string) (string, map[string]any, error) {
	loc := assocName[:strings.Index(assocName, "/backupPlanAssociations/")]
	s.mu.Lock()
	a := s.st.Associations[assocName]
	if a == nil {
		s.mu.Unlock()
		return "", nil, notFound("Backup plan association %s does not exist.", assocName)
	}
	p := s.st.Plans[a.Plan]
	if p == nil {
		s.mu.Unlock()
		return "", nil, notFound("Backup plan %s does not exist.", a.Plan)
	}
	var ru *rule
	for i := range p.Rules {
		if p.Rules[i].RuleID == ruleID {
			ru = &p.Rules[i]
		}
	}
	switch {
	case ruleID != "" && ru == nil:
		s.mu.Unlock()
		return "", nil, invalid("Backup plan %s has no rule %q.", a.Plan, ruleID)
	case ruleID == "":
		// An on-demand backup with custom retention (#1923), capped by
		// the plan's maximum custom on-demand retention.
		if max := s.maxOnDemandLocked(p); retentionDays > max {
			s.mu.Unlock()
			return "", nil, invalid("customRetentionDays %d is more than backup plan %s's maximum custom on-demand retention, %d days.",
				retentionDays, a.Plan, max)
		}
		if v := s.st.Vaults[p.Vault]; v != nil && retentionDays < v.minRetentionDays() {
			s.mu.Unlock()
			return "", nil, invalid("customRetentionDays %d is less than the backup vault's minimum enforced retention, %d days.",
				retentionDays, v.minRetentionDays())
		}
	default:
		retentionDays = ru.RetentionDays
	}
	rev := p.revisionID()
	d := s.st.DataSources[a.DataSource]
	vlt := s.st.Vaults[p.Vault]
	s.mu.Unlock()
	if d == nil {
		return "", nil, notFound("Data source %s does not exist.", a.DataSource)
	}
	if err := s.checkVaultKeyFor(d.Name); err != nil {
		return "", nil, err
	}
	if d.Kind != "" {
		return s.computeBackup(assocName, a.Plan, ruleID, onDemand, retentionDays, labels, rev, d, vlt)
	}
	cat := "SCHEDULED_BACKUP"
	if onDemand {
		cat = "ON_DEMAND_BACKUP"
	}
	jobPayload := map[string]any{"jobCategory": cat, "resourceType": "Cloud SQL", "backupRule": ruleID,
		"sourceResourceName": "projects/" + d.Project + "/instances/" + d.Instance, "sourceResourceId": d.Instance,
		"sourceResourceLocation": d.Info.Region, "backupVaultName": vaultOf(d.Name), "backupPlanName": a.Plan, "dataSourceName": d.Name}
	desc := "Backup and DR " + assocName + " rule " + ruleID
	if ruleID == "" {
		desc = "Backup and DR " + assocName + " on demand"
	}
	runID, done, err := s.sql.Backup(d.Project, d.Instance, desc)
	if err != nil {
		s.mu.Lock()
		s.jobDoneLocked(BackupRestoreJobsLog, d.Project, locationOfName(d.Name), jobPayload, err)
		_ = s.saveLocked()
		s.mu.Unlock()
		return "", nil, err
	}
	now := s.now().UTC()
	b := &backup{Name: d.Name + "/backups/" + strconv.FormatInt(now.UnixNano(), 10), DataSource: d.Name, RunID: runID,
		Plan: a.Plan, RuleID: ruleID, CreateTime: now.Format(time.RFC3339Nano), Labels: labels, Revision: rev,
		ExpireTime: now.Add(time.Duration(retentionDays) * 24 * time.Hour).Format(time.RFC3339Nano)}
	// The enforced retention end (#1921): the vault's minimum enforced
	// retention from now, or the expire time with MATCH_BACKUP_EXPIRE_TIME.
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
	s.st.Backups[b.Name] = b
	if ruleID != "" {
		a.LastBackup[ruleID] = lastBackup{State: "PENDING", Time: a.LastBackup[ruleID].Time}
	}
	s.ops[opName] = op
	_ = s.saveLocked()
	out := copyMap(op)
	s.mu.Unlock()
	go func() {
		<-done
		r, _ := s.runLocked(runID)
		var err error
		if r.Status != "SUCCESSFUL" {
			err = fmt.Errorf("the backup of Cloud SQL instance %s failed: %s", d.Instance, r.Error)
		}
		s.mu.Lock()
		var jerr error
		if err != nil {
			jerr = errorf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
		}
		if r.Status == "SUCCESSFUL" {
			// A full dump each time: the "incremental" size is the dump's.
			job.Payload["incrementalBackupSizeGib"] = float64(r.SizeBytes) / (1 << 30)
		}
		s.finishJobLocked(job, jerr)
		if aa := s.st.Associations[assocName]; aa != nil && ruleID != "" {
			lb := aa.LastBackup[ruleID]
			if err != nil {
				lb.State, lb.Error = "FAILED", err.Error()
			} else {
				lb.State, lb.Time, lb.Error = "SUCCEEDED", r.EndTime, ""
			}
			aa.LastBackup[ruleID] = lb
		}
		var resp map[string]any
		if aa := s.st.Associations[assocName]; aa != nil {
			resp = s.associationJSON(aa)
		}
		s.finishOp(op, resp, "google.cloud.backupdr.v1.BackupPlanAssociation", err)
		_ = s.saveLocked()
		s.mu.Unlock()
	}()
	return opName, out, nil
}

// --- what Cloud SQL resolves -----------------------------------------

// DataSourceInstance is the project and Cloud SQL instance a data source
// protects.
func (s *Server) DataSourceInstance(name string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.st.DataSources[name]
	if d == nil {
		return "", "", fmt.Errorf("The Backup and DR data source %s does not exist.", name)
	}
	if d.Kind != "" {
		return "", "", fmt.Errorf("The Backup and DR data source %s protects a %s, not a Cloud SQL instance.", name, d.Kind)
	}
	return d.Project, d.Instance, nil
}

// BackupRunID is the Cloud SQL backup run a backup holds, for a restore:
// an error while its vault's customer-managed key cannot be used.
func (s *Server) BackupRunID(name string) (string, error) {
	if err := s.checkVaultKeyFor(name); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Backups[name]
	if b == nil {
		return "", fmt.Errorf("The Backup and DR backup %s does not exist.", name)
	}
	if strings.HasPrefix(b.RunID, computeRunPrefix) {
		return "", fmt.Errorf("The Backup and DR backup %s is a Compute Engine backup, not a Cloud SQL one: restore it with backups.restore.", name)
	}
	return b.RunID, nil
}

var (
	weekdays   = map[string]bool{"MONDAY": true, "TUESDAY": true, "WEDNESDAY": true, "THURSDAY": true, "FRIDAY": true, "SATURDAY": true, "SUNDAY": true}
	monthNames = map[string]bool{"JANUARY": true, "FEBRUARY": true, "MARCH": true, "APRIL": true, "MAY": true, "JUNE": true, "JULY": true,
		"AUGUST": true, "SEPTEMBER": true, "OCTOBER": true, "NOVEMBER": true, "DECEMBER": true}
	weeksOfMonth = map[string]bool{"FIRST": true, "SECOND": true, "THIRD": true, "FOURTH": true, "LAST": true}
)

// checkScheduleDays checks the fields that say when a rule runs, as
// StandardSchedule documents them (#1923): hourlyFrequency for HOURLY only,
// 6 to 23 for a Cloud SQL instance; daysOfWeek required for WEEKLY and
// only there; daysOfMonth or weekDayOfMonth for MONTHLY and YEARLY only;
// months for YEARLY only, and required there.
func checkScheduleDays(i int, rt string, sch map[string]any, typ string) error {
	f := fmt.Sprintf("backupPlan.backupRules[%d].standardSchedule.", i)
	list := func(k string) []any { l, _ := sch[k].([]any); return l }
	_, hasHourly := sch["hourlyFrequency"]
	wdm, hasWDM := sch["weekDayOfMonth"].(map[string]any)
	switch {
	case rt == "HOURLY":
		h, _ := sch["hourlyFrequency"].(float64)
		if !hasHourly {
			return invalid("%shourlyFrequency is required for an HOURLY rule.", f)
		}
		// "compute.googleapis.com/Instance: 1-23 ... compute.googleapis.com/Disk:
		// 1-23 ... sqladmin.googleapis.com/Instance: 6-23" (StandardSchedule).
		lo := 6.0
		if typ == ComputeInstanceResourceType || typ == ComputeDiskResourceType {
			lo = 1
		}
		if typ == "" {
			typ = CloudSQLResourceType
		}
		if h < lo || h > 23 || h != float64(int(h)) {
			return invalid("%shourlyFrequency %v is not supported: for %s it must be %v to 23.", f, sch["hourlyFrequency"], typ, lo)
		}
	case hasHourly:
		return invalid("%shourlyFrequency applies to an HOURLY rule only, not %s.", f, rt)
	}
	days := list("daysOfWeek")
	switch {
	case rt == "WEEKLY" && len(days) == 0:
		return invalid("%sdaysOfWeek is required for a WEEKLY rule.", f)
	case rt != "WEEKLY" && len(days) > 0:
		return invalid("%sdaysOfWeek applies to a WEEKLY rule only, not %s.", f, rt)
	}
	for _, d := range days {
		if s, _ := d.(string); !weekdays[s] {
			return invalid("%sdaysOfWeek %v is not a day of the week such as MONDAY.", f, d)
		}
	}
	dom := list("daysOfMonth")
	monthly := rt == "MONTHLY" || rt == "YEARLY"
	if !monthly && (len(dom) > 0 || hasWDM) {
		return invalid("%sdaysOfMonth and weekDayOfMonth apply to a MONTHLY or YEARLY rule only, not %s.", f, rt)
	}
	for _, d := range dom {
		if n, _ := d.(float64); n < 1 || n > 31 || n != float64(int(n)) {
			return invalid("%sdaysOfMonth %v is not a day of the month, 1 to 31.", f, d)
		}
	}
	if hasWDM {
		w, _ := wdm["weekOfMonth"].(string)
		d, _ := wdm["dayOfWeek"].(string)
		if !weeksOfMonth[w] || !weekdays[d] {
			return invalid("%sweekDayOfMonth needs weekOfMonth (FIRST, SECOND, THIRD, FOURTH or LAST) and dayOfWeek (such as MONDAY).", f)
		}
		if len(dom) > 0 {
			return invalid("%sgive daysOfMonth or weekDayOfMonth, not both.", f)
		}
	}
	if monthly && len(dom) == 0 && !hasWDM {
		return invalid("%sdaysOfMonth or weekDayOfMonth is required for a %s rule.", f, rt)
	}
	months := list("months")
	switch {
	case rt == "YEARLY" && len(months) == 0:
		return invalid("%smonths is required for a YEARLY rule.", f)
	case rt != "YEARLY" && len(months) > 0:
		return invalid("%smonths applies to a YEARLY rule only, not %s.", f, rt)
	}
	for _, m := range months {
		if s, _ := m.(string); !monthNames[s] {
			return invalid("%smonths %v is not a month such as JANUARY.", f, m)
		}
	}
	return nil
}

// vaultOf is the vault a data source's name is under.
func vaultOf(name string) string {
	if i := strings.Index(name, "/dataSources/"); i >= 0 {
		return name[:i]
	}
	return name
}

// locationOfName is the location segment of projects/{p}/locations/{l}/....
func locationOfName(name string) string {
	if p := strings.Split(name, "/"); len(p) > 3 {
		return p[3]
	}
	return ""
}

// The schedule's enums by number: the REST clients of
// cloud.google.com/go/backupdr send enums as numbers (protojson with
// UseEnumNumbers), which Google's API accepts as it does their names.
var (
	recurrenceEnum = []string{"RECURRENCE_TYPE_UNSPECIFIED", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"}
	dayEnum        = []string{"DAY_OF_WEEK_UNSPECIFIED", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}
	monthEnum      = []string{"MONTH_UNSPECIFIED", "JANUARY", "FEBRUARY", "MARCH", "APRIL", "MAY", "JUNE", "JULY", "AUGUST",
		"SEPTEMBER", "OCTOBER", "NOVEMBER", "DECEMBER"}
	weekEnum = []string{"WEEK_OF_MONTH_UNSPECIFIED", "FIRST", "SECOND", "THIRD", "FOURTH", "LAST"}
)

// enumName is v as its enum's name when it is a number.
func enumName(v any, names []string) any {
	if n, ok := v.(float64); ok && n >= 0 && int(n) < len(names) && n == float64(int(n)) {
		return names[int(n)]
	}
	return v
}

// enumNames rewrites a standardSchedule's numeric enums as their names.
func enumNames(sch map[string]any) {
	if v, ok := sch["recurrenceType"]; ok {
		sch["recurrenceType"] = enumName(v, recurrenceEnum)
	}
	for k, names := range map[string][]string{"daysOfWeek": dayEnum, "months": monthEnum} {
		if l, ok := sch[k].([]any); ok {
			for i := range l {
				l[i] = enumName(l[i], names)
			}
		}
	}
	if w, ok := sch["weekDayOfMonth"].(map[string]any); ok {
		w["weekOfMonth"] = enumName(w["weekOfMonth"], weekEnum)
		w["dayOfWeek"] = enumName(w["dayOfWeek"], dayEnum)
	}
}
