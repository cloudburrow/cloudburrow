package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// dataformProvider is the console's Dataform page under BigQuery (#2105),
// laid out as Google's (docs.cloud.google.com/dataform/docs/quickstart-create-workflow,
// release-configurations, workflow-configurations, connect-repository,
// read 2026-11-21): the project's repositories in every region with
// Create repository (Repository ID, Region, Service account); a
// repository's page with its Development workspaces, Releases &
// Scheduling (release configurations and workflow configurations, each
// with its Frequency and Time zone), Workflow Execution Logs, Settings
// (workspace compilation overrides) and Git connection (Connect with Git);
// a workspace's editor with its Files, the compiled graph and its errors,
// and Compile, Start execution, Commit, Push and Pull; a file's page with
// its SQLX, Edit (Save), Rename and Delete; a release or workflow
// configuration's page with its scheduled and manual results; and an
// invocation's actions with their states and SQL, Cancel and Delete.
//
// Paths: [repository], [repository, "workspaces", ID],
// [repository, "workspaces", ID, "files", file path],
// [repository, "releases", ID], [repository, "workflows", ID],
// [repository, "invocations", ID].
type dataformProvider struct {
	svc *dataformService
}

const dataformNotStarted = "Dataform has not started"

func (dataformProvider) ID() string    { return "dataform" }
func (dataformProvider) Title() string { return "Dataform" }

func (p dataformProvider) api() (*dataform.Server, error) {
	s := p.svc.API()
	if s == nil {
		return nil, errors.New(dataformNotStarted)
	}
	return s, nil
}

func (p dataformProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Region", "Git remote", "Service account", "Created"}, NameColumn: "Repository", Noun: "repositories"}
	if project == "" {
		listing.Prompt = "Dataform lists repositories per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataform.Locations() {
		resp, err := s.ListRepositories(ctx, &dfpb.ListRepositoriesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, r := range resp.GetRepositories() {
			// Studio's code assets are hidden here, as Google hides them
			// (consolebigqueryassets.go).
			if dataformIsAsset(r) {
				continue
			}
			listing.Items = append(listing.Items, console.Resource{Name: r.GetName(), Fields: map[string]string{
				"Region": kmsLocation(r.GetName()), "Git remote": kmsOrDash(r.GetGitRemoteSettings().GetUrl()),
				"Service account": dataformServiceAccount(r.GetServiceAccount()), "Created": kmsTime(r.GetCreateTime())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func dataformServiceAccount(sa string) string {
	if sa == "" {
		return "Default Dataform service agent"
	}
	return sa
}

func (p dataformProvider) CreateForm() (string, []console.Field) {
	return "Create repository", []console.Field{
		{Name: "repositoryId", Label: "Repository ID", Type: "text", Required: true, Pattern: `^[a-zA-Z_0-9\-]{1,63}$`,
			Help: "Letters, numbers, hyphens and underscores; at most 63 characters."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: dataform.Locations()},
		{Name: "serviceAccount", Label: "Service account", Type: "text",
			Help: "The service account workflow executions run as. Empty is the default Dataform service agent."},
		{Name: "kmsKeyName", Label: "Cloud KMS key", Type: "text", Browse: "kms",
			Pattern: `^(projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+)?$`,
			Help:    "Optional: a key of this instance's Cloud KMS in the repository's region, which encrypts its code."},
	}
}

func (p dataformProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a repository")
	}
	r := &dfpb.Repository{ServiceAccount: strings.TrimSpace(values["serviceAccount"]), KmsKeyName: strings.TrimSpace(values["kmsKeyName"])}
	out, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["region"]),
		RepositoryId: strings.TrimSpace(values["repositoryId"]), Repository: r})
	if err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p dataformProvider) repository(ctx context.Context, project, name string) (*dataform.Server, *dfpb.Repository, error) {
	s, err := p.api()
	if err != nil {
		return nil, nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, nil, err
	}
	r, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: name})
	return s, r, err
}

// dataformChild is a child resource's name: the repository's, a
// collection and an ID that names nothing deeper.
func dataformChild(repo, collection, id string) (string, error) {
	if id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("%q is not an ID", id)
	}
	return repo + "/" + collection + "/" + id, nil
}

func (p dataformProvider) Detail(ctx context.Context, project string, pth []string) (console.Detail, error) {
	s, repo, err := p.repository(ctx, project, pth[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the repository: " + consoleErr(err)}, nil
	}
	if len(pth) == 1 {
		return p.repositoryDetail(ctx, s, repo), nil
	}
	if len(pth) < 3 {
		return console.DeeperThan(1, pth), nil
	}
	if pth[1] == "code" && len(pth) == 3 {
		// A file of the repository's code at its head, read without a
		// workspace (ReadRepositoryFile, #2094).
		d := dataformCodeFile(ctx, s, repo.GetName(), pth[2])
		if d.Unavailable == "" {
			d.Title = pth[2]
			d.Trail = []console.Crumb{{Label: lastSegment(repo.GetName()), Path: pth[:1]}, {Label: pth[2]}}
		}
		return d, nil
	}
	name, err := dataformChild(repo.GetName(), pth[1], pth[2])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	trail := []console.Crumb{{Label: lastSegment(repo.GetName()), Path: pth[:1]}}
	var d console.Detail
	switch {
	case pth[1] == "workspaces" && len(pth) == 3:
		d = p.workspaceDetail(ctx, s, repo, name)
	case pth[1] == "workspaces" && len(pth) == 5 && pth[3] == "files":
		d = p.fileDetail(ctx, s, name, pth[4])
		trail = append(trail, console.Crumb{Label: pth[2], Path: pth[:3]})
		d.Title = pth[4]
	case pth[1] == "releases" && len(pth) == 3:
		d = p.releaseDetail(ctx, s, dataformCollection(name))
	case pth[1] == "workflows" && len(pth) == 3:
		d = p.workflowDetail(ctx, s, dataformCollection(name))
	case pth[1] == "invocations" && len(pth) == 3:
		d = p.invocationDetail(ctx, s, dataformCollection(name))
	default:
		return console.Detail{Unavailable: "this repository has nothing at " + strings.Join(pth[1:], "/")}, nil
	}
	if d.Unavailable == "" {
		label := pth[len(pth)-1]
		d.Trail = append(trail, console.Crumb{Label: label})
	}
	return d, nil
}

func (p dataformProvider) repositoryDetail(ctx context.Context, s *dataform.Server, repo *dfpb.Repository) console.Detail {
	name := repo.GetName()
	workspaces := console.Listing{Columns: []string{"Created"}, NameColumn: "Workspace", Noun: "development workspaces"}
	if resp, err := s.ListWorkspaces(ctx, &dfpb.ListWorkspacesRequest{Parent: name, PageSize: 1000}); err == nil {
		for _, w := range resp.GetWorkspaces() {
			id := lastSegment(w.GetName())
			workspaces.Items = append(workspaces.Items, console.Resource{Name: id, Opens: []string{name, "workspaces", id},
				Target: []string{name, "workspaces", id}, Fields: map[string]string{"Created": kmsTime(w.GetCreateTime())}})
		}
	} else {
		workspaces.Unavailable = consoleErr(err)
	}
	workspaces.Total = len(workspaces.Items)

	releases := console.Listing{Columns: []string{"Git commitish", "Frequency", "Time zone", "Latest compilation", "State"}, NameColumn: "Release ID", Noun: "release configurations"}
	if resp, err := s.ListReleaseConfigs(ctx, &dfpb.ListReleaseConfigsRequest{Parent: name, PageSize: 1000}); err == nil {
		for _, rc := range resp.GetReleaseConfigs() {
			id := lastSegment(rc.GetName())
			releases.Items = append(releases.Items, console.Resource{Name: id, Opens: []string{name, "releases", id}, Fields: map[string]string{
				"Git commitish": rc.GetGitCommitish(), "Frequency": kmsOrDash(rc.GetCronSchedule()), "Time zone": dataformTimeZone(rc.GetTimeZone()),
				"Latest compilation": kmsOrDash(lastSegment(rc.GetReleaseCompilationResult())), "State": dataformEnabledText(rc.GetDisabled())}})
		}
	} else {
		releases.Unavailable = consoleErr(err)
	}
	releases.Total = len(releases.Items)

	workflows := console.Listing{Columns: []string{"Release configuration", "Frequency", "Time zone", "State"}, NameColumn: "Config ID", Noun: "workflow configurations"}
	if resp, err := s.ListWorkflowConfigs(ctx, &dfpb.ListWorkflowConfigsRequest{Parent: name, PageSize: 1000}); err == nil {
		for _, wc := range resp.GetWorkflowConfigs() {
			id := lastSegment(wc.GetName())
			workflows.Items = append(workflows.Items, console.Resource{Name: id, Opens: []string{name, "workflows", id}, Fields: map[string]string{
				"Release configuration": lastSegment(wc.GetReleaseConfig()), "Frequency": kmsOrDash(wc.GetCronSchedule()),
				"Time zone": dataformTimeZone(wc.GetTimeZone()), "State": dataformEnabledText(wc.GetDisabled())}})
		}
	} else {
		workflows.Unavailable = consoleErr(err)
	}
	workflows.Total = len(workflows.Items)

	logs := console.Listing{Columns: []string{"State", "Source", "Start time", "End time"}, NameColumn: "Invocation", Noun: "workflow executions"}
	if resp, err := s.ListWorkflowInvocations(ctx, &dfpb.ListWorkflowInvocationsRequest{Parent: name, PageSize: 1000}); err == nil {
		invs := resp.GetWorkflowInvocations()
		sort.SliceStable(invs, func(i, j int) bool {
			return invs[i].GetInvocationTiming().GetStartTime().AsTime().After(invs[j].GetInvocationTiming().GetStartTime().AsTime())
		})
		for _, inv := range invs {
			id := lastSegment(inv.GetName())
			st := dataformState(inv.GetState().String())
			logs.Items = append(logs.Items, console.Resource{Name: id, Status: st, Opens: []string{name, "invocations", id}, Fields: map[string]string{
				"State": st, "Source": dataformInvocationSource(inv), "Start time": kmsTime(inv.GetInvocationTiming().GetStartTime()),
				"End time": kmsTime(inv.GetInvocationTiming().GetEndTime())}})
		}
	} else {
		logs.Unavailable = consoleErr(err)
	}
	logs.Total = len(logs.Items)

	ov := repo.GetWorkspaceCompilationOverrides()
	git := repo.GetGitRemoteSettings()
	gitGroup := console.PropertyGroup{Heading: "Git connection", Properties: []console.Property{{Label: "Remote", Value: "Not connected: the repository keeps its code in Dataform's own Git repository."}}}
	if git != nil {
		gitGroup.Properties = []console.Property{{Label: "Remote Git repository URL", Value: git.GetUrl()},
			{Label: "Default remote branch name", Value: git.GetDefaultBranch()},
			{Label: "Secret", Value: git.GetAuthenticationTokenSecretVersion()},
			{Label: "Token status", Value: dataformTokenStatus(ctx, s, name)}}
	}
	gitActions := []console.Action{{ID: "connectgit", Label: "Connect with Git"}}
	if git != nil {
		gitActions = []console.Action{{ID: "connectgit", Label: "Edit Git connection"}, {ID: "disconnectgit", Label: "Disconnect", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "The repository goes back to Dataform's own Git repository; the remote keeps its commits."}}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: kmsLocation(name)}, {Label: "Service account", Value: dataformServiceAccount(repo.GetServiceAccount())}},
		Sections: []console.Section{
			{ID: "workspaces", Label: "Development workspaces", Listing: workspaces, Actions: []console.Action{p.createWorkspaceAction()}},
			{ID: "releases", Label: "Release configurations", Group: "Releases & Scheduling", Listing: releases,
				Actions: []console.Action{p.releaseAction("createrelease", "Create release configuration", nil)}},
			{ID: "workflows", Label: "Workflow configurations", Group: "Releases & Scheduling", Listing: workflows,
				Actions: []console.Action{p.workflowAction(ctx, s, name, "createworkflow", "Create workflow configuration", nil)}},
			{ID: "logs", Label: "Workflow Execution Logs", Listing: logs},
			{ID: "settings", Label: "Settings", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Repository", Properties: []console.Property{{Label: "Resource name", Value: name},
					{Label: "Service account", Value: dataformServiceAccount(repo.GetServiceAccount())},
					{Label: "Encryption", Value: dataformEncryption(repo.GetKmsKeyName())},
					{Label: "Created", Value: kmsTime(repo.GetCreateTime())}}},
				{Heading: "Workspace compilation overrides", Properties: []console.Property{
					{Label: "Google Cloud Project ID", Value: kmsOrDash(ov.GetDefaultDatabase())},
					{Label: "Schema suffix", Value: kmsOrDash(ov.GetSchemaSuffix())},
					{Label: "Table prefix", Value: kmsOrDash(ov.GetTablePrefix())}}}},
				Actions: []console.Action{{ID: "settings", Label: "Edit"}}},
			{ID: "git", Label: "Git connection", Kind: console.KindProperties, Groups: []console.PropertyGroup{gitGroup}, Actions: gitActions},
			dataformCode(ctx, s, name),
			{ID: "permissions", Kind: console.KindPermissions},
		},
	}
}

// dataformTokenStatus is ComputeRepositoryAccessTokenStatus's answer.
func dataformTokenStatus(ctx context.Context, s *dataform.Server, repo string) string {
	resp, err := s.ComputeRepositoryAccessTokenStatus(ctx, &dfpb.ComputeRepositoryAccessTokenStatusRequest{Name: repo})
	if err != nil {
		return "Cannot check: " + consoleErr(err)
	}
	return dataformState(resp.GetTokenStatus().String())
}

func dataformEncryption(key string) string {
	if key == "" {
		return "Google-managed"
	}
	return "Customer-managed: " + key
}

func dataformTimeZone(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

func dataformEnabledText(disabled bool) string {
	if disabled {
		return "Disabled"
	}
	return "Enabled"
}

// dataformState is an API state word as the console shows it.
func dataformState(s string) string {
	if s == "" || strings.HasSuffix(s, "UNSPECIFIED") {
		return "—"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func dataformInvocationSource(inv *dfpb.WorkflowInvocation) string {
	if wc := inv.GetWorkflowConfig(); wc != "" {
		return "Workflow configuration " + lastSegment(wc)
	}
	return "Compilation result " + lastSegment(inv.GetCompilationResult())
}

func (dataformProvider) createWorkspaceAction() console.Action {
	return console.Action{ID: "createworkspace", Label: "Create development workspace", Submit: "Create", Fields: []console.Field{
		{Name: "workspaceId", Label: "Workspace ID", Type: "text", Required: true, Pattern: `^[a-zA-Z_0-9\-]{1,63}$`,
			Help: "Letters, numbers, hyphens and underscores; at most 63 characters."}}}
}

func (dataformProvider) releaseAction(id, label string, rc *dfpb.ReleaseConfig) console.Action {
	cfg := rc.GetCodeCompilationConfig()
	fields := []console.Field{}
	if rc == nil {
		fields = append(fields, console.Field{Name: "releaseId", Label: "Release ID", Type: "text", Required: true, Section: "Release settings",
			Pattern: `^[a-zA-Z_0-9\-]{1,63}$`})
	}
	commitish := rc.GetGitCommitish()
	if commitish == "" {
		commitish = "main"
	}
	fields = append(fields,
		console.Field{Name: "gitCommitish", Label: "Git commitish", Type: "text", Required: true, Default: commitish, Section: "Release settings",
			Help: "A branch, a tag or a commit SHA."},
		console.Field{Name: "cronSchedule", Label: "Frequency", Type: "text", Default: rc.GetCronSchedule(), Section: "Release settings",
			Help: "A cron schedule, such as 0 * * * *; empty compiles only on demand."},
		console.Field{Name: "timeZone", Label: "Time zone", Type: "text", Default: rc.GetTimeZone(), Section: "Release settings",
			Help: "An IANA time zone, such as Europe/Paris; empty is UTC."},
		console.Field{Name: "defaultDatabase", Label: "Google Cloud Project ID", Type: "text", Default: cfg.GetDefaultDatabase(), Section: "Compilation overrides"},
		console.Field{Name: "schemaSuffix", Label: "Schema suffix", Type: "text", Default: cfg.GetSchemaSuffix(), Section: "Compilation overrides"},
		console.Field{Name: "tablePrefix", Label: "Table prefix", Type: "text", Default: cfg.GetTablePrefix(), Section: "Compilation overrides"},
		console.Field{Name: "vars", Label: "Compilation variables", Type: "map", Default: console.FormatMap(cfg.GetVars()), Section: "Compilation variables",
			Help: "Key and value pairs, read in SQLX as dataform.projectConfig.vars."},
	)
	if rc != nil {
		fields = append(fields, console.Field{Name: "disabled", Label: "Disable this release configuration", Type: "checkbox",
			Default: strconv.FormatBool(rc.GetDisabled()), Section: "Release settings"})
	}
	submit := "Create"
	if rc != nil {
		submit = "Save"
	}
	return console.Action{ID: id, Label: label, Submit: submit, Fields: fields}
}

func (dataformProvider) workflowAction(ctx context.Context, s *dataform.Server, repo, id, label string, wc *dfpb.WorkflowConfig) console.Action {
	var releases []string
	if resp, err := s.ListReleaseConfigs(ctx, &dfpb.ListReleaseConfigsRequest{Parent: repo, PageSize: 1000}); err == nil {
		for _, rc := range resp.GetReleaseConfigs() {
			releases = append(releases, lastSegment(rc.GetName()))
		}
	}
	ic := wc.GetInvocationConfig()
	fields := []console.Field{}
	if wc == nil {
		fields = append(fields, console.Field{Name: "workflowId", Label: "Config ID", Type: "text", Required: true, Pattern: `^[a-zA-Z_0-9\-]{1,63}$`})
	}
	fields = append(fields,
		console.Field{Name: "releaseConfig", Label: "Release configuration", Type: "select", Required: true, Options: releases,
			Default: lastSegment(wc.GetReleaseConfig()), Help: "The release whose latest compilation result each execution runs."},
		console.Field{Name: "serviceAccount", Label: "Service account", Type: "text", Default: ic.GetServiceAccount()},
		console.Field{Name: "cronSchedule", Label: "Frequency", Type: "text", Default: wc.GetCronSchedule(), Help: "A cron schedule; empty runs only on demand."},
		console.Field{Name: "timeZone", Label: "Time zone", Type: "text", Default: wc.GetTimeZone(), Help: "An IANA time zone; empty is UTC."},
	)
	fields = append(fields, dataformSelectionFields(ic)...)
	submit := "Create"
	if wc != nil {
		submit = "Save"
		fields = append(fields, console.Field{Name: "disabled", Label: "Disable this workflow configuration", Type: "checkbox", Default: strconv.FormatBool(wc.GetDisabled())})
	}
	return console.Action{ID: id, Label: label, Submit: submit, Fields: fields}
}

// dataformSelectionFields are Google's Start execution choices: All
// actions, Selection of actions or Selection of tags, Include
// dependencies, Include dependents and Run with full refresh.
func dataformSelectionFields(ic *dfpb.InvocationConfig) []console.Field {
	scope, actions := "all", []string{}
	for _, t := range ic.GetIncludedTargets() {
		actions = append(actions, dataformTarget(t))
	}
	if len(actions) > 0 {
		scope = "actions"
	} else if len(ic.GetIncludedTags()) > 0 {
		scope = "tags"
	}
	return []console.Field{
		{Name: "scope", Label: "Actions", Type: "radio", Default: scope, Options: []string{"all", "actions", "tags"},
			OptionLabels: map[string]string{"all": "All actions", "actions": "Selection of actions", "tags": "Selection of tags"}},
		{Name: "actions", Label: "Actions to run", Type: "textarea", ShowWhen: "scope=actions", Default: strings.Join(actions, "\n"),
			Help: "One action a line, as dataset.name or project.dataset.name."},
		{Name: "tags", Label: "Tags", Type: "text", ShowWhen: "scope=tags", Default: strings.Join(ic.GetIncludedTags(), ", "), Help: "Comma-separated."},
		{Name: "dependencies", Label: "Include dependencies", Type: "checkbox", Default: strconv.FormatBool(ic.GetTransitiveDependenciesIncluded())},
		{Name: "dependents", Label: "Include dependents", Type: "checkbox", Default: strconv.FormatBool(ic.GetTransitiveDependentsIncluded())},
		{Name: "fullRefresh", Label: "Run with full refresh", Type: "checkbox", Default: strconv.FormatBool(ic.GetFullyRefreshIncrementalTablesEnabled())},
	}
}

func dataformTarget(t *dfpb.Target) string {
	parts := []string{}
	for _, s := range []string{t.GetDatabase(), t.GetSchema(), t.GetName()} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ".")
}

func dataformInvocationConfig(values map[string]string) (*dfpb.InvocationConfig, error) {
	ic := &dfpb.InvocationConfig{ServiceAccount: strings.TrimSpace(values["serviceAccount"]),
		TransitiveDependenciesIncluded: values["dependencies"] == "true", TransitiveDependentsIncluded: values["dependents"] == "true",
		FullyRefreshIncrementalTablesEnabled: values["fullRefresh"] == "true"}
	switch values["scope"] {
	case "actions":
		for _, line := range strings.Split(values["actions"], "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, ".")
			switch len(parts) {
			case 2:
				ic.IncludedTargets = append(ic.IncludedTargets, &dfpb.Target{Schema: parts[0], Name: parts[1]})
			case 3:
				ic.IncludedTargets = append(ic.IncludedTargets, &dfpb.Target{Database: parts[0], Schema: parts[1], Name: parts[2]})
			default:
				return nil, fmt.Errorf("%q is not dataset.name or project.dataset.name", line)
			}
		}
		if len(ic.IncludedTargets) == 0 {
			return nil, fmt.Errorf("name at least one action, or choose All actions")
		}
	case "tags":
		for _, t := range strings.Split(values["tags"], ",") {
			if t = strings.TrimSpace(t); t != "" {
				ic.IncludedTags = append(ic.IncludedTags, t)
			}
		}
		if len(ic.IncludedTags) == 0 {
			return nil, fmt.Errorf("name at least one tag, or choose All actions")
		}
	}
	return ic, nil
}

// dataformFiles lists a workspace's files and directories, depth first.
func dataformFiles(ctx context.Context, s *dataform.Server, ws, dir string, out *[]console.Resource, wsPath []string) error {
	resp, err := s.QueryDirectoryContents(ctx, &dfpb.QueryDirectoryContentsRequest{Workspace: ws, Path: dir, PageSize: 1000})
	if err != nil {
		return err
	}
	entries := resp.GetDirectoryEntries()
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].GetDirectory() != "", entries[j].GetDirectory() != ""
		if di != dj {
			return di
		}
		return entries[i].GetFile()+entries[i].GetDirectory() < entries[j].GetFile()+entries[j].GetDirectory()
	})
	for _, e := range entries {
		if d := e.GetDirectory(); d != "" {
			full := path.Join(dir, path.Base(d))
			*out = append(*out, console.Resource{Name: full + "/", Fields: map[string]string{"Type": "Folder"}})
			if err := dataformFiles(ctx, s, ws, full, out, wsPath); err != nil {
				return err
			}
			continue
		}
		full := path.Join(dir, path.Base(e.GetFile()))
		*out = append(*out, console.Resource{Name: full, Opens: append(append([]string{}, wsPath...), "files", full),
			Fields: map[string]string{"Type": "File"}})
	}
	return nil
}

// latestCompilation is the workspace's newest compilation result.
func latestCompilation(ctx context.Context, s *dataform.Server, repo, ws string) *dfpb.CompilationResult {
	resp, err := s.ListCompilationResults(ctx, &dfpb.ListCompilationResultsRequest{Parent: repo, PageSize: 1000})
	if err != nil {
		return nil
	}
	var latest *dfpb.CompilationResult
	for _, c := range resp.GetCompilationResults() {
		if c.GetWorkspace() != ws {
			continue
		}
		if latest == nil || c.GetCreateTime().AsTime().After(latest.GetCreateTime().AsTime()) {
			latest = c
		}
	}
	return latest
}

func dataformActionType(a *dfpb.CompilationResultAction) string {
	switch {
	case a.GetRelation() != nil:
		return dataformState(strings.TrimPrefix(a.GetRelation().GetRelationType().String(), "RELATION_TYPE_"))
	case a.GetOperations() != nil:
		return "Operations"
	case a.GetAssertion() != nil:
		return "Assertion"
	case a.GetDeclaration() != nil:
		return "Declaration"
	case a.GetNotebook() != nil:
		return "Notebook"
	case a.GetDataPreparation() != nil:
		return "Data preparation"
	}
	return "—"
}

func dataformDependencies(a *dfpb.CompilationResultAction) []*dfpb.Target {
	switch {
	case a.GetRelation() != nil:
		return a.GetRelation().GetDependencyTargets()
	case a.GetOperations() != nil:
		return a.GetOperations().GetDependencyTargets()
	case a.GetAssertion() != nil:
		return a.GetAssertion().GetDependencyTargets()
	case a.GetNotebook() != nil:
		return a.GetNotebook().GetDependencyTargets()
	}
	return nil
}

// dataformGraph is a compilation result's actions (the compiled graph:
// each action, its type, its file and what it depends on) and its errors.
func dataformGraph(ctx context.Context, s *dataform.Server, c *dfpb.CompilationResult) (console.Listing, console.Listing) {
	graph := console.Listing{Columns: []string{"Type", "File", "Depends on"}, NameColumn: "Action", Noun: "actions"}
	errs := console.Listing{Columns: []string{"File", "Action", "Message"}, NameColumn: "Error", Noun: "compilation errors"}
	if c == nil {
		graph.Prompt = "Compile the workspace to see its compiled graph."
		errs.Prompt = "Compile the workspace to see its compilation errors."
		return graph, errs
	}
	if resp, err := s.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: c.GetName(), PageSize: 1000}); err == nil {
		for _, a := range resp.GetCompilationResultActions() {
			deps := []string{}
			for _, d := range dataformDependencies(a) {
				deps = append(deps, dataformTarget(d))
			}
			graph.Items = append(graph.Items, console.Resource{Name: dataformTarget(a.GetTarget()), Fields: map[string]string{
				"Type": dataformActionType(a), "File": a.GetFilePath(), "Depends on": kmsOrDash(strings.Join(deps, ", "))}})
		}
	} else {
		graph.Unavailable = consoleErr(err)
	}
	for i, e := range c.GetCompilationErrors() {
		errs.Items = append(errs.Items, console.Resource{Name: strconv.Itoa(i + 1), Status: "Error", Fields: map[string]string{
			"File": kmsOrDash(e.GetPath()), "Action": kmsOrDash(dataformTarget(e.GetActionTarget())), "Message": e.GetMessage()}})
	}
	graph.Total, errs.Total = len(graph.Items), len(errs.Items)
	if len(graph.Items) == 0 && len(errs.Items) > 0 {
		graph.Note = "The workspace does not compile: see Compilation errors."
	}
	return graph, errs
}

func (p dataformProvider) workspaceDetail(ctx context.Context, s *dataform.Server, repo *dfpb.Repository, ws string) console.Detail {
	w, err := s.GetWorkspace(ctx, &dfpb.GetWorkspaceRequest{Name: ws})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the workspace: " + consoleErr(err)}
	}
	wsPath := []string{repo.GetName(), "workspaces", lastSegment(ws)}
	files := console.Listing{Columns: []string{"Type"}, NameColumn: "Path", Noun: "files"}
	if err := dataformFiles(ctx, s, ws, "", &files.Items, wsPath); err != nil {
		files.Unavailable = consoleErr(err)
	}
	files.Total = len(files.Items)

	changes := console.Listing{Columns: []string{"State"}, NameColumn: "File", Noun: "uncommitted changes"}
	if resp, err := s.FetchFileGitStatuses(ctx, &dfpb.FetchFileGitStatusesRequest{Name: ws}); err == nil {
		for _, c := range resp.GetUncommittedFileChanges() {
			st := dataformState(c.GetState().String())
			changes.Items = append(changes.Items, console.Resource{Name: c.GetPath(), Status: st, Fields: map[string]string{"State": st}})
		}
	} else {
		changes.Unavailable = consoleErr(err)
	}
	changes.Total = len(changes.Items)
	ahead := "—"
	if ab, err := s.FetchGitAheadBehind(ctx, &dfpb.FetchGitAheadBehindRequest{Name: ws}); err == nil {
		ahead = fmt.Sprintf("%d ahead, %d behind", ab.GetCommitsAhead(), ab.GetCommitsBehind())
		changes.Note = "Commits: " + ahead + " the default branch."
	}

	c := latestCompilation(ctx, s, repo.GetName(), ws)
	graph, errs := dataformGraph(ctx, s, c)
	compiled := "Not compiled"
	if c != nil {
		compiled = kmsTime(c.GetCreateTime())
		if len(c.GetCompilationErrors()) > 0 {
			compiled += fmt.Sprintf(" (%d errors)", len(c.GetCompilationErrors()))
		}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Created", Value: kmsTime(w.GetCreateTime())}, {Label: "Git", Value: ahead},
			{Label: "Last compiled", Value: compiled}},
		Sections: []console.Section{
			{ID: "files", Label: "Files", Listing: files, Actions: []console.Action{p.createFileAction(), {ID: "createdir", Label: "Create directory",
				Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true}}},
				{ID: "movedir", Label: "Rename directory", Menu: true, Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true},
					{Name: "newPath", Label: "New path", Type: "text", Required: true}}},
				{ID: "removedir", Label: "Delete directory", Menu: true, Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true}}}}},
			{ID: "graph", Label: "Compiled graph", Listing: graph},
			{ID: "errors", Label: "Compilation errors", Listing: errs},
			{ID: "changes", Label: "Uncommitted changes", Listing: changes},
		},
	}
}

func (dataformProvider) createFileAction() console.Action {
	return console.Action{ID: "createfile", Label: "Create file", Title: "Create new file", Submit: "Create file", Fields: []console.Field{
		{Name: "path", Label: "Add a file path", Type: "text", Required: true, Help: "Such as definitions/my_table.sqlx."},
		{Name: "contents", Label: "Contents", Type: "textarea"}}}
}

// dataformLanguage is how a file's content is highlighted.
func dataformLanguage(p string) string {
	switch path.Ext(p) {
	case ".sqlx", ".sql":
		return "sql"
	case ".js", ".json":
		return "javascript"
	case ".yaml", ".yml":
		return "yaml"
	case ".py":
		return "python"
	}
	return "text"
}

func (p dataformProvider) fileDetail(ctx context.Context, s *dataform.Server, ws, file string) console.Detail {
	resp, err := s.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws, Path: file})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the file: " + consoleErr(err)}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Workspace", Value: lastSegment(ws)}, {Label: "Size", Value: fmt.Sprintf("%d bytes", len(resp.GetFileContents()))}},
		Sections: []console.Section{{ID: "code", Label: "Code", Kind: console.KindText, Text: string(resp.GetFileContents()), Language: dataformLanguage(file)},
			dataformDiff(ctx, s, ws, file)},
	}
}

// dataformDiff is the file's uncommitted changes (FetchFileDiff).
func dataformDiff(ctx context.Context, s *dataform.Server, ws, file string) console.Section {
	sec := console.Section{ID: "diff", Label: "Changes", Kind: console.KindText, Language: "text"}
	resp, err := s.FetchFileDiff(ctx, &dfpb.FetchFileDiffRequest{Workspace: ws, Path: file})
	switch {
	case err != nil:
		sec.Unavailable = "cannot read the file's changes: " + consoleErr(err)
	case resp.GetFormattedDiff() == "":
		sec.Unavailable = "The file has no uncommitted changes."
	default:
		sec.Text = resp.GetFormattedDiff()
	}
	return sec
}

func (p dataformProvider) releaseDetail(ctx context.Context, s *dataform.Server, name string) console.Detail {
	rc, err := s.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the release configuration: " + consoleErr(err)}
	}
	repo := name[:strings.Index(name, "/releaseConfigs/")]
	scheduled := console.Listing{Columns: []string{"Release time", "Result"}, NameColumn: "Compilation result", Noun: "scheduled compilation results"}
	for _, rec := range rc.GetRecentScheduledReleaseRecords() {
		res := "Succeeded"
		if e := rec.GetErrorStatus(); e != nil {
			res = "Failed: " + e.GetMessage()
		}
		scheduled.Items = append(scheduled.Items, console.Resource{Name: kmsOrDash(lastSegment(rec.GetCompilationResult())), Fields: map[string]string{
			"Release time": kmsTime(rec.GetReleaseTime()), "Result": res}})
	}
	scheduled.Total = len(scheduled.Items)
	manual := console.Listing{Columns: []string{"Created", "Commit", "Errors"}, NameColumn: "Compilation result", Noun: "manual/API compilation results"}
	if resp, err := s.ListCompilationResults(ctx, &dfpb.ListCompilationResultsRequest{Parent: repo, PageSize: 1000}); err == nil {
		for _, c := range resp.GetCompilationResults() {
			if c.GetReleaseConfig() != name {
				continue
			}
			manual.Items = append(manual.Items, console.Resource{Name: lastSegment(c.GetName()), Fields: map[string]string{
				"Created": kmsTime(c.GetCreateTime()), "Commit": kmsOrDash(c.GetResolvedGitCommitSha()), "Errors": strconv.Itoa(len(c.GetCompilationErrors()))}})
		}
	}
	manual.Total = len(manual.Items)
	cfg := rc.GetCodeCompilationConfig()
	latest := "—"
	if c := rc.GetReleaseCompilationResult(); c != "" {
		if cr, err := s.GetCompilationResult(ctx, &dfpb.GetCompilationResultRequest{Name: c}); err == nil {
			latest = fmt.Sprintf("%s (commit %s, %d errors)", lastSegment(c), kmsOrDash(cr.GetResolvedGitCommitSha()), len(cr.GetCompilationErrors()))
		}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Git commitish", Value: rc.GetGitCommitish()}, {Label: "Frequency", Value: kmsOrDash(rc.GetCronSchedule())},
			{Label: "Time zone", Value: dataformTimeZone(rc.GetTimeZone())}, {Label: "State", Value: dataformEnabledText(rc.GetDisabled())}},
		Sections: []console.Section{
			{ID: "scheduled", Label: "Scheduled compilation results", Listing: scheduled},
			{ID: "manual", Label: "Manual/API compilation results", Listing: manual},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Compilation overrides", Properties: []console.Property{
				{Label: "Google Cloud Project ID", Value: kmsOrDash(cfg.GetDefaultDatabase())}, {Label: "Schema suffix", Value: kmsOrDash(cfg.GetSchemaSuffix())},
				{Label: "Table prefix", Value: kmsOrDash(cfg.GetTablePrefix())}, {Label: "Compilation variables", Value: kmsOrDash(labelsText(cfg.GetVars()))},
				{Label: "Latest compilation result", Value: latest}}}}},
		},
	}
}

func (p dataformProvider) workflowDetail(ctx context.Context, s *dataform.Server, name string) console.Detail {
	wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the workflow configuration: " + consoleErr(err)}
	}
	repo := name[:strings.Index(name, "/workflowConfigs/")]
	runs := console.Listing{Columns: []string{"Execution time", "Result"}, NameColumn: "Invocation", Noun: "scheduled executions"}
	for _, rec := range wc.GetRecentScheduledExecutionRecords() {
		res, opens := "Started", []string(nil)
		if e := rec.GetErrorStatus(); e != nil {
			res = "Failed: " + e.GetMessage()
		}
		if inv := rec.GetWorkflowInvocation(); inv != "" {
			opens = []string{repo, "invocations", lastSegment(inv)}
		}
		runs.Items = append(runs.Items, console.Resource{Name: kmsOrDash(lastSegment(rec.GetWorkflowInvocation())), Opens: opens,
			Fields: map[string]string{"Execution time": kmsTime(rec.GetExecutionTime()), "Result": res}})
	}
	runs.Total = len(runs.Items)
	ic := wc.GetInvocationConfig()
	sel := "All actions"
	if len(ic.GetIncludedTargets()) > 0 {
		t := []string{}
		for _, x := range ic.GetIncludedTargets() {
			t = append(t, dataformTarget(x))
		}
		sel = strings.Join(t, ", ")
	} else if len(ic.GetIncludedTags()) > 0 {
		sel = "Tags: " + strings.Join(ic.GetIncludedTags(), ", ")
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Release configuration", Value: lastSegment(wc.GetReleaseConfig())},
			{Label: "Frequency", Value: kmsOrDash(wc.GetCronSchedule())}, {Label: "Time zone", Value: dataformTimeZone(wc.GetTimeZone())},
			{Label: "State", Value: dataformEnabledText(wc.GetDisabled())}},
		Sections: []console.Section{
			{ID: "executions", Label: "Scheduled executions", Listing: runs},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Execution options", Properties: []console.Property{
				{Label: "Actions", Value: sel}, {Label: "Include dependencies", Value: strconv.FormatBool(ic.GetTransitiveDependenciesIncluded())},
				{Label: "Include dependents", Value: strconv.FormatBool(ic.GetTransitiveDependentsIncluded())},
				{Label: "Run with full refresh", Value: strconv.FormatBool(ic.GetFullyRefreshIncrementalTablesEnabled())},
				{Label: "Service account", Value: dataformServiceAccount(ic.GetServiceAccount())},
				{Label: "Created", Value: kmsTime(wc.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(wc.GetUpdateTime())}}}}},
		},
	}
}

func (p dataformProvider) invocationDetail(ctx context.Context, s *dataform.Server, name string) console.Detail {
	inv, err := s.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the workflow execution: " + consoleErr(err)}
	}
	actions := console.Listing{Columns: []string{"State", "Start time", "End time", "BigQuery job", "Failure reason"}, NameColumn: "Action", Noun: "actions"}
	var log strings.Builder
	if resp, err := s.QueryWorkflowInvocationActions(ctx, &dfpb.QueryWorkflowInvocationActionsRequest{Name: name, PageSize: 1000}); err == nil {
		for _, a := range resp.GetWorkflowInvocationActions() {
			st := dataformState(a.GetState().String())
			t := dataformTarget(a.GetTarget())
			actions.Items = append(actions.Items, console.Resource{Name: t, Status: st, Fields: map[string]string{
				"State": st, "Start time": kmsTime(a.GetInvocationTiming().GetStartTime()), "End time": kmsTime(a.GetInvocationTiming().GetEndTime()),
				"BigQuery job": kmsOrDash(a.GetBigqueryAction().GetJobId()), "Failure reason": kmsOrDash(a.GetFailureReason())}})
			fmt.Fprintf(&log, "-- %s: %s\n", t, st)
			if a.GetFailureReason() != "" {
				fmt.Fprintf(&log, "-- %s\n", a.GetFailureReason())
			}
			if sql := a.GetBigqueryAction().GetSqlScript(); sql != "" {
				log.WriteString(strings.TrimRight(sql, "\n") + "\n")
			}
			log.WriteString("\n")
		}
	} else {
		actions.Unavailable = consoleErr(err)
	}
	actions.Total = len(actions.Items)
	st := dataformState(inv.GetState().String())
	return console.Detail{
		Summary: []console.Property{{Label: "State", Value: st}, {Label: "Source", Value: dataformInvocationSource(inv)},
			{Label: "Start time", Value: kmsTime(inv.GetInvocationTiming().GetStartTime())}, {Label: "End time", Value: kmsTime(inv.GetInvocationTiming().GetEndTime())}},
		Sections: []console.Section{
			{ID: "actions", Label: "Actions", Listing: actions},
			{ID: "log", Label: "Execution log", Kind: console.KindText, Text: log.String(), Language: "sql"},
		},
	}
}

func (p dataformProvider) DetailActions(ctx context.Context, project string, pth []string) []console.Action {
	s, repo, err := p.repository(ctx, project, pth[0])
	if err != nil {
		return nil
	}
	if len(pth) == 1 {
		acts := []console.Action{p.createWorkspaceAction(), p.releaseAction("createrelease", "Create release configuration", nil),
			p.workflowAction(ctx, s, repo.GetName(), "createworkflow", "Create workflow configuration", nil),
			p.settingsAction(repo), p.connectGitAction(repo)}
		if repo.GetGitRemoteSettings() != nil {
			acts = append(acts, console.Action{ID: "disconnectgit", Label: "Disconnect", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
				Confirm: "The repository goes back to Dataform's own Git repository; the remote keeps its commits."})
		}
		return append(acts, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
			Confirm: "Every development workspace, release configuration, workflow configuration and execution of the repository is deleted with it."})
	}
	if len(pth) < 3 {
		return nil
	}
	name, err := dataformChild(repo.GetName(), pth[1], pth[2])
	if err != nil {
		return nil
	}
	switch {
	case pth[1] == "workspaces" && len(pth) == 3:
		return []console.Action{
			{ID: "compile", Label: "Compile"},
			{ID: "execute", Label: "Start execution", Title: "Execute", Submit: "Start execution", Fields: dataformSelectionFields(nil)},
			{ID: "commit", Label: "Commit", Submit: "Commit", Fields: []console.Field{
				{Name: "message", Label: "Commit message", Type: "textarea", Required: true},
				{Name: "authorName", Label: "Author name", Type: "text", Required: true, Default: "CloudBurrow user"},
				{Name: "authorEmail", Label: "Author email", Type: "text", Required: true, Default: "user@example.com"}}},
			{ID: "push", Label: "Push to default branch"},
			{ID: "pull", Label: "Pull from default branch"},
			{ID: "reset", Label: "Revert all changes", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Every uncommitted change in the workspace is discarded."},
			p.createFileAction(),
			{ID: "createdir", Label: "Create directory", Linked: true, Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true}}},
			{ID: "movedir", Label: "Rename directory", Linked: true, Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true},
				{Name: "newPath", Label: "New path", Type: "text", Required: true}}},
			{ID: "removedir", Label: "Delete directory", Linked: true, Fields: []console.Field{{Name: "path", Label: "Directory path", Type: "text", Required: true}}},
			{ID: "install", Label: "Install packages", Confirm: "npm installs the packages package.json names into the workspace's node_modules."},
			{ID: "deleteworkspace", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The workspace's uncommitted changes are lost."},
		}
	case pth[1] == "workspaces" && len(pth) == 5 && pth[3] == "files":
		resp, err := s.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: name, Path: pth[4]})
		if err != nil {
			return nil
		}
		return []console.Action{
			{ID: "save", Label: "Edit", Title: "Edit " + pth[4], Submit: "Save", Fields: []console.Field{
				{Name: "contents", Label: "Contents", Type: "textarea", Default: string(resp.GetFileContents())}}},
			{ID: "move", Label: "Rename", Fields: []console.Field{{Name: "newPath", Label: "New path", Type: "text", Required: true, Default: pth[4]}}},
			{ID: "deletefile", Label: "Delete", Destructive: true, Leaves: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "The file is removed from the workspace."},
		}
	case pth[1] == "releases" && len(pth) == 3:
		rc, err := s.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: dataformCollection(name)})
		if err != nil {
			return nil
		}
		return []console.Action{{ID: "newcompilation", Label: "New compilation"}, p.releaseAction("editrelease", "Edit", rc),
			{ID: "deleterelease", Label: "Delete", Destructive: true, Leaves: true, Confirm: "Workflow configurations that run this release stop running."}}
	case pth[1] == "workflows" && len(pth) == 3:
		wc, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: dataformCollection(name)})
		if err != nil {
			return nil
		}
		return []console.Action{{ID: "run", Label: "Execute"}, p.workflowAction(ctx, s, repo.GetName(), "editworkflow", "Edit", wc),
			{ID: "deleteworkflow", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The schedule stops; past executions are kept."}}
	case pth[1] == "invocations" && len(pth) == 3:
		inv, err := s.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: dataformCollection(name)})
		if err != nil {
			return nil
		}
		acts := []console.Action{}
		if inv.GetState() == dfpb.WorkflowInvocation_RUNNING {
			acts = append(acts, console.Action{ID: "cancel", Label: "Cancel", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
				Confirm: "The action in progress is stopped and the remaining actions are cancelled."})
		}
		return append(acts, console.Action{ID: "deleteinvocation", Label: "Delete", Destructive: true, Leaves: true, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "The execution's record and its log are deleted; its tables are kept."})
	}
	return nil
}

// dataformCollection maps the console's collection words to the API's.
func dataformCollection(name string) string {
	r := strings.NewReplacer("/releases/", "/releaseConfigs/", "/workflows/", "/workflowConfigs/", "/invocations/", "/workflowInvocations/")
	return r.Replace(name)
}

func (dataformProvider) settingsAction(repo *dfpb.Repository) console.Action {
	ov := repo.GetWorkspaceCompilationOverrides()
	return console.Action{ID: "settings", Label: "Edit", Title: "Workspace compilation overrides", Submit: "Save", Fields: []console.Field{
		{Name: "defaultDatabase", Label: "Google Cloud Project ID", Type: "text", Default: ov.GetDefaultDatabase()},
		{Name: "schemaSuffix", Label: "Schema suffix", Type: "text", Default: ov.GetSchemaSuffix()},
		{Name: "tablePrefix", Label: "Table prefix", Type: "text", Default: ov.GetTablePrefix()}}}
}

func (dataformProvider) connectGitAction(repo *dfpb.Repository) console.Action {
	g := repo.GetGitRemoteSettings()
	branch := g.GetDefaultBranch()
	if branch == "" {
		branch = "main"
	}
	return console.Action{ID: "connectgit", Label: "Connect with Git", Submit: "Link", Fields: []console.Field{
		{Name: "url", Label: "Remote Git repository URL", Type: "text", Required: true, Default: g.GetUrl(),
			Help: "The HTTPS URL of a Secure Source Manager repository of this instance (its git_https URI)."},
		{Name: "branch", Label: "Default remote branch name", Type: "text", Required: true, Default: branch},
		{Name: "secret", Label: "Secret", Type: "text", Required: true, Default: g.GetAuthenticationTokenSecretVersion(),
			Help: "The Secret Manager secret version holding the access token: projects/PROJECT/secrets/SECRET/versions/VERSION."}}}
}

func (p dataformProvider) ActAt(ctx context.Context, project string, pth []string, action string, values map[string]string) error {
	if len(pth) == 0 {
		return fmt.Errorf("no repository")
	}
	s, repo, err := p.repository(ctx, project, pth[0])
	if err != nil {
		return err
	}
	rname := repo.GetName()
	if len(pth) == 1 {
		return p.actOnRepository(ctx, s, repo, action, values)
	}
	if len(pth) < 3 {
		return fmt.Errorf("%s needs a resource of the repository", action)
	}
	name, err := dataformChild(rname, pth[1], pth[2])
	if err != nil {
		return err
	}
	api := dataformCollection(name)
	switch {
	case pth[1] == "workspaces" && len(pth) == 3:
		return p.actOnWorkspace(ctx, s, rname, name, action, values)
	case pth[1] == "workspaces" && len(pth) == 5 && pth[3] == "files":
		switch action {
		case "save":
			_, err := s.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: name, Path: pth[4], Contents: []byte(values["contents"])})
			return err
		case "move":
			_, err := s.MoveFile(ctx, &dfpb.MoveFileRequest{Workspace: name, Path: pth[4], NewPath: strings.TrimSpace(values["newPath"])})
			return err
		case "deletefile":
			_, err := s.RemoveFile(ctx, &dfpb.RemoveFileRequest{Workspace: name, Path: pth[4]})
			return err
		}
	case pth[1] == "releases" && len(pth) == 3:
		switch action {
		case "newcompilation":
			_, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: rname,
				CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_ReleaseConfig{ReleaseConfig: api}}})
			return err
		case "editrelease":
			rc, err := dataformReleaseConfig(values)
			if err != nil {
				return err
			}
			rc.Name = api
			_, err = s.UpdateReleaseConfig(ctx, &dfpb.UpdateReleaseConfigRequest{ReleaseConfig: rc})
			return err
		case "deleterelease":
			_, err := s.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: api})
			return err
		}
	case pth[1] == "workflows" && len(pth) == 3:
		switch action {
		case "run":
			_, err := s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: rname,
				WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_WorkflowConfig{WorkflowConfig: api}}})
			return err
		case "editworkflow":
			wc, err := dataformWorkflowConfig(rname, values)
			if err != nil {
				return err
			}
			wc.Name = api
			_, err = s.UpdateWorkflowConfig(ctx, &dfpb.UpdateWorkflowConfigRequest{WorkflowConfig: wc})
			return err
		case "deleteworkflow":
			_, err := s.DeleteWorkflowConfig(ctx, &dfpb.DeleteWorkflowConfigRequest{Name: api})
			return err
		}
	case pth[1] == "invocations" && len(pth) == 3:
		switch action {
		case "cancel":
			_, err := s.CancelWorkflowInvocation(ctx, &dfpb.CancelWorkflowInvocationRequest{Name: api})
			return err
		case "deleteinvocation":
			_, err := s.DeleteWorkflowInvocation(ctx, &dfpb.DeleteWorkflowInvocationRequest{Name: api})
			return err
		}
	}
	return fmt.Errorf("unknown action %q", action)
}

func dataformReleaseConfig(values map[string]string) (*dfpb.ReleaseConfig, error) {
	vars, err := console.ParseMap(values["vars"])
	if err != nil {
		return nil, err
	}
	return &dfpb.ReleaseConfig{GitCommitish: strings.TrimSpace(values["gitCommitish"]), CronSchedule: strings.TrimSpace(values["cronSchedule"]),
		TimeZone: strings.TrimSpace(values["timeZone"]), Disabled: values["disabled"] == "true",
		CodeCompilationConfig: &dfpb.CodeCompilationConfig{DefaultDatabase: strings.TrimSpace(values["defaultDatabase"]),
			SchemaSuffix: strings.TrimSpace(values["schemaSuffix"]), TablePrefix: strings.TrimSpace(values["tablePrefix"]), Vars: vars}}, nil
}

func dataformWorkflowConfig(repo string, values map[string]string) (*dfpb.WorkflowConfig, error) {
	ic, err := dataformInvocationConfig(values)
	if err != nil {
		return nil, err
	}
	rc := strings.TrimSpace(values["releaseConfig"])
	if rc == "" {
		return nil, fmt.Errorf("choose a release configuration; create one first")
	}
	return &dfpb.WorkflowConfig{ReleaseConfig: repo + "/releaseConfigs/" + rc, InvocationConfig: ic, CronSchedule: strings.TrimSpace(values["cronSchedule"]),
		TimeZone: strings.TrimSpace(values["timeZone"]), Disabled: values["disabled"] == "true"}, nil
}

func (p dataformProvider) actOnRepository(ctx context.Context, s *dataform.Server, repo *dfpb.Repository, action string, values map[string]string) error {
	name := repo.GetName()
	switch action {
	case "createworkspace":
		_, err := s.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: name, WorkspaceId: strings.TrimSpace(values["workspaceId"]), Workspace: &dfpb.Workspace{}})
		return err
	case "createrelease":
		rc, err := dataformReleaseConfig(values)
		if err != nil {
			return err
		}
		_, err = s.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: name, ReleaseConfigId: strings.TrimSpace(values["releaseId"]), ReleaseConfig: rc})
		return err
	case "createworkflow":
		wc, err := dataformWorkflowConfig(name, values)
		if err != nil {
			return err
		}
		_, err = s.CreateWorkflowConfig(ctx, &dfpb.CreateWorkflowConfigRequest{Parent: name, WorkflowConfigId: strings.TrimSpace(values["workflowId"]), WorkflowConfig: wc})
		return err
	case "settings":
		_, err := s.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: name,
			WorkspaceCompilationOverrides: &dfpb.Repository_WorkspaceCompilationOverrides{DefaultDatabase: strings.TrimSpace(values["defaultDatabase"]),
				SchemaSuffix: strings.TrimSpace(values["schemaSuffix"]), TablePrefix: strings.TrimSpace(values["tablePrefix"])}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workspace_compilation_overrides"}}})
		return err
	case "connectgit":
		_, err := s.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: name,
			GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: strings.TrimSpace(values["url"]), DefaultBranch: strings.TrimSpace(values["branch"]),
				AuthenticationTokenSecretVersion: strings.TrimSpace(values["secret"])}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"git_remote_settings"}}})
		return err
	case "disconnectgit":
		_, err := s.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: name},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"git_remote_settings"}}})
		return err
	case "delete":
		_, err := s.DeleteRepository(ctx, &dfpb.DeleteRepositoryRequest{Name: name, Force: true})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func (p dataformProvider) actOnWorkspace(ctx context.Context, s *dataform.Server, repo, ws, action string, values map[string]string) error {
	switch action {
	case "compile":
		_, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo,
			CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws}}})
		return err
	case "execute":
		ic, err := dataformInvocationConfig(values)
		if err != nil {
			return err
		}
		c, err := s.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo,
			CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws}}})
		if err != nil {
			return err
		}
		if n := len(c.GetCompilationErrors()); n > 0 {
			return fmt.Errorf("the workspace does not compile (%d errors): see Compilation errors", n)
		}
		_, err = s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo, WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: c.GetName()}, InvocationConfig: ic}})
		return err
	case "commit":
		_, err := s.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws, CommitMessage: values["message"],
			Author: &dfpb.CommitAuthor{Name: strings.TrimSpace(values["authorName"]), EmailAddress: strings.TrimSpace(values["authorEmail"])}})
		return err
	case "push":
		_, err := s.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws})
		return err
	case "pull":
		_, err := s.PullGitCommits(ctx, &dfpb.PullGitCommitsRequest{Name: ws, Author: &dfpb.CommitAuthor{Name: "CloudBurrow user", EmailAddress: "user@example.com"}})
		return err
	case "reset":
		_, err := s.ResetWorkspaceChanges(ctx, &dfpb.ResetWorkspaceChangesRequest{Name: ws, Clean: true})
		return err
	case "createfile":
		_, err := s.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: ws, Path: strings.TrimSpace(values["path"]), Contents: []byte(values["contents"])})
		return err
	case "createdir":
		_, err := s.MakeDirectory(ctx, &dfpb.MakeDirectoryRequest{Workspace: ws, Path: strings.TrimSpace(values["path"])})
		return err
	case "movedir":
		_, err := s.MoveDirectory(ctx, &dfpb.MoveDirectoryRequest{Workspace: ws, Path: strings.TrimSpace(values["path"]), NewPath: strings.TrimSpace(values["newPath"])})
		return err
	case "removedir":
		_, err := s.RemoveDirectory(ctx, &dfpb.RemoveDirectoryRequest{Workspace: ws, Path: strings.TrimSpace(values["path"])})
		return err
	case "install":
		_, err := s.InstallNpmPackages(ctx, &dfpb.InstallNpmPackagesRequest{Workspace: ws})
		return err
	case "deleteworkspace":
		_, err := s.DeleteWorkspace(ctx, &dfpb.DeleteWorkspaceRequest{Name: ws})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator   = dataformProvider{}
	_ console.Driller   = dataformProvider{}
	_ console.PathActor = dataformProvider{}
)
