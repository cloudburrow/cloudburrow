package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
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
type runProvider struct{ kubeconfig, namespace string }

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
