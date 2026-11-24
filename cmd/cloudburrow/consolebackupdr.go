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
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
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
	// compute is the Compute Engine API (#2075), nil when not served.
	compute func() *compute.Server
	// alloydb and filestore are AlloyDB's and Filestore's REST APIs
	// (#1524); nil, or answering nil, when not served.
	alloydb, filestore func() http.Handler
}

func (backupDRProvider) ID() string    { return "backupdr" }
func (backupDRProvider) Title() string { return "Backup vaults" }

const backupDRNote = "Backup vaults hold the backups of the Cloud SQL instances, Compute Engine VMs and disks, AlloyDB clusters and Filestore instances their backup plans protect. " +
	"A Cloud SQL backup is a real dump of the instance's databases, restored by Cloud SQL; a VM's or disk's holds its disks' contents, " +
	"restored to a new VM or disk. AlloyDB clusters' and Filestore instances' backups are restored by AlloyDB and Filestore into new clusters and instances (docs/backupdr.md)."

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

// List implements console.Provider: the project's backup vaults, Google's
// Backup vaults page (#1844).
func (p backupDRProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Location", "Backups", "Minimum enforced retention", "Access restriction"},
		NameColumn: "Backup vault", Noun: "backup vaults", Note: backupDRNote, AlwaysStatus: true}
	if project == "" {
		l.Prompt = "Backup vaults lists a project's vaults. Choose one in the toolbar."
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
			"Minimum enforced retention": bdrDuration(str(v, "backupMinimumEnforcedRetentionDuration")),
			"Access restriction":         bdrAccessLabels[str(v, "accessRestriction")]}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// bdrAccessLabels are the access restrictions as Google's vault page words
// them (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/backup-vault-create).
var bdrAccessLabels = map[string]string{"WITHIN_ORGANIZATION": "Restrict access to current organization",
	"WITHIN_PROJECT": "Restrict access to current project", "UNRESTRICTED": "No restrictions",
	"WITHIN_ORG_BUT_UNRESTRICTED_FOR_BA": "Restrict access to current organization, except for backup appliances"}

// bdrMaxRetentionDays is Google's limit on the minimum enforced retention:
// 0 days to 99 years.
const bdrMaxRetentionDays = 99 * 365

// CreateForm implements console.Creator: Google's Create a backup vault page.
func (backupDRProvider) CreateForm() (string, []console.Field) {
	return "Create backup vault", []console.Field{
		{Name: "id", Label: "Backup vault name", Type: "text", Required: true, Pattern: bdrIDPattern, Section: "Name your backup vault",
			Help: bdrIDHelp + "."},
		{Name: "description", Label: "Description", Type: "text", Section: "Name your backup vault"},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Name your backup vault"},
		{Name: "locationType", Label: "Location type", Type: "radio", Required: true, Section: "Choose where to store your data",
			Options: []string{"region", "multi-region"}, Default: "region",
			OptionLabels: map[string]string{"region": "Region", "multi-region": "Multi-region"}},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: "Choose where to store your data",
			Options: backupdr.Regions, OptionLabels: bdrRegionLabels(), Default: "us-central1", ShowWhen: "locationType=region"},
		{Name: "multiRegion", Label: "Multi-region", Type: "select", Section: "Choose where to store your data",
			Options: []string{"us", "eu", "asia"}, Default: "us", ShowWhen: "locationType=multi-region",
			OptionLabels: map[string]string{"us": "us (multiple regions in United States)", "eu": "eu (multiple regions in European Union)",
				"asia": "asia (multiple regions in Asia)"},
			Help: "A Cloud SQL instance is backed up to the multi-region whose regions share its region's prefix: us-central1 to us."},
		{Name: "retentionDays", Label: "Minimum enforced retention (days)", Type: "text", Required: true, Default: "1",
			Pattern: `[0-9]{1,5}`, Section: "Prevent backup deletion",
			Help: "0 days to 99 years (36135 days): no backup in the vault can be deleted before it is this old."},
		{Name: "inherit", Label: "Prevent deletion for duration specified in backup rule", Type: "checkbox", Section: "Prevent backup deletion",
			Help: "Each backup's enforced retention ends at its expire time, the backup rule's Delete backups after. It cannot be changed later."},
		{Name: "lock", Label: "Lock the enforced retention", Type: "checkbox", Section: "Prevent backup deletion",
			Help: "From the lock's date on, the minimum enforced retention can only be increased, and the lock cannot be removed."},
		{Name: "lockDate", Label: "Lock takes effect on", Type: "datetime-local", Section: "Prevent backup deletion", ShowWhen: "lock",
			Help: "A future date and time, UTC."},
		{Name: "encryption", Label: "Encryption", Type: "radio", Required: true, Section: "Encryption", Default: "google",
			Options: []string{"google", "cmek"}, OptionLabels: map[string]string{"google": "Google-managed encryption key",
				"cmek": "Customer-managed encryption key (CMEK)"}},
		{Name: "kmsKeyName", Label: "Customer-managed encryption key", Type: "text", Section: "Encryption", ShowWhen: "encryption=cmek",
			Browse: "kms", Help: "A key of this instance's Cloud KMS in the vault's region. Backups into the vault, and restores from it, " +
				"work only while the key can be used. It can be set only when the vault is created."},
		{Name: "access", Label: "Define access to your backup vault", Type: "radio", Required: true, Section: "Define access to your backup vault",
			Options: []string{"WITHIN_ORGANIZATION", "WITHIN_PROJECT", "UNRESTRICTED"}, Default: "WITHIN_ORGANIZATION", OptionLabels: bdrAccessLabels},
	}
}

func (backupDRProvider) CreateOnPage() bool   { return true }
func (backupDRProvider) CreateTitle() string  { return "Create a backup vault" }
func (backupDRProvider) CreateSubmit() string { return "Create" }

// bdrRetentionSeconds reads Minimum enforced retention (days) as the API's
// duration.
func bdrRetentionSeconds(days string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(days))
	if err != nil || n < 0 || n > bdrMaxRetentionDays {
		return "", fmt.Errorf("the minimum enforced retention is 0 days to 99 years (%d days)", bdrMaxRetentionDays)
	}
	return strconv.Itoa(n*86400) + "s", nil
}

// Create implements console.Creator.
func (p backupDRProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	if !bdrIDRe.MatchString(v["id"]) {
		return "", fmt.Errorf("the backup vault name must be %s", bdrIDHelp)
	}
	loc := v["region"]
	if v["locationType"] == "multi-region" {
		loc = v["multiRegion"]
		if !backupdr.IsMultiRegion(loc) {
			return "", errors.New("choose the multi-region: us, eu or asia")
		}
	}
	retention, err := bdrRetentionSeconds(v["retentionDays"])
	if err != nil {
		return "", err
	}
	body := map[string]any{"backupMinimumEnforcedRetentionDuration": retention}
	if v["inherit"] == "true" || v["inherit"] == "on" {
		body["backupRetentionInheritance"] = "MATCH_BACKUP_EXPIRE_TIME"
	}
	if v["lock"] == "true" || v["lock"] == "on" {
		t, err := bdrLockTime(v["lockDate"])
		if err != nil {
			return "", err
		}
		body["effectiveTime"] = t
	}
	if v["description"] != "" {
		body["description"] = v["description"]
	}
	if labels, err := console.ParseMap(v["labels"]); err != nil {
		return "", err
	} else if len(labels) > 0 {
		body["labels"] = labels
	}
	if a := v["access"]; a != "" {
		body["accessRestriction"] = a
	}
	if k := strings.TrimSpace(v["kmsKeyName"]); k != "" && v["encryption"] != "google" {
		body["encryptionConfig"] = map[string]any{"kmsKeyName": k}
	} else if v["encryption"] == "cmek" {
		return "", errors.New("choose the customer-managed encryption key")
	}
	_, err = p.call(ctx, p.bdr, http.MethodPost, "/v1/projects/"+project+"/locations/"+url.PathEscape(loc)+
		"/backupVaults?backupVaultId="+url.QueryEscape(v["id"]), body)
	return v["id"], err
}

// Detail implements console.Driller: a vault's page ([vault]) and a data
// source's ([vault, data source]), the latter now Vaulted backups' page.
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
	enc := "Google-managed encryption key"
	ec, _ := v["encryptionConfig"].(map[string]any)
	if ec != nil {
		enc = "Customer-managed encryption key (CMEK)"
	}
	d := console.Detail{Summary: []console.Property{
		{Label: "Created", Value: str(v, "createTime")}, {Label: "Last updated", Value: str(v, "updateTime")},
		{Label: "Description", Value: str(v, "description")}, {Label: "Status", Value: str(v, "state")},
		{Label: "Location type", Value: bdrLocationType(locationOf(vault))}, {Label: "Location", Value: bdrRegionLabel(locationOf(vault))},
		{Label: "Stored bytes", Value: str(v, "totalStoredBytes")}, {Label: "Backups", Value: str(v, "backupCount")},
		{Label: "Enforced retention", Value: "Minimum for backups: " + bdrDuration(str(v, "backupMinimumEnforcedRetentionDuration"))},
		{Label: "Retention lock status", Value: bdrLockStatus(v)},
		{Label: "Retention inheritance", Value: bdrInheritanceLabel(str(v, "backupRetentionInheritance"))},
		{Label: "Encryption type", Value: enc}}}
	if ec != nil {
		d.Summary = append(d.Summary, console.Property{Label: "Encryption key", Value: str(ec, "kmsKeyName")})
	}
	d.Summary = append(d.Summary, console.Property{Label: "Access restriction", Value: bdrAccessLabels[str(v, "accessRestriction")]},
		console.Property{Label: "Service agent", Value: str(v, "serviceAccount"), Copy: "Copy service agent email"},
		console.Property{Label: "Name", Value: vault})
	if labels := bdrLabelsText(v["labels"]); labels != "" {
		d.Summary = append(d.Summary, console.Property{Label: "Labels", Value: labels})
	}

	ds := console.Listing{Columns: []string{"Instance", "Version", "Backups", "Last backup", "Configuration"}, NameColumn: "Data source", Noun: "data sources"}
	if out, err := p.get(ctx, vault+"/dataSources"); err != nil {
		ds.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "dataSources") {
			res, _ := x["dataSourceGcpResource"].(map[string]any)
			props, _ := res["cloudSqlInstanceDatasourceProperties"].(map[string]any)
			cfg, _ := x["backupConfigInfo"].(map[string]any)
			ds.Items = append(ds.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"),
				Link: "/backupdr-vaulted-backups/" + url.PathEscape(path[0]) + "/" + url.PathEscape(lastSegment(str(x, "name"))),
				Fields: map[string]string{"Instance": str(props, "name"), "Version": str(props, "databaseInstalledVersion"),
					"Backups": str(x, "backupCount"), "Last backup": str(cfg, "lastBackupState"), "Configuration": str(x, "configState")}})
		}
	}
	ds.Total = len(ds.Items)

	loc := vault[:strings.Index(vault, "/backupVaults/")]
	plans := console.Listing{Columns: []string{"Resource type", "Backup rules", "Log retention (days)"}, NameColumn: "Backup plan", Noun: "backup plans"}
	if out, err := p.get(ctx, loc+"/backupPlans"); err != nil {
		plans.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "backupPlans") {
			if str(x, "backupVault") != vault {
				continue
			}
			plans.Items = append(plans.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"),
				Link: "/backupdr-plans/" + url.PathEscape(lastSegment(str(x, "name"))),
				Fields: map[string]string{"Resource type": bdrTypeLabel(str(x, "resourceType")), "Backup rules": rulesText(x),
					"Log retention (days)": str(x, "logRetentionDays")}})
		}
	}
	plans.Total = len(plans.Items)

	// Protected resources (#1501): resourceBackupConfigs.list, the
	// resources whose backups this vault holds.
	prot := console.Listing{Columns: []string{"Type", "Association", "Last successful backup"}, NameColumn: "Resource", Noun: "protected resources",
		Note: "resourceBackupConfigs.list: each Cloud SQL instance whose backup plan association backs it up into this vault."}
	if out, err := p.get(ctx, loc+"/resourceBackupConfigs"); err != nil {
		prot.Unavailable = err.Error()
	} else {
		for _, x := range bdrItems(out, "resourceBackupConfigs") {
			for _, dt := range bdrItems(x, "backupConfigsDetails") {
				if str(dt, "type") != "BACKUPDR_BACKUP_PLAN" || str(dt, "backupVault") != vault {
					continue
				}
				prot.Items = append(prot.Items, console.Resource{Name: str(x, "targetResourceDisplayName"), Status: str(dt, "state"),
					Fields: map[string]string{"Type": str(x, "targetResourceType"), "Association": lastSegment(str(dt, "backupConfigSource")),
						"Last successful backup": str(dt, "latestSuccessfulBackupTime")}})
			}
		}
	}
	prot.Total = len(prot.Items)

	d.Sections = []console.Section{
		{ID: "datasources", Label: "Data sources", Listing: ds},
		{ID: "plans", Label: "Backup plans", Listing: plans},
		{ID: "protected", Label: "Protected resources", Listing: prot},
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
			// Not "delete…": the console leaves a page after an action whose
			// ID starts so, and this one deletes a backup, not the page's
			// data source. Delete association and Delete backup plan alike.
			acts = append(acts, console.Action{ID: "backupdelete", Label: "Delete backup", Destructive: true,
				Confirm: "The backup and its dump are deleted; it can no longer be restored.",
				Fields:  []console.Field{{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: all}}})
		}
		return acts
	}
	// A vault's page has Google's Edit minimum enforced retention and
	// Delete (#1844); plans and associations are acted on from their own
	// pages, and Edit vault keeps the description and labels editable.
	_ = loc
	acts := []console.Action{}
	if v, err := p.get(ctx, vault); err == nil {
		days := strings.TrimSuffix(bdrDuration(str(v, "backupMinimumEnforcedRetentionDuration")), " days")
		days = strings.TrimSuffix(days, " day")
		acts = append(acts, console.Action{ID: "editretention", Label: "Edit minimum enforced retention", Submit: "Save",
			Fields: bdrRetentionFields(v, days)},
			console.Action{ID: "editvault", Label: "Edit vault", Fields: []console.Field{
				{Name: "description", Label: "Description", Type: "text", Default: str(v, "description")},
				{Name: "labels", Label: "Labels", Type: "text", Default: bdrLabelsText(v["labels"]), Help: "key=value pairs, comma-separated."},
				{Name: "retention", Label: "Minimum enforced retention", Type: "text", Required: true,
					Default: str(v, "backupMinimumEnforcedRetentionDuration"), Help: "A duration in seconds, such as 86400s (one day)."}}})
	}
	acts = append(acts, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
		Confirm: "The vault is deleted. A vault holding backups that have not expired, or used by a plan, cannot be."})
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
				if sch["recurrenceType"] != v["recurrence"] {
					sch["recurrenceType"] = v["recurrence"]
					delete(sch, "hourlyFrequency")
					if v["recurrence"] == "HOURLY" {
						sch["hourlyFrequency"] = 6
					}
					if err := bdrApplyDays(sch, v["recurrence"], "", "", "", true); err != nil {
						return err
					}
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
		if err := bdrApplyDays(sch, v["recurrence"], "", "", "", true); err != nil {
			return err
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
	case "editretention":
		retention, e := bdrRetentionSeconds(v["retentionDays"])
		if e != nil {
			return e
		}
		mask, body := "backupMinimumEnforcedRetentionDuration", map[string]any{"backupMinimumEnforcedRetentionDuration": retention}
		switch v["lock"] {
		case "lock":
			t, e := bdrLockTime(v["lockDate"])
			if e != nil {
				return e
			}
			mask, body["effectiveTime"] = mask+",effectiveTime", t
		case "remove":
			mask, body["effectiveTime"] = mask+",effectiveTime", ""
		}
		_, err = p.call(ctx, p.bdr, http.MethodPatch, "/v1/"+vault+"?updateMask="+mask, body)
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
	case "plandelete":
		_, err = p.call(ctx, p.bdr, http.MethodDelete, "/v1/"+loc+"/backupPlans/"+url.PathEscape(v["plan"]), nil)
	case "assocdelete":
		_, err = p.call(ctx, p.bdr, http.MethodDelete, "/v1/"+loc+"/backupPlanAssociations/"+url.PathEscape(v["association"]), nil)
	case "backupdelete":
		if len(path) != 2 {
			return errors.New("delete backup is offered on a data source's page")
		}
		_, err = p.call(ctx, p.bdr, http.MethodDelete, "/v1/"+vault+"/dataSources/"+path[1]+"/backups/"+url.PathEscape(v["backup"]), nil)
	case "trigger":
		if a, r, ok := strings.Cut(v["target"], bdrTriggerSep); ok {
			v = map[string]string{"association": a, "rule": r}
		}
		if err := p.checkTriggerRule(ctx, loc, v["association"], v["rule"]); err != nil {
			return err
		}
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

// bdrTriggerSep joins an association and a rule in Trigger backup's choice.
const bdrTriggerSep = " / "

// checkTriggerRule refuses a rule that is not in the association's plan,
// naming the plan's rules, before triggerBackup would fail on it.
func (p backupDRProvider) checkTriggerRule(ctx context.Context, loc, assoc, rule string) error {
	a, err := p.get(ctx, loc+"/backupPlanAssociations/"+url.PathEscape(assoc))
	if err != nil {
		return err
	}
	plan, err := p.get(ctx, strings.TrimPrefix(str(a, "backupPlan"), "/"))
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range bdrItems(plan, "backupRules") {
		if str(r, "ruleId") == rule {
			return nil
		}
		ids = append(ids, str(r, "ruleId"))
	}
	return fmt.Errorf("rule %q is not in backup plan %s, which association %s applies; its rules are %s",
		rule, lastSegment(str(a, "backupPlan")), assoc, strings.Join(ids, ", "))
}

// bdrLockTime reads a retention lock's date, a datetime-local in UTC
// ("2026-12-01T00:00") or RFC 3339, as the API's effectiveTime.
func bdrLockTime(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("choose the date the retention lock takes effect")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("the lock date %q is not a date and time such as 2026-12-01T00:00", v)
}

// bdrLockStatus is Google's Retention lock status: Unlocked, Lock takes
// effect on …, or Locked.
func bdrLockStatus(v map[string]any) string {
	e := str(v, "effectiveTime")
	if e == "" {
		return "Unlocked"
	}
	if t, err := time.Parse(time.RFC3339Nano, e); err == nil && t.After(time.Now()) {
		return "Lock takes effect on " + t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return "Locked"
}

func bdrLocationType(loc string) string {
	if backupdr.IsMultiRegion(loc) {
		return "Multi-region"
	}
	return "Region"
}

func bdrInheritanceLabel(v string) string {
	if v == "MATCH_BACKUP_EXPIRE_TIME" {
		return "Prevent deletion for duration specified in backup rule"
	}
	return "Minimum enforced retention of the vault"
}

// bdrRetentionFields are Edit minimum enforced retention's fields: the
// days, and the retention lock as Google's states allow it (#1921). While
// Unlocked, a lock can be set; while it is pending, moved or removed; once
// Locked, the retention can only be increased.
func bdrRetentionFields(v map[string]any, days string) []console.Field {
	status := bdrLockStatus(v)
	help := "0 days to 99 years (36135 days). While the retention is unlocked it can be increased or decreased."
	if status == "Locked" {
		help = "The retention is locked: it can only be increased."
	}
	fields := []console.Field{{Name: "retentionDays", Label: "New minimum enforced retention period (days)", Type: "text", Required: true,
		Default: days, Pattern: `[0-9]{1,5}`, Help: help}}
	switch {
	case status == "Unlocked":
		fields = append(fields,
			console.Field{Name: "lock", Label: "Retention lock", Type: "radio", Options: []string{"keep", "lock"}, Default: "keep",
				OptionLabels: map[string]string{"keep": "Leave unlocked", "lock": "Lock the enforced retention"}},
			console.Field{Name: "lockDate", Label: "Lock takes effect on", Type: "datetime-local", ShowWhen: "lock=lock",
				Help: "A future date and time, UTC. From then on the lock cannot be removed."})
	case status != "Locked":
		fields = append(fields,
			console.Field{Name: "lock", Label: "Retention lock", Type: "radio", Options: []string{"keep", "lock", "remove"}, Default: "keep",
				OptionLabels: map[string]string{"keep": status, "lock": "Change the lock date", "remove": "Remove the lock"},
				Help:         "The lock can be removed until its date; removing it clears the date."},
			console.Field{Name: "lockDate", Label: "Lock takes effect on", Type: "datetime-local", ShowWhen: "lock=lock",
				Help: "A future date and time, UTC."})
	}
	return fields
}
