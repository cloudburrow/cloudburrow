package main

// Cloud Build's Create trigger as Google's single page (#1854): Name,
// Region, Description; Event; Source (a Developer Connect repository and
// the branch or tag regex); Configuration (Autodetected, a Cloud Build
// configuration file in the repository or inline, a Dockerfile, or
// Buildpacks); Substitution variables; Approval; Service account. Order
// and wording from
// https://docs.cloud.google.com/build/docs/automating-builds/create-manage-triggers.
// The same fields, prefilled, are the trigger's Edit form.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Google's event and configuration choices, worded as its form words them.
const (
	evPushBranch  = "Push to a branch"
	evPushTag     = "Push new tag"
	evPullRequest = "Pull request"
	evManual      = "Manual invocation"
	evPubsub      = "Pub/Sub message"
	evWebhook     = "Webhook event"

	cfgAuto       = "Autodetected"
	cfgFile       = "Cloud Build configuration file (yaml or json)"
	cfgDockerfile = "Dockerfile"
	cfgBuildpacks = "Buildpacks"

	locRepository = "Repository"
	locInline     = "Inline"

	// defaultServiceAccountChoice is the Service account drop-down's one
	// choice: builds run as the project's default build service account.
	defaultServiceAccountChoice = "default"

	defaultInlineBuild = "steps:\n- name: alpine:3.20\n  entrypoint: sh\n  args: ['-c', 'echo triggered by $TRIGGER_NAME']\n"
)

// triggerRegions are the Region choices: global, then the regions a
// Developer Connect connection can be in here.
var triggerRegions = []string{"global", "us-central1", "us-east1", "us-west1", "europe-west1", "europe-west2", "asia-east1", "asia-northeast1"}

var substitutionKeyRE = `^_[A-Z0-9_]+$`

// triggerFields are Create trigger's fields; t, when set, prefills them as
// its Edit form, with the name and region fixed.
func (p buildTriggersProvider) triggerFields(ctx context.Context, project string, t *cbpb.BuildTrigger) []console.Field {
	project = p.project(project)
	links := repoLinks(project)
	repoHelp := "A Developer Connect git repository link to one of this instance's Secure Source Manager repositories " +
		"(Repositories, 2nd gen). Needed by Push to a branch and Push new tag, and by every configuration read from the repository."
	if len(links) == 0 {
		repoHelp = "No repository is linked yet: on Repositories (2nd gen), create a host connection and link a repository."
	}
	pools := []string{""}
	for _, w := range runadapter.WorkerPools(project) {
		pools = append(pools, w.GetName())
	}
	sa := "the default Cloud Build service account"
	if groups := p.defaultServiceAccount(ctx, project); len(groups) > 0 && len(groups[0].Properties) > 0 {
		sa = groups[0].Properties[0].Value
	}
	// The project's IAM service accounts (#1854), by email, as Google lists them.
	saOptions, saLabels := []string{defaultServiceAccountChoice}, map[string]string{defaultServiceAccountChoice: sa}
	for _, email := range p.serviceAccounts(project) {
		name := "projects/" + project + "/serviceAccounts/" + email
		saOptions, saLabels[name] = append(saOptions, name), email
	}
	if t != nil && t.GetServiceAccount() != "" && saLabels[t.GetServiceAccount()] == "" {
		saOptions, saLabels[t.GetServiceAccount()] = append(saOptions, t.GetServiceAccount()), t.GetServiceAccount()
	}
	fileShown := "config=" + cfgFile
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-zA-Z0-9][\\-a-zA-Z0-9_]{0,62}$",
			Help: "Must be unique within the project. 1-64 letters, digits, - or _."},
		{Name: "region", Label: "Region", Type: "select", Options: triggerRegions, Default: "global",
			Help: "A 2nd gen repository's trigger is in its connection's region."},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "tags", Label: "Tags", Type: "text", Help: "Optional: comma separated."},

		{Name: "event", Label: "Event", Type: "radio", Section: "Event", Default: evPushBranch,
			Options:            []string{evPushBranch, evPushTag, evPullRequest, evManual, evPubsub, evWebhook},
			UnavailableOptions: map[string]string{evPullRequest: "Pull request triggers do not run: this instance's Secure Source Manager serves pull requests but runs no build when one is opened or updated (#1927), so it takes pushes only."},
			Help:               "Repository event that invokes trigger, or another event."},
		{Name: "topic", Label: "Subscription", Type: "text", Section: "Event", ShowWhen: "event=" + evPubsub,
			Pattern: `^projects/[^\/]+/topics/[^\/]+$`,
			Help:    "The Pub/Sub topic, projects/PROJECT/topics/TOPIC, on this instance's Pub/Sub; $(body.message.data.KEY) in a substitution reads the message."},
		{Name: "secret", Label: "Secret", Type: "text", Section: "Event", ShowWhen: "event=" + evWebhook,
			Pattern: `^projects/[^\/]+/secrets/[^\/]+/versions/[^\/]+$`,
			Help:    "A Secret Manager version, projects/PROJECT/secrets/SECRET/versions/N: the webhook URL's secret."},

		{Name: "repository", Label: "Repository", Type: "select", Options: append([]string{""}, links...), Section: "Source", Help: repoHelp},
		{Name: "branch", Label: "Branch", Type: "text", Section: "Source", Default: "^main$", ShowWhen: "event=" + evPushBranch,
			Help: "A regular expression (RE2) matched against the pushed branch."},
		{Name: "tag", Label: "Tag", Type: "text", Section: "Source", Default: ".*", ShowWhen: "event=" + evPushTag,
			Help: "A regular expression (RE2) matched against the pushed tag."},
		{Name: "invert", Label: "Invert Regex", Type: "checkbox", Section: "Source",
			Help: "Builds every branch or tag the regular expression does not match."},

		{Name: "config", Label: "Type", Type: "radio", Section: "Configuration", Default: cfgAuto,
			Options: []string{cfgAuto, cfgFile, cfgDockerfile, cfgBuildpacks},
			Help:    "Read from the repository at the pushed commit; a manual, Pub/Sub or webhook trigger takes an inline configuration."},
		{Name: "location", Label: "Location", Type: "radio", Section: "Configuration", Default: locRepository,
			Options: []string{locRepository, locInline}, ShowWhen: fileShown},
		{Name: "filename", Label: "Cloud Build configuration file location", Type: "text", Section: "Configuration",
			Default: "/cloudbuild.yaml", ShowWhen: fileShown + "&location=" + locRepository},
		{Name: "inline", Label: "Inline", Type: "textarea", Section: "Configuration", Default: defaultInlineBuild,
			ShowWhen: fileShown + "&location=" + locInline, Help: "The build config, YAML or JSON."},
		{Name: "dockerfile", Label: "Dockerfile name", Type: "text", Section: "Configuration", Default: "Dockerfile",
			ShowWhen: "config=" + cfgDockerfile},
		{Name: "dockerDir", Label: "Dockerfile directory", Type: "text", Section: "Configuration", Default: "/",
			ShowWhen: "config=" + cfgDockerfile},
		{Name: "imageName", Label: "Image name", Type: "text", Section: "Configuration",
			Default: "gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA", ShowWhen: "config=" + cfgDockerfile,
			Help: "Dockerfile and Buildpacks: the image built and pushed."},
		{Name: "packImage", Label: "Image", Type: "text", Section: "Configuration",
			Default: "gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA", ShowWhen: "config=" + cfgBuildpacks},
		{Name: "packPath", Label: "Path", Type: "text", Section: "Configuration", Default: "/", ShowWhen: "config=" + cfgBuildpacks,
			Help: "The directory, in the repository, that Google Cloud's buildpacks build."},

		{Name: "substitutions", Label: "Substitution variables", Type: "map", Section: "Advanced",
			KeyPattern: substitutionKeyRE, KeyHelp: "a variable begins with an underscore, then capital letters, digits and underscores",
			Help: "User-defined substitutions, such as _DEPLOY_REGION."},
		{Name: "approval", Label: "Require approval before build executes", Type: "checkbox", Section: "Advanced",
			Help: "Each build waits, Awaiting approval, until someone approves it on History."},
		{Name: "pool", Label: "Private pool", Type: "select", Options: pools, Section: "Advanced",
			OptionLabels: map[string]string{"": "Default pool"},
			Help:         "A worker pool on Worker pools; a build that names one runs on the host's Docker, with the pool recorded."},
		{Name: "serviceAccount", Label: "Service account", Type: "select", Section: "Advanced", Default: defaultServiceAccountChoice,
			Options: saOptions, OptionLabels: saLabels,
			Help: "The account the trigger's builds run as: the metadata server names it to every step, and its tokens are its. " +
				"A user-specified account's build must log to its own bucket (logsBucket) or to Cloud Logging only (options.logging: CLOUD_LOGGING_ONLY)."},
	}
	if t == nil {
		return fields
	}
	v := triggerValues(t)
	for i := range fields {
		f := &fields[i]
		if val, ok := v[f.Name]; ok {
			f.Default = val
		} else if f.Type == "checkbox" {
			f.Default = ""
		}
		if f.Name == "name" || f.Name == "region" {
			f.Immutable = true
		}
	}
	return fields
}

// triggerValues are a trigger's form values, for its Edit form.
func triggerValues(t *cbpb.BuildTrigger) map[string]string {
	v := map[string]string{"name": t.GetName(), "region": strings.Split(t.GetResourceName(), "/")[3],
		"description": t.GetDescription(), "tags": strings.Join(t.GetTags(), ", "), "event": evManual,
		"pool": t.GetBuild().GetOptions().GetPool().GetName(), "substitutions": console.FormatMap(t.GetSubstitutions())}
	if t.GetServiceAccount() != "" {
		v["serviceAccount"] = t.GetServiceAccount()
	}
	switch {
	case t.GetPubsubConfig() != nil:
		v["event"], v["topic"] = evPubsub, t.GetPubsubConfig().GetTopic()
	case t.GetWebhookConfig() != nil:
		v["event"], v["secret"] = evWebhook, t.GetWebhookConfig().GetSecret()
	}
	if rc := t.GetRepositoryEventConfig(); rc != nil {
		v["repository"] = rc.GetRepository()
		if push := rc.GetPush(); push != nil {
			if push.GetTag() != "" {
				v["event"], v["tag"] = evPushTag, push.GetTag()
			} else {
				v["event"], v["branch"] = evPushBranch, push.GetBranch()
			}
			if push.GetInvertRegex() {
				v["invert"] = "true"
			}
		}
	}
	if runadapter.TriggerNeedsApproval(t.GetResourceName()) {
		v["approval"] = "true"
	}
	switch {
	case t.GetAutodetect():
		v["config"] = cfgAuto
	case t.GetFilename() != "":
		v["config"], v["location"], v["filename"] = cfgFile, locRepository, t.GetFilename()
	default:
		v["config"], v["location"] = cfgFile, locInline
		if b := t.GetBuild(); b != nil {
			plain := &cbpb.Build{Steps: b.GetSteps(), Images: b.GetImages(), Substitutions: b.GetSubstitutions(),
				Timeout: b.GetTimeout(), Tags: b.GetTags(), LogsBucket: b.GetLogsBucket()}
			if l := b.GetOptions().GetLogging(); l != cbpb.BuildOptions_LOGGING_UNSPECIFIED {
				// A user-specified account's build keeps its logging (#1854).
				plain.Options = &cbpb.BuildOptions{Logging: l}
			}
			if j, err := protojson.Marshal(plain); err == nil {
				var doc any
				if json.Unmarshal(j, &doc) == nil {
					if y, err := yaml.Marshal(doc); err == nil {
						v["inline"] = string(y)
					}
				}
			}
		}
	}
	return v
}

// triggerFromValues is the trigger, and whether it needs approval, that a
// Create trigger or Edit form describes. A form without an Event is the
// older one-step form (an image and a sh -c command, with a topic or
// webhook secret implying the event), which the API callers of the
// console still send.
func triggerFromValues(v map[string]string) (*cbpb.BuildTrigger, error) {
	t := &cbpb.BuildTrigger{Name: strings.TrimSpace(v["name"]), Description: strings.TrimSpace(v["description"])}
	for _, tag := range strings.Split(v["tags"], ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			t.Tags = append(t.Tags, tag)
		}
	}
	subs, err := console.ParseMap(v["substitutions"])
	if err != nil {
		return nil, fmt.Errorf("substitution variables: %w", err)
	}
	if len(subs) > 0 {
		t.Substitutions = subs
	}
	if sa := strings.TrimSpace(v["serviceAccount"]); sa != "" && sa != defaultServiceAccountChoice {
		t.ServiceAccount = sa
	}
	event := v["event"]
	if event == "" {
		// The one-step form.
		image := strings.TrimSpace(v["image"])
		if image == "" || strings.TrimSpace(v["command"]) == "" {
			return nil, fmt.Errorf("a step image and a command are required")
		}
		t.BuildTemplate = &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: image, Entrypoint: "sh", Args: []string{"-c", v["command"]}}}}}
		event = evManual
		if strings.TrimSpace(v["secret"]) != "" {
			event = evWebhook
		}
		if strings.TrimSpace(v["topic"]) != "" {
			event = evPubsub
		}
	}
	repo := strings.TrimSpace(v["repository"])
	onRepo := event == evPushBranch || event == evPushTag
	switch event {
	case evPushBranch, evPushTag:
		if repo == "" {
			return nil, fmt.Errorf("%s needs a repository: link one on Repositories (2nd gen)", event)
		}
		push := &cbpb.PushFilter{InvertRegex: v["invert"] == "true" || v["invert"] == "on"}
		if event == evPushBranch {
			push.GitRef = &cbpb.PushFilter_Branch{Branch: orText(strings.TrimSpace(v["branch"]), ".*")}
		} else {
			push.GitRef = &cbpb.PushFilter_Tag{Tag: orText(strings.TrimSpace(v["tag"]), ".*")}
		}
		t.RepositoryEventConfig = &cbpb.RepositoryEventConfig{Repository: repo, Filter: &cbpb.RepositoryEventConfig_Push{Push: push}}
	case evPullRequest:
		return nil, fmt.Errorf("pull request triggers are not served: Secure Source Manager here takes pushes only")
	case evManual:
	case evPubsub:
		t.PubsubConfig = &cbpb.PubsubConfig{Topic: strings.TrimSpace(v["topic"])}
	case evWebhook:
		t.WebhookConfig = &cbpb.WebhookConfig{AuthMethod: &cbpb.WebhookConfig_Secret{Secret: strings.TrimSpace(v["secret"])}}
	default:
		return nil, fmt.Errorf("unknown event %q", event)
	}
	if t.BuildTemplate == nil {
		if err := applyTriggerConfig(t, v, onRepo); err != nil {
			return nil, err
		}
	}
	if pool := strings.TrimSpace(v["pool"]); pool != "" {
		if b := t.GetBuild(); b != nil {
			if b.Options == nil {
				b.Options = &cbpb.BuildOptions{}
			}
			b.Options.Pool = &cbpb.BuildOptions_PoolOption{Name: pool}
		} else {
			return nil, fmt.Errorf("a private pool is set in the build config file's options.pool.name when the configuration is read from the repository")
		}
	}
	return t, nil
}

// applyTriggerConfig sets the Configuration section's build template:
// Autodetected and a configuration file in the repository are read at the
// pushed commit; Inline is the build itself; Dockerfile and Buildpacks are
// the inline build Google's console writes for them, which builds the
// commit's /workspace and pushes the image.
func applyTriggerConfig(t *cbpb.BuildTrigger, v map[string]string, onRepo bool) error {
	config, location := orText(v["config"], cfgAuto), orText(v["location"], locRepository)
	needsRepo := func() error {
		if !onRepo {
			return fmt.Errorf("configuration %q reads the repository: choose Push to a branch or Push new tag, or an Inline configuration file", config)
		}
		return nil
	}
	clean := func(p string) (string, error) {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if strings.Contains(p, "..") {
			return "", fmt.Errorf("path %q leaves the repository", p)
		}
		return p, nil
	}
	switch config {
	case cfgAuto:
		if err := needsRepo(); err != nil {
			return err
		}
		t.BuildTemplate = &cbpb.BuildTrigger_Autodetect{Autodetect: true}
	case cfgFile:
		if location == locInline {
			b, err := runadapter.ParseBuildConfig([]byte(v["inline"]))
			if err != nil {
				return fmt.Errorf("inline build config: %w", err)
			}
			if len(b.GetSteps()) == 0 {
				return fmt.Errorf("inline build config: a build needs at least one step")
			}
			t.BuildTemplate = &cbpb.BuildTrigger_Build{Build: b}
			return nil
		}
		if err := needsRepo(); err != nil {
			return err
		}
		f, err := clean(orText(v["filename"], "cloudbuild.yaml"))
		if err != nil {
			return err
		}
		t.BuildTemplate = &cbpb.BuildTrigger_Filename{Filename: f}
	case cfgDockerfile:
		if err := needsRepo(); err != nil {
			return err
		}
		dir, err := clean(v["dockerDir"])
		if err != nil {
			return err
		}
		df, err := clean(orText(v["dockerfile"], "Dockerfile"))
		if err != nil {
			return err
		}
		image := orText(strings.TrimSpace(v["imageName"]), "gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA")
		step := &cbpb.BuildStep{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", image, "-f", df, "."}}
		if dir != "" {
			step.Dir = dir
		}
		t.BuildTemplate = &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{step}, Images: []string{image}}}
	case cfgBuildpacks:
		if err := needsRepo(); err != nil {
			return err
		}
		dir, err := clean(v["packPath"])
		if err != nil {
			return err
		}
		image := orText(strings.TrimSpace(v["packImage"]), "gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA")
		args := []string{"build", image, "--builder=gcr.io/buildpacks/builder:latest", "--network=cloudbuild", "--path=" + orText(dir, ".")}
		t.BuildTemplate = &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "gcr.io/k8s-skaffold/pack", Entrypoint: "pack", Args: args}},
			Images: []string{image}}}
	default:
		return fmt.Errorf("unknown configuration %q", config)
	}
	return nil
}

// orText is s, or d when s is empty.
func orText(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// serviceAccounts are the project's live IAM service accounts' emails, in
// the registry's order, or none without a registry.
func (p buildTriggersProvider) serviceAccounts(project string) []string {
	if p.registry == nil {
		return nil
	}
	var out []string
	resp, err := p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100})
	for err == nil {
		for _, a := range resp.GetAccounts() {
			if !a.GetDisabled() {
				out = append(out, a.GetEmail())
			}
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		resp, err = p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100, PageToken: resp.GetNextPageToken()})
	}
	return out
}
