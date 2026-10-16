package main

// Cloud Run worker pools (#1300).
//
// The Worker pools page of the Cloud Run product: /run/worker-pools, a pool's
// page with its revisions and configuration, Deploy worker pool, Edit, Scale
// and Delete. Every read and write goes through the adapter's own WorkerPools
// and Revisions API, the endpoint an SDK dials, so the page cannot show a pool
// the API would not return or accept a configuration it refuses.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runWorkerPoolsProvider is the Worker pools page of Cloud Run.
type runWorkerPoolsProvider struct {
	runAddr        func() string
	defaultProject string
	region         string
	// readyTimeout is the adapter's: how long an operation waits for the
	// pods and route to be ready before it fails (#1348).
	readyTimeout time.Duration
}

// MutationBudget implements console.MutationBudgeter: a create, an update
// and a start wait until the rollout is ready, as a service's deploy does,
// so the console waits as long as the adapter's operation can, and a
// little more for the operation's own answer, rather than the default
// minute (#1348).
func (p runWorkerPoolsProvider) MutationBudget() time.Duration {
	if p.readyTimeout <= 0 {
		return 0
	}
	return p.readyTimeout + 30*time.Second
}

func (runWorkerPoolsProvider) ID() string    { return "run-worker-pools" }
func (runWorkerPoolsProvider) Title() string { return "Cloud Run worker pools" }

const runWorkerPoolNameImmutable = "A worker pool name cannot be changed after it is created."

// runWorkerPoolNotOffered names what the form leaves out because the adapter
// refuses it (docs/coverage/run.md).
const runWorkerPoolNotOffered = "Not offered, because the Cloud Run adapter refuses them: ports (a worker pool " +
	"has no ingress), instance splits across revisions, VPC access, binary authorization and a service mesh."

const actScale = "scale"

func (p runWorkerPoolsProvider) jobs() runJobsProvider {
	return runJobsProvider{runAddr: p.runAddr, defaultProject: p.defaultProject, region: p.region}
}

func (p runWorkerPoolsProvider) poolName(project, pool string) string {
	if strings.HasPrefix(pool, "projects/") {
		return pool
	}
	return p.jobs().parent(project) + "/workerPools/" + pool
}

// withClients dials the WorkerPools and Revisions clients as an SDK would.
func (p runWorkerPoolsProvider) withClients(ctx context.Context,
	fn func(*runclient.WorkerPoolsClient, *runclient.RevisionsClient) error) error {
	addr := p.jobs().endpoint()
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	wc, err := runclient.NewWorkerPoolsClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = wc.Close() }()
	rc, err := runclient.NewRevisionsClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return fn(wc, rc)
}

var runWorkerPoolColumns = []string{"Instances", "Latest revision", "Image", "Age"}

func (p runWorkerPoolsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: runWorkerPoolColumns, NameColumn: "Worker pool", Noun: "worker pools", AlwaysStatus: true}
	project = p.jobs().project(project)
	if project == "" {
		out.Prompt = "Choose a project to list its Cloud Run worker pools."
		return out, nil
	}
	err := p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		it := wc.ListWorkerPools(ctx, &runpb.ListWorkerPoolsRequest{Parent: p.jobs().parent(project)})
		for {
			wp, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out.Items = append(out.Items, workerPoolRow(wp))
		}
	})
	if err != nil {
		out.Unavailable = apiMessage(err)
		return out, nil
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}

func workerPoolStatus(wp *runpb.WorkerPool) string {
	return conditionState(wp.GetTerminalCondition().GetState())
}

func workerPoolImage(wp *runpb.WorkerPool) string {
	if cs := wp.GetTemplate().GetContainers(); len(cs) > 0 {
		return cs[0].GetImage()
	}
	return ""
}

func workerPoolRow(wp *runpb.WorkerPool) console.Resource {
	return console.Resource{
		Name: lastSegment(wp.GetName()), Status: workerPoolStatus(wp),
		Fields: map[string]string{
			"Instances":       fmt.Sprint(wp.GetScaling().GetManualInstanceCount()),
			"Latest revision": orDash(lastSegment(wp.GetLatestCreatedRevision())),
			"Image":           orDash(workerPoolImage(wp)),
			"Age":             stampAge(wp.GetCreateTime()),
		},
	}
}

// --- create, edit, scale and delete -------------------------------------------

func (runWorkerPoolsProvider) CreateForm() (string, []console.Field) {
	return "Deploy worker pool", []console.Field{
		{
			Name: "name", Label: "Worker pool name", Type: "text", Required: true,
			Help:    "Lowercase letters, numbers and hyphens, starting with a letter; at most 63 characters.",
			Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Section: "Worker pool settings",
		},
		runLabelsField("Worker pool settings"),
		{
			Name: "instances", Label: "Number of instances", Type: "text", Default: "1",
			Help:    "Worker pools are scaled manually: this many instances run at all times.",
			Pattern: `^[0-9]{1,4}$`,
			Section: "Worker pool settings",
		},
		{
			Name: "image", Label: "Container image URL", Type: "text", Required: true,
			Help: "A tagged image, such as docker.io/library/busybox:1.36. A locally built one " +
				"is rewritten to dev.local/ and never pulled; an untagged reference is refused.",
			Section: "Container",
		},
		{Name: "command", Label: "Container command", Type: "text",
			Help: "Optional. Overrides the image's entrypoint. Quoted as a shell would: sh -c", Section: "Container"},
		{Name: "args", Label: "Container arguments", Type: "text",
			Help: `Optional. Quoted as a shell would: 'echo "hello, world"'`, Section: "Container"},
		{Name: "env", Label: "Environment variables", Type: "map", Help: "Optional. One KEY=value per line.", Section: "Container"},
		{Name: "cpu", Label: "CPU limit", Type: "text", Help: `Optional, as Kubernetes quantities: "1", "500m".`,
			Pattern: `^[0-9]+(\.[0-9]+)?m?$`, Section: "Resources"},
		{Name: "memory", Label: "Memory limit", Type: "text",
			Help:    `Optional, as Kubernetes quantities: "512Mi", "1Gi". ` + runWorkerPoolNotOffered,
			Pattern: `^[0-9]+(Ki|Mi|Gi|K|M|G)?$`, Section: "Resources"},
	}
}

func (runWorkerPoolsProvider) CreateOnPage() bool { return true }

// workerPoolForm is the template and instance count the form describes.
func workerPoolForm(values map[string]string) (*runpb.Container, *int32, error) {
	image := strings.TrimSpace(values["image"])
	if image == "" {
		return nil, nil, errors.New("a container image is required")
	}
	c := &runpb.Container{Image: image}
	var err error
	if c.Command, err = shellSplit(values["command"]); err != nil {
		return nil, nil, fmt.Errorf("container command: %w", err)
	}
	if c.Args, err = shellSplit(values["args"]); err != nil {
		return nil, nil, fmt.Errorf("container arguments: %w", err)
	}
	env, err := console.ParseMap(values["env"])
	if err != nil {
		return nil, nil, fmt.Errorf("environment variables: %w", err)
	}
	for _, k := range sortedKeys(env) {
		c.Env = append(c.Env, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: env[k]}})
	}
	limits := map[string]string{}
	for _, k := range []string{"cpu", "memory"} {
		if v := strings.TrimSpace(values[k]); v != "" {
			limits[k] = v
		}
	}
	if len(limits) > 0 {
		c.Resources = &runpb.ResourceRequirements{Limits: limits}
	}
	count, err := instanceCount(values["instances"])
	return c, count, err
}

func instanceCount(v string) (*int32, error) {
	n, err := optionalInt(v, "number of instances")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	c := int32(n)
	return &c, nil
}

func (p runWorkerPoolsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	project = p.jobs().project(project)
	if project == "" {
		return "", errors.New("choose a project before deploying a worker pool")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", errors.New("a worker pool name is required")
	}
	c, count, err := workerPoolForm(values)
	if err != nil {
		return "", err
	}
	labels, _, err := runLabels(values)
	if err != nil {
		return "", err
	}
	var name string
	err = p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		op, err := wc.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: p.jobs().parent(project), WorkerPoolId: id,
			WorkerPool: &runpb.WorkerPool{Labels: labels,
				Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{c}},
				Scaling:  &runpb.WorkerPoolScaling{ManualInstanceCount: count}}})
		if err != nil {
			return err
		}
		wp, err := op.Wait(ctx)
		if err != nil {
			return err
		}
		name = wp.GetName()
		return nil
	})
	return name, err
}

// update reads a pool, applies change and saves it with UpdateWorkerPool.
func (p runWorkerPoolsProvider) update(ctx context.Context, project, pool string, change func(*runpb.WorkerPool) error) error {
	project = p.jobs().project(project)
	if project == "" {
		return errors.New("choose a project before changing a worker pool")
	}
	return p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		wp, err := wc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: p.poolName(project, pool)})
		if err != nil {
			return err
		}
		if err := change(wp); err != nil {
			return err
		}
		op, err := wc.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{WorkerPool: wp})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// Edit implements console.Editor: the form's fields replaced on the pool
// read from the API, so what the form does not show is kept. A changed
// container is a new revision.
func (p runWorkerPoolsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a worker pool can be edited: a revision is immutable")
	}
	if v, ok := values["name"]; ok && strings.TrimSpace(v) != path[0] {
		return errors.New(runWorkerPoolNameImmutable)
	}
	next, count, err := workerPoolForm(values)
	if err != nil {
		return err
	}
	return p.update(ctx, project, path[0], func(wp *runpb.WorkerPool) error {
		cs := wp.GetTemplate().GetContainers()
		if len(cs) != 1 {
			return fmt.Errorf("this worker pool runs %d containers, and the form edits exactly one", len(cs))
		}
		cur := cs[0]
		cur.Image, cur.Command, cur.Args, cur.Env = next.Image, next.Command, next.Args, next.Env
		cur.Resources = next.Resources
		wp.Template.Revision = ""
		if count != nil {
			wp.Scaling = &runpb.WorkerPoolScaling{ManualInstanceCount: count}
		}
		if labels, ok, err := runLabels(values); err != nil {
			return err
		} else if ok {
			wp.Labels = labels
		}
		return nil
	})
}

func scaleAction(current int32) console.Action {
	return console.Action{ID: actScale, Label: "Scale", Fields: []console.Field{{
		Name: "instances", Label: "Number of instances", Type: "text", Required: true, Default: fmt.Sprint(current),
		Pattern: `^[0-9]{1,4}$`,
		Help:    "Saved with UpdateWorkerPool. The running revision is kept; only its instance count changes.",
	}}}
}

// DetailActions implements console.PathActor: a pool's Scale.
func (p runWorkerPoolsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	var actions []console.Action
	_ = p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		wp, err := wc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: p.poolName(p.jobs().project(project), path[0])})
		if err != nil {
			return err
		}
		actions = []console.Action{scaleAction(wp.GetScaling().GetManualInstanceCount())}
		return nil
	})
	return actions
}

// ActAt implements console.PathActor.
func (p runWorkerPoolsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 || action != actScale {
		return fmt.Errorf("unknown action %q", action)
	}
	count, err := instanceCount(values["instances"])
	if err != nil {
		return err
	}
	if count == nil {
		return errors.New("a number of instances is required")
	}
	return p.update(ctx, project, path[0], func(wp *runpb.WorkerPool) error {
		wp.Scaling = &runpb.WorkerPoolScaling{ManualInstanceCount: count}
		return nil
	})
}

// Delete implements console.Deleter: DeleteWorkerPool.
func (p runWorkerPoolsProvider) Delete(ctx context.Context, project, name string) error {
	project = p.jobs().project(project)
	if project == "" {
		return errors.New("choose a project before deleting a worker pool")
	}
	return p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		op, err := wc.DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: p.poolName(project, name)})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// --- detail ---------------------------------------------------------------

// Detail implements console.Driller: a pool, or one of its revisions.
func (p runWorkerPoolsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	project = p.jobs().project(project)
	if project == "" {
		return console.Detail{Prompt: "Choose a project to open its Cloud Run worker pools."}, nil
	}
	var d console.Detail
	err := p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, rc *runclient.RevisionsClient) error {
		if len(path) == 2 {
			rev, err := rc.GetRevision(ctx, &runpb.GetRevisionRequest{Name: p.poolName(project, path[0]) + "/revisions/" + path[1]})
			if err != nil {
				return err
			}
			d = workerPoolRevisionDetail(rev)
			return nil
		}
		wp, err := wc.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: p.poolName(project, path[0])})
		if err != nil {
			return err
		}
		d = workerPoolDetail(wp, p.revisions(ctx, rc, wp))
		return nil
	})
	if err != nil {
		return console.Detail{Unavailable: apiMessage(err)}, nil
	}
	return d, nil
}

func (p runWorkerPoolsProvider) revisions(ctx context.Context, rc *runclient.RevisionsClient, wp *runpb.WorkerPool) console.Listing {
	out := console.Listing{Columns: []string{"Instances", "Created"}, NameColumn: "Revision", Noun: "revisions",
		AlwaysStatus: true, RowsOpenable: true}
	it := rc.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: wp.GetName()})
	for {
		rev, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			out.Unavailable = apiMessage(err)
			return out
		}
		instances, status := "0", "Retired"
		if rev.GetName() == wp.GetLatestCreatedRevision() {
			instances = fmt.Sprint(wp.GetScaling().GetManualInstanceCount())
			status = workerPoolStatus(wp)
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(rev.GetName()), Status: status,
			Fields: map[string]string{"Instances": instances, "Created": stampAge(rev.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out
}

func workerPoolDetail(wp *runpb.WorkerPool, revisions console.Listing) console.Detail {
	summary := stored(
		console.Property{Label: "Status", Value: workerPoolStatus(wp)},
		console.Property{Label: "Region", Value: regionOf(wp.GetName())},
		console.Property{Label: "Instances", Value: fmt.Sprint(wp.GetScaling().GetManualInstanceCount())},
		console.Property{Label: "Latest revision", Value: lastSegment(wp.GetLatestCreatedRevision())},
		console.Property{Label: "Service account", Value: runAccountText(wp.GetTemplate().GetServiceAccount(), projectOf(wp.GetName()))},
		console.Property{Label: "Message", Value: wp.GetTerminalCondition().GetMessage()},
		console.Property{Label: "Generation", Value: fmt.Sprint(wp.GetGeneration())},
		console.Property{Label: "Created", Value: stampText(wp.GetCreateTime())},
		console.Property{Label: "Updated", Value: stampText(wp.GetUpdateTime())},
	)
	groups := containerGroups(wp.GetTemplate().GetContainers())
	if len(wp.GetLabels()) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: labelPairs(wp.GetLabels())})
	}
	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "revisions", Label: "Revisions", Listing: revisions},
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups,
				Note: "Changed with Edit, which deploys a new revision through UpdateWorkerPool, or with Scale. " +
					runWorkerPoolNotOffered},
		},
		Edit: workerPoolEditForm(wp),
	}
}

func workerPoolRevisionDetail(rev *runpb.Revision) console.Detail {
	ready := ""
	for _, c := range rev.GetConditions() {
		if c.GetType() == "Active" {
			ready = conditionState(c.GetState())
		}
	}
	return console.Detail{
		Summary: stored(
			console.Property{Label: "Active", Value: ready},
			console.Property{Label: "Generation", Value: fmt.Sprint(rev.GetGeneration())},
			console.Property{Label: "Created", Value: stampText(rev.GetCreateTime())},
		),
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: containerGroups(rev.GetContainers()), Note: "A revision is immutable: Edit the worker pool to deploy another."}},
	}
}

func workerPoolEditForm(wp *runpb.WorkerPool) *console.EditForm {
	cs := wp.GetTemplate().GetContainers()
	if len(cs) != 1 {
		return nil
	}
	c := proto.Clone(cs[0]).(*runpb.Container)
	env := map[string]string{}
	for _, e := range c.GetEnv() {
		if e.GetValueSource() == nil {
			env[e.GetName()] = e.GetValue()
		}
	}
	prefill := map[string]string{
		"name": lastSegment(wp.GetName()), "image": c.GetImage(),
		"command": shellJoin(c.GetCommand()), "args": shellJoin(c.GetArgs()),
		"env": console.FormatMap(env), "labels": console.FormatMap(wp.GetLabels()),
		"instances": fmt.Sprint(wp.GetScaling().GetManualInstanceCount()),
		"cpu":       c.GetResources().GetLimits()["cpu"], "memory": c.GetResources().GetLimits()["memory"],
	}
	_, fields := runWorkerPoolsProvider{}.CreateForm()
	for i := range fields {
		fields[i].Default = prefill[fields[i].Name]
		if fields[i].Name == "name" {
			fields[i].Immutable = true
			fields[i].Help = runWorkerPoolNameImmutable
		}
	}
	return &console.EditForm{Label: "Edit worker pool", Fields: fields,
		Note: "Saved with UpdateWorkerPool. A changed container deploys a new revision; the old one is retired."}
}

var (
	_ console.Driller     = runWorkerPoolsProvider{}
	_ console.PageCreator = runWorkerPoolsProvider{}
	_ console.Deleter     = runWorkerPoolsProvider{}
	_ console.Editor      = runWorkerPoolsProvider{}
	_ console.PathActor   = runWorkerPoolsProvider{}
)
