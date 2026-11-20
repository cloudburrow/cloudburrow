package iameval

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
)

// The twelfth round's checks (#1879) of the APIs served over JSON alone,
// by path: the Backup and DR Service and the Cloud Storage companion APIs
// (Storage Batch Operations, Storage Insights and Storage Transfer v1).

// PathRule is a JSON method matched by its path: the permission on the
// resource its path (the pattern's first group, after Prefix), its
// projectId query parameter or its body names.
type PathRule struct {
	// Method is the HTTP method, "" for any.
	Method string
	// Pattern matches the path; its first group is the resource.
	Pattern    *regexp.Regexp
	Permission string
	// Prefix goes before the group ("projects/" for a project ID).
	Prefix string
	// Project, when set, names the resource's project instead of the
	// path: "query" (the projectId parameter), "body" (the body's
	// projectId), "filter" (the filter parameter's JSON projectId) or
	// "lookup" (the project the Lookup func finds for the path's group).
	Project string
	// Filter is the answer's repeated field kept to what the caller holds
	// Permission on (a list that answers only what the caller may get).
	Filter string
	// Test answers testIamPermissions on the resource with what the
	// caller holds.
	Test bool
}

// pathRx is ^/v1/PATTERN$, with L standing for a location's name.
func pathRx(p string) *regexp.Regexp {
	p = strings.ReplaceAll(p, "{L}", `projects/[^/]+/locations/[^/]+`)
	p = strings.ReplaceAll(p, "{id}", `[^/:]+`)
	return regexp.MustCompile("^/v1/" + p + "$")
}

// rule is a PathRule of method, pattern and permission.
func rule(method, pattern, permission string) PathRule {
	return PathRule{Method: method, Pattern: pathRx(pattern), Permission: permission}
}

// MatchPathRule is the first of rules matching a request, with the
// path's group.
func MatchPathRule(rules []PathRule, method, path string) (*PathRule, string) {
	for i := range rules {
		r := &rules[i]
		if r.Method != "" && r.Method != method {
			continue
		}
		if g := r.Pattern.FindStringSubmatch(path); g != nil {
			return r, g[1]
		}
	}
	return nil, ""
}

// HTTPRules checks svc's JSON requests by rules; lookup, when set, is the
// project of a Project "lookup" rule's group, "" for unknown.
func (e *Enforcer) HTTPRules(svc *Service, rules []PathRule, lookup func(r *http.Request, group string) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		ru, group := MatchPathRule(rules, r.Method, r.URL.Path)
		if ru == nil {
			next.ServeHTTP(w, r)
			return
		}
		resource := ru.Prefix + group
		switch ru.Project {
		case "query":
			resource = projectName(r.URL.Query().Get("projectId"))
		case "body":
			resource = projectName(bodyProject(r))
		case "filter":
			var f struct {
				ProjectID      string `json:"projectId"`
				ProjectIDSnake string `json:"project_id"`
			}
			_ = json.Unmarshal([]byte(r.URL.Query().Get("filter")), &f)
			if f.ProjectID == "" {
				f.ProjectID = f.ProjectIDSnake
			}
			resource = projectName(f.ProjectID)
		case "lookup":
			resource = ""
			if lookup != nil {
				resource = projectName(lookup(r, group))
			}
		}
		switch {
		case ru.Test:
			e.testPermissionsJSON(w, r, svc, resource, next)
			return
		case ru.Filter != "":
			e.filterJSON(w, r, svc, &Method{Permission: ru.Permission, Filter: ru.Filter}, next)
			return
		case resource == "":
			// A request naming no project, or an operation that does not
			// exist: the handler refuses it (project_id is required, NOT_FOUND).
			next.ServeHTTP(w, r)
			return
		}
		if err := e.Authorize(r.Context(), svc, Bearer(r.Header.Get("Authorization")), ru.Permission, resource); err != nil {
			WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// projectName is "projects/ID", "" for no ID.
func projectName(id string) string {
	if id == "" {
		return ""
	}
	return "projects/" + id
}

// bodyProject is a JSON body's top-level projectId, left for the handler.
func bodyProject(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	var body struct {
		ProjectID string `json:"projectId"`
	}
	if err != nil || json.Unmarshal(b, &body) != nil {
		return ""
	}
	return body.ProjectID
}

// AnswerProject is the projectId (top-level or in metadata) of next's
// answer to a GET of path, as the instance's own account, "" for none.
func AnswerProject(r *http.Request, path string, next http.Handler) string {
	u := &url.URL{Path: path}
	req := httptest.NewRequest(http.MethodGet, u.String(), nil).WithContext(r.Context())
	req.Host = r.Host
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return ""
	}
	var out struct {
		ProjectID string `json:"projectId"`
		Metadata  struct {
			ProjectID string `json:"projectId"`
		} `json:"metadata"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		return ""
	}
	if out.ProjectID != "" {
		return out.ProjectID
	}
	return out.Metadata.ProjectID
}

// BackupDR is the Backup and DR Service's table. Its deny wording is
// newerDeny's (UNVERIFIED, 2026-11-20: the pages give none). It keeps no
// resource-level policies (a vault's testIamPermissions answers what the
// caller holds on it, "A caller is not required to have Google IAM
// permission to make this request").
var BackupDR = &Service{Name: "backupdr.googleapis.com", Deny: newerDeny,
	Types: map[string]string{"backupVaults": "backupdr.googleapis.com/BackupVault", "backupPlans": "backupdr.googleapis.com/BackupPlan",
		"backupPlanAssociations": "backupdr.googleapis.com/BackupPlanAssociation", "dataSources": "backupdr.googleapis.com/DataSource",
		"backups": "backupdr.googleapis.com/Backup"}}

// BackupDRRules are its methods' permissions. The backup vault subtree's
// (vaults, data sources, backups) are those its REST reference pages name
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/reference/rest/v1,
// read 2026-11-20); backupVaults.fetchUsable answers "the ones that user
// has backupdr.backupVaults.get permission" on. The pages of plans,
// revisions, associations, data source references, resource backup
// configs, the trial, locations and operations name no permission: each
// is the permission of its name in "Backup and DR roles and permissions"
// (https://docs.cloud.google.com/iam/docs/roles-permissions/backupdr),
// the Cloud SQL one where the index names a permission per workload
// (backupPlanAssociations.createForCloudSqlInstance), as CloudBurrow
// backs up Cloud SQL instances only. managementServers.msComplianceMetadata
// names none and has none of its name, and is not checked.
var BackupDRRules = []PathRule{
	rule("GET", `(projects/[^/]+)/locations`, "backupdr.locations.list"),
	rule("GET", `({L})`, "backupdr.locations.get"),
	rule("GET", `({L})/resourceBackupConfigs`, "backupdr.resourceBackupConfigs.list"),
	rule("GET", `({L})/resourceBackupConfigs:fetch`, "backupdr.resourceBackupConfigs.fetch"),
	rule("GET", `((?:folders|organizations)/[^/]+/locations/[^/]+)/resourceBackupConfigs:fetch`, "backupdr.resourceBackupConfigs.fetch"),
	rule("GET", `({L})/trial`, "backupdr.trial.get"),
	rule("POST", `({L})/trial:subscribe`, "backupdr.trial.subscribe"),
	rule("POST", `({L})/trial:end`, "backupdr.trial.end"),
	rule("GET", `({L})/operations`, "backupdr.operations.list"),
	rule("GET", `({L}/operations/{id})`, "backupdr.operations.get"),
	rule("POST", `({L}/operations/{id}):cancel`, "backupdr.operations.cancel"),
	rule("DELETE", `({L}/operations/{id})`, "backupdr.operations.delete"),
	rule("GET", `({L})/dataSourceReferences`, "backupdr.dataSourceReferences.list"),
	rule("GET", `({L})/dataSourceReferences:fetchForResourceType`, "backupdr.dataSourceReferences.fetchForCloudSqlInstance"),
	rule("GET", `({L}/dataSourceReferences/{id})`, "backupdr.dataSourceReferences.getForCloudSqlInstance"),
	rule("GET", `({L})/backupVaults`, "backupdr.backupVaults.list"),
	{Method: "GET", Pattern: pathRx(`({L})/backupVaults:fetchUsable`), Permission: "backupdr.backupVaults.get", Filter: "backupVaults"},
	rule("POST", `({L})/backupVaults`, "backupdr.backupVaults.create"),
	rule("GET", `({L}/backupVaults/{id})`, "backupdr.backupVaults.get"),
	rule("PATCH", `({L}/backupVaults/{id})`, "backupdr.backupVaults.update"),
	rule("DELETE", `({L}/backupVaults/{id})`, "backupdr.backupVaults.delete"),
	{Method: "POST", Pattern: pathRx(`({L}/backupVaults/{id}):testIamPermissions`), Test: true},
	rule("GET", `({L}/backupVaults/{id})/dataSources`, "backupdr.bvdataSources.list"),
	rule("GET", `({L}/backupVaults/{id}/dataSources/{id})`, "backupdr.bvdataSources.get"),
	rule("PATCH", `({L}/backupVaults/{id}/dataSources/{id})`, "backupdr.bvdataSources.update"),
	rule("GET", `({L}/backupVaults/{id}/dataSources/{id})/backups`, "backupdr.bvbackups.list"),
	rule("GET", `({L}/backupVaults/{id}/dataSources/{id})/backups:fetchForResourceType`, "backupdr.bvbackups.fetchForCloudSqlInstance"),
	rule("GET", `({L}/backupVaults/{id}/dataSources/{id}/backups/{id})`, "backupdr.bvbackups.get"),
	rule("PATCH", `({L}/backupVaults/{id}/dataSources/{id}/backups/{id})`, "backupdr.bvbackups.update"),
	rule("DELETE", `({L}/backupVaults/{id}/dataSources/{id}/backups/{id})`, "backupdr.bvbackups.delete"),
	rule("POST", `({L}/backupVaults/{id}/dataSources/{id}/backups/{id}):restore`, "backupdr.bvbackups.restore"),
	rule("GET", `({L})/backupPlans`, "backupdr.backupPlans.list"),
	rule("POST", `({L})/backupPlans`, "backupdr.backupPlans.create"),
	rule("GET", `({L}/backupPlans/{id})`, "backupdr.backupPlans.get"),
	rule("PATCH", `({L}/backupPlans/{id})`, "backupdr.backupPlans.update"),
	rule("DELETE", `({L}/backupPlans/{id})`, "backupdr.backupPlans.delete"),
	rule("GET", `({L}/backupPlans/{id})/revisions`, "backupdr.backupPlanRevisions.list"),
	rule("GET", `({L}/backupPlans/{id}/revisions/{id})`, "backupdr.backupPlanRevisions.get"),
	rule("GET", `({L})/backupPlanAssociations`, "backupdr.backupPlanAssociations.list"),
	rule("POST", `({L})/backupPlanAssociations`, "backupdr.backupPlanAssociations.createForCloudSqlInstance"),
	rule("GET", `({L})/backupPlanAssociations:fetchForResourceType`, "backupdr.backupPlanAssociations.fetchForCloudSqlInstance"),
	rule("GET", `({L}/backupPlanAssociations/{id})`, "backupdr.backupPlanAssociations.getForCloudSqlInstance"),
	rule("PATCH", `({L}/backupPlanAssociations/{id})`, "backupdr.backupPlanAssociations.updateForCloudSqlInstance"),
	rule("DELETE", `({L}/backupPlanAssociations/{id})`, "backupdr.backupPlanAssociations.deleteForCloudSqlInstance"),
	rule("POST", `({L}/backupPlanAssociations/{id}):triggerBackup`, "backupdr.backupPlanAssociations.triggerBackupForCloudSqlInstance"),
}

// StorageBatchOperations is Storage Batch Operations' table; it keeps no
// resource-level policies. Deny wording: newerDeny's (UNVERIFIED).
var StorageBatchOperations = &Service{Name: "storagebatchoperations.googleapis.com", Deny: newerDeny}

// StorageBatchOperationsRules are its methods' permissions, as its REST
// reference pages name them
// (https://docs.cloud.google.com/storage/docs/storagebatchoperations/reference/rest,
// read 2026-11-20): storagebatchoperations.COLLECTION.VERB on the parent
// for a list or a create, on the resource otherwise.
var StorageBatchOperationsRules = []PathRule{
	rule("GET", `(projects/[^/]+)/locations`, "storagebatchoperations.locations.list"),
	rule("GET", `({L})`, "storagebatchoperations.locations.get"),
	rule("GET", `({L})/jobs`, "storagebatchoperations.jobs.list"),
	rule("POST", `({L})/jobs`, "storagebatchoperations.jobs.create"),
	rule("GET", `({L}/jobs/{id})`, "storagebatchoperations.jobs.get"),
	rule("DELETE", `({L}/jobs/{id})`, "storagebatchoperations.jobs.delete"),
	rule("POST", `({L}/jobs/{id}):cancel`, "storagebatchoperations.jobs.cancel"),
	rule("GET", `({L}/jobs/{id})/bucketOperations`, "storagebatchoperations.bucketOperations.list"),
	rule("GET", `({L}/jobs/{id}/bucketOperations/{id})`, "storagebatchoperations.bucketOperations.get"),
	rule("GET", `({L})/operations`, "storagebatchoperations.operations.list"),
	rule("GET", `({L}/operations/{id})`, "storagebatchoperations.operations.get"),
	rule("POST", `({L}/operations/{id}):cancel`, "storagebatchoperations.operations.cancel"),
	rule("DELETE", `({L}/operations/{id})`, "storagebatchoperations.operations.delete"),
}

// StorageInsights is Storage Insights' table; it keeps no resource-level
// policies. Deny wording: newerDeny's (UNVERIFIED).
var StorageInsights = &Service{Name: "storageinsights.googleapis.com", Deny: newerDeny}

// StorageInsightsRules are its methods' permissions, as its REST reference
// pages name them (https://docs.cloud.google.com/storage/docs/insights/reference/rest,
// read 2026-11-20): storageinsights.COLLECTION.VERB, patch's ".update",
// on the parent for a list or a create, on the resource otherwise.
var StorageInsightsRules = []PathRule{
	rule("GET", `(projects/[^/]+)/locations`, "storageinsights.locations.list"),
	rule("GET", `({L})`, "storageinsights.locations.get"),
	rule("GET", `({L})/reportConfigs`, "storageinsights.reportConfigs.list"),
	rule("POST", `({L})/reportConfigs`, "storageinsights.reportConfigs.create"),
	rule("GET", `({L}/reportConfigs/{id})`, "storageinsights.reportConfigs.get"),
	rule("PATCH", `({L}/reportConfigs/{id})`, "storageinsights.reportConfigs.update"),
	rule("DELETE", `({L}/reportConfigs/{id})`, "storageinsights.reportConfigs.delete"),
	rule("GET", `({L}/reportConfigs/{id})/reportDetails`, "storageinsights.reportDetails.list"),
	rule("GET", `({L}/reportConfigs/{id}/reportDetails/{id})`, "storageinsights.reportDetails.get"),
	rule("GET", `({L})/datasetConfigs`, "storageinsights.datasetConfigs.list"),
	rule("POST", `({L})/datasetConfigs`, "storageinsights.datasetConfigs.create"),
	rule("GET", `({L}/datasetConfigs/{id})`, "storageinsights.datasetConfigs.get"),
	rule("PATCH", `({L}/datasetConfigs/{id})`, "storageinsights.datasetConfigs.update"),
	rule("DELETE", `({L}/datasetConfigs/{id})`, "storageinsights.datasetConfigs.delete"),
	rule("POST", `({L}/datasetConfigs/{id}):linkDataset`, "storageinsights.datasetConfigs.linkDataset"),
	rule("POST", `({L}/datasetConfigs/{id}):unlinkDataset`, "storageinsights.datasetConfigs.unlinkDataset"),
	rule("GET", `({L})/operations`, "storageinsights.operations.list"),
	rule("GET", `({L}/operations/{id})`, "storageinsights.operations.get"),
	rule("POST", `({L}/operations/{id}):cancel`, "storageinsights.operations.cancel"),
	rule("DELETE", `({L}/operations/{id})`, "storageinsights.operations.delete"),
}

// StorageTransfer is the Storage Transfer API v1's table; it keeps no
// resource-level policies, and every check is on the project. Deny
// wording: newerDeny's (UNVERIFIED).
var StorageTransfer = &Service{Name: "storagetransfer.googleapis.com", Deny: newerDeny}

// StorageTransferRules are its methods' permissions
// (https://docs.cloud.google.com/storage-transfer/docs/reference/rest,
// read 2026-11-20): the agent pool, googleServiceAccounts.get and
// transferOperations pages name theirs on the project or the operation;
// the transferJobs pages name none, and each is storagetransfer.jobs.VERB
// ("Storage Transfer Service roles and permissions",
// https://docs.cloud.google.com/iam/docs/roles-permissions/storagetransfer),
// patch's ".update", on its projectId's project. An operation is checked
// on the project it runs in.
var StorageTransferRules = []PathRule{
	{Method: "GET", Pattern: pathRx(`googleServiceAccounts/([^/]+)`), Prefix: "projects/", Permission: "storagetransfer.projects.getServiceAccount"},
	rule("GET", `(projects/[^/]+)/agentPools`, "storagetransfer.agentpools.list"),
	rule("POST", `(projects/[^/]+)/agentPools`, "storagetransfer.agentpools.create"),
	rule("GET", `(projects/[^/]+/agentPools/{id})`, "storagetransfer.agentpools.get"),
	rule("PATCH", `(projects/[^/]+/agentPools/{id})`, "storagetransfer.agentpools.update"),
	rule("DELETE", `(projects/[^/]+/agentPools/{id})`, "storagetransfer.agentpools.delete"),
	{Method: "POST", Pattern: pathRx(`(transferJobs)`), Permission: "storagetransfer.jobs.create", Project: "body"},
	{Method: "GET", Pattern: pathRx(`(transferJobs)`), Permission: "storagetransfer.jobs.list", Project: "filter"},
	{Method: "GET", Pattern: pathRx(`(transferJobs/.+)`), Permission: "storagetransfer.jobs.get", Project: "query"},
	{Method: "DELETE", Pattern: pathRx(`(transferJobs/.+)`), Permission: "storagetransfer.jobs.delete", Project: "query"},
	{Method: "PATCH", Pattern: pathRx(`(transferJobs/.+)`), Permission: "storagetransfer.jobs.update", Project: "body"},
	{Method: "POST", Pattern: pathRx(`(transferJobs/.+):run`), Permission: "storagetransfer.jobs.run", Project: "body"},
	{Method: "GET", Pattern: pathRx(`(transferOperations)`), Permission: "storagetransfer.operations.list", Project: "filter"},
	{Method: "GET", Pattern: pathRx(`(transferOperations/[^:]+)`), Permission: "storagetransfer.operations.get", Project: "lookup"},
	{Method: "POST", Pattern: pathRx(`(transferOperations/[^:]+):pause`), Permission: "storagetransfer.operations.pause", Project: "lookup"},
	{Method: "POST", Pattern: pathRx(`(transferOperations/[^:]+):resume`), Permission: "storagetransfer.operations.resume", Project: "lookup"},
	{Method: "POST", Pattern: pathRx(`(transferOperations/[^:]+):cancel`), Permission: "storagetransfer.operations.cancel", Project: "lookup"},
}
