package backupdr

import (
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*Server, string) {
	t.Helper()
	s := NewServer("", &fakeSQL{runs: map[string]Run{}}, "")
	for _, c := range []struct{ path, body string }{
		{loc + "/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`},
		{loc + "/backupVaults?backupVaultId=w", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`},
		{loc + "/backupPlans?backupPlanId=plan", planBody},
		{loc + "/backupPlans?backupPlanId=other", strings.Replace(planBody, "backupVaults/v", "backupVaults/w", 1)},
		{loc + "/backupPlanAssociations?backupPlanAssociationId=a", `{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan",
			"resource":"projects/p/instances/db","resourceType":"sqladmin.googleapis.com/Instance"}`},
		{loc + "/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`},
	} {
		if code, out := call(t, s, "POST", c.path, c.body); code != 200 {
			t.Fatalf("POST %s: %d %v", c.path, code, out)
		}
	}
	_, out := call(t, s, "GET", loc+"/backupPlanAssociations/a", "")
	return s, out["dataSource"].(string)
}

func response(t *testing.T, what string, f func() (int, map[string]any)) map[string]any {
	t.Helper()
	code, out := f()
	if code != 200 || out["done"] != true || out["error"] != nil {
		t.Fatalf("%s: %d %v", what, code, out)
	}
	r, _ := out["response"].(map[string]any)
	return r
}

func TestPatchesAndRevisions(t *testing.T) {
	s, ds := setup(t)
	if code, _ := call(t, s, "PATCH", loc+"/backupVaults/v", `{"description":"x"}`); code != 400 {
		t.Errorf("a patch without updateMask: %d", code)
	}
	r := response(t, "vault patch", func() (int, map[string]any) {
		return call(t, s, "PATCH", loc+"/backupVaults/v?updateMask=description,labels", `{"description":"gold","labels":{"k":"v"}}`)
	})
	if r["description"] != "gold" || r["labels"].(map[string]any)["k"] != "v" {
		t.Errorf("vault patch: %v", r)
	}
	if code, _ := call(t, s, "PATCH", loc+"/backupVaults/v?updateMask=uid", `{}`); code != 400 {
		t.Errorf("patching uid: %d", code)
	}

	r = response(t, "plan patch", func() (int, map[string]any) {
		return call(t, s, "PATCH", loc+"/backupPlans/plan?update_mask=description,backup_rules", `{"description":"new",
		  "backupRules":[{"ruleId":"hourly","backupRetentionDays":3,"standardSchedule":{"recurrenceType":"HOURLY","hourlyFrequency":6,"timeZone":"UTC"}}]}`)
	})
	if r["revisionId"] != "v1" || r["description"] != "new" || len(r["backupRules"].([]any)) != 1 {
		t.Errorf("plan patch: %v", r)
	}
	_, revs := call(t, s, "GET", loc+"/backupPlans/plan/revisions", "")
	list := revs["backupPlanRevisions"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["revisionId"] != "v1" || list[0].(map[string]any)["state"] != "ACTIVE" ||
		list[1].(map[string]any)["state"] != "INACTIVE" {
		t.Fatalf("revisions: %v", revs)
	}
	code, v0 := call(t, s, "GET", loc+"/backupPlans/plan/revisions/v0", "")
	if code != 200 || v0["backupPlanSnapshot"].(map[string]any)["backupRules"].([]any)[0].(map[string]any)["ruleId"] != "daily" {
		t.Errorf("revision v0: %d %v", code, v0)
	}
	if code, _ := call(t, s, "GET", loc+"/backupPlans/plan/revisions/v9", ""); code != 404 {
		t.Errorf("a missing revision: %d", code)
	}
	_, a := call(t, s, "GET", loc+"/backupPlanAssociations/a", "")
	if a["backupPlanRevisionId"] != "v1" {
		t.Errorf("the association's revision: %v", a["backupPlanRevisionId"])
	}

	// The backup taken under v0 keeps it.
	_, bl := call(t, s, "GET", "/v1/"+ds+"/backups", "")
	b := bl["backups"].([]any)[0].(map[string]any)
	if b["gcpBackupPlanInfo"].(map[string]any)["backupPlanRevisionId"] != "v0" {
		t.Errorf("backup revision: %v", b["gcpBackupPlanInfo"])
	}
	exp, _ := time.Parse(time.RFC3339Nano, b["expireTime"].(string))
	later := exp.Add(48 * time.Hour).Format(time.RFC3339)
	r = response(t, "backup patch", func() (int, map[string]any) {
		return call(t, s, "PATCH", "/v1/"+b["name"].(string)+"?updateMask=expireTime,enforcedRetentionEndTime",
			`{"expireTime":"`+later+`","enforcedRetentionEndTime":"`+exp.Add(24*time.Hour).Format(time.RFC3339)+`"}`)
	})
	if !strings.HasPrefix(r["expireTime"].(string), later[:19]) || r["enforcedRetentionEndTime"] == r["expireTime"] {
		t.Errorf("backup patch: %v %v", r["expireTime"], r["enforcedRetentionEndTime"])
	}
	if code, out := call(t, s, "PATCH", "/v1/"+b["name"].(string)+"?updateMask=enforcedRetentionEndTime",
		`{"enforcedRetentionEndTime":"`+exp.Format(time.RFC3339)+`"}`); code != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "not reduced") {
		t.Errorf("shortening enforced retention: %d %v", code, out)
	}

	r = response(t, "data source patch", func() (int, map[string]any) {
		return call(t, s, "PATCH", "/v1/"+ds+"?updateMask=labels", `{"labels":{"env":"prod"}}`)
	})
	if r["labels"].(map[string]any)["env"] != "prod" {
		t.Errorf("data source patch: %v", r)
	}

	// Moving the association to a plan in another vault gives the
	// instance a data source there; the old one goes passive.
	r = response(t, "association patch", func() (int, map[string]any) {
		return call(t, s, "PATCH", loc+"/backupPlanAssociations/a?updateMask=backupPlan", `{"backupPlan":"projects/p/locations/us-central1/backupPlans/other"}`)
	})
	if r["backupPlan"] != "projects/p/locations/us-central1/backupPlans/other" || !strings.Contains(r["dataSource"].(string), "/backupVaults/w/") {
		t.Errorf("association patch: %v", r)
	}
	_, old := call(t, s, "GET", "/v1/"+ds, "")
	if old["configState"] != "PASSIVE" {
		t.Errorf("the old data source: %v", old["configState"])
	}
}

func TestFetchViewsAndOperations(t *testing.T) {
	s, ds := setup(t)
	_, out := call(t, s, "GET", loc+"/backupVaults:fetchUsable", "")
	if len(out["backupVaults"].([]any)) != 2 {
		t.Errorf("fetchUsable: %v", out)
	}
	_, out = call(t, s, "GET", loc+"/backupPlanAssociations:fetchForResourceType?resourceType=sqladmin.googleapis.com/Instance", "")
	if len(out["backupPlanAssociations"].([]any)) != 1 {
		t.Errorf("associations fetch: %v", out)
	}
	_, out = call(t, s, "GET", loc+"/backupPlanAssociations:fetchForResourceType?resourceType=compute.googleapis.com/Instance", "")
	if len(out["backupPlanAssociations"].([]any)) != 0 {
		t.Errorf("compute associations fetch: %v", out)
	}
	if code, _ := call(t, s, "GET", loc+"/backupPlanAssociations:fetchForResourceType", ""); code != 400 {
		t.Errorf("fetch without resourceType: %d", code)
	}
	_, out = call(t, s, "GET", "/v1/"+ds+"/backups:fetchForResourceType?resourceType=sqladmin.googleapis.com/Instance", "")
	if len(out["backups"].([]any)) != 1 {
		t.Errorf("backups fetch: %v", out)
	}
	_, out = call(t, s, "GET", loc+"/dataSourceReferences", "")
	refs := out["dataSourceReferences"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["dataSource"] != ds {
		t.Fatalf("references: %v", out)
	}
	ref := refs[0].(map[string]any)["name"].(string)
	if code, got := call(t, s, "GET", "/v1/"+ref, ""); code != 200 || got["dataSourceBackupCount"] != "1" ||
		got["dataSourceGcpResourceInfo"].(map[string]any)["cloudSqlInstanceProperties"].(map[string]any)["name"] != "projects/p/instances/db" {
		t.Errorf("reference get: %d %v", code, got)
	}
	_, out = call(t, s, "GET", loc+"/dataSourceReferences:fetchForResourceType?resourceType=sqladmin.googleapis.com/Instance", "")
	if len(out["dataSourceReferences"].([]any)) != 1 {
		t.Errorf("references fetch: %v", out)
	}
	_, out = call(t, s, "POST", loc+"/backupVaults/v:testIamPermissions", `{"permissions":["backupdr.backupVaults.get"]}`)
	if len(out["permissions"].([]any)) != 1 {
		t.Errorf("testIamPermissions: %v", out)
	}

	_, ops := call(t, s, "GET", loc+"/operations", "")
	op := ops["operations"].([]any)[0].(map[string]any)["name"].(string)
	if code, _ := call(t, s, "POST", "/v1/"+op+":cancel", `{}`); code != 200 {
		t.Errorf("cancel: %d", code)
	}
	if code, _ := call(t, s, "DELETE", "/v1/"+op, ""); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, s, "GET", "/v1/"+op, ""); code != 404 {
		t.Errorf("a deleted operation: %d", code)
	}
}

func TestRefusalsNameWhatIsMissing(t *testing.T) {
	s, ds := setup(t)
	for _, c := range []struct{ method, path, want string }{
		{"GET", loc + "/managementServers", "Compute Engine"},
		{"POST", loc + "/managementServers?managementServerId=m", "VPC peering"},
		{"GET", loc + "/managementServers/m:getIamPolicy", "management server"},
		{"POST", loc + ":msComplianceMetadata", "management server"},
		{"POST", "/v1/" + ds + ":initiateBackup", "appliance"},
		{"POST", "/v1/" + ds + ":fetchAccessToken", "appliance"},
		{"POST", "/v1/" + ds + ":remove", "Compute Engine"},
		{"GET", loc + "/trial", "billing"},
		{"POST", loc + "/trial:subscribe", "billing"},
		{"GET", loc + "/resourceBackupConfigs", "Cloud Asset Inventory"},
		{"GET", "/v1/folders/f/locations/us-central1/resourceBackupConfigs:fetch", "folder"},
		{"POST", loc + "/serviceConfig:initialize", "default backup vault"},
	} {
		code, out := call(t, s, c.method, c.path, `{}`)
		msg, _ := out["error"].(map[string]any)["message"].(string)
		if code != 501 || !strings.Contains(msg, c.want) {
			t.Errorf("%s %s: %d %q, want 501 naming %q", c.method, c.path, code, msg, c.want)
		}
	}
}
