package iameval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRound12BackupRules (#1879): every permission the Backup and DR and
// Cloud Storage companion tables name is in Google's index, and each
// served method's path picks its own row.
func TestRound12BackupRules(t *testing.T) {
	names := iamroles.Names()
	for _, rs := range [][]PathRule{BackupDRRules, StorageBatchOperationsRules, StorageInsightsRules, StorageTransferRules} {
		for _, r := range rs {
			if r.Test {
				continue
			}
			if _, ok := slices.BinarySearch(names, r.Permission); !ok {
				t.Errorf("%s: %s is not in Google's permission index", r.Pattern, r.Permission)
			}
		}
	}
	const l = "projects/p/locations/us-central1"
	for _, c := range []struct {
		rules                []PathRule
		method, path         string
		permission, resource string
	}{
		{BackupDRRules, "GET", "/v1/projects/p/locations", "backupdr.locations.list", "projects/p"},
		{BackupDRRules, "GET", "/v1/" + l, "backupdr.locations.get", l},
		{BackupDRRules, "GET", "/v1/" + l + "/backupVaults", "backupdr.backupVaults.list", l},
		{BackupDRRules, "GET", "/v1/" + l + "/backupVaults/v", "backupdr.backupVaults.get", l + "/backupVaults/v"},
		{BackupDRRules, "GET", "/v1/" + l + "/backupVaults:fetchUsable", "backupdr.backupVaults.get", l},
		{BackupDRRules, "PATCH", "/v1/" + l + "/backupVaults/v", "backupdr.backupVaults.update", l + "/backupVaults/v"},
		{BackupDRRules, "GET", "/v1/" + l + "/backupVaults/v/dataSources/d/backups", "backupdr.bvbackups.list", l + "/backupVaults/v/dataSources/d"},
		{BackupDRRules, "POST", "/v1/" + l + "/backupVaults/v/dataSources/d/backups/b:restore", "backupdr.bvbackups.restore", l + "/backupVaults/v/dataSources/d/backups/b"},
		{BackupDRRules, "POST", "/v1/" + l + "/backupPlanAssociations/a:triggerBackup", "backupdr.backupPlanAssociations.triggerBackupForCloudSqlInstance", l + "/backupPlanAssociations/a"},
		{BackupDRRules, "GET", "/v1/" + l + "/backupPlans/x/revisions/r", "backupdr.backupPlanRevisions.get", l + "/backupPlans/x/revisions/r"},
		{BackupDRRules, "GET", "/v1/folders/1/locations/us-central1/resourceBackupConfigs:fetch", "backupdr.resourceBackupConfigs.fetch", "folders/1/locations/us-central1"},
		{BackupDRRules, "POST", "/v1/" + l + "/operations/o:cancel", "backupdr.operations.cancel", l + "/operations/o"},
		{StorageBatchOperationsRules, "POST", "/v1/projects/p/locations/global/jobs", "storagebatchoperations.jobs.create", "projects/p/locations/global"},
		{StorageBatchOperationsRules, "POST", "/v1/projects/p/locations/global/jobs/j:cancel", "storagebatchoperations.jobs.cancel", "projects/p/locations/global/jobs/j"},
		{StorageBatchOperationsRules, "GET", "/v1/projects/p/locations/global/jobs/j/bucketOperations/b", "storagebatchoperations.bucketOperations.get", "projects/p/locations/global/jobs/j/bucketOperations/b"},
		{StorageInsightsRules, "POST", "/v1/" + l + "/datasetConfigs/d:linkDataset", "storageinsights.datasetConfigs.linkDataset", l + "/datasetConfigs/d"},
		{StorageInsightsRules, "PATCH", "/v1/" + l + "/reportConfigs/r", "storageinsights.reportConfigs.update", l + "/reportConfigs/r"},
		{StorageTransferRules, "POST", "/v1/transferJobs/transferJobs/123:run", "storagetransfer.jobs.run", "transferJobs/transferJobs/123"},
		{StorageTransferRules, "GET", "/v1/transferJobs/transferJobs/123", "storagetransfer.jobs.get", "transferJobs/transferJobs/123"},
		{StorageTransferRules, "POST", "/v1/transferOperations/transferJobs-1-2:pause", "storagetransfer.operations.pause", "transferOperations/transferJobs-1-2"},
		{StorageTransferRules, "GET", "/v1/googleServiceAccounts/p", "storagetransfer.projects.getServiceAccount", "p"},
	} {
		r, group := MatchPathRule(c.rules, c.method, c.path)
		if r == nil || r.Permission != c.permission || group != c.resource {
			got := ""
			if r != nil {
				got = r.Permission
			}
			t.Errorf("%s %s = %s on %q; want %s on %q", c.method, c.path, got, group, c.permission, c.resource)
		}
	}
}

// TestRound12BackupEnforcer: a caller without a role is refused with
// Google's words; a project grant, a filter and testIamPermissions answer
// what the caller holds; a transfer job is checked on its projectId and an
// operation on the project its answer names.
func TestRound12BackupEnforcer(t *testing.T) {
	src := fakeSource{policies: map[string]*iampolicy.Stored{}}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true },
		CallerOf: func(b string) Caller { return Caller{Email: b + "@p.iam.gserviceaccount.com"} }}
	grant := func(role string) {
		src.policies["projects/p"] = &iampolicy.Stored{Bindings: []iampolicy.Binding{{Role: role, Members: []string{"serviceAccount:u@p.iam.gserviceaccount.com"}}}}
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":fetchUsable"):
			_ = json.NewEncoder(w).Encode(map[string]any{"backupVaults": []any{map[string]any{"name": "projects/p/locations/l/backupVaults/v"}}})
		case strings.HasSuffix(r.URL.Path, ":testIamPermissions"):
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": []string{"backupdr.backupVaults.get", "backupdr.backupVaults.delete"}})
		case strings.HasPrefix(r.URL.Path, "/v1/transferOperations/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "transferOperations/x", "metadata": map[string]any{"projectId": "p"}})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	call := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(context.Background())
		req.Header.Set("Authorization", "Bearer u")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	bdr := e.HTTPRules(BackupDR, BackupDRRules, nil, backend)
	rec := call(bdr, "GET", "/v1/projects/p/locations/l/backupVaults/v", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(),
		"Permission 'backupdr.backupVaults.get' denied on resource 'projects/p/locations/l/backupVaults/v' (or it may not exist).") {
		t.Fatalf("get without a role = %d %s", rec.Code, rec.Body)
	}
	if rec := call(bdr, "GET", "/v1/projects/p/locations/l/backupVaults:fetchUsable", ""); strings.Contains(rec.Body.String(), "backupVaults/v") {
		t.Errorf("fetchUsable without a role = %s; want the vault left out", rec.Body)
	}
	grant("roles/backupdr.viewer")
	if rec := call(bdr, "GET", "/v1/projects/p/locations/l/backupVaults/v", ""); rec.Code != http.StatusOK {
		t.Errorf("get as a viewer = %d %s", rec.Code, rec.Body)
	}
	if rec := call(bdr, "GET", "/v1/projects/p/locations/l/backupVaults:fetchUsable", ""); !strings.Contains(rec.Body.String(), "backupVaults/v") {
		t.Errorf("fetchUsable as a viewer = %s", rec.Body)
	}
	rec = call(bdr, "POST", "/v1/projects/p/locations/l/backupVaults/v:testIamPermissions", `{}`)
	if b := rec.Body.String(); !strings.Contains(b, "backupdr.backupVaults.get") || strings.Contains(b, "backupdr.backupVaults.delete") {
		t.Errorf("testIamPermissions as a viewer = %s", b)
	}
	if rec := call(bdr, "DELETE", "/v1/projects/p/locations/l/backupVaults/v", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete as a viewer = %d", rec.Code)
	}

	st := e.HTTPRules(StorageTransfer, StorageTransferRules, func(r *http.Request, name string) string {
		return AnswerProject(r, "/v1/"+name, backend)
	}, backend)
	if rec := call(st, "GET", "/v1/transferJobs/transferJobs/1?projectId=p", ""); rec.Code != http.StatusForbidden {
		t.Errorf("transferJobs.get as a Backup and DR viewer = %d", rec.Code)
	}
	if rec := call(st, "POST", "/v1/transferOperations/x:pause", "{}"); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "storagetransfer.operations.pause") {
		t.Errorf("operations.pause without a role = %d %s", rec.Code, rec.Body)
	}
	grant("roles/storagetransfer.admin")
	for _, c := range [][3]string{{"GET", "/v1/transferJobs/transferJobs/1?projectId=p", ""},
		{"POST", "/v1/transferJobs/transferJobs/1:run", `{"projectId":"p"}`},
		{"POST", "/v1/transferOperations/x:pause", "{}"}} {
		if rec := call(st, c[0], c[1], c[2]); rec.Code != http.StatusOK {
			t.Errorf("%s %s as an admin = %d %s", c[0], c[1], rec.Code, rec.Body)
		}
	}
	if rec := call(st, "POST", "/v1/transferJobs/transferJobs/1:run", `{"projectId":"q"}`); rec.Code != http.StatusForbidden {
		t.Errorf("run in another project = %d", rec.Code)
	}
}
