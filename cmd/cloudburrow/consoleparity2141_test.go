package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestSQLInsightsConfig (#2141): Google's limits on the Query insights
// options, 256 to 4500 bytes and 0 to 20 plans a minute.
func TestSQLInsightsConfig(t *testing.T) {
	c, err := sqlInsightsConfig(map[string]string{"recordClientAddress": "true", "queryStringLength": "4500", "queryPlansPerMinute": "0"})
	if err != nil || !c.QueryInsightsEnabled || !c.RecordClientAddress || c.RecordApplicationTags || c.QueryStringLength != 4500 ||
		c.QueryPlansPerMinute != 0 || !strings.Contains(strings.Join(c.ForceSendFields, ","), "QueryPlansPerMinute") {
		t.Fatalf("config %+v, %v", c, err)
	}
	for _, bad := range []map[string]string{{"queryStringLength": "255"}, {"queryStringLength": "4501"}, {"queryStringLength": "x"},
		{"queryPlansPerMinute": "21"}, {"queryPlansPerMinute": "-1"}} {
		if _, err := sqlInsightsConfig(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if f := sqlInsightsFields("Query insights"); len(f) != 4 || f[2].Default != "1024" || f[3].Default != "5" || f[0].VisibleWhen["queryInsightsEnabled"] != "true" {
		t.Errorf("fields %+v", f)
	}
}

// TestSQLInsightsSections (#2141): the pages without a server, with Query
// insights off, and MySQL's columns, which have no User.
func TestSQLInsightsSections(t *testing.T) {
	ctx := context.Background()
	pg := &sqladminapi.DatabaseInstance{Name: "made", DatabaseVersion: "POSTGRES_17", State: "RUNNABLE", Settings: &sqladminapi.Settings{}}
	q := sqlQueryInsightsSection(ctx, nil, "p", pg)
	if q.Listing.Prompt == "" || len(q.Actions) != 1 || q.Actions[0].ID != "enable-query-insights" || q.Actions[0].Submit != "Enable" {
		t.Errorf("Query insights off: %+v", q)
	}
	if q.Listing.TableTitle != "Top queries" || strings.Join(q.Listing.Columns, ",") != "Database,User,Load by total time,Avg execution time (ms),Times called,Avg rows returned" {
		t.Errorf("columns %v", q.Listing.Columns)
	}
	pg.Settings.InsightsConfig = &sqladminapi.InsightsConfig{QueryInsightsEnabled: true}
	if q := sqlQueryInsightsSection(ctx, nil, "p", pg); q.Listing.Unavailable == "" || len(q.Actions) != 0 {
		t.Errorf("no server: %+v", q)
	}
	my := &sqladminapi.DatabaseInstance{Name: "made", DatabaseVersion: "MYSQL_8_4", State: "STOPPED",
		Settings: &sqladminapi.Settings{InsightsConfig: &sqladminapi.InsightsConfig{QueryInsightsEnabled: true}}}
	if q := sqlQueryInsightsSection(ctx, nil, "p", my); strings.Contains(strings.Join(q.Listing.Columns, ","), "User") {
		t.Errorf("MySQL columns %v", q.Listing.Columns)
	}
	if s := sqlSystemInsightsSection(ctx, nil, "p", my); s.Unavailable == "" {
		t.Errorf("System insights with no server: %+v", s)
	}
	ext := &sqladminapi.DatabaseInstance{Name: "ext", DatabaseVersion: "MYSQL_8_0", InstanceType: "ON_PREMISES_INSTANCE"}
	if a := enableQueryInsights(ext); a != nil {
		t.Errorf("an external server offers Enable: %+v", a)
	}
}

// TestSQLRestoreDestinations (#2141): Restore's three destinations, the
// existing one disabled with no other instance.
func TestSQLRestoreDestinations(t *testing.T) {
	f := restoreFields([]string{"1"}, nil)
	if len(f) != 4 || f[1].Name != "restoreTo" || f[1].Default != "source" || strings.Join(f[1].DisabledOptions, ",") != "existing" ||
		f[1].OptionLabels["new"] != "Restore to a new instance" || f[2].VisibleWhen["restoreTo"] != "new" {
		t.Errorf("fields %+v", f)
	}
	if f := restoreFields([]string{"1"}, []string{"other"}); len(f[1].DisabledOptions) != 0 || f[3].Options[0] != "other" {
		t.Errorf("with another instance %+v", f)
	}
}

// TestSQLCreateMySQLRootPassword (#2141): Google's create page asks for
// root's password unless No password is checked.
func TestSQLCreateMySQLRootPassword(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"op","status":"DONE"}`))
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"MYSQL_8_4"}}
	v := map[string]string{"engine": "MySQL", "mysqlVersion": "MYSQL_8_4", "databaseVersion": "MYSQL_8_4", "name": "my",
		"edition": "ENTERPRISE", "preset": "Sandbox"}
	if _, err := p.Create(context.Background(), "proj", v); err == nil || !strings.Contains(err.Error(), "No password") || calls != 0 {
		t.Errorf("no root password: %v (calls %d)", err, calls)
	}
}

// TestSecretFormRows (#2142): the Add label, Add annotation and Add alias
// rows become the key=value maps the older fields carried, and the
// destruction delay in days becomes versionDestroyTtl.
func TestSecretFormRows(t *testing.T) {
	out := secretFormValues(map[string]string{
		"labels":           `[{"key":"env","value":"dev"},{"key":"","value":"ignored"}]`,
		"versionAliases":   `[{"key":"current","value":"2"}]`,
		"destroyDelayDays": "3",
	})
	if m, err := console.ParseMap(out["labels"]); err != nil || len(m) != 1 || m["env"] != "dev" {
		t.Errorf("labels %q, %v", out["labels"], err)
	}
	if m, _ := console.ParseMap(out["versionAliases"]); m["current"] != "2" {
		t.Errorf("aliases %q", out["versionAliases"])
	}
	if out["versionDestroyTtl"] != "72h0m0s" {
		t.Errorf("delay %q", out["versionDestroyTtl"])
	}
	if out := secretFormValues(map[string]string{"destroyDelayDays": "1001"}); !strings.Contains(out["versionDestroyTtl"], "1 to 1000") {
		t.Errorf("1001 days became %q", out["versionDestroyTtl"])
	}
	if out := secretFormValues(map[string]string{"destroyDelayDays": "5", "setDestroyDelay": "false"}); out["versionDestroyTtl"] != "" {
		t.Errorf("an unchecked delay kept %q", out["versionDestroyTtl"])
	}
}

// TestParameterLocationAndLabels (#2143): the Location section and the
// Add label rows.
func TestParameterLocationAndLabels(t *testing.T) {
	for v, want := range map[*map[string]string]string{
		{"locationType": "multi", "region": "us-east1"}:  "global",
		{"locationType": "region", "region": "us-east1"}: "us-east1",
		{"location": "europe-west1"}:                     "europe-west1",
	} {
		if got := pmLocation(*v); got != want {
			t.Errorf("pmLocation(%v) = %q, want %q", *v, got, want)
		}
	}
	m, err := pmLabels(`[{"key":"team","value":"burrow"}]`)
	if err != nil || len(m) != 1 || m["team"] != "burrow" {
		t.Errorf("rows: %v, %v", m, err)
	}
	if m, err := pmLabels(`{"a":"b"}`); err != nil || m["a"] != "b" {
		t.Errorf("map: %v, %v", m, err)
	}
	f := pmLocationFields()
	if f[0].OptionLabels["multi"] != "Multi-region" || f[1].OptionLabels["global"] != "global (Global)" || pmHas(f[2].Options, "global") {
		t.Errorf("location fields %+v", f)
	}
	acts := pmRowActions(parametersRoute, "projects/p/locations/global/parameters/app")
	if acts[0].Label != "Add new version" || !strings.HasSuffix(acts[0].Href, "?action=newversion") {
		t.Errorf("row actions %+v", acts)
	}
}

// The Backups page's Edit, next to Settings, is a page ending in Save:
// Automate backups and its window, then Advanced options' location and
// Number of backups, which cannot be less than seven (#2141).
func TestCloudSQLBackupSettingsPage(t *testing.T) {
	a := backupSettingsAction(true, "03:00", "", "europe-west1")
	if !a.Page || a.Submit != "Save" || a.Fields[0].Name != "backupsEnabled" || a.Fields[1].Default != "03:00" {
		t.Fatalf("the page: %+v", a)
	}
	byName := map[string]string{}
	for _, f := range a.Fields {
		byName[f.Name] = f.Default
	}
	if byName["locationType"] != "region" || byName["region"] != "europe-west1" || byName["retainedBackups"] != "7" {
		t.Errorf("the defaults: %v", byName)
	}
	bc, err := backupSettingsOf(map[string]string{"backupsEnabled": "true", "backupStartTime": "05:00", "locationType": "multi",
		"multiRegion": "eu", "retainedBackups": "30"})
	if err != nil || !bc.Enabled || bc.StartTime != "05:00" || bc.Location != "eu" || bc.BackupRetentionSettings.RetainedBackups != 30 {
		t.Fatalf("saved: %+v, %v", bc, err)
	}
	if _, err := backupSettingsOf(map[string]string{"backupsEnabled": "true", "locationType": "multi", "retainedBackups": "3"}); err == nil {
		t.Error("3 backups were accepted")
	}
	if bc, err := backupSettingsOf(map[string]string{"backupsEnabled": "false", "locationType": "region", "region": "us-east1"}); err != nil ||
		bc.Enabled || bc.Location != "us-east1" {
		t.Errorf("disabled: %+v, %v", bc, err)
	}
}
