package main

// Cloud Build's Triggers and Worker pools pages (#1422): create, run and
// delete a trigger, create and delete a pool, through the official client
// on the Run port, as the API does.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/proto"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// buildTriggersProvider is the Triggers page.
type buildTriggersProvider struct {
	b runBuildsProvider
	// registry lists the project's IAM service accounts, the Service
	// account drop-down's choices (#1854); nil offers the default only.
	registry *resourcemanager.Registry
}

func (buildTriggersProvider) ID() string    { return "cloud-build-triggers" }
func (buildTriggersProvider) Title() string { return "Cloud Build triggers" }

func (p buildTriggersProvider) project(project string) string {
	if project == "" {
		return p.b.defaultProject
	}
	return project
}

func (p buildTriggersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Description", "Region", "Repository", "Event", "Build configuration", "Pool", "Approval", "Age"},
		NameColumn: "Name", Noun: "triggers", AlwaysStatus: true, RowsOpenable: true,
		Note: "Push to a branch and Push new tag triggers run on a Developer Connect link to one of this instance's Secure Source Manager " +
			"repositories; manual, webhook and Pub/Sub triggers run an inline build. A Pub/Sub trigger's builds, which arrive from the " +
			"cluster, run docker build, docker push and pack steps only. GitHub, GitLab and Bitbucket need a hosted git service."}
	for _, t := range runadapter.Triggers(p.project(project), "") {
		region := strings.Split(t.GetResourceName(), "/")[3]
		st := "Enabled"
		if t.GetDisabled() {
			st = "Disabled"
		}
		repo := "-"
		if r := t.GetRepositoryEventConfig().GetRepository(); r != "" {
			repo = r[strings.LastIndex(r, "/")+1:]
		}
		out.Items = append(out.Items, console.Resource{Name: t.GetName(), Status: st, Fields: map[string]string{
			"Region": region, "Event": triggerEvent(t), "Description": orDash(t.GetDescription()), "Repository": repo,
			"Build configuration": triggerConfigLabel(t),
			"Pool":                orDash(t.GetBuild().GetOptions().GetPool().GetName()), "Approval": approvalLabel(t.GetResourceName()),
			"Age": stampAge(t.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// triggerEvent is the Event column, in Google's words.
func triggerEvent(t *cbpb.BuildTrigger) string {
	switch {
	case t.GetPubsubConfig() != nil:
		return "Pub/Sub message: " + t.GetPubsubConfig().GetTopic()
	case t.GetWebhookConfig() != nil:
		return "Webhook event"
	}
	// A trigger on a Developer Connect link (#1852).
	if push := t.GetRepositoryEventConfig().GetPush(); push != nil {
		if push.GetTag() != "" {
			return "Push new tag: " + push.GetTag()
		}
		return "Push to branch: " + push.GetBranch()
	}
	return "Manual invocation"
}

// triggerConfigLabel is the Build configuration column.
func triggerConfigLabel(t *cbpb.BuildTrigger) string {
	switch {
	case t.GetAutodetect():
		return "Autodetected"
	case t.GetFilename() != "":
		return "Cloud Build configuration file: " + t.GetFilename()
	}
	return "Inline"
}

// find resolves a trigger by its name, which may be in any region.
func (p buildTriggersProvider) find(project, name string) (*cbpb.BuildTrigger, error) {
	var match []*cbpb.BuildTrigger
	for _, t := range runadapter.Triggers(p.project(project), "") {
		if t.GetName() == name {
			match = append(match, t)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("trigger %q not found", name)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("trigger %q is in %d regions; use gcloud builds triggers --region", name, len(match))
}

// CreateForm is Google's Create trigger page (consolebuildtriggerform.go).
func (p buildTriggersProvider) CreateForm() (string, []console.Field) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return "Create trigger", p.triggerFields(ctx, "", nil)
}

// CreateOnPage, CreateLayout and CreateSubmit draw Create trigger as one
// page under its headings, ending in Create, as Google's is.
func (buildTriggersProvider) CreateOnPage() bool   { return true }
func (buildTriggersProvider) CreateLayout() string { return console.CreateLayoutPage }
func (buildTriggersProvider) CreateSubmit() string { return "Create" }
func (buildTriggersProvider) DeleteLabel() string  { return "Delete" }
func (buildTriggersProvider) RowEditLabel() string { return "Edit" }

// RowEditFirst begins the row's menu with Edit: Google's reads Edit,
// Disable, Delete (#1854).
func (buildTriggersProvider) RowEditFirst() bool { return true }

// CreateFormFor is Create trigger for the selected project (#1854): its
// Repository lists that project's linked repositories, its Private pool
// that project's pools and its Service account that project's accounts.
func (p buildTriggersProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return p.triggerFields(ctx, project, nil)
}

// approvalLabel is the Approval column: whether the trigger's builds wait
// for approval (#1449).
func approvalLabel(resourceName string) string {
	if runadapter.TriggerNeedsApproval(resourceName) {
		return "Required"
	}
	return "-"
}

func (p buildTriggersProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	t, err := triggerFromValues(v)
	if err != nil {
		return "", err
	}
	err = p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		made, err := c.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{ProjectId: project,
			Parent: "projects/" + project + "/locations/" + region, Trigger: t})
		if err != nil || (v["approval"] != "true" && v["approval"] != "on") {
			return err
		}
		// The Go client's protos have no approvalConfig; the JSON API
		// carries it, and the console sets it as that API does.
		return runadapter.SetTriggerApproval(made.GetResourceName(), true)
	})
	return t.GetName(), err
}

// Actions offers Run on an enabled manual trigger, as Google's Triggers
// page does; a webhook trigger runs from its URL.
func (p buildTriggersProvider) Actions(r console.Resource) []console.Action {
	var acts []console.Action
	ev := r.Fields["Event"]
	if r.Status == "Enabled" && (ev == "Manual invocation" || strings.HasPrefix(ev, "Push ")) {
		// A button on the row, as Google's (#1854).
		acts = append(acts, console.Action{ID: "run", Label: "Run", Button: true, Confirm: "a build runs from the trigger's build config"})
	}
	// Disable and Enable in the row's menu (#1854), as Google's.
	if r.Status == "Disabled" {
		return append(acts, console.Action{ID: "enable-trigger", Label: "Enable"})
	}
	return append(acts, console.Action{ID: "disable-trigger", Label: "Disable",
		Confirm: "A disabled trigger starts no build, from Run, a push, its topic or its webhook, until it is enabled."})
}

func (p buildTriggersProvider) Act(ctx context.Context, project, name, action string) error {
	if action == "enable-trigger" || action == "disable-trigger" {
		return p.ActAt(ctx, project, []string{name}, action, nil)
	}
	if action != "run" {
		return fmt.Errorf("unknown action %q", action)
	}
	t, err := p.find(project, name)
	if err != nil {
		return err
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		_, err := c.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: p.project(project), TriggerId: t.GetId()})
		return err
	})
}

func (p buildTriggersProvider) Delete(ctx context.Context, project, name string) error {
	t, err := p.find(project, name)
	if err != nil {
		return err
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		return c.DeleteBuildTrigger(ctx, &cbpb.DeleteBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: p.project(project), TriggerId: t.GetId()})
	})
}

// buildWorkerPoolsProvider is the Worker pools page.
type buildWorkerPoolsProvider struct{ b runBuildsProvider }

func (buildWorkerPoolsProvider) ID() string    { return "cloud-build-worker-pools" }
func (buildWorkerPoolsProvider) Title() string { return "Cloud Build worker pools" }

func (p buildWorkerPoolsProvider) project(project string) string {
	if project == "" {
		return p.b.defaultProject
	}
	return project
}

func (p buildWorkerPoolsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Region", "Machine type", "Disk (GB)", "Display name", "Age"},
		NameColumn: "Pool", Noun: "worker pools", AlwaysStatus: true, RowsOpenable: true,
		Note: "A build that names a pool runs on the host's Docker, like every build here; the pool is recorded on the build."}
	for _, w := range runadapter.WorkerPools(p.project(project)) {
		parts := strings.Split(w.GetName(), "/")
		wc := w.GetPrivatePoolV1Config().GetWorkerConfig()
		disk := "-"
		if wc.GetDiskSizeGb() != 0 {
			disk = fmt.Sprint(wc.GetDiskSizeGb())
		}
		st := "Running"
		if w.GetState() != cbpb.WorkerPool_RUNNING {
			st = w.GetState().String()
		}
		// The pool's name opens Edit private pool, as Google's does.
		edit := "/cloud-build/worker-pools/" + url.PathEscape(parts[5]) + "?edit=1&project=" + url.QueryEscape(p.project(project))
		out.Items = append(out.Items, console.Resource{Name: parts[5], Status: st, Link: edit, Fields: map[string]string{
			"Region": parts[3], "Machine type": orDash(wc.GetMachineType()), "Disk (GB)": disk,
			"Display name": orDash(w.GetDisplayName()), "Age": stampAge(w.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// Create private pool (#1854), as Google's "Create and manage private
// pools" page draws it: Name and Region; Machine configuration (Series,
// Machine type, Disk size, Nested virtualization for C3); Worker release
// (a release channel or a release); Network type (Default network or a
// Private network's project, network and IP range); Assign external IPs.
// The pool is stored as CreateWorkerPool takes it; a build on it runs on
// the host's Docker, whatever its machine or network.
func (buildWorkerPoolsProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "Can only contain alphanumeric characters [a-z][0-9] or dashes -; 1 to 63 characters."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Options: poolRegions, Default: "us-central1"},
		{Name: "series", Label: "Series", Type: "select", Section: "Machine configuration", Options: []string{"e2", "n1", "c3"},
			OptionLabels: map[string]string{"e2": "E2", "n1": "N1", "c3": "C3"}, Default: "e2"},
	}
	for _, series := range []string{"e2", "n1", "c3"} {
		fields = append(fields, console.Field{Name: "machineType-" + series, Label: "Machine type", Type: "select",
			Section: "Machine configuration", Options: poolMachineTypes[series], Default: poolMachineTypes[series][0],
			ShowWhen: "series=" + series})
	}
	return "Create private pool", append(fields,
		console.Field{Name: "diskSizeGb", Label: "Disk size", Type: "number", Section: "Machine configuration",
			Pattern: `^$|^([1-9][0-9]{2}|[1-3][0-9]{3}|4000)$`, Help: "100 to 4000 GB. Left blank, Cloud Build uses 100."},
		console.Field{Name: "nestedVirtualization", Label: "Nested virtualization", Type: "checkbox", Section: "Machine configuration",
			ShowWhen: "series=c3", Help: "Run VMs inside the build's VM. C3 machines only."},
		console.Field{Name: "release", Label: "Worker release", Type: "radio", Section: "Worker release",
			Options: []string{"channel", "version"}, OptionLabels: map[string]string{"channel": "Release channel", "version": "Release"}, Default: "channel"},
		console.Field{Name: "channel", Label: "Release channel", Type: "select", Section: "Worker release", ShowWhen: "release=channel",
			Options: []string{"regular", "stable", "rapid"}, Default: "regular"},
		console.Field{Name: "version", Label: "Release", Type: "text", Section: "Worker release", ShowWhen: "release=version",
			Help: "A worker release version number."},
		console.Field{Name: "network", Label: "Network type", Type: "radio", Section: "Network type",
			Options: []string{"default", "private"}, OptionLabels: map[string]string{"default": "Default network", "private": "Private network"},
			Default: "default", Help: "Default network: the pool uses the service producer network. Private network: workers peer to a VPC network of yours."},
		console.Field{Name: "networkProject", Label: "Project", Type: "text", Section: "Network type", ShowWhen: "network=private"},
		console.Field{Name: "networkName", Label: "Network", Type: "text", Section: "Network type", ShowWhen: "network=private",
			Pattern: `^$|^[a-z]([a-z0-9\-]*[a-z0-9])?$`},
		console.Field{Name: "ipRange", Label: "IP range", Type: "text", Section: "Network type", ShowWhen: "network=private",
			Pattern: `^$|^([0-9]{1,3}(\.[0-9]{1,3}){3})?/([0-9]|1[0-9]|2[0-9])$`,
			Help:    "Optional. CIDR notation, such as 192.0.2.0/24; the prefix can't exceed /29, and /24 is assigned if empty."},
		console.Field{Name: "externalIPs", Label: "Assign external IPs", Type: "checkbox", Section: "Network type", Default: "true",
			Help: "Lets the pool reach the public internet. Clear it to restrict access to your private network."},
	)
}

// poolRegions are the regions Create private pool offers.
var poolRegions = []string{"us-central1", "us-east1", "us-west1", "europe-west1", "europe-west2", "asia-east1", "asia-northeast1"}

// poolMachineTypes are each series' machine types, as Google's drop-down
// lists them.
var poolMachineTypes = map[string][]string{
	"e2": {"e2-standard-2", "e2-standard-4", "e2-standard-8", "e2-standard-16", "e2-standard-32", "e2-medium",
		"e2-highcpu-2", "e2-highcpu-4", "e2-highcpu-8", "e2-highcpu-16", "e2-highcpu-32", "e2-highmem-2", "e2-highmem-4", "e2-highmem-8", "e2-highmem-16"},
	"n1": {"n1-standard-1", "n1-standard-2", "n1-standard-4", "n1-standard-8", "n1-standard-16", "n1-standard-32", "n1-standard-64", "n1-standard-96",
		"n1-highcpu-8", "n1-highcpu-32"},
	"c3": {"c3-standard-4", "c3-standard-8", "c3-standard-22", "c3-standard-44", "c3-standard-88", "c3-standard-176"},
}

// CreateOnPage, CreateLayout and CreateSubmit draw Create private pool as
// one page under its headings, ending in Create, as Google's is.
func (buildWorkerPoolsProvider) CreateOnPage() bool   { return true }
func (buildWorkerPoolsProvider) CreateLayout() string { return console.CreateLayoutPage }
func (buildWorkerPoolsProvider) CreateSubmit() string { return "Create" }

// poolFromValues is the pool Create private pool describes.
func poolFromValues(v map[string]string) (*cbpb.WorkerPool, error) {
	on := func(k string) bool { return v[k] == "true" || v[k] == "on" }
	series := orText(strings.TrimSpace(v["series"]), "e2")
	mt := strings.TrimSpace(v["machineType-"+series])
	if mt == "" {
		// The API callers' older form names the machine type alone.
		mt = strings.TrimSpace(v["machineType"])
	}
	wc := &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: mt}
	if d := strings.TrimSpace(v["diskSizeGb"]); d != "" {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil || n < 100 || n > 4000 {
			return nil, fmt.Errorf("disk size %q: enter a value from 100 to 4000", d)
		}
		wc.DiskSizeGb = n
	}
	if series == "c3" && on("nestedVirtualization") {
		wc.EnableNestedVirtualization = proto.Bool(true)
	}
	switch v["release"] {
	case "version":
		wc.WorkerRelease = strings.TrimSpace(v["version"])
	case "channel":
		wc.WorkerRelease = orText(strings.TrimSpace(v["channel"]), "regular")
	}
	cfg := &cbpb.PrivatePoolV1Config{WorkerConfig: wc}
	egress := cbpb.PrivatePoolV1Config_NetworkConfig_PUBLIC_EGRESS
	if _, set := v["externalIPs"]; set && !on("externalIPs") {
		egress = cbpb.PrivatePoolV1Config_NetworkConfig_NO_PUBLIC_EGRESS
	}
	nc := &cbpb.PrivatePoolV1Config_NetworkConfig{EgressOption: egress}
	if v["network"] == "private" {
		project, network := strings.TrimSpace(v["networkProject"]), strings.TrimSpace(v["networkName"])
		if project == "" || network == "" {
			return nil, fmt.Errorf("a private network needs its project and network")
		}
		nc.PeeredNetwork = "projects/" + project + "/global/networks/" + network
		nc.PeeredNetworkIpRange = strings.TrimSpace(v["ipRange"])
	} else if strings.TrimSpace(v["ipRange"]) != "" {
		return nil, fmt.Errorf("an IP range is set only after choosing a private network")
	}
	if v["network"] != "" || v["externalIPs"] != "" {
		cfg.NetworkConfig = nc
	}
	return &cbpb.WorkerPool{Config: &cbpb.WorkerPool_PrivatePoolV1Config{PrivatePoolV1Config: cfg}}, nil
}

func (p buildWorkerPoolsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	name := strings.TrimSpace(v["name"])
	pool, err := poolFromValues(v)
	if err != nil {
		return "", err
	}
	err = p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: "projects/" + project + "/locations/" + strings.TrimSpace(v["region"]),
			WorkerPoolId: name, WorkerPool: pool})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return name, err
}

func (p buildWorkerPoolsProvider) Delete(ctx context.Context, project, name string) error {
	var match []string
	for _, w := range runadapter.WorkerPools(p.project(project)) {
		if strings.HasSuffix(w.GetName(), "/workerPools/"+name) {
			match = append(match, w.GetName())
		}
	}
	if len(match) != 1 {
		return fmt.Errorf("worker pool %q is in %d regions; use gcloud builds worker-pools delete --region", name, len(match))
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.DeleteWorkerPool(ctx, &cbpb.DeleteWorkerPoolRequest{Name: match[0]})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

var (
	_ console.CreateLayouter      = buildTriggersProvider{}
	_ console.PageCreator         = buildTriggersProvider{}
	_ console.CreateSubmitter     = buildTriggersProvider{}
	_ console.Actor               = buildTriggersProvider{}
	_ console.DeleteLabeler       = buildTriggersProvider{}
	_ console.RowEditor           = buildTriggersProvider{}
	_ console.RowEditLeader       = buildTriggersProvider{}
	_ console.ProjectCreateFormer = buildTriggersProvider{}
	_ console.Creator             = buildWorkerPoolsProvider{}
	_ console.PageCreator         = buildWorkerPoolsProvider{}
	_ console.CreateLayouter      = buildWorkerPoolsProvider{}
	_ console.CreateSubmitter     = buildWorkerPoolsProvider{}
	_ console.Deleter             = buildWorkerPoolsProvider{}
)
