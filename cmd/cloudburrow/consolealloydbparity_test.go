package main

import (
	"context"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// TestConsoleAlloyDBParity2251 holds the 2026-11-25 round's AlloyDB pages
// to Google's: the Clusters list's columns, Data protection's two Edits,
// read back from the API, and Upgrade's two steps, the cluster ID typed
// back, with the Overview's Upgrade status after it and Cancel upgrade
// greyed out.
func TestConsoleAlloyDBParity2251(t *testing.T) {
	ctx := context.Background()
	api, err := alloydb.NewServer("", "", alloydb.Options{ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	p := alloyDBProvider{svc: &alloyDBService{api: api, engine: newAlloyDBEngine(config.Config{}, nil)}}
	// The primary needs an engine: the cluster is made first.
	_, _ = p.Create(ctx, "p1", map[string]string{"clusterId": "c1", "password": "pw", "version": "POSTGRES_15", "region": "us-central1",
		"network": "default", "instanceId": "primary", "cpu": "2"})
	name := "projects/p1/locations/us-central1/clusters/c1"
	if _, err := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); err != nil {
		t.Fatal(err)
	}

	l, err := p.List(ctx, "p1")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if got := strings.Join(l.Columns, "|"); got != "Resource name|Resource type|Location|Status|Private IP address|High availability|"+
		"Read pool nodes|Automated backups|Most recent backup|Labels|Created|Encryption" {
		t.Errorf("columns = %s", got)
	}
	row := l.Items[0].Fields
	if row["Resource type"] != "Primary cluster" || row["Location"] != "us-central1" || row["Encryption"] != "Google-managed" ||
		row["Automated backups"] != "Disabled" || row["Most recent backup"] != "-" {
		t.Errorf("the cluster's row = %v", row)
	}

	section := func(id string) console.Section {
		t.Helper()
		d, err := p.Detail(ctx, "p1", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no %s tab", id)
		return console.Section{}
	}
	prop := func(s console.Section, heading, label string) console.Property {
		t.Helper()
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				if g.Heading == heading && pr.Label == label {
					return pr
				}
			}
		}
		t.Fatalf("%s has no %s / %s: %+v", s.Label, heading, label, s.Groups)
		return console.Property{}
	}
	action := func(id string) console.Action {
		t.Helper()
		for _, a := range p.DetailActions(ctx, "p1", []string{name}) {
			if a.ID == id {
				return a
			}
		}
		t.Fatalf("no %s action", id)
		return console.Action{}
	}

	// Data protection, as Google's: its policies each with Edit, ending in
	// Update, over the backups, Create backup at its top.
	dp := section("backups")
	if dp.Label != "Data protection" || dp.Kind != console.KindProperties || len(dp.Actions) != 2 || dp.Actions[0].Label != "Create backup" {
		t.Errorf("Data protection = %+v", dp)
	}
	if pr := prop(dp, "Continuous backups and recovery policy", "Continuous backup and recovery"); pr.Action != "edit-continuous-backup" || pr.ActionText != "Edit" {
		t.Errorf("continuous backup's Edit = %+v", pr)
	}
	if pr := prop(dp, "Continuous backups and recovery policy", "Recovery window (days)"); pr.Value != "14" {
		t.Errorf("the default recovery window = %+v", pr)
	}
	if a := action("edit-continuous-backup"); a.Submit != "Update" || !a.Linked || a.Fields[0].Label != "Recovery window (days)" || a.Fields[0].Default != "14" {
		t.Errorf("Edit continuous backups = %+v", a)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-continuous-backup", map[string]string{"window": "36", "encryption": "GOOGLE"}); err == nil {
		t.Error("a 36-day recovery window was taken")
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-continuous-backup", map[string]string{"window": "21", "encryption": "GOOGLE"}); err != nil {
		t.Fatal(err)
	}
	c, _ := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	if c.GetContinuousBackupConfig().GetRecoveryWindowDays() != 21 || !c.GetContinuousBackupConfig().GetEnabled() {
		t.Errorf("continuous backup reads back as %v", c.GetContinuousBackupConfig())
	}
	if a := action("edit-automated-backup"); a.Submit != "Update" || a.Fields[0].Label != "Automate backups" || a.Fields[0].Default != "false" {
		t.Errorf("Edit automated backup policy = %+v", a)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-automated-backup", map[string]string{"automate": "true"}); err != nil {
		t.Fatal(err)
	}
	c, _ = api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	pol := c.GetAutomatedBackupPolicy()
	if !pol.GetEnabled() || len(pol.GetWeeklySchedule().GetDaysOfWeek()) != 7 || pol.GetTimeBasedRetention().GetRetentionPeriod().AsDuration() != 14*24*time.Hour {
		t.Errorf("automated backups read back as %v", pol)
	}
	dp = section("backups")
	if v := prop(dp, "Automated backup policy", "Automated backups").Value; v != "Enabled" {
		t.Errorf("Automated backups = %q", v)
	}
	if v := prop(dp, "Automated backup policy", "Schedule").Value; v != "every day at 23:00 UTC" {
		t.Errorf("Schedule = %q", v)
	}
	if v := prop(dp, "Automated backup policy", "Retention").Value; v != "14 days" {
		t.Errorf("Retention = %q", v)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-automated-backup", map[string]string{"automate": "false"}); err != nil {
		t.Fatal(err)
	}
	c, _ = api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	if c.GetAutomatedBackupPolicy().GetEnabled() || c.GetAutomatedBackupPolicy().GetTimeBasedRetention() == nil {
		t.Errorf("turned off, the policy reads back as %v", c.GetAutomatedBackupPolicy())
	}
	if l, _ := p.List(ctx, "p1"); l.Items[0].Fields["Automated backups"] != "Disabled" {
		t.Errorf("the list's Automated backups = %q", l.Items[0].Fields["Automated backups"])
	}

	// Upgrade: Upgrade beside the version; two steps, only newer
	// versions, the cluster's ID typed back.
	if pr := prop(section("overview"), "Cluster", "Database version"); pr.Action != "upgrade" || pr.ActionText != "Upgrade" {
		t.Errorf("Database version = %+v", pr)
	}
	up := action("upgrade")
	if up.Steps != "continue" || up.Submit != "Start upgrade" || up.Unavailable != "" || len(up.Fields) != 2 ||
		up.Fields[0].Section != "Choose database version" || up.Fields[1].Section != "Upgrade cluster" || up.Fields[1].Label != "Cluster ID" {
		t.Fatalf("Upgrade = %+v", up)
	}
	if got := strings.Join(up.Fields[0].Options, ","); got != "POSTGRES_16,POSTGRES_17" {
		t.Errorf("Upgrade's versions = %s", got)
	}
	if a := action("cancel-upgrade"); a.Unavailable == "" {
		t.Errorf("Cancel upgrade with none running = %+v", a)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "upgrade", map[string]string{"version": "POSTGRES_16", "confirm": "c2"}); err == nil {
		t.Error("an upgrade confirmed with another cluster's ID was started")
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "upgrade", map[string]string{"version": "POSTGRES_16", "confirm": "c1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ov := section("overview")
		if prop(ov, "Upgrade status", "Upgrade status").Value == "Succeeded" {
			if d := prop(ov, "Upgrade status", "Details").Value; !strings.Contains(d, "POSTGRES_15") || !strings.Contains(d, "POSTGRES_16") {
				t.Errorf("Upgrade status details = %q", d)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the upgrade did not finish: %+v", ov.Groups)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c, _ := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); c.GetDatabaseVersion() != alloydbpb.DatabaseVersion_POSTGRES_16 {
		t.Errorf("the cluster reads back as %v", c.GetDatabaseVersion())
	}
	if got := strings.Join(action("upgrade").Fields[0].Options, ","); got != "POSTGRES_17" {
		t.Errorf("after the upgrade, Upgrade's versions = %s", got)
	}
}

// The Users rows' menu is Google's: Change password (Password or No
// password, ending in OK; greyed out for a Cloud IAM user) and Delete.
func TestConsoleAlloyDBUserRowActions2251(t *testing.T) {
	builtIn := alloyDBUserRowActions(&alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_BUILT_IN})
	if len(builtIn) != 2 || builtIn[0].Label != "Change password" || builtIn[0].Submit != "OK" || builtIn[0].Unavailable != "" ||
		builtIn[0].Fields[0].Label != "Password" || builtIn[0].Fields[1].Label != "No password" || builtIn[1].Label != "Delete" || !builtIn[1].Destructive {
		t.Errorf("a built-in user's menu = %+v", builtIn)
	}
	if iam := alloyDBUserRowActions(&alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_IAM_USER}); iam[0].Unavailable == "" {
		t.Errorf("a Cloud IAM user's Change password = %+v", iam[0])
	}
}
