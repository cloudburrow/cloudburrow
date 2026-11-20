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
	out := console.Listing{Columns: []string{"Region", "Instance URL", "Encryption", "Created"}, NameColumn: "Instance ID", Noun: "instances",
		AlwaysStatus: true, RowsOpenable: true,
		Note: "Each instance is ACTIVE when created, and serves its repositories over git's HTTP protocol on this instance's " +
			"Run port, its web interface being the instance's page here. A push runs the push triggers of the repository's " +
			".cloudbuild/triggers.yaml as Cloud Builds. Private instances and webhooks are not served."}
	ins, err := p.instances(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	for _, in := range ins {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(in.GetName()), Status: stateWord(in.GetState().String()),
			Fields: map[string]string{"Region": locationOf(in.GetName()), "Instance URL": orDash(in.GetHostConfig().GetHtml()),
				"Encryption": ssmEncryption(in), "Created": stampAge(in.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// ssmEncryption is an instance's encryption as the list names it.
func ssmEncryption(in *ssmpb.Instance) string {
	if in.GetKmsKey() != "" {
		return "Customer-managed key"
	}
	return "Google-managed key"
}

// stateWord is an enum value as the list's status: ACTIVE is Active.
func stateWord(s string) string {
	if s == "" {
		return ""
	}
	return s[:1] + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

// ssmRegions are the regions Google's Secure Source Manager locations page
// lists (https://docs.cloud.google.com/secure-source-manager/docs/locations,
// read 2026-11-19), each with its city.
var ssmRegions = []string{"us-central1", "northamerica-northeast1", "us-east1", "asia-east1", "asia-northeast1",
	"asia-northeast3", "australia-southeast1", "europe-west2", "europe-west4", "me-west1", "me-central2"}

var ssmRegionLabels = map[string]string{"us-central1": "us-central1 (Iowa)", "northamerica-northeast1": "northamerica-northeast1 (Montréal)",
	"us-east1": "us-east1 (South Carolina)", "asia-east1": "asia-east1 (Taiwan)", "asia-northeast1": "asia-northeast1 (Tokyo)",
	"asia-northeast3": "asia-northeast3 (Seoul)", "australia-southeast1": "australia-southeast1 (Sydney)",
	"europe-west2": "europe-west2 (London)", "europe-west4": "europe-west4 (Netherlands)", "me-west1": "me-west1 (Tel Aviv)",
	"me-central2": "me-central2 (Dammam)"}

const (
	ssmEncGoogle = "google"
	ssmEncCMEK   = "cmek"
)

// ssmPrivateUnavailable is why a private instance cannot be made here.
const ssmPrivateUnavailable = "A private instance is reached through Private Service Connect, which no CloudBurrow instance provides."

// CreateForm is Create instance, as Google's create-instance page documents
// it: an Instance ID, its Region, and its encryption, Google-managed or a
// customer-managed Cloud KMS key in the instance's region.
func (sourceManagerProvider) CreateForm() (string, []console.Field) {
	return "Create instance", []console.Field{
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "Lowercase letters, numbers and hyphens, starting with a letter. It cannot be changed later."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Options: ssmRegions, OptionLabels: ssmRegionLabels, Default: "us-central1"},
		{Name: "labels", Label: "Labels", Type: "map", KeyPattern: `^\p{Ll}[\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`,
			KeyHelp: "A key is 1 to 63 lowercase letters, digits, _ and -, starting with a lowercase letter.", ValueHelp: "A value is 0 to 63 lowercase letters, digits, _ and -.", MaxEntries: 64},
		{Name: "private", Label: "Private instance", Type: "checkbox", Default: "false", Unavailable: ssmPrivateUnavailable},
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: "Encryption", Options: []string{ssmEncGoogle, ssmEncCMEK}, Default: ssmEncGoogle,
			OptionLabels: map[string]string{ssmEncGoogle: "Google-managed encryption key", ssmEncCMEK: "Customer-managed encryption key (CMEK)"}},
		{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Section: "Encryption", Pattern: kmsKeyPattern, Browse: "kms", Required: true,
			ShowWhen: "encryption=" + ssmEncCMEK,
			Help:     "A key in the instance's region; it can be in another project. Stored on the instance; the local repositories are not encrypted with it."},
	}
}

// CreateOnPage, CreateTitle and CreateSubmit make Create instance a page of
// its own, ending in Create.
func (sourceManagerProvider) CreateOnPage() bool   { return true }
func (sourceManagerProvider) CreateTitle() string  { return "Create instance" }
func (sourceManagerProvider) CreateSubmit() string { return "Create" }

func (p sourceManagerProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	_, fields := p.CreateForm()
	if err := refuseUnavailable(v, fields); err != nil {
		return "", err
	}
	id := strings.TrimSpace(v["name"])
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "us-central1"
	}
	in := &ssmpb.Instance{}
	if strings.TrimSpace(v["labels"]) != "" {
		labels, err := arFormLabels(v["labels"])
		if err != nil {
			return "", err
		}
		in.Labels = labels
	}
	if v["encryption"] == ssmEncCMEK {
		in.KmsKey = strings.TrimSpace(v["kmsKey"])
		if in.KmsKey == "" {
			return "", errors.New("a customer-managed encryption key needs its Cloud KMS key")
		}
		if loc := locationOf(in.KmsKey); loc != region {
			return "", fmt.Errorf("the key is in %s: it must be in the instance's region, %s", loc, region)
		}
	}
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		op, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + region,
			InstanceId: id, Instance: in})
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
// repositories; a repository's page; and the pages inside a repository:
// its code at a ref (tree/REF/PATH...), an issue (issues/N) and a pull
// request (pulls/N).
func (p sourceManagerProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	switch {
	case len(path) == 1:
		return p.instanceDetail(ctx, project, in)
	case len(path) == 2:
		return p.repositoryDetail(ctx, project, in, path[1])
	case len(path) == 3 && (path[2] == ssmIssuesSegment || path[2] == ssmPullsSegment):
		// The level above an issue or a pull request, which Delete issue
		// goes up to: the repository's page on that tab.
		d, err := p.repositoryDetail(ctx, project, in, path[1])
		d.OpenTab = path[2]
		return d, err
	case len(path) == 3 && path[2] == ssmHooksSegment:
		// The level above a webhook, which its Delete goes up to.
		d, err := p.repositoryDetail(ctx, project, in, path[1])
		d.OpenTab = "settings"
		return d, err
	case len(path) == 4 && path[2] == ssmHooksSegment:
		return p.hookDetail(ctx, in, path)
	case len(path) >= 4 && path[2] == ssmTreeSegment:
		return p.codeDetail(ctx, in, path)
	case len(path) == 4 && path[2] == ssmIssuesSegment:
		return p.issueDetail(ctx, in, path)
	case len(path) == 4 && path[2] == ssmPullsSegment:
		return p.pullDetail(ctx, in, path)
	}
	return console.DeeperThan(2, path), nil
}

// instanceDetail is an instance's page: its details, with its web
// interface's URL, then its repositories (Google's web interface's My
// repositories) and its Developer Connect connections.
func (p sourceManagerProvider) instanceDetail(ctx context.Context, project string, in *ssmpb.Instance) (console.Detail, error) {
	hc := in.GetHostConfig()
	labels := make([]string, 0, len(in.GetLabels()))
	for k, v := range in.GetLabels() {
		labels = append(labels, k+": "+v)
	}
	sort.Strings(labels)
	d := console.Detail{Summary: dropDashes([]console.Property{{Label: "Name", Value: in.GetName()},
		{Label: "Region", Value: locationOf(in.GetName())},
		{Label: "State", Value: stateWord(in.GetState().String())},
		{Label: "Web interface", Value: orDash(httpURL(hc.GetHtml())), Copy: "Copy URL"},
		{Label: "Git host", Value: hc.GetGitHttp()},
		{Label: "API host", Value: hc.GetApi()},
		{Label: "Encryption", Value: ssmEncryption(in)},
		{Label: "Cloud KMS key", Value: in.GetKmsKey()},
		{Label: "Labels", Value: strings.Join(labels, ", ")},
		{Label: "Created", Value: stampAge(in.GetCreateTime())},
		{Label: "Updated", Value: stampAge(in.GetUpdateTime())}})}
	repos, err := p.repositories(ctx, in)
	sec := console.Section{ID: "repositories", Label: "Repositories", Listing: console.Listing{
		Columns: []string{"Description", "Clone URL", "Created"}, NameColumn: "Repository", Noun: "repositories", RowsOpenable: true},
		Note:    "The instance's repositories, as its web interface's My repositories lists them; this console is that web interface here.",
		Actions: []console.Action{createRepositoryAction()}}
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	for _, r := range repos {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(r.GetName()), Fields: map[string]string{
			"Description": orDash(r.GetDescription()), "Clone URL": r.GetUris().GetGitHttps(), "Created": stampAge(r.GetCreateTime())}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	d.Sections = []console.Section{sec, p.connectionsSection(project, in), {ID: "permissions", Kind: console.KindPermissions}}
	return d, nil
}

// httpURL is a host as a URL a browser opens.
func httpURL(host string) string {
	if host == "" || strings.Contains(host, "://") {
		return host
	}
	return "http://" + host
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

// ssmInitializeUnavailable is why Initialize repository cannot be chosen.
const ssmInitializeUnavailable = "The .gitignore, license and README templates are Google's own, which a local instance does not hold: push the files instead."

// createRepositoryAction is the web interface's Create repository form, as
// Google's create-repository page documents it.
func createRepositoryAction() console.Action {
	return console.Action{ID: "create-repo", Label: "Create repository", Submit: "Create repository", Fields: []console.Field{
		{Name: "id", Label: "Repository ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "Lowercase letters, numbers and dashes, starting with a letter. It cannot be changed later."},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "branch", Label: "Default branch", Type: "text", Default: "main",
			Help: "the branch a clone checks out, and whose " + runadapter.SourceManagerTriggersFile + " a push's triggers are read from"},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Pattern: `^[^@\s]+@[^@\s]+$`,
			Help: "A user-managed service account for integrations such as Cloud Build (optional)."},
		{Name: "initialize", Label: "Initialize repository", Type: "checkbox", Default: "false", Unavailable: ssmInitializeUnavailable},
	}}
}

// pushHelp is how to push to a repository: an empty repository's Code tab.
func pushHelp(r *ssmpb.Repository) string {
	clone := r.GetUris().GetGitHttps()
	branch := ssmDefaultBranch(r)
	return "This repository is empty. Clone it, or add it as a remote of a repository you have, and push:\n\n" +
		"  git clone " + clone + "\n\n" +
		"  git remote add google " + clone + "\n" +
		"  git push google " + branch + "\n\n" +
		"No credential is asked for: this instance authenticates nothing.\n" +
		"A push runs the push triggers in " + runadapter.SourceManagerTriggersFile + " on " + branch + ", the default branch,\n" +
		"each as a Cloud Build of its configFilePath at the pushed commit."
}

// ssmDefaultBranch is a repository's default branch.
func ssmDefaultBranch(r *ssmpb.Repository) string {
	if b := r.GetInitialConfig().GetDefaultBranch(); b != "" {
		return b
	}
	return "main"
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
// instance's page; a repository's, an issue's, a pull request's, a branch
// rule's and an issue comment's own actions (consolesourcemanagerrepo.go).
func (p sourceManagerProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if len(path) == 1 {
		return append([]console.Action{createRepositoryAction()}, p.connectionActions(project, in)...)
	}
	return p.repoActions(ctx, project, in, path)
}

// ActAt implements console.PathActor.
func (p sourceManagerProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return err
	}
	if ok, err := p.devConnectAct(ctx, path, action, in, v); ok {
		return err
	}
	parent := in.GetName()[:strings.Index(in.GetName(), "/instances/")]
	if len(path) == 1 && action == "create-repo" {
		if err := refuseUnavailable(v, createRepositoryAction().Fields); err != nil {
			return err
		}
		r := &ssmpb.Repository{Instance: in.GetName(), Description: strings.TrimSpace(v["description"]),
			ServiceAccount: strings.TrimSpace(v["serviceAccount"])}
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
	}
	if len(path) >= 2 {
		return p.repoAct(ctx, in, path, action, v)
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator         = sourceManagerProvider{}
	_ console.PageCreator     = sourceManagerProvider{}
	_ console.CreateTitler    = sourceManagerProvider{}
	_ console.CreateSubmitter = sourceManagerProvider{}
	_ console.Deleter         = sourceManagerProvider{}
	_ console.Driller         = sourceManagerProvider{}
	_ console.PathActor       = sourceManagerProvider{}
)
