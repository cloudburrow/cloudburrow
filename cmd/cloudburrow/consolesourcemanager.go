package main

// Secure Source Manager's pages (#1527): its instances, with Create and
// Delete; an instance's page with its repositories, Create repository and
// Delete; and a repository's page with its clone URL, how to push to it,
// the push triggers of its .cloudbuild/triggers.yaml and the builds a push
// ran, with Edit description and Delete repository. Every change goes
// through the official client on the Run port, as an SDK's would (#1521).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// sourceManagerProvider is the Secure Source Manager page.
type sourceManagerProvider struct{ b runBuildsProvider }

func (sourceManagerProvider) ID() string    { return "secure-source-manager" }
func (sourceManagerProvider) Title() string { return "Secure Source Manager" }

func (p sourceManagerProvider) project(project string) string {
	if project == "" {
		return p.b.defaultProject
	}
	return project
}

// withClient dials Secure Source Manager on the Run port as an SDK would.
func (p sourceManagerProvider) withClient(ctx context.Context, fn func(*securesourcemanager.Client) error) error {
	addr := ""
	if p.b.runAddr != nil {
		addr = p.b.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := securesourcemanager.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Secure Source Manager: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// instances are the project's instances, each location's through
// ListInstances.
func (p sourceManagerProvider) instances(ctx context.Context, project string) ([]*ssmpb.Instance, error) {
	project = p.project(project)
	var out []*ssmpb.Instance
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		for _, loc := range runadapter.SourceManagerLocations(project) {
			it := c.ListInstances(ctx, &ssmpb.ListInstancesRequest{Parent: "projects/" + project + "/locations/" + loc})
			for {
				in, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				out = append(out, in)
			}
		}
		return nil
	})
	return out, err
}

// instance resolves an instance by its ID; one ID in two locations is
// refused, since the page names instances by ID alone.
func (p sourceManagerProvider) instance(ctx context.Context, project, id string) (*ssmpb.Instance, error) {
	all, err := p.instances(ctx, project)
	if err != nil {
		return nil, err
	}
	var match []*ssmpb.Instance
	for _, in := range all {
		if strings.HasSuffix(in.GetName(), "/instances/"+id) {
			match = append(match, in)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("instance %q not found", id)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("instance %q is in %d locations; use gcloud source-manager instances --region", id, len(match))
}

func (p sourceManagerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Location", "Git host", "Age"}, NameColumn: "Instance", Noun: "instances",
		AlwaysStatus: true, RowsOpenable: true,
		Note: "Each instance is ACTIVE when created, and serves its repositories over git's HTTP protocol on this instance's " +
			"Run port. A push runs the push triggers of the repository's .cloudbuild/triggers.yaml as Cloud Builds. " +
			"Private instances, pull requests, issues, hooks and branch rules are not served."}
	ins, err := p.instances(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	for _, in := range ins {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(in.GetName()), Status: stateWord(in.GetState().String()),
			Fields: map[string]string{"Location": locationOf(in.GetName()), "Git host": orDash(in.GetHostConfig().GetGitHttp()),
				"Age": stampAge(in.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// stateWord is an enum value as the list's status: ACTIVE is Active.
func stateWord(s string) string {
	if s == "" {
		return ""
	}
	return s[:1] + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

func (sourceManagerProvider) CreateForm() (string, []console.Field) {
	return "Create instance", []console.Field{
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$"},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: "us-central1"},
	}
}

func (p sourceManagerProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	id := strings.TrimSpace(v["name"])
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "us-central1"
	}
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		op, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + region,
			InstanceId: id, Instance: &ssmpb.Instance{}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

// Delete deletes an instance and its repositories (force), as the
// confirmation names.
func (p sourceManagerProvider) Delete(ctx context.Context, project, name string) error {
	in, err := p.instance(ctx, project, name)
	if err != nil {
		return err
	}
	return p.withClient(ctx, func(c *securesourcemanager.Client) error {
		op, err := c.DeleteInstance(ctx, &ssmpb.DeleteInstanceRequest{Name: in.GetName(), Force: true})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

// repositories are an instance's repositories, through ListRepositories.
func (p sourceManagerProvider) repositories(ctx context.Context, in *ssmpb.Instance) ([]*ssmpb.Repository, error) {
	var out []*ssmpb.Repository
	parent := in.GetName()[:strings.Index(in.GetName(), "/instances/")]
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListRepositories(ctx, &ssmpb.ListRepositoriesRequest{Parent: parent, Instance: in.GetName()})
		for {
			r, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, r)
		}
	})
	return out, err
}

// repository reads a repository of an instance through GetRepository.
func (p sourceManagerProvider) repository(ctx context.Context, in *ssmpb.Instance, id string) (*ssmpb.Repository, error) {
	parent := in.GetName()[:strings.Index(in.GetName(), "/instances/")]
	var r *ssmpb.Repository
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		var e error
		r, e = c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: parent + "/repositories/" + id})
		return e
	})
	if err == nil && r.GetInstance() != in.GetName() {
		return nil, fmt.Errorf("repository %q is not on instance %q", id, lastSegment(in.GetName()))
	}
	return r, err
}

// Detail implements console.Driller: an instance's page, with its
// repositories, and a repository's page.
func (p sourceManagerProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	if len(path) == 2 {
		return p.repositoryDetail(ctx, project, in, path[1])
	}
	hc := in.GetHostConfig()
	d := console.Detail{Summary: dropDashes([]console.Property{{Label: "Name", Value: in.GetName()},
		{Label: "State", Value: in.GetState().String()}, {Label: "Git host", Value: hc.GetGitHttp()},
		{Label: "API host", Value: hc.GetApi()}, {Label: "Created", Value: stampAge(in.GetCreateTime())}})}
	repos, err := p.repositories(ctx, in)
	sec := console.Section{ID: "repositories", Label: "Repositories", Listing: console.Listing{
		Columns: []string{"Description", "Clone URL", "Age"}, NameColumn: "Repository", Noun: "repositories", RowsOpenable: true}}
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	for _, r := range repos {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(r.GetName()), Fields: map[string]string{
			"Description": orDash(r.GetDescription()), "Clone URL": r.GetUris().GetGitHttps(), "Age": stampAge(r.GetCreateTime())}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	d.Sections = []console.Section{sec}
	return d, nil
}

func dropDashes(props []console.Property) []console.Property {
	var out []console.Property
	for _, pr := range props {
		if pr.Value != "" && pr.Value != "-" {
			out = append(out, pr)
		}
	}
	return out
}

// pushHelp is the repository page's How to push section.
func pushHelp(r *ssmpb.Repository) string {
	clone := r.GetUris().GetGitHttps()
	branch := r.GetInitialConfig().GetDefaultBranch()
	if branch == "" {
		branch = "main"
	}
	return "Clone it, or add it as a remote of a repository you have, and push:\n\n" +
		"  git clone " + clone + "\n\n" +
		"  git remote add google " + clone + "\n" +
		"  git push google " + branch + "\n\n" +
		"No credential is asked for: this instance authenticates nothing.\n" +
		"A push runs the push triggers in " + runadapter.SourceManagerTriggersFile + " on " + branch + ", the default branch,\n" +
		"each as a Cloud Build of its configFilePath at the pushed commit."
}

func (p sourceManagerProvider) repositoryDetail(ctx context.Context, project string, in *ssmpb.Instance, id string) (console.Detail, error) {
	r, err := p.repository(ctx, in, id)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	branch := r.GetInitialConfig().GetDefaultBranch()
	if branch == "" {
		branch = "main"
	}
	d := console.Detail{Summary: dropDashes([]console.Property{{Label: "Name", Value: r.GetName()},
		{Label: "Description", Value: r.GetDescription()}, {Label: "Clone URL", Value: r.GetUris().GetGitHttps()},
		{Label: "Default branch", Value: branch}, {Label: "Instance", Value: lastSegment(r.GetInstance())},
		{Label: "Etag", Value: r.GetEtag()}, {Label: "Created", Value: stampAge(r.GetCreateTime())},
		{Label: "Updated", Value: stampAge(r.GetUpdateTime())}})}
	d.Sections = append(d.Sections, console.Section{ID: "push", Label: "How to push", Kind: console.KindText, Text: pushHelp(r)})

	trig := console.Section{ID: "triggers", Label: "Triggers", Listing: console.Listing{
		Columns: []string{"Event", "Config file", "Branches", "Ignored", "Service account"}, NameColumn: "Trigger", Noun: "triggers",
		AlwaysStatus: true}, Note: "Read from " + runadapter.SourceManagerTriggersFile + " on " + branch + "; change them by pushing it."}
	ts, err := runadapter.SourceManagerTriggers(ctx, r.GetName())
	if err != nil {
		trig.Unavailable = consoleErr(err)
	}
	for _, t := range ts {
		st := "Enabled"
		if t.Disabled {
			st = "Disabled"
		}
		if t.ServiceAccount == "" {
			st = "No service account"
		}
		trig.Listing.Items = append(trig.Listing.Items, console.Resource{Name: t.Name, Status: st, Fields: map[string]string{
			"Event": t.EventType, "Config file": t.ConfigFilePath, "Branches": orDash(t.IncludedGitRefs),
			"Ignored": orDash(t.IgnoredGitRefs), "Service account": orDash(t.ServiceAccount)}})
	}
	trig.Listing.Total = len(trig.Listing.Items)
	d.Sections = append(d.Sections, trig)

	builds := console.Section{ID: "builds", Label: "Builds", Listing: console.Listing{
		Columns: []string{"Commit", "Created"}, NameColumn: "Build", Noun: "builds", AlwaysStatus: true},
		Note: "The builds pushes to this repository ran, newest first; each opens on Cloud Build's Build history."}
	bs, err := p.pushBuilds(ctx, project, r)
	if err != nil {
		builds.Unavailable = consoleErr(err)
	}
	for _, b := range bs {
		builds.Listing.Items = append(builds.Listing.Items, console.Resource{Name: b.GetId(), Status: stateWord(b.GetStatus().String()),
			Link:   "/run/builds/" + url.PathEscape(b.GetId()) + "?project=" + url.QueryEscape(p.project(project)),
			Fields: map[string]string{"Commit": orDash(shortSHA(b.GetSource().GetGitSource().GetRevision())), "Created": stampAge(b.GetCreateTime())}})
	}
	builds.Listing.Total = len(builds.Listing.Items)
	d.Sections = append(d.Sections, builds)
	return d, nil
}

// shortSHA is a commit's first seven hex digits, as SHORT_SHA is.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// pushBuilds are the builds whose source is the repository, through
// ListBuilds in the repository's location.
func (p sourceManagerProvider) pushBuilds(ctx context.Context, project string, r *ssmpb.Repository) ([]*cbpb.Build, error) {
	project = p.project(project)
	var out []*cbpb.Build
	err := p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		it := c.ListBuilds(ctx, &cbpb.ListBuildsRequest{ProjectId: project, Parent: "projects/" + project + "/locations/" + locationOf(r.GetName())})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			if b.GetSource().GetGitSource().GetUrl() == r.GetUris().GetGitHttps() {
				out = append(out, b)
			}
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetCreateTime().AsTime().After(out[j].GetCreateTime().AsTime()) })
	return out, err
}

// DetailActions implements console.PathActor: Create repository on an
// instance's page; Edit description and Delete repository on a
// repository's.
func (p sourceManagerProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{{ID: "create-repo", Label: "Create repository", Fields: []console.Field{
			{Name: "id", Label: "Repository ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$"},
			{Name: "description", Label: "Description", Type: "text"},
			{Name: "branch", Label: "Default branch", Type: "text", Default: "main",
				Help: "the branch a clone checks out, and whose " + runadapter.SourceManagerTriggersFile + " a push's triggers are read from"},
		}}}
	case 2:
		r, err := p.repository(ctx, in, path[1])
		if err != nil {
			return nil
		}
		return []console.Action{
			{ID: "edit-description", Label: "Edit description", Fields: []console.Field{
				{Name: "description", Label: "Description", Type: "text", Default: r.GetDescription()}}},
			{ID: "delete-repo", Label: "Delete repository", Destructive: true, Leaves: true,
				Confirm: "the repository and every commit pushed to it"},
		}
	}
	return nil
}

// ActAt implements console.PathActor.
func (p sourceManagerProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return err
	}
	parent := in.GetName()[:strings.Index(in.GetName(), "/instances/")]
	switch {
	case len(path) == 1 && action == "create-repo":
		r := &ssmpb.Repository{Instance: in.GetName(), Description: strings.TrimSpace(v["description"])}
		if b := strings.TrimSpace(v["branch"]); b != "" {
			r.InitialConfig = &ssmpb.Repository_InitialConfig{DefaultBranch: b}
		}
		return p.withClient(ctx, func(c *securesourcemanager.Client) error {
			op, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: strings.TrimSpace(v["id"]), Repository: r})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case len(path) == 2 && action == "edit-description":
		return p.withClient(ctx, func(c *securesourcemanager.Client) error {
			op, err := c.UpdateRepository(ctx, &ssmpb.UpdateRepositoryRequest{
				Repository: &ssmpb.Repository{Name: parent + "/repositories/" + path[1], Description: strings.TrimSpace(v["description"])},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case len(path) == 2 && action == "delete-repo":
		return p.withClient(ctx, func(c *securesourcemanager.Client) error {
			op, err := c.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: parent + "/repositories/" + path[1]})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator   = sourceManagerProvider{}
	_ console.Deleter   = sourceManagerProvider{}
	_ console.Driller   = sourceManagerProvider{}
	_ console.PathActor = sourceManagerProvider{}
)
