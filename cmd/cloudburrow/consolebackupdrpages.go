package main

// The Backup and DR Service's pages in Google's menu (#1844): Overview,
// Vaulted backups, Backup plans, Backup plan associations, Backup vaults
// (backupDRProvider, at /backupdr) and Jobs, each read through the Backup and
// DR Service in process, as backupDRProvider is; Reports is "Not available
// locally" (console.js). Google's pages, from its public docs:
// https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/sql/csql-backup,
// .../cloud-console/backup-plan-create, .../cloud-console/backup-vault-create,
// .../cloud-console/bv-backup-manage, .../quickstarts/csql-quickstart and
// .../monitor-reports/monitor-jobs-console.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// bdrResourceTypes are Google's Resource Type choices; only Cloud SQL is
// served here (docs/backupdr.md).
var bdrResourceTypes = []string{backupdr.CloudSQLResourceType, "compute.googleapis.com/Instance", "compute.googleapis.com/Disk",
	"alloydb.googleapis.com/Cluster", "file.googleapis.com/Instance"}

var bdrResourceTypeLabels = map[string]string{backupdr.CloudSQLResourceType: "Cloud SQL", "compute.googleapis.com/Instance": "Compute Engine",
	"compute.googleapis.com/Disk": "Disk", "alloydb.googleapis.com/Cluster": "AlloyDB", "file.googleapis.com/Instance": "Filestore"}

// bdrUnservedTypes are the Resource Type choices drawn disabled, each with
// why.
func bdrUnservedTypes() map[string]string {
	out := map[string]string{}
	for _, t := range bdrResourceTypes[1:] {
		out[t] = "Not available locally: CloudBurrow's Backup and DR Service backs up Cloud SQL instances only; " +
			bdrResourceTypeLabels[t] + " is not served (docs/backupdr.md)."
	}
	return out
}

func bdrTypeLabel(t string) string {
	if l := bdrResourceTypeLabels[t]; l != "" {
		return l
	}
	return t
}

// bdrRegionLabels names each served region as Google's lists do,
// "us-central1 (Iowa)".
func bdrRegionLabels() map[string]string {
	city := map[string]string{}
	for _, r := range bqRegionNames {
		city[r[0]] = r[1]
	}
	out := map[string]string{}
	for _, l := range backupdr.Locations {
		if c := city[l]; c != "" {
			out[l] = l + " (" + c + ")"
		}
	}
	return out
}

func bdrRegionLabel(l string) string {
	if s := bdrRegionLabels()[l]; s != "" {
		return s
	}
	return l
}

// bdrIDPattern is the API's rule for a vault's, plan's, association's and
// rule's ID (checkID).
const bdrIDPattern = `[a-z][a-z0-9\-]{0,61}[a-z0-9]|[a-z]`

var bdrIDRe = regexp.MustCompile(`^(?:` + bdrIDPattern + `)$`)

const bdrIDHelp = "1 to 63 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen"

// --- shared reads -------------------------------------------------------

func (p backupDRProvider) list(ctx context.Context, project, collection string) ([]map[string]any, error) {
	out, err := p.get(ctx, "projects/"+project+"/locations/-/"+collection)
	if err != nil {
		return nil, err
	}
	return bdrItems(out, collection), nil
}

// bdrSource is one data source with the vault holding it.
type bdrSource struct {
	vault, ds map[string]any
}

func (s bdrSource) vaultID() string { return lastSegment(str(s.vault, "name")) }
func (s bdrSource) id() string      { return lastSegment(str(s.ds, "name")) }
func (s bdrSource) instance() string {
	return str(nested(s.ds, "dataSourceGcpResource", "cloudSqlInstanceDatasourceProperties"), "name")
}
func (s bdrSource) instanceName() string { return lastSegment(s.instance()) }
func (s bdrSource) location() string     { return str(nested(s.ds, "dataSourceGcpResource"), "location") }
func (s bdrSource) plan() string {
	return str(nested(s.ds, "backupConfigInfo", "gcpBackupConfig"), "backupPlan")
}
func (s bdrSource) association() string {
	return str(nested(s.ds, "backupConfigInfo", "gcpBackupConfig"), "backupPlanAssociation")
}

// sources are every data source of the project's vaults.
func (p backupDRProvider) sources(ctx context.Context, project string) ([]bdrSource, error) {
	vaults, err := p.list(ctx, project, "backupVaults")
	if err != nil {
		return nil, err
	}
	var out []bdrSource
	for _, v := range vaults {
		res, err := p.get(ctx, str(v, "name")+"/dataSources")
		if err != nil {
			return nil, err
		}
		for _, d := range bdrItems(res, "dataSources") {
			out = append(out, bdrSource{vault: v, ds: d})
		}
	}
	return out, nil
}

func (p backupDRProvider) source(ctx context.Context, project, vaultID, dsID string) (bdrSource, error) {
	vault, err := p.vaultName(ctx, project, vaultID)
	if err != nil {
		return bdrSource{}, err
	}
	v, err := p.get(ctx, vault)
	if err != nil {
		return bdrSource{}, err
	}
	d, err := p.get(ctx, vault+"/dataSources/"+dsID)
	if err != nil {
		return bdrSource{}, err
	}
	return bdrSource{vault: v, ds: d}, nil
}

// planByID finds the project's plan with this ID in any location.
func (p backupDRProvider) planByID(ctx context.Context, project, id string) (map[string]any, error) {
	plans, err := p.list(ctx, project, "backupPlans")
	if err != nil {
		return nil, err
	}
	for _, x := range plans {
		if lastSegment(str(x, "name")) == id {
			return x, nil
		}
	}
	return nil, fmt.Errorf("backup plan %s does not exist", id)
}

func (p backupDRProvider) assocByID(ctx context.Context, project, id string) (map[string]any, error) {
	assocs, err := p.list(ctx, project, "backupPlanAssociations")
	if err != nil {
		return nil, err
	}
	for _, x := range assocs {
		if lastSegment(str(x, "name")) == id {
			return x, nil
		}
	}
	return nil, fmt.Errorf("backup plan association %s does not exist", id)
}

// sqlInstances are the project's Cloud SQL instances: name to region and
// database version.
func (p backupDRProvider) sqlInstances(ctx context.Context, project string) map[string][2]string {
	out := map[string][2]string{}
	if p.sql == nil {
		return out
	}
	res, err := p.call(ctx, p.sql, http.MethodGet, "/v1/projects/"+project+"/instances", nil)
	if err != nil {
		return out
	}
	for _, i := range bdrItems(res, "items") {
		out[str(i, "name")] = [2]string{str(i, "region"), str(i, "databaseVersion")}
	}
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bdrPlanOptions are the project's plans for a select, by full name, each
// labelled "plan (region, vault)".
func (p backupDRProvider) planOptions(ctx context.Context, project string) ([]string, map[string]string) {
	plans, _ := p.list(ctx, project, "backupPlans")
	var opts []string
	labels := map[string]string{}
	for _, x := range plans {
		n := str(x, "name")
		opts = append(opts, n)
		labels[n] = fmt.Sprintf("%s (%s, vault %s)", lastSegment(n), locationOf(n), lastSegment(str(x, "backupVault")))
	}
	return opts, labels
}

func bdrRuleIDs(plan map[string]any) []string {
	var out []string
	for _, r := range bdrItems(plan, "backupRules") {
		out = append(out, str(r, "ruleId"))
	}
	return out
}

// bdrWindow is a rule's backup window, "00:00 to 24:00".
func bdrWindow(r map[string]any) string {
	w := nested(r, "standardSchedule", "backupWindow")
	if len(w) == 0 {
		return ""
	}
	st, _ := w["startHourOfDay"].(float64)
	en, _ := w["endHourOfDay"].(float64)
	return fmt.Sprintf("%02d:00 to %02d:00", int(st), int(en))
}

func bdrRecurrence(r map[string]any) string {
	sch := nested(r, "standardSchedule")
	rt := str(sch, "recurrenceType")
	if rt == "HOURLY" {
		if f, ok := sch["hourlyFrequency"].(float64); ok {
			return fmt.Sprintf("Hourly, every %d hours", int(f))
		}
	}
	if rt == "" {
		return ""
	}
	out := rt[:1] + strings.ToLower(rt[1:])
	var on []string
	for _, k := range []string{"months", "daysOfWeek"} {
		if l, ok := sch[k].([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok && len(s) > 3 {
					on = append(on, s[:1]+strings.ToLower(s[1:3]))
				}
			}
		}
	}
	if l, ok := sch["daysOfMonth"].([]any); ok {
		for _, x := range l {
			if f, ok := x.(float64); ok {
				on = append(on, strconv.Itoa(int(f)))
			}
		}
	}
	if len(on) > 0 {
		out += ", on " + strings.Join(on, " ")
	}
	return out
}

// bdrOnDemandText is a plan's Maximum custom on-demand retention, or the
// default Google uses when it is unset.
func bdrOnDemandText(plan map[string]any) string {
	if f, ok := plan["maxCustomOnDemandRetentionDays"].(float64); ok && f > 0 {
		return fmt.Sprintf("%d days", int(f))
	}
	return "Not set: the vault's minimum enforced retention plus 30 days"
}

func bdrRetention(r map[string]any) string {
	if d, ok := r["backupRetentionDays"].(float64); ok {
		return fmt.Sprintf("%d days", int(d))
	}
	return ""
}

// bdrDuration is a vault's minimum enforced retention, "86400s", in days.
func bdrDuration(s string) string {
	n, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil || s == "" {
		return s
	}
	days := n / 86400
	if days == float64(int64(days)) {
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", int64(days))
	}
	return s
}

func bdrStateWord(s string) string {
	switch s {
	case "ACTIVE":
		return "Succeeded"
	case "CREATING":
		return "Running"
	case "ERROR":
		return "Failed"
	}
	return s
}

// --- Overview -----------------------------------------------------------

// bdrOverviewProvider is Backup and DR's Overview: what the project
// protects, and the page's own actions (trials, protected resources search,
// Assured Workloads), which were the Backup and DR page's (#1536).
type bdrOverviewProvider struct{ api backupDRProvider }

func (bdrOverviewProvider) ID() string    { return "backupdr-overview" }
func (bdrOverviewProvider) Title() string { return "Overview" }

func (p bdrOverviewProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{SummaryOnly: true, Noun: "resources", Note: backupDRNote}
	if project == "" {
		l.Prompt = "Backup and DR shows a project's protection. Choose one in the toolbar."
		return l, nil
	}
	vaults, err := p.api.list(ctx, project, "backupVaults")
	if err != nil {
		l.Unavailable = "cannot reach the Backup and DR Service: " + err.Error()
		return l, nil
	}
	plans, _ := p.api.list(ctx, project, "backupPlans")
	assocs, _ := p.api.list(ctx, project, "backupPlanAssociations")
	srcs, _ := p.api.sources(ctx, project)
	backups := 0
	for _, v := range vaults {
		n, _ := strconv.Atoi(str(v, "backupCount"))
		backups += n
	}
	jobs := map[string]int{}
	since := time.Now().Add(-24 * time.Hour)
	for _, j := range p.api.backupJobs(ctx, srcs) {
		if t, err := time.Parse(time.RFC3339Nano, j.Fields["Start time"]); err == nil && t.After(since) {
			jobs[j.Fields["Job status"]]++
		}
	}
	protected := 0
	for _, s := range srcs {
		if s.association() != "" {
			protected++
		}
	}
	l.Summary = []console.PropertyGroup{
		{Heading: "Protection summary", Properties: []console.Property{
			{Label: "Protected resources", Value: strconv.Itoa(protected), Link: "/backupdr-vaulted-backups"},
			{Label: "Vaulted backups", Value: strconv.Itoa(backups), Link: "/backupdr-vaulted-backups"},
			{Label: "Backup plans", Value: strconv.Itoa(len(plans)), Link: "/backupdr-plans"},
			{Label: "Backup plan associations", Value: strconv.Itoa(len(assocs)), Link: "/backupdr-associations"},
			{Label: "Backup vaults", Value: strconv.Itoa(len(vaults)), Link: "/backupdr"}}},
		{Heading: "Jobs in the last 24 hours", Properties: []console.Property{
			{Label: "Succeeded", Value: strconv.Itoa(jobs["Succeeded"]), Link: "/backupdr-jobs"},
			{Label: "Failed", Value: strconv.Itoa(jobs["Failed"]), Link: "/backupdr-jobs"},
			{Label: "Running", Value: strconv.Itoa(jobs["Running"]), Link: "/backupdr-jobs"}}},
	}
	return l, nil
}

func (p bdrOverviewProvider) ListActions(ctx context.Context, project string) []console.Action {
	return p.api.overviewActions(project)
}

func (p bdrOverviewProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

func (p bdrOverviewProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, v)
	return err
}

func (p bdrOverviewProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if len(path) > 0 {
		return nil, errors.New("the Overview has no resource pages")
	}
	return p.api.ActAtResult(ctx, project, nil, action, v)
}

// --- Vaulted backups ------------------------------------------------------

// bdrVaultedProvider is Vaulted backups: each resource whose backups a vault
// holds (a data source), its page with its backups, Schedule backup, Create
// on-demand backup, Change backup plan, Remove backup plan, Restore and
// Delete.
type bdrVaultedProvider struct{ api backupDRProvider }

func (bdrVaultedProvider) ID() string    { return "backupdr-vaulted" }
func (bdrVaultedProvider) Title() string { return "Vaulted backups" }

var bdrVaultedColumns = []string{"Resource type", "Location", "Backup plan", "Backup vault", "Backups", "Last backup", "Last successful backup"}

func (p bdrVaultedProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: bdrVaultedColumns, NameColumn: "Resource name", Noun: "protected resources", AlwaysStatus: true,
		Note: "The resources with a backup plan applied, or with backups in a backup vault of this project: Cloud SQL instances only (docs/backupdr.md)."}
	if project == "" {
		l.Prompt = "Vaulted backups lists a project's protected resources. Choose one in the toolbar."
		return l, nil
	}
	srcs, err := p.api.sources(ctx, project)
	if err != nil {
		l.Unavailable = "cannot reach the Backup and DR Service: " + err.Error()
		return l, nil
	}
	for _, s := range srcs {
		cfg := nested(s.ds, "backupConfigInfo")
		l.Items = append(l.Items, console.Resource{Name: s.instanceName(), Status: str(s.ds, "configState"),
			Opens: []string{s.vaultID(), s.id()},
			Fields: map[string]string{"Resource type": "Cloud SQL", "Location": s.location(), "Backup plan": lastSegment(s.plan()),
				"Backup vault": s.vaultID(), "Backups": str(s.ds, "backupCount"), "Last backup": bdrLastState(str(cfg, "lastBackupState")),
				"Last successful backup": str(cfg, "lastSuccessfulBackupConsistencyTime")}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func bdrLastState(s string) string {
	if s == "" {
		return ""
	}
	return s[:1] + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

// CreateForm implements console.Creator: Google's Schedule backup.
func (bdrVaultedProvider) CreateForm() (string, []console.Field) {
	return "Schedule backup", bdrScheduleFields(nil, nil, nil, nil)
}

func bdrScheduleFields(instances []string, instLabels map[string]string, plans []string, planLabels map[string]string) []console.Field {
	regions := backupdr.Regions
	return []console.Field{
		{Name: "resourceType", Label: "Resource Type", Type: "radio", Required: true, Section: "Choose resources",
			Options: bdrResourceTypes, OptionLabels: bdrResourceTypeLabels, Default: backupdr.CloudSQLResourceType,
			UnavailableOptions: bdrUnservedTypes()},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: "Choose resources", Options: regions,
			OptionLabels: bdrRegionLabels(), Default: "us-central1", Help: "The region of the instance."},
		{Name: "instance", Label: "Resources", Type: "select", Required: true, Section: "Choose resources", Options: instances,
			OptionLabels: instLabels, Help: "The Cloud SQL instance to back up, in the region chosen."},
		{Name: "plan", Label: "Backup plan", Type: "select", Required: true, Section: "Apply backup plan", Options: plans,
			OptionLabels: planLabels, Help: "A Cloud SQL backup plan in the instance's region."},
	}
}

// CreateFormFor implements console.ProjectCreateFormer: the project's
// instances and plans.
func (p bdrVaultedProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	inst := p.api.sqlInstances(ctx, project)
	labels := map[string]string{}
	for _, n := range sortedKeysOf(inst) {
		labels[n] = n + " (" + inst[n][0] + ")"
	}
	plans, planLabels := p.api.planOptions(ctx, project)
	return bdrScheduleFields(sortedKeysOf(inst), labels, plans, planLabels)
}

func (bdrVaultedProvider) CreateOnPage() bool   { return true }
func (bdrVaultedProvider) CreateTitle() string  { return "Schedule backup" }
func (bdrVaultedProvider) CreateSubmit() string { return "Schedule" }

// Create applies the plan to the instance: a backup plan association,
// whose ID Google's console does not ask for.
func (p bdrVaultedProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	if v["resourceType"] != "" && v["resourceType"] != backupdr.CloudSQLResourceType {
		return "", fmt.Errorf("%s is not served here: Backup and DR backs up Cloud SQL instances only", bdrTypeLabel(v["resourceType"]))
	}
	inst := p.api.sqlInstances(ctx, project)
	i, ok := inst[v["instance"]]
	if !ok {
		return "", fmt.Errorf("Cloud SQL instance %s does not exist in project %s", v["instance"], project)
	}
	if i[0] != v["region"] {
		return "", fmt.Errorf("instance %s is in %s, not %s", v["instance"], i[0], v["region"])
	}
	if locationOf(v["plan"]) != v["region"] {
		return "", fmt.Errorf("backup plan %s is in %s; choose a plan in %s, the instance's region", lastSegment(v["plan"]), locationOf(v["plan"]), v["region"])
	}
	loc := "projects/" + project + "/locations/" + v["region"]
	_, err := p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+loc+"/backupPlanAssociations?backupPlanAssociationId="+url.QueryEscape(bdrAssocID(v["instance"])),
		map[string]any{"backupPlan": v["plan"], "resourceType": backupdr.CloudSQLResourceType, "resource": "projects/" + project + "/instances/" + v["instance"]})
	return v["instance"], err
}

// bdrAssocID is the association ID the page gives an instance's backup
// plan: the instance's name and a time, cut to the API's 63 characters.
func bdrAssocID(instance string) string {
	suffix := "-" + strconv.FormatInt(time.Now().UnixMilli()%1e9, 36)
	if len(instance)+len(suffix) > 63 {
		instance = strings.TrimRight(instance[:63-len(suffix)], "-")
	}
	return instance + suffix
}

// Detail implements console.Driller: a resource's page, [vault, data
// source], with its backups.
func (p bdrVaultedProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 2 {
		return console.Detail{Unavailable: "a vaulted resource is addressed by its backup vault and data source"}, nil
	}
	s, err := p.api.source(ctx, project, path[0], path[1])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	cfg := nested(s.ds, "backupConfigInfo")
	d := console.Detail{Title: s.instanceName(), Summary: []console.Property{
		{Label: "Resource name", Value: s.instance()}, {Label: "Resource type", Value: "Cloud SQL"},
		{Label: "Location", Value: bdrRegionLabel(s.location())},
		{Label: "Backup vault", Value: s.vaultID(), Link: "/backupdr/" + url.PathEscape(s.vaultID())},
		{Label: "Backup plan", Value: lastSegment(s.plan()), Link: bdrLinkIf(s.plan() != "", "/backupdr-plans/"+url.PathEscape(lastSegment(s.plan())))},
		{Label: "Backup plan association", Value: lastSegment(s.association()),
			Link: bdrLinkIf(s.association() != "", "/backupdr-associations/"+url.PathEscape(lastSegment(s.association())))},
		{Label: "Database version", Value: str(nested(s.ds, "dataSourceGcpResource", "cloudSqlInstanceDatasourceProperties"), "databaseInstalledVersion")},
		{Label: "Last backup", Value: bdrLastState(str(cfg, "lastBackupState"))},
		{Label: "Last successful backup", Value: str(cfg, "lastSuccessfulBackupConsistencyTime")},
		{Label: "Data source", Value: str(s.ds, "name")}}}
	if s.association() == "" {
		d.Summary = append(d.Summary, console.Property{Label: "Backup configuration",
			Value: "No backup plan: its backups are kept until they expire. Schedule backup applies a plan again."})
	}
	l := console.Listing{Columns: []string{"Created", "Backup type", "Backup rule", "Expires", "Enforced retention ends"},
		NameColumn: "Backup", Noun: "backups", AlwaysStatus: true}
	if out, err := p.api.get(ctx, str(s.ds, "name")+"/backups"); err != nil {
		l.Unavailable = err.Error()
	} else {
		for _, b := range bdrItems(out, "backups") {
			id := lastSegment(str(b, "name"))
			typ := "Scheduled"
			if str(b, "backupType") == "ON_DEMAND" {
				typ = "On-demand"
			}
			row := console.Resource{Name: id, Status: bdrStateWord(str(b, "state")), Target: []string{path[0], path[1], id},
				Fields: map[string]string{"Created": str(b, "createTime"), "Backup type": typ,
					"Backup rule": str(nested(b, "gcpBackupPlanInfo"), "backupPlanRuleId"), "Expires": str(b, "expireTime"),
					"Enforced retention ends": str(b, "enforcedRetentionEndTime")}}
			row.Actions = p.backupActions(ctx, project, s, b)
			l.Items = append(l.Items, row)
		}
	}
	l.Total = len(l.Items)
	d.Sections = []console.Section{{ID: "backups", Label: "Backups", Listing: l}}
	return d, nil
}

func bdrLinkIf(ok bool, link string) string {
	if ok {
		return link
	}
	return ""
}

// backupActions are one backup's: Restore while it is usable, Delete.
func (p bdrVaultedProvider) backupActions(ctx context.Context, project string, s bdrSource, b map[string]any) []console.Action {
	var out []console.Action
	if str(b, "state") == "ACTIVE" {
		out = append(out, p.restoreAction(ctx, project, s, nil))
	}
	return append(out, console.Action{ID: "backupdelete", Label: "Delete", Destructive: true,
		Confirm: "The backup is deleted and can no longer be restored. A backup inside its enforced retention cannot be deleted."})
}

// restoreAction is Google's Restore: the backup (chosen on the resource's
// page, given on a backup's row), then Choose restore destination.
func (p bdrVaultedProvider) restoreAction(ctx context.Context, project string, s bdrSource, backups []string) console.Action {
	engine := strings.SplitN(str(nested(s.ds, "dataSourceGcpResource", "cloudSqlInstanceDatasourceProperties"), "databaseInstalledVersion"), "_", 2)[0]
	var targets []string
	inst := p.api.sqlInstances(ctx, project)
	for _, n := range sortedKeysOf(inst) {
		if strings.SplitN(inst[n][1], "_", 2)[0] == engine && n != s.instanceName() {
			targets = append(targets, n)
		}
	}
	fields := []console.Field{}
	if backups != nil {
		fields = append(fields, console.Field{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: backups,
			Section: "Choose the backup"})
	}
	dest := []string{"source", "existing", "new"}
	unavail := map[string]string{}
	if len(targets) == 0 {
		unavail["existing"] = "No other " + engine + " instance in this project to overwrite."
	}
	fields = append(fields,
		console.Field{Name: "destination", Label: "Choose restore destination", Type: "radio", Required: true, Section: "Choose restore destination",
			Options: dest, Default: "source", UnavailableOptions: unavail, OptionLabels: map[string]string{
				"source": "Overwrite the source instance", "existing": "Overwrite an existing instance", "new": "Restore to a new instance"}},
		console.Field{Name: "target", Label: "Instance", Type: "select", Options: targets, Section: "Choose restore destination",
			ShowWhen: "destination=existing", Help: "An existing " + engine + " instance of this project; its databases are replaced."},
		// Google: "creates a new instance based on the configuration
		// stored in your backup" (#1922).
		console.Field{Name: "newinstance", Label: "Instance ID", Type: "text", Section: "Choose restore destination",
			ShowWhen: "destination=new", Pattern: `[a-z][a-z0-9\-]{0,96}`,
			Help: "Provide a new Instance ID using lowercase letters, numbers, and hyphens. Start with a letter."})
	return console.Action{ID: "restore", Label: "Restore", Submit: "Restore", Steps: "continue", Fields: fields,
		Confirm: "The destination instance's databases are replaced by the backup's."}
}

// DetailActions implements console.PathActor: a resource's (path of two)
// and a backup's (path of three, its row).
func (p bdrVaultedProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) < 2 || len(path) > 3 {
		return nil
	}
	s, err := p.api.source(ctx, project, path[0], path[1])
	if err != nil {
		return nil
	}
	if len(path) == 3 {
		b, err := p.api.get(ctx, str(s.ds, "name")+"/backups/"+path[2])
		if err != nil {
			return nil
		}
		return p.backupActions(ctx, project, s, b)
	}
	var acts []console.Action
	if a := s.association(); a != "" {
		if plan, err := p.api.get(ctx, s.plan()); err == nil {
			acts = append(acts, console.Action{ID: "ondemand", Label: "Create on-demand backup", Submit: "Create",
				Fields: bdrOnDemandFields(plan)})
		}
		plans, labels := p.api.planOptions(ctx, project)
		var same []string
		for _, n := range plans {
			if locationOf(n) == locationOf(a) && n != s.plan() {
				same = append(same, n)
			}
		}
		if len(same) > 0 {
			acts = append(acts, console.Action{ID: "changeplan", Label: "Change backup plan", Submit: "Apply",
				Fields: []console.Field{{Name: "plan", Label: "Select a backup plan", Type: "select", Required: true, Options: same, OptionLabels: labels,
					Help: "A plan storing into another vault gives the instance a data source there."}}})
		}
		acts = append(acts, console.Action{ID: "removeplan", Label: "Remove backup plan", Destructive: true,
			Confirm: "The instance is no longer backed up. Its existing backups are not deleted."})
	}
	var active []string
	if out, err := p.api.get(ctx, str(s.ds, "name")+"/backups"); err == nil {
		for _, b := range bdrItems(out, "backups") {
			if str(b, "state") == "ACTIVE" {
				active = append(active, lastSegment(str(b, "name")))
			}
		}
	}
	if len(active) > 0 {
		acts = append(acts, p.restoreAction(ctx, project, s, active))
	}
	acts = append(acts, console.Action{ID: "pitr", Label: "Point-in-time restore", Fields: []console.Field{
		{Name: "target", Label: "New instance ID", Type: "text", Required: true, Pattern: `[a-z][a-z0-9\-]{0,96}`},
		{Name: "time", Label: "Point in time", Type: "text", Required: true,
			Help: "An RFC 3339 time inside the instance's recovery window, such as 2026-10-16T12:00:00Z."}}})
	return acts
}

// ActAt implements console.PathActor.
func (p bdrVaultedProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) < 2 || len(path) > 3 {
		return errors.New("a vaulted resource is addressed by its backup vault and data source")
	}
	s, err := p.api.source(ctx, project, path[0], path[1])
	if err != nil {
		return err
	}
	loc := "/v1/projects/" + project + "/locations/" + locationOf(str(s.vault, "name"))
	if len(path) == 3 {
		v["backup"] = path[2]
	}
	switch action {
	case "ondemand":
		if s.association() == "" {
			return errors.New("the resource has no backup plan: Schedule backup applies one")
		}
		body, e := bdrTriggerBody(v)
		if e != nil {
			return e
		}
		_, err = p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+s.association()+":triggerBackup", body)
	case "changeplan":
		if s.association() == "" {
			return errors.New("the resource has no backup plan to change")
		}
		_, err = p.api.call(ctx, p.api.bdr, http.MethodPatch, "/v1/"+s.association()+"?updateMask=backupPlan", map[string]any{"backupPlan": v["plan"]})
	case "removeplan":
		if s.association() == "" {
			return errors.New("the resource has no backup plan to remove")
		}
		_, err = p.api.call(ctx, p.api.bdr, http.MethodDelete, "/v1/"+s.association(), nil)
	case "restore":
		target := s.instanceName()
		switch v["destination"] {
		case "", "source":
		case "existing":
			if v["target"] == "" {
				return errors.New("choose the instance to overwrite")
			}
			target = v["target"]
		case "new":
			// gcloud sql backups restore NAME --restore-instance=NEW sends
			// the backup alone: the instance takes the backup's settings.
			if v["newinstance"] == "" {
				return errors.New("give the new instance's ID")
			}
			target = v["newinstance"]
		default:
			return fmt.Errorf("unknown restore destination %q", v["destination"])
		}
		if v["backup"] == "" {
			return errors.New("choose the backup to restore")
		}
		if p.api.sql == nil {
			return errors.New("the Cloud SQL Admin API is not served by this instance")
		}
		_, err = p.api.call(ctx, p.api.sql, http.MethodPost, "/v1/projects/"+project+"/instances/"+url.PathEscape(target)+"/restoreBackup",
			map[string]any{"backupdrBackup": str(s.ds, "name") + "/backups/" + v["backup"]})
	case "pitr":
		if p.api.sql == nil {
			return errors.New("the Cloud SQL Admin API is not served by this instance")
		}
		_, err = p.api.call(ctx, p.api.sql, http.MethodPost, "/v1/projects/"+project+":pointInTimeRestore",
			map[string]any{"datasource": str(s.ds, "name"), "targetInstance": v["target"], "pointInTime": v["time"]})
	case "backupdelete":
		if v["backup"] == "" {
			return errors.New("delete is a backup's action")
		}
		_, err = p.api.call(ctx, p.api.bdr, http.MethodDelete, "/v1/"+str(s.ds, "name")+"/backups/"+url.PathEscape(v["backup"]), nil)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	_ = loc
	return err
}

// --- Backup plans -----------------------------------------------------------

// bdrPlansProvider is Backup plans: every plan of the project, Create Backup
// plan as Google's page, and a plan's own page.
type bdrPlansProvider struct{ api backupDRProvider }

func (bdrPlansProvider) ID() string    { return "backupdr-plans" }
func (bdrPlansProvider) Title() string { return "Backup plans" }

func (p bdrPlansProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Resource type", "Location", "Backup vault", "Backup rules", "Resources", "Description"},
		NameColumn: "Backup plan", Noun: "backup plans", RowsOpenable: true, AlwaysStatus: true}
	if project == "" {
		l.Prompt = "Backup plans lists a project's plans. Choose one in the toolbar."
		return l, nil
	}
	plans, err := p.api.list(ctx, project, "backupPlans")
	if err != nil {
		l.Unavailable = "cannot reach the Backup and DR Service: " + err.Error()
		return l, nil
	}
	uses := map[string]int{}
	if assocs, err := p.api.list(ctx, project, "backupPlanAssociations"); err == nil {
		for _, a := range assocs {
			uses[str(a, "backupPlan")]++
		}
	}
	for _, x := range plans {
		l.Items = append(l.Items, console.Resource{Name: lastSegment(str(x, "name")), Status: str(x, "state"), Fields: map[string]string{
			"Resource type": bdrTypeLabel(str(x, "resourceType")), "Location": locationOf(str(x, "name")),
			"Backup vault": lastSegment(str(x, "backupVault")), "Backup rules": strconv.Itoa(len(bdrItems(x, "backupRules"))),
			"Resources": strconv.Itoa(uses[str(x, "name")]), "Description": str(x, "description")}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm implements console.Creator: Google's Create Backup plan.
func (bdrPlansProvider) CreateForm() (string, []console.Field) {
	return "Create Backup plan", bdrPlanFields(nil, nil)
}

func bdrHours(from, to int) ([]string, map[string]string) {
	var opts []string
	labels := map[string]string{}
	for h := from; h <= to; h++ {
		s := strconv.Itoa(h)
		opts = append(opts, s)
		labels[s] = fmt.Sprintf("%02d:00", h)
	}
	return opts, labels
}

func bdrPlanFields(vaults []string, vaultLabels map[string]string) []console.Field {
	starts, startLabels := bdrHours(0, 23)
	ends, endLabels := bdrHours(1, 24)
	recur := []string{"HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"}
	return []console.Field{
		{Name: "resourceType", Label: "Resource Type", Type: "radio", Required: true, Section: "Resource Type",
			Options: bdrResourceTypes, OptionLabels: bdrResourceTypeLabels, Default: backupdr.CloudSQLResourceType,
			UnavailableOptions: bdrUnservedTypes()},
		{Name: "id", Label: "Name", Type: "text", Required: true, Section: "Identity", Pattern: bdrIDPattern, Help: bdrIDHelp + "."},
		{Name: "description", Label: "Description", Type: "text", Section: "Identity"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: "Location", Options: backupdr.Regions,
			OptionLabels: bdrRegionLabels(), Default: "us-central1", Help: "The plan is active in this region; it protects the resources there."},
		{Name: "vault", Label: "Backup vault", Type: "select", Required: true, Section: "Destination", Options: vaults, OptionLabels: vaultLabels,
			Help: "A backup vault of this project in the plan's region."},
		{Name: "rules", Label: "Backup rules", Type: console.GroupFieldType, Required: true, Section: "Backup rules", AddLabel: "Add rule",
			Default: console.FormatGroup([]map[string]string{{"ruleId": "daily-backup", "recurrence": "DAILY", "hourly": "6", "start": "0", "end": "24", "retention": "7"}}),
			Help: "Each rule's recurrence, its backup window (24-hour clock, start inclusive, end exclusive, in the plan's time zone) and how many " +
				"days its backups are kept; an hourly rule repeats every 6 to 23 hours. A weekly rule runs on its days of the week; a monthly rule " +
				"on its days of the month; a yearly rule in its months, on its days of the month.",
			Group: []console.Field{
				{Name: "ruleId", Label: "Rule name", Type: "text", Required: true, Pattern: bdrIDPattern, Help: bdrIDHelp},
				{Name: "recurrence", Label: "Recurrence", Type: "select", Options: recur, OptionLabels: map[string]string{
					"HOURLY": "Hourly", "DAILY": "Daily", "WEEKLY": "Weekly", "MONTHLY": "Monthly", "YEARLY": "Yearly"}},
				{Name: "hourly", Label: "Every (hours, hourly only)", Type: "text", Pattern: `[6-9]|1[0-9]|2[0-3]`, Help: "6 to 23"},
				{Name: "daysOfWeek", Label: "Days of the week (weekly only)", Type: "text", Pattern: bdrTokensPattern,
					Help: "Such as MON WED: MON, TUE, WED, THU, FRI, SAT, SUN."},
				{Name: "daysOfMonth", Label: "Days of the month (monthly and yearly only)", Type: "text", Pattern: `[0-9 ,]*`,
					Help: "Such as 1 15: 1 to 31."},
				{Name: "months", Label: "Months (yearly only)", Type: "text", Pattern: bdrTokensPattern,
					Help: "Such as MAR JUN SEP DEC: JAN to DEC."},
				{Name: "start", Label: "Backup window start", Type: "select", Options: starts, OptionLabels: startLabels},
				{Name: "end", Label: "Backup window end", Type: "select", Options: ends, OptionLabels: endLabels, Default: "24"},
				{Name: "retention", Label: "Retention period (days)", Type: "text", Required: true, Pattern: `[1-9][0-9]{0,4}`, Help: "1 to 99999 days"},
			}},
		{Name: "timeZone", Label: "Time zone", Type: "text", Required: true, Section: "Backup rules", Default: "UTC",
			Help: "The plan's time zone, an IANA name such as UTC or Europe/Paris: its rules' windows and days are read in it."},
		{Name: "ondemand", Label: "Maximum custom on-demand retention", Type: "checkbox", Section: "On-Demand Retention",
			Help: "Caps the retention of on-demand backups taken with a custom retention. Unset, the cap is the vault's minimum " +
				"enforced retention plus 30 days. With it set, the plan may have no backup rules."},
		{Name: "ondemandDays", Label: "Maximum retention (days)", Type: "text", Section: "On-Demand Retention", ShowWhen: "ondemand",
			Pattern: `[1-9][0-9]{0,4}`, Help: "1 to 36159 days; at least the vault's minimum enforced retention."},
		{Name: "logs", Label: "Database Logs", Type: "checkbox", Section: "Database Logs",
			Help: "Keeps the instance's transaction logs, for point-in-time restore."},
		{Name: "logDays", Label: "Log retention (days)", Type: "text", Section: "Database Logs", ShowWhen: "logs", Default: "7",
			Pattern: `[1-9][0-9]{0,4}`, Help: "How long the logs are kept; at least the vault's minimum enforced retention."},
	}
}

// CreateFormFor implements console.ProjectCreateFormer: the project's vaults.
func (p bdrPlansProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	vaults, _ := p.api.list(ctx, project, "backupVaults")
	var opts []string
	labels := map[string]string{}
	for _, v := range vaults {
		n := str(v, "name")
		opts = append(opts, n)
		labels[n] = lastSegment(n) + " (" + locationOf(n) + ")"
	}
	return bdrPlanFields(opts, labels)
}

func (bdrPlansProvider) CreateOnPage() bool   { return true }
func (bdrPlansProvider) CreateTitle() string  { return "Create backup plan" }
func (bdrPlansProvider) CreateSubmit() string { return "Create" }

// bdrRulesFrom reads the Backup rules group into the API's rules, checking
// what Google's form checks.
func bdrRulesFrom(group, tz string, optional bool) ([]any, error) {
	rows, err := console.ParseGroup(group)
	if err != nil {
		return nil, err
	}
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, fmt.Errorf("the time zone %q is not an IANA time zone such as UTC or Europe/Paris", tz)
	}
	if len(rows) == 0 && !optional {
		return nil, errors.New("add at least one backup rule, or set a maximum custom on-demand retention")
	}
	var out []any
	seen := map[string]bool{}
	for i, r := range rows {
		n := i + 1
		if !bdrIDRe.MatchString(r["ruleId"]) {
			return nil, fmt.Errorf("rule %d: the rule name must be %s", n, bdrIDHelp)
		}
		if seen[r["ruleId"]] {
			return nil, fmt.Errorf("rule %d: the rule name %s is used twice", n, r["ruleId"])
		}
		seen[r["ruleId"]] = true
		ret, err := strconv.Atoi(r["retention"])
		if err != nil || ret < 1 {
			return nil, fmt.Errorf("rule %d: the retention period is a number of days, at least 1", n)
		}
		st, _ := strconv.Atoi(r["start"])
		en := 24
		if r["end"] != "" {
			en, _ = strconv.Atoi(r["end"])
		}
		if en <= st {
			return nil, fmt.Errorf("rule %d: the backup window must end after it starts", n)
		}
		rec := r["recurrence"]
		if rec == "" {
			rec = "DAILY"
		}
		sch := map[string]any{"recurrenceType": rec, "timeZone": tz,
			"backupWindow": map[string]any{"startHourOfDay": st, "endHourOfDay": en}}
		if err := bdrApplyDays(sch, rec, r["daysOfWeek"], r["daysOfMonth"], r["months"], false); err != nil {
			return nil, fmt.Errorf("rule %d: %w", n, err)
		}
		if rec == "HOURLY" {
			h, err := strconv.Atoi(r["hourly"])
			if err != nil || h < 6 || h > 23 {
				return nil, fmt.Errorf("rule %d: an hourly rule repeats every 6 to 23 hours", n)
			}
			sch["hourlyFrequency"] = h
		}
		out = append(out, map[string]any{"ruleId": r["ruleId"], "backupRetentionDays": ret, "standardSchedule": sch})
	}
	return out, nil
}

// Create makes the plan through backupPlans.create.
func (p bdrPlansProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	if v["resourceType"] != "" && v["resourceType"] != backupdr.CloudSQLResourceType {
		return "", fmt.Errorf("%s is not served here: Backup and DR backs up Cloud SQL instances only", bdrTypeLabel(v["resourceType"]))
	}
	if !bdrIDRe.MatchString(v["id"]) {
		return "", fmt.Errorf("the name must be %s", bdrIDHelp)
	}
	if vl := locationOf(v["vault"]); vl != v["region"] && !(backupdr.IsMultiRegion(vl) && strings.HasPrefix(v["region"], backupdr.MultiRegions[vl])) {
		return "", fmt.Errorf("backup vault %s is in %s; choose a vault in %s, the plan's region, or in the multi-region that spans it",
			lastSegment(v["vault"]), vl, v["region"])
	}
	ondemand := v["ondemand"] == "true" || v["ondemand"] == "on"
	rules, err := bdrRulesFrom(v["rules"], strings.TrimSpace(v["timeZone"]), ondemand)
	if err != nil {
		return "", err
	}
	body := map[string]any{"backupVault": v["vault"], "resourceType": backupdr.CloudSQLResourceType, "backupRules": rules}
	if ondemand {
		n, err := strconv.Atoi(strings.TrimSpace(v["ondemandDays"]))
		if err != nil || n < 1 || n > 36159 {
			return "", errors.New("the maximum custom on-demand retention is 1 to 36159 days")
		}
		body["maxCustomOnDemandRetentionDays"] = n
	}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if v["logs"] == "true" {
		n, err := strconv.Atoi(v["logDays"])
		if err != nil || n < 1 {
			return "", errors.New("the log retention is a number of days, at least 1")
		}
		body["logRetentionDays"] = n
	}
	_, err = p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/projects/"+project+"/locations/"+url.PathEscape(v["region"])+
		"/backupPlans?backupPlanId="+url.QueryEscape(v["id"]), body)
	return v["id"], err
}

// Detail implements console.Driller: a plan's page.
func (p bdrPlansProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	x, err := p.api.planByID(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	name := str(x, "name")
	var retention []string
	for _, r := range bdrItems(x, "backupRules") {
		retention = append(retention, str(r, "ruleId")+": "+bdrRetention(r))
	}
	d := console.Detail{Summary: []console.Property{
		{Label: "Resource type", Value: bdrTypeLabel(str(x, "resourceType"))},
		{Label: "Description", Value: str(x, "description")},
		{Label: "Status", Value: str(x, "state")},
		{Label: "Location", Value: bdrRegionLabel(locationOf(name))},
		{Label: "Backup vault name", Value: lastSegment(str(x, "backupVault")), Link: "/backupdr/" + url.PathEscape(lastSegment(str(x, "backupVault")))},
		{Label: "Backup storage location", Value: bdrRegionLabel(locationOf(str(x, "backupVault")))},
		{Label: "Backup retention period", Value: strings.Join(retention, "; ")},
		{Label: "Log retention", Value: bdrDaysText(str(x, "logRetentionDays"))},
		{Label: "Maximum custom on-demand retention", Value: bdrOnDemandText(x)},
		{Label: "Revision", Value: str(x, "revisionId")},
		{Label: "Created", Value: str(x, "createTime")}, {Label: "Last updated", Value: str(x, "updateTime")}}}
	rules := console.Listing{Columns: []string{"Recurrence", "Backup window", "Time zone", "Retention period"}, NameColumn: "Backup rule", Noun: "backup rules"}
	for _, r := range bdrItems(x, "backupRules") {
		rules.Items = append(rules.Items, console.Resource{Name: str(r, "ruleId"), Fields: map[string]string{
			"Recurrence": bdrRecurrence(r), "Backup window": bdrWindow(r), "Time zone": str(nested(r, "standardSchedule"), "timeZone"),
			"Retention period": bdrRetention(r)}})
	}
	rules.Total = len(rules.Items)
	applied := console.Listing{Columns: []string{"Resource type", "Backup plan association", "Last backup"}, NameColumn: "Resource", Noun: "resources"}
	if assocs, err := p.api.list(ctx, project, "backupPlanAssociations"); err == nil {
		for _, a := range assocs {
			if str(a, "backupPlan") != name {
				continue
			}
			applied.Items = append(applied.Items, console.Resource{Name: lastSegment(str(a, "resource")), Status: str(a, "state"),
				Link: "/backupdr-associations/" + url.PathEscape(lastSegment(str(a, "name"))),
				Fields: map[string]string{"Resource type": bdrTypeLabel(str(a, "resourceType")), "Backup plan association": lastSegment(str(a, "name")),
					"Last backup": bdrAssocLast(a)}})
		}
	}
	applied.Total = len(applied.Items)
	revs := console.Listing{Columns: []string{"Created", "Backup rules"}, NameColumn: "Revision", Noun: "revisions"}
	if out, err := p.api.get(ctx, name+"/revisions"); err != nil {
		revs.Unavailable = err.Error()
	} else {
		for _, r := range bdrItems(out, "backupPlanRevisions") {
			snap, _ := r["backupPlanSnapshot"].(map[string]any)
			revs.Items = append(revs.Items, console.Resource{Name: str(r, "revisionId"), Status: str(r, "state"),
				Fields: map[string]string{"Created": str(r, "createTime"), "Backup rules": rulesText(snap)}})
		}
	}
	revs.Total = len(revs.Items)
	d.Sections = []console.Section{{ID: "rules", Label: "Backup rules", Listing: rules},
		{ID: "resources", Label: "Resources", Listing: applied}, {ID: "revisions", Label: "Revisions", Listing: revs}}
	return d, nil
}

func bdrDaysText(s string) string {
	if s == "" || s == "0" {
		return ""
	}
	return s + " days"
}

func bdrAssocLast(a map[string]any) string {
	var last []string
	for _, r := range bdrItems(a, "rulesConfigInfo") {
		if st := str(r, "lastBackupState"); st != "" {
			last = append(last, str(r, "ruleId")+": "+bdrLastState(st))
		}
	}
	return strings.Join(last, ", ")
}

// DetailActions implements console.PathActor: Schedule backup with this
// plan, Edit and Delete.
func (p bdrPlansProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	x, err := p.api.planByID(ctx, project, path[0])
	if err != nil {
		return nil
	}
	return []console.Action{
		{ID: "schedule", Label: "Apply to a resource", Href: "/backupdr-vaulted-backups/create?region=" + url.QueryEscape(locationOf(str(x, "name"))) +
			"&plan=" + url.QueryEscape(str(x, "name"))},
		{ID: "editplan", Label: "Edit", Submit: "Save", Fields: []console.Field{
			{Name: "rule", Label: "Backup rule", Type: "select", Required: true, Options: bdrRuleIDs(x),
				Help: "Each edit makes a new revision of the plan, listed under Revisions."},
			{Name: "recurrence", Label: "Recurrence", Type: "select", Options: []string{"", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"},
				OptionLabels: map[string]string{"": "Keep the rule's"}},
			{Name: "retention", Label: "Retention period (days)", Type: "text", Pattern: `[1-9][0-9]{0,4}`, Help: "Empty keeps the rule's."},
			{Name: "description", Label: "Description", Type: "text", Default: str(x, "description"), Help: "Empty keeps the plan's."}}},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
			Confirm: "The backup plan is deleted. A plan applied to a resource cannot be: remove it from the resource first."},
	}
}

// ActAt implements console.PathActor.
func (p bdrPlansProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("a backup plan is addressed by its ID")
	}
	x, err := p.api.planByID(ctx, project, path[0])
	if err != nil {
		return err
	}
	name := str(x, "name")
	loc := name[:strings.Index(name, "/backupPlans/")]
	switch action {
	case "editplan":
		v["plan"] = path[0]
		if v["description"] == str(x, "description") {
			v["description"] = ""
		}
		return p.api.editPlan(ctx, loc, v)
	case "delete":
		_, err = p.api.call(ctx, p.api.bdr, http.MethodDelete, "/v1/"+name, nil)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// --- Backup plan associations -------------------------------------------------

// bdrAssocsProvider is Backup plan associations: each resource a plan is
// applied to, with Create on-demand backup, Change backup plan and Remove
// backup plan on its page.
type bdrAssocsProvider struct{ api backupDRProvider }

func (bdrAssocsProvider) ID() string    { return "backupdr-associations" }
func (bdrAssocsProvider) Title() string { return "Backup plan associations" }

func (p bdrAssocsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Resource", "Resource type", "Location", "Backup plan", "Backup vault", "Last backup", "Created"},
		NameColumn: "Backup plan association", Noun: "backup plan associations", RowsOpenable: true, AlwaysStatus: true}
	if project == "" {
		l.Prompt = "Backup plan associations lists a project's. Choose one in the toolbar."
		return l, nil
	}
	assocs, err := p.api.list(ctx, project, "backupPlanAssociations")
	if err != nil {
		l.Unavailable = "cannot reach the Backup and DR Service: " + err.Error()
		return l, nil
	}
	for _, a := range assocs {
		l.Items = append(l.Items, console.Resource{Name: lastSegment(str(a, "name")), Status: str(a, "state"), Fields: map[string]string{
			"Resource": lastSegment(str(a, "resource")), "Resource type": bdrTypeLabel(str(a, "resourceType")),
			"Location": locationOf(str(a, "name")), "Backup plan": lastSegment(str(a, "backupPlan")),
			"Backup vault": bdrVaultOf(str(a, "dataSource")), "Last backup": bdrAssocLast(a), "Created": str(a, "createTime")}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// bdrVaultOf is the vault ID of a data source's name.
func bdrVaultOf(ds string) string {
	_, rest, ok := strings.Cut(ds, "/backupVaults/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

func (p bdrAssocsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	a, err := p.api.assocByID(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ds := str(a, "dataSource")
	d := console.Detail{Summary: []console.Property{
		{Label: "Resource", Value: str(a, "resource")}, {Label: "Resource type", Value: bdrTypeLabel(str(a, "resourceType"))},
		{Label: "Status", Value: str(a, "state")}, {Label: "Location", Value: bdrRegionLabel(locationOf(str(a, "name")))},
		{Label: "Backup plan", Value: lastSegment(str(a, "backupPlan")), Link: "/backupdr-plans/" + url.PathEscape(lastSegment(str(a, "backupPlan")))},
		{Label: "Backup plan revision", Value: str(a, "backupPlanRevisionId")},
		{Label: "Backup vault", Value: bdrVaultOf(ds), Link: bdrLinkIf(ds != "", "/backupdr/"+url.PathEscape(bdrVaultOf(ds)))},
		{Label: "Vaulted backups", Value: lastSegment(ds), Link: bdrLinkIf(ds != "", "/backupdr-vaulted-backups/"+url.PathEscape(bdrVaultOf(ds))+"/"+url.PathEscape(lastSegment(ds)))},
		{Label: "Created", Value: str(a, "createTime")}, {Label: "Last updated", Value: str(a, "updateTime")}}}
	rules := console.Listing{Columns: []string{"Last backup", "Last successful backup"}, NameColumn: "Backup rule", Noun: "backup rules"}
	for _, r := range bdrItems(a, "rulesConfigInfo") {
		rules.Items = append(rules.Items, console.Resource{Name: str(r, "ruleId"), Fields: map[string]string{
			"Last backup": bdrLastState(str(r, "lastBackupState")), "Last successful backup": str(r, "lastSuccessfulBackupConsistencyTime")}})
	}
	rules.Total = len(rules.Items)
	d.Sections = []console.Section{{ID: "rules", Label: "Backup rules", Listing: rules}}
	return d, nil
}

func (p bdrAssocsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	a, err := p.api.assocByID(ctx, project, path[0])
	if err != nil {
		return nil
	}
	var acts []console.Action
	if plan, err := p.api.get(ctx, str(a, "backupPlan")); err == nil {
		acts = append(acts, console.Action{ID: "ondemand", Label: "Create on-demand backup", Submit: "Create",
			Fields: bdrOnDemandFields(plan)})
	}
	plans, labels := p.api.planOptions(ctx, project)
	var same []string
	for _, n := range plans {
		if locationOf(n) == locationOf(str(a, "name")) && n != str(a, "backupPlan") {
			same = append(same, n)
		}
	}
	if len(same) > 0 {
		acts = append(acts, console.Action{ID: "changeplan", Label: "Change backup plan", Submit: "Apply",
			Fields: []console.Field{{Name: "plan", Label: "Select a backup plan", Type: "select", Required: true, Options: same, OptionLabels: labels}}})
	}
	return append(acts, console.Action{ID: "delete", Label: "Remove backup plan", Destructive: true, Leaves: true,
		Confirm: "The resource is no longer backed up. Its existing backups are not deleted."})
}

func (p bdrAssocsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return errors.New("a backup plan association is addressed by its ID")
	}
	a, err := p.api.assocByID(ctx, project, path[0])
	if err != nil {
		return err
	}
	name := str(a, "name")
	loc := name[:strings.Index(name, "/backupPlanAssociations/")]
	switch action {
	case "ondemand":
		body, e := bdrTriggerBody(v)
		if e != nil {
			return e
		}
		if body["ruleId"] != nil {
			if err := p.api.checkTriggerRule(ctx, loc, path[0], v["rule"]); err != nil {
				return err
			}
		}
		_, err = p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+name+":triggerBackup", body)
	case "changeplan":
		_, err = p.api.call(ctx, p.api.bdr, http.MethodPatch, "/v1/"+name+"?updateMask=backupPlan", map[string]any{"backupPlan": v["plan"]})
	case "delete":
		_, err = p.api.call(ctx, p.api.bdr, http.MethodDelete, "/v1/"+name, nil)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

// --- Jobs -----------------------------------------------------------------

// bdrJobsProvider is Jobs: the backup jobs (each backup the vaults hold or
// are taking) on its Jobs tab, the backup plan and association jobs (the
// service's operations) on System Jobs.
type bdrJobsProvider struct{ api backupDRProvider }

func (bdrJobsProvider) ID() string    { return "backupdr-jobs" }
func (bdrJobsProvider) Title() string { return "Jobs" }

var bdrJobColumns = []string{"Job category", "Job status", "Resource name", "Resource type", "Resource location", "Backup plan name",
	"Backup rule", "Backup vault name", "Incremental backup size GiB", "Start time", "Duration", "Error code and message",
	"Restore project name", "Restore resource name", "Recovery Point Time", "Backup name"}

// backupJobs is a row per backup of the sources, newest first.
func (p backupDRProvider) backupJobs(ctx context.Context, srcs []bdrSource) []console.Resource {
	var out []console.Resource
	for _, s := range srcs {
		res, err := p.get(ctx, str(s.ds, "name")+"/backups")
		if err != nil {
			continue
		}
		for _, b := range bdrItems(res, "backups") {
			cat := "Scheduled backup"
			if str(b, "backupType") == "ON_DEMAND" {
				cat = "On-demand backup"
			}
			st := bdrStateWord(str(b, "state"))
			out = append(out, console.Resource{Name: lastSegment(str(b, "name")), Status: st,
				Link: "/backupdr-vaulted-backups/" + url.PathEscape(s.vaultID()) + "/" + url.PathEscape(s.id()),
				Fields: map[string]string{"Job category": cat, "Job status": st, "Resource name": s.instanceName(), "Resource type": "Cloud SQL",
					"Resource location": s.location(), "Backup plan name": lastSegment(str(nested(b, "gcpBackupPlanInfo"), "backupPlan")),
					"Backup rule": str(nested(b, "gcpBackupPlanInfo"), "backupPlanRuleId"), "Backup vault name": s.vaultID(),
					"Start time": str(b, "createTime"), "End time": str(b, "consistencyTime"), "Backup name": str(b, "name")}})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fields["Start time"] > out[j].Fields["Start time"] })
	return out
}

func (p bdrJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.ListFiltered(ctx, project, "")
}

// bdrRanges are the Jobs page's time ranges; 24 hours is the default.
var bdrRanges = map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour,
	"7d": 7 * 24 * time.Hour, "14d": 14 * 24 * time.Hour}

const bdrRangeHelp = "The time range: 1h, 6h, 12h, 24h (the default), 7d or 14d."

// ListFiltered implements console.FilterLister: the filter is Google's
// time range (#1924).
func (p bdrJobsProvider) ListFiltered(ctx context.Context, project, filter string) (console.Listing, error) {
	rng := strings.ToLower(strings.TrimSpace(filter))
	if rng == "" {
		rng = "24h"
	}
	sf := &console.ServerFilter{Value: filter, Placeholder: "24h", Help: bdrRangeHelp}
	l := console.Listing{Columns: bdrJobColumns, NameColumn: "Job ID", Noun: "jobs", AlwaysStatus: true, ServerFilter: sf,
		ListTab: "jobs",
		Note: "The backup and restore jobs of the project, from the job logs the Backup and DR Service writes to Cloud Logging " +
			"(bdr_backup_restore_jobs and bdr_backup_plan_jobs), in the chosen time range."}
	d, ok := bdrRanges[rng]
	if !ok {
		sf.Error = bdrRangeHelp
		return l, nil
	}
	if project == "" {
		l.Prompt = "Jobs lists a project's backup and restore jobs. Choose one in the toolbar."
		return l, nil
	}
	srv, _ := p.api.bdr.(*backupdr.Server)
	if srv == nil {
		l.Unavailable = "the Backup and DR Service is not served by this instance"
		return l, nil
	}
	var system []console.PropertyGroup
	for _, j := range srv.Jobs(project, time.Now().Add(-d)) {
		if j.Log == backupdr.BackupPlanJobsLog {
			system = append(system, bdrSystemJob(j))
			continue
		}
		l.Items = append(l.Items, bdrJobRow(j))
	}
	l.Total = len(l.Items)
	failures := []console.PropertyGroup{}
	for _, f := range srv.CurrentFailures(project) {
		failures = append(failures, console.PropertyGroup{Heading: "Current failures: " + f.ErrorType, Properties: []console.Property{
			{Label: "Error type", Value: f.ErrorType}, {Label: "Source project name", Value: f.Project},
			{Label: "Current failures", Value: strconv.Itoa(f.Count)}, {Label: "Error code", Value: strconv.Itoa(f.ErrorCode)},
			{Label: "Error message", Value: f.ErrorMessage}, {Label: "Resource ID", Value: strings.Join(f.ResourceIDs, ", ")}}})
	}
	if len(failures) == 0 {
		failures = append(failures, console.PropertyGroup{Heading: "Current failures", Properties: []console.Property{
			{Label: "Current failures", Value: "0: no resource's last backup job failed"}}})
	}
	if len(system) == 0 {
		system = append(system, console.PropertyGroup{Heading: "System jobs", Properties: []console.Property{
			{Label: "Jobs", Value: "No backup plan or backup plan association job in this time range"}}})
	}
	l.Tabs = []console.Section{{ID: "jobs", Label: "Jobs", Groups: failures},
		{ID: "system", Label: "System Jobs", Groups: system,
			Note: "Each backup plan and backup plan association job: create, update and delete, in the chosen time range."}}
	return l, nil
}

var bdrCategoryLabels = map[string]string{"SCHEDULED_BACKUP": "Scheduled backup", "ON_DEMAND_BACKUP": "On-demand backup",
	"RESTORE": "Restore", "PITR": "Point-in-time restore"}

var bdrJobStatusLabels = map[string]string{"RUNNING": "Running", "SUCCESSFUL": "Succeeded", "FAILED": "Failed", "SKIPPED": "Skipped"}

// bdrJobRow is a backup or restore job's row.
func bdrJobRow(j backupdr.Job) console.Resource {
	st := bdrJobStatusLabels[j.Status()]
	errText := ""
	if j.Field("errorType") != "" {
		code, _ := j.Payload["errorCode"].(float64)
		errText = fmt.Sprintf("%d %s: %s", int(code), j.Field("errorType"), j.Field("errorMessage"))
	}
	size := ""
	if b, ok := j.Payload["incrementalBackupSizeGib"].(float64); ok {
		size = strconv.FormatFloat(b, 'f', -1, 64)
	}
	restoreProject, restoreName := "", ""
	if r := j.Field("restoreResourceName"); r != "" {
		parts := strings.Split(r, "/")
		if len(parts) == 4 {
			restoreProject, restoreName = parts[1], parts[3]
		}
	}
	link := ""
	if ds := j.Field("dataSourceName"); ds != "" {
		parts := strings.Split(ds, "/")
		if len(parts) == 8 {
			link = "/backupdr-vaulted-backups/" + url.PathEscape(parts[5]) + "/" + url.PathEscape(parts[7])
		}
	}
	return console.Resource{Name: j.ID(), Status: st, Link: link, Fields: map[string]string{
		"Job category": bdrCategoryLabels[j.Field("jobCategory")], "Job status": st,
		"Resource name": lastSegment(j.Field("sourceResourceName")), "Resource type": j.Field("resourceType"),
		"Resource location": j.Field("sourceResourceLocation"), "Backup plan name": lastSegment(j.Field("backupPlanName")),
		"Backup rule": j.Field("backupRule"), "Backup vault name": lastSegment(j.Field("backupVaultName")),
		"Incremental backup size GiB": size, "Start time": j.Field("startTime"), "Duration": j.Field("duration"),
		"Error code and message": errText, "Restore project name": restoreProject, "Restore resource name": restoreName,
		"Recovery Point Time": j.Field("recoveryPointTime"), "Backup name": j.Field("backupName")}}
}

// bdrSystemJob is a backup plan or association job, as a group of its
// columns.
func bdrSystemJob(j backupdr.Job) console.PropertyGroup {
	props := []console.Property{{Label: "Job ID", Value: j.ID()}, {Label: "Job category", Value: j.Field("jobCategory")},
		{Label: "Job status", Value: bdrJobStatusLabels[j.Status()]}}
	add := func(label, field string) {
		if v := j.Field(field); v != "" {
			props = append(props, console.Property{Label: label, Value: v})
		}
	}
	if a := j.Field("backupPlanAssociationName"); a != "" {
		add("Resource name", "sourceResourceName")
		add("Resource ID", "sourceResourceId")
		add("Resource location", "sourceResourceLocation")
		add("Resource type", "resourceType")
		add("Backup plan association name", "backupPlanAssociationName")
		add("Name of the previous backup plan", "previousBackupPlanName")
		add("Previous backup plan revision ID", "previousBackupPlanRevisionId")
		add("Name of the new backup plan", "newBackupPlanName")
		add("New backup plan revision ID", "newBackupPlanRevisionId")
	} else {
		add("Resource type", "resourceType")
		add("Backup plan name", "backupPlanName")
		add("Previous backup plan revision ID", "previousBackupPlanRevisionId")
		add("New backup plan revision ID", "newBackupPlanRevisionId")
		if n, ok := j.Payload["workloadsAffected"].(float64); ok {
			props = append(props, console.Property{Label: "Number of workloads affected", Value: strconv.Itoa(int(n))})
		}
	}
	add("Start time", "startTime")
	add("End time", "endTime")
	add("Duration", "duration")
	add("Error type", "errorType")
	add("Error message", "errorMessage")
	heading := "Backup plan job"
	if j.Field("backupPlanAssociationName") != "" {
		heading = "Backup plan association job"
	}
	return console.PropertyGroup{Heading: heading + ": " + strings.ToLower(strings.ReplaceAll(j.Field("jobCategory"), "_", " ")), Properties: props}
}

// bdrTokensPattern is a list of day or month names, such as MON WED.
const bdrTokensPattern = `[A-Za-z ,]*`

var (
	bdrWeekdays = map[string]string{"MON": "MONDAY", "TUE": "TUESDAY", "WED": "WEDNESDAY", "THU": "THURSDAY", "FRI": "FRIDAY",
		"SAT": "SATURDAY", "SUN": "SUNDAY"}
	bdrMonths = map[string]string{"JAN": "JANUARY", "FEB": "FEBRUARY", "MAR": "MARCH", "APR": "APRIL", "MAY": "MAY", "JUN": "JUNE",
		"JUL": "JULY", "AUG": "AUGUST", "SEP": "SEPTEMBER", "OCT": "OCTOBER", "NOV": "NOVEMBER", "DEC": "DECEMBER"}
)

// bdrNames reads "MON WED" (or MONDAY, comma-separated) as the API's names.
func bdrNames(v string, names map[string]string, what string) ([]any, error) {
	var out []any
	for _, t := range strings.FieldsFunc(strings.ToUpper(v), func(r rune) bool { return r == ' ' || r == ',' }) {
		full := names[t]
		if full == "" && len(t) > 3 && names[t[:3]] == t {
			full = t
		}
		if full == "" {
			return nil, fmt.Errorf("%q is not a %s", t, what)
		}
		out = append(out, full)
	}
	return out, nil
}

// bdrApplyDays sets a rule's days for its recurrence (#1923): daysOfWeek
// for WEEKLY, daysOfMonth for MONTHLY and YEARLY, and months for YEARLY.
// With defaults, a rule that names none runs on SUNDAY, on the 1st, in
// JANUARY, for the forms that do not ask.
func bdrApplyDays(sch map[string]any, rec, days, dom, months string, defaults bool) error {
	dw, err := bdrNames(days, bdrWeekdays, "day of the week (MON to SUN)")
	if err != nil {
		return err
	}
	ms, err := bdrNames(months, bdrMonths, "month (JAN to DEC)")
	if err != nil {
		return err
	}
	var dm []any
	for _, t := range strings.FieldsFunc(dom, func(r rune) bool { return r == ' ' || r == ',' }) {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > 31 {
			return fmt.Errorf("%q is not a day of the month, 1 to 31", t)
		}
		dm = append(dm, n)
	}
	if defaults {
		if len(dw) == 0 {
			dw = []any{"SUNDAY"}
		}
		if len(dm) == 0 {
			dm = []any{1}
		}
		if len(ms) == 0 {
			ms = []any{"JANUARY"}
		}
	}
	delete(sch, "daysOfWeek")
	delete(sch, "daysOfMonth")
	delete(sch, "months")
	switch rec {
	case "WEEKLY":
		if len(dw) == 0 {
			return errors.New("a weekly rule needs its days of the week")
		}
		sch["daysOfWeek"] = dw
	case "MONTHLY", "YEARLY":
		if len(dm) == 0 {
			return fmt.Errorf("a %s rule needs its days of the month", strings.ToLower(rec))
		}
		sch["daysOfMonth"] = dm
		if rec == "YEARLY" {
			if len(ms) == 0 {
				return errors.New("a yearly rule needs its months")
			}
			sch["months"] = ms
		}
	}
	return nil
}

// bdrOnDemandFields are Create on-demand backup's fields: a backup rule's
// retention, or a custom retention up to the plan's maximum custom
// on-demand retention (#1923).
func bdrOnDemandFields(plan map[string]any) []console.Field {
	rules := bdrRuleIDs(plan)
	kinds, def := []string{"rule", "custom"}, "rule"
	if len(rules) == 0 {
		kinds, def = []string{"custom"}, "custom"
	}
	return []console.Field{
		{Name: "retentionType", Label: "Retention", Type: "radio", Options: kinds, Default: def,
			OptionLabels: map[string]string{"rule": "Use a backup rule's retention", "custom": "Custom retention"}},
		{Name: "rule", Label: "Backup rule", Type: "select", Required: true, Options: rules, ShowWhen: "retentionType=rule",
			Help: "The backup is kept for the rule's retention."},
		{Name: "customDays", Label: "Retain backup for (days)", Type: "text", Required: true, Pattern: `[1-9][0-9]{0,4}`,
			ShowWhen: "retentionType=custom", Help: "At most the plan's maximum custom on-demand retention: " + bdrOnDemandText(plan) + "."},
	}
}

// bdrTriggerBody is triggerBackup's request: ruleId, or customRetentionDays.
func bdrTriggerBody(v map[string]string) (map[string]any, error) {
	if v["retentionType"] == "custom" {
		n, err := strconv.Atoi(strings.TrimSpace(v["customDays"]))
		if err != nil || n < 1 {
			return nil, errors.New("the custom retention is a number of days, at least 1")
		}
		return map[string]any{"customRetentionDays": n}, nil
	}
	if v["rule"] == "" {
		return nil, errors.New("choose the backup rule")
	}
	return map[string]any{"ruleId": v["rule"]}, nil
}
