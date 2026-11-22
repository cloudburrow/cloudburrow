package main

// Colab Enterprise's Runtimes and Runtime templates pages (#2105), in
// Google's layout (https://docs.cloud.google.com/colab/docs/create-runtime-template,
// create-runtime, manage-runtimes, runtimes; read 2026-11-22):
//
//   - Runtime templates: a Region menu, the ID column opening the
//     template's details, each row's Actions "Set as Default", and New
//     template, the "Create new runtime template" dialog of Runtime basics
//     (Display name, Region, Description, labels), Configure compute
//     (Machine type, accelerators, Data disk type and size, idle shutdown
//     in minutes from 10 to 1440), Environment (Environment, Post-startup
//     script and its behaviour, environment variables) and Networking and
//     security (Network, Subnetwork, Enable public internet access, Enable
//     end-user credentials), each section ending in Continue and the dialog
//     in Create. A template's page has Edit, the "Edit runtime template"
//     panel saved with Update, which changes what the API's update mask
//     allows.
//   - Runtimes: a Region menu, Create (the "Create Agent Platform runtime"
//     dialog: Runtime template and Runtime name), and Start, Stop and
//     Delete (with Confirm).
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/colab/docs/manage-runtimes):
// both lists' columns, which Google does not list, are the API's fields a
// runtime or template is told apart by; a runtime is assigned to the
// instance's local user, developer@cloudburrow.local, the dialog having no
// Runtime user field.
//
// Every read and write goes through the official aiplatform NotebookClient
// to the instance's local Vertex AI endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	colabRegionKey = "region"
	colabUser      = "developer@cloudburrow.local"
	colabBudget    = 35 * time.Minute
)

var colabDiskTypes = []string{"pd-standard", "pd-balanced", "pd-ssd", "pd-extreme", "hyperdisk-balanced"}

var colabDiskLabels = map[string]string{"pd-standard": "Standard persistent disk", "pd-balanced": "Balanced persistent disk",
	"pd-ssd": "SSD persistent disk", "pd-extreme": "Extreme persistent disk", "hyperdisk-balanced": "Hyperdisk Balanced"}

var colabMachineTypes = []string{"e2-standard-2", "e2-standard-4", "e2-standard-8", "e2-standard-16", "e2-highmem-2", "e2-highmem-4",
	"e2-highmem-8", "n1-standard-4", "n1-standard-8", "n2-standard-4", "n2-standard-8", "g2-standard-4", "a2-highgpu-1g"}

var colabAccelerators = []string{"NONE", "NVIDIA_TESLA_T4", "NVIDIA_L4", "NVIDIA_TESLA_V100", "NVIDIA_TESLA_A100", "NVIDIA_A100_80GB"}

func (v vertexAPI) notebooks(ctx context.Context) (*aiplatform.NotebookClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewNotebookClient(ctx, v.opts()...)
}

func colabRegion(ctx context.Context) string {
	if r := console.ScopeValue(ctx, colabRegionKey); r != "" {
		return r
	}
	return "us-central1"
}

func colabScope(ctx context.Context) *console.Scope {
	s := &console.Scope{Key: colabRegionKey, Label: "Region", Selected: colabRegion(ctx), Default: "us-central1"}
	for _, r := range vertexRegions {
		s.Options = append(s.Options, console.ScopeOption{ID: r})
	}
	return s
}

func colabParent(ctx context.Context, project string) string {
	return "projects/" + project + "/locations/" + colabRegion(ctx)
}

func colabIdle(c *aiplatformpb.NotebookIdleShutdownConfig) string {
	if c == nil || c.GetIdleShutdownDisabled() {
		return "Disabled"
	}
	return fmt.Sprintf("%d minutes", int(c.GetIdleTimeout().AsDuration().Minutes()))
}

func colabEnvLine(env []*aiplatformpb.EnvVar) string {
	var out []string
	for _, e := range env {
		out = append(out, e.GetName()+"="+e.GetValue())
	}
	return strings.Join(out, "\n")
}

func colabParseEnv(s string) ([]*aiplatformpb.EnvVar, error) {
	m, err := console.ParseMap(s)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*aiplatformpb.EnvVar
	for _, k := range keys {
		out = append(out, &aiplatformpb.EnvVar{Name: k, Value: m[k]})
	}
	return out, nil
}

var colabScriptBehaviours = []string{"RUN_ONCE", "RUN_EVERY_START", "DOWNLOAD_AND_RUN_EVERY_START"}

var colabScriptLabels = map[string]string{"RUN_ONCE": "Run once", "RUN_EVERY_START": "Run every start", "DOWNLOAD_AND_RUN_EVERY_START": "Download and run every start"}

// ---- Runtime templates

type colabTemplatesProvider struct{ api vertexAPI }

func (colabTemplatesProvider) ID() string                    { return "colab-templates" }
func (colabTemplatesProvider) Title() string                 { return "Runtime templates" }
func (colabTemplatesProvider) ScopeKey() string              { return colabRegionKey }
func (colabTemplatesProvider) CreateTitle() string           { return "Create new runtime template" }
func (colabTemplatesProvider) CreateSubmit() string          { return "Create" }
func (colabTemplatesProvider) MutationBudget() time.Duration { return time.Minute }

func (p colabTemplatesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Display name", "Machine type", "Data disk", "Idle shutdown", "Last modified"},
		NameColumn: "ID", Noun: "runtime templates", Scope: colabScope(ctx),
		Note: "A runtime from a template runs on this instance's notebook runtime, a container beside its BigQuery, on this machine's CPU."}
	if project == "" {
		l.Prompt = "Runtime templates are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	parent := colabParent(ctx, project)
	for it := c.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent}); ; {
		t, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		disk := t.GetDataPersistentDiskSpec()
		l.Items = append(l.Items, console.Resource{Name: vertexLast(t.GetName()), Fields: map[string]string{
			"Display name": t.GetDisplayName(), "Machine type": t.GetMachineSpec().GetMachineType(),
			"Data disk":     fmt.Sprintf("%d GB %s", disk.GetDiskSizeGb(), disk.GetDiskType()),
			"Idle shutdown": colabIdle(t.GetIdleShutdownConfig()), "Last modified": vertexTime(t.GetUpdateTime())},
			Actions: []console.Action{{ID: "set-default", Label: "Set as Default"}}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (colabTemplatesProvider) CreateForm() (string, []console.Field) {
	const basics, compute, env, net = "Runtime basics", "Configure compute", "Environment", "Networking and security"
	return "New template", []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Section: basics, Pattern: `^.{1,128}$`, Help: "Up to 128 characters."},
		{Name: "region", Label: "Region", Type: "select", Options: vertexRegions, Default: "us-central1", Required: true, Section: basics},
		{Name: "description", Label: "Description", Type: "textarea", Section: basics},
		{Name: "labels", Label: "Labels", Type: "map", Section: basics, MaxEntries: 64, KeyPattern: `^[a-z][a-z0-9_\-]{0,63}$`, ValuePattern: `^[a-z0-9_\-]{0,64}$`,
			KeyHelp: "Up to 64 lowercase letters, digits, underscores and hyphens, starting with a letter.", ValueHelp: "Up to 64 lowercase letters, digits, underscores and hyphens."},
		{Name: "machineType", Label: "Machine type", Type: "select", Options: colabMachineTypes, Default: "e2-standard-4", Required: true, Section: compute},
		{Name: "acceleratorType", Label: "Accelerator type", Type: "select", Options: colabAccelerators, Default: "NONE", Section: compute,
			OptionLabels: map[string]string{"NONE": "None"}, Help: "A runtime with accelerators cannot start here: this instance's runtimes run on the machine's CPU."},
		{Name: "acceleratorCount", Label: "Accelerator count", Type: "text", Default: "1", Pattern: `^[1-8]$`, Section: compute, ShowWhen: "acceleratorType=NVIDIA_TESLA_T4|NVIDIA_L4|NVIDIA_TESLA_V100|NVIDIA_TESLA_A100|NVIDIA_A100_80GB"},
		{Name: "diskType", Label: "Data disk type", Type: "select", Options: colabDiskTypes, OptionLabels: colabDiskLabels, Default: "pd-standard", Section: compute},
		{Name: "diskSize", Label: "Data disk size (GB)", Type: "text", Default: "100", Pattern: `^[0-9]{2,5}$`, Section: compute, Help: "From 10 to 65536."},
		{Name: "idleShutdown", Label: "Enable idle shutdown", Type: "checkbox", Default: "true", Section: compute},
		{Name: "idleMinutes", Label: "Time of inactivity before shutdown (Minutes)", Type: "text", Default: "180", Pattern: `^[0-9]{2,4}$`, Section: compute,
			ShowWhen: "idleShutdown", Help: "Any integer value from 10 to 1440."},
		{Name: "environment", Label: "Environment", Type: "select", Options: []string{"latest"}, OptionLabels: map[string]string{"latest": "Latest"}, Default: "latest", Section: env,
			Help: "The instance's notebook runtime image: Python with Google's BigQuery client and bigquery-magics."},
		{Name: "postStartupScript", Label: "Post-startup script", Type: "textarea", Section: env,
			Help: "Optional: a bash script run in the runtime after it starts. A gs:// script is not fetched by this instance; paste the script."},
		{Name: "postStartupBehavior", Label: "Post-startup script behavior", Type: "select", Options: colabScriptBehaviours, OptionLabels: colabScriptLabels, Default: "RUN_ONCE", Section: env},
		{Name: "env", Label: "Environment variables", Type: "map", Section: env, MaxEntries: 100, KeyPattern: `^[A-Za-z_][A-Za-z0-9_]*$`, KeyHelp: "A variable name: letters, digits and underscores."},
		{Name: "network", Label: "Network", Type: "text", Section: net, Pattern: `^(projects/[^\/]+/global/networks/[^\/]+)?$`,
			Help: "Optional: projects/PROJECT/global/networks/NETWORK; if you don't select one, your default network is selected."},
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Section: net, Pattern: `^(projects/[^\/]+/regions/[^\/]+/subnetworks/[^\/]+)?$`,
			Help: "Optional: projects/PROJECT/regions/REGION/subnetworks/SUBNETWORK."},
		{Name: "internet", Label: "Enable public internet access", Type: "checkbox", Default: "true", Section: net,
			Help: "Recorded on the template. A runtime here never has a route to the internet: its only peer is the instance's BigQuery."},
		{Name: "euc", Label: "Enable end-user credentials", Type: "checkbox", Default: "true", Section: net},
	}
}

func colabTemplateFrom(values map[string]string) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	env, err := colabParseEnv(values["env"])
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(values["diskSize"]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("data disk size: %q is not a number", values["diskSize"])
	}
	t := &aiplatformpb.NotebookRuntimeTemplate{DisplayName: strings.TrimSpace(values["displayName"]), Description: values["description"], Labels: labels,
		MachineSpec:            &aiplatformpb.MachineSpec{MachineType: values["machineType"]},
		DataPersistentDiskSpec: &aiplatformpb.PersistentDiskSpec{DiskType: values["diskType"], DiskSizeGb: size},
		NetworkSpec: &aiplatformpb.NetworkSpec{EnableInternetAccess: values["internet"] == "true", Network: strings.TrimSpace(values["network"]),
			Subnetwork: strings.TrimSpace(values["subnetwork"])},
		EucConfig:      &aiplatformpb.NotebookEucConfig{EucDisabled: values["euc"] != "true"},
		SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: env}}
	if a := values["acceleratorType"]; a != "" && a != "NONE" {
		n, err := strconv.Atoi(strings.TrimSpace(values["acceleratorCount"]))
		if err != nil {
			return nil, fmt.Errorf("accelerator count: %q is not a number", values["acceleratorCount"])
		}
		t.MachineSpec.AcceleratorType = aiplatformpb.AcceleratorType(aiplatformpb.AcceleratorType_value[a])
		t.MachineSpec.AcceleratorCount = int32(n)
	}
	if values["idleShutdown"] == "true" {
		m, err := strconv.Atoi(strings.TrimSpace(values["idleMinutes"]))
		if err != nil || m < 10 || m > 1440 {
			return nil, errors.New("time of inactivity before shutdown: any integer value from 10 to 1440")
		}
		t.IdleShutdownConfig = &aiplatformpb.NotebookIdleShutdownConfig{IdleTimeout: durationpb.New(time.Duration(m) * time.Minute)}
	} else {
		t.IdleShutdownConfig = &aiplatformpb.NotebookIdleShutdownConfig{IdleShutdownDisabled: true}
	}
	if s := values["postStartupScript"]; strings.TrimSpace(s) != "" {
		t.SoftwareConfig.PostStartupScriptConfig = &aiplatformpb.PostStartupScriptConfig{PostStartupScript: s,
			PostStartupScriptBehavior: aiplatformpb.PostStartupScriptConfig_PostStartupScriptBehavior(aiplatformpb.PostStartupScriptConfig_PostStartupScriptBehavior_value[values["postStartupBehavior"]])}
	}
	return t, nil
}

func (p colabTemplatesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a runtime template")
	}
	t, err := colabTemplateFrom(values)
	if err != nil {
		return "", err
	}
	region := values["region"]
	if region == "" {
		region = "us-central1"
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: "projects/" + project + "/locations/" + region, NotebookRuntimeTemplate: t})
	if err != nil {
		return "", err
	}
	out, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	return vertexLast(out.GetName()), nil
}

func (p colabTemplatesProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteNotebookRuntimeTemplate(ctx, &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: colabParent(ctx, project) + "/notebookRuntimeTemplates/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (p colabTemplatesProvider) get(ctx context.Context, project, id string) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.GetNotebookRuntimeTemplate(ctx, &aiplatformpb.GetNotebookRuntimeTemplateRequest{Name: colabParent(ctx, project) + "/notebookRuntimeTemplates/" + id})
}

func (p colabTemplatesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	m, disk, n := t.GetMachineSpec(), t.GetDataPersistentDiskSpec(), t.GetNetworkSpec()
	accel := "None"
	if m.GetAcceleratorType() != aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED {
		accel = fmt.Sprintf("%s x %d", m.GetAcceleratorType(), m.GetAcceleratorCount())
	}
	sc := t.GetSoftwareConfig()
	yes := map[bool]string{true: "Enabled", false: "Disabled"}
	d := console.Detail{Title: t.GetDisplayName(), Summary: []console.Property{
		{Label: "ID", Value: vertexLast(t.GetName())},
		{Label: "Name", Value: t.GetName(), Copy: t.GetName()},
		{Label: "Region", Value: vertexLocation(t.GetName())},
		{Label: "Description", Value: dashIfEmpty(t.GetDescription())},
		{Label: "Labels", Value: labelsLine(t.GetLabels())},
		{Label: "Created", Value: vertexTime(t.GetCreateTime())},
	}}
	d.Sections = []console.Section{
		{ID: "compute", Label: "Compute", Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Machine type", Value: m.GetMachineType()}, {Label: "Accelerator", Value: accel},
			{Label: "Data disk type", Value: disk.GetDiskType()}, {Label: "Data disk size", Value: fmt.Sprintf("%d GB", disk.GetDiskSizeGb())},
			{Label: "Idle shutdown", Value: colabIdle(t.GetIdleShutdownConfig())}}}}},
		{ID: "environment", Label: "Environment", Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Environment", Value: "Latest"},
			{Label: "Post-startup script", Value: dashIfEmpty(sc.GetPostStartupScriptConfig().GetPostStartupScript())},
			{Label: "Post-startup script behavior", Value: dashIfEmpty(colabScriptLabels[sc.GetPostStartupScriptConfig().GetPostStartupScriptBehavior().String()])},
			{Label: "Environment variables", Value: dashIfEmpty(strings.ReplaceAll(colabEnvLine(sc.GetEnv()), "\n", ", "))}}}}},
		{ID: "networking", Label: "Networking and security", Groups: []console.PropertyGroup{{Properties: []console.Property{
			{Label: "Network", Value: dashIfEmpty(n.GetNetwork())}, {Label: "Subnetwork", Value: dashIfEmpty(n.GetSubnetwork())},
			{Label: "Public internet access", Value: yes[n.GetEnableInternetAccess()]}}}}},
	}
	d.Edit = &console.EditForm{Label: "Edit", Title: "Edit runtime template", Submit: "Update", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: t.GetDisplayName(), Pattern: `^.{1,128}$`},
		{Name: "postStartupScript", Label: "Post-startup script", Type: "textarea", Default: sc.GetPostStartupScriptConfig().GetPostStartupScript()},
		{Name: "postStartupBehavior", Label: "Post-startup script behavior", Type: "select", Options: colabScriptBehaviours, OptionLabels: colabScriptLabels,
			Default: strings.TrimPrefix(sc.GetPostStartupScriptConfig().GetPostStartupScriptBehavior().String(), "POST_STARTUP_SCRIPT_BEHAVIOR_UNSPECIFIED")},
		{Name: "env", Label: "Environment variables", Type: "map", Default: colabEnvLine(sc.GetEnv()), MaxEntries: 100, KeyPattern: `^[A-Za-z_][A-Za-z0-9_]*$`},
	}, Note: "A template's compute and networking cannot be changed after it is created; make a new template for those."}
	return d, nil
}

func (p colabTemplatesProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("edit a runtime template from its page")
	}
	env, err := colabParseEnv(values["env"])
	if err != nil {
		return err
	}
	behavior := aiplatformpb.PostStartupScriptConfig_PostStartupScriptBehavior(aiplatformpb.PostStartupScriptConfig_PostStartupScriptBehavior_value[values["postStartupBehavior"]])
	t := &aiplatformpb.NotebookRuntimeTemplate{Name: colabParent(ctx, project) + "/notebookRuntimeTemplates/" + path[0], DisplayName: strings.TrimSpace(values["displayName"]),
		SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: env, PostStartupScriptConfig: &aiplatformpb.PostStartupScriptConfig{
			PostStartupScript: values["postStartupScript"], PostStartupScriptBehavior: behavior}}}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{NotebookRuntimeTemplate: t,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "software_config.env",
			"software_config.post_startup_script_config.post_startup_script", "software_config.post_startup_script_config.post_startup_script_behavior"}}})
	return err
}

func (p colabTemplatesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{{ID: "set-default", Label: "Set as Default"}}
}

func (p colabTemplatesProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 || action != "set-default" {
		return fmt.Errorf("unknown action %q", action)
	}
	if p.api.at() == "" {
		return errors.New("the local AI endpoint is not running")
	}
	return vertexSetDefault(ctx, p.api.at(), colabParent(ctx, project)+"/notebookRuntimeTemplates/"+path[0])
}

// vertexSetDefault is notebookRuntimeTemplates:setDefault, which the v1
// discovery document has and the client libraries do not.
func vertexSetDefault(ctx context.Context, addr, name string) error {
	parent := name[:strings.Index(name, "/notebookRuntimeTemplates/")]
	body, _ := json.Marshal(map[string]string{"name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/v1/"+parent+"/notebookRuntimeTemplates:setDefault", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("setDefault: %s", resp.Status)
	}
	return nil
}

// Actions are a row's: Set as Default.
func (colabTemplatesProvider) Actions(r console.Resource) []console.Action { return r.Actions }

// Act performs a row's action.
func (p colabTemplatesProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}

// ---- Runtimes

type colabRuntimesProvider struct{ api vertexAPI }

func (colabRuntimesProvider) ID() string                    { return "colab-runtimes" }
func (colabRuntimesProvider) Title() string                 { return "Runtimes" }
func (colabRuntimesProvider) ScopeKey() string              { return colabRegionKey }
func (colabRuntimesProvider) CreateTitle() string           { return "Create Agent Platform runtime" }
func (colabRuntimesProvider) MutationBudget() time.Duration { return colabBudget }

func colabRuntimeState(s aiplatformpb.NotebookRuntime_RuntimeState) string {
	switch s {
	case aiplatformpb.NotebookRuntime_RUNNING:
		return "Running"
	case aiplatformpb.NotebookRuntime_BEING_STARTED:
		return "Starting"
	case aiplatformpb.NotebookRuntime_BEING_STOPPED:
		return "Stopping"
	case aiplatformpb.NotebookRuntime_STOPPED:
		return "Stopped"
	case aiplatformpb.NotebookRuntime_BEING_UPGRADED:
		return "Upgrading"
	case aiplatformpb.NotebookRuntime_ERROR:
		return "Error"
	}
	return "Unknown"
}

func (p colabRuntimesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Runtime name", "Runtime template", "Runtime user", "Machine type", "Idle shutdown", "Expires", "Created"},
		NameColumn: "ID", Noun: "runtimes", Scope: colabScope(ctx), AlwaysStatus: true,
		Note: "Each runtime is a notebook runtime of this instance: a container whose only route is this instance's BigQuery. Stop deletes the container; Start makes a new one."}
	if project == "" {
		l.Prompt = "Runtimes are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListNotebookRuntimes(ctx, &aiplatformpb.ListNotebookRuntimesRequest{Parent: colabParent(ctx, project)}); ; {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		state := r.GetRuntimeState()
		var actions []console.Action
		if state == aiplatformpb.NotebookRuntime_STOPPED || state == aiplatformpb.NotebookRuntime_ERROR {
			actions = append(actions, console.Action{ID: "start", Label: "Start"})
		}
		if state == aiplatformpb.NotebookRuntime_RUNNING || state == aiplatformpb.NotebookRuntime_BEING_STARTED {
			actions = append(actions, console.Action{ID: "stop", Label: "Stop"})
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(r.GetName()), Status: colabRuntimeState(state), Actions: actions, Fields: map[string]string{
			"Runtime name": r.GetDisplayName(), "Runtime template": vertexLast(r.GetNotebookRuntimeTemplateRef().GetNotebookRuntimeTemplate()),
			"Runtime user": r.GetRuntimeUser(), "Machine type": r.GetMachineSpec().GetMachineType(), "Idle shutdown": colabIdle(r.GetIdleShutdownConfig()),
			"Expires": vertexTime(r.GetExpirationTime()), "Created": vertexTime(r.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (colabRuntimesProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "notebookRuntimeTemplate", Label: "Runtime template", Type: "select", Required: true},
		{Name: "displayName", Label: "Runtime name", Type: "text", Required: true, Pattern: `^.{1,128}$`, Help: "Up to 128 characters."},
	}
}

// CreateFormFor offers the region's runtime templates.
func (p colabRuntimesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	_, fields := p.CreateForm()
	if project == "" {
		return fields
	}
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return fields
	}
	defer func() { _ = c.Close() }()
	labels := map[string]string{}
	for it := c.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: colabParent(ctx, project)}); ; {
		t, err := it.Next()
		if err != nil {
			break
		}
		id := vertexLast(t.GetName())
		fields[0].Options = append(fields[0].Options, id)
		labels[id] = t.GetDisplayName() + " (" + id + ")"
	}
	fields[0].OptionLabels = labels
	if len(fields[0].Options) == 0 {
		fields[0].Help = "No runtime templates in " + colabRegion(ctx) + ": create one on the Runtime templates page."
	} else {
		fields[0].Default = fields[0].Options[0]
	}
	return fields
}

func (p colabRuntimesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a runtime")
	}
	tpl := strings.TrimSpace(values["notebookRuntimeTemplate"])
	if tpl == "" {
		return "", errors.New("choose a runtime template")
	}
	parent := colabParent(ctx, project)
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: parent + "/notebookRuntimeTemplates/" + tpl,
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: strings.TrimSpace(values["displayName"]), RuntimeUser: colabUser}})
	if err != nil {
		return "", err
	}
	r, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	return vertexLast(r.GetName()), nil
}

func (p colabRuntimesProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteNotebookRuntime(ctx, &aiplatformpb.DeleteNotebookRuntimeRequest{Name: colabParent(ctx, project) + "/notebookRuntimes/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (p colabRuntimesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{{ID: "start", Label: "Start"}, {ID: "stop", Label: "Stop"}}
}

func (p colabRuntimesProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("choose a runtime")
	}
	name := colabParent(ctx, project) + "/notebookRuntimes/" + path[0]
	c, err := p.api.notebooks(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	switch action {
	case "start":
		op, err := c.StartNotebookRuntime(ctx, &aiplatformpb.StartNotebookRuntimeRequest{Name: name})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "stop":
		op, err := c.StopNotebookRuntime(ctx, &aiplatformpb.StopNotebookRuntimeRequest{Name: name})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// Actions are a row's: Start or Stop, as its state allows.
func (colabRuntimesProvider) Actions(r console.Resource) []console.Action { return r.Actions }

// Act performs a row's action.
func (p colabRuntimesProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}
