package main

// Vertex AI Workbench's Instances page (#2173), in Google's words
// (https://docs.cloud.google.com/vertex-ai/docs/workbench/instances/create-console-quickstart,
// .../instances/create, idle-shutdown; read 2026-11-22):
//
//   - "Go to the Instances page"; "Click Create new"; "For Name, enter
//     my-instance"; "Click Create";
//   - "The Open JupyterLab link is disabled while the instance
//     provisions"; "Next to the instance's name, click Open JupyterLab";
//   - "select the instance and click Stop" / "Start"; "click the Reset
//     icon, then Reset to confirm" (a reset "forcibly wipes the memory
//     contents"); "click Delete ... To confirm, click Confirm";
//   - Advanced options: Region and Zone, Machine type, Idle shutdown
//     ("an integer from 10 through 1440"), Single user's User email.
//
// Local differences: JupyterLab is not served; Open JupyterLab opens the
// instance's page, whose code box runs Python on the instance's kernel.
// UNVERIFIED (re-read 2026-11-22, the same pages): the list's columns, the
// zones offered and the machine types offered.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	notebooks "cloud.google.com/go/notebooks/apiv2"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

var (
	workbenchZones        = []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-east1-b", "us-east4-a", "us-west1-a", "europe-west1-b", "europe-west4-a", "asia-east1-a", "asia-northeast1-a"}
	workbenchMachineTypes = []string{"n1-standard-4", "n1-standard-8", "e2-standard-2", "e2-standard-4", "e2-standard-8", "n2-standard-4", "n2-standard-8"}
)

const workbenchRoute = "/agent-platform/workbench/instances"

func (v vertexAPI) workbench(ctx context.Context) (*notebooks.NotebookClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return notebooks.NewNotebookClient(ctx, v.opts()...)
}

// workbenchProvider's exec runs Python on an instance (the local Vertex AI
// endpoint's WorkbenchExecute), nil when it is not served in-process.
type workbenchProvider struct {
	api  vertexAPI
	exec func(ctx context.Context, name, code string) ([]json.RawMessage, error)
}

func (workbenchProvider) ID() string                    { return "workbench-instances" }
func (workbenchProvider) Title() string                 { return "Instances" }
func (workbenchProvider) CreateTitle() string           { return "Create instance" }
func (workbenchProvider) MutationBudget() time.Duration { return colabBudget }

func workbenchStateText(s nbpb.State) string {
	switch s {
	case nbpb.State_ACTIVE:
		return "Active"
	case nbpb.State_STARTING, nbpb.State_PROVISIONING, nbpb.State_INITIALIZING:
		return "Provisioning"
	case nbpb.State_STOPPING:
		return "Stopping"
	case nbpb.State_STOPPED:
		return "Stopped"
	case nbpb.State_UPGRADING:
		return "Upgrading"
	}
	return "Unknown"
}

func workbenchZone(name string) string {
	p := strings.Split(name, "/")
	if len(p) >= 4 {
		return p[3]
	}
	return ""
}

func workbenchIdleText(md map[string]string) string {
	v := md["idle-timeout-seconds"]
	if v == "" {
		return "180 minutes"
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return v
	}
	return fmt.Sprintf("%d minutes", n/60)
}

func workbenchLabels(m map[string]string) string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, k+": "+m[k])
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// instances lists every location's instances.
func (p workbenchProvider) instances(ctx context.Context, project string) ([]*nbpb.Instance, error) {
	c, err := p.api.workbench(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []*nbpb.Instance
	for it := c.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"}); ; {
		in, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return vertexLast(out[i].GetName()) < vertexLast(out[j].GetName()) })
	return out, nil
}

// find is the instance of id, in any zone.
func (p workbenchProvider) find(ctx context.Context, project, id string) (*nbpb.Instance, error) {
	all, err := p.instances(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, in := range all {
		if vertexLast(in.GetName()) == id {
			return in, nil
		}
	}
	return nil, fmt.Errorf("instance %q not found", id)
}

func workbenchActions(project string, in *nbpb.Instance) []console.Action {
	switch in.GetState() {
	case nbpb.State_ACTIVE:
		return []console.Action{
			{ID: "open-jupyterlab", Label: "Open JupyterLab", Href: workbenchRoute + "/" + url.PathEscape(vertexLast(in.GetName())) + "?project=" + url.QueryEscape(project)},
			{ID: "stop", Label: "Stop"},
			{ID: "reset", Label: "Reset", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "A reset forcibly wipes the memory contents of the instance."},
		}
	case nbpb.State_STOPPED:
		return []console.Action{{ID: "start", Label: "Start"}}
	}
	return nil
}

func (p workbenchProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Zone", "Machine type", "Owner", "Idle shutdown", "Labels", "Created"},
		NameColumn: "Instance name", Noun: "instances", AlwaysStatus: true,
		Note: "Each instance is a notebook runtime of this CloudBurrow instance: a kernel container whose only route is its BigQuery. " +
			"JupyterLab is not served: Open JupyterLab opens the instance's page, which runs Python on its kernel. Stop deletes the container; Start makes a new one."}
	if project == "" {
		l.Prompt = "Instances are listed per project. Choose one in the toolbar."
		return l, nil
	}
	all, err := p.instances(ctx, project)
	if err != nil {
		return l, err
	}
	for _, in := range all {
		owner := strings.Join(in.GetInstanceOwners(), ", ")
		if owner == "" && len(in.GetGceSetup().GetServiceAccounts()) == 1 {
			owner = in.GetGceSetup().GetServiceAccounts()[0].GetEmail()
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(in.GetName()), Status: workbenchStateText(in.GetState()), Actions: workbenchActions(project, in),
			Fields: map[string]string{"Zone": workbenchZone(in.GetName()), "Machine type": in.GetGceSetup().GetMachineType(), "Owner": dashIfEmpty(owner),
				"Idle shutdown": workbenchIdleText(in.GetGceSetup().GetMetadata()), "Labels": workbenchLabels(in.GetLabels()), "Created": vertexTime(in.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p workbenchProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	in, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	g := in.GetGceSetup()
	disk := func(size int64, t nbpb.DiskType) string { return fmt.Sprintf("%d GB, %s", size, t) }
	data := "-"
	if len(g.GetDataDisks()) == 1 {
		data = disk(g.GetDataDisks()[0].GetDiskSizeGb(), g.GetDataDisks()[0].GetDiskType())
	}
	sa := "Compute Engine default service account"
	if len(g.GetServiceAccounts()) == 1 && g.GetServiceAccounts()[0].GetEmail() != "" {
		sa = g.GetServiceAccounts()[0].GetEmail()
	}
	d := console.Detail{Title: vertexLast(in.GetName()), Summary: []console.Property{
		{Label: "Name", Value: in.GetName(), Copy: in.GetName()},
		{Label: "Status", Value: workbenchStateText(in.GetState())},
		{Label: "Zone", Value: workbenchZone(in.GetName())},
		{Label: "Machine type", Value: g.GetMachineType()},
		{Label: "Boot disk", Value: disk(g.GetBootDisk().GetDiskSizeGb(), g.GetBootDisk().GetDiskType())},
		{Label: "Data disk", Value: data},
		{Label: "Idle shutdown", Value: workbenchIdleText(g.GetMetadata())},
		{Label: "Owner", Value: dashIfEmpty(strings.Join(in.GetInstanceOwners(), ", "))},
		{Label: "Service account", Value: sa},
		{Label: "Creator", Value: dashIfEmpty(in.GetCreator())},
		{Label: "Labels", Value: workbenchLabels(in.GetLabels())},
		{Label: "Created", Value: vertexTime(in.GetCreateTime())},
		{Label: "Updated", Value: vertexTime(in.GetUpdateTime())},
	}}
	if msg := in.GetHealthInfo()["error"]; msg != "" {
		d.Summary = append(d.Summary, console.Property{Label: "Error", Value: msg})
	}
	return d, nil
}

func (p workbenchProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	in, err := p.find(ctx, project, path[0])
	if err != nil {
		return nil
	}
	var out []console.Action
	for _, a := range workbenchActions(project, in) {
		if a.ID != "open-jupyterlab" {
			out = append(out, a)
		}
	}
	return out
}

func (workbenchProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "Must start with a letter followed by up to 62 lowercase letters, numbers, or hyphens (-), and cannot end with a hyphen."},
		{Name: "zone", Label: "Zone", Type: "select", Options: workbenchZones, Default: "us-central1-a", Required: true, Section: "Details"},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Details", MaxEntries: 64, KeyPattern: `^[a-z][a-z0-9_\-]{0,63}$`, ValuePattern: `^[a-z0-9_\-]{0,64}$`},
		{Name: "machineType", Label: "Machine type", Type: "select", Options: workbenchMachineTypes, Default: "n1-standard-4", Section: "Machine type"},
		{Name: "idleShutdown", Label: "Enable Idle Shutdown", Type: "checkbox", Default: "true", Section: "Machine type"},
		{Name: "idleMinutes", Label: "Time of inactivity before shutdown (Minutes)", Type: "text", Default: "180", Pattern: `^[0-9]{2,4}$`, Section: "Machine type",
			ShowWhen: "idleShutdown=true", Help: "An integer from 10 through 1440."},
		{Name: "owner", Label: "User email", Type: "text", Section: "IAM and security", Pattern: `^([^@\s]+@[^@\s]+)?$`,
			Help: "Single user: the user account that you want to grant access. Empty: the service account's users."},
		{Name: "serviceAccount", Label: "Service account email", Type: "text", Section: "IAM and security", Pattern: `^([^@\s]+@[^@\s]+)?$`,
			Help: "You need the Service Account User role on it. Empty: the Compute Engine default service account."},
	}
}

func (p workbenchProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating an instance")
	}
	name := strings.TrimSpace(values["name"])
	g := &nbpb.GceSetup{MachineType: values["machineType"], Metadata: map[string]string{}}
	if values["idleShutdown"] == "true" {
		m, err := strconv.Atoi(strings.TrimSpace(values["idleMinutes"]))
		if err != nil || m < 10 || m > 1440 {
			return "", errors.New("Time of inactivity before shutdown must be an integer from 10 through 1440")
		}
		g.Metadata["idle-timeout-seconds"] = strconv.Itoa(m * 60)
	} else {
		// Idle shutdown off: the longest the API takes (UNVERIFIED: Google's
		// metadata for off is not documented on idle-shutdown).
		g.Metadata["idle-timeout-seconds"] = "86400"
	}
	if sa := strings.TrimSpace(values["serviceAccount"]); sa != "" {
		g.ServiceAccounts = []*nbpb.ServiceAccount{{Email: sa}}
	}
	in := &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: g}, Labels: map[string]string{}}
	if o := strings.TrimSpace(values["owner"]); o != "" {
		in.InstanceOwners = []string{o}
	}
	if v := values["labels"]; v != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(v), &m); err == nil {
			in.Labels = m
		}
	}
	c, err := p.api.workbench(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + values["zone"], InstanceId: name, Instance: in})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return name, nil
}

func (workbenchProvider) DeleteWords() console.DeleteWords {
	return console.DeleteWords{Submit: "Confirm"}
}

func (p workbenchProvider) Delete(ctx context.Context, project, id string) error {
	in, err := p.find(ctx, project, id)
	if err != nil {
		return err
	}
	c, err := p.api.workbench(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteInstance(ctx, &nbpb.DeleteInstanceRequest{Name: in.GetName()})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

func (workbenchProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p workbenchProvider) Act(ctx context.Context, project, id, action string) error {
	return p.ActAt(ctx, project, []string{id}, action, nil)
}

func (p workbenchProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("choose an instance")
	}
	in, err := p.find(ctx, project, path[0])
	if err != nil {
		return err
	}
	c, err := p.api.workbench(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	name := in.GetName()
	var wait func(context.Context) error
	switch action {
	case "start":
		op, err := c.StartInstance(ctx, &nbpb.StartInstanceRequest{Name: name})
		if err != nil {
			return err
		}
		wait = func(ctx context.Context) error { _, err := op.Wait(ctx); return err }
	case "stop":
		op, err := c.StopInstance(ctx, &nbpb.StopInstanceRequest{Name: name})
		if err != nil {
			return err
		}
		wait = func(ctx context.Context) error { _, err := op.Wait(ctx); return err }
	case "reset":
		op, err := c.ResetInstance(ctx, &nbpb.ResetInstanceRequest{Name: name})
		if err != nil {
			return err
		}
		wait = func(ctx context.Context) error { _, err := op.Wait(ctx); return err }
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return wait(ctx)
}

// Query runs Python on the instance's kernel: CloudBurrow's stand-in for
// JupyterLab, which it does not serve.
func (p workbenchProvider) Query(ctx context.Context, project string, path []string, statement string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Output"}, NameColumn: "#", Noun: "outputs"}
	if len(path) != 1 {
		return l, errors.New("open an instance to run code on it")
	}
	if p.exec == nil {
		return l, errors.New("running code needs the local Vertex AI endpoint in this process")
	}
	in, err := p.find(ctx, project, path[0])
	if err != nil {
		return l, err
	}
	outs, err := p.exec(ctx, in.GetName(), statement)
	if err != nil {
		return l, err
	}
	for i, raw := range outs {
		l.Items = append(l.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{"Output": workbenchOutputText(raw)}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (workbenchProvider) QueryHint() string {
	return "Python, run on the instance's kernel as a JupyterLab cell is: its state is kept until the instance stops or is reset."
}

// workbenchOutputText is an nbformat output's text.
func workbenchOutputText(raw json.RawMessage) string {
	var o struct {
		OutputType string          `json:"output_type"`
		Text       json.RawMessage `json:"text"`
		Data       map[string]any  `json:"data"`
		Ename      string          `json:"ename"`
		Evalue     string          `json:"evalue"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return string(raw)
	}
	joined := func(r json.RawMessage) string {
		var s string
		if json.Unmarshal(r, &s) == nil {
			return s
		}
		var ss []string
		if json.Unmarshal(r, &ss) == nil {
			return strings.Join(ss, "")
		}
		return string(r)
	}
	switch o.OutputType {
	case "stream":
		return strings.TrimRight(joined(o.Text), "\n")
	case "error":
		return o.Ename + ": " + o.Evalue
	}
	if t, ok := o.Data["text/plain"]; ok {
		b, _ := json.Marshal(t)
		return joined(b)
	}
	return string(raw)
}
