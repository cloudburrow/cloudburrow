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
	"sort"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	iampb "cloud.google.com/go/iam/apiv1/iampb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// buildDashboardProvider is the Dashboard: a summary per trigger of its
// latest build and its recent history, and how many builds await approval.
type buildDashboardProvider struct{ b runBuildsProvider }

func (buildDashboardProvider) ID() string    { return "cloud-build-dashboard" }
func (buildDashboardProvider) Title() string { return "Cloud Build dashboard" }

func (p buildDashboardProvider) List(_ context.Context, project string) (console.Listing, error) {
	if project == "" {
		project = p.b.defaultProject
	}
	out := console.Listing{Columns: []string{"Repository", "Event", "Latest build", "Latest build status", "Duration", "Build history", "Pass rate"},
		NameColumn: "Trigger", Noun: "triggers", AlwaysStatus: true}
	builds := runadapter.CloudBuilds(project, "")
	byTrigger := map[string][]*cbpb.Build{}
	pending := 0
	for _, b := range builds {
		byTrigger[b.GetBuildTriggerId()] = append(byTrigger[b.GetBuildTriggerId()], b)
		if b.GetStatus() == cbpb.Build_PENDING && b.GetApproval().GetState() == cbpb.BuildApproval_PENDING {
			pending++
		}
	}
	for _, t := range runadapter.Triggers(project, "") {
		bs := byTrigger[t.GetId()]
		sort.Slice(bs, func(i, j int) bool { return bs[i].GetCreateTime().AsTime().After(bs[j].GetCreateTime().AsTime()) })
		st := "Enabled"
		if t.GetDisabled() {
			st = "Disabled"
		}
		repo := "-"
		if r := t.GetRepositoryEventConfig().GetRepository(); r != "" {
			repo = r[strings.LastIndex(r, "/")+1:]
		}
		f := map[string]string{"Repository": repo, "Event": triggerEvent(t), "Latest build": "-", "Latest build status": "-",
			"Duration": "-", "Build history": "-", "Pass rate": "-"}
		if len(bs) > 0 {
			f["Latest build"], f["Latest build status"] = bs[0].GetId(), buildStatus(bs[0])
			f["Duration"] = orDash(buildDuration(bs[0]))
			var hist []string
			passed, done := 0, 0
			for i, b := range bs {
				if i < 10 {
					hist = append(hist, buildStatus(b))
				}
				switch b.GetStatus() {
				case cbpb.Build_SUCCESS:
					passed++
					done++
				case cbpb.Build_FAILURE, cbpb.Build_TIMEOUT, cbpb.Build_INTERNAL_ERROR:
					done++
				}
			}
			f["Build history"] = strings.Join(hist, ", ")
			if done > 0 {
				f["Pass rate"] = fmt.Sprintf("%d%% (%d of %d)", passed*100/done, passed, done)
			}
		}
		out.Items = append(out.Items, console.Resource{Name: t.GetName(), Status: st, Fields: f,
			Link: "/cloud-build/triggers/" + url.PathEscape(t.GetName())})
	}
	out.Total = len(out.Items)
	if pending > 0 {
		filter := url.QueryEscape(`status="PENDING"`)
		out.Summary = []console.PropertyGroup{{Heading: fmt.Sprintf("%d builds awaiting approval", pending), Properties: []console.Property{
			{Label: "Approval", Value: "View Builds", Link: "/cloud-build/builds?filter=" + filter}}}}
	}
	if len(out.Items) == 0 {
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

func (p buildSettingsProvider) account(ctx context.Context, project string) string {
	groups := buildTriggersProvider{b: p.b}.defaultServiceAccount(ctx, project)
	if len(groups) == 0 || len(groups[0].Properties) == 0 {
		return ""
	}
	return groups[0].Properties[0].Value
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
	_ console.Provider  = buildSettingsProvider{}
	_ console.PathActor = buildRepositoriesProvider{}
)
