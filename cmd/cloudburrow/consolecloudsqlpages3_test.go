package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLConsoleRound3 (#1860): Stop and Start are confirmed as
// Google's are, "Stop database instance?" and "Start database instance?"
// with nothing typed; the password policy's options are in the Set password
// policy dialog under Enable password policies, on the create page and on
// Edit; the create page's Customize groups are Google's, in Google's order,
// with Encryption (Google-managed or CMEK), Connections (Public IP and the
// authorized networks), Security (the server CA) and Query insights each a
// group of its own, and Create sends what they set. Databases and Users are
// Google's dialogs: Create database opens "New database" ending in Create,
// the list has Collation type, Character set and Database type; Add user
// account opens "Add a user account to instance NAME" with Built-in
// authentication or Cloud IAM, a MySQL user's Host name choice, ending in
// Add; Remove asks for nothing typed; Change password ends in OK.
func TestCloudSQLConsoleRound3(t *testing.T) {
	running := &sqladminapi.DatabaseInstance{Name: "pg", State: "RUNNABLE", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladminapi.Settings{ActivationPolicy: "ALWAYS"}}
	stop := stopStartActions(running)
	if len(stop) != 1 || stop[0].ID != "stop" || stop[0].ConfirmTitle != "Stop database instance?" ||
		stop[0].ConfirmStyle != console.DeleteConfirmClick {
		t.Errorf("Stop %+v; want Google's Stop database instance? with nothing typed", stop)
	}
	running.Settings.ActivationPolicy = "NEVER"
	start := stopStartActions(running)
	if len(start) != 1 || start[0].ID != "start" || start[0].ConfirmTitle != "Start database instance?" ||
		start[0].ConfirmStyle != console.DeleteConfirmClick {
		t.Errorf("Start %+v; want Google's Start database instance? with nothing typed", start)
	}

	for _, f := range passwordPolicyFields("", nil, "POSTGRES_17") {
		want := sqlPasswordPolicyPanel
		if f.Name == "enablePasswordPolicy" {
			want = ""
		}
		if f.Panel != want {
			t.Errorf("Edit's %s is in panel %q; want %q", f.Name, f.Panel, want)
		}
	}

	var mu sync.Mutex
	var inserts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances") {
			inserts = append(inserts, string(b))
		}
		_, _ = io.WriteString(w, `{"name":"op","status":"DONE"}`)
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"POSTGRES_17", "MYSQL_8_0"}}

	_, fields := p.CreateForm()
	var sections []string
	by := map[string]console.Field{}
	for _, f := range fields {
		by[f.Name] = f
		if n := len(sections); f.Section != "" && (n == 0 || sections[n-1] != f.Section) {
			sections = append(sections, f.Section)
		}
	}
	want := "Choose your database engine,Choose a Cloud SQL edition,Instance info,Choose region and zonal availability," +
		"Machine configuration,Data cache,Storage,Encryption,Connections,Security,Data protection,Maintenance,Flags," +
		"Query insights,Labels,Source representation (external server)"
	if got := strings.Join(sections, ","); got != want {
		t.Errorf("create page groups\n %s\nwant\n %s", got, want)
	}
	if f := by["kmsKeyName"]; f.Section != "Encryption" || f.VisibleWhen["encryption"] != sqlEncryptionCMEK {
		t.Errorf("CMEK key %+v; want it under Encryption, shown for CMEK", f)
	}
	if f := by["encryption"]; f.Default != sqlEncryptionGoogle || f.OptionLabels[sqlEncryptionGoogle] != "Google-managed encryption key" {
		t.Errorf("Encryption %+v; want Google-managed by default", f)
	}
	for _, n := range []string{"serverCaMode", "serverCaPool", "serverCertificateRotationMode"} {
		if by[n].Section != "Security" {
			t.Errorf("%s is under %q; want Security", n, by[n].Section)
		}
	}
	if by["publicIp"].Section != "Connections" || by["publicIp"].Default != "true" || by["authorizedNetworks"].Section != "Connections" {
		t.Errorf("Connections %+v %+v", by["publicIp"], by["authorizedNetworks"])
	}
	if f := by["queryInsightsEnabled"]; f.Section != "Query insights" || f.Label != "Enable Query insights" {
		t.Errorf("Query insights %+v", f)
	}
	if by["enablePasswordPolicy"].Panel != "" || by["passwordMinLength"].Panel != sqlPasswordPolicyPanel ||
		by["passwordChangeInterval"].Panel != sqlPasswordPolicyPanel {
		t.Errorf("create page's password policy panel: %q %q %q", by["enablePasswordPolicy"].Panel,
			by["passwordMinLength"].Panel, by["passwordChangeInterval"].Panel)
	}

	ctx := context.Background()
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "c3",
		"encryption": sqlEncryptionGoogle, "publicIp": "true", "authorizedNetworks": "203.0.113.0/24, 198.51.100.7",
		"queryInsightsEnabled": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "c4",
		"encryption": sqlEncryptionCMEK}); err == nil || !strings.Contains(err.Error(), "customer-managed encryption key") {
		t.Errorf("CMEK without a key: %v", err)
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "c5",
		"publicIp": "true", "authorizedNetworks": "office"}); err == nil || !strings.Contains(err.Error(), "not a CIDR range") {
		t.Errorf("a bad authorized network: %v", err)
	}
	if _, err := p.Create(ctx, "proj", map[string]string{"engine": "PostgreSQL", "databaseVersion": "POSTGRES_17", "name": "c6",
		"publicIp": "false"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inserts) != 2 {
		t.Fatalf("inserts %q", inserts)
	}
	for _, s := range []string{`"ipv4Enabled":true`, `"value":"203.0.113.0/24"`, `"value":"198.51.100.7"`,
		`"insightsConfig":{"queryInsightsEnabled":true}`} {
		if !strings.Contains(inserts[0], s) {
			t.Errorf("insert %s lacks %s", inserts[0], s)
		}
	}
	if strings.Contains(inserts[0], "diskEncryptionConfiguration") {
		t.Errorf("a Google-managed instance was sent a key: %s", inserts[0])
	}
	if !strings.Contains(inserts[1], `"ipv4Enabled":false`) {
		t.Errorf("Public IP off sent %s", inserts[1])
	}

	acts := map[string]console.Action{}
	for _, a := range databaseUserActions(&sqladminapi.DatabaseInstance{Name: "my", DatabaseVersion: "MYSQL_8_0"}, []string{"shop"}, []string{"root"}) {
		acts[a.ID] = a
	}
	if a := acts["create-database"]; a.Title != "New database" || a.Submit != "Create" {
		t.Errorf("Create database %+v; want New database, ending in Create", a)
	}
	if _, ok := acts["add-iam-user"]; ok {
		t.Error("Cloud IAM is still an action of its own, not Add user account's choice")
	}
	add := acts["add-user"]
	var addFields []string
	for _, f := range add.Fields {
		addFields = append(addFields, f.Name)
	}
	if add.Title != "Add a user account to instance my" || add.Submit != "Add" ||
		strings.Join(addFields, ",") != "authentication,userName,password,userPasswordPolicy,hostChoice,host,principal,type" ||
		add.Fields[3].Unavailable == "" ||
		add.Fields[0].OptionLabels[sqlAuthBuiltIn] != "Built-in authentication" || add.Fields[0].Default != sqlAuthBuiltIn ||
		add.Fields[2].Required {
		t.Errorf("Add user account %+v", add)
	}
	if a := acts["remove-user"]; a.Label != "Remove" || a.ConfirmStyle != console.DeleteConfirmClick {
		t.Errorf("Remove %+v; want Remove with nothing typed", a)
	}
	if a := acts["change-password"]; a.Submit != "OK" || a.Fields[len(a.Fields)-1].Unavailable == "" {
		t.Errorf("Change password ends in %q; want OK", a.Submit)
	}
	if sqlDatabaseType("postgres") != "System" || sqlDatabaseType("shop") != "User" {
		t.Error("database types")
	}
	if a := userActions(true, ""); a[0].Label != "Add user account" || a[0].Submit != "Add" || a[1].Label != "Remove" {
		t.Errorf("the Studio's user actions %+v", a)
	}
	backups := map[string]console.Action{}
	for _, a := range p.backupActions([]string{"7"}) {
		backups[a.ID] = a
	}
	if a := backups["create-backup"]; a.Submit != "Create" || len(a.Fields) != 1 || a.Fields[0].Name != "description" {
		t.Errorf("Create backup %+v; want only Description, ending in Create", a)
	}
	if a := backups["delete-backup"]; a.ConfirmStyle != console.DeleteConfirmWordTitle || a.ConfirmTitle != "Delete backup" {
		t.Errorf("Delete backup %+v; want Google's Delete backup window, the word Delete typed", a)
	}
}
