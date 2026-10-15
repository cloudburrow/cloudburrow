package main

// Cloud Run standalone instances (#1299).
//
// The Instances page of the Cloud Run product: /run/instances, an instance's
// page with its status and configuration, Create instance, Stop, Start and
// Delete. Every read and write goes through the adapter's Instances API, the
// endpoint an SDK dials.

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

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runInstancesProvider is the Instances page of Cloud Run.
type runInstancesProvider struct {
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
func (p runInstancesProvider) MutationBudget() time.Duration {
	if p.readyTimeout <= 0 {
		return 0
	}
	return p.readyTimeout + 30*time.Second
}

func (runInstancesProvider) ID() string    { return "run-instances" }
func (runInstancesProvider) Title() string { return "Cloud Run instances" }

// runInstanceStopped is the terminal condition's message on a stopped
// instance, as the adapter writes it.
const runInstanceStopped = "the instance is stopped"

func (p runInstancesProvider) jobs() runJobsProvider {
	return runJobsProvider{runAddr: p.runAddr, defaultProject: p.defaultProject, region: p.region}
}

func (p runInstancesProvider) instanceName(project, id string) string {
	if strings.HasPrefix(id, "projects/") {
		return id
	}
	return p.jobs().parent(project) + "/instances/" + id
}

func (p runInstancesProvider) withClient(ctx context.Context, fn func(*runclient.InstancesClient) error) error {
	addr := p.jobs().endpoint()
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := runclient.NewInstancesClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

func instanceStatus(in *runpb.Instance) string {
	if in.GetTerminalCondition().GetMessage() == runInstanceStopped {
		return "Stopped"
	}
	return conditionState(in.GetTerminalCondition().GetState())
}

func runInstanceActions(in *runpb.Instance) []console.Action {
	if instanceStatus(in) == "Stopped" {
		return []console.Action{{ID: "start", Label: "Start"}}
	}
	return []console.Action{{ID: "stop", Label: "Stop"}}
}

func (p runInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Image", "Age"}, NameColumn: "Instance", Noun: "instances", AlwaysStatus: true}
	project = p.jobs().project(project)
	if project == "" {
		out.Prompt = "Choose a project to list its Cloud Run instances."
		return out, nil
	}
	err := p.withClient(ctx, func(c *runclient.InstancesClient) error {
		it := c.ListInstances(ctx, &runpb.ListInstancesRequest{Parent: p.jobs().parent(project)})
		for {
			in, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			image := ""
			if cs := in.GetContainers(); len(cs) > 0 {
				image = cs[0].GetImage()
			}
			out.Items = append(out.Items, console.Resource{Name: lastSegment(in.GetName()), Status: instanceStatus(in),
				Fields: map[string]string{"Image": orDash(image), "Age": stampAge(in.GetCreateTime())}, Actions: runInstanceActions(in)})
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

func (runInstancesProvider) CreateForm() (string, []console.Field) {
	_, pool := runWorkerPoolsProvider{}.CreateForm()
	fields := []console.Field{{
		Name: "name", Label: "Instance name", Type: "text", Required: true,
		Help:    "Lowercase letters, numbers and hyphens, starting with a letter; at most 63 characters.",
		Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`, Section: "Instance settings",
	}, {Name: "description", Label: "Description", Type: "text", Help: "Optional.", Section: "Instance settings"}}
	for _, f := range pool {
		switch f.Name {
		case "name", "labels", "instances":
			continue
		case "memory":
			f.Help = `Optional, as Kubernetes quantities: "512Mi", "1Gi". Not offered, because the Cloud Run ` +
				"adapter refuses them: ingress other than all, IAP, VPC access and binary authorization."
		}
		fields = append(fields, f)
	}
	return "Create instance", fields
}

func (runInstancesProvider) CreateOnPage() bool { return true }

func (p runInstancesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	project = p.jobs().project(project)
	if project == "" {
		return "", errors.New("choose a project before creating an instance")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", errors.New("an instance name is required")
	}
	c, _, err := workerPoolForm(values)
	if err != nil {
		return "", err
	}
	var name string
	err = p.withClient(ctx, func(ic *runclient.InstancesClient) error {
		op, err := ic.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: p.jobs().parent(project), InstanceId: id,
			Instance: &runpb.Instance{Description: strings.TrimSpace(values["description"]), Containers: []*runpb.Container{c}}})
		if err != nil {
			return err
		}
		in, err := op.Wait(ctx)
		if err != nil {
			return err
		}
		name = in.GetName()
		return nil
	})
	return name, err
}

// Actions implements console.Actor: Stop or Start on a row.
func (runInstancesProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p runInstancesProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}

func (p runInstancesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	var actions []console.Action
	_ = p.withClient(ctx, func(c *runclient.InstancesClient) error {
		in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: p.instanceName(p.jobs().project(project), path[0])})
		if err != nil {
			return err
		}
		actions = runInstanceActions(in)
		return nil
	})
	return actions
}

func (p runInstancesProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	name := p.instanceName(p.jobs().project(project), path[0])
	return p.withClient(ctx, func(c *runclient.InstancesClient) error {
		switch action {
		case "stop":
			op, err := c.StopInstance(ctx, &runpb.StopInstanceRequest{Name: name})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case "start":
			op, err := c.StartInstance(ctx, &runpb.StartInstanceRequest{Name: name})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		}
		return fmt.Errorf("unknown action %q", action)
	})
}

func (p runInstancesProvider) Delete(ctx context.Context, project, name string) error {
	project = p.jobs().project(project)
	if project == "" {
		return errors.New("choose a project before deleting an instance")
	}
	return p.withClient(ctx, func(c *runclient.InstancesClient) error {
		op, err := c.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: p.instanceName(project, name)})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

func (p runInstancesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	project = p.jobs().project(project)
	if project == "" {
		return console.Detail{Prompt: "Choose a project to open its Cloud Run instances."}, nil
	}
	var d console.Detail
	err := p.withClient(ctx, func(c *runclient.InstancesClient) error {
		in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: p.instanceName(project, path[0])})
		if err != nil {
			return err
		}
		msg := in.GetTerminalCondition().GetMessage()
		if msg == runInstanceStopped {
			msg = ""
		}
		d = console.Detail{
			Summary: stored(
				console.Property{Label: "Status", Value: instanceStatus(in)},
				console.Property{Label: "Region", Value: regionOf(in.GetName())},
				console.Property{Label: "Description", Value: in.GetDescription()},
				console.Property{Label: "URL", Value: strings.Join(in.GetUrls(), ", ")},
				console.Property{Label: "Message", Value: msg},
				console.Property{Label: "Created", Value: stampText(in.GetCreateTime())},
				console.Property{Label: "Updated", Value: stampText(in.GetUpdateTime())},
			),
			Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: containerGroups(in.GetContainers()),
				Note: "The URL is served through the cluster's gateway, as a service's is; the container listening on " +
					"PORT (8080 unless it declares a port) answers it. Ingress other than all is refused."}},
		}
		return nil
	})
	if err != nil {
		return console.Detail{Unavailable: apiMessage(err)}, nil
	}
	return d, nil
}

var (
	_ console.Driller          = runInstancesProvider{}
	_ console.PageCreator      = runInstancesProvider{}
	_ console.Deleter          = runInstancesProvider{}
	_ console.Actor            = runInstancesProvider{}
	_ console.PathActor        = runInstancesProvider{}
	_ console.MutationBudgeter = runInstancesProvider{}
)
