package iameval

import (
	"context"
	"maps"
	"regexp"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
)

// The twelfth round's source tools (#1879): Secure Source Manager,
// Developer Connect and Container Analysis, all on the Cloud Run port,
// over gRPC and JSON.

// ssmMethods are Secure Source Manager's rows (the permission index
// spells issue and pull request comments issuecomments and prcomments).
func ssmMethods() map[string]Method {
	p := "securesourcemanager."
	r := map[string][2]string{
		"CreateInstance": {p + "instances.create", "parent"},
		"GetInstance":    {p + "instances.get", "name"},
		"ListInstances":  {p + "instances.list", "parent"},
		"DeleteInstance": {p + "instances.delete", "name"},

		"CreateRepository": {p + "repositories.create", "parent"},
		"GetRepository":    {p + "repositories.get", "name"},
		"ListRepositories": {p + "repositories.list", "parent"},
		"UpdateRepository": {p + "repositories.update", "repository.name"},
		"DeleteRepository": {p + "repositories.delete", "name"},
		"FetchTree":        {p + "repositories.fetch", "repository"},
		"FetchBlob":        {p + "repositories.fetch", "repository"},
		"FetchRefs":        {p + "repositories.fetch", "repository"},
		"GetIamPolicyRepo": {p + "repositories.getIamPolicy", "resource"},
		"SetIamPolicyRepo": {p + "repositories.setIamPolicy", "resource"},

		"CreateBranchRule": {p + "branchRules.create", "parent"},
		"GetBranchRule":    {p + "branchRules.get", "name"},
		"ListBranchRules":  {p + "branchRules.list", "parent"},
		"UpdateBranchRule": {p + "branchRules.update", "branch_rule.name"},
		"DeleteBranchRule": {p + "branchRules.delete", "name"},

		"CreateHook": {p + "hooks.create", "parent"},
		"GetHook":    {p + "hooks.get", "name"},
		"ListHooks":  {p + "hooks.list", "parent"},
		"UpdateHook": {p + "hooks.update", "hook.name"},
		"DeleteHook": {p + "hooks.delete", "name"},

		"CreateIssue": {p + "issues.create", "parent"},
		"GetIssue":    {p + "issues.get", "name"},
		"ListIssues":  {p + "issues.list", "parent"},
		"UpdateIssue": {p + "issues.update", "issue.name"},
		"DeleteIssue": {p + "issues.delete", "name"},
		"OpenIssue":   {p + "issues.open", "name"},
		"CloseIssue":  {p + "issues.close", "name"},

		"CreateIssueComment": {p + "issuecomments.create", "parent"},
		"GetIssueComment":    {p + "issuecomments.get", "name"},
		"ListIssueComments":  {p + "issuecomments.list", "parent"},
		"UpdateIssueComment": {p + "issuecomments.update", "issue_comment.name"},
		"DeleteIssueComment": {p + "issuecomments.delete", "name"},

		"CreatePullRequest":        {p + "pullRequests.create", "parent"},
		"GetPullRequest":           {p + "pullRequests.get", "name"},
		"ListPullRequests":         {p + "pullRequests.list", "parent"},
		"UpdatePullRequest":        {p + "pullRequests.update", "pull_request.name"},
		"MergePullRequest":         {p + "pullRequests.merge", "name"},
		"OpenPullRequest":          {p + "pullRequests.open", "name"},
		"ClosePullRequest":         {p + "pullRequests.close", "name"},
		"ListPullRequestFileDiffs": {p + "pullRequests.listFileDiffs", "name"},

		"CreatePullRequestComment":       {p + "prcomments.create", "parent"},
		"BatchCreatePullRequestComments": {p + "prcomments.create", "parent"},
		"GetPullRequestComment":          {p + "prcomments.get", "name"},
		"ListPullRequestComments":        {p + "prcomments.list", "parent"},
		"UpdatePullRequestComment":       {p + "prcomments.update", "pull_request_comment.name"},
		"DeletePullRequestComment":       {p + "prcomments.delete", "name"},
		"ResolvePullRequestComments":     {p + "prcomments.resolve", "parent"},
		"UnresolvePullRequestComments":   {p + "prcomments.unresolve", "parent"},
	}
	return sm(r, "/google.cloud.securesourcemanager.v1.SecureSourceManager/")
}

// SecureSourceManager is Secure Source Manager's table: the permission
// each method's REST reference names, on the resource it names
// (https://docs.cloud.google.com/secure-source-manager/docs/reference/rest,
// read 2026-11-20): securesourcemanager.COLLECTION.VERB, a patch's
// .update, fetchTree, fetchBlob and fetchRefs
// securesourcemanager.repositories.fetch, a pull request comment's
// batchCreate .prcomments.create and resolve and unresolve
// .prcomments.resolve and .unresolve, on the pull request. Instances and
// repositories keep their own policies; "Access control with IAM"
// (https://docs.cloud.google.com/secure-source-manager/docs/access-control)
// grants an instance's roles on the instance and a repository's "granted
// separately" on the repository, so an instance's policy does not apply
// to its repositories (a repository's name does not hold its instance's).
// TestIamPermissionsRepo and an instance's testIamPermissions need none
// ("IAM permission checks are not required on this method.") and answer
// what the caller holds. The Operations and Locations mixins name no
// permission (their pages give OAuth scopes only) and are not checked.
// An instance's IAMPolicy mixin is served over JSON alone (the port's
// gRPC IAMPolicy is Cloud Functions'): SecureSourceManagerJSON. Google's
// wording of a denial is UNVERIFIED (re-read 2026-11-20: the pages give
// none): the newer APIs'.
var SecureSourceManager = &Service{
	Name:              "securesourcemanager.googleapis.com",
	PolicyCollections: []string{"instances", "repositories"},
	Types: map[string]string{"instances": "securesourcemanager.googleapis.com/Instance",
		"repositories": "securesourcemanager.googleapis.com/Repository"},
	Methods: ssmMethods(),
	Deny:    newerDenyOf,
}

// SecureSourceManagerJSON is SecureSourceManager's table over JSON, where
// an instance's IAMPolicy mixin is its own: getIamPolicy and setIamPolicy
// as the instances' REST pages name them,
// securesourcemanager.instances.getIamPolicy and .setIamPolicy.
var SecureSourceManagerJSON = func() *Service {
	s := *SecureSourceManager
	s.Methods = maps.Clone(SecureSourceManager.Methods)
	s.Methods["/google.iam.v1.IAMPolicy/GetIamPolicy"] = Method{Permission: "securesourcemanager.instances.getIamPolicy", Field: "resource"}
	s.Methods["/google.iam.v1.IAMPolicy/SetIamPolicy"] = Method{Permission: "securesourcemanager.instances.setIamPolicy", Field: "resource"}
	return &s
}()

// SSMTestIamPermissionsRepo is the repositories' TestIamPermissions,
// answered with what the caller holds.
const SSMTestIamPermissionsRepo = "/google.cloud.securesourcemanager.v1.SecureSourceManager/TestIamPermissionsRepo"

// SSMTestRepoJSON is TestIamPermissionsRepo's JSON path, without the
// port's /securesourcemanager prefix; its group is the repository.
var SSMTestRepoJSON = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/repositories/[^/:]+):testIamPermissions$`)

// DeveloperConnect is Developer Connect's table: the permission each
// method's REST reference names
// (https://docs.cloud.google.com/developer-connect/docs/api/reference/rest,
// read 2026-11-20): developerconnect.connections.* and
// developerconnect.gitRepositoryLinks.*, on parent for a list or a create
// and on the resource otherwise; fetchLinkableGitRepositories on the
// connection, fetchGitRefs, fetchReadToken and fetchReadWriteToken on the
// link. Its operations are developerconnect.operations.get, .list, .cancel
// and .delete (projects.locations.operations' pages), checked by the port's
// Operations mixin (RunPortOperation) and DeveloperConnectOpsJSON. It keeps
// no resource-level policies. Google's wording of a denial is UNVERIFIED
// (re-read 2026-11-20: the pages give none): the newer APIs'.
var DeveloperConnect = &Service{
	Name: "developerconnect.googleapis.com",
	Types: map[string]string{"connections": "developerconnect.googleapis.com/Connection",
		"gitRepositoryLinks": "developerconnect.googleapis.com/GitRepositoryLink"},
	Methods: sm(map[string][2]string{
		"CreateConnection":             {"developerconnect.connections.create", "parent"},
		"GetConnection":                {"developerconnect.connections.get", "name"},
		"ListConnections":              {"developerconnect.connections.list", "parent"},
		"UpdateConnection":             {"developerconnect.connections.update", "connection.name"},
		"DeleteConnection":             {"developerconnect.connections.delete", "name"},
		"FetchLinkableGitRepositories": {"developerconnect.connections.fetchLinkableGitRepositories", "connection"},
		"CreateGitRepositoryLink":      {"developerconnect.gitRepositoryLinks.create", "parent"},
		"GetGitRepositoryLink":         {"developerconnect.gitRepositoryLinks.get", "name"},
		"ListGitRepositoryLinks":       {"developerconnect.gitRepositoryLinks.list", "parent"},
		"DeleteGitRepositoryLink":      {"developerconnect.gitRepositoryLinks.delete", "name"},
		"FetchGitRefs":                 {"developerconnect.gitRepositoryLinks.fetchGitRefs", "git_repository_link"},
		"FetchReadToken":               {"developerconnect.gitRepositoryLinks.fetchReadToken", "git_repository_link"},
		"FetchReadWriteToken":          {"developerconnect.gitRepositoryLinks.fetchReadWriteToken", "git_repository_link"},
	}, "/google.cloud.developerconnect.v1.DeveloperConnect/"),
	Deny: newerDenyOf,
}

// DeveloperConnectOpsJSON are Developer Connect's operations over JSON,
// without the port's /developerconnect prefix.
var DeveloperConnectOpsJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+)/operations$`), Permission: "developerconnect.operations.list"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/operations/[^/:]+)$`), Permission: "developerconnect.operations.get"},
	{Method: "DELETE", Pattern: regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/operations/[^/:]+)$`), Permission: "developerconnect.operations.delete"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/operations/[^/:]+):cancel$`), Permission: "developerconnect.operations.cancel"},
}

// caIAM is the ContainerAnalysis service's IAM permission of verb on a
// note or an occurrence, by the resource's collection.
func caIAM(verb string) func(string) string {
	return func(resource string) string {
		if strings.Contains(resource, "/occurrences/") {
			return "containeranalysis.occurrences." + verb
		}
		return "containeranalysis.notes." + verb
	}
}

// ContainerAnalysis is Container Analysis's table, grafeas.v1.Grafeas and
// google.devtools.containeranalysis.v1.ContainerAnalysis. The REST
// reference pages (https://docs.cloud.google.com/artifact-analysis/docs/reference/rest,
// read 2026-11-20) name no permission but for the IAM methods, so each
// method's is the permission of its name in "Container Analysis
// permissions" (https://docs.cloud.google.com/artifact-analysis/docs/ca-access-control):
// containeranalysis.notes.* and containeranalysis.occurrences.*, on parent
// for a list or a create; an occurrence's create (one or a batch) also
// needs containeranalysis.notes.attachOccurrence on its note ("Container
// Analysis Notes Attacher", in the note's project), and a note's
// occurrences listing containeranalysis.notes.listOccurrences on the note.
// GetOccurrenceNote is containeranalysis.occurrences.get on the
// occurrence, and GetVulnerabilityOccurrencesSummary
// containeranalysis.occurrences.list on its parent (UNVERIFIED which
// Google checks, re-read 2026-11-20: no page names one). GetIamPolicy's
// page says it "Requires containeranalysis.notes.setIamPolicy or
// containeranalysis.occurrences.setIamPolicy", as SetIamPolicy's does;
// the permission index's .getIamPolicy, which the Viewer role holds, is
// checked for it (UNVERIFIED). TestIamPermissions answers what the caller
// holds. Notes and occurrences keep their own policies. Google's wording
// of a denial is UNVERIFIED: the newer APIs'.
var ContainerAnalysis = &Service{
	Name:              "containeranalysis.googleapis.com",
	PolicyCollections: []string{"notes", "occurrences"},
	Types: map[string]string{"notes": "containeranalysis.googleapis.com/Note",
		"occurrences": "containeranalysis.googleapis.com/Occurrence"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/grafeas.v1.Grafeas/": {
				"GetNote":                {"containeranalysis.notes.get", "name"},
				"ListNotes":              {"containeranalysis.notes.list", "parent"},
				"CreateNote":             {"containeranalysis.notes.create", "parent"},
				"BatchCreateNotes":       {"containeranalysis.notes.create", "parent"},
				"UpdateNote":             {"containeranalysis.notes.update", "name"},
				"DeleteNote":             {"containeranalysis.notes.delete", "name"},
				"ListNoteOccurrences":    {"containeranalysis.notes.listOccurrences", "name"},
				"GetOccurrence":          {"containeranalysis.occurrences.get", "name"},
				"ListOccurrences":        {"containeranalysis.occurrences.list", "parent"},
				"CreateOccurrence":       {"containeranalysis.occurrences.create", "parent"},
				"BatchCreateOccurrences": {"containeranalysis.occurrences.create", "parent"},
				"UpdateOccurrence":       {"containeranalysis.occurrences.update", "name"},
				"DeleteOccurrence":       {"containeranalysis.occurrences.delete", "name"},
				"GetOccurrenceNote":      {"containeranalysis.occurrences.get", "name"},
			},
			"/google.devtools.containeranalysis.v1.ContainerAnalysis/": {
				"GetVulnerabilityOccurrencesSummary": {"containeranalysis.occurrences.list", "parent"},
			},
		})
		attach := func(full, field string) {
			row := m[full]
			row.Also = []Also{{Field: field, Permission: "containeranalysis.notes.attachOccurrence"}}
			m[full] = row
		}
		attach("/grafeas.v1.Grafeas/CreateOccurrence", "occurrence.note_name")
		attach("/grafeas.v1.Grafeas/BatchCreateOccurrences", "occurrences.note_name")
		ca := "/google.devtools.containeranalysis.v1.ContainerAnalysis/"
		m[ca+"GetIamPolicy"] = Method{Field: "resource", PermissionFor: caIAM("getIamPolicy")}
		m[ca+"SetIamPolicy"] = Method{Field: "resource", PermissionFor: caIAM("setIamPolicy")}
		return m
	}(),
	Deny: newerDenyOf,
}

// TestPermissionsGRPC answers a TestIamPermissions-shaped call (one with
// another name, Secure Source Manager's TestIamPermissionsRepo) with the
// requested permissions the caller holds.
func (e *Enforcer) TestPermissionsGRPC(ctx context.Context, svc *Service, req any, handler grpc.UnaryHandler) (any, error) {
	if e == nil || e.On == nil || !e.On() {
		return handler(ctx, req)
	}
	if _, ok := req.(*iampb.TestIamPermissionsRequest); !ok {
		return handler(ctx, req)
	}
	return e.testPermissions(ctx, svc, req, handler)
}

func init() {
	Services["google.cloud.securesourcemanager.v1.SecureSourceManager"] = SecureSourceManager
	Services["google.cloud.developerconnect.v1.DeveloperConnect"] = DeveloperConnect
	Services["grafeas.v1.Grafeas"] = ContainerAnalysis
	Services["google.devtools.containeranalysis.v1.ContainerAnalysis"] = ContainerAnalysis
}
