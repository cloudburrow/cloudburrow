package main

// Cloud Run: Connect repo on the Services list (#1852), Google's
// "Continuously deploy from a repository" set up with Cloud Build: pick the
// repository provider and a repository, then the build configuration (a
// branch regex and Dockerfile or Google Cloud's buildpacks), and the
// service. Create makes the service (Google's placeholder image until the
// first build deploys), then the Cloud Build trigger Google's console
// makes: a push to a matching branch builds the image, pushes it, and
// deploys it with `gcloud run services update`, a new revision. The
// trigger is run once at once, as Google's console runs it.
//
// The repository is a Developer Connect git repository link to one of this
// instance's Secure Source Manager repositories: no hosted provider
// (GitHub, GitLab, Bitbucket) is reachable from an instance.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	runConnectRepo = "connect-repo"

	runRepoProviderSSM = "Secure Source Manager (Developer Connect)"
	runBuildDockerfile = "Dockerfile"
	runBuildPacks      = "Go, Node.js, Python, Java, .NET Core, Ruby or PHP via Google Cloud's buildpacks"

	// runPlaceholderImage is the image Google's console gives a service
	// until its repository's first build deploys.
	runPlaceholderImage = "us-docker.pkg.dev/cloudrun/container/placeholder"
	// runDeployManagedBy is the label and tag Google's trigger carries.
	runDeployManagedBy = "gcp-cloud-build-deploy-cloud-run"
)

// repoLinks are the project's Developer Connect git repository links.
func repoLinks(project string) []string {
	var out []string
	for _, c := range runadapter.DeveloperConnectConnections(project) {
		for _, l := range runadapter.DeveloperConnectLinks(c.GetName()) {
			out = append(out, l.GetName())
		}
	}
	return out
}

// connectRepoAction is Connect repo, offered where Cloud Build is served
// with Cloud Run (on the Run port).
func (p runProvider) connectRepoAction(project string) console.Action {
	_, create := p.CreateForm()
	var name console.Field
	for _, f := range create {
		if f.Name == "name" {
			name = f
		}
	}
	name.Section, name.Tab = "Configure", ""
	links := repoLinks(p.project(project))
	repoHelp := "A Developer Connect git repository link to one of this instance's Secure Source Manager repositories."
	if len(links) == 0 {
		repoHelp = "No repository is linked yet: on Secure Source Manager, open a repository and use Link repository, " +
			"then come back."
	}
	return console.Action{ID: runConnectRepo, Label: "Connect repo", Submit: "Create", Fields: []console.Field{
		{Name: "provider", Label: "Repository Provider", Type: "select", Options: []string{runRepoProviderSSM},
			Default: runRepoProviderSSM, Required: true, Section: "Source repository",
			Help: "GitHub, GitLab and Bitbucket are hosted services no instance reaches; this instance's own Secure Source " +
				"Manager repositories are connected through Developer Connect."},
		{Name: "repository", Label: "Repository", Type: "select", Options: links, Required: true,
			Section: "Source repository", Help: repoHelp},
		{Name: "branch", Label: "Branch", Type: "text", Default: "^main$", Required: true, Section: "Build Configuration",
			Help: "A regular expression (RE2): a push to a branch it matches builds and deploys a new revision."},
		{Name: "buildType", Label: "Build Type", Type: "radio", Options: []string{runBuildDockerfile, runBuildPacks},
			Default: runBuildDockerfile, Section: "Build Configuration"},
		{Name: "dockerfile", Label: "Source location", Type: "text", Default: "/Dockerfile", Section: "Build Configuration",
			Help: "Dockerfile only: the Dockerfile's path in the repository."},
		{Name: "context", Label: "Build context directory", Type: "text", Default: "/", Section: "Build Configuration",
			Help: "Buildpacks only: the directory, in the repository, that is built."},
		{Name: "entrypoint", Label: "Entrypoint", Type: "text", Section: "Build Configuration",
			Help: "Buildpacks only, optional: the command the container runs (GOOGLE_ENTRYPOINT)."},
		{Name: "functionTarget", Label: "Function target", Type: "text", Section: "Build Configuration",
			Help: "Buildpacks only, optional: the function the Functions Framework serves (GOOGLE_FUNCTION_TARGET)."},
		name,
		{Name: "region", Label: "Region", Type: "select", Options: []string{p.location()}, Default: p.location(),
			Section: "Configure", Help: "This instance serves Cloud Run in one region."},
	}}
}

func (p runProvider) project(project string) string {
	if project == "" {
		return p.defaultProject
	}
	return project
}

var literalBranchRE = regexp.MustCompile(`^\^([A-Za-z0-9._/-]+)\$$`)

// connectRepoBuild is the build Google's console gives the trigger: Build,
// Push and Deploy, with the substitutions it sets.
func connectRepoBuild(project, region, service string, values map[string]string) (*cbpb.Build, map[string]string, error) {
	image := "$_AR_HOSTNAME/$_AR_PROJECT_ID/$_AR_REPOSITORY/$REPO_NAME/$_SERVICE_NAME:$COMMIT_SHA"
	var build *cbpb.BuildStep
	switch values["buildType"] {
	case "", runBuildDockerfile:
		df := strings.TrimPrefix(strings.TrimSpace(values["dockerfile"]), "/")
		if df == "" {
			df = "Dockerfile"
		}
		if strings.Contains(df, "..") {
			return nil, nil, fmt.Errorf("source location %q leaves the repository", values["dockerfile"])
		}
		build = &cbpb.BuildStep{Id: "Build", Name: "gcr.io/cloud-builders/docker",
			Args: []string{"build", "--no-cache", "-t", image, ".", "-f", df}}
	case runBuildPacks:
		dir := strings.Trim(strings.TrimSpace(values["context"]), "/")
		if dir == "" {
			dir = "."
		}
		if strings.Contains(dir, "..") {
			return nil, nil, fmt.Errorf("build context directory %q leaves the repository", values["context"])
		}
		args := []string{"build", image, "--builder=gcr.io/buildpacks/builder:latest", "--network=cloudbuild", "--path=" + dir}
		if e := strings.TrimSpace(values["entrypoint"]); e != "" {
			args = append(args, "--env=GOOGLE_ENTRYPOINT="+e)
		}
		if f := strings.TrimSpace(values["functionTarget"]); f != "" {
			args = append(args, "--env=GOOGLE_FUNCTION_TARGET="+f)
		}
		build = &cbpb.BuildStep{Id: "Buildpack", Name: "gcr.io/k8s-skaffold/pack", Entrypoint: "pack", Args: args}
	default:
		return nil, nil, fmt.Errorf("build type %q is not Dockerfile or buildpacks", values["buildType"])
	}
	b := &cbpb.Build{
		Steps: []*cbpb.BuildStep{
			build,
			{Id: "Push", Name: "gcr.io/cloud-builders/docker", Args: []string{"push", image}},
			{Id: "Deploy", Name: "gcr.io/google.com/cloudsdktool/cloud-sdk:slim", Entrypoint: "gcloud", Args: []string{
				"run", "services", "update", "$_SERVICE_NAME", "--platform=managed", "--image=" + image,
				"--labels=managed-by=" + runDeployManagedBy + ",commit-sha=$COMMIT_SHA,gcb-build-id=$BUILD_ID,gcb-trigger-id=$_TRIGGER_ID",
				"--region=$_DEPLOY_REGION", "--quiet"}},
		},
		Images:  []string{image},
		Options: &cbpb.BuildOptions{SubstitutionOption: cbpb.BuildOptions_ALLOW_LOOSE},
		Tags:    []string{runDeployManagedBy, runDeployManagedBy + "-managed", service},
	}
	subs := map[string]string{
		"_AR_HOSTNAME": region + "-docker.pkg.dev", "_AR_PROJECT_ID": project, "_AR_REPOSITORY": "cloud-run-source-deploy",
		"_PLATFORM": "managed", "_SERVICE_NAME": service, "_DEPLOY_REGION": region,
	}
	return b, subs, nil
}

// connectRepo makes the service and its trigger, and runs the trigger.
func (p runProvider) connectRepo(ctx context.Context, project string, values map[string]string) error {
	project = p.project(project)
	if project == "" {
		return errors.New("choose a project before connecting a repository")
	}
	if values["provider"] != "" && values["provider"] != runRepoProviderSSM {
		return fmt.Errorf("repository provider %q is not served here", values["provider"])
	}
	link := strings.TrimSpace(values["repository"])
	found := false
	for _, l := range repoLinks(project) {
		found = found || l == link
	}
	if !found {
		return fmt.Errorf("repository %q is not a git repository link of project %s", link, project)
	}
	service := strings.TrimSpace(values["name"])
	if !runServiceNameRE.MatchString(service) {
		return fmt.Errorf("service name %q: lowercase letters, numbers and hyphens, starting with a letter, at most 49", service)
	}
	branch := strings.TrimSpace(values["branch"])
	if branch == "" {
		branch = "^main$"
	}
	if _, err := regexp.Compile(branch); err != nil {
		return fmt.Errorf("branch %q is not a regular expression: %v", branch, err)
	}
	region := p.location()
	build, subs, err := connectRepoBuild(project, region, service, values)
	if err != nil {
		return err
	}

	sc, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sc.Close() }()
	parent := "projects/" + project + "/locations/" + region
	if _, err := sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: runPlaceholderImage}}},
	}}); err != nil {
		return err
	}
	// The service's first revision becomes ready on its own; the trigger
	// replaces its image with each build.
	return runBuilds(p.runEndpoint(), ctx, func(c *cloudbuild.Client) error {
		t, err := c.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: project, Trigger: &cbpb.BuildTrigger{
			Name:        runTriggerName(service),
			Description: "Build and deploy to Cloud Run service " + service + " on push to \"" + branch + "\"",
			Tags:        []string{runDeployManagedBy, runDeployManagedBy + "-managed", service},
			RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: link,
				Filter: &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: branch}}}},
			BuildTemplate: &cbpb.BuildTrigger_Build{Build: build},
			Substitutions: subs,
		}})
		if err != nil {
			return fmt.Errorf("create the Cloud Build trigger: %w", err)
		}
		t.Substitutions["_TRIGGER_ID"] = t.GetId()
		if t, err = c.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: project, TriggerId: t.GetId(),
			Trigger: t, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"substitutions"}}}); err != nil {
			return fmt.Errorf("set the trigger's _TRIGGER_ID: %w", err)
		}
		src := &cbpb.RepoSource{}
		if m := literalBranchRE.FindStringSubmatch(branch); m != nil {
			src.Revision = &cbpb.RepoSource_BranchName{BranchName: m[1]}
		}
		// An empty repository has nothing to build yet: its first push will.
		if _, err := c.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: project,
			TriggerId: t.GetId(), Source: src}); err != nil && status.Code(err) != codes.FailedPrecondition {
			return fmt.Errorf("run the trigger: %w", err)
		}
		return nil
	})
}

var runServiceNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,47}[a-z0-9])?$`)

// runTriggerName is the trigger's name, as Google's console names it, cut
// to a trigger name's 64 characters.
func runTriggerName(service string) string {
	n := service + "-trigger"
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// runBuilds dials Cloud Build on the Run port.
func runBuilds(addr string, ctx context.Context, fn func(*cloudbuild.Client) error) error {
	return runBuildsProvider{runAddr: func() string { return addr }}.withClient(ctx, fn)
}
