//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/logadmin"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestBackupDRVaultLocationsAndRetentionLock (#1921), through the official
// backupdr/v1 client, as Google's Create a backup vault page documents
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/backup-vault-create):
//   - a vault in the us multi-region, which locations.list names; a plan in
//     us-central1 stores into it, a plan in europe-west1 cannot;
//   - backupRetentionInheritance MATCH_BACKUP_EXPIRE_TIME is kept, and
//     "Once set, it cannot be changed";
//   - the retention lock (effectiveTime): a past date is refused; while it
//     is pending the retention can be decreased and the lock removed; once
//     it takes effect the retention can only be increased and the lock can
//     be neither removed nor moved.
//
// covers: backupdr.projects.locations.list, backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.patch, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.delete
func TestBackupDRVaultLocationsAndRetentionLock(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	us := "projects/" + project + "/locations/us"
	region := "projects/" + project + "/locations/us-central1"

	locs, err := bdr.Projects.Locations.List("projects/" + project).Do()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, l := range locs.Locations {
		names[l.LocationId] = true
	}
	if !names["us"] || !names["eu"] || !names["asia"] || !names["us-central1"] {
		t.Errorf("locations.list: %v; want the regions and the us, eu and asia multi-regions", names)
	}

	// A multi-region vault, with retention inheritance.
	mrName := us + "/backupVaults/mr-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(us, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s",
		BackupRetentionInheritance: "MATCH_BACKUP_EXPIRE_TIME"}).BackupVaultId("mr-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(mrName).Force(true).IgnoreBackupPlanReferences(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	v, err := bdr.Projects.Locations.BackupVaults.Get(mrName).Do()
	if err != nil || v.BackupRetentionInheritance != "MATCH_BACKUP_EXPIRE_TIME" || v.EffectiveTime != "" {
		t.Fatalf("multi-region vault: %v %+v", err, v)
	}
	_, err = bdr.Projects.Locations.BackupVaults.Patch(mrName, &backupdr.BackupVault{BackupRetentionInheritance: "INHERIT_VAULT_RETENTION"}).
		UpdateMask("backupRetentionInheritance").Do()
	wantBackupDRCode(t, "changing backupRetentionInheritance", err, http.StatusBadRequest, "cannot be changed")

	planName := region + "/backupPlans/mrplan-" + suffix
	wait(bdr.Projects.Locations.BackupPlans.Create(region, &backupdr.BackupPlan{BackupVault: mrName,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("mrplan-" + suffix).Do())
	if op, err := bdr.Projects.Locations.BackupPlans.Delete(planName).Do(); err == nil {
		wait(op, nil)
	}
	eu := "projects/" + project + "/locations/europe-west1"
	_, err = bdr.Projects.Locations.BackupPlans.Create(eu, &backupdr.BackupPlan{BackupVault: mrName,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("euplan-" + suffix).Do()
	wantBackupDRCode(t, "a europe-west1 plan into the us multi-region", err, http.StatusBadRequest, "multi-region")

	// The retention lock.
	_, err = bdr.Projects.Locations.BackupVaults.Create(region, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s",
		EffectiveTime: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}).BackupVaultId("past-" + suffix).Do()
	wantBackupDRCode(t, "a lock date in the past", err, http.StatusBadRequest, "in the future")

	lockName := region + "/backupVaults/lock-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(region, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "259200s",
		EffectiveTime: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}).BackupVaultId("lock-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(lockName).Force(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	// Pending: decrease, and remove the lock.
	wait(bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "172800s"}).
		UpdateMask("backupMinimumEnforcedRetentionDuration").Do())
	op := wait(bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{ForceSendFields: []string{"EffectiveTime"}}).
		UpdateMask("effectiveTime").Do())
	if v, err := bdr.Projects.Locations.BackupVaults.Get(lockName).Do(); err != nil || v.EffectiveTime != "" ||
		v.BackupMinimumEnforcedRetentionDuration != "172800s" {
		t.Fatalf("after removing the pending lock: %v %+v (%s)", err, v, op.Name)
	}
	// Lock in two seconds, and wait for it.
	at := time.Now().Add(2 * time.Second).UTC()
	wait(bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{EffectiveTime: at.Format(time.RFC3339Nano)}).
		UpdateMask("effectiveTime").Do())
	time.Sleep(time.Until(at) + time.Second)
	_, err = bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		UpdateMask("backupMinimumEnforcedRetentionDuration").Do()
	wantBackupDRCode(t, "decreasing a locked retention", err, http.StatusBadRequest, "not reduced")
	_, err = bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{ForceSendFields: []string{"EffectiveTime"}}).
		UpdateMask("effectiveTime").Do()
	wantBackupDRCode(t, "removing a lock in effect", err, http.StatusBadRequest, "cannot be removed")
	_, err = bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{EffectiveTime: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}).
		UpdateMask("effectiveTime").Do()
	wantBackupDRCode(t, "moving a lock in effect", err, http.StatusBadRequest, "cannot be changed")
	wait(bdr.Projects.Locations.BackupVaults.Patch(lockName, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "345600s"}).
		UpdateMask("backupMinimumEnforcedRetentionDuration").Do())
	if v, err := bdr.Projects.Locations.BackupVaults.Get(lockName).Do(); err != nil || v.BackupMinimumEnforcedRetentionDuration != "345600s" ||
		v.EffectiveTime == "" {
		t.Errorf("after increasing a locked retention: %v %+v", err, v)
	}
}

// TestBackupDRPlanRuleChecks (#1923): backupPlans.create and patch check
// what Google documents
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/backup-plan-create
// and the StandardSchedule, BackupRule and BackupPlan field docs):
// hourlyFrequency 6 to 23 for a Cloud SQL instance; daysOfWeek for WEEKLY
// only, and required there; months for YEARLY only; a rule's retention and
// logRetentionDays at least the vault's minimum enforced retention; no rules
// only with maxCustomOnDemandRetentionDays, which is kept. A weekly rule on
// MONDAY and FRIDAY, a monthly rule on the LAST SUNDAY and a yearly rule in
// Europe/Paris are accepted.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.patch, backupdr.projects.locations.backupPlans.delete
func TestBackupDRPlanRuleChecks(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	loc := "projects/" + project + "/locations/us-central1"
	vaultName := loc + "/backupVaults/rvault-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "259200s"}).
		BackupVaultId("rvault-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).IgnoreBackupPlanReferences(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	rule := func(id string, days int64, sch *backupdr.StandardSchedule) *backupdr.BackupRule {
		if sch.TimeZone == "" {
			sch.TimeZone = "UTC"
		}
		sch.BackupWindow = &backupdr.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 7}
		return &backupdr.BackupRule{RuleId: id, BackupRetentionDays: days, StandardSchedule: sch}
	}
	n := 0
	create := func(p *backupdr.BackupPlan) (string, error) {
		n++
		id := fmt.Sprintf("rplan-%s-%d", suffix, n)
		p.BackupVault, p.ResourceType = vaultName, "sqladmin.googleapis.com/Instance"
		_, err := bdr.Projects.Locations.BackupPlans.Create(loc, p).BackupPlanId(id).Do()
		return loc + "/backupPlans/" + id, err
	}
	for _, c := range []struct {
		what  string
		plan  *backupdr.BackupPlan
		wants string
	}{
		{"hourly every 4 hours", &backupdr.BackupPlan{BackupRules: []*backupdr.BackupRule{rule("h", 7, &backupdr.StandardSchedule{
			RecurrenceType: "HOURLY", HourlyFrequency: 4})}}, "6 to 23"},
		{"weekly without days", &backupdr.BackupPlan{BackupRules: []*backupdr.BackupRule{rule("w", 7, &backupdr.StandardSchedule{
			RecurrenceType: "WEEKLY"})}}, "daysOfWeek is required"},
		{"months on a monthly rule", &backupdr.BackupPlan{BackupRules: []*backupdr.BackupRule{rule("m", 7, &backupdr.StandardSchedule{
			RecurrenceType: "MONTHLY", DaysOfMonth: []int64{1}, Months: []string{"MARCH"}})}}, "YEARLY rule only"},
		{"days of week on a daily rule", &backupdr.BackupPlan{BackupRules: []*backupdr.BackupRule{rule("d", 7, &backupdr.StandardSchedule{
			RecurrenceType: "DAILY", DaysOfWeek: []string{"MONDAY"}})}}, "WEEKLY rule only"},
		{"a retention under the vault's minimum", &backupdr.BackupPlan{BackupRules: []*backupdr.BackupRule{rule("d", 2, &backupdr.StandardSchedule{
			RecurrenceType: "DAILY"})}}, "minimum enforced retention"},
		{"a log retention under the vault's minimum", &backupdr.BackupPlan{LogRetentionDays: 2, BackupRules: []*backupdr.BackupRule{
			rule("d", 7, &backupdr.StandardSchedule{RecurrenceType: "DAILY"})}}, "logRetentionDays"},
		{"no rules and no maximum on-demand retention", &backupdr.BackupPlan{}, "maxCustomOnDemandRetentionDays"},
	} {
		_, err := create(c.plan)
		wantBackupDRCode(t, c.what, err, http.StatusBadRequest, c.wants)
	}

	name, err := create(&backupdr.BackupPlan{MaxCustomOnDemandRetentionDays: 40, BackupRules: []*backupdr.BackupRule{
		rule("weekly", 14, &backupdr.StandardSchedule{RecurrenceType: "WEEKLY", DaysOfWeek: []string{"MONDAY", "FRIDAY"}}),
		rule("monthly", 30, &backupdr.StandardSchedule{RecurrenceType: "MONTHLY",
			WeekDayOfMonth: &backupdr.WeekDayOfMonth{WeekOfMonth: "LAST", DayOfWeek: "SUNDAY"}}),
		rule("yearly", 365, &backupdr.StandardSchedule{RecurrenceType: "YEARLY", Months: []string{"MARCH", "SEPTEMBER"},
			DaysOfMonth: []int64{10}, TimeZone: "Europe/Paris"}),
		rule("hourly", 3, &backupdr.StandardSchedule{RecurrenceType: "HOURLY", HourlyFrequency: 6}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlans.Delete(name).Do(); err == nil {
			wait(op, nil)
		}
	}()
	p, err := bdr.Projects.Locations.BackupPlans.Get(name).Do()
	if err != nil || p.MaxCustomOnDemandRetentionDays != 40 || len(p.BackupRules) != 4 ||
		strings.Join(p.BackupRules[0].StandardSchedule.DaysOfWeek, ",") != "MONDAY,FRIDAY" ||
		p.BackupRules[1].StandardSchedule.WeekDayOfMonth == nil || p.BackupRules[2].StandardSchedule.TimeZone != "Europe/Paris" {
		t.Fatalf("plan: %v %+v", err, p)
	}
	// Patch: the maximum is changed; dropping every rule is allowed while
	// it is set.
	wait(bdr.Projects.Locations.BackupPlans.Patch(name, &backupdr.BackupPlan{MaxCustomOnDemandRetentionDays: 60,
		BackupRules: []*backupdr.BackupRule{}, ForceSendFields: []string{"BackupRules"}}).UpdateMask("maxCustomOnDemandRetentionDays,backupRules").Do())
	if p, err := bdr.Projects.Locations.BackupPlans.Get(name).Do(); err != nil || p.MaxCustomOnDemandRetentionDays != 60 || len(p.BackupRules) != 0 {
		t.Errorf("patched plan: %v %+v", err, p)
	}
	_, err = bdr.Projects.Locations.BackupPlans.Patch(name, &backupdr.BackupPlan{MaxCustomOnDemandRetentionDays: 2}).
		UpdateMask("maxCustomOnDemandRetentionDays").Do()
	wantBackupDRCode(t, "a maximum on-demand retention under the vault's minimum", err, http.StatusBadRequest, "minimum enforced retention")
}

// TestBackupDRJobLogsStoredBytesAndOnDemandRetention (#1921, #1923, #1924):
// a Cloud SQL instance is protected by a plan with a maximum custom
// on-demand retention of 5 days, in a vault whose minimum enforced
// retention is 1 day:
//   - triggerBackup with customRetentionDays 6 is refused, with ruleId and
//     customRetentionDays together too ("mutually exclusive"); with 4 it
//     takes an ON_DEMAND backup that expires in 4 days and whose enforced
//     retention ends in 1 day (INHERIT_VAULT_RETENTION);
//   - the backup's resourceSizeBytes, the data source's and the vault's
//     totalStoredBytes are the dump's size, more than 0;
//   - the backup is restored into a new instance;
//   - Cloud Logging holds the jobs under Google's log names: an
//     ON_DEMAND_BACKUP and a RESTORE job in bdr_backup_restore_jobs on
//     backupdr.googleapis.com/BackupDRProject, each RUNNING then SUCCESSFUL
//     with one jobId, and the plan's and association's creation in
//     bdr_backup_plan_jobs
//     (https://docs.cloud.google.com/backup-disaster-recovery/docs/monitor-reports/backupvault-backup-recovery-jobs).
//
// covers: backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.backups.get, backupdr.projects.locations.backupVaults.dataSources.get, backupdr.projects.locations.backupVaults.get, sql.instances.restoreBackup, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestBackupDRJobLogsStoredBytesAndOnDemandRetention(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	instance := ""
	switch {
	case os.Getenv(EnvCloudSQL) != "":
		h.Endpoint(EnvCloudSQL)
		instance = "postgres"
	case os.Getenv(EnvMySQL) != "":
		h.Endpoint(EnvMySQL)
		instance = "mysql"
	default:
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	logAddr := shardEndpoint(h, envSQLLogging, EnvLogging)
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	loc := "projects/" + project + "/locations/us-central1"
	vaultName := loc + "/backupVaults/jvault-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		BackupVaultId("jvault-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).IgnoreBackupPlanReferences(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	planName := loc + "/backupPlans/jplan-" + suffix
	wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName, MaxCustomOnDemandRetentionDays: 5,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("jplan-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlans.Delete(planName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	assocName := loc + "/backupPlanAssociations/jassoc-" + suffix
	wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/" + instance, ResourceType: "sqladmin.googleapis.com/Instance"}).
		BackupPlanAssociationId("jassoc-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	assoc, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil {
		t.Fatal(err)
	}

	_, err = bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{CustomRetentionDays: 6}).Do()
	wantBackupDRCode(t, "a custom retention over the plan's maximum", err, http.StatusBadRequest, "maximum custom on-demand retention")
	_, err = bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily",
		CustomRetentionDays: 3}).Do()
	wantBackupDRCode(t, "ruleId with customRetentionDays", err, http.StatusBadRequest, "mutually exclusive")
	before := time.Now()
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{CustomRetentionDays: 4}).Do())
	backups, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(assoc.DataSource).Do()
	if err != nil || len(backups.Backups) != 1 {
		t.Fatalf("backups.list: %v %+v", err, backups)
	}
	b := backups.Backups[0]
	exp, _ := time.Parse(time.RFC3339Nano, b.ExpireTime)
	eret, _ := time.Parse(time.RFC3339Nano, b.EnforcedRetentionEndTime)
	if b.BackupType != "ON_DEMAND" || b.State != "ACTIVE" || exp.Sub(before) < 4*24*time.Hour-time.Minute || exp.Sub(before) > 4*24*time.Hour+time.Minute ||
		eret.Sub(before) < 24*time.Hour-time.Minute || eret.Sub(before) > 24*time.Hour+time.Minute {
		t.Errorf("on-demand backup: %s %s expires %s, enforced retention ends %s", b.BackupType, b.State, b.ExpireTime, b.EnforcedRetentionEndTime)
	}
	if b.ResourceSizeBytes <= 0 {
		t.Errorf("backup resourceSizeBytes = %d; want the dump's size", b.ResourceSizeBytes)
	}
	ds, err := bdr.Projects.Locations.BackupVaults.DataSources.Get(assoc.DataSource).Do()
	if err != nil || ds.TotalStoredBytes != b.ResourceSizeBytes {
		t.Errorf("data source totalStoredBytes: %v %d; want %d", err, ds.TotalStoredBytes, b.ResourceSizeBytes)
	}
	v, err := bdr.Projects.Locations.BackupVaults.Get(vaultName).Do()
	if err != nil || v.TotalStoredBytes != b.ResourceSizeBytes {
		t.Errorf("vault totalStoredBytes: %v %d; want %d", err, v.TotalStoredBytes, b.ResourceSizeBytes)
	}

	// Restored into a new instance (#1922), so the shard's own instance
	// keeps its rows.
	target := "jrestore-" + suffix
	rop, err := svc.Instances.RestoreBackup(project, target, &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: b.Name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d, err := svc.Instances.Delete(project, target).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}()
	waitSQLOp(t, svc, project, rop)

	// The job logs, read with the official Logging client.
	ctx := h.Context()
	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	read := func(log string) []map[string]any {
		filter := `logName = "projects/` + project + `/logs/backupdr.googleapis.com%2F` + log + `" AND timestamp >= "` +
			before.Add(-time.Minute).UTC().Format(time.RFC3339) + `"`
		var out []map[string]any
		it := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("entries of %s: %v", log, err)
			}
			var p map[string]any
			switch v := e.Payload.(type) {
			case map[string]any:
				p = v
			case *structpb.Struct:
				p = v.AsMap()
			}
			if p == nil {
				t.Fatalf("%s: payload %T", log, e.Payload)
			}
			if log == "bdr_backup_restore_jobs" && (e.Resource == nil || e.Resource.Type != "backupdr.googleapis.com/BackupDRProject") {
				t.Errorf("%s: resource %+v", log, e.Resource)
			}
			out = append(out, p)
		}
	}
	type key struct{ category, status string }
	var jobs map[key]string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		jobs = map[key]string{}
		for _, p := range read("bdr_backup_restore_jobs") {
			if p["sourceResourceName"] != "projects/"+project+"/instances/"+instance || p["dataSourceName"] != assoc.DataSource {
				continue
			}
			c, _ := p["jobCategory"].(string)
			s, _ := p["jobStatus"].(string)
			id, _ := p["jobId"].(string)
			jobs[key{c, s}] = id
		}
		if len(jobs) >= 4 || time.Now().After(deadline) {
			break
		}
	}
	for _, c := range []string{"ON_DEMAND_BACKUP", "RESTORE"} {
		run, done := jobs[key{c, "RUNNING"}], jobs[key{c, "SUCCESSFUL"}]
		if run == "" || run != done {
			t.Errorf("%s job: RUNNING %q, SUCCESSFUL %q; want one jobId logged twice (%v)", c, run, done, jobs)
		}
	}
	cats := map[string]bool{}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		for _, p := range read("bdr_backup_plan_jobs") {
			if p["backupPlanName"] == planName {
				c, _ := p["jobCategory"].(string)
				cats[c] = true
			}
		}
		if (cats["CREATE_BACKUP_PLAN"] && cats["CREATE_BACKUP_PLAN_ASSOCIATION"]) || time.Now().After(deadline) {
			break
		}
	}
	if !cats["CREATE_BACKUP_PLAN"] || !cats["CREATE_BACKUP_PLAN_ASSOCIATION"] {
		t.Errorf("bdr_backup_plan_jobs categories: %v", cats)
	}
}

// TestGcloudBackupDRRetentionLockAndPlanDays (#1921, #1923): `gcloud
// backup-dr` makes a vault in the us multi-region with
// --backup-retention-inheritance match-backup-expire-time and an
// --effective-time a year ahead, unlocks it with
// --unlock-backup-min-enforced-retention (which sends the zero time), and
// makes a plan with --max-custom-on-demand-retention-days and a weekly rule
// with days-of-week and time-zone, through the configuration `cloudburrow
// gcloud-setup` writes with egress to Google blocked.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.patch, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.delete
func TestGcloudBackupDRRetentionLockAndPlanDays(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	h.Endpoint(EnvBackupDR)
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	vault, plan := fmt.Sprintf("glock-%d", n), fmt.Sprintf("gdays-%d", n)
	run := func(location string, args ...string) string {
		t.Helper()
		full := append(append([]string{"backup-dr"}, args...), "--location", location, "--project", g.project, "--quiet")
		out, err := g.run(nil, full...)
		if err != nil {
			t.Fatalf("gcloud backup-dr %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	lock := time.Now().AddDate(1, 0, 0).UTC().Format("2006-01-02")
	run("us", "backup-vaults", "create", vault, "--backup-min-enforced-retention", "1d",
		"--backup-retention-inheritance", "match-backup-expire-time", "--effective-time", lock)
	defer g.run(nil, "backup-dr", "backup-vaults", "delete", vault, "--ignore-backup-plan-references", "--no-async",
		"--location", "us", "--project", g.project, "--quiet")
	out := run("us", "backup-vaults", "describe", vault, "--format", "value(backupRetentionInheritance,effectiveTime)")
	if !strings.Contains(out, "MATCH_BACKUP_EXPIRE_TIME") || !strings.Contains(out, lock) {
		t.Errorf("backup-vaults describe: %s", out)
	}
	run("us", "backup-vaults", "update", vault, "--unlock-backup-min-enforced-retention")
	if out := run("us", "backup-vaults", "describe", vault, "--format", "value(effectiveTime)"); strings.TrimSpace(out) != "" {
		t.Errorf("effectiveTime after --unlock-backup-min-enforced-retention: %q", out)
	}
	run("us-central1", "backup-plans", "create", plan, "--backup-vault", "projects/"+g.project+"/locations/us/backupVaults/"+vault,
		"--resource-type", "sqladmin.googleapis.com/Instance", "--max-custom-on-demand-retention-days", "40",
		"--backup-rule", "rule-id=weekly,retention-days=14,recurrence=WEEKLY,backup-window-start=1,backup-window-end=7,"+
			"days-of-week=MONDAY FRIDAY,time-zone=Europe/Paris")
	defer g.run(nil, "backup-dr", "backup-plans", "delete", plan, "--location", "us-central1", "--project", g.project, "--quiet")
	out = run("us-central1", "backup-plans", "describe", plan, "--format",
		"value(maxCustomOnDemandRetentionDays,backupRules[0].standardSchedule.daysOfWeek.list(),backupRules[0].standardSchedule.timeZone)")
	if !strings.Contains(out, "40") || !strings.Contains(out, "MONDAY,FRIDAY") || !strings.Contains(out, "Europe/Paris") {
		t.Errorf("backup-plans describe: %s", out)
	}
}
