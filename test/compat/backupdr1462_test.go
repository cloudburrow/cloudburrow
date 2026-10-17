//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestBackupDRUpdatesAndViews (#1462): through the official backupdr/v1
// client, on a vault, two plans and the association of a created Cloud SQL
// instance with one backup:
//   - patches: a vault's description and labels; a plan's description and
//     rules, which makes revision v1 (revisions.list and get show v0's
//     snapshot too); a data source's labels; a backup's expiry and enforced
//     retention, which can be extended and not reduced; an association
//     moved to a plan in another vault, which gives the instance a data
//     source there;
//   - views: fetchUsable, testIamPermissions, the fetchForResourceType
//     methods and data source references, each listing the Cloud SQL
//     resources and nothing for another type;
//   - operations.cancel and operations.delete.
//
// covers: backupdr.projects.locations.backupVaults.patch, backupdr.projects.locations.backupPlans.patch, backupdr.projects.locations.backupPlans.revisions.list, backupdr.projects.locations.backupPlans.revisions.get, backupdr.projects.locations.backupPlanAssociations.patch, backupdr.projects.locations.backupVaults.dataSources.patch, backupdr.projects.locations.backupVaults.dataSources.backups.patch
// covers: backupdr.projects.locations.backupVaults.fetchUsable, backupdr.projects.locations.backupVaults.testIamPermissions, backupdr.projects.locations.backupPlanAssociations.fetchForResourceType, backupdr.projects.locations.backupVaults.dataSources.backups.fetchForResourceType, backupdr.projects.locations.dataSourceReferences.list, backupdr.projects.locations.dataSourceReferences.get, backupdr.projects.locations.dataSourceReferences.fetchForResourceType
// covers: backupdr.projects.locations.operations.cancel, backupdr.projects.locations.operations.delete
func TestBackupDRUpdatesAndViews(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	version := "POSTGRES_17"
	switch {
	case os.Getenv(EnvCloudSQL) != "":
	case os.Getenv(EnvMySQL) != "":
		version = "MYSQL_8_4"
	default:
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	svc := sqlAdminService(t, h)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	const sqlType = "sqladmin.googleapis.com/Instance"

	vault := func(id string) string {
		wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{
			BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId(id).Do())
		name := loc + "/backupVaults/" + id
		t.Cleanup(func() {
			if op, err := bdr.Projects.Locations.BackupVaults.Delete(name).Force(true).Do(); err == nil {
				waitBackupDROp(t, bdr, op, nil)
			}
		})
		return name
	}
	plan := func(id, vaultName string) string {
		wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
			ResourceType: sqlType, BackupRules: dailyRule()}).BackupPlanId(id).Do())
		name := loc + "/backupPlans/" + id
		t.Cleanup(func() {
			if op, err := bdr.Projects.Locations.BackupPlans.Delete(name).Do(); err == nil {
				waitBackupDROp(t, bdr, op, nil)
			}
		})
		return name
	}
	vaultA, vaultB := vault("upd-a-"+suffix), vault("upd-b-"+suffix)
	planA, planB := plan("upd-a-"+suffix, vaultA), plan("upd-b-"+suffix, vaultB)

	instance := "bdr-upd-" + suffix
	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: instance, DatabaseVersion: version,
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	assocName := loc + "/backupPlanAssociations/upd-" + suffix
	wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{
		BackupPlan: planA, Resource: "projects/" + project + "/instances/" + instance, ResourceType: sqlType}).
		BackupPlanAssociationId("upd-" + suffix).Do())
	t.Cleanup(func() {
		if op, err := bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do(); err == nil {
			waitBackupDROp(t, bdr, op, nil)
		}
	})
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
	assoc, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil {
		t.Fatal(err)
	}
	dsA := assoc.DataSource
	backups, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(dsA).Do()
	if err != nil || len(backups.Backups) != 1 {
		t.Fatalf("backups.list: %v %+v", err, backups)
	}
	bk := backups.Backups[0]

	t.Run("views", func(t *testing.T) {
		u, err := bdr.Projects.Locations.BackupVaults.FetchUsable(loc).Do()
		if err != nil || !hasVault(u.BackupVaults, vaultA) || !hasVault(u.BackupVaults, vaultB) {
			t.Errorf("fetchUsable: %v", err)
		}
		perms := []string{"backupdr.backupVaults.get", "backupdr.backupVaults.update"}
		if p, err := bdr.Projects.Locations.BackupVaults.TestIamPermissions(vaultA, &backupdr.TestIamPermissionsRequest{Permissions: perms}).Do(); err != nil || len(p.Permissions) != 2 {
			t.Errorf("testIamPermissions: %v %+v", err, p)
		}
		fa, err := bdr.Projects.Locations.BackupPlanAssociations.FetchForResourceType(loc).ResourceType(sqlType).Do()
		if err != nil || !hasAssociation(fa.BackupPlanAssociations, assocName) {
			t.Errorf("backupPlanAssociations.fetchForResourceType: %v", err)
		}
		if fa, err := bdr.Projects.Locations.BackupPlanAssociations.FetchForResourceType(loc).ResourceType("compute.googleapis.com/Instance").Do(); err != nil || len(fa.BackupPlanAssociations) != 0 {
			t.Errorf("backupPlanAssociations.fetchForResourceType for Compute Engine: %v %+v", err, fa)
		}
		if fb, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.FetchForResourceType(dsA).ResourceType(sqlType).Do(); err != nil || len(fb.Backups) != 1 || fb.Backups[0].Name != bk.Name {
			t.Errorf("backups.fetchForResourceType: %v %+v", err, fb)
		}
		refs, err := bdr.Projects.Locations.DataSourceReferences.List(loc).Do()
		var ref *backupdr.DataSourceReference
		for _, r := range refs.DataSourceReferences {
			if r.DataSource == dsA {
				ref = r
			}
		}
		if err != nil || ref == nil {
			t.Fatalf("dataSourceReferences.list: %v, no reference to %s", err, dsA)
		}
		got, err := bdr.Projects.Locations.DataSourceReferences.Get(ref.Name).Do()
		if err != nil || got.DataSourceBackupCount != 1 || got.DataSourceGcpResourceInfo == nil ||
			got.DataSourceGcpResourceInfo.CloudSqlInstanceProperties.Name != "projects/"+project+"/instances/"+instance {
			t.Errorf("dataSourceReferences.get: %v %+v", err, got)
		}
		fr, err := bdr.Projects.Locations.DataSourceReferences.FetchForResourceType(loc).ResourceType(sqlType).Do()
		if err != nil || len(fr.DataSourceReferences) == 0 {
			t.Errorf("dataSourceReferences.fetchForResourceType: %v", err)
		}
	})

	t.Run("patches", func(t *testing.T) {
		op := wait(bdr.Projects.Locations.BackupVaults.Patch(vaultA, &backupdr.BackupVault{Description: "gold",
			Labels: map[string]string{"tier": "gold"}}).UpdateMask("description,labels").Do())
		if op.Response == nil {
			t.Errorf("vault patch: no response")
		}
		if v, err := bdr.Projects.Locations.BackupVaults.Get(vaultA).Do(); err != nil || v.Description != "gold" || v.Labels["tier"] != "gold" {
			t.Errorf("vault after patch: %v %+v", err, v)
		}
		_, err := bdr.Projects.Locations.BackupVaults.Patch(vaultA, &backupdr.BackupVault{Description: "x"}).Do()
		wantBackupDRCode(t, "a patch without updateMask", err, http.StatusBadRequest, "updateMask")

		hourly := []*backupdr.BackupRule{{RuleId: "hourly", BackupRetentionDays: 3, StandardSchedule: &backupdr.StandardSchedule{
			RecurrenceType: "HOURLY", HourlyFrequency: 6, TimeZone: "UTC"}}}
		wait(bdr.Projects.Locations.BackupPlans.Patch(planA, &backupdr.BackupPlan{Description: "every six hours", BackupRules: hourly}).
			UpdateMask("description,backupRules").Do())
		p, err := bdr.Projects.Locations.BackupPlans.Get(planA).Do()
		if err != nil || p.RevisionId != "v1" || p.Description != "every six hours" || len(p.BackupRules) != 1 || p.BackupRules[0].RuleId != "hourly" {
			t.Errorf("plan after patch: %v %+v", err, p)
		}
		revs, err := bdr.Projects.Locations.BackupPlans.Revisions.List(planA).Do()
		if err != nil || len(revs.BackupPlanRevisions) != 2 || revs.BackupPlanRevisions[0].RevisionId != "v1" || revs.BackupPlanRevisions[0].State != "ACTIVE" {
			t.Errorf("revisions.list: %v %+v", err, revs)
		}
		v0, err := bdr.Projects.Locations.BackupPlans.Revisions.Get(planA + "/revisions/v0").Do()
		if err != nil || v0.State != "INACTIVE" || v0.BackupPlanSnapshot == nil || v0.BackupPlanSnapshot.BackupRules[0].RuleId != "daily" {
			t.Errorf("revisions.get v0: %v %+v", err, v0)
		}
		if a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do(); err != nil || a.BackupPlanRevisionId != "v1" {
			t.Errorf("association after the plan's patch: %v %+v", err, a)
		}

		wait(bdr.Projects.Locations.BackupVaults.DataSources.Patch(dsA, &backupdr.DataSource{Labels: map[string]string{"env": "test"}}).
			UpdateMask("labels").Do())
		if d, err := bdr.Projects.Locations.BackupVaults.DataSources.Get(dsA).Do(); err != nil || d.Labels["env"] != "test" {
			t.Errorf("data source after patch: %v %+v", err, d)
		}

		exp, err := time.Parse(time.RFC3339Nano, bk.ExpireTime)
		if err != nil {
			t.Fatal(err)
		}
		wait(bdr.Projects.Locations.BackupVaults.DataSources.Backups.Patch(bk.Name, &backupdr.Backup{
			ExpireTime: exp.Add(48 * time.Hour).Format(time.RFC3339), EnforcedRetentionEndTime: exp.Add(24 * time.Hour).Format(time.RFC3339)}).
			UpdateMask("expireTime,enforcedRetentionEndTime").Do())
		b, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.Get(bk.Name).Do()
		if err != nil || !strings.HasPrefix(b.ExpireTime, exp.Add(48*time.Hour).UTC().Format("2006-01-02T15:04:05")) ||
			!strings.HasPrefix(b.EnforcedRetentionEndTime, exp.Add(24*time.Hour).UTC().Format("2006-01-02T15:04:05")) {
			t.Errorf("backup after patch: %v %+v", err, b)
		}
		_, err = bdr.Projects.Locations.BackupVaults.DataSources.Backups.Patch(bk.Name, &backupdr.Backup{
			EnforcedRetentionEndTime: exp.Format(time.RFC3339)}).UpdateMask("enforcedRetentionEndTime").Do()
		wantBackupDRCode(t, "reducing enforced retention", err, http.StatusBadRequest, "not reduced")

		wait(bdr.Projects.Locations.BackupPlanAssociations.Patch(assocName, &backupdr.BackupPlanAssociation{BackupPlan: planB}).
			UpdateMask("backupPlan").Do())
		a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
		if err != nil || a.BackupPlan != planB || !strings.HasPrefix(a.DataSource, vaultB+"/") {
			t.Errorf("association after patch: %v %+v", err, a)
		}
		if d, err := bdr.Projects.Locations.BackupVaults.DataSources.Get(dsA).Do(); err != nil || d.ConfigState != "PASSIVE" {
			t.Errorf("the first vault's data source after the move: %v %+v", err, d)
		}
	})

	t.Run("operations", func(t *testing.T) {
		ops, err := bdr.Projects.Locations.Operations.List(loc).Do()
		if err != nil || len(ops.Operations) == 0 {
			t.Fatalf("operations.list: %v", err)
		}
		name := ops.Operations[0].Name
		if _, err := bdr.Projects.Locations.Operations.Cancel(name, &backupdr.CancelOperationRequest{}).Do(); err != nil {
			t.Errorf("operations.cancel: %v", err)
		}
		if _, err := bdr.Projects.Locations.Operations.Delete(name).Do(); err != nil {
			t.Errorf("operations.delete: %v", err)
		}
		_, err = bdr.Projects.Locations.Operations.Get(name).Do()
		wantBackupDRCode(t, "a deleted operation", err, http.StatusNotFound, "does not exist")
	})
}

// TestBackupDRRefusals (#1462): what needs Compute Engine, Google-managed
// networking, the appliance or Resource Manager's hierarchy is refused 501,
// naming it.
// covers: backupdr.projects.locations.managementServers.create (unimplemented), backupdr.projects.locations.managementServers.delete (unimplemented), backupdr.projects.locations.managementServers.get (unimplemented), backupdr.projects.locations.managementServers.list (unimplemented), backupdr.projects.locations.managementServers.getIamPolicy (unimplemented), backupdr.projects.locations.managementServers.setIamPolicy (unimplemented), backupdr.projects.locations.managementServers.testIamPermissions (unimplemented)
// covers: backupdr.projects.locations.backupVaults.dataSources.initiateBackup (unimplemented), backupdr.projects.locations.backupVaults.dataSources.finalizeBackup (unimplemented), backupdr.projects.locations.backupVaults.dataSources.abandonBackup (unimplemented), backupdr.projects.locations.backupVaults.dataSources.fetchAccessToken (unimplemented), backupdr.projects.locations.backupVaults.dataSources.setInternalStatus (unimplemented), backupdr.projects.locations.backupVaults.dataSources.remove (unimplemented)
// covers: backupdr.folders.locations.resourceBackupConfigs.fetch (unimplemented), backupdr.organizations.locations.resourceBackupConfigs.fetch (unimplemented), backupdr.projects.locations.serviceConfig.initialize (unimplemented)
func TestBackupDRRefusals(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	bdr := backupDRService(t, h)
	loc := "projects/" + h.Project() + "/locations/us-central1"
	ms := loc + "/managementServers/ms"
	ds := loc + "/backupVaults/v/dataSources/d"
	l := bdr.Projects.Locations
	for _, c := range []struct {
		what, want string
		call       func() error
	}{
		{"managementServers.create", "Compute Engine", func() error {
			_, err := l.ManagementServers.Create(loc, &backupdr.ManagementServer{}).ManagementServerId("ms").Do()
			return err
		}},
		{"managementServers.delete", "VPC peering", func() error { _, err := l.ManagementServers.Delete(ms).Do(); return err }},
		{"managementServers.get", "management server", func() error { _, err := l.ManagementServers.Get(ms).Do(); return err }},
		{"managementServers.list", "management server", func() error { _, err := l.ManagementServers.List(loc).Do(); return err }},
		{"managementServers.getIamPolicy", "management server", func() error { _, err := l.ManagementServers.GetIamPolicy(ms).Do(); return err }},
		{"managementServers.setIamPolicy", "management server", func() error {
			_, err := l.ManagementServers.SetIamPolicy(ms, &backupdr.SetIamPolicyRequest{Policy: &backupdr.Policy{}}).Do()
			return err
		}},
		{"managementServers.testIamPermissions", "management server", func() error {
			_, err := l.ManagementServers.TestIamPermissions(ms, &backupdr.TestIamPermissionsRequest{Permissions: []string{"x"}}).Do()
			return err
		}},
		{"dataSources.initiateBackup", "appliance", func() error {
			_, err := l.BackupVaults.DataSources.InitiateBackup(ds, &backupdr.InitiateBackupRequest{}).Do()
			return err
		}},
		{"dataSources.finalizeBackup", "appliance", func() error {
			_, err := l.BackupVaults.DataSources.FinalizeBackup(ds, &backupdr.FinalizeBackupRequest{}).Do()
			return err
		}},
		{"dataSources.abandonBackup", "appliance", func() error {
			_, err := l.BackupVaults.DataSources.AbandonBackup(ds, &backupdr.AbandonBackupRequest{}).Do()
			return err
		}},
		{"dataSources.fetchAccessToken", "appliance", func() error {
			_, err := l.BackupVaults.DataSources.FetchAccessToken(ds, &backupdr.FetchAccessTokenRequest{}).Do()
			return err
		}},
		{"dataSources.setInternalStatus", "Compute Engine", func() error {
			_, err := l.BackupVaults.DataSources.SetInternalStatus(ds, &backupdr.SetInternalStatusRequest{}).Do()
			return err
		}},
		{"dataSources.remove", "Compute Engine", func() error {
			_, err := l.BackupVaults.DataSources.Remove(ds, &backupdr.RemoveDataSourceRequest{}).Do()
			return err
		}},
		{"folders resourceBackupConfigs.fetch", "folder", func() error {
			_, err := bdr.Folders.Locations.ResourceBackupConfigs.Fetch("folders/1/locations/us-central1").Do()
			return err
		}},
		{"organizations resourceBackupConfigs.fetch", "organization", func() error {
			_, err := bdr.Organizations.Locations.ResourceBackupConfigs.Fetch("organizations/1/locations/us-central1").Do()
			return err
		}},
		{"serviceConfig.initialize", "default backup vault", func() error {
			_, err := l.ServiceConfig.Initialize(loc+"/serviceConfig", &backupdr.InitializeServiceRequest{ResourceType: "sqladmin.googleapis.com/Instance"}).Do()
			return err
		}},
	} {
		wantBackupDRCode(t, c.what, c.call(), http.StatusNotImplemented, c.want)
	}
}

func hasAssociation(as []*backupdr.BackupPlanAssociation, name string) bool {
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}
