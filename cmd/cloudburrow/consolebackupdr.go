package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// backupDRProvider is the Backup and DR page (#1433): a project's backup
// vaults, each vault's page with its data sources, the backup plans (with
// their rules) that store into it and the associations applying them, and
// a data source's page with its backups. Create vault, Create backup plan,
// Associate an instance and Trigger backup go through the Backup and DR
// Service, as do (#1462) Edit vault, Edit backup plan (a new revision, listed
// in Plan revisions), Move association and Edit backup; Restore a backup (instances.restoreBackup with backupdrBackup)
// and Point-in-time restore (instances.pointInTimeRestore with the data
// source) go through the Cloud SQL Admin API. Both are called in process
// through their own HTTP handlers, so the page shows what gcloud and
// Terraform see and holds no state of its own.
type backupDRProvider struct {
	bdr http.Handler
	sql http.Handler
}

func (backupDRProvider) ID() string    { return "backupdr" }
func (backupDRProvider) Title() string { return "Backup and DR" }

const backupDRNote = "Backup vaults hold the backups of the Cloud SQL instances their backup plans protect. " +
	"A backup is a real dump of the instance's databases, restored by Cloud SQL; " +
	"other workload types (Compute Engine, disks, AlloyDB, Filestore) are not served (docs/backupdr.md)."

// call sends one request to h and decodes its JSON answer, or returns the
// API's own error message.
func (backupDRProvider) call(ctx context.Context, h http.Handler, method, path string, body any) (map[string]any, error) {
	if h == nil {
		return nil, errors.New("the API is not served by this instance")
	}
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code >= 300 {
		if e, ok := out["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); m != "" {
				return nil, errors.New(m)
			}
		}
		return nil, fmt.Errorf("HTTP %d", rec.Code)
	}
	// A long-running operation that failed at once.
	if e, ok := out["error"].(map[string]any); ok {
		if m, _ := e["message"].(string); m != "" {
			return nil, errors.New(m)
		}
	}
	return out, nil
}

func (p backupDRProvider) get(ctx context.Context, path string) (map[string]any, error) {
	return p.call(ctx, p.bdr, http.MethodGet, "/v1/"+path, nil)
}

func bdrItems(m map[string]any, key string) []map[string]any {
	var out []map[string]any
	l, _ := m[key].([]any)
	for _, x := range l {
		if r, ok := x.(map[string]any); ok {
			out = append(out, r)
		}
	}
	return out
}

// vaultName finds the vault with this ID in any of the project's locations.
func (p backupDRProvider) vaultName(ctx context.Context, project, id string) (string, error) {
	out, err := p.get(ctx, "projects/"+project+"/locations/-/backupVaults")
	if err != nil {
		return "", err
	}
	for _, v := range bdrItems(out, "backupVaults") {
		if lastSegment(str(v, "name")) == id {
			return str(v, "name"), nil
		}
	}
	return "", fmt.Errorf("backup vault %s does not exist", id)
}

func locationOf(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) > 3 {
		return parts[3]
	}
	return ""
}

// List implements console.Provider: the project's backup vaults.
func (p backupDRProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Location", "Backups", "Minimum enforced retention", "Access restriction"},
		NameColumn: "Backup vault", Noun: "backup vaults", Note: backupDRNote}
	if project == "" {
		l.Prompt = "Backup and DR lists vaults per project. Choose one in the toolbar."
		return l, nil
	}
	out, err := p.get(ctx, "projects/"+project+"/locations/-/backupVaults")
	if err != nil {
		l.Unavailable = "cannot reach the Backup and DR Service: " + err.Error()
		return l, nil
	}
	for _, v := range bdrItems(out, "backupVaults") {
		name := str(v, "name")
		l.Items = append(l.Items, console.Resource{Name: lastSegment(name), Status: str(v, "state"), Fields: map[string]string{
			"Location": locationOf(name), "Backups": str(v, "backupCount"),
			"Minimum enforced retention": str(v, "backupMinimumEnforcedRetentionDuration"),
			"Access restriction":         str(v, "accessRestriction")}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm implements console.Creator: Create backup vault.
func (backupDRProvider) CreateForm() (string, []console.Field) {
	return "Create backup vault", []console.Field{
		{Name: "id", Label: "Backup vault ID", Type: "text", Required: true, Pattern: `[a-z][a-z0-9\-]{0,61}[a-z0-9]`},
		{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1"},
		{Name: "retention", Label: "Minimum enforced retention", Type: "text", Required: true, Default: "86400s",
			Help: "A duration in seconds, such as 86400s (one day)."},
		{Name: "description", Label: "Description", Type: "text"},
	}
}

// Create implements console.Creator.
func (p backupDRProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	body := map[string]any{"backupMinimumEnforcedRetentionDuration": v["retention"]}
	if v["description"] != "" {
		body["description"] = v["description"]
	}
	_, err := p.call(ctx, p.bdr, http.MethodPost, "/v1/projects/"+project+"/locations/"+url.PathEscape(v["location"])+
		"/backupVaults?backupVaultId="+url.QueryEscape(v["id"]), body)
	return v["id"], err
}

// Detail implements console.Driller: a vault's page ([vault]) and a data
// source's ([vault, data source]).
func (p backupDRProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	vault, err := p.vaultName(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	if len(path) == 2 {
		return p.dataSourceDetail(ctx, vault, path)
	}
	v, err := p.get(ctx, vault)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	d := console.Detail{Summary: []console.Property{{Label: "Name", Value: vault}, {Label: "State", Value: str(v, "state")},
		{Label: "Backups", Value: str(v, "backupCount")},
		{Label: "Minimum enforced retention", Value: str(v, "backupMinimumEnforcedRetentionDuration")},
		{Label: "Created", Value: str(v, "createTime")}}}

	ds := console.Listing{Columns: []string{"Instance", "Version", "Backups", "Last backup", "Configuration"}, NameColumn: "Data source", Noun: "data sources"}
	if out, err := p.get(ctx, vault+"/dataSources"); err != nil {
		ds.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "dataSources") {
			res, _ := x["dataSourceGcpResource"].(map[string]any)
			props, _ := res["cloudSqlInstanceDatasourceProperties"].(map[string]any)
			cfg, _ := x["backupConfigInfo"].(map[string]any)
			ds.Items = append(ds.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"),
				Opens: []string{path[0], lastSegment(str(x, "name"))},
				Fields: map[string]string{"Instance": str(props, "name"), "Version": str(props, "databaseInstalledVersion"),
					"Backups": str(x, "backupCount"), "Last backup": str(cfg, "lastBackupState"), "Configuration": str(x, "configState")}})
		}
	}
	ds.Total = len(ds.Items)

	loc := vault[:strings.Index(vault, "/backupVaults/")]
	plans := console.Listing{Columns: []string{"Resource type", "Rules", "Log retention (days)"}, NameColumn: "Backup plan", Noun: "backup plans"}
	if out, err := p.get(ctx, loc+"/backupPlans"); err != nil {
		plans.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "backupPlans") {
			if str(x, "backupVault") != vault {
				continue
			}
			plans.Items = append(plans.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"),
				Fields: map[string]string{"Resource type": str(x, "resourceType"), "Rules": rulesText(x),
					"Log retention (days)": str(x, "logRetentionDays")}})
		}
	}
	plans.Total = len(plans.Items)

	assocs := console.Listing{Columns: []string{"Resource", "Backup plan", "Data source", "Last backup"}, NameColumn: "Association", Noun: "associations"}
	if out, err := p.get(ctx, loc+"/backupPlanAssociations"); err != nil {
		assocs.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "backupPlanAssociations") {
			if !strings.HasPrefix(str(x, "dataSource"), vault+"/") {
				continue
			}
			var last []string
			for _, r := range bdrItems(x, "rulesConfigInfo") {
				last = append(last, str(r, "ruleId")+": "+str(r, "lastBackupState"))
			}
			assocs.Items = append(assocs.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"),
				Fields: map[string]string{"Resource": str(x, "resource"), "Backup plan": lastSegment(str(x, "backupPlan")),
					"Data source": lastSegment(str(x, "dataSource")), "Last backup": strings.Join(last, ", ")}})
		}
	}
	assocs.Total = len(assocs.Items)

	revs := console.Listing{Columns: []string{"Backup plan", "Revision", "Created", "Rules"}, NameColumn: "Revision", Noun: "revisions"}
	for _, pl := range plans.Items {
		out, err := p.get(ctx, loc+"/backupPlans/"+pl.Name+"/revisions")
		if err != nil {
			revs.Unavailable = err.Error()
			break
		}
		for _, r := range bdrItems(out, "backupPlanRevisions") {
			snap, _ := r["backupPlanSnapshot"].(map[string]any)
			revs.Items = append(revs.Items, console.Resource{Name: pl.Name + "/" + str(r, "revisionId"), Status: str(r, "state"),
				Fields: map[string]string{"Backup plan": pl.Name, "Revision": str(r, "revisionId"), "Created": str(r, "createTime"),
					"Rules": rulesText(snap)}})
		}
	}
	revs.Total = len(revs.Items)

	d.Sections = []console.Section{
		{ID: "datasources", Label: "Data sources", Listing: ds},
		{ID: "plans", Label: "Backup plans", Listing: plans},
		{ID: "revisions", Label: "Plan revisions", Listing: revs},
		{ID: "associations", Label: "Associations", Listing: assocs},
	}
	return d, nil
}

// rulesText is a plan's rules, one "id: recurrence, N days" each.
func rulesText(plan map[string]any) string {
	var out []string
	for _, r := range bdrItems(plan, "backupRules") {
		sch, _ := r["standardSchedule"].(map[string]any)
		days := fmt.Sprint(r["backupRetentionDays"])
		out = append(out, fmt.Sprintf("%s: %s, %s days", str(r, "ruleId"), str(sch, "recurrenceType"), days))
	}
	return strings.Join(out, "; ")
}

func (p backupDRProvider) dataSourceDetail(ctx context.Context, vault string, path []string) (console.Detail, error) {
	name := vault + "/dataSources/" + path[1]
	x, err := p.get(ctx, name)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	res, _ := x["dataSourceGcpResource"].(map[string]any)
	props, _ := res["cloudSqlInstanceDatasourceProperties"].(map[string]any)
	d := console.Detail{Summary: []console.Property{{Label: "Name", Value: name}, {Label: "Instance", Value: str(props, "name")},
		{Label: "Version", Value: str(props, "databaseInstalledVersion")}, {Label: "Configuration", Value: str(x, "configState")},
		{Label: "Backups", Value: str(x, "backupCount")}}}
	l := console.Listing{Columns: []string{"Type", "Consistency time", "Expires"}, NameColumn: "Backup", Noun: "backups"}
	if out, err := p.get(ctx, name+"/backups"); err != nil {
		l.Unavailable = err.Error()
	} else {
		for _, b := range bdrItems(out, "backups") {
			l.Items = append(l.Items, console.Resource{Name: lastSegment(str(b, "name")), Status: str(b, "state"),
				Fields: map[string]string{"Type": str(b, "backupType"), "Consistency time": str(b, "consistencyTime"),
					"Expires": str(b, "expireTime")}})
		}
	}
	l.Total = len(l.Items)
	d.Sections = []console.Section{{ID: "backups", Label: "Backups", Listing: l}}
	return d, nil
}

// DetailActions implements console.PathActor.
func (p backupDRProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	vault, err := p.vaultName(ctx, project, path[0])
	if err != nil {
		return nil
	}
	loc := vault[:strings.Index(vault, "/backupVaults/")]
	if len(path) == 2 {
		var backups, all []string
		if out, err := p.get(ctx, vault+"/dataSources/"+path[1]+"/backups"); err == nil {
			for _, b := range bdrItems(out, "backups") {
				all = append(all, lastSegment(str(b, "name")))
				if str(b, "state") == "ACTIVE" {
					backups = append(backups, lastSegment(str(b, "name")))
				}
			}
		}
		acts := []console.Action{}
		if len(backups) > 0 {
			acts = append(acts, console.Action{ID: "restore", Label: "Restore a backup",
				Confirm: "The target instance's databases are replaced by the backup's.",
				Fields: []console.Field{{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: backups},
					{Name: "target", Label: "Target instance", Type: "text", Required: true,
						Help: "An existing Cloud SQL instance of the same engine, in this project."}}})
		}
		acts = append(acts, console.Action{ID: "pitr", Label: "Point-in-time restore",
			Fields: []console.Field{{Name: "target", Label: "New instance ID", Type: "text", Required: true},
				{Name: "time", Label: "Point in time", Type: "text", Required: true,
					Help: "An RFC 3339 time inside the instance's recovery window, such as 2026-10-16T12:00:00Z."}}})
		if len(all) > 0 {
			acts = append(acts, console.Action{ID: "editbackup", Label: "Edit backup", Fields: []console.Field{
				{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: all},
				{Name: "expire", Label: "Expires", Type: "text",
					Help: "An RFC 3339 time, such as 2026-12-31T00:00:00Z; empty keeps it. It cannot be before the enforced retention ends."},
				{Name: "enforced", Label: "Enforced retention ends", Type: "text",
					Help: "An RFC 3339 time; empty keeps it. Enforced retention can be extended, not reduced."}}})
		}
		return acts
	}
	var plans, allPlans, assocs, rules []string
	if out, err := p.get(ctx, loc+"/backupPlans"); err == nil {
		for _, x := range bdrItems(out, "backupPlans") {
			allPlans = append(allPlans, lastSegment(str(x, "name")))
			if str(x, "backupVault") == vault {
				plans = append(plans, lastSegment(str(x, "name")))
				for _, r := range bdrItems(x, "backupRules") {
					rules = append(rules, str(r, "ruleId"))
				}
			}
		}
	}
	if out, err := p.get(ctx, loc+"/backupPlanAssociations"); err == nil {
		for _, x := range bdrItems(out, "backupPlanAssociations") {
			if strings.HasPrefix(str(x, "dataSource"), vault+"/") {
				assocs = append(assocs, lastSegment(str(x, "name")))
			}
		}
	}
	sort.Strings(rules)
	rules = compactStrings(rules)
	acts := []console.Action{{ID: "createplan", Label: "Create backup plan", Fields: []console.Field{
		{Name: "id", Label: "Backup plan ID", Type: "text", Required: true},
		{Name: "rule", Label: "Rule ID", Type: "text", Required: true, Default: "daily"},
		{Name: "recurrence", Label: "Recurrence", Type: "select", Required: true, Default: "DAILY",
			Options: []string{"HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"}},
		{Name: "retention", Label: "Retention (days)", Type: "number", Required: true, Default: "7"},
		{Name: "logdays", Label: "Log retention (days)", Type: "number", Default: "7",
			Help: "How long the instance's transaction logs are kept for point-in-time restore."},
	}}}
	if len(plans) > 0 {
		acts = append(acts, console.Action{ID: "associate", Label: "Associate an instance", Fields: []console.Field{
			{Name: "id", Label: "Association ID", Type: "text", Required: true},
			{Name: "plan", Label: "Backup plan", Type: "select", Required: true, Options: plans},
			{Name: "instance", Label: "Cloud SQL instance", Type: "text", Required: true}}})
	}
	if len(plans) > 0 {
		acts = append(acts, console.Action{ID: "editplan", Label: "Edit backup plan", Fields: []console.Field{
			{Name: "plan", Label: "Backup plan", Type: "select", Required: true, Options: plans,
				Help: "Each edit makes a new revision of the plan, listed in Plan revisions."},
			{Name: "rule", Label: "Rule", Type: "select", Required: true, Options: rules},
			{Name: "recurrence", Label: "Recurrence", Type: "select", Options: []string{"", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"},
				Help: "Empty keeps the rule's."},
			{Name: "retention", Label: "Retention (days)", Type: "number", Help: "Empty keeps the rule's."},
			{Name: "description", Label: "Description", Type: "text", Help: "Empty keeps the plan's."}}})
	}
	if len(assocs) > 0 {
		acts = append(acts, console.Action{ID: "moveassoc", Label: "Move association", Fields: []console.Field{
			{Name: "association", Label: "Association", Type: "select", Required: true, Options: assocs},
			{Name: "plan", Label: "New backup plan", Type: "select", Required: true, Options: allPlans,
				Help: "A plan storing into another vault gives the instance a data source there."}}})
	}
	if len(assocs) > 0 && len(rules) > 0 {
		acts = append(acts, console.Action{ID: "trigger", Label: "Trigger backup", Fields: []console.Field{
			{Name: "association", Label: "Association", Type: "select", Required: true, Options: assocs},
			{Name: "rule", Label: "Rule", Type: "select", Required: true, Options: rules}}})
	}
	if v, err := p.get(ctx, vault); err == nil {
		acts = append(acts, console.Action{ID: "editvault", Label: "Edit vault", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: str(v, "description")},
			{Name: "labels", Label: "Labels", Type: "text", Default: bdrLabelsText(v["labels"]), Help: "key=value pairs, comma-separated."},
			{Name: "retention", Label: "Minimum enforced retention", Type: "text", Required: true,
				Default: str(v, "backupMinimumEnforcedRetentionDuration"), Help: "A duration in seconds, such as 86400s (one day)."}}})
	}
	acts = append(acts, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
		Confirm: "The vault is deleted. A vault holding data sources or used by a plan cannot be."})
	return acts
}

// bdrLabelsText is labels as k=v pairs, sorted; bdrParseLabels reads them back.
func bdrLabelsText(v any) string {
	m, _ := v.(map[string]any)
	var out []string
	for k, x := range m {
		out = append(out, k+"="+fmt.Sprint(x))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func bdrParseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("label %q is not key=value", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// editPlan changes one rule's recurrence or retention, and the plan's
// description, through backupPlans.patch.
func (p backupDRProvider) editPlan(ctx context.Context, loc string, v map[string]string) error {
	name := loc + "/backupPlans/" + v["plan"]
	plan, err := p.get(ctx, name)
	if err != nil {
		return err
	}
	var mask []string
	body := map[string]any{}
	if v["recurrence"] != "" || v["retention"] != "" {
		rules := bdrItems(plan, "backupRules")
		found := false
		for _, r := range rules {
			if str(r, "ruleId") != v["rule"] {
				continue
			}
			found = true
			if v["retention"] != "" {
				var n int
				if _, e := fmt.Sscan(v["retention"], &n); e != nil || n <= 0 {
					return fmt.Errorf("retention %q is not a number of days", v["retention"])
				}
				r["backupRetentionDays"] = n
			}
			if v["recurrence"] != "" {
				sch, _ := r["standardSchedule"].(map[string]any)
				if sch == nil {
					sch = map[string]any{"timeZone": "UTC"}
				}
				sch["recurrenceType"] = v["recurrence"]
				if v["recurrence"] == "HOURLY" && sch["hourlyFrequency"] == nil {
					sch["hourlyFrequency"] = 6
				}
				r["standardSchedule"] = sch
			}
		}
		if !found {
			return fmt.Errorf("backup plan %s has no rule %q", v["plan"], v["rule"])
		}
		body["backupRules"] = rules
		mask = append(mask, "backupRules")
	}
	if v["description"] != "" {
		body["description"] = v["description"]
		mask = append(mask, "description")
	}
	if len(mask) == 0 {
		return errors.New("nothing to change: set a recurrence, a retention or a description")
	}
	_, err = p.call(ctx, p.bdr, http.MethodPatch, "/v1/"+name+"?updateMask="+strings.Join(mask, ","), body)
	return err
}

func compactStrings(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ActAt implements console.PathActor.
func (p backupDRProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	vault, err := p.vaultName(ctx, project, path[0])
	if err != nil {
		return err
	}
	loc := vault[:strings.Index(vault, "/backupVaults/")]
	switch action {
	case "delete":
		_, err = p.call(ctx, p.bdr, http.MethodDelete, "/v1/"+vault, nil)
	case "createplan":
		sch := map[string]any{"recurrenceType": v["recurrence"], "timeZone": "UTC",
			"backupWindow": map[string]any{"startHourOfDay": 0, "endHourOfDay": 24}}
		if v["recurrence"] == "HOURLY" {
			sch["hourlyFrequency"] = 6
		}
		var ret, logDays int
		if _, e := fmt.Sscan(v["retention"], &ret); e != nil {
			return fmt.Errorf("retention %q is not a number of days", v["retention"])
		}
		body := map[string]any{"backupVault": vault, "resourceType": "sqladmin.googleapis.com/Instance",
			"backupRules": []any{map[string]any{"ruleId": v["rule"], "backupRetentionDays": ret, "standardSchedule": sch}}}
		if v["logdays"] != "" {
			if _, e := fmt.Sscan(v["logdays"], &logDays); e == nil {
				body["logRetentionDays"] = logDays
			}
		}
		_, err = p.call(ctx, p.bdr, http.MethodPost, "/v1/"+loc+"/backupPlans?backupPlanId="+url.QueryEscape(v["id"]), body)
	case "associate":
		_, err = p.call(ctx, p.bdr, http.MethodPost, "/v1/"+loc+"/backupPlanAssociations?backupPlanAssociationId="+url.QueryEscape(v["id"]),
			map[string]any{"backupPlan": loc + "/backupPlans/" + v["plan"], "resourceType": "sqladmin.googleapis.com/Instance",
				"resource": "projects/" + project + "/instances/" + v["instance"]})
	case "editvault":
		labels, e := bdrParseLabels(v["labels"])
		if e != nil {
			return e
		}
		_, err = p.call(ctx, p.bdr, http.MethodPatch, "/v1/"+vault+"?updateMask=description,labels,backupMinimumEnforcedRetentionDuration",
			map[string]any{"description": v["description"], "labels": labels, "backupMinimumEnforcedRetentionDuration": v["retention"]})
	case "editplan":
		err = p.editPlan(ctx, loc, v)
	case "moveassoc":
		_, err = p.call(ctx, p.bdr, http.MethodPatch, "/v1/"+loc+"/backupPlanAssociations/"+v["association"]+"?updateMask=backupPlan",
			map[string]any{"backupPlan": loc + "/backupPlans/" + v["plan"]})
	case "editbackup":
		if len(path) != 2 {
			return errors.New("edit backup is offered on a data source's page")
		}
		var mask []string
		body := map[string]any{}
		if v["expire"] != "" {
			body["expireTime"], mask = v["expire"], append(mask, "expireTime")
		}
		if v["enforced"] != "" {
			body["enforcedRetentionEndTime"], mask = v["enforced"], append(mask, "enforcedRetentionEndTime")
		}
		if len(mask) == 0 {
			return errors.New("nothing to change: set an expiry or an enforced retention end")
		}
		_, err = p.call(ctx, p.bdr, http.MethodPatch, "/v1/"+vault+"/dataSources/"+path[1]+"/backups/"+v["backup"]+
			"?updateMask="+strings.Join(mask, ","), body)
	case "trigger":
		_, err = p.call(ctx, p.bdr, http.MethodPost, "/v1/"+loc+"/backupPlanAssociations/"+v["association"]+":triggerBackup",
			map[string]any{"ruleId": v["rule"]})
	case "restore":
		if len(path) != 2 {
			return errors.New("restore is offered on a data source's page")
		}
		_, err = p.call(ctx, p.sql, http.MethodPost, "/v1/projects/"+project+"/instances/"+url.PathEscape(v["target"])+"/restoreBackup",
			map[string]any{"backupdrBackup": vault + "/dataSources/" + path[1] + "/backups/" + v["backup"]})
	case "pitr":
		if len(path) != 2 {
			return errors.New("point-in-time restore is offered on a data source's page")
		}
		_, err = p.call(ctx, p.sql, http.MethodPost, "/v1/projects/"+project+":pointInTimeRestore",
			map[string]any{"datasource": vault + "/dataSources/" + path[1], "targetInstance": v["target"], "pointInTime": v["time"]})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}
