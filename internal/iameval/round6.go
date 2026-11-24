package iameval

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The sixth round's tables (#1879): Cloud Run Admin API v2's every method,
// Cloud Functions' admin surface (v2 and v1), Artifact Registry and Cloud
// Build, each method with the permission Google's reference names for it:
// its REST or RPC reference page ("Authorization requires the following
// IAM permission on the specified resource"), or, where that names none,
// the service's audit-logging page, which lists each method's permission
// (read 2026-11-20). Every permission is in Google's permission index
// (TestRoundSixTables).

// newerDenyOf is the denial Google's newer APIs give, "Permission 'P'
// denied on resource 'R' (or it may not exist).", with ErrorInfo
// IAM_PERMISSION_DENIED.
func newerDenyOf(permission, resource string) error {
	return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
		permission, resource, true)
}

// Run is Cloud Run Admin API v2's table. The permissions are those of
// https://docs.cloud.google.com/run/docs/reference/rest/v2 and
// https://docs.cloud.google.com/run/docs/reference/rpc/google.cloud.run.v2,
// or, for the Get, Update, Delete and IAM methods those pages leave
// without one, of "Cloud Run audit logging"
// (https://docs.cloud.google.com/run/docs/audit-logging), read 2026-11-20.
// CancelExecution is run.jobs.run on the execution, as its REST and RPC
// pages name it (the audit page names run.executions.cancel). RunJob also
// needs run.jobs.runWithOverrides when it carries overrides ("on the
// specified resource overrides"). Five methods no page names a permission
// for are checked with the permission of their name, UNVERIFIED (docs
// re-read 2026-11-20): UpdateInstance run.instances.update, and the
// worker pools' and instances' GetIamPolicy and SetIamPolicy. SubmitBuild
// is cloudbuild.builds.create on its parent (the audit page).
// SourceUpload's UploadSource and the port's Operations and Locations
// mixins are the seventh round's (round7.go). TestIamPermissions needs no
// permission ("There are no permissions required for making this API
// call.") and answers what the caller holds. Services, jobs, worker pools
// and instances keep their own policies. A template's service account
// needs iam.serviceAccounts.actAs on it, "required to attach a service
// account on the service or revision" (and "on the job", "on the worker
// pool": https://docs.cloud.google.com/run/docs/configuring/services/service-identity,
// .../jobs/service-identity, .../workerpools/service-identity), refused
// in the troubleshooting page's words, "Permission
// 'iam.serviceaccounts.actAs' denied on service account E"
// (https://docs.cloud.google.com/run/docs/troubleshooting). Its own
// denials are the newer APIs' wording, UNVERIFIED (re-read 2026-11-20:
// the pages give none).
var Run = &Service{
	Name:              "run.googleapis.com",
	PolicyCollections: []string{"services", "jobs", "workerPools", "instances"},
	Types: map[string]string{"services": "run.googleapis.com/Service", "revisions": "run.googleapis.com/Revision",
		"jobs": "run.googleapis.com/Job", "executions": "run.googleapis.com/Execution", "tasks": "run.googleapis.com/Task",
		"workerPools": "run.googleapis.com/WorkerPool", "instances": "run.googleapis.com/Instance"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.cloud.run.v2.Services/": {
				"CreateService": {"run.services.create", "parent"},
				"GetService":    {"run.services.get", "name"},
				"ListServices":  {"run.services.list", "parent"},
				"UpdateService": {"run.services.update", "service.name"},
				"DeleteService": {"run.services.delete", "name"},
				"GetIamPolicy":  {"run.services.getIamPolicy", "resource"},
				"SetIamPolicy":  {"run.services.setIamPolicy", "resource"},
			},
			"/google.cloud.run.v2.Revisions/": {
				"GetRevision":    {"run.revisions.get", "name"},
				"ListRevisions":  {"run.revisions.list", "parent"},
				"DeleteRevision": {"run.revisions.delete", "name"},
			},
			"/google.cloud.run.v2.Jobs/": {
				"CreateJob":    {"run.jobs.create", "parent"},
				"GetJob":       {"run.jobs.get", "name"},
				"ListJobs":     {"run.jobs.list", "parent"},
				"UpdateJob":    {"run.jobs.update", "job.name"},
				"DeleteJob":    {"run.jobs.delete", "name"},
				"RunJob":       {"run.jobs.run", "name"},
				"GetIamPolicy": {"run.jobs.getIamPolicy", "resource"},
				"SetIamPolicy": {"run.jobs.setIamPolicy", "resource"},
			},
			"/google.cloud.run.v2.Executions/": {
				"GetExecution":    {"run.executions.get", "name"},
				"ListExecutions":  {"run.executions.list", "parent"},
				"DeleteExecution": {"run.executions.delete", "name"},
				"CancelExecution": {"run.jobs.run", "name"},
			},
			"/google.cloud.run.v2.Tasks/": {
				"GetTask":   {"run.tasks.get", "name"},
				"ListTasks": {"run.tasks.list", "parent"},
			},
			"/google.cloud.run.v2.WorkerPools/": {
				"CreateWorkerPool": {"run.workerpools.create", "parent"},
				"GetWorkerPool":    {"run.workerpools.get", "name"},
				"ListWorkerPools":  {"run.workerpools.list", "parent"},
				"UpdateWorkerPool": {"run.workerpools.update", "worker_pool.name"},
				"DeleteWorkerPool": {"run.workerpools.delete", "name"},
				"GetIamPolicy":     {"run.workerpools.getIamPolicy", "resource"},
				"SetIamPolicy":     {"run.workerpools.setIamPolicy", "resource"},
			},
			"/google.cloud.run.v2.Instances/": {
				"CreateInstance": {"run.instances.create", "parent"},
				"GetInstance":    {"run.instances.get", "name"},
				"ListInstances":  {"run.instances.list", "parent"},
				"UpdateInstance": {"run.instances.update", "instance.name"},
				"DeleteInstance": {"run.instances.delete", "name"},
				"StartInstance":  {"run.instances.start", "name"},
				"StopInstance":   {"run.instances.stop", "name"},
				"GetIamPolicy":   {"run.instances.getIamPolicy", "resource"},
				"SetIamPolicy":   {"run.instances.setIamPolicy", "resource"},
			},
			"/google.cloud.run.v2.Builds/": {
				"SubmitBuild": {"cloudbuild.builds.create", "parent"},
			},
		})
		row := m["/google.cloud.run.v2.Jobs/RunJob"]
		row.IfSet, row.IfSetPermission = "overrides", RunJobOverrides
		m["/google.cloud.run.v2.Jobs/RunJob"] = row
		for full, account := range map[string]string{
			"/google.cloud.run.v2.Services/CreateService":       "service.template.service_account",
			"/google.cloud.run.v2.Services/UpdateService":       "service.template.service_account",
			"/google.cloud.run.v2.Jobs/CreateJob":               "job.template.template.service_account",
			"/google.cloud.run.v2.Jobs/UpdateJob":               "job.template.template.service_account",
			"/google.cloud.run.v2.WorkerPools/CreateWorkerPool": "worker_pool.template.service_account",
			"/google.cloud.run.v2.WorkerPools/UpdateWorkerPool": "worker_pool.template.service_account",
		} {
			row := m[full]
			row.ActAs = []string{account}
			m[full] = row
		}
		return m
	}(),
	Deny: newerDenyOf,
	DenyActAs: func(account, resource string) error {
		return grpcDenied("Permission 'iam.serviceaccounts.actAs' denied on service account "+account, ActAs, resource, true)
	},
}

// RunJobOverrides is the permission RunJob also needs when it carries
// overrides (the RunJob reference: "run.jobs.runWithOverrides ... on the
// specified resource overrides", read 2026-11-20).
const RunJobOverrides = "run.jobs.runWithOverrides"

// Functions is Cloud Functions' table, v2's FunctionService and its
// IAMPolicy mixin, and v1's CloudFunctionsService: the permission each
// method's REST reference names
// (https://docs.cloud.google.com/functions/docs/reference/rest/v2/projects.locations.functions
// and .../v1/projects.locations.functions, read 2026-11-20), or, for
// UpdateFunction and the IAM methods, which name none, "Cloud Functions
// audit logging" (https://docs.cloud.google.com/functions/docs/monitoring/audit-logging).
// GenerateUploadUrl is cloudfunctions.functions.sourceCodeSet, as its REST
// page names it (the audit page's generateUploadUrl is not in Google's
// permission index). Not checked: ListRuntimes ("iam.permissions.none"),
// TestIamPermissions (no page names one; it answers what the caller
// holds); the shared Operations and Locations mixins are the Run port's
// (round7.go). A function keeps
// its own policy. Deployers need "Service Account User role
// (roles/iam.serviceAccountUser) on the runtime service account", and on
// the build's (https://docs.cloud.google.com/functions/docs/reference/iam/roles),
// so CreateFunction and UpdateFunction check iam.serviceAccounts.actAs on
// them, refused in the troubleshooting page's words "Caller is missing
// permission 'iam.serviceaccounts.actAs' on service account
// projects/-/serviceAccounts/E." (https://docs.cloud.google.com/functions/docs/troubleshooting;
// its final period UNVERIFIED, re-read 2026-11-20). Its own denials are the
// newer APIs' wording, UNVERIFIED (re-read 2026-11-20: the pages give
// none). The IAMPolicy mixin is shared with Secure Source Manager's
// instances on this port, which are not checked here.
var Functions = &Service{
	Name:              "cloudfunctions.googleapis.com",
	PolicyCollections: []string{"functions"},
	Types:             map[string]string{"functions": "cloudfunctions.googleapis.com/Function"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.cloud.functions.v2.FunctionService/": {
				"GetFunction":         {"cloudfunctions.functions.get", "name"},
				"ListFunctions":       {"cloudfunctions.functions.list", "parent"},
				"CreateFunction":      {"cloudfunctions.functions.create", "parent"},
				"UpdateFunction":      {"cloudfunctions.functions.update", "function.name"},
				"DeleteFunction":      {"cloudfunctions.functions.delete", "name"},
				"GenerateUploadUrl":   {"cloudfunctions.functions.sourceCodeSet", "parent"},
				"GenerateDownloadUrl": {"cloudfunctions.functions.sourceCodeGet", "name"},
			},
			"/google.cloud.functions.v1.CloudFunctionsService/": {
				"ListFunctions":       {"cloudfunctions.functions.list", "parent"},
				"GetFunction":         {"cloudfunctions.functions.get", "name"},
				"CreateFunction":      {"cloudfunctions.functions.create", "location"},
				"UpdateFunction":      {"cloudfunctions.functions.update", "function.name"},
				"DeleteFunction":      {"cloudfunctions.functions.delete", "name"},
				"CallFunction":        {"cloudfunctions.functions.call", "name"},
				"GenerateUploadUrl":   {"cloudfunctions.functions.sourceCodeSet", "parent"},
				"GenerateDownloadUrl": {"cloudfunctions.functions.sourceCodeGet", "name"},
				"GetIamPolicy":        {"cloudfunctions.functions.getIamPolicy", "resource"},
				"SetIamPolicy":        {"cloudfunctions.functions.setIamPolicy", "resource"},
			},
		})
		for _, verb := range []string{"GetIamPolicy", "SetIamPolicy"} {
			perm := "cloudfunctions.functions." + strings.ToLower(verb[:1]) + verb[1:]
			m["/google.iam.v1.IAMPolicy/"+verb] = Method{Field: "resource", PermissionFor: func(resource string) string {
				if !strings.Contains(resource, "/functions/") {
					return "" // a Secure Source Manager instance's
				}
				return perm
			}}
		}
		for full, accounts := range map[string][]string{
			"/google.cloud.functions.v2.FunctionService/CreateFunction":       {"function.service_config.service_account_email", "function.build_config.service_account"},
			"/google.cloud.functions.v2.FunctionService/UpdateFunction":       {"function.service_config.service_account_email", "function.build_config.service_account"},
			"/google.cloud.functions.v1.CloudFunctionsService/CreateFunction": {"function.service_account_email", "function.build_service_account"},
			"/google.cloud.functions.v1.CloudFunctionsService/UpdateFunction": {"function.service_account_email", "function.build_service_account"},
		} {
			row := m[full]
			row.ActAs = accounts
			m[full] = row
		}
		return m
	}(),
	Deny: newerDenyOf,
	DenyActAs: func(account, resource string) error {
		return status.Error(codes.PermissionDenied, "Caller is missing permission 'iam.serviceaccounts.actAs' on service account projects/-/serviceAccounts/"+account+".")
	},
}

// ArtifactRegistry is Artifact Registry's table: the permission and
// request field the RPC reference names for each method
// (https://docs.cloud.google.com/artifact-registry/docs/reference/rpc/google.devtools.artifactregistry.v1,
// read 2026-11-20), in the lower-case form Google's permission index
// spells (the page writes dockerImages, mavenArtifacts, npmPackages and
// pythonPackages); GetIamPolicy and SetIamPolicy as "Artifact Registry
// audit logging" names them (https://docs.cloud.google.com/artifact-registry/docs/audit-logging).
// BatchDeleteVersions checks artifactregistry.versions.delete on each of
// its names. Not checked: GetVPCSCConfig and UpdateVPCSCConfig, whose
// artifactregistry.vpcscconfigs.get and .update the audit page names but
// neither Google's permission index nor any Artifact Registry role holds
// (re-read 2026-11-20), so no role could grant them,
// TestIamPermissions (no page names one; it answers what the caller
// holds), and the shared mixins. A repository keeps its own policy, which
// applies to its packages, versions, tags and files. Google's wording of a
// denial is UNVERIFIED (re-read 2026-11-20: the pages give none): the
// newer APIs'.
var ArtifactRegistry = &Service{
	Name:              "artifactregistry.googleapis.com",
	PolicyCollections: []string{"repositories"},
	Types:             map[string]string{"repositories": "artifactregistry.googleapis.com/Repository"},
	Methods: rows(map[string]map[string][2]string{
		"/google.devtools.artifactregistry.v1.ArtifactRegistry/": {
			"ListDockerImages":      {"artifactregistry.dockerimages.list", "parent"},
			"GetDockerImage":        {"artifactregistry.dockerimages.get", "name"},
			"ListMavenArtifacts":    {"artifactregistry.mavenartifacts.list", "parent"},
			"GetMavenArtifact":      {"artifactregistry.mavenartifacts.get", "name"},
			"ListNpmPackages":       {"artifactregistry.npmpackages.list", "parent"},
			"GetNpmPackage":         {"artifactregistry.npmpackages.get", "name"},
			"ListPythonPackages":    {"artifactregistry.pythonpackages.list", "parent"},
			"GetPythonPackage":      {"artifactregistry.pythonpackages.get", "name"},
			"ImportAptArtifacts":    {"artifactregistry.aptartifacts.create", "parent"},
			"ImportYumArtifacts":    {"artifactregistry.yumartifacts.create", "parent"},
			"ListRepositories":      {"artifactregistry.repositories.list", "parent"},
			"GetRepository":         {"artifactregistry.repositories.get", "name"},
			"CreateRepository":      {"artifactregistry.repositories.create", "parent"},
			"UpdateRepository":      {"artifactregistry.repositories.update", "repository.name"},
			"DeleteRepository":      {"artifactregistry.repositories.delete", "name"},
			"ListPackages":          {"artifactregistry.packages.list", "parent"},
			"GetPackage":            {"artifactregistry.packages.get", "name"},
			"DeletePackage":         {"artifactregistry.packages.delete", "name"},
			"UpdatePackage":         {"artifactregistry.packages.update", "package.name"},
			"ListVersions":          {"artifactregistry.versions.list", "parent"},
			"GetVersion":            {"artifactregistry.versions.get", "name"},
			"DeleteVersion":         {"artifactregistry.versions.delete", "name"},
			"BatchDeleteVersions":   {"artifactregistry.versions.delete", "names"},
			"UpdateVersion":         {"artifactregistry.versions.update", "version.name"},
			"ListFiles":             {"artifactregistry.files.list", "parent"},
			"GetFile":               {"artifactregistry.files.get", "name"},
			"DeleteFile":            {"artifactregistry.files.delete", "name"},
			"UpdateFile":            {"artifactregistry.files.update", "file.name"},
			"ListTags":              {"artifactregistry.tags.list", "parent"},
			"GetTag":                {"artifactregistry.tags.get", "name"},
			"CreateTag":             {"artifactregistry.tags.create", "parent"},
			"UpdateTag":             {"artifactregistry.tags.update", "tag.name"},
			"DeleteTag":             {"artifactregistry.tags.delete", "name"},
			"CreateRule":            {"artifactregistry.rules.create", "parent"},
			"ListRules":             {"artifactregistry.rules.list", "parent"},
			"GetRule":               {"artifactregistry.rules.get", "name"},
			"UpdateRule":            {"artifactregistry.rules.update", "rule.name"},
			"DeleteRule":            {"artifactregistry.rules.delete", "name"},
			"SetIamPolicy":          {"artifactregistry.repositories.setIamPolicy", "resource"},
			"GetIamPolicy":          {"artifactregistry.repositories.getIamPolicy", "resource"},
			"GetProjectSettings":    {"artifactregistry.projectsettings.get", "name"},
			"UpdateProjectSettings": {"artifactregistry.projectsettings.update", "project_settings.name"},
			"ListAttachments":       {"artifactregistry.attachments.list", "parent"},
			"GetAttachment":         {"artifactregistry.attachments.get", "name"},
			"CreateAttachment":      {"artifactregistry.attachments.create", "parent"},
			"DeleteAttachment":      {"artifactregistry.attachments.delete", "name"},
			"ExportArtifact":        {"artifactregistry.repositories.exportArtifacts", "repository"},
		},
	}),
	Deny: newerDenyOf,
}

// CloudBuild is Cloud Build's table: the permission each method's REST
// reference names (https://docs.cloud.google.com/build/docs/api/reference/rest,
// read 2026-11-20), checked on the request's project ("on the specified
// resource projectId"), or its parent or name where the locations form
// sets one; UpdateWorkerPool's from "Cloud Build audit logging"
// (https://docs.cloud.google.com/build/docs/securing-builds/audit-logs),
// as its page names none. Triggers' deletes, updates and runs are
// cloudbuild.builds.create, and a cancel cloudbuild.builds.update, as
// their pages name them. RetryBuild's page names "one or more of"
// cloudbuild.builds.get and cloudbuild.builds.create: cloudbuild.builds.create
// is checked, as the retry creates a build (UNVERIFIED which one Google
// checks, re-read 2026-11-20). Not checked: ReceiveTriggerWebhook, which
// the trigger's webhook secret authorizes, not IAM ("You'll need a secret
// to authenticate incoming webhook events",
// https://docs.cloud.google.com/build/docs/automate-builds-webhook-events,
// re-read 2026-11-20). Cloud Build
// keeps no resource-level policies on these resources. Google's wording
// of a denial is UNVERIFIED (re-read 2026-11-20: the pages give none): the
// newer APIs'.
var CloudBuild = &Service{
	Name: "cloudbuild.googleapis.com",
	Types: map[string]string{"builds": "cloudbuild.googleapis.com/Build", "triggers": "cloudbuild.googleapis.com/BuildTrigger",
		"workerPools": "cloudbuild.googleapis.com/WorkerPool", "connections": "cloudbuild.googleapis.com/Connection",
		"repositories": "cloudbuild.googleapis.com/Repository"},
	Methods: rows(map[string]map[string][2]string{
		"/google.devtools.cloudbuild.v1.CloudBuild/": {
			"CreateBuild":              {"cloudbuild.builds.create", "parent|project_id"},
			"GetBuild":                 {"cloudbuild.builds.get", "name|project_id"},
			"ListBuilds":               {"cloudbuild.builds.list", "parent|project_id"},
			"CancelBuild":              {"cloudbuild.builds.update", "name|project_id"},
			"RetryBuild":               {"cloudbuild.builds.create", "name|project_id"},
			"ApproveBuild":             {"cloudbuild.builds.approve", "name"},
			"CreateBuildTrigger":       {"cloudbuild.builds.create", "parent|project_id"},
			"GetBuildTrigger":          {"cloudbuild.builds.get", "name|project_id"},
			"ListBuildTriggers":        {"cloudbuild.builds.list", "parent|project_id"},
			"DeleteBuildTrigger":       {"cloudbuild.builds.create", "name|project_id"},
			"UpdateBuildTrigger":       {"cloudbuild.builds.create", "trigger.resource_name|project_id"},
			"RunBuildTrigger":          {"cloudbuild.builds.create", "name|project_id"},
			"CreateWorkerPool":         {"cloudbuild.workerpools.create", "parent"},
			"GetWorkerPool":            {"cloudbuild.workerpools.get", "name"},
			"DeleteWorkerPool":         {"cloudbuild.workerpools.delete", "name"},
			"UpdateWorkerPool":         {"cloudbuild.workerpools.update", "worker_pool.name"},
			"ListWorkerPools":          {"cloudbuild.workerpools.list", "parent"},
			"GetDefaultServiceAccount": {"cloudbuild.builds.get", "name"},
		},
		// Cloud Build repositories, 2nd gen (#2197): the permissions
		// https://docs.cloud.google.com/iam/docs/roles-permissions/cloudbuild
		// lists (read 2026-11-23), on the resource each method names.
		"/google.devtools.cloudbuild.v2.RepositoryManager/": {
			"CreateConnection":          {"cloudbuild.connections.create", "parent"},
			"GetConnection":             {"cloudbuild.connections.get", "name"},
			"ListConnections":           {"cloudbuild.connections.list", "parent"},
			"UpdateConnection":          {"cloudbuild.connections.update", "connection.name"},
			"DeleteConnection":          {"cloudbuild.connections.delete", "name"},
			"FetchLinkableRepositories": {"cloudbuild.connections.fetchLinkableRepositories", "connection"},
			"CreateRepository":          {"cloudbuild.repositories.create", "parent"},
			"BatchCreateRepositories":   {"cloudbuild.repositories.create", "parent"},
			"GetRepository":             {"cloudbuild.repositories.get", "name"},
			"ListRepositories":          {"cloudbuild.repositories.list", "parent"},
			"DeleteRepository":          {"cloudbuild.repositories.delete", "name"},
			"FetchReadToken":            {"cloudbuild.repositories.accessReadToken", "repository"},
			"FetchReadWriteToken":       {"cloudbuild.repositories.accessReadWriteToken", "repository"},
			"FetchGitRefs":              {"cloudbuild.repositories.fetchGitRefs", "repository"},
		},
	}),
	Deny: newerDenyOf,
}
