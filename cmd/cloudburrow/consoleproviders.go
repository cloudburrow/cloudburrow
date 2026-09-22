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
	"sort"
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
	"github.com/identity-wael/cloudburrow/internal/localai"
	"github.com/identity-wael/cloudburrow/internal/service/secrets"
	"github.com/identity-wael/cloudburrow/internal/service/tasks"
)

// A note on the Pattern fields below.
//
// An HTML `pattern` attribute is compiled with the RegExp `v` flag, which
// requires `-` to be escaped inside a character class even in trailing
// position. An unescaped one makes the whole pattern invalid, and the browser
// then **silently skips validation** rather than reporting it — so the form
// appears to validate and does not. Found by driving the real browser during
// #49; a unit test now compiles every shipped pattern under `v`.
//
// The providers below read the same surfaces an SDK client reads. None of
// them keeps state, and none of them holds a store the services do not: a
// console with its own copy would give CloudBurrow two answers to the same
// question, and the developer would see whichever they happened to ask.

// storageProvider lists buckets through the Cloud Storage JSON API.
type storageProvider struct{ endpoint string }

func (storageProvider) ID() string    { return "storage" }
func (storageProvider) Title() string { return "Cloud Storage" }

func (p storageProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// The JSON API requires a project to list buckets; with none chosen the
	// screen says so rather than inventing one.
	if project == "" {
		return console.Listing{
			Columns: []string{"Location", "Storage class"},
			Noun:    "buckets",
			Prompt:  "Cloud Storage lists buckets per project. Choose one in the toolbar.",
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
		Noun:    "buckets",
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
func (pubsubProvider) Title() string { return "Pub/Sub" }

func (p pubsubProvider) List(ctx context.Context, project string) (console.Listing, error) {
	if project == "" {
		return console.Listing{
			Columns: []string{"Subscriptions"},
			Noun:    "topics",
			Prompt:  "Pub/Sub lists topics per project. Choose one in the toolbar.",
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
	return console.Listing{Columns: []string{}, Noun: "topics", Items: items, Total: len(items)}, nil
}

// tasksProvider lists queues from the in-process Cloud Tasks store.
//
// Cloud Tasks runs in this process, so this is the same store the gRPC
// service serves — not a copy of it.
type tasksProvider struct{ svc *tasksService }

func (tasksProvider) ID() string    { return "tasks" }
func (tasksProvider) Title() string { return "Cloud Tasks" }

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
		// Depth, from the same store the gRPC service serves. A queue screen
		// that cannot say how much is in the queue is answering the wrong
		// question: "paused" and "paused with four hundred tasks waiting" are
		// very different facts.
		depth, due := "—", "—"
		if tasks, err := st.ListTasks(q.Name); err == nil {
			depth = fmt.Sprint(len(tasks))
			ready := 0
			now := time.Now()
			for _, t := range tasks {
				if !t.ScheduleTime.After(now) {
					ready++
				}
			}
			due = fmt.Sprint(ready)
		}
		items = append(items, console.Resource{
			Name:   q.Name,
			Status: string(q.State),
			Fields: map[string]string{
				"Tasks in queue": depth,
				"Due now":        due,
				"Created":        q.Created.Format(time.RFC3339),
			},
		})
	}
	return console.Listing{
		Columns:      []string{"Tasks in queue", "Due now", "Created"},
		Noun:         "queues",
		Items:        items,
		Total:        len(items),
		AlwaysStatus: true,
	}, nil
}

// Detail implements console.Driller for one queue.
//
// A queue's tasks were invisible: the store held them, the gRPC service
// served them, and the console offered no way to look. Depth on the list
// answers "how much"; this answers "what, and when".
func (p tasksProvider) Detail(_ context.Context, project, name string) (console.Detail, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: "Cloud Tasks has not started"}, nil
	}
	queue, err := st.GetQueue(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the queue: " + err.Error()}, nil
	}

	listing := console.Listing{
		Columns:    []string{"Scheduled", "Attempts", "Responses", "Last response"},
		NameColumn: "Task",
		Noun:       "tasks",
	}
	tasks, err := st.ListTasks(name)
	if err != nil {
		listing.Unavailable = "cannot list tasks: " + err.Error()
	} else {
		shown := tasks
		if len(shown) > detailLimit {
			shown = shown[:detailLimit]
			listing.Note = truncatedNote(len(shown), "tasks")
		}
		for _, t := range shown {
			last := "—"
			if t.LastResponseCode != 0 {
				last = fmt.Sprint(t.LastResponseCode)
			}
			listing.Items = append(listing.Items, console.Resource{
				Name: t.Name,
				Fields: map[string]string{
					"Scheduled":     t.ScheduleTime.Format(time.RFC3339),
					"Attempts":      fmt.Sprint(t.DispatchCount),
					"Responses":     fmt.Sprint(t.ResponseCount),
					"Last response": last,
				},
			})
		}
		listing.Total = len(listing.Items)
	}

	// The queue's own configuration, which the create path sets and nothing
	// ever showed back.
	summary := []console.Property{
		{Label: "State", Value: string(queue.State)},
		{Label: "Tasks in queue", Value: fmt.Sprint(len(tasks))},
		{Label: "Created", Value: queue.Created.Format(time.RFC3339)},
	}
	r := queue.RetryConfig
	summary = append(summary,
		console.Property{Label: "Max attempts", Value: fmt.Sprint(r.MaxAttempts)},
		console.Property{Label: "Min backoff", Value: r.MinBackoff.String()},
		console.Property{Label: "Max backoff", Value: r.MaxBackoff.String()})
	// maxDoublings is not modelled by the dispatcher, and showing a value the
	// backend ignores would be a working-looking control in a read-only card.
	// docs/compatibility.md records the gap; the card does not repeat it.
	l := queue.RateLimits
	summary = append(summary,
		console.Property{Label: "Dispatches per second",
			Value: fmt.Sprintf("%.2f", l.MaxDispatchesPerSecond)},
		console.Property{Label: "Max concurrent dispatches",
			Value: fmt.Sprint(l.MaxConcurrentDispatches)})

	return console.Detail{
		Summary:  summary,
		Sections: []console.Section{{ID: "tasks", Label: "Tasks", Listing: listing}},
	}, nil
}

// secretsProvider lists secrets from the in-process Secret Manager store.
type secretsProvider struct{ svc *secretsService }

func (secretsProvider) ID() string    { return "secrets" }
func (secretsProvider) Title() string { return "Secret Manager" }

func (p secretsProvider) List(_ context.Context, project string) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return console.Listing{
			Columns: []string{"Versions"},
			Noun:    "secrets",
			Prompt:  "Secret Manager lists secrets per project. Choose one in the toolbar.",
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
		Columns: []string{"Versions", "Created"}, Noun: "secrets", Items: items, Total: len(items),
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
func (runProvider) Title() string { return "Cloud Run" }

func (p runProvider) List(ctx context.Context, _ string) (console.Listing, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "ksvc")
	if err != nil {
		return console.Listing{}, err
	}
	var list struct {
		Items []ksvcStatus `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Listing{}, fmt.Errorf("decode services: %w", err)
	}

	items := make([]console.Resource, 0, len(list.Items))
	for _, s := range list.Items {
		items = append(items, s.resource())
	}
	return console.Listing{
		Columns: runColumns, Noun: "services", Items: items, Total: len(items),
		AlwaysStatus: true,
	}, nil
}

// Detail implements console.Driller for one service.
//
// A Cloud Run service is a history of revisions and a split of traffic
// between them, and neither was reachable from this console: clicking a row
// did nothing, because the provider offered no detail at all.
func (p runProvider) Detail(ctx context.Context, _, name string) (console.Detail, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "ksvc")
	if err != nil {
		return console.Detail{Unavailable: "cannot read services: " + err.Error()}, nil
	}
	var list struct {
		Items []ksvcStatus `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Detail{Unavailable: "decode services: " + err.Error()}, nil
	}

	var svc *ksvcStatus
	for i := range list.Items {
		if list.Items[i].Metadata.Name == name {
			svc = &list.Items[i]
			break
		}
	}
	if svc == nil {
		return console.Detail{Unavailable: "no service named " + name}, nil
	}

	state, reason, message := svc.ready()
	summary := []console.Property{
		{Label: "Status", Value: state},
		{Label: "URL", Value: svc.Status.URL},
		{Label: "Image", Value: svc.image()},
		{Label: "Serving revision", Value: svc.Status.LatestReadyRevisionName},
		{Label: "Latest revision", Value: svc.Status.LatestCreatedRevisionName},
		{Label: "Age", Value: shortAge(svc.Metadata.CreationTimestamp)},
	}
	if reason != "" {
		summary = append(summary, console.Property{Label: "Reason", Value: reason})
	}
	if message != "" {
		summary = append(summary, console.Property{Label: "Detail", Value: message})
	}

	sections := []console.Section{
		{ID: "revisions", Label: "Revisions", Listing: p.revisions(ctx, name, svc)},
		{ID: "traffic", Label: "Traffic", Listing: trafficListing(svc)},
	}
	return console.Detail{Summary: summary, Sections: sections}, nil
}

// revisions lists the service's own revisions, newest first.
func (p runProvider) revisions(ctx context.Context, service string, svc *ksvcStatus) console.Listing {
	out := console.Listing{
		Columns:      []string{"Image", "Digest", "Ready", "Reason", "Age"},
		NameColumn:   "Revision",
		Noun:         "revisions",
		AlwaysStatus: true,
	}
	raw, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "revisions")
	if err != nil {
		out.Unavailable = "cannot read revisions: " + err.Error()
		return out
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		out.Unavailable = "decode revisions: " + err.Error()
		return out
	}

	serving := svc.Status.LatestReadyRevisionName
	for _, item := range list.Items {
		m := meta(item)
		labels, _ := m["labels"].(map[string]any)
		if str(labels, "serving.knative.dev/service") != service {
			continue
		}
		status := "Unknown"
		reason := ""
		if conds, ok := nested(item, "status")["conditions"].([]any); ok {
			for _, c := range conds {
				cond, ok := c.(map[string]any)
				if !ok || str(cond, "type") != "Ready" {
					continue
				}
				switch str(cond, "status") {
				case "True":
					status = "Ready"
				case "False":
					status = "Failed"
					reason = str(cond, "reason")
				default:
					status = "Pending"
					reason = str(cond, "reason")
				}
			}
		}
		image, digest := "", ""
		if containers, ok := nested(item, "spec")["containers"].([]any); ok && len(containers) > 0 {
			if c, ok := containers[0].(map[string]any); ok {
				image = str(c, "image")
			}
		}
		// The digest Knative resolved, which is what actually ran.
		if cs, ok := nested(item, "status")["containerStatuses"].([]any); ok && len(cs) > 0 {
			if c, ok := cs[0].(map[string]any); ok {
				digest = shortDigest(str(c, "imageDigest"))
			}
		}
		name := str(m, "name")
		if name == serving {
			// Marked rather than reordered: which one is serving is the
			// question, and a badge answers it without moving the row.
			name += " (serving)"
		}
		out.Items = append(out.Items, console.Resource{
			Name: name, Status: status,
			Fields: map[string]string{
				"Image": image, "Digest": digest,
				"Ready": status, "Reason": reason,
				"Age": shortAge(str(m, "creationTimestamp")),
			},
		})
	}
	// Newest first: the revision someone is looking for is the one that just
	// deployed.
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name > out.Items[j].Name })
	out.Total = len(out.Items)
	return out
}

// trafficListing shows where requests actually go.
func trafficListing(svc *ksvcStatus) console.Listing {
	out := console.Listing{
		Columns:    []string{"Percent", "Latest"},
		NameColumn: "Revision",
		Noun:       "traffic targets",
	}
	for _, t := range svc.Status.Traffic {
		latest := "no"
		if t.LatestRevision {
			latest = "yes"
		}
		out.Items = append(out.Items, console.Resource{
			Name: t.RevisionName,
			Fields: map[string]string{
				"Percent": fmt.Sprintf("%d%%", t.Percent),
				"Latest":  latest,
			},
		})
	}
	if len(out.Items) == 0 {
		// Knative reports no split until a revision is ready. Saying so beats
		// an empty table that reads as "no traffic is served".
		out.Note = "Knative reports no traffic split until a revision is ready."
	}
	out.Total = len(out.Items)
	return out
}

// runColumns is what a Cloud Run list has to answer.
//
// It used to be the URL alone, so the one question a deploy screen exists to
// settle — which build is actually serving — could not be answered, and a
// failed deploy said "Failed" with the reason discarded on the floor.
var runColumns = []string{"URL", "Image", "Revision", "Deploying", "Reason", "Detail", "Age"}

// ksvcStatus is the part of a Knative Service the console reads.
type ksvcStatus struct {
	Metadata struct {
		Name              string `json:"name"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		URL                       string `json:"url"`
		LatestReadyRevisionName   string `json:"latestReadyRevisionName"`
		LatestCreatedRevisionName string `json:"latestCreatedRevisionName"`
		Conditions                []struct {
			Type, Status, Reason, Message string
		} `json:"conditions"`
		Traffic []struct {
			RevisionName   string `json:"revisionName"`
			Percent        int    `json:"percent"`
			LatestRevision bool   `json:"latestRevision"`
		} `json:"traffic"`
	} `json:"status"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func (s ksvcStatus) ready() (state, reason, message string) {
	state = "Unknown"
	for _, c := range s.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		switch c.Status {
		case "True":
			state = "Ready"
		case "False":
			// The reason is the whole content of a failure. "Failed" on its
			// own tells a developer that something went wrong, which they
			// already knew from the fact that they are looking.
			state = "Failed"
			reason, message = c.Reason, c.Message
		default:
			state = "Pending"
			reason, message = c.Reason, c.Message
		}
	}
	return state, reason, message
}

func (s ksvcStatus) image() string {
	cs := s.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}

func (s ksvcStatus) resource() console.Resource {
	state, reason, message := s.ready()

	// A revision that was created but has not become ready is a deploy in
	// flight or a deploy that failed, and it is the single most useful thing
	// this screen can say. Equal names mean nothing is in flight.
	deploying := ""
	if c := s.Status.LatestCreatedRevisionName; c != "" && c != s.Status.LatestReadyRevisionName {
		deploying = c
	}

	fields := map[string]string{
		"URL":       s.Status.URL,
		"Image":     s.image(),
		"Revision":  s.Status.LatestReadyRevisionName,
		"Deploying": deploying,
		"Reason":    reason,
		"Age":       shortAge(s.Metadata.CreationTimestamp),
	}
	// The message is long and belongs beside the row rather than in it. The
	// info panel renders declared columns only, so it is declared.
	if message != "" {
		fields["Detail"] = message
	}
	return console.Resource{Name: s.Metadata.Name, Status: state, Fields: fields}
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
		Pattern: `^[a-z0-9][a-z0-9._\-]{1,61}[a-z0-9]$`,
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
	return "Create topic", []console.Field{
		{
			Name: "name", Label: "Topic ID", Type: "text", Required: true,
			Help:    "3-255 characters, starting with a letter.",
			Pattern: `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`,
		},
		{
			// The console this mirrors offers the same checkbox on Create
			// topic, and it is not decoration: a topic with no subscription
			// drops every message published to it, which is a confusing first
			// experience for someone testing a publisher.
			Name: "defaultSubscription", Label: "Add a default subscription", Type: "checkbox",
			Default: "true",
			Help:    "Creates a pull subscription named after the topic, with the default settings.",
		},
	}
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

	// A checkbox that is read and ignored is the working-looking control the
	// parity specification forbids, so the subscription is created through
	// the same API a client would use and its failure is reported. The topic
	// already exists at this point and is left in place: deleting it would
	// destroy something that was created successfully to report a fault in
	// something else.
	if values["defaultSubscription"] == "true" {
		sub := fmt.Sprintf("projects/%s/subscriptions/%s-sub", project, id)
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name:  sub,
			Topic: name,
		}); err != nil {
			return "", fmt.Errorf("the topic was created; its default subscription was not: %w", err)
		}
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
			Pattern: `^[A-Za-z][A-Za-z0-9\-]{0,99}$`,
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
			Pattern: `^[a-z]([a-z0-9\-]{0,47}[a-z0-9])?$`,
			Section: "Service settings",
		},
		{
			Name: "image", Label: "Container image URL", Type: "text", Required: true,
			Default: "ghcr.io/knative/helloworld-go:latest",
			Help: "A tagged image. A locally built one is rewritten to dev.local/ " +
				"and never pulled; an untagged reference is refused.",
			Section: "Container",
		},
		{
			Name: "env", Label: "Environment variables", Type: "textarea",
			Help:    "Optional, as KEY=value separated by commas.",
			Section: "Container",
		},
	}
}

// CreateOnPage implements console.PageCreator.
//
// Three fields would otherwise be a dialog. Deploying a service is the one
// create in this console that starts a container and waits for it to become
// ready, its fields divide into settings that belong to the service and
// settings that belong to the container, and the image field's explanation is
// three lines long. That is a page, not a 440px box.
func (runProvider) CreateOnPage() bool { return true }

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
	// sortKey orders the listing, descending. Empty means the order kubectl
	// returned, which is the cluster's own and is right for a workload list.
	// It is wrong for events, where the newest is the one being looked for.
	sortKey func(item map[string]any) string
	// alwaysStatus declares that this listing has a status column even when
	// no row currently carries one. Without it the column appears and
	// disappears as rows change, and a sort applied to it is lost.
	alwaysStatus bool
	// detail builds the resource's own page from the object. Nil means the
	// rows of this kind cannot be opened, which the console reports rather
	// than offering a link that goes nowhere.
	detail func(item map[string]any) console.Detail
	// enrich joins data the object itself does not carry. A pod's CPU is not
	// in `kubectl get pods`; it is in the kubelet summary, which is a second
	// read. Kept as a hook so the row extractor stays a pure function of one
	// object and remains unit-testable without a cluster.
	enrich func(ctx context.Context, items []console.Resource)
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

	if p.sortKey != nil {
		sort.SliceStable(list.Items, func(i, j int) bool {
			return p.sortKey(list.Items[i]) > p.sortKey(list.Items[j])
		})
	}

	items := make([]console.Resource, 0, len(list.Items))
	for _, raw := range list.Items {
		r, ok := p.row(raw)
		if !ok {
			continue
		}
		items = append(items, r)
	}
	if p.enrich != nil {
		p.enrich(ctx, items)
	}
	return console.Listing{
		Columns: p.columns, Items: items, Total: len(items),
		AlwaysStatus: p.alwaysStatus,
		Note: "Read-only. CloudBurrow owns this cluster; workloads are created " +
			"through Cloud Run or kubectl, not from the console.",
	}, nil
}

// detail implements console.Driller for a cluster object.
//
// Clicking a row did nothing on every Kubernetes screen: the provider offered
// no detail, so a pod that was failing could be seen failing and not asked
// why. The containers, the conditions and the object's own events are the
// three answers, and all three are one kubectl read away.
// CanDrill implements console.OptionalDriller.
//
// One provider type serves Pods, Services, Jobs and Events; only the ones
// given a detail function can open a row, and the console must not offer a
// link for the others.
func (p kubeProvider) CanDrill() bool { return p.detail != nil }

func (p kubeProvider) Detail(ctx context.Context, _, name string) (console.Detail, error) {
	if p.detail == nil {
		return console.Detail{Unavailable: p.title + " rows cannot be opened"}, nil
	}
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, p.kind)
	if err != nil {
		return console.Detail{Unavailable: "cannot read " + p.kind + ": " + err.Error()}, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return console.Detail{Unavailable: "decode " + p.kind + ": " + err.Error()}, nil
	}
	for _, item := range list.Items {
		m := meta(item)
		if str(m, "name") != name {
			continue
		}
		// These screens list every namespace, so a name is not unique. Two
		// objects called "bigtable" in different namespaces would both match
		// and the first one kubectl returned would win.
		if p.namespace != "" && str(m, "namespace") != p.namespace {
			continue
		}
		d := p.detail(item)
		// The object's own events, which is where the reason for a failure
		// actually lives.
		d.Sections = append(d.Sections, console.Section{
			ID: "events", Label: "Events", Listing: p.objectEvents(ctx, name),
		})
		return d, nil
	}
	return console.Detail{Unavailable: "no " + p.kind + " named " + name}, nil
}

// objectEvents lists the events naming one object.
func (p kubeProvider) objectEvents(ctx context.Context, name string) console.Listing {
	out := console.Listing{
		Columns:      []string{"Reason", "Message", "Count", "Last seen"},
		NameColumn:   "Type",
		Noun:         "events",
		AlwaysStatus: true,
	}
	raw, err := kubectlJSON(ctx, p.kubeconfig, "", "events")
	if err != nil {
		out.Unavailable = "cannot read events: " + err.Error()
		return out
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		out.Unavailable = "decode events: " + err.Error()
		return out
	}
	for _, item := range list.Items {
		if str(nested(item, "involvedObject"), "name") != name {
			continue
		}
		count := 1
		if n, ok := item["count"].(float64); ok {
			count = int(n)
		}
		kind := str(item, "type")
		if kind == "" {
			kind = "Normal"
		}
		out.Items = append(out.Items, console.Resource{
			Name: kind, Status: kind,
			Fields: map[string]string{
				"Reason":    str(item, "reason"),
				"Message":   str(item, "message"),
				"Count":     fmt.Sprint(count),
				"Last seen": shortAge(eventTime(item, "last")),
			},
		})
	}
	if len(out.Items) == 0 {
		// Nothing has happened to this object, which is not the same as the
		// events being unreadable.
		out.Note = "The cluster has recorded no events for this object."
	}
	out.Total = len(out.Items)
	return out
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

// podStatus reports what is actually wrong with a pod.
//
// `status.phase` is the wrong field to show. A pod whose container is in
// CrashLoopBackOff has phase Running; one that cannot pull its image has
// phase Pending. Both are the two failures a developer most needs to see, and
// both were rendered as if nothing had happened — an image that does not
// exist looked exactly like a container that had not started yet.
//
// The order matters: deletion first, because a terminating pod's containers
// still report whatever they were doing; then a waiting reason, which is
// where the pull and crash failures live; then a non-zero exit; then the
// phase, which is right for a pod that is genuinely fine.
func podStatus(item map[string]any) string {
	m := meta(item)
	if str(m, "deletionTimestamp") != "" {
		return "Terminating"
	}
	st := nested(item, "status")

	for _, cs := range containerStatuses(item) {
		state, _ := cs["state"].(map[string]any)
		if state == nil {
			continue
		}
		if waiting, ok := state["waiting"].(map[string]any); ok {
			// A reason is the useful half: "ContainerCreating" is normal and
			// "ImagePullBackOff" is not, and only the reason distinguishes them.
			if reason := str(waiting, "reason"); reason != "" {
				return reason
			}
		}
		if term, ok := state["terminated"].(map[string]any); ok {
			code, _ := term["exitCode"].(float64)
			if code != 0 {
				if reason := str(term, "reason"); reason != "" {
					return reason
				}
				return "Error"
			}
		}
	}
	return str(st, "phase")
}

func containerStatuses(item map[string]any) []map[string]any {
	raw, _ := nested(item, "status")["containerStatuses"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, cs := range raw {
		if c, ok := cs.(map[string]any); ok {
			out = append(out, c)
		}
	}
	return out
}

// shortAge formats a creation timestamp the way kubectl does.
//
// Coarse on purpose: "3h" is the answer to "is this new", and a pod's age to
// the second is noise that changes on every poll.
func shortAge(stamp string) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortDigest is the part of an imageID worth showing.
//
// A full sha256 is 71 characters and would be the widest column on the
// screen; the first twelve are what a developer compares.
func shortDigest(imageID string) string {
	if i := strings.Index(imageID, "sha256:"); i >= 0 {
		d := imageID[i+len("sha256:"):]
		if len(d) > 12 {
			return d[:12]
		}
		return d
	}
	return ""
}

func podsProvider(kubeconfig string, metrics console.MetricsSource) kubeProvider {
	return kubeProvider{
		id: "pods", title: "Pods", kind: "pods", kubeconfig: kubeconfig,
		enrich: podUsage(metrics),
		detail: podDetail,
		// Image and readiness are why someone opens this screen: which
		// version is actually running, and is it actually up. Neither was
		// shown, on a console whose whole point is the deployment it runs.
		columns: []string{"Namespace", "Ready", "CPU", "Memory", "Image", "Digest", "Restarts", "Age", "Node", "CloudBurrow"},
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			statuses := containerStatuses(item)

			restarts, ready := 0, 0
			var image, digest string
			for _, c := range statuses {
				if n, ok := c["restartCount"].(float64); ok {
					restarts += int(n)
				}
				if r, ok := c["ready"].(bool); ok && r {
					ready++
				}
				// The digest comes from the status, because that is what is
				// actually running. The name does not: kubelet rewrites
				// status.image to the resolved form, so reading it there put
				// a bare sha256 in the column where the reader expects
				// "postgres:17". The spec below holds what was asked for.
				if digest == "" {
					digest = shortDigest(str(c, "imageID"))
				}
			}
			// The image as written, from the spec. Pairing the requested name
			// with the running digest is the whole answer to "which build is
			// this": either alone is half of it.
			if specs, ok := nested(item, "spec")["containers"].([]any); ok {
				for _, cs := range specs {
					if c, ok := cs.(map[string]any); ok {
						image = str(c, "image")
						break
					}
				}
			}

			return console.Resource{
				Name:   str(m, "name"),
				Status: podStatus(item),
				Fields: map[string]string{
					"Namespace":   str(m, "namespace"),
					"Ready":       fmt.Sprintf("%d/%d", ready, len(statuses)),
					"Image":       image,
					"Digest":      digest,
					"Restarts":    fmt.Sprint(restarts),
					"Age":         shortAge(str(m, "creationTimestamp")),
					"Node":        str(nested(item, "spec"), "nodeName"),
					"CloudBurrow": ownedBy(item),
					// Filled by enrich, or left as the em dash it starts as.
					"CPU":    "—",
					"Memory": "—",
				},
			}, true
		},
	}
}

// podUsage joins each pod's kubelet reading onto its row.
//
// A pod the kubelet did not report keeps its em dash. A missing measurement
// is not a measurement of zero, and showing 0.00 for a pod nobody measured
// would be the console inventing the one number it was asked for.
func podUsage(metrics console.MetricsSource) func(context.Context, []console.Resource) {
	if metrics == nil {
		return nil
	}
	return func(ctx context.Context, items []console.Resource) {
		m := metrics(ctx)
		if m.Unavailable != "" || len(m.Pods) == 0 {
			return
		}
		byName := make(map[string]console.PodMetrics, len(m.Pods))
		for _, pod := range m.Pods {
			byName[pod.Name] = pod
		}
		for i := range items {
			pod, ok := byName[items[i].Name]
			if !ok {
				continue
			}
			if items[i].Fields == nil {
				items[i].Fields = map[string]string{}
			}
			// Labelled as the kubelet's instantaneous usage, because that is
			// what it is: metrics-server is not installed, so there is no
			// windowed average to be had.
			items[i].Fields["CPU"] = fmt.Sprintf("%.3f", pod.CPUUsedCores)
			items[i].Fields["Memory"] = formatBytes(pod.MemoryWorkingSetBytes)
		}
	}
}

// formatBytes renders a byte count the way a reader reads one.
func formatBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// podDetail is one pod's own page: what it is, and what its containers are
// doing.
func podDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")

	summary := []console.Property{
		{Label: "Status", Value: podStatus(item)},
		{Label: "Namespace", Value: str(m, "namespace")},
		{Label: "Node", Value: str(spec, "nodeName")},
		{Label: "Pod IP", Value: str(nested(item, "status"), "podIP")},
		{Label: "QoS class", Value: str(nested(item, "status"), "qosClass")},
		{Label: "Service account", Value: str(spec, "serviceAccountName")},
		{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
	}

	containers := console.Listing{
		Columns:      []string{"Image", "Digest", "State", "Reason", "Restarts"},
		NameColumn:   "Container",
		Noun:         "containers",
		AlwaysStatus: true,
	}
	// The spec holds the images as written; the status holds what each one is
	// doing. Joined by name, because neither alone answers "is this working".
	statuses := map[string]map[string]any{}
	for _, cs := range containerStatuses(item) {
		statuses[str(cs, "name")] = cs
	}
	if specs, ok := spec["containers"].([]any); ok {
		for _, c := range specs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name := str(cm, "name")
			state, reason, restarts, digest := "Unknown", "", "0", ""
			if cs, ok := statuses[name]; ok {
				state, reason = containerState(cs)
				if n, ok := cs["restartCount"].(float64); ok {
					restarts = fmt.Sprint(int(n))
				}
				digest = shortDigest(str(cs, "imageID"))
			}
			containers.Items = append(containers.Items, console.Resource{
				Name: name, Status: state,
				Fields: map[string]string{
					"Image": str(cm, "image"), "Digest": digest,
					"State": state, "Reason": reason, "Restarts": restarts,
				},
			})
		}
	}
	containers.Total = len(containers.Items)

	conditions := console.Listing{
		Columns:      []string{"Reason", "Message", "Since"},
		NameColumn:   "Condition",
		Noun:         "conditions",
		AlwaysStatus: true,
	}
	if conds, ok := nested(item, "status")["conditions"].([]any); ok {
		for _, c := range conds {
			cond, ok := c.(map[string]any)
			if !ok {
				continue
			}
			conditions.Items = append(conditions.Items, console.Resource{
				Name:   str(cond, "type"),
				Status: str(cond, "status"),
				Fields: map[string]string{
					"Reason":  str(cond, "reason"),
					"Message": str(cond, "message"),
					"Since":   shortAge(str(cond, "lastTransitionTime")),
				},
			})
		}
	}
	conditions.Total = len(conditions.Items)

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "containers", Label: "Containers", Listing: containers},
			{ID: "conditions", Label: "Conditions", Listing: conditions},
		},
	}
}

// containerState reports what one container is doing, and why.
func containerState(cs map[string]any) (state, reason string) {
	st, _ := cs["state"].(map[string]any)
	if st == nil {
		return "Unknown", ""
	}
	if w, ok := st["waiting"].(map[string]any); ok {
		return "Waiting", str(w, "reason")
	}
	if t, ok := st["terminated"].(map[string]any); ok {
		code, _ := t["exitCode"].(float64)
		return "Terminated", fmt.Sprintf("%s (exit %d)", str(t, "reason"), int(code))
	}
	if _, ok := st["running"].(map[string]any); ok {
		return "Running", ""
	}
	return "Unknown", ""
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
// eventTime picks the timestamp an event actually carries.
//
// There are two API shapes in play. core/v1 events use firstTimestamp and
// lastTimestamp; events.k8s.io uses eventTime and, for a repeating event,
// series.lastObservedTime. On this very cluster all three observed events
// have eventTime null and rely on lastTimestamp, so the fallback is not
// hypothetical. Returning the empty string rather than a zero time matters:
// a fabricated 1970 is worse than an em dash.
func eventTime(item map[string]any, field string) string {
	switch field {
	case "last":
		for _, candidate := range []string{
			str(item, "lastTimestamp"),
			str(nested(item, "series"), "lastObservedTime"),
			str(item, "eventTime"),
			str(meta(item), "creationTimestamp"),
		} {
			if candidate != "" {
				return candidate
			}
		}
	case "first":
		for _, candidate := range []string{
			str(item, "firstTimestamp"),
			str(item, "eventTime"),
			str(meta(item), "creationTimestamp"),
		} {
			if candidate != "" {
				return candidate
			}
		}
	}
	return ""
}

func eventsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "events", title: "Events", kind: "events", kubeconfig: kubeconfig,
		columns: []string{"Object", "Namespace", "Reason", "Message", "Count", "First seen", "Last seen"},
		// Newest first. An event list in the cluster's own order buries the
		// thing that just broke under everything that has ever happened.
		sortKey: func(item map[string]any) string { return eventTime(item, "last") },
		// Normal is a status, not the absence of one.
		alwaysStatus: true,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			involved := nested(item, "involvedObject")
			kind := str(involved, "kind")
			name := str(involved, "name")

			count := 1
			if n, ok := item["count"].(float64); ok {
				count = int(n)
			}
			// The event's own type, not a word invented for it. "Failed" was
			// wrong: a Warning event is a warning, and plenty of them —
			// BackOff, Unhealthy — describe something retrying rather than
			// something that failed.
			status := str(item, "type")
			if status == "" {
				status = "Normal"
			}
			return console.Resource{
				// The involved object is what the reader is looking for. An
				// event's own metadata.name is a generated string nobody
				// searches for.
				Name: strings.TrimSpace(kind + "/" + name), Status: status,
				Fields: map[string]string{
					"Namespace":  str(m, "namespace"),
					"Object":     str(m, "name"),
					"Reason":     str(item, "reason"),
					"Message":    str(item, "message"),
					"Count":      fmt.Sprint(count),
					"First seen": shortAge(eventTime(item, "first")),
					"Last seen":  shortAge(eventTime(item, "last")),
				},
			}, true
		},
	}
}

// --- Local AI: the catalogue, with each model's real status ----------
//
// The runtime exists: LiteRT-LM builds for Linux from Google's source and was
// measured generating text (docs/local-ai.md §4). What blocks a given model is
// therefore per-model, not global, and the screen says which.
//
// A model is runnable when it can actually be obtained and executed. Today
// that is exactly one: the community Gemma conversion. Every Google-published
// artifact is gated, and every embedding artifact is gated, so those are
// reported as needing credentials rather than as broken.

type aiProvider struct{}

func (aiProvider) ID() string    { return "ai" }
func (aiProvider) Title() string { return "Vertex AI Model Garden" }

func (aiProvider) List(context.Context, string) (console.Listing, error) {
	models := localai.Models()
	items := make([]console.Resource, 0, len(models))
	for _, m := range models {
		runtime := m.Runtime
		if runtime == "" {
			runtime = "none"
		}
		status, why := modelStatus(m)
		items = append(items, console.Resource{
			Name:   m.ID,
			Status: status,
			Fields: map[string]string{
				"Publisher":     string(m.Publisher),
				"Access":        string(m.Access),
				"Modality":      string(m.Modality),
				"Licence":       m.License,
				"Runtime":       runtime,
				"Repository":    m.Repo,
				"Status detail": why,
			},
		})
	}
	return console.Listing{
		Columns: []string{"Publisher", "Access", "Modality", "Licence", "Runtime", "Status detail"},
		Noun:    "models",
		Items:   items, Total: len(items),
		Note: "The generation runtime is built from Google's source and has been " +
			"measured running (docs/local-ai.md). What a model needs is per-model: " +
			"every Google-published artifact is gated, and so is every embedding " +
			"artifact, so the one model that runs today is a community conversion — " +
			"labelled as one, because it is not Google-published.",
	}, nil
}

// modelStatus reports whether a model can actually be used, and why not when
// it cannot.
//
// The reasons differ per model, and a single blanket message would hide that —
// "gated" and "no runtime for this format" need different actions from a user.
func modelStatus(m localai.Model) (status, detail string) {
	switch {
	case m.Runtime == "" && m.Modality == localai.ModalityEmbedding:
		// The embedding runtime exists and builds; what is missing is a model
		// it will accept. Saying "no runtime" would point at the wrong half
		// of the problem and send someone to build something that is already
		// built.
		return "Unavailable", "no compatible model: the embedding runtime builds, " +
			"but every obtainable artifact is rejected — see docs/embeddings.md"
	case m.Runtime == "":
		return "Unavailable", "no runtime for this artifact format"
	case m.Access == localai.AccessGated:
		return "Credentials required",
			"gated: accept the licence and supply a token to download it"
	case m.Publisher == localai.PublisherCommunity:
		return "Available",
			"runs on the locally built runtime; a COMMUNITY conversion, not Google-published"
	default:
		return "Available", "runs on the locally built runtime"
	}
}
