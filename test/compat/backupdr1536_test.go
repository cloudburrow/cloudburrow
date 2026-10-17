//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
)

// TestBackupDRFetchTrialCompliance (#1536): through the official backupdr/v1
// client, resourceBackupConfigs.fetch answers the project's Cloud SQL
// instance of the region, filtered and ordered as gcloud sends it (and
// list takes the same filter), with an association's config typed
// BACKUPDR_BACKUP_PLAN; a filter key it does not know is 400. A trial in
// "us" is ELIGIBLE, subscribed for 30 days, refused a second subscription,
// ended with its reason; a regional trial is 400. msComplianceMetadata
// answers that the project is no Assured Workloads project.
// covers: backupdr.projects.locations.resourceBackupConfigs.fetch, backupdr.projects.locations.resourceBackupConfigs.list, backupdr.projects.locations.getTrial, backupdr.projects.locations.trial.subscribe, backupdr.projects.locations.trial.end, backupdr.projects.locations.managementServers.msComplianceMetadata
func TestBackupDRFetchTrialCompliance(t *testing.T) {
	h := New(t)
	bdr := backupDRService(t, h)
	instance := backupDRInstance(t)
	wait := backupDRWaiter(t, bdr)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	vaultID, planID, assocID := fmt.Sprintf("fvault-%d", n), fmt.Sprintf("fplan-%d", n), fmt.Sprintf("fassoc-%d", n)
	l := bdr.Projects.Locations

	wait(l.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId(vaultID).Do())
	vaultName := loc + "/backupVaults/" + vaultID
	defer l.BackupVaults.Delete(vaultName).Force(true).Do()
	wait(l.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName, ResourceType: "sqladmin.googleapis.com/Instance", LogRetentionDays: 7,
		BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, StandardSchedule: &backupdr.StandardSchedule{
			RecurrenceType: "DAILY", TimeZone: "UTC", BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 5}}}}}).BackupPlanId(planID).Do())
	planName := loc + "/backupPlans/" + planID
	defer l.BackupPlans.Delete(planName).Do()
	wait(l.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/" + instance, ResourceType: "sqladmin.googleapis.com/Instance"}).BackupPlanAssociationId(assocID).Do())
	defer l.BackupPlanAssociations.Delete(loc + "/backupPlanAssociations/" + assocID).Do()

	t.Run("fetch", func(t *testing.T) {
		byName := fmt.Sprintf("target_resource_display_name=%q AND backup_configs_details.backup_config_source_display_name:%q", instance, assocID)
		got, err := l.ResourceBackupConfigs.Fetch(loc).Filter(byName).OrderBy("target_resource_display_name desc").Do()
		if err != nil || len(got.ResourceBackupConfigs) != 1 {
			t.Fatalf("fetch %s: %v %+v", byName, err, got)
		}
		c := got.ResourceBackupConfigs[0]
		found := false
		for _, d := range c.BackupConfigsDetails {
			found = found || (d.Type == "BACKUPDR_BACKUP_PLAN" && d.BackupVault == vaultName && strings.HasSuffix(d.BackupConfigSource, "/"+assocID))
		}
		if c.TargetResourceType != "CLOUD_SQL_INSTANCE" || !c.Vaulted || !c.BackupConfigured || !found {
			t.Errorf("fetched config: %+v", c)
		}
		if lst, err := l.ResourceBackupConfigs.List(loc).Filter(byName + " AND vaulted=true").Do(); err != nil || len(lst.ResourceBackupConfigs) != 1 {
			t.Errorf("list with the filter: %v %+v", err, lst)
		}
		if got, err := l.ResourceBackupConfigs.Fetch(loc).Filter(byName + " AND vaulted=false").Do(); err != nil || len(got.ResourceBackupConfigs) != 0 {
			t.Errorf("fetch vaulted=false: %v %+v", err, got)
		}
		_, err = l.ResourceBackupConfigs.Fetch(loc).Filter(`create_time="2026"`).Do()
		wantBackupDRCode(t, "fetch with an unknown key", err, http.StatusBadRequest, "unknown key")
		_, err = l.ResourceBackupConfigs.Fetch(loc).OrderBy("create_time").Do()
		wantBackupDRCode(t, "fetch ordered by create_time", err, http.StatusBadRequest, "target_resource_display_name")
	})

	t.Run("trial", func(t *testing.T) {
		us := "projects/" + project + "/locations/us"
		tr, err := l.GetTrial(us + "/trial").Do()
		if err != nil || tr.Name != us+"/trial" {
			t.Fatalf("getTrial: %v %+v", err, tr)
		}
		if tr.State == "ELIGIBLE" {
			sub, err := l.Trial.Subscribe(us, &backupdr.SubscribeTrialRequest{}).Do()
			if err != nil || sub.State != "SUBSCRIBED" || sub.StartTime == "" || sub.EndTime == "" {
				t.Fatalf("trial.subscribe: %v %+v", err, sub)
			}
			start, _ := time.Parse(time.RFC3339Nano, sub.StartTime)
			end, _ := time.Parse(time.RFC3339Nano, sub.EndTime)
			if end.Sub(start) != 30*24*time.Hour {
				t.Errorf("a trial of %v, want 30 days", end.Sub(start))
			}
		}
		_, err = l.Trial.Subscribe(us, &backupdr.SubscribeTrialRequest{}).Do()
		wantBackupDRCode(t, "a second subscription", err, http.StatusBadRequest, "once")
		_, err = l.Trial.End(us, &backupdr.EndTrialRequest{}).Do()
		if tr, _ := l.GetTrial(us + "/trial").Do(); tr != nil && tr.State == "SUBSCRIBED" {
			wantBackupDRCode(t, "trial.end without a reason", err, http.StatusBadRequest, "endReason")
			ended, err := l.Trial.End(us, &backupdr.EndTrialRequest{EndReason: "MOVE_TO_PAID"}).Do()
			if err != nil || ended.State != "UNSUBSCRIBED" || ended.EndReason != "MOVE_TO_PAID" {
				t.Errorf("trial.end: %v %+v", err, ended)
			}
		}
		_, err = l.GetTrial(loc + "/trial").Do()
		wantBackupDRCode(t, "a regional trial", err, http.StatusBadRequest, "us, eu and asia")
	})

	t.Run("msComplianceMetadata", func(t *testing.T) {
		got, err := l.ManagementServers.MsComplianceMetadata(loc+"/managementServers", &backupdr.FetchMsComplianceMetadataRequest{ProjectId: project}).Do()
		if err != nil || got.IsAssuredWorkload {
			t.Errorf("msComplianceMetadata: %v %+v", err, got)
		}
		_, err = l.ManagementServers.MsComplianceMetadata(loc+"/managementServers", &backupdr.FetchMsComplianceMetadataRequest{}).Do()
		wantBackupDRCode(t, "msComplianceMetadata without projectId", err, http.StatusBadRequest, "projectId")
	})
}

// TestGcloudBackupDRResourceBackupConfigFilter (#1536): `gcloud backup-dr
// resource-backup-config list` sends its filter and sort to the server
// (rbc_filter_rewrite), which answers the Cloud SQL instance it names.
// covers: backupdr.projects.locations.resourceBackupConfigs.list
func TestGcloudBackupDRResourceBackupConfigFilter(t *testing.T) {
	h := New(t)
	instance := backupDRInstance(t)
	h.Endpoint(EnvBackupDR)
	g := newGcloudSession(t, h)
	locFlags := []string{"--location", "us-central1", "--project", g.project, "--quiet"}
	list := func(filter string) string {
		t.Helper()
		out, err := g.run(nil, append([]string{"backup-dr", "resource-backup-config", "list", "--filter", filter,
			"--sort-by", "~target_resource_display_name", "--format", "value(targetResourceDisplayName)"}, locFlags...)...)
		if err != nil {
			t.Fatalf("gcloud backup-dr resource-backup-config list --filter %s: %v\n%s", filter, err, out)
		}
		return out
	}
	if out := list(fmt.Sprintf("target_resource_display_name=%s AND target_resource_type=CLOUD_SQL_INSTANCE", instance)); strings.TrimSpace(out) != instance {
		t.Errorf("the instance by name: %q", out)
	}
	if out := list("target_resource_display_name=no-such-instance"); strings.TrimSpace(out) != "" {
		t.Errorf("a name no instance has: %q", out)
	}
}
