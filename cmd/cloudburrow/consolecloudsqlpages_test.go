package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// allOf is detailAll's page, every action in d.Actions, for the tests that
// read an instance's settings and actions whatever page they are drawn on.
func (p cloudSQLInstancesProvider) allOf(ctx context.Context, project string, path []string) (console.Detail, error) {
	d, _, _, err := p.detailAll(ctx, project, path)
	return d, err
}

// TestCloudSQLConsoleGoogleLayout (#1860): an instance's page has Google's
// menu, drawn at the side; Edit, Import, Export, Restart, Clone and Delete
// beside the title; Create database, Add user account and Create backup on
// their pages, a database's Delete, a user's Change password and Remove and
// a backup's Restore and Delete on its row; the Overview's cards name the
// version as Google does; and DetailActions still offers every action.
func TestCloudSQLConsoleGoogleLayout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/instances/pg"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "pg", "state": "RUNNABLE", "databaseVersion": "POSTGRES_17",
				"connectionName": "proj:us-central1:pg", "region": "us-central1",
				"ipAddresses": []any{map[string]any{"type": "PRIMARY", "ipAddress": "127.0.0.1"}},
				"settings":    map[string]any{"tier": "db-custom-1-3840", "edition": "ENTERPRISE_PLUS", "availabilityType": "ZONAL"}})
		case strings.HasSuffix(r.URL.Path, "/instances/pg/databases"):
			_, _ = io.WriteString(w, `{"items":[{"name":"shop"}]}`)
		case strings.HasSuffix(r.URL.Path, "/instances/pg/users"):
			_, _ = io.WriteString(w, `{"items":[{"name":"postgres"}]}`)
		case strings.HasSuffix(r.URL.Path, "/instances/pg/backupRuns"):
			_, _ = io.WriteString(w, `{"items":[{"id":"7","status":"SUCCESSFUL","type":"ON_DEMAND"}]}`)
		case strings.HasSuffix(r.URL.Path, "/operations"):
			_, _ = io.WriteString(w, `{"items":[{"name":"op1","status":"DONE","operationType":"CREATE"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17", "MYSQL_8_0"}}
	ctx := context.Background()
	d, err := p.Detail(ctx, "proj", []string{"pg"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.SideNav || d.Edit == nil || d.Edit.Label != "Edit" {
		t.Errorf("SideNav %v, Edit %+v", d.SideNav, d.Edit)
	}
	var menu []string
	secs := map[string]console.Section{}
	for _, s := range d.Sections {
		menu = append(menu, s.Label)
		secs[s.ID] = s
	}
	if got := strings.Join(menu, ","); got != "Overview,System insights,Query insights,Connections,Users,Databases,Backups,Replicas,Operations" {
		t.Errorf("menu %s", got)
	}
	labels := func(acts []console.Action) string {
		var out []string
		for _, a := range acts {
			out = append(out, a.Label)
		}
		return strings.Join(out, ",")
	}
	for _, want := range []string{"Restart", "Delete", "Clone", "Export", "Import"} {
		if !strings.Contains(","+labels(d.Actions)+",", ","+want+",") {
			t.Errorf("beside the title %s; want %s", labels(d.Actions), want)
		}
	}
	for id, want := range map[string]string{"databases": "Create database", "users": "Add user account", "backups": "Create backup"} {
		if labels(secs[id].Actions) != want && !strings.HasPrefix(labels(secs[id].Actions), want) {
			t.Errorf("%s page actions %s; want %s", id, labels(secs[id].Actions), want)
		}
	}
	rows := map[string]string{"databases": "Delete", "users": "Change password,Remove", "backups": "Restore,Delete"}
	for id, want := range rows {
		items := secs[id].Listing.Items
		if len(items) != 1 || labels(items[0].Actions) != want || strings.Join(items[0].ActsOn, "/") != "pg" {
			t.Errorf("%s row actions %+v; want %s", id, items, want)
		}
	}
	text := ""
	for _, g := range secs["overview"].Groups {
		for _, pr := range g.Properties {
			text += g.Heading + ": " + pr.Label + "=" + pr.Value + "\n"
		}
	}
	for _, want := range []string{"Configuration: Database version=PostgreSQL 17", "Configuration: Cloud SQL edition=Enterprise Plus",
		"Connect to this instance: Connection name=proj:us-central1:pg", "Connect to this instance: Public IP address=127.0.0.1"} {
		if !strings.Contains(text, want) {
			t.Errorf("Overview lacks %q:\n%s", want, text)
		}
	}
	all := map[string]bool{}
	for _, a := range p.DetailActions(ctx, "proj", []string{"pg"}) {
		all[a.ID] = true
	}
	for _, id := range []string{"restart", "delete-instance", "drop-database", "remove-user", "restore-backup", "create-replica", "export"} {
		if !all[id] {
			t.Errorf("DetailActions lacks %s", id)
		}
	}
}

// TestCloudSQLConsoleCreatePage (#1860): Google's create page, the engine
// first with SQL Server shown and not choosable, each engine's versions
// named as Google names them, point-in-time recovery on by default; a
// MySQL create sends the MySQL version, the preset's machine type and the
// edition, and SQL Server is refused.
func TestCloudSQLConsoleCreatePage(t *testing.T) {
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"op","status":"DONE"}`)
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_16", "POSTGRES_17", "MYSQL_8_0", "MYSQL_8_4"}}
	label, fields := p.CreateForm()
	if label != "Create Instance" || p.CreateTitle() != "Create an instance" {
		t.Errorf("label %q, title %q", label, p.CreateTitle())
	}
	by := map[string]console.Field{}
	for _, f := range fields {
		by[f.Name] = f
	}
	if f := fields[0]; f.Name != "engine" || f.Section != "Choose your database engine" || strings.Join(f.DisabledOptions, ",") != "SQL Server" {
		t.Errorf("first field %+v", f)
	}
	if f := by["databaseVersion"]; strings.Join(orderedLabels(f), ",") != "PostgreSQL 16,PostgreSQL 17" || f.Default != "POSTGRES_17" || f.VisibleWhen["engine"] != "PostgreSQL" {
		t.Errorf("PostgreSQL versions %+v", f)
	}
	if f := by["mysqlVersion"]; strings.Join(orderedLabels(f), ",") != "MySQL 8.0,MySQL 8.4" || f.VisibleWhen["engine"] != "MySQL" {
		t.Errorf("MySQL versions %+v", f)
	}
	if by["pitr"].Default != "true" || by["availabilityType"].OptionLabels["ZONAL"] != "Single zone" {
		t.Errorf("pitr %+v, availability %+v", by["pitr"], by["availabilityType"])
	}
	ctx := context.Background()
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "MySQL", "mysqlVersion": "MYSQL_8_4", "databaseVersion": "POSTGRES_17",
		"name": "my", "edition": "ENTERPRISE", "preset": "Sandbox", "pitr": "true", "backupsEnabled": "true", "maintenanceDay": "7", "maintenanceHour": "3"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"databaseVersion":"MYSQL_8_4"`, `"tier":"db-f1-micro"`, `"edition":"ENTERPRISE"`, `"binaryLogEnabled":true`,
		`"maintenanceWindow":{"day":7,"hour":3}`} {
		if !strings.Contains(body, want) {
			t.Errorf("instances.insert body %s lacks %s", body, want)
		}
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "SQL Server", "name": "ss"}); err == nil || !strings.Contains(err.Error(), "SQL Server version") {
		t.Errorf("SQL Server create with no version = %v", err)
	}
}

// TestCloudSQLConsoleSQLServer (#1916): with the Admin API served, SQL
// Server is choosable, its versions name the local edition, and the
// licence checkbox links Microsoft's terms; a create sends the acceptance
// header only when the checkbox is checked.
func TestCloudSQLConsoleSQLServer(t *testing.T) {
	var header, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, header = string(b), r.Header.Get(sqladmin.EULAHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"op","status":"DONE"}`)
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"),
		versions: append([]string{"POSTGRES_17"}, sqladmin.SQLServerVersions()...)}
	_, fields := p.CreateForm()
	by := map[string]console.Field{}
	for _, f := range fields {
		by[f.Name] = f
	}
	if f := by["engine"]; len(f.DisabledOptions) != 0 || f.OptionLabels["SQL Server"] != "SQL Server" {
		t.Errorf("engine %+v", f)
	}
	v := by["sqlserverVersion"]
	if len(v.Options) != 15 || v.Default != "SQLSERVER_2022_STANDARD" || v.VisibleWhen["engine"] != "SQL Server" ||
		v.OptionLabels["SQLSERVER_2022_STANDARD"] != "SQL Server 2022 Standard (runs Developer locally)" ||
		v.OptionLabels["SQLSERVER_2019_EXPRESS"] != "SQL Server 2019 Express" ||
		v.OptionLabels["SQLSERVER_2025_ENTERPRISE"] != "SQL Server 2025 Enterprise (runs Enterprise Developer locally)" {
		t.Errorf("SQL Server versions %+v", v)
	}
	eula := by["mssqlEula"]
	if eula.Type != "checkbox" || eula.Default == "true" || len(eula.Links) != 2 || eula.Links[0].URL != sqladmin.MSSQLEULAURL {
		t.Errorf("licence checkbox %+v", eula)
	}
	ctx := context.Background()
	in := map[string]string{"engine": "SQL Server", "sqlserverVersion": "SQLSERVER_2019_WEB", "name": "ss", "rootPassword": "Str0ng!pw",
		"pitr": "true", "backupsEnabled": "true"}
	if _, err := p.Create(ctx, "proj", in); err != nil {
		t.Fatal(err)
	}
	if header != "" || !strings.Contains(body, `"databaseVersion":"SQLSERVER_2019_WEB"`) || strings.Contains(body, "backupConfiguration") {
		t.Errorf("unchecked: header %q, body %s", header, body)
	}
	in["mssqlEula"] = "true"
	if _, err := p.Create(ctx, "proj", in); err != nil {
		t.Fatal(err)
	}
	if header != "Y" {
		t.Errorf("checked: header %q", header)
	}
}

// orderedLabels is a field's option labels in the order of its options.
func orderedLabels(f console.Field) []string {
	out := make([]string, 0, len(f.Options))
	for _, o := range f.Options {
		out = append(out, f.OptionLabels[o])
	}
	return out
}
