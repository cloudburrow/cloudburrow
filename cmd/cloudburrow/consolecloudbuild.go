package main

// Cloud Build as its own product (#1854): its Dashboard, its Repositories
// (2nd gen: Developer Connect connections to this instance's Secure Source
// Manager, and their linked repositories) and its Settings (Service account
// permissions). History, Triggers and Worker pools are
// consolesourcedeploy.go and consolebuildtriggers.go. Pages and wording from
// https://docs.cloud.google.com/build/docs/view-build-results,
// https://docs.cloud.google.com/build/docs/automating-builds/create-manage-triggers and
// https://docs.cloud.google.com/build/docs/securing-builds/configure-access-for-cloud-build-service-account.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	iampb "cloud.google.com/go/iam/apiv1/iampb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// buildDashboardProvider is the Dashboard, as Google's "View build results
// for build triggers" page describes it (#1854): a card per trigger,
// ordered by repository name and then trigger name, titled by its latest
// build's outcome, the repository and the trigger, with Latest build,
// Duration, Source, Commit, Failed step (when the latest build failed),
// Build history, Average duration and Pass - Fail % of the 20 most recent
// builds; and how many builds await approval, with View Builds.
type buildDashboardProvider struct{ b runBuildsProvider }

func (buildDashboardProvider) ID() string    { return "cloud-build-dashboard" }
func (buildDashboardProvider) Title() string { return "Cloud Build dashboard" }

// dashboardRecent is how many of a trigger's builds its card summarizes.
const dashboardRecent = 20

// buildOutcome is a card's title word for its latest build.
func buildOutcome(b *cbpb.Build) string {
	switch b.GetStatus() {
	case cbpb.Build_SUCCESS:
		return "Successful"
	case cbpb.Build_FAILURE, cbpb.Build_INTERNAL_ERROR:
		return "Failed"
	case cbpb.Build_TIMEOUT:
		return "Timed out"
	case cbpb.Build_CANCELLED, cbpb.Build_EXPIRED:
		return "Cancelled"
	case cbpb.Build_WORKING:
		return "Running"
	case cbpb.Build_PENDING:
		return "Awaiting approval"
	}
	return "Queued"
}

// barStatus colours a Build history bar: green for a success, red for a
// failure or timeout, neither for the rest.
func barStatus(b *cbpb.Build) string {
	switch b.GetStatus() {
	case cbpb.Build_SUCCESS:
		return "ok"
	case cbpb.Build_FAILURE, cbpb.Build_TIMEOUT, cbpb.Build_INTERNAL_ERROR:
		return "fail"
	}
	return ""
}

// hms is a duration as HH:MM:SS.
func hms(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// finishedDuration is a finished build's runtime.
func finishedDuration(b *cbpb.Build) (time.Duration, bool) {
	if b.GetStartTime() == nil || b.GetFinishTime() == nil {
		return 0, false
	}
	return b.GetFinishTime().AsTime().Sub(b.GetStartTime().AsTime()), true
}

func (p buildDashboardProvider) List(_ context.Context, project string) (console.Listing, error) {
	if project == "" {
		project = p.b.defaultProject
	}
	out := console.Listing{NameColumn: "Trigger", Noun: "triggers", SummaryOnly: true, CardFilter: "Filter triggers"}
	byTrigger := map[string][]*cbpb.Build{}
	pending := 0
	for _, b := range runadapter.CloudBuilds(project, "") {
		byTrigger[b.GetBuildTriggerId()] = append(byTrigger[b.GetBuildTriggerId()], b)
		if awaitingApproval(b) {
			pending++
		}
	}
	if pending > 0 {
		filter := url.QueryEscape(`status="PENDING"`)
		out.Summary = append(out.Summary, console.PropertyGroup{Heading: fmt.Sprintf("%d builds awaiting approval", pending),
			Properties: []console.Property{{Label: "Approval", Value: "View Builds", Link: "/cloud-build/builds?filter=" + filter}}})
	}
	q := "?project=" + url.QueryEscape(project)
	triggers := runadapter.Triggers(project, "")
	repoOf := func(t *cbpb.BuildTrigger) string {
		r := t.GetRepositoryEventConfig().GetRepository()
		return r[strings.LastIndex(r, "/")+1:]
	}
	sort.SliceStable(triggers, func(i, j int) bool {
		if ri, rj := repoOf(triggers[i]), repoOf(triggers[j]); ri != rj {
			return ri < rj
		}
		return triggers[i].GetName() < triggers[j].GetName()
	})
	for _, t := range triggers {
		bs := byTrigger[t.GetId()]
		sort.Slice(bs, func(i, j int) bool { return bs[i].GetCreateTime().AsTime().After(bs[j].GetCreateTime().AsTime()) })
		if len(bs) > dashboardRecent {
			bs = bs[:dashboardRecent]
		}
		repo := repoOf(t)
		title := []string{"No builds yet"}
		if len(bs) > 0 {
			title[0] = buildOutcome(bs[0])
		}
		if repo != "" {
			title = append(title, repo)
		}
		title = append(title, t.GetName())
		props := []console.Property{{Label: "Trigger", Value: t.GetName(), Link: "/cloud-build/triggers/" + url.PathEscape(t.GetName()) + q}}
		if t.GetDisabled() {
			props = append(props, console.Property{Label: "State", Value: "Disabled"})
		}
		if len(bs) == 0 {
			props = append(props, console.Property{Label: "Latest build", Value: "-"})
			out.Summary = append(out.Summary, console.PropertyGroup{Heading: strings.Join(title, " · "), Properties: props,
				Pin: t.GetId(), Filter: strings.Join([]string{repo, t.GetName(), t.GetDescription()}, " ")})
			continue
		}
		latest := bs[0]
		buildLink := "/cloud-build/builds/" + url.PathEscape(latest.GetId()) + q
		props = append(props, console.Property{Label: "Latest build", Value: latest.GetCreateTime().AsTime().Local().Format("Jan 2, 2006, 3:04:05 PM"), Link: buildLink})
		if d, ok := finishedDuration(latest); ok {
			props = append(props, console.Property{Label: "Duration", Value: hms(d)})
		} else if latest.GetStartTime() != nil {
			props = append(props, console.Property{Label: "Duration", Value: hms(time.Since(latest.GetStartTime().AsTime()))})
		}
		if repo != "" {
			props = append(props, console.Property{Label: "Source", Value: repo, Link: "/cloud-build/repositories" + q})
		} else if src := buildSource(latest); src != "" {
			props = append(props, console.Property{Label: "Source", Value: src})
		}
		if sha := latest.GetSubstitutions()["COMMIT_SHA"]; sha != "" {
			props = append(props, console.Property{Label: "Commit", Value: sha[:min(7, len(sha))], Link: buildLink})
		}
		if latest.GetStatus() == cbpb.Build_FAILURE || latest.GetStatus() == cbpb.Build_TIMEOUT {
			for i, st := range latest.GetSteps() {
				if st.GetStatus() == cbpb.Build_FAILURE || st.GetStatus() == cbpb.Build_TIMEOUT {
					name := st.GetId()
					if name == "" {
						name = fmt.Sprintf("Step #%d", i)
					}
					props = append(props, console.Property{Label: "Failed step", Value: name, Link: buildLink})
					break
				}
			}
		}
		// Build history: Google's bar chart of each recent build's
		// duration, oldest first, each bar its outcome, duration and time.
		var hist []string
		var bars []console.Bar
		var total time.Duration
		finished, passed, failed := 0, 0, 0
		for i := len(bs) - 1; i >= 0; i-- {
			b := bs[i]
			d, ok := finishedDuration(b)
			word := buildOutcome(b)
			if ok {
				total += d
				finished++
				word += " " + hms(d)
			} else if b.GetStartTime() != nil {
				d = time.Since(b.GetStartTime().AsTime())
			}
			hist = append(hist, word)
			bars = append(bars, console.Bar{Value: d.Seconds(), Status: barStatus(b),
				Label: word + ", " + b.GetCreateTime().AsTime().Local().Format("Jan 2, 2006, 3:04:05 PM"),
				Link:  "/cloud-build/builds/" + url.PathEscape(b.GetId()) + q})
			switch b.GetStatus() {
			case cbpb.Build_SUCCESS:
				passed++
			case cbpb.Build_FAILURE, cbpb.Build_TIMEOUT, cbpb.Build_INTERNAL_ERROR:
				failed++
			}
		}
		props = append(props, console.Property{Label: "Build history", Value: strings.Join(hist, ", "), Chart: &console.BarChart{Bars: bars}})
		if finished > 0 {
			props = append(props, console.Property{Label: "Average duration", Value: hms(total / time.Duration(finished))})
		}
		if passed+failed > 0 {
			props = append(props, console.Property{Label: "Pass - Fail %",
				Value: fmt.Sprintf("%d%% - %d%%", passed*100/(passed+failed), 100-passed*100/(passed+failed))})
		}
		out.Summary = append(out.Summary, console.PropertyGroup{Heading: strings.Join(title, " · "), Properties: props,
			Pin: t.GetId(), Filter: strings.Join([]string{repo, t.GetName(), t.GetDescription()}, " ")})
	}
	out.Total = len(triggers)
	if len(triggers) == 0 {
		out.Note = "The dashboard summarizes each trigger's builds: create a trigger on Triggers."
	}
	return out, nil
}

// buildRepositoriesProvider is Repositories: the 2nd gen tab lists the
// project's Developer Connect connections, each a host connection to one
// of this instance's Secure Source Manager instances, and the repositories
// linked through them; 1st gen repositories connect hosted services.
type buildRepositoriesProvider struct{ ssm sourceManagerProvider }

func (buildRepositoriesProvider) ID() string    { return "cloud-build-repositories" }
func (buildRepositoriesProvider) Title() string { return "Cloud Build repositories" }

func (p buildRepositoriesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	project = p.ssm.project(project)
	out := console.Listing{Columns: []string{"Type", "Region", "Host", "Connection", "Clone URI", "Age"},
		NameColumn: "Name", Noun: "connections and repositories", AlwaysStatus: true,
		Tabs: []console.Section{{ID: "2nd-gen", Label: "2nd gen"}, {ID: "1st-gen", Label: "1st gen",
			Note: "1st gen repositories connect GitHub, GitHub Enterprise, Bitbucket and GitLab, hosted services no instance reaches. " +
				"Connect this instance's Secure Source Manager repositories on the 2nd gen tab."}},
		ListTab: "2nd-gen"}
	for _, c := range runadapter.DeveloperConnectConnections(project) {
		st := "Installed"
		if c.GetDisabled() {
			st = "Disabled"
		}
		host := "Secure Source Manager"
		if in := c.GetSecureSourceManagerInstanceConfig().GetInstance(); in != "" {
			host += ": " + lastSegment(in)
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(c.GetName()), Status: st, Fields: map[string]string{
			"Type": "Host connection", "Region": locationOf(c.GetName()), "Host": host, "Connection": "-", "Clone URI": "-",
			"Age": stampAge(c.GetCreateTime())}, Actions: []console.Action{{ID: "delete-connection", Label: "Delete",
			Destructive: true, Confirm: "the host connection (its linked repositories must be deleted first)"}},
			Target: []string{"connection", c.GetName()}})
		for _, l := range runadapter.DeveloperConnectLinks(c.GetName()) {
			out.Items = append(out.Items, console.Resource{Name: lastSegment(l.GetName()), Status: "Linked", Fields: map[string]string{
				"Type": "Repository", "Region": locationOf(c.GetName()), "Host": host, "Connection": lastSegment(c.GetName()),
				"Clone URI": l.GetCloneUri(), "Age": stampAge(l.GetCreateTime())}, Actions: []console.Action{{ID: "delete-link",
				Label: "Delete", Destructive: true, Confirm: "the linked repository; triggers on it stop running"}},
				Target: []string{"link", l.GetName()}})
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

// ListActions are Create host connection and Link repository.
func (p buildRepositoriesProvider) ListActions(ctx context.Context, project string) []console.Action {
	project = p.ssm.project(project)
	instances, err := p.ssm.instances(ctx, project)
	if err != nil {
		return nil
	}
	var ins, repos []string
	for _, in := range instances {
		ins = append(ins, in.GetName())
		rs, err := p.ssm.repositories(ctx, in)
		if err != nil {
			continue
		}
		for _, r := range rs {
			repos = append(repos, r.GetName())
		}
	}
	var conns []string
	for _, c := range runadapter.DeveloperConnectConnections(project) {
		conns = append(conns, c.GetName())
	}
	idPattern := "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$"
	var acts []console.Action
	if len(ins) > 0 {
		acts = append(acts, console.Action{ID: "create-connection", Label: "Create host connection", Submit: "Connect", Fields: []console.Field{
			{Name: "provider", Label: "Provider", Type: "select", Options: []string{"Secure Source Manager"}, Default: "Secure Source Manager",
				Help: "GitHub, GitLab and Bitbucket are hosted services no instance reaches."},
			{Name: "instance", Label: "Instance", Type: "select", Options: ins, Default: ins[0], Required: true,
				Help: "The connection is made in the instance's region."},
			{Name: "id", Label: "Name", Type: "text", Required: true, Pattern: idPattern}}})
	}
	if len(conns) > 0 && len(repos) > 0 {
		acts = append(acts, console.Action{ID: "create-link", Label: "Link repository", Submit: "Link", Fields: []console.Field{
			{Name: "connection", Label: "Connection", Type: "select", Options: conns, Default: conns[0], Required: true},
			{Name: "repository", Label: "Repository", Type: "select", Options: repos, Default: repos[0], Required: true},
			{Name: "id", Label: "Repository name", Type: "text", Required: true, Pattern: idPattern}}})
	}
	return acts
}

// DetailActions are a row's Delete, at the path the row names.
func (p buildRepositoriesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch {
	case len(path) == 0:
		return p.ListActions(ctx, project)
	case len(path) == 2 && path[0] == "connection":
		return []console.Action{{ID: "delete-connection", Label: "Delete", Destructive: true,
			Confirm: "the host connection (its linked repositories must be deleted first)"}}
	case len(path) == 2 && path[0] == "link":
		return []console.Action{{ID: "delete-link", Label: "Delete", Destructive: true,
			Confirm: "the linked repository; triggers on it stop running"}}
	}
	return nil
}

// ActAt makes or deletes a connection or link through Developer Connect,
// as the Secure Source Manager pages do.
func (p buildRepositoriesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	project = p.ssm.project(project)
	instanceOf := func(name string) (*ssmpb.Instance, error) {
		ins, err := p.ssm.instances(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, in := range ins {
			if in.GetName() == name {
				return in, nil
			}
		}
		return nil, fmt.Errorf("instance %s not found", name)
	}
	switch action {
	case "create-connection":
		in, err := instanceOf(v["instance"])
		if err != nil {
			return err
		}
		_, err = p.ssm.devConnectAct(ctx, []string{lastSegment(in.GetName())}, action, in, v)
		return err
	case "create-link":
		var conn *string
		for _, c := range runadapter.DeveloperConnectConnections(project) {
			if c.GetName() == v["connection"] {
				n := c.GetSecureSourceManagerInstanceConfig().GetInstance()
				conn = &n
			}
		}
		if conn == nil {
			return fmt.Errorf("connection %s not found", v["connection"])
		}
		in, err := instanceOf(*conn)
		if err != nil {
			return err
		}
		repo := v["repository"]
		if !strings.HasPrefix(repo, in.GetName()[:strings.Index(in.GetName(), "/instances/")]+"/repositories/") {
			return fmt.Errorf("repository %s is not in the connection's region", repo)
		}
		_, err = p.ssm.devConnectAct(ctx, []string{lastSegment(in.GetName()), lastSegment(repo)}, action, in,
			map[string]string{"connection": lastSegment(v["connection"]), "id": v["id"]})
		return err
	case "delete-connection", "delete-link":
		if len(path) != 2 {
			return fmt.Errorf("unknown action %q", action)
		}
		name := path[1]
		parent := name[:strings.Index(name, "/connections/")]
		in := &ssmpb.Instance{Name: parent + "/instances/-"}
		if action == "delete-connection" {
			_, err := p.ssm.devConnectAct(ctx, []string{"-"}, action, in, map[string]string{"connection": lastSegment(name)})
			return err
		}
		_, err := p.ssm.devConnectAct(ctx, []string{"-", "-"}, action, in, map[string]string{"link": name})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// buildSettingsRoles are the roles Google's Service account permissions
// page lists, with the GCP service each belongs to.
var buildSettingsRoles = []struct{ service, role, title string }{
	{"Cloud Run", "roles/run.admin", "Cloud Run Admin"},
	{"Cloud Functions", "roles/cloudfunctions.developer", "Cloud Functions Developer"},
	{"App Engine", "roles/appengine.appAdmin", "App Engine Admin"},
	{"Kubernetes Engine", "roles/container.developer", "Kubernetes Engine Developer"},
	{"Compute Engine", "roles/compute.instanceAdmin.v1", "Compute Instance Admin (v1)"},
	{"Firebase", "roles/firebase.admin", "Firebase Admin"},
	{"Cloud KMS", "roles/cloudkms.cryptoKeyDecrypter", "Cloud KMS CryptoKey Decrypter"},
	{"Secret Manager", "roles/secretmanager.secretAccessor", "Secret Manager Secret Accessor"},
	{"Service Accounts", "roles/iam.serviceAccountUser", "Service Account User"},
	{"Cloud Build", "roles/cloudbuild.workerPoolUser", "Cloud Build WorkerPool User"},
}

// buildSettingsProvider is Settings, which opens on Service account
// permissions: each role's status for the build service account, set on
// the project's IAM policy (stored, not enforced: ADR-0006).
type buildSettingsProvider struct {
	b        runBuildsProvider
	registry *resourcemanager.Registry
}

func (buildSettingsProvider) ID() string    { return "cloud-build-settings" }
func (buildSettingsProvider) Title() string { return "Cloud Build settings" }

// settingsAccountKey carries Settings' Service account drop-down: "Use the
// drop-down menu to select the service account you want to update"
// (configure-access-for-cloud-build-service-account, #1854).
const settingsAccountKey = "serviceAccount"

// FilterKeys implements console.Filterer: the Service account drop-down.
func (buildSettingsProvider) FilterKeys() []string { return []string{settingsAccountKey} }

// defaultAccount is the project's default build service account.
func (p buildSettingsProvider) defaultAccount(ctx context.Context, project string) string {
	groups := buildTriggersProvider{b: p.b}.defaultServiceAccount(ctx, project)
	if len(groups) == 0 || len(groups[0].Properties) == 0 {
		return ""
	}
	return groups[0].Properties[0].Value
}

// accounts are the drop-down's choices: the default build service account,
// then the project's other IAM service accounts.
func (p buildSettingsProvider) accounts(ctx context.Context, project string) []string {
	def := p.defaultAccount(ctx, project)
	if def == "" {
		return nil
	}
	out := []string{def}
	for _, a := range (buildTriggersProvider{registry: p.registry}).serviceAccounts(project) {
		if a != def {
			out = append(out, a)
		}
	}
	return out
}

// account is the service account the page updates: the one chosen in the
// drop-down, else the default build service account.
func (p buildSettingsProvider) account(ctx context.Context, project string) string {
	all := p.accounts(ctx, project)
	if len(all) == 0 {
		return ""
	}
	if want := console.ScopeValue(ctx, settingsAccountKey); want != "" && slices.Contains(all, want) {
		return want
	}
	return all[0]
}

func (p buildSettingsProvider) policy(ctx context.Context, project string) (*iampb.Policy, error) {
	if p.registry == nil {
		return nil, fmt.Errorf("Resource Manager is not served, so the project has no IAM policy here")
	}
	return resourcemanager.NewProjectsServer(p.registry).GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resourcemanager.V3Name(project)})
}

func (p buildSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	if project == "" {
		project = p.b.defaultProject
	}
	out := console.Listing{Columns: []string{"GCP Service", "Role"}, NameColumn: "Role", Noun: "roles", AlwaysStatus: true,
		Tabs: []console.Section{{ID: "permissions", Label: "Service account permissions"}}, ListTab: "permissions",
		Note: "Each role is a binding on the project's IAM policy for the build service account, stored and not enforced (ADR-0006)."}
	sa := p.account(ctx, project)
	if sa == "" {
		out.Unavailable = "Cloud Build is not reachable, so its service account is not known"
		return out, nil
	}
	all := p.accounts(ctx, project)
	choice := &console.Scope{Key: settingsAccountKey, Label: "Service account", Selected: sa, Default: all[0]}
	for i, a := range all {
		label := a
		if i == 0 {
			label += " (default)"
		}
		choice.Options = append(choice.Options, console.ScopeOption{ID: a, Label: label})
	}
	out.Filters = []*console.Scope{choice}
	out.Summary = []console.PropertyGroup{{Heading: "Service account permissions", Properties: []console.Property{
		{Label: "Service account email", Value: sa}}}}
	pol, err := p.policy(ctx, project)
	if err != nil {
		out.Unavailable = consoleErr(err)
		return out, nil
	}
	member := "serviceAccount:" + sa
	for _, r := range buildSettingsRoles {
		st := "Disabled"
		for _, b := range pol.GetBindings() {
			if b.GetRole() == r.role && b.GetCondition() == nil {
				for _, m := range b.GetMembers() {
					if m == member {
						st = "Enabled"
					}
				}
			}
		}
		out.Items = append(out.Items, console.Resource{Name: r.title, Status: st, Fields: map[string]string{"GCP Service": r.service, "Role": r.role}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// Actions are Enable and Disable, the status drop-down of each role.
func (p buildSettingsProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "Enabled" {
		return []console.Action{{ID: "disable", Label: "Disable", Confirm: "the build service account loses " + r.Fields["Role"]}}
	}
	return []console.Action{{ID: "enable", Label: "Enable"}}
}

// Act grants or removes the role through SetIamPolicy with the etag read.
func (p buildSettingsProvider) Act(ctx context.Context, project, name, action string) error {
	if project == "" {
		project = p.b.defaultProject
	}
	role := ""
	for _, r := range buildSettingsRoles {
		if r.title == name {
			role = r.role
		}
	}
	if role == "" || (action != "enable" && action != "disable") {
		return fmt.Errorf("unknown action %q on %q", action, name)
	}
	sa := p.account(ctx, project)
	if sa == "" {
		return fmt.Errorf("Cloud Build is not reachable, so its service account is not known")
	}
	pol, err := p.policy(ctx, project)
	if err != nil {
		return err
	}
	member := "serviceAccount:" + sa
	var kept []*iampb.Binding
	found := false
	for _, b := range pol.GetBindings() {
		if b.GetRole() == role && b.GetCondition() == nil {
			found = true
			var ms []string
			for _, m := range b.GetMembers() {
				if m != member {
					ms = append(ms, m)
				}
			}
			if action == "enable" {
				ms = append(ms, member)
			}
			if len(ms) == 0 {
				continue
			}
			b.Members = ms
		}
		kept = append(kept, b)
	}
	if !found && action == "enable" {
		kept = append(kept, &iampb.Binding{Role: role, Members: []string{member}})
	}
	pol.Bindings = kept
	_, err = resourcemanager.NewProjectsServer(p.registry).SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resourcemanager.V3Name(project), Policy: pol})
	return err
}

var (
	_ console.Provider  = buildDashboardProvider{}
	_ console.ListActor = buildRepositoriesProvider{}
	_ console.Actor     = buildSettingsProvider{}
	_ console.Filterer  = buildSettingsProvider{}
	_ console.Provider  = buildSettingsProvider{}
	_ console.PathActor = buildRepositoriesProvider{}
)
