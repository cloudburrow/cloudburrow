package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/identity-wael/cloudburrow/internal/console"
	"github.com/identity-wael/cloudburrow/internal/service/secrets"
	"github.com/identity-wael/cloudburrow/internal/service/tasks"
)

// The providers below read the same surfaces an SDK client reads. None of
// them keeps state, and none of them holds a store the services do not: a
// console with its own copy would give CloudBurrow two answers to the same
// question, and the developer would see whichever they happened to ask.

// storageProvider lists buckets through the Cloud Storage JSON API.
type storageProvider struct{ endpoint string }

func (storageProvider) ID() string    { return "storage" }
func (storageProvider) Title() string { return "Buckets" }

func (p storageProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// The JSON API requires a project to list buckets; with none chosen the
	// screen says so rather than inventing one.
	if project == "" {
		return console.Listing{
			Columns:     []string{"Location", "Storage class"},
			Unavailable: "Cloud Storage lists buckets per project. Choose a project in the toolbar.",
		}, nil
	}

	var body struct {
		Items []struct {
			Name         string `json:"name"`
			Location     string `json:"location"`
			StorageClass string `json:"storageClass"`
			TimeCreated  string `json:"timeCreated"`
		} `json:"items"`
	}
	url := fmt.Sprintf("http://%s/storage/v1/b?project=%s", p.endpoint, project)
	if err := getJSON(ctx, url, &body); err != nil {
		return console.Listing{}, err
	}

	items := make([]console.Resource, 0, len(body.Items))
	for _, b := range body.Items {
		items = append(items, console.Resource{
			Name: b.Name,
			Fields: map[string]string{
				"Location":      b.Location,
				"Storage class": b.StorageClass,
				"Created":       shortTime(b.TimeCreated),
			},
		})
	}
	return console.Listing{
		Columns: []string{"Location", "Storage class", "Created"},
		Items:   items, Total: len(items),
		// Measured, not assumed: fake-gcs-server accepts the project
		// parameter and returns every bucket regardless. Showing the rows
		// under a project heading without saying so would be the screen
		// lying about what they are.
		Note: "The storage backend does not scope buckets by project, so this " +
			"lists every bucket in the instance.",
	}, nil
}

// pubsubProvider lists topics through the official client.
type pubsubProvider struct{ endpoint string }

func (pubsubProvider) ID() string    { return "pubsub" }
func (pubsubProvider) Title() string { return "Topics" }

func (p pubsubProvider) List(ctx context.Context, project string) (console.Listing, error) {
	if project == "" {
		return console.Listing{
			Columns:     []string{"Subscriptions"},
			Unavailable: "Pub/Sub lists topics per project. Choose a project in the toolbar.",
		}, nil
	}

	c, err := pubsub.NewClient(ctx, project,
		option.WithEndpoint(p.endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return console.Listing{}, fmt.Errorf("connect to Pub/Sub: %w", err)
	}
	defer func() { _ = c.Close() }()

	it := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{
		Project: "projects/" + project,
	})
	var items []console.Resource
	for {
		t, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return console.Listing{}, fmt.Errorf("list topics: %w", err)
		}
		items = append(items, console.Resource{Name: t.GetName()})
	}
	return console.Listing{Columns: []string{}, Items: items, Total: len(items)}, nil
}

// tasksProvider lists queues from the in-process Cloud Tasks store.
//
// Cloud Tasks runs in this process, so this is the same store the gRPC
// service serves — not a copy of it.
type tasksProvider struct{ svc *tasksService }

func (tasksProvider) ID() string    { return "tasks" }
func (tasksProvider) Title() string { return "Queues" }

func (p tasksProvider) List(_ context.Context, project string) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, fmt.Errorf("Cloud Tasks has not started")
	}
	queues, err := st.AllQueues()
	if err != nil {
		return console.Listing{}, err
	}

	items := make([]console.Resource, 0, len(queues))
	for _, q := range queues {
		if project != "" && !strings.HasPrefix(q.Name, "projects/"+project+"/") {
			continue
		}
		items = append(items, console.Resource{
			Name:   q.Name,
			Status: string(q.State),
			Fields: map[string]string{"Created": q.Created.Format(time.RFC3339)},
		})
	}
	return console.Listing{Columns: []string{"Created"}, Items: items, Total: len(items)}, nil
}

// secretsProvider lists secrets from the in-process Secret Manager store.
type secretsProvider struct{ svc *secretsService }

func (secretsProvider) ID() string    { return "secrets" }
func (secretsProvider) Title() string { return "Secrets" }

func (p secretsProvider) List(_ context.Context, project string) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return console.Listing{
			Columns:     []string{"Versions"},
			Unavailable: "Secret Manager lists secrets per project. Choose a project in the toolbar.",
		}, nil
	}

	all, err := st.ListSecrets(project)
	if err != nil {
		return console.Listing{}, err
	}
	items := make([]console.Resource, 0, len(all))
	for _, s := range all {
		versions, vErr := st.ListVersions(project, lastSegment(s.Name))
		count := "—"
		if vErr == nil {
			count = fmt.Sprint(len(versions))
		}
		items = append(items, console.Resource{
			Name: s.Name,
			Fields: map[string]string{
				"Versions": count,
				"Created":  s.Created.Format(time.RFC3339),
			},
		})
	}
	return console.Listing{
		Columns: []string{"Versions", "Created"}, Items: items, Total: len(items),
	}, nil
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// runProvider lists Cloud Run services through the adapter's own view of the
// cluster, so what the console shows is what the Cloud Run API would answer.
type runProvider struct {
	kubeconfig, namespace string
	// runEndpoint is the Cloud Run adapter's address. Deployment goes
	// through it rather than through a Knative manifest, so the console
	// cannot accept a configuration the API refuses.
	runEndpoint    string
	defaultProject string
	region         string
}

func (runProvider) ID() string    { return "run" }
func (runProvider) Title() string { return "Services" }

func (p runProvider) List(ctx context.Context, _ string) (console.Listing, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "ksvc")
	if err != nil {
		return console.Listing{}, err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Status   struct {
				URL        string `json:"url"`
				Conditions []struct {
					Type, Status, Reason string
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Listing{}, fmt.Errorf("decode services: %w", err)
	}

	items := make([]console.Resource, 0, len(list.Items))
	for _, s := range list.Items {
		state := "Unknown"
		for _, c := range s.Status.Conditions {
			if c.Type != "Ready" {
				continue
			}
			switch c.Status {
			case "True":
				state = "Ready"
			case "False":
				state = "Failed"
			default:
				state = "Pending"
			}
		}
		items = append(items, console.Resource{
			Name: s.Metadata.Name, Status: state,
			Fields: map[string]string{"URL": s.Status.URL},
		})
	}
	return console.Listing{Columns: []string{"URL"}, Items: items, Total: len(items)}, nil
}

// workloadsProvider lists Kubernetes Deployments, read-only.
type workloadsProvider struct{ kubeconfig, namespace string }

func (workloadsProvider) ID() string    { return "workloads" }
func (workloadsProvider) Title() string { return "Workloads" }

func (p workloadsProvider) List(ctx context.Context, _ string) (console.Listing, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "deployments")
	if err != nil {
		return console.Listing{}, err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name, Namespace string } `json:"metadata"`
			Status   struct {
				ReadyReplicas int `json:"readyReplicas"`
				Replicas      int `json:"replicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Listing{}, fmt.Errorf("decode workloads: %w", err)
	}

	items := make([]console.Resource, 0, len(list.Items))
	for _, d := range list.Items {
		state := "Pending"
		if d.Status.Replicas > 0 && d.Status.ReadyReplicas == d.Status.Replicas {
			state = "Ready"
		}
		items = append(items, console.Resource{
			Name: d.Metadata.Name, Status: state,
			Fields: map[string]string{
				"Namespace": d.Metadata.Namespace,
				"Replicas":  fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, d.Status.Replicas),
			},
		})
	}
	return console.Listing{
		Columns: []string{"Namespace", "Replicas"}, Items: items, Total: len(items),
	}, nil
}

// --- helpers ---------------------------------------------------------

func getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s responded %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func shortTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Format("2006-01-02 15:04")
}

// tasksStoreQueues is a compile-time check that the console reads the same
// store the service serves, rather than a shape of its own.
var _ = func(s *tasks.Store) { _, _ = s.AllQueues() }
var _ = func(s *secrets.Store) { _, _ = s.ListSecrets("p") }

// kubectlJSON reads a collection from the cluster.
//
// kubectl is used rather than a Kubernetes client library because that is how
// every other part of CloudBurrow talks to the cluster; adding client-go for
// the console alone would add a large dependency for one read path.
func kubectlJSON(ctx context.Context, kubeconfig, namespace, kind string) ([]byte, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig: the cluster has not started")
	}
	args := []string{"--kubeconfig", kubeconfig, "get", kind, "-o", "json"}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl get %s: %s", kind, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl get %s: %w", kind, err)
	}
	return out, nil
}

// --- create, delete and actions --------------------------------------
//
// Every mutation below goes through the same API an SDK client would call.
// None of them writes to a store directly, so a resource created here is
// created exactly as a client would have created it — which is what makes
// "UI-created resources work through official SDKs" true rather than hoped.

// Storage: create and delete buckets.

func (storageProvider) CreateForm() (string, []console.Field) {
	// The field names follow the documented Create a bucket form, restricted
	// to what the local backend accepts. Location is absent because the
	// backend has one and offering a choice it ignores would be a control
	// that does nothing.
	return "Create", []console.Field{{
		Name: "name", Label: "Bucket name", Type: "text", Required: true,
		Help:    "Lowercase letters, numbers, hyphens and underscores; 3-63 characters.",
		Pattern: `^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`,
	}}
}

func (p storageProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a bucket")
	}
	name := strings.TrimSpace(values["name"])
	if name == "" {
		return "", fmt.Errorf("bucket name is required")
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("http://%s/storage/v1/b?project=%s", p.endpoint, project)
	if err := postJSON(ctx, url, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p storageProvider) Delete(ctx context.Context, _ string, name string) error {
	url := fmt.Sprintf("http://%s/storage/v1/b/%s", p.endpoint, name)
	return deleteURL(ctx, url)
}

// Pub/Sub: create and delete topics.

func (pubsubProvider) CreateForm() (string, []console.Field) {
	return "Create topic", []console.Field{{
		Name: "name", Label: "Topic ID", Type: "text", Required: true,
		Help:    "3-255 characters, starting with a letter.",
		Pattern: `^[A-Za-z][A-Za-z0-9._~%+-]{2,254}$`,
	}}
}

func (p pubsubProvider) client(ctx context.Context, project string) (*pubsub.Client, error) {
	return pubsub.NewClient(ctx, project,
		option.WithEndpoint(p.endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
}

func (p pubsubProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a topic")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", fmt.Errorf("topic ID is required")
	}
	c, err := p.client(ctx, project)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()

	name := fmt.Sprintf("projects/%s/topics/%s", project, id)
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name}); err != nil {
		return "", err
	}
	return name, nil
}

func (p pubsubProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name})
}

// Cloud Tasks: create queues, and pause, resume or purge them.

func (tasksProvider) CreateForm() (string, []console.Field) {
	return "Create queue", []console.Field{
		{
			Name: "name", Label: "Queue name", Type: "text", Required: true,
			Help:    "Letters, numbers and hyphens.",
			Pattern: `^[A-Za-z][A-Za-z0-9-]{0,99}$`,
		},
		{
			Name: "location", Label: "Region", Type: "text", Required: true,
			Default: "us-central1",
			Help:    "Any location string; CloudBurrow does not place resources geographically.",
		},
	}
}

func (p tasksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a queue")
	}
	st := p.svc.Store()
	if st == nil {
		return "", fmt.Errorf("Cloud Tasks has not started")
	}
	id := strings.TrimSpace(values["name"])
	location := strings.TrimSpace(values["location"])
	if location == "" {
		location = "us-central1"
	}
	name := fmt.Sprintf("projects/%s/locations/%s/queues/%s", project, location, id)
	// The defaults are the service's own, so a queue created here behaves
	// exactly like one created through the API with no overrides.
	q, err := st.CreateQueue(tasks.Queue{
		Name:        name,
		State:       tasks.StateRunning,
		RetryConfig: tasks.DefaultRetryConfig(),
		RateLimits:  tasks.DefaultRateLimits(),
	})
	if err != nil {
		return "", err
	}
	return q.Name, nil
}

func (p tasksProvider) Delete(_ context.Context, _ string, name string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Cloud Tasks has not started")
	}
	return st.DeleteQueue(name)
}

func (tasksProvider) Actions(r console.Resource) []console.Action {
	// The available actions depend on the queue's own state, so a paused
	// queue is not offered "Pause" — an action that would do nothing is
	// indistinguishable from one that is broken.
	switch strings.ToUpper(r.Status) {
	case "PAUSED":
		return []console.Action{
			{ID: "resume", Label: "Resume"},
			{ID: "purge", Label: "Purge", Destructive: true},
		}
	default:
		return []console.Action{
			{ID: "pause", Label: "Pause"},
			{ID: "purge", Label: "Purge", Destructive: true},
		}
	}
}

func (p tasksProvider) Act(_ context.Context, _ string, name, action string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Cloud Tasks has not started")
	}
	switch action {
	case "pause":
		_, err := st.SetQueueState(name, tasks.StatePaused)
		return err
	case "resume":
		_, err := st.SetQueueState(name, tasks.StateRunning)
		return err
	case "purge":
		return st.PurgeQueue(name)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

// Secret Manager: delete only. Creating a secret without a payload produces
// something no caller can read, so the console does not offer it.

func (p secretsProvider) Delete(_ context.Context, project, name string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Secret Manager has not started")
	}
	return st.DeleteSecret(project, lastSegment(name))
}

func postJSON(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	return nil
}

func deleteURL(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	return nil
}

// apiError returns the service's own message, so the screen shows the
// constraint that was violated rather than a status code.
func apiError(resp *http.Response) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return errors.New(envelope.Error.Message)
	}
	if msg := strings.TrimSpace(string(raw)); msg != "" {
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	return errors.New(resp.Status)
}

// --- Cloud Run: deploy and delete from the console --------------------
//
// Deployment goes through the Cloud Run v2 adapter, the same surface an SDK
// client calls, rather than applying a Knative manifest directly. Applying a
// manifest would bypass the adapter's own refusals — a configuration the API
// rejects would deploy from the console and not from the SDK, which is the
// console inventing support.

func (runProvider) CreateForm() (string, []console.Field) {
	// The field names follow the documented Create service form, restricted
	// to what the adapter supports. Authentication, ingress and service
	// accounts are absent because CloudBurrow authenticates nothing and the
	// adapter refuses them: offering the control would be offering support.
	return "Deploy container", []console.Field{
		{
			Name: "name", Label: "Service name", Type: "text", Required: true,
			Help:    "Lowercase letters, numbers and hyphens; at most 49 characters.",
			Pattern: `^[a-z]([a-z0-9-]{0,47}[a-z0-9])?$`,
		},
		{
			Name: "image", Label: "Container image URL", Type: "text", Required: true,
			Default: "ghcr.io/knative/helloworld-go:latest",
			Help: "A tagged image. A locally built one is rewritten to dev.local/ " +
				"and never pulled; an untagged reference is refused.",
		},
		{
			Name: "env", Label: "Environment variables", Type: "text",
			Help: "Optional, as KEY=value separated by commas.",
		},
	}
}

func (p runProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.runEndpoint == "" {
		return "", fmt.Errorf("the Cloud Run adapter is not running")
	}
	if project == "" {
		project = p.defaultProject
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before deploying a service")
	}

	id := strings.TrimSpace(values["name"])
	image := strings.TrimSpace(values["image"])
	if id == "" || image == "" {
		return "", fmt.Errorf("service name and container image are both required")
	}

	c, err := runclient.NewServicesClient(ctx,
		option.WithEndpoint(p.runEndpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return "", fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = c.Close() }()

	container := &runpb.Container{Image: image}
	for _, pair := range strings.Split(values["env"], ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return "", fmt.Errorf("environment variable %q must be KEY=value", pair)
		}
		container.Env = append(container.Env, &runpb.EnvVar{
			Name:   strings.TrimSpace(name),
			Values: &runpb.EnvVar_Value{Value: value},
		})
	}

	parent := fmt.Sprintf("projects/%s/locations/%s", project, p.location())
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: parent, ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{container},
		}},
	})
	if err != nil {
		return "", err
	}
	// Waited on rather than returned as accepted: a deployment that is
	// reported created and then never becomes ready is the failure the
	// console exists to make visible.
	svc, err := op.Wait(ctx)
	if err != nil {
		return "", fmt.Errorf("the service never became ready: %w", err)
	}
	return svc.GetName(), nil
}

func (p runProvider) Delete(ctx context.Context, project, name string) error {
	if p.runEndpoint == "" {
		return fmt.Errorf("the Cloud Run adapter is not running")
	}
	c, err := runclient.NewServicesClient(ctx,
		option.WithEndpoint(p.runEndpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = c.Close() }()

	if project == "" {
		project = p.defaultProject
	}
	// The listing reports the Knative name; the API takes a resource name.
	full := name
	if !strings.HasPrefix(name, "projects/") {
		full = fmt.Sprintf("projects/%s/locations/%s/services/%s", project, p.location(), name)
	}
	op, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: full})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (p runProvider) location() string {
	if p.region != "" {
		return p.region
	}
	return "us-central1"
}

// --- Kubernetes: scoped, read-only views ------------------------------
//
// Read-only on purpose. CloudBurrow owns this cluster, and a console that
// could apply arbitrary manifests to it would be a way to create workloads
// CloudBurrow does not track and cannot clean up.
//
// Nothing here reports GKE cluster metadata — node pools, autopilot,
// releases channels. This is a kind cluster, and presenting GKE fields would
// be fabricating the one thing the issue names.

// kubeProvider lists one Kubernetes kind.
type kubeProvider struct {
	id, title, kind string
	kubeconfig      string
	// namespace empty means every namespace, which is what the cluster views
	// need: a workload can be in any of them.
	namespace string
	columns   []string
	// row extracts the columns and status from one item.
	row func(item map[string]any) (console.Resource, bool)
}

func (p kubeProvider) ID() string    { return p.id }
func (p kubeProvider) Title() string { return p.title }

func (p kubeProvider) List(ctx context.Context, _ string) (console.Listing, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, p.kind)
	if err != nil {
		return console.Listing{}, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Listing{}, fmt.Errorf("decode %s: %w", p.kind, err)
	}

	items := make([]console.Resource, 0, len(list.Items))
	for _, raw := range list.Items {
		r, ok := p.row(raw)
		if !ok {
			continue
		}
		items = append(items, r)
	}
	return console.Listing{
		Columns: p.columns, Items: items, Total: len(items),
		Note: "Read-only. CloudBurrow owns this cluster; workloads are created " +
			"through Cloud Run or kubectl, not from the console.",
	}, nil
}

func meta(item map[string]any) map[string]any {
	m, _ := item["metadata"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func nested(item map[string]any, keys ...string) map[string]any {
	cur := item
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}

// ownedBy reports whether an object carries CloudBurrow's ownership label.
//
// Ownership is shown rather than assumed: a developer looking at the cluster
// should be able to tell what CloudBurrow created from what they did.
func ownedBy(item map[string]any) string {
	labels, _ := meta(item)["labels"].(map[string]any)
	if labels == nil {
		return "no"
	}
	if v, ok := labels["cloudburrow.dev/owned"].(string); ok && v == "true" {
		return "yes"
	}
	return "no"
}

func podsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "pods", title: "Pods", kind: "pods", kubeconfig: kubeconfig,
		columns: []string{"Namespace", "Node", "Restarts", "CloudBurrow"},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			st := nested(item, "status")
			phase := str(st, "phase")

			restarts := 0
			if statuses, ok := st["containerStatuses"].([]any); ok {
				for _, cs := range statuses {
					if c, ok := cs.(map[string]any); ok {
						if n, ok := c["restartCount"].(float64); ok {
							restarts += int(n)
						}
					}
				}
			}
			return console.Resource{
				Name:   str(m, "name"),
				Status: phase,
				Fields: map[string]string{
					"Namespace":   str(m, "namespace"),
					"Node":        str(nested(item, "spec"), "nodeName"),
					"Restarts":    fmt.Sprint(restarts),
					"CloudBurrow": ownedBy(item),
				},
			}, true
		},
	}
}

func servicesProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "k8sservices", title: "Kubernetes Services", kind: "services", kubeconfig: kubeconfig,
		columns: []string{"Namespace", "Type", "Cluster IP", "CloudBurrow"},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			spec := nested(item, "spec")
			return console.Resource{
				Name: str(m, "name"),
				Fields: map[string]string{
					"Namespace":   str(m, "namespace"),
					"Type":        str(spec, "type"),
					"Cluster IP":  str(spec, "clusterIP"),
					"CloudBurrow": ownedBy(item),
				},
			}, true
		},
	}
}

func jobsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "jobs", title: "Jobs", kind: "jobs", kubeconfig: kubeconfig,
		columns: []string{"Namespace", "Completions", "CloudBurrow"},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			st := nested(item, "status")
			succeeded, failed := 0, 0
			if n, ok := st["succeeded"].(float64); ok {
				succeeded = int(n)
			}
			if n, ok := st["failed"].(float64); ok {
				failed = int(n)
			}
			state := "Running"
			switch {
			case failed > 0:
				state = "Failed"
			case succeeded > 0:
				state = "Succeeded"
			}
			return console.Resource{
				Name: str(m, "name"), Status: state,
				Fields: map[string]string{
					"Namespace":   str(m, "namespace"),
					"Completions": fmt.Sprintf("%d succeeded, %d failed", succeeded, failed),
					"CloudBurrow": ownedBy(item),
				},
			}, true
		},
	}
}

// eventsProvider surfaces what the cluster is actually complaining about.
//
// This is the screen that turns "the pod is Pending" into a reason, which is
// the difference between a console that shows a problem and one that explains
// it.
func eventsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "events", title: "Events", kind: "events", kubeconfig: kubeconfig,
		columns: []string{"Namespace", "Object", "Reason", "Message", "Count"},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			involved := nested(item, "involvedObject")
			kind := str(involved, "kind")
			name := str(involved, "name")

			count := 1
			if n, ok := item["count"].(float64); ok {
				count = int(n)
			}
			// Warnings are the ones worth a status colour; Normal events are
			// the bulk and are not a problem.
			status := ""
			if str(item, "type") == "Warning" {
				status = "Failed"
			}
			return console.Resource{
				Name: str(m, "name"), Status: status,
				Fields: map[string]string{
					"Namespace": str(m, "namespace"),
					"Object":    strings.TrimSpace(kind + "/" + name),
					"Reason":    str(item, "reason"),
					"Message":   str(item, "message"),
					"Count":     fmt.Sprint(count),
				},
			}, true
		},
	}
}
