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
	return "CloudBurrow's Backup and DR Service protects Cloud SQL instances (" + CloudSQLResourceType + "); " + typ +
		" is another workload type (Compute Engine instances and disks, AlloyDB, Filestore), whose servers CloudBurrow " +
		"does not run (docs/backupdr.md)"
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
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	switch {
	case body.ResourceType == "":
		return nil, invalid("backupPlan.resourceType is required.")
	case body.ResourceType != CloudSQLResourceType:
		return nil, unimplemented("a backup plan for "+body.ResourceType, workloadWhy(body.ResourceType))
	case body.Vault == "":
		return nil, invalid("backupPlan.backupVault is required.")
	case len(body.Rules) == 0:
		return nil, invalid("backupPlan.backupRules must have at least one rule.")
	}
	if err := checkRules(body.Rules); err != nil {
		return nil, err
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
	if s.st.Vaults[body.Vault] == nil {
		s.mu.Unlock()
		return nil, invalid("backupPlan.backupVault %s does not exist.", body.Vault)
	}
	now := s.stamp()
	p := &plan{Name: name, Description: body.Description, Labels: body.Labels, Vault: body.Vault, ResourceType: body.ResourceType,
		Rules: body.Rules, LogRetentionDays: logDays, CreateTime: now, UpdateTime: now}
	s.st.Plans[name] = p
	s.addRevisionLocked(p)
	err = s.saveLocked()
	out := s.planJSON(p)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "create", name, out, "google.cloud.backupdr.v1.BackupPlan", nil), nil
}

// checkRules validates a plan's backup rules.
func checkRules(rules []rule) error {
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
		if ru.RetentionDays <= 0 {
			return invalid("backupPlan.backupRules[%d].backupRetentionDays must be positive.", i)
		}
		if err := checkSchedule(i, ru.Schedule); err != nil {
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

func checkSchedule(i int, sch map[string]any) error {
	if sch == nil {
		return invalid("backupPlan.backupRules[%d].standardSchedule is required.", i)
	}
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
	if rt == "HOURLY" {
		if f, _ := sch["hourlyFrequency"].(float64); f < 1 {
			return invalid("backupPlan.backupRules[%d].standardSchedule.hourlyFrequency is required for an HOURLY rule.", i)
		}
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
	return map[string]any{"name": a.Name, "backupPlan": a.Plan, "resource": a.Resource, "resourceType": a.ResourceType,
		"dataSource": a.DataSource, "createTime": a.CreateTime, "updateTime": a.UpdateTime, "state": "ACTIVE",
		"rulesConfigInfo": infos, "backupPlanRevisionId": s.planRevision(a.Plan), "backupPlanRevisionName": a.Plan + "/revisions/" + s.planRevision(a.Plan),
		"cloudSqlInstanceBackupPlanAssociationProperties": map[string]any{
			"instanceCreateTime": s.st.DataSources[a.DataSource].info().CreateTime,
		}}
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
	case body.ResourceType != CloudSQLResourceType:
		return nil, unimplemented("a backup plan association for "+body.ResourceType, workloadWhy(body.ResourceType))
	case body.Resource == "":
		return nil, invalid("backupPlanAssociation.resource is required.")
	case body.Plan == "":
		return nil, invalid("backupPlanAssociation.backupPlan is required.")
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
		if ap, ai, _ := sqlResource(a.Resource); ap == project && ai == instance {
			s.mu.Unlock()
			return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "The Cloud SQL instance %s is already protected by %s.", instance, a.Name)
		}
	}
	now := s.stamp()
	// A data source in the plan's vault, for this instance: the one an
	// earlier association made, if its backups are still there.
	var ds *dataSource
	for _, d := range s.st.DataSources {
		if d.Project == project && d.Instance == instance && strings.HasPrefix(d.Name, p.Vault+"/") {
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
		if r, ok := s.sql.Run(b.RunID); ok && r.Status == "SUCCESSFUL" && (last == nil || b.CreateTime > last.CreateTime) {
			last = b
		}
	}
	res := "projects/" + d.Project + "/instances/" + d.Instance
	cfg := map[string]any{"lastBackupState": "FIRST_BACKUP_PENDING"}
	if last != nil {
		r, _ := s.sql.Run(last.RunID)
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
	out := map[string]any{"name": d.Name, "state": "ACTIVE", "configState": state, "backupCount": strconv.Itoa(count),
		"createTime": d.CreateTime, "updateTime": d.UpdateTime, "etag": strconv.Quote(d.UpdateTime), "totalStoredBytes": "0",
		"backupConfigInfo": cfg,
		"dataSourceGcpResource": map[string]any{"gcpResourcename": "//sqladmin.googleapis.com/" + res, "location": d.Info.Region,
			"type": CloudSQLResourceType, "cloudSqlInstanceDatasourceProperties": map[string]any{
				"name": res, "databaseInstalledVersion": d.Info.DatabaseVersion, "instanceCreateTime": d.Info.CreateTime,
				"instanceTier": d.Info.Tier}}}
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
	state, consistency := "CREATING", ""
	if r, ok := s.sql.Run(b.RunID); !ok {
		state = "ERROR"
	} else {
		switch r.Status {
		case "SUCCESSFUL":
			state, consistency = "ACTIVE", r.EndTime
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
			"type": CloudSQLResourceType}, "resourceSizeBytes": "0"}
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
	if r, ok := s.sql.Run(b.RunID); ok && r.Status == "RUNNING" {
		s.mu.Unlock()
		return nil, precondition("Backup %s is still being taken.", name)
	}
	delete(s.st.Backups, name)
	err := s.saveLocked()
	s.mu.Unlock()
	if err == nil {
		err = s.sql.DeleteRun(b.RunID)
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
	s.mu.Unlock()
	if b == nil {
		return nil, notFound("Backup %s does not exist.", name)
	}
	target := "a Compute Engine instance or disk"
	for k := range body {
		if strings.HasPrefix(k, "disk") || strings.HasPrefix(k, "regionDisk") {
			target = "a disk"
		}
	}
	return nil, unimplemented("backups.restore into "+target, workloadWhy("compute.googleapis.com/Instance")+
		"; a Cloud SQL backup is restored by Cloud SQL: sqladmin instances.restoreBackup with backupdrBackup "+name+
		", or instances.pointInTimeRestore with its data source")
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
	if body.RuleID == "" {
		// Without a rule, the plan's first rule, with the custom retention.
		s.mu.Lock()
		if a := s.st.Associations[name]; a != nil {
			if p := s.st.Plans[a.Plan]; p != nil && len(p.Rules) > 0 {
				body.RuleID = p.Rules[0].RuleID
			}
		}
		s.mu.Unlock()
		if body.RuleID == "" {
			return nil, invalid("ruleId is required.")
		}
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
	var ru *rule
	if p != nil {
		for i := range p.Rules {
			if p.Rules[i].RuleID == ruleID {
				ru = &p.Rules[i]
			}
		}
	}
	if ru == nil {
		s.mu.Unlock()
		return "", nil, invalid("Backup plan %s has no rule %q.", a.Plan, ruleID)
	}
	if retentionDays <= 0 {
		retentionDays = ru.RetentionDays
	}
	rev := p.revisionID()
	d := s.st.DataSources[a.DataSource]
	s.mu.Unlock()
	if d == nil {
		return "", nil, notFound("Data source %s does not exist.", a.DataSource)
	}
	runID, done, err := s.sql.Backup(d.Project, d.Instance, "Backup and DR "+assocName+" rule "+ruleID)
	if err != nil {
		return "", nil, err
	}
	now := s.now().UTC()
	b := &backup{Name: d.Name + "/backups/" + strconv.FormatInt(now.UnixNano(), 10), DataSource: d.Name, RunID: runID,
		Plan: a.Plan, RuleID: ruleID, CreateTime: now.Format(time.RFC3339Nano), Labels: labels, Revision: rev,
		ExpireTime: now.Add(time.Duration(retentionDays) * 24 * time.Hour).Format(time.RFC3339Nano)}
	if onDemand {
		b.RuleID = ""
		b.Description = "on demand, rule " + ruleID
	}
	opName, op := s.newOp(loc, "triggerBackup", assocName)
	s.mu.Lock()
	s.st.Backups[b.Name] = b
	a.LastBackup[ruleID] = lastBackup{State: "PENDING", Time: a.LastBackup[ruleID].Time}
	s.ops[opName] = op
	_ = s.saveLocked()
	out := copyMap(op)
	s.mu.Unlock()
	go func() {
		<-done
		r, _ := s.sql.Run(runID)
		var err error
		if r.Status != "SUCCESSFUL" {
			err = fmt.Errorf("the backup of Cloud SQL instance %s failed: %s", d.Instance, r.Error)
		}
		s.mu.Lock()
		if aa := s.st.Associations[assocName]; aa != nil {
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
	return d.Project, d.Instance, nil
}

// BackupRunID is the Cloud SQL backup run a backup holds.
func (s *Server) BackupRunID(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Backups[name]
	if b == nil {
		return "", fmt.Errorf("The Backup and DR backup %s does not exist.", name)
	}
	return b.RunID, nil
}
