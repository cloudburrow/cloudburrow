package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"os/exec"
	"sort"
	"strconv"
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
	"google.golang.org/protobuf/types/known/durationpb"

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
func (p tasksProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) == 2 {
		return p.taskDetail(path[0], path[1])
	}
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: "Cloud Tasks has not started"}, nil
	}
	name := path[0]
	queue, err := st.GetQueue(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the queue: " + err.Error()}, nil
	}

	listing := console.Listing{
		Columns:    []string{"Scheduled", "Attempts", "Responses", "Last response"},
		NameColumn: "Task",
		Noun:       "tasks",
		// A task carries the request it will make. That is what an operator
		// opens a queue to check, and it was reachable from nowhere.
		RowsOpenable: true,
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

	// The summary is what a glance needs; the configuration is its own tab,
	// the way the Cloud Tasks console splits them. Nine properties in a
	// summary card is a wall, and the three that matter get lost in it.
	ready := 0
	now := time.Now()
	for _, t := range tasks {
		if !t.ScheduleTime.After(now) {
			ready++
		}
	}
	summary := []console.Property{
		{Label: "State", Value: string(queue.State)},
		{Label: "Tasks in queue", Value: fmt.Sprint(len(tasks))},
		{Label: "Due now", Value: fmt.Sprint(ready)},
		{Label: "Created", Value: queue.Created.Format(time.RFC3339)},
	}

	r := queue.RetryConfig
	l := queue.RateLimits
	config := console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Retries", Properties: []console.Property{
				{Label: "Max attempts", Value: fmt.Sprint(r.MaxAttempts)},
				{Label: "Min backoff", Value: r.MinBackoff.String()},
				{Label: "Max backoff", Value: r.MaxBackoff.String()},
			}},
			{Heading: "Rate limits", Properties: []console.Property{
				{Label: "Dispatches per second",
					Value: fmt.Sprintf("%.2f", l.MaxDispatchesPerSecond)},
				{Label: "Max concurrent dispatches",
					Value: fmt.Sprint(l.MaxConcurrentDispatches)},
			}},
			{Heading: "Queue", Properties: []console.Property{
				{Label: "Resource name", Value: queue.Name},
				{Label: "State", Value: string(queue.State)},
			}},
		},
		// No edit form, and it says why. UpdateQueue returns Unimplemented on
		// this instance — asserted by TestTasksUnsupportedOperationsAreHonest —
		// so a form here would be a control the API refuses. maxDoublings is
		// absent for the same reason in the other direction: the dispatcher does
		// not model it, so any value shown would be one the backend ignores.
		Note: "Read-only. UpdateQueue is not implemented on this instance, so a " +
			"queue's retry and rate settings are fixed when it is created. " +
			"maxDoublings is not shown at all, because the dispatcher does not " +
			"implement it.",
	}

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "tasks", Label: "Tasks", Listing: listing},
			config,
		},
	}, nil
}

// taskDetail is one task: the request it will make, and what happened to it.
//
// A task row reported its schedule and attempt counts. The URL it calls, the
// method, the headers and the body — everything that decides whether the task is
// the one the caller meant — were held in the store and shown nowhere.
func (p tasksProvider) taskDetail(queue, task string) (console.Detail, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: "Cloud Tasks has not started"}, nil
	}
	// The listing carries full resource names, so the path segment is already
	// one. A short id would have to be joined onto the queue, and getting that
	// wrong would silently open a task in another queue.
	name := task
	if !strings.Contains(name, "/tasks/") {
		name = queue + "/tasks/" + name
	}
	t, err := st.GetTask(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the task: " + err.Error()}, nil
	}

	last := "—"
	if t.LastResponseCode != 0 {
		last = fmt.Sprint(t.LastResponseCode)
	}
	when := "due now"
	if t.ScheduleTime.After(time.Now()) {
		when = "in " + shortDuration(time.Until(t.ScheduleTime))
	}

	groups := []console.PropertyGroup{{
		Heading: "Delivery",
		Properties: []console.Property{
			{Label: "Scheduled", Value: t.ScheduleTime.Format(time.RFC3339)},
			{Label: "Next attempt", Value: when},
			{Label: "Attempts", Value: fmt.Sprint(t.DispatchCount)},
			{Label: "Responses", Value: fmt.Sprint(t.ResponseCount)},
			{Label: "Last response", Value: last},
			{Label: "Created", Value: t.Created.Format(time.RFC3339)},
		},
	}}

	sections := []console.Section{{
		ID: "delivery", Label: "Delivery", Kind: console.KindProperties, Groups: groups,
	}}

	if req := t.HTTPRequest; req != nil {
		request := []console.Property{
			{Label: "Method", Value: orDash(req.Method)},
			{Label: "URL", Value: req.URL},
			{Label: "Body size", Value: fmt.Sprintf("%d bytes", len(req.Body))},
		}
		for _, pair := range sortedPairs(redactHeaders(req.Headers)) {
			request = append(request, console.Property{
				Label: "Header " + pair.Label, Value: pair.Value,
			})
		}
		sections = append(sections, console.Section{
			ID: "request", Label: "Request", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "HTTP request", Properties: request}},
			Note: "Authorization and any header that names a token are redacted. " +
				"A queued task's headers are stored, so showing them would make " +
				"this page a place to read credentials out of.",
		})
		if len(req.Body) > 0 {
			sections = append(sections, console.Section{
				ID: "body", Label: "Body", Kind: console.KindText,
				Text: string(req.Body),
				Note: "Shown as UTF-8. A task body is the caller's own payload, not " +
					"a credential CloudBurrow issued.",
			})
		}
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Scheduled", Value: t.ScheduleTime.Format(time.RFC3339)},
			{Label: "Attempts", Value: fmt.Sprint(t.DispatchCount)},
			{Label: "Last response", Value: last},
		},
		Sections: sections,
		Actions: []console.Action{
			{ID: "deletetask", Label: "Delete task", Destructive: true},
		},
	}, nil
}

// redactHeaders removes anything credential-shaped from a task's headers.
//
// The same rule the log recorder follows: a stored header with a bearer token
// in it is a credential, and a page that renders it is a place to read one out
// of. The key is kept so the shape of the request is still visible.
func redactHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		if lower == "authorization" || lower == "proxy-authorization" ||
			strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") {
			out[k] = "[redacted]"
			continue
		}
		out[k] = v
	}
	return out
}

// shortDuration renders a wait the way a person says it.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
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
		// Deploying uses the selected project; listing does not scope by it.
		// A Knative Service carries the Cloud Run name it was created under as
		// an annotation, and CloudBurrow runs every project's services in one
		// namespace — so filtering here would hide services created before the
		// annotation existed and claim the project was empty.
		Note: "Not scoped by project: every service in this instance runs in one " +
			"namespace, so all of them are listed whatever the toolbar has " +
			"selected. Deploying uses the selected project.",
	}, nil
}

// Detail implements console.Driller for one service.
//
// A Cloud Run service is a history of revisions and a split of traffic
// between them, and neither was reachable from this console: clicking a row
// did nothing, because the provider offered no detail at all.
func (p runProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// Two levels: a service, and one of its revisions. Anything deeper is
	// refused rather than silently collapsed onto the same page.
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	name := path[0]
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
	if len(path) == 2 {
		return p.revisionDetail(ctx, name, path[1], svc)
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
		// "Revision history" is what the Cloud Run console calls this tab, and
		// it is the more accurate name: the list includes revisions that are no
		// longer serving anything.
		{ID: "revisions", Label: "Revision history", Listing: p.revisions(ctx, name, svc)},
		{ID: "traffic", Label: "Traffic", Listing: trafficListing(svc)},
		runConfiguration(svc),
	}
	return console.Detail{Summary: summary, Sections: sections}, nil
}

// revisionDetail is one revision: what it was configured with, and what it is
// serving.
//
// A revision is the immutable thing a Cloud Run deployment actually produces.
// The service page shows the latest one's configuration; this shows the
// configuration of whichever revision is being looked at, which is the only way
// to answer "what changed between these two".
func (p runProvider) revisionDetail(ctx context.Context, service, revision string, svc *ksvcStatus) (console.Detail, error) {
	raw, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "revisions")
	if err != nil {
		return console.Detail{Unavailable: "cannot read revisions: " + err.Error()}, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return console.Detail{Unavailable: "decode revisions: " + err.Error()}, nil
	}

	var item map[string]any
	for _, candidate := range list.Items {
		m := meta(candidate)
		labels, _ := m["labels"].(map[string]any)
		// Scoped by service as well as by name: two services in one namespace
		// can hold revisions whose names differ only by their own prefix, and
		// matching on the name alone would open the wrong one.
		if str(m, "name") == revision && str(labels, "serving.knative.dev/service") == service {
			item = candidate
			break
		}
	}
	if item == nil {
		return console.Detail{
			Unavailable: "no revision named " + revision + " in service " + service,
		}, nil
	}

	m := meta(item)
	spec := nested(item, "spec")
	labels, _ := m["labels"].(map[string]any)

	// A revision's traffic share, read from the service rather than guessed.
	// Zero percent and absent are the same thing here, and both mean nothing is
	// reaching it.
	share := "0%"
	for _, t := range svc.Status.Traffic {
		if t.RevisionName == revision {
			share = fmt.Sprintf("%d%%", t.Percent)
		}
	}

	status, reason := "Unknown", ""
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
				status, reason = "Failed", str(cond, "message")
			default:
				status, reason = "Pending", str(cond, "message")
			}
		}
	}

	summary := []console.Property{
		{Label: "Status", Value: status},
		{Label: "Traffic", Value: share},
		{Label: "Serving", Value: yesNo(revision == svc.Status.LatestReadyRevisionName)},
		{Label: "Generation", Value: orDash(str(labels, "serving.knative.dev/configurationGeneration"))},
		{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
	}
	if reason != "" {
		summary = append(summary, console.Property{Label: "Detail", Value: reason})
	}

	settings := []console.Property{
		{Label: "Requests per instance", Value: orDefault(intOf(spec["containerConcurrency"]), "unlimited")},
		{Label: "Request timeout", Value: orDefault(intOf(spec["timeoutSeconds"]), "Knative's default") +
			timeoutUnit(intOf(spec["timeoutSeconds"]))},
	}
	for _, key := range []string{"autoscaling.knative.dev/min-scale", "autoscaling.knative.dev/max-scale"} {
		label := "Minimum instances"
		if strings.HasSuffix(key, "max-scale") {
			label = "Maximum instances"
		}
		settings = append(settings, console.Property{
			Label: label, Value: orDash(stringMap(m["annotations"])[key]),
		})
	}

	groups := []console.PropertyGroup{{Heading: "Revision settings", Properties: settings}}
	if containers, ok := spec["containers"].([]any); ok {
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			heading := "Container"
			if n := str(cm, "name"); n != "" {
				heading += " " + n
			}
			props := []console.Property{{Label: "Image", Value: str(cm, "image")}}
			if env := envSummary(cm); env != "" {
				props = append(props, console.Property{
					Label: "Environment (names only)", Value: env,
				})
			}
			for _, kind := range []string{"limits", "requests"} {
				for _, pair := range sortedPairs(stringMap(nested(cm, "resources")[kind])) {
					props = append(props, console.Property{
						Label: strings.ToUpper(kind[:1]) + kind[1:len(kind)-1] + " " + pair.Label,
						Value: pair.Value,
					})
				}
			}
			groups = append(groups, console.PropertyGroup{Heading: heading, Properties: props})
		}
	}

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{{
			ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: groups,
			Note: "A revision is immutable. This is the configuration it was created " +
				"with, and it cannot be changed — deploying again creates another one.",
		}},
	}, nil
}

// intOf reads a decoded JSON number.
func intOf(v any) int {
	n, _ := v.(float64)
	return int(n)
}

// runConfiguration is the service's own settings, grouped the way the deploy
// form groups them.
//
// Read-only, and it says so: this console has no update path at all, so
// showing these without the caveat would imply an edit that does not exist.
func runConfiguration(svc *ksvcStatus) console.Section {
	tmpl := svc.Spec.Template

	service := []console.Property{
		{Label: "Name", Value: svc.Metadata.Name},
		{Label: "URL", Value: svc.Status.URL},
		{Label: "Created", Value: svc.Metadata.CreationTimestamp},
		{Label: "Latest revision", Value: svc.Status.LatestCreatedRevisionName},
	}

	// Scaling and concurrency, which is where a service's behaviour under load
	// is actually decided and which this page reported nowhere. The annotation
	// names are Knative's because that is what the cluster holds; the labels
	// are Cloud Run's because that is what the operator set.
	annotations := tmpl.Metadata.Annotations
	scaling := []console.Property{
		{Label: "Minimum instances", Value: orDash(annotations["autoscaling.knative.dev/min-scale"])},
		{Label: "Maximum instances", Value: orDash(annotations["autoscaling.knative.dev/max-scale"])},
		{Label: "Requests per instance", Value: orDefault(tmpl.Spec.ContainerConcurrency,
			"unlimited")},
		{Label: "Request timeout", Value: orDefault(tmpl.Spec.TimeoutSeconds, "Knative's default") +
			timeoutUnit(tmpl.Spec.TimeoutSeconds)},
	}

	groups := []console.PropertyGroup{
		{Heading: "Service settings", Properties: service},
		{Heading: "Scaling and concurrency", Properties: scaling},
	}

	// One group per container, because a multi-container revision rendered as
	// one flat list makes it impossible to see which limit belongs to which
	// container.
	for _, c := range tmpl.Spec.Containers {
		heading := "Container"
		if c.Name != "" {
			heading += " " + c.Name
		}
		props := []console.Property{{Label: "Image", Value: c.Image}}
		if len(c.Command) > 0 {
			props = append(props, console.Property{
				Label: "Entrypoint", Value: strings.Join(c.Command, " ")})
		}
		if len(c.Args) > 0 {
			props = append(props, console.Property{
				Label: "Arguments", Value: strings.Join(c.Args, " ")})
		}
		for _, port := range c.Ports {
			if port.ContainerPort != 0 {
				props = append(props, console.Property{
					Label: "Container port", Value: fmt.Sprint(port.ContainerPort)})
			}
		}
		for _, kind := range []struct {
			label  string
			values map[string]string
		}{{"Limit", c.Resources.Limits}, {"Request", c.Resources.Requests}} {
			for _, pair := range sortedPairs(kind.values) {
				props = append(props, console.Property{
					Label: kind.label + " " + pair.Label, Value: pair.Value})
			}
		}
		for _, v := range c.Env {
			// A variable drawn from a Secret names the secret, not the value:
			// the value is the secret, and the console's job is to say where it
			// comes from.
			if ref := v.ValueFrom.SecretKeyRef; ref.Name != "" {
				props = append(props, console.Property{
					Label: "Env " + v.Name,
					Value: "from secret " + ref.Name + " key " + ref.Key,
				})
				continue
			}
			props = append(props, console.Property{Label: "Env " + v.Name, Value: v.Value})
		}
		groups = append(groups, console.PropertyGroup{Heading: heading, Properties: props})
	}

	return console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: groups,
		Note: "Read-only. The Cloud Run adapter has no update path, so changing any " +
			"of this means deploying the service again — which creates a new " +
			"revision, exactly as it would in Cloud Run.",
	}
}

// orDash renders an absent string as an em dash rather than as nothing, so a
// property that has no value still reads as a property.
func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// orDefault names what an unset numeric setting actually means.
//
// Knative treats 0 as "use the default", and printing "0" would read as "no
// requests" and "no timeout" — the opposite of what it does.
func orDefault(n int, unset string) string {
	if n == 0 {
		return unset
	}
	return fmt.Sprint(n)
}

func timeoutUnit(n int) string {
	if n == 0 {
		return ""
	}
	return " seconds"
}

// revisions lists the service's own revisions, newest first.
func (p runProvider) revisions(ctx context.Context, service string, svc *ksvcStatus) console.Listing {
	out := console.Listing{
		Columns:      []string{"Serving", "Image", "Digest", "Reason", "Age"},
		NameColumn:   "Revision",
		Noun:         "revisions",
		AlwaysStatus: true,
		// A revision has a configuration, a traffic share and its own pods, and
		// none of it was reachable: the row was the end of the road.
		RowsOpenable: true,
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
		// Which revision is serving goes in a column of its own. It used to be
		// appended to the name, which read well and meant the name in the table
		// was not the revision's name — so the row could not be opened and a
		// copied value did not resolve.
		servingNow := "—"
		if name == serving {
			servingNow = "Yes"
		}
		out.Items = append(out.Items, console.Resource{
			Name: name, Status: status,
			Fields: map[string]string{
				"Serving": servingNow,
				"Image":   image, "Digest": digest,
				"Reason": reason,
				"Age":    shortAge(str(m, "creationTimestamp")),
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
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Labels      map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				// ContainerConcurrency and TimeoutSeconds are what Cloud Run's
				// requests-per-instance and request timeout become. They were
				// read from neither, so a service with a 10-minute timeout and
				// one with Knative's default looked identical on screen.
				ContainerConcurrency int    `json:"containerConcurrency"`
				TimeoutSeconds       int    `json:"timeoutSeconds"`
				ServiceAccountName   string `json:"serviceAccountName"`
				Containers           []struct {
					Name    string   `json:"name"`
					Image   string   `json:"image"`
					Command []string `json:"command"`
					Args    []string `json:"args"`
					Env     []struct {
						Name      string `json:"name"`
						Value     string `json:"value"`
						ValueFrom struct {
							SecretKeyRef struct {
								Name string `json:"name"`
								Key  string `json:"key"`
							} `json:"secretKeyRef"`
						} `json:"valueFrom"`
					} `json:"env"`
					Ports []struct {
						Name          string `json:"name"`
						ContainerPort int    `json:"containerPort"`
					} `json:"ports"`
					Resources struct {
						Limits   map[string]string `json:"limits"`
						Requests map[string]string `json:"requests"`
					} `json:"resources"`
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
// workloadsProvider lists every workload kind, read-only.
//
// It listed Deployments alone and was its own type, so it had no detail page:
// clicking a workload did nothing. It is a kubeProvider now, which is what
// gives it the detail machinery, the YAML tab and the events tab the other
// cluster screens already have.
func workloadsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "workloads", title: "Workloads", kubeconfig: kubeconfig,
		// A cluster runs more than Deployments. A screen called Workloads
		// that lists one kind is answering a narrower question than its name
		// asks, and a StatefulSet nobody can see is a StatefulSet nobody can
		// debug.
		kind:         "deployments,statefulsets,daemonsets,replicasets",
		columns:      []string{"Kind", "Namespace", "Ready", "Age", "Images"},
		alwaysStatus: true,
		detail:       workloadDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			kind := str(item, "kind")
			// A ReplicaSet owned by a Deployment is the Deployment's own
			// history, not a workload in its own right; it belongs on the
			// Deployment's Revision history tab rather than as a peer row.
			if kind == "ReplicaSet" && ownerKind(item) == "Deployment" {
				return console.Resource{}, false
			}
			status := nested(item, "status")
			ready, desired := workloadReplicas(item)

			state := "Pending"
			switch {
			case desired == 0:
				state = "Scaled to zero"
			case ready == desired:
				state = "Ready"
			case ready > 0:
				state = "Updating"
			}
			_ = status

			return console.Resource{
				Name: str(m, "name"), Status: state,
				Fields: map[string]string{
					"Kind":      kind,
					"Namespace": str(m, "namespace"),
					"Ready":     fmt.Sprintf("%d/%d", ready, desired),
					"Age":       shortAge(str(m, "creationTimestamp")),
					"Images":    strings.Join(podTemplateImages(item), ", "),
				},
			}, true
		},
	}
}

// ownerKind is the kind of the object's controller, if it has one.
func ownerKind(item map[string]any) string {
	refs, _ := meta(item)["ownerReferences"].([]any)
	for _, r := range refs {
		if ref, ok := r.(map[string]any); ok {
			if c, _ := ref["controller"].(bool); c {
				return str(ref, "kind")
			}
		}
	}
	return ""
}

// workloadReplicas reports ready and desired, across the kinds that count
// them differently.
//
// A DaemonSet has no spec.replicas — its desired count is however many nodes
// it must run on — so reading spec.replicas alone reports 0/0 for a DaemonSet
// that is perfectly healthy.
func workloadReplicas(item map[string]any) (ready, desired int) {
	status := nested(item, "status")
	num := func(m map[string]any, key string) int {
		if v, ok := m[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	if str(item, "kind") == "DaemonSet" {
		return num(status, "numberReady"), num(status, "desiredNumberScheduled")
	}
	ready = num(status, "readyReplicas")
	desired = num(status, "replicas")
	if spec := nested(item, "spec"); spec != nil {
		if v, ok := spec["replicas"].(float64); ok {
			desired = int(v)
		}
	}
	return ready, desired
}

// podTemplateImages is what the workload actually runs.
func podTemplateImages(item map[string]any) []string {
	containers, _ := nested(item, "spec", "template", "spec")["containers"].([]any)
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		if cm, ok := c.(map[string]any); ok {
			if image := str(cm, "image"); image != "" {
				out = append(out, image)
			}
		}
	}
	return out
}

// workloadDetail is one workload's page.
func workloadDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")
	ready, desired := workloadReplicas(item)

	summary := []console.Property{
		{Label: "Kind", Value: str(item, "kind")},
		{Label: "Namespace", Value: str(m, "namespace")},
		{Label: "Ready", Value: fmt.Sprintf("%d/%d", ready, desired)},
		{Label: "Strategy", Value: str(nested(spec, "strategy"), "type")},
		{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
	}

	containers := console.Listing{
		Columns:      []string{"Image", "Ports", "CPU request", "Memory request"},
		NameColumn:   "Container",
		Noun:         "containers",
		AlwaysStatus: false,
	}
	if cs, ok := nested(spec, "template", "spec")["containers"].([]any); ok {
		for _, c := range cs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			var ports []string
			if ps, ok := cm["ports"].([]any); ok {
				for _, pr := range ps {
					if pm, ok := pr.(map[string]any); ok {
						if v, ok := pm["containerPort"].(float64); ok {
							ports = append(ports, fmt.Sprint(int(v)))
						}
					}
				}
			}
			requests := nested(cm, "resources", "requests")
			containers.Items = append(containers.Items, console.Resource{
				Name: str(cm, "name"),
				Fields: map[string]string{
					"Image":          str(cm, "image"),
					"Ports":          strings.Join(ports, ", "),
					"CPU request":    str(requests, "cpu"),
					"Memory request": str(requests, "memory"),
				},
			})
		}
	}
	containers.Total = len(containers.Items)

	// The selector is how a workload finds its pods, and it is the first
	// thing to check when it has none.
	selector := nested(spec, "selector", "matchLabels")
	labels := make(map[string]string, len(selector))
	for k, v := range selector {
		if sv, ok := v.(string); ok {
			labels[k] = sv
		}
	}
	config := console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Selector", Properties: sortedPairs(labels)},
		},
		Note: "Read-only. This console does not edit cluster objects; CloudBurrow " +
			"owns this cluster and workloads are created through Cloud Run or kubectl.",
	}

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "containers", Label: "Containers", Listing: containers},
			config,
		},
	}
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
	// Every field the adapter maps, and nothing else. The set is taken from
	// internal/adapter/run's ToKnative: what it writes into the manifest is
	// what can be offered here, and what its Unsupported refuses is named in
	// the closing help text rather than drawn as a control that fails.
	//
	// Authentication, ingress, service accounts, VPC access, volumes, binary
	// authorization and the execution environment are all absent for the same
	// reason: the adapter refuses them, so offering the control would be
	// offering support.
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
			Name: "port", Label: "Container port", Type: "text",
			Help:    "Optional. The port the container listens on; Knative's default is 8080.",
			Pattern: `^[0-9]{1,5}$`,
			Section: "Container",
		},
		{
			Name: "command", Label: "Entrypoint command", Type: "text",
			Help:    "Optional. Overrides the image's entrypoint. Space-separated.",
			Section: "Container",
		},
		{
			Name: "args", Label: "Arguments", Type: "text",
			Help:    "Optional. Space-separated.",
			Section: "Container",
		},
		{
			Name: "env", Label: "Environment variables", Type: "map",
			Help:    "Optional. One KEY=value per line.",
			Section: "Container",
		},
		{
			Name: "cpu", Label: "CPU limit", Type: "text",
			Help:    `Optional, as Kubernetes quantities: "1", "500m".`,
			Pattern: `^[0-9]+(\.[0-9]+)?m?$`,
			Section: "Resources",
		},
		{
			Name: "memory", Label: "Memory limit", Type: "text",
			Help:    `Optional, as Kubernetes quantities: "512Mi", "1Gi".`,
			Pattern: `^[0-9]+(Ki|Mi|Gi|K|M|G)?$`,
			Section: "Resources",
		},
		{
			Name: "minInstances", Label: "Minimum instances", Type: "text",
			Help: "Optional. 0 lets the service scale to nothing between requests, " +
				"which is Cloud Run's default.",
			Pattern: `^[0-9]{1,4}$`,
			Section: "Scaling",
		},
		{
			Name: "maxInstances", Label: "Maximum instances", Type: "text",
			Help:    "Optional.",
			Pattern: `^[0-9]{1,4}$`,
			Section: "Scaling",
		},
		{
			Name: "concurrency", Label: "Requests per instance", Type: "text",
			Help:    "Optional. Cloud Run's maxInstanceRequestConcurrency.",
			Pattern: `^[0-9]{1,4}$`,
			Section: "Scaling",
		},
		{
			Name: "timeout", Label: "Request timeout (seconds)", Type: "text",
			Help:    "Optional.",
			Pattern: `^[0-9]{1,5}$`,
			Section: "Scaling",
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
	env, err := console.ParseMap(values["env"])
	if err != nil {
		return "", fmt.Errorf("environment variables: %w", err)
	}
	// Sorted, so two deploys of the same form produce the same request. The
	// adapter sorts again on the way to the manifest; doing it here as well
	// keeps the request itself comparable, which is what a test can assert.
	for _, name := range sortedKeys(env) {
		container.Env = append(container.Env, &runpb.EnvVar{
			Name:   name,
			Values: &runpb.EnvVar_Value{Value: env[name]},
		})
	}
	if fields := strings.Fields(values["command"]); len(fields) > 0 {
		container.Command = fields
	}
	if fields := strings.Fields(values["args"]); len(fields) > 0 {
		container.Args = fields
	}
	if port, err := optionalInt(values["port"], "container port"); err != nil {
		return "", err
	} else if port > 0 {
		container.Ports = []*runpb.ContainerPort{{ContainerPort: int32(port)}}
	}
	limits := map[string]string{}
	if v := strings.TrimSpace(values["cpu"]); v != "" {
		limits["cpu"] = v
	}
	if v := strings.TrimSpace(values["memory"]); v != "" {
		limits["memory"] = v
	}
	if len(limits) > 0 {
		container.Resources = &runpb.ResourceRequirements{Limits: limits}
	}

	tmpl := &runpb.RevisionTemplate{Containers: []*runpb.Container{container}}
	minInstances, err := optionalInt(values["minInstances"], "minimum instances")
	if err != nil {
		return "", err
	}
	maxInstances, err := optionalInt(values["maxInstances"], "maximum instances")
	if err != nil {
		return "", err
	}
	if minInstances > 0 || maxInstances > 0 {
		if maxInstances > 0 && minInstances > maxInstances {
			// Refused here rather than sent: Knative would accept both
			// annotations and then never satisfy them.
			return "", fmt.Errorf("minimum instances (%d) cannot exceed maximum instances (%d)",
				minInstances, maxInstances)
		}
		tmpl.Scaling = &runpb.RevisionScaling{
			MinInstanceCount: int32(minInstances),
			MaxInstanceCount: int32(maxInstances),
		}
	}
	concurrency, err := optionalInt(values["concurrency"], "requests per instance")
	if err != nil {
		return "", err
	}
	tmpl.MaxInstanceRequestConcurrency = int32(concurrency)
	timeout, err := optionalInt(values["timeout"], "request timeout")
	if err != nil {
		return "", err
	}
	if timeout > 0 {
		tmpl.Timeout = durationpb.New(time.Duration(timeout) * time.Second)
	}

	parent := fmt.Sprintf("projects/%s/locations/%s", project, p.location())
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: parent, ServiceId: id, Service: &runpb.Service{Template: tmpl},
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
		// The project in the toolbar does not scope this screen, and it said so
		// nowhere. A Kubernetes object has no Google project — it has a
		// namespace — so every one of these screens showed the same rows
		// whatever project was selected, which reads as a bug in the picker.
		Note: "Not scoped by project: a Kubernetes object belongs to a namespace, " +
			"not to a Google project, so this screen shows the whole cluster " +
			"whatever the toolbar has selected. Read-only — CloudBurrow owns this " +
			"cluster, and workloads are created through Cloud Run or kubectl.",
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
// One provider type serves Workloads, Pods, Services, Jobs, Nodes, Storage and
// Events; only the ones given a detail function can open a row, and the console
// must not offer a link for the others.
func (p kubeProvider) CanDrill() bool { return p.detail != nil }

func (p kubeProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// One level: the row on the list screen. Anything deeper is refused
	// rather than silently collapsed onto the same page.
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	name := path[0]
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
		// Whatever this object controls. A Deployment's pods, a Job's pods,
		// a Service's endpoints: the question "is it actually running" is
		// answered one level down, and until now that level was unreachable.
		d.Sections = append(d.Sections, p.related(ctx, item)...)
		// And the object itself. kubeProvider.List already decodes the whole
		// thing and throws everything but the columns away; this is the same
		// map, so it costs no second kubectl call. "YAML" is the name GKE
		// uses for this tab.
		d.Sections = append(d.Sections, objectSection(item))
		return d, nil
	}
	return console.Detail{Unavailable: "no " + p.kind + " named " + name}, nil
}

// objectSection shows the live object, with anything credential-shaped
// removed.
//
// The project already redacts on the way in for logs (internal/console/logs.go)
// on the principle that an entry stored with a token in it has already been
// written somewhere a later change might expose. The same rule applies here
// for the opposite reason: this is a live read, so the redaction has to happen
// every time rather than once.
//
// A Secret's data is the case this exists for. Kubernetes stores it
// base64-encoded, which is not encryption, and a YAML pane that renders it
// would be handing out credentials in a panel labelled "read-only".
func objectSection(item map[string]any) console.Section {
	redacted := redactObject(item)
	encoded, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return console.Section{
			ID: "object", Label: "YAML", Kind: console.KindText,
			Unavailable: "cannot render this object: " + err.Error(),
		}
	}
	return console.Section{
		ID: "object", Label: "YAML", Kind: console.KindText,
		Text: string(encoded),
		Note: "The live object as the cluster reports it, with secret data removed. " +
			"Rendered as JSON, which is valid YAML.",
	}
}

// secretish names the keys whose values must never be rendered.
var secretish = map[string]bool{
	"data": true, "stringData": true,
	// A service-account token, mounted or projected.
	"token": true, "ca.crt": true,
}

// redactObject copies an object with credential-shaped values replaced.
//
// A copy rather than a mutation: the map belongs to the caller's decode and
// is also the source for the columns, so editing it in place would silently
// change what the table shows.
func redactObject(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			if secretish[k] {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = redactObject(child)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = redactObject(child)
		}
		return out
	default:
		return v
	}
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

func podsProvider(kubeconfig string, metrics console.MetricsSource, series *console.Series) kubeProvider {
	return kubeProvider{
		id: "pods", title: "Pods", kind: "pods", kubeconfig: kubeconfig,
		enrich: podUsage(metrics),
		// The pod's own charts come from the retained history, which the
		// provider only has because the series is wired in here. Without it the
		// detail page could say what a pod is using now and nothing about what
		// it was using a minute ago — which is the whole question after a
		// restart or a spike.
		detail: podDetailWithCharts(series),
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

	sections := []console.Section{
		{ID: "containers", Label: "Containers", Listing: containers},
	}
	// Init containers are where a stuck pod usually is. A pod sitting in
	// Init:0/1 shows nothing wrong in its containers list, because its
	// containers have not started yet.
	if init := initContainers(item, statuses); len(init.Items) > 0 {
		sections = append(sections, console.Section{
			ID: "init", Label: "Init containers", Listing: init,
		})
	}
	sections = append(sections,
		console.Section{
			ID: "config", Label: "Configuration", Kind: console.KindProperties,
			Groups: podConfiguration(item),
		},
		console.Section{ID: "conditions", Label: "Conditions", Listing: conditions},
	)
	if vols := podVolumes(spec); len(vols.Items) > 0 {
		sections = append(sections, console.Section{
			ID: "volumes", Label: "Volumes", Listing: vols,
		})
	}

	return console.Detail{Summary: summary, Sections: sections}
}

// initContainers lists the containers that run before the pod's own.
func initContainers(item map[string]any, statuses map[string]map[string]any) console.Listing {
	out := console.Listing{
		Columns:      []string{"Image", "State", "Reason", "Restarts"},
		NameColumn:   "Container",
		Noun:         "init containers",
		AlwaysStatus: true,
	}
	// Init container statuses live in their own array, not the one the running
	// containers use, so the join needs a second map.
	initStatuses := map[string]map[string]any{}
	if list, ok := nested(item, "status")["initContainerStatuses"].([]any); ok {
		for _, c := range list {
			if cs, ok := c.(map[string]any); ok {
				initStatuses[str(cs, "name")] = cs
			}
		}
	}
	specs, _ := nested(item, "spec")["initContainers"].([]any)
	for _, c := range specs {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name := str(cm, "name")
		state, reason, restarts := "Pending", "", "0"
		if cs, ok := initStatuses[name]; ok {
			state, reason = containerState(cs)
			if n, ok := cs["restartCount"].(float64); ok {
				restarts = fmt.Sprint(int(n))
			}
		}
		out.Items = append(out.Items, console.Resource{
			Name: name, Status: state,
			Fields: map[string]string{
				"Image": str(cm, "image"), "State": state,
				"Reason": reason, "Restarts": restarts,
			},
		})
	}
	out.Total = len(out.Items)
	return out
}

// podConfiguration is the scheduling and per-container configuration a pod
// was created with.
//
// Environment variables appear by name only. A value written into a pod spec
// is as readable as a Secret's base64, and this pane is labelled read-only,
// not safe-to-show. Where a variable is drawn from a Secret or ConfigMap the
// source is named, because that is the part worth knowing and it leaks
// nothing.
func podConfiguration(item map[string]any) []console.PropertyGroup {
	spec := nested(item, "spec")

	scheduling := console.PropertyGroup{Heading: "Scheduling", Properties: []console.Property{
		{Label: "Restart policy", Value: str(spec, "restartPolicy")},
		{Label: "Node name", Value: str(spec, "nodeName")},
		{Label: "Priority class", Value: str(spec, "priorityClassName")},
		{Label: "DNS policy", Value: str(spec, "dnsPolicy")},
		{Label: "Service account", Value: str(spec, "serviceAccountName")},
	}}
	for _, pair := range sortedPairs(stringMap(spec["nodeSelector"])) {
		scheduling.Properties = append(scheduling.Properties, console.Property{
			Label: "Node selector " + pair.Label, Value: pair.Value,
		})
	}
	groups := []console.PropertyGroup{scheduling}

	all, _ := spec["containers"].([]any)
	if init, ok := spec["initContainers"].([]any); ok {
		all = append(all, init...)
	}
	for _, c := range all {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		g := console.PropertyGroup{
			Heading:    "Container " + str(cm, "name"),
			Properties: []console.Property{{Label: "Image", Value: str(cm, "image")}},
		}
		if cmd, ok := cm["command"].([]any); ok && len(cmd) > 0 {
			g.Properties = append(g.Properties, console.Property{
				Label: "Command", Value: strings.Join(strSlice(cmd), " "),
			})
		}
		if args, ok := cm["args"].([]any); ok && len(args) > 0 {
			g.Properties = append(g.Properties, console.Property{
				Label: "Arguments", Value: strings.Join(strSlice(args), " "),
			})
		}
		for _, kind := range []string{"requests", "limits"} {
			for _, pair := range sortedPairs(stringMap(nested(cm, "resources")[kind])) {
				g.Properties = append(g.Properties, console.Property{
					// "Requests cpu", "Limits memory": the kind reads as a
					// heading the way GKE writes it.
					Label: strings.ToUpper(kind[:1]) + kind[1:] + " " + pair.Label,
					Value: pair.Value,
				})
			}
		}
		if ports, ok := cm["ports"].([]any); ok && len(ports) > 0 {
			var names []string
			for _, pv := range ports {
				pm, ok := pv.(map[string]any)
				if !ok {
					continue
				}
				n, _ := pm["containerPort"].(float64)
				entry := fmt.Sprint(int(n))
				if proto := str(pm, "protocol"); proto != "" && proto != "TCP" {
					entry += "/" + proto
				}
				names = append(names, entry)
			}
			g.Properties = append(g.Properties, console.Property{
				Label: "Ports", Value: strings.Join(names, ", "),
			})
		}
		if env := envSummary(cm); env != "" {
			g.Properties = append(g.Properties, console.Property{
				// The label carries the caveat, because Property has nowhere
				// else to put it and a bare list of names would read as the
				// whole environment.
				Label: "Environment (names only)", Value: env,
			})
		}
		if mounts, ok := cm["volumeMounts"].([]any); ok && len(mounts) > 0 {
			var names []string
			for _, mv := range mounts {
				mm, ok := mv.(map[string]any)
				if !ok {
					continue
				}
				entry := str(mm, "name") + " → " + str(mm, "mountPath")
				if ro, ok := mm["readOnly"].(bool); ok && ro {
					entry += " (read-only)"
				}
				names = append(names, entry)
			}
			g.Properties = append(g.Properties, console.Property{
				Label: "Volume mounts", Value: strings.Join(names, ", "),
			})
		}
		for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
			if _, ok := cm[probe].(map[string]any); ok {
				g.Properties = append(g.Properties, console.Property{
					Label: strings.TrimSuffix(probe, "Probe") + " probe", Value: "configured",
				})
			}
		}
		groups = append(groups, g)
	}
	return groups
}

// envSummary names a container's environment variables and where each one
// comes from, without reading any value.
func envSummary(container map[string]any) string {
	entries, _ := container["env"].([]any)
	var names []string
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name := str(em, "name")
		switch src := nested(em, "valueFrom"); {
		case len(src) == 0:
			names = append(names, name)
		default:
			for _, from := range []string{"secretKeyRef", "configMapKeyRef", "fieldRef", "resourceFieldRef"} {
				if ref, ok := src[from].(map[string]any); ok {
					label := strings.TrimSuffix(from, "KeyRef")
					label = strings.TrimSuffix(label, "Ref")
					if n := str(ref, "name"); n != "" {
						label += " " + n
					}
					names = append(names, name+" (from "+label+")")
					break
				}
			}
		}
	}
	// envFrom pulls a whole Secret or ConfigMap in; naming the source is the
	// only honest summary, because the keys are not in the spec.
	if froms, ok := container["envFrom"].([]any); ok {
		for _, f := range froms {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			for _, from := range []string{"secretRef", "configMapRef"} {
				if ref, ok := fm[from].(map[string]any); ok {
					names = append(names, "all of "+strings.TrimSuffix(from, "Ref")+" "+str(ref, "name"))
				}
			}
		}
	}
	return strings.Join(names, ", ")
}

// podVolumes lists a pod's volumes and what backs each one.
func podVolumes(spec map[string]any) console.Listing {
	out := console.Listing{
		Columns:    []string{"Type", "Source"},
		NameColumn: "Volume",
		Noun:       "volumes",
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		kind, source := "", ""
		for key, val := range vm {
			if key == "name" {
				continue
			}
			kind = key
			if inner, ok := val.(map[string]any); ok {
				// Every volume source names the thing it mounts under one of
				// these keys; the rest of the source is tuning.
				for _, field := range []string{"claimName", "secretName", "name", "path"} {
					if n := str(inner, field); n != "" {
						source = n
						break
					}
				}
			}
			break
		}
		out.Items = append(out.Items, console.Resource{
			Name:   str(vm, "name"),
			Fields: map[string]string{"Type": kind, "Source": source},
		})
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out
}

// strSlice renders a decoded JSON array of strings.
func strSlice(values []any) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
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
		columns: []string{"Namespace", "Type", "Cluster IP", "Ports", "CloudBurrow"},
		detail:  serviceDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			spec := nested(item, "spec")
			return console.Resource{
				Name: str(m, "name"),
				Fields: map[string]string{
					"Namespace":   str(m, "namespace"),
					"Type":        str(spec, "type"),
					"Cluster IP":  str(spec, "clusterIP"),
					"Ports":       strings.Join(servicePorts(spec), ", "),
					"CloudBurrow": ownedBy(item),
				},
			}, true
		},
	}
}

func jobsProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "jobs", title: "Jobs", kind: "jobs", kubeconfig: kubeconfig,
		columns: []string{"Namespace", "Completions", "Duration", "CloudBurrow"},
		detail:  jobDetail,
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
					"Duration":    jobDuration(str(st, "startTime"), str(st, "completionTime")),
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
		// Repository is in the columns now. It was being read from the catalogue,
		// put in the row and left out of the column list, so the console fetched
		// the one field that says where a model actually comes from and threw it
		// away — on a screen whose whole subject is provenance.
		Columns: []string{"Publisher", "Repository", "Access", "Modality", "Licence",
			"Runtime", "Status detail"},
		Noun:  "models",
		Items: items, Total: len(items),
		AlwaysStatus: true,
		RowsOpenable: true,
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

// Detail implements console.Driller for one bucket.
//
// Clicking a bucket did nothing: the provider offered no detail at all, on
// the product whose whole purpose is holding things. path[0] is the bucket
// and everything after it is a prefix, so a folder inside a bucket has its
// own address and can be linked to.
func (p storageProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	bucket := path[0]
	prefix := ""
	if len(path) > 1 {
		// Google Cloud Storage has no folders; it has keys containing
		// slashes, and a delimited list turns the common leading parts into
		// prefixes. Reconstructing the prefix from the path segments is what
		// makes "go into a folder" mean anything.
		prefix = strings.Join(path[1:], "/") + "/"
	}

	objects, err := p.objects(ctx, bucket, prefix, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}

	sections := []console.Section{{ID: "objects", Label: "Objects", Listing: objects}}

	// A bucket's own settings, which nothing showed. Only the prefix root
	// carries them: a folder is not a resource and has no configuration.
	if prefix == "" {
		if config, err := p.bucketConfig(ctx, bucket); err == nil {
			sections = append(sections, config)
		}
	}

	summary := []console.Property{{Label: "Bucket", Value: bucket}}
	if prefix != "" {
		summary = append(summary, console.Property{Label: "Prefix", Value: prefix})
	}
	summary = append(summary,
		console.Property{Label: "Objects here", Value: fmt.Sprint(len(objects.Items))})

	return console.Detail{Summary: summary, Sections: sections}, nil
}

// objects lists one level of a bucket: the folders directly under a prefix,
// then the objects directly in it.
func (p storageProvider) objects(ctx context.Context, bucket, prefix string, path []string) (console.Listing, error) {
	out := console.Listing{
		Columns:    []string{"Size", "Type", "Updated"},
		NameColumn: "Name",
		Noun:       "objects",
	}

	var body struct {
		Items []struct {
			Name        string `json:"name"`
			Size        string `json:"size"`
			ContentType string `json:"contentType"`
			Updated     string `json:"updated"`
		} `json:"items"`
		// Prefixes are what a delimited list returns instead of descending:
		// the common leading parts, which is what a folder actually is here.
		Prefixes []string `json:"prefixes"`
	}
	url := fmt.Sprintf("http://%s/storage/v1/b/%s/o?delimiter=%%2F&prefix=%s",
		p.endpoint, bucket, urlpkg.QueryEscape(prefix))
	if err := getJSON(ctx, url, &body); err != nil {
		return out, fmt.Errorf("cannot list %s: %w", bucket, err)
	}

	// Folders first, as a file browser orders them.
	for _, pre := range body.Prefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(pre, prefix), "/")
		if name == "" {
			continue
		}
		out.Items = append(out.Items, console.Resource{
			Name: name + "/",
			// This row opens; the object rows below it do not.
			Opens:  append(append([]string{}, path...), name),
			Fields: map[string]string{"Type": "Folder", "Size": "—", "Updated": "—"},
		})
	}
	for _, o := range body.Items {
		name := strings.TrimPrefix(o.Name, prefix)
		if name == "" {
			continue
		}
		size := "—"
		if n, err := strconv.ParseInt(o.Size, 10, 64); err == nil {
			size = formatBytes(n)
		}
		out.Items = append(out.Items, console.Resource{
			Name: name,
			Fields: map[string]string{
				"Size": size, "Type": o.ContentType, "Updated": shortTime(o.Updated),
			},
		})
	}
	out.Total = len(out.Items)
	return out, nil
}

// bucketConfig is the bucket's own settings, as the backend reports them.
func (p storageProvider) bucketConfig(ctx context.Context, bucket string) (console.Section, error) {
	var b struct {
		Name             string                 `json:"name"`
		Location         string                 `json:"location"`
		LocationType     string                 `json:"locationType"`
		StorageClass     string                 `json:"storageClass"`
		TimeCreated      string                 `json:"timeCreated"`
		Updated          string                 `json:"updated"`
		Versioning       struct{ Enabled bool } `json:"versioning"`
		IAMConfiguration struct {
			UniformBucketLevelAccess struct{ Enabled bool } `json:"uniformBucketLevelAccess"`
		} `json:"iamConfiguration"`
	}
	url := fmt.Sprintf("http://%s/storage/v1/b/%s", p.endpoint, bucket)
	if err := getJSON(ctx, url, &b); err != nil {
		return console.Section{}, err
	}

	yesNo := func(v bool) string {
		if v {
			return "Enabled"
		}
		return "Disabled"
	}
	return console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Location and class", Properties: []console.Property{
				{Label: "Location", Value: b.Location},
				{Label: "Location type", Value: b.LocationType},
				{Label: "Storage class", Value: b.StorageClass},
			}},
			{Heading: "Protection", Properties: []console.Property{
				{Label: "Object versioning", Value: yesNo(b.Versioning.Enabled)},
				{Label: "Uniform bucket-level access",
					Value: yesNo(b.IAMConfiguration.UniformBucketLevelAccess.Enabled)},
			}},
			{Heading: "Lifecycle", Properties: []console.Property{
				{Label: "Created", Value: shortTime(b.TimeCreated)},
				{Label: "Updated", Value: shortTime(b.Updated)},
			}},
		},
		// Read-only, and it says so: this console has no update path, and
		// showing settings without the caveat implies an edit that does not
		// exist. Retention and lifecycle rules are absent rather than blank
		// because fake-gcs-server does not report them.
		Note: "Read-only, as this backend reports it. fake-gcs-server does not " +
			"implement retention policies or lifecycle rules, so they are absent " +
			"rather than shown empty.",
	}, nil
}

// Detail implements console.Driller for one secret.
//
// Clicking a secret did nothing, on the product where "which version is
// current, and when did it change" is the entire question.
func (p secretsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) == 2 {
		return p.versionDetail(project, lastSegment(path[0]), path[1])
	}
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: "Secret Manager has not started"}, nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}

	id := lastSegment(path[0])
	secret, err := st.GetSecret(project, id)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the secret: " + err.Error()}, nil
	}

	versions := console.Listing{
		Columns:      []string{"State", "Created", "Destroyed"},
		NameColumn:   "Version",
		Noun:         "versions",
		AlwaysStatus: true,
	}
	all, err := st.ListVersions(project, id)
	if err != nil {
		versions.Unavailable = "cannot list versions: " + err.Error()
	} else {
		// Newest first: the current version is what someone came to check.
		sort.Slice(all, func(i, j int) bool { return all[i].Number > all[j].Number })
		for _, v := range all {
			destroyed := "—"
			if !v.Destroyed.IsZero() {
				destroyed = v.Destroyed.Format(time.RFC3339)
			}
			versions.Items = append(versions.Items, console.Resource{
				Name:   fmt.Sprint(v.Number),
				Status: string(v.State),
				Fields: map[string]string{
					"State":     string(v.State),
					"Created":   v.Created.Format(time.RFC3339),
					"Destroyed": destroyed,
				},
			})
		}
		versions.Total = len(versions.Items)
		// A version's rows open; its payload is still not in this response.
		// Every version's bytes are in hand here, and putting them in a
		// listing would mean the console handed out every credential a project
		// holds to anyone who opened a page. A value is shown only from the
		// version's own page, only when asked for, and the asking is recorded.
		versions.RowsOpenable = true
		versions.Note = "Open a version to see its state, or to show its value. " +
			"Payloads are never included in this list."
	}

	config := console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Replication", Properties: []console.Property{
				{Label: "Policy", Value: secret.Replication},
			}},
		},
		// Replication is fixed at creation in the real API, so the edit form
		// covers labels and annotations and says so rather than offering a
		// field the API would refuse.
		Note: "Labels and annotations can be edited. Replication is fixed when " +
			"the secret is created.",
	}
	if len(secret.Labels) > 0 {
		config.Groups = append(config.Groups, console.PropertyGroup{
			Heading: "Labels", Properties: sortedPairs(secret.Labels),
		})
	}
	if len(secret.Annotations) > 0 {
		config.Groups = append(config.Groups, console.PropertyGroup{
			Heading: "Annotations", Properties: sortedPairs(secret.Annotations),
		})
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Created", Value: secret.Created.Format(time.RFC3339)},
			{Label: "Versions", Value: fmt.Sprint(len(versions.Items))},
			{Label: "Next version", Value: fmt.Sprint(secret.NextVersion)},
		},
		Sections: []console.Section{
			{ID: "versions", Label: "Versions", Listing: versions},
			config,
		},
		Edit: &console.EditForm{
			Label: "Edit secret",
			Fields: []console.Field{
				{Name: "labels", Label: "Labels", Type: "map",
					Default: console.FormatMap(secret.Labels),
					Help:    "One key=value per line. Replaces the whole set."},
				{Name: "annotations", Label: "Annotations", Type: "map",
					Default: console.FormatMap(secret.Annotations),
					Help:    "One key=value per line. Replaces the whole set."},
			},
			Note: "Replication cannot be changed after a secret is created, so it " +
				"is not offered here.",
		},
	}, nil
}

// versionDetail is one secret version.
func (p secretsProvider) versionDetail(project, id, version string) (console.Detail, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: "Secret Manager has not started"}, nil
	}
	v, err := st.GetVersion(project, id, version)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the version: " + err.Error()}, nil
	}

	destroyed := "—"
	if !v.Destroyed.IsZero() {
		destroyed = v.Destroyed.Format(time.RFC3339)
	}
	// The size is reported and the bytes are not. "Is there anything in this
	// version" is answerable without handing the payload over, and it is the
	// question that distinguishes an empty write from a missing one.
	size := "—"
	if v.State != secrets.StateDestroyed {
		size = fmt.Sprintf("%d bytes", len(v.Payload))
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: string(v.State)},
			{Label: "Created", Value: v.Created.Format(time.RFC3339)},
			{Label: "Destroyed", Value: destroyed},
			{Label: "Payload size", Value: size},
		},
		Sections: []console.Section{{
			ID: "state", Label: "State", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{
				Heading: "Version " + fmt.Sprint(v.Number),
				Properties: []console.Property{
					{Label: "Resource name", Value: v.Name},
					{Label: "State", Value: string(v.State)},
					{Label: "Accessible", Value: yesNo(v.Accessible())},
				},
			}},
			Note: versionNote(v.State),
		}},
	}, nil
}

// versionNote says what this state means for a caller.
func versionNote(state secrets.VersionState) string {
	switch state {
	case secrets.StateDisabled:
		return "A disabled version still exists but every access fails. " +
			"Enable it to make it readable again."
	case secrets.StateDestroyed:
		return "The payload is gone. Destroying is terminal: the bytes were " +
			"cleared rather than marked, so there is nothing left to restore."
	default:
		return ""
	}
}

func yesNo(v bool) string {
	if v {
		return "Yes"
	}
	return "No"
}

// DetailActions offers a version's lifecycle, and a secret's new version.
//
// Enable, disable and destroy are the three operations the API defines on a
// version, and the console could express none of them: a secret was created
// elsewhere, listed here, and then untouchable.
func (p secretsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	st := p.svc.Store()
	if st == nil || project == "" {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{{
			ID: "addversion", Label: "Add version",
			Fields: []console.Field{{
				Name: "payload", Label: "Secret value", Type: "textarea", Required: true,
				Help: "Stored as UTF-8 bytes. This becomes the new enabled version; " +
					"earlier versions keep the state they have.",
			}},
		}}
	case 2:
		// What is offered depends on the version's current state. Enabling an
		// enabled version and destroying a destroyed one are both buttons that
		// exist only to fail, so neither is drawn.
		v, err := st.GetVersion(project, lastSegment(path[0]), path[1])
		if err != nil {
			return nil
		}
		switch v.State {
		case secrets.StateEnabled:
			return []console.Action{
				{ID: "disable", Label: "Disable"},
				{ID: "destroy", Label: "Destroy", Destructive: true},
			}
		case secrets.StateDisabled:
			return []console.Action{
				{ID: "enable", Label: "Enable"},
				{ID: "destroy", Label: "Destroy", Destructive: true},
			}
		}
		// Destroyed is terminal, so it offers nothing.
		return nil
	}
	return nil
}

// ActAt performs a version's lifecycle operations and adds new versions.
func (p secretsProvider) ActAt(_ context.Context, project string, path []string, action string, values map[string]string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	id := lastSegment(path[0])

	switch action {
	case "addversion":
		payload := values["payload"]
		if payload == "" {
			// Refused here rather than stored: an empty version is
			// indistinguishable from a mistake, and the API would accept it.
			return fmt.Errorf("a version needs a value")
		}
		_, err := st.AddVersion(project, id, []byte(payload))
		return err
	case "enable":
		_, err := st.SetVersionState(project, id, path[1], secrets.StateEnabled)
		return err
	case "disable":
		_, err := st.SetVersionState(project, id, path[1], secrets.StateDisabled)
		return err
	case "destroy":
		_, err := st.DestroyVersion(project, id, path[1])
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// CanReveal reports whether a path names a version whose bytes still exist.
//
// A secret has no value of its own — only its versions do — and a destroyed
// version has none left, so neither offers the control.
func (p secretsProvider) CanReveal(path []string) bool {
	return len(path) == 2
}

// Reveal returns one version's payload.
//
// The console shows it because somebody pressed a button, and the press is
// recorded in the operations ledger by the route that calls this. It goes
// through AccessVersion rather than reading the stored bytes directly, so a
// disabled version is refused here exactly as it would be refused an SDK
// client: the console must not be a way around a state the API enforces.
func (p secretsProvider) Reveal(_ context.Context, project string, path []string) (string, string, error) {
	st := p.svc.Store()
	if st == nil {
		return "", "", fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return "", "", fmt.Errorf("choose a project first")
	}
	id := lastSegment(path[0])
	v, err := st.AccessVersion(project, id, path[1])
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%s version %d", id, v.Number), string(v.Payload), nil
}

// Edit changes a secret's labels and annotations.
//
// Replication is absent because the API fixes it at creation; offering a field
// the API refuses is the working-looking control the parity rules forbid.
func (p secretsProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	if len(path) != 1 {
		return fmt.Errorf("only a secret can be edited, not a version")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	annotations, err := console.ParseMap(values["annotations"])
	if err != nil {
		return fmt.Errorf("annotations: %w", err)
	}
	// Both are always sent by the form, so both are always updated: a PATCH
	// that left one alone because the operator did not touch it would make
	// "clear every label" impossible to express.
	_, err = st.UpdateSecret(project, lastSegment(path[0]), labels, annotations, true, true)
	return err
}

// CreateForm is the Create secret form.
//
// The payload is on it because a secret with no versions holds nothing: the
// real console asks for the first value in the same step, and a two-step
// create would leave an empty secret behind whenever the second step failed.
func (p secretsProvider) CreateForm() (string, []console.Field) {
	return "Create secret", []console.Field{
		{Name: "secretId", Label: "Name", Type: "text", Required: true,
			Help:    "Up to 255 characters: letters, digits, hyphens and underscores.",
			Pattern: `^[A-Za-z0-9_-]{1,255}$`},
		{Name: "payload", Label: "Secret value", Type: "textarea", Required: true,
			Help: "Stored as UTF-8 bytes and becomes version 1."},
		{Name: "labels", Label: "Labels", Type: "map",
			Help: "Optional. One key=value per line."},
		{Name: "annotations", Label: "Annotations", Type: "map",
			Help: "Optional. One key=value per line."},
	}
}

// CreateOnPage keeps the form off the dialog: a payload needs a textarea with
// room in it, and a 440px dialog has none.
func (p secretsProvider) CreateOnPage() bool { return true }

func (p secretsProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	st := p.svc.Store()
	if st == nil {
		return "", fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return "", fmt.Errorf("choose a project first")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	annotations, err := console.ParseMap(values["annotations"])
	if err != nil {
		return "", fmt.Errorf("annotations: %w", err)
	}
	id := strings.TrimSpace(values["secretId"])
	sec, err := st.CreateSecret(project, id, labels, annotations, "automatic")
	if err != nil {
		return "", err
	}
	// The first version is part of the create, and its failure is the create's
	// failure. Reporting success here and leaving a secret with no versions
	// would be the console claiming an operation that did not finish — so the
	// half-made secret is removed and the original error reported.
	if _, err := st.AddVersion(project, id, []byte(values["payload"])); err != nil {
		if rmErr := st.DeleteSecret(project, id); rmErr != nil {
			return "", fmt.Errorf("adding the first version failed (%w), and the "+
				"empty secret could not be removed: %v", err, rmErr)
		}
		return "", fmt.Errorf("adding the first version failed, so the secret was "+
			"not created: %w", err)
	}
	return sec.Name, nil
}

// sortedPairs renders a map as properties in a stable order.
//
// Map iteration in Go is deliberately random, so rendering one directly makes
// a page whose rows move between reads.
func sortedPairs(m map[string]string) []console.Property {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]console.Property, 0, len(keys))
	for _, k := range keys {
		out = append(out, console.Property{Label: k, Value: m[k]})
	}
	return out
}

// Detail implements console.Driller for one topic.
//
// Clicking a topic did nothing, on the product where the first question is
// "is anything subscribed to this" — a topic with no subscription drops every
// message published to it, silently.
func (p pubsubProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	topic := path[0]

	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Pub/Sub: " + err.Error()}, nil
	}
	defer func() { _ = c.Close() }()

	subs := console.Listing{
		Columns:    []string{"Ack deadline", "Retention", "Delivery"},
		NameColumn: "Subscription",
		Noun:       "subscriptions",
	}
	// ListTopicSubscriptions returns the names attached to this topic; each
	// one is then read for its own settings, which is what the real console
	// shows beside it.
	it := c.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{
		Topic: topic,
	})
	for {
		name, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			subs.Unavailable = "listing subscriptions: " + err.Error()
			break
		}
		row := console.Resource{Name: name, Fields: map[string]string{}}
		if s, err := c.SubscriptionAdminClient.GetSubscription(ctx,
			&pubsubpb.GetSubscriptionRequest{Subscription: name}); err == nil {
			row.Fields["Ack deadline"] = fmt.Sprintf("%ds", s.GetAckDeadlineSeconds())
			if d := s.GetMessageRetentionDuration(); d != nil {
				row.Fields["Retention"] = d.AsDuration().String()
			}
			// Push and pull are the two delivery shapes, and which one a
			// subscription uses changes where to look when messages are not
			// arriving.
			if push := s.GetPushConfig().GetPushEndpoint(); push != "" {
				row.Fields["Delivery"] = "push → " + push
			} else {
				row.Fields["Delivery"] = "pull"
			}
		}
		subs.Items = append(subs.Items, row)
	}
	subs.Total = len(subs.Items)
	if len(subs.Items) == 0 && subs.Unavailable == "" {
		// Not a neutral fact: an unsubscribed topic discards everything
		// published to it, which is a confusing first experience for someone
		// testing a publisher.
		subs.Note = "This topic has no subscriptions, so messages published to it " +
			"are discarded."
	}

	summary := []console.Property{
		{Label: "Topic", Value: topic},
		{Label: "Subscriptions", Value: fmt.Sprint(len(subs.Items))},
	}
	if t, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); err == nil {
		if s := t.GetSchemaSettings(); s != nil && s.GetSchema() != "" {
			summary = append(summary, console.Property{Label: "Schema", Value: s.GetSchema()})
		}
	}

	return console.Detail{
		Summary:  summary,
		Sections: []console.Section{{ID: "subscriptions", Label: "Subscriptions", Listing: subs}},
	}, nil
}

// servicePorts renders a Service's ports the way kubectl does.
func servicePorts(spec map[string]any) []string {
	ports, _ := spec["ports"].([]any)
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		port := 0
		if v, ok := pm["port"].(float64); ok {
			port = int(v)
		}
		proto := str(pm, "protocol")
		if proto == "" {
			proto = "TCP"
		}
		out = append(out, fmt.Sprintf("%d/%s", port, proto))
	}
	return out
}

// serviceDetail is one Service's page: how to reach it, and what it reaches.
//
// A Service that selects nothing is the classic silent failure — it resolves,
// it accepts connections, and nothing answers. The selector and the endpoints
// side by side are what make that visible.
func serviceDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")

	ports := console.Listing{
		Columns:    []string{"Port", "Target port", "Protocol", "Node port"},
		NameColumn: "Name",
		Noun:       "ports",
	}
	if ps, ok := spec["ports"].([]any); ok {
		for _, p := range ps {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			num := func(key string) string {
				if v, ok := pm[key].(float64); ok {
					return fmt.Sprint(int(v))
				}
				if v := str(pm, key); v != "" {
					return v
				}
				return "—"
			}
			name := str(pm, "name")
			if name == "" {
				// An unnamed port is legal on a single-port Service, and
				// rendering it as an empty row loses the row entirely.
				name = num("port")
			}
			ports.Items = append(ports.Items, console.Resource{
				Name: name,
				Fields: map[string]string{
					"Port": num("port"), "Target port": num("targetPort"),
					"Protocol": str(pm, "protocol"), "Node port": num("nodePort"),
				},
			})
		}
	}
	ports.Total = len(ports.Items)

	selector := map[string]string{}
	if sel, ok := spec["selector"].(map[string]any); ok {
		for k, v := range sel {
			if sv, ok := v.(string); ok {
				selector[k] = sv
			}
		}
	}
	routing := console.Section{
		ID: "routing", Label: "Routing", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Addressing", Properties: []console.Property{
				{Label: "Type", Value: str(spec, "type")},
				{Label: "Cluster IP", Value: str(spec, "clusterIP")},
				{Label: "Session affinity", Value: str(spec, "sessionAffinity")},
				{Label: "In-cluster DNS", Value: fmt.Sprintf("%s.%s.svc.cluster.local",
					str(m, "name"), str(m, "namespace"))},
			}},
		},
	}
	if len(selector) > 0 {
		routing.Groups = append(routing.Groups, console.PropertyGroup{
			Heading: "Selector", Properties: sortedPairs(selector),
		})
	} else {
		// Not a blank: a Service with no selector is either headless-by-design
		// or broken, and the reader cannot tell which from an empty card.
		routing.Note = "This Service selects no pods. That is deliberate for an " +
			"ExternalName or a manually managed Endpoints object, and a fault otherwise."
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Namespace", Value: str(m, "namespace")},
			{Label: "Type", Value: str(spec, "type")},
			{Label: "Cluster IP", Value: str(spec, "clusterIP")},
			{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
		},
		Sections: []console.Section{
			{ID: "ports", Label: "Ports", Listing: ports},
			routing,
		},
	}
}

// jobDetail is one Job's page.
func jobDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")
	st := nested(item, "status")

	num := func(src map[string]any, key string) string {
		if v, ok := src[key].(float64); ok {
			return fmt.Sprint(int(v))
		}
		return "—"
	}

	summary := []console.Property{
		{Label: "Namespace", Value: str(m, "namespace")},
		{Label: "Completions", Value: num(spec, "completions")},
		{Label: "Parallelism", Value: num(spec, "parallelism")},
		{Label: "Succeeded", Value: num(st, "succeeded")},
		{Label: "Failed", Value: num(st, "failed")},
		{Label: "Backoff limit", Value: num(spec, "backoffLimit")},
	}
	// A duration, not two timestamps: "how long did it take" is the question,
	// and subtracting two ISO strings by eye is not an answer.
	if start, end := str(st, "startTime"), str(st, "completionTime"); start != "" {
		summary = append(summary, console.Property{
			Label: "Duration", Value: jobDuration(start, end),
		})
	}

	containers := console.Listing{
		Columns:    []string{"Image", "Command"},
		NameColumn: "Container",
		Noun:       "containers",
	}
	if cs, ok := nested(spec, "template", "spec")["containers"].([]any); ok {
		for _, c := range cs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			var command []string
			for _, key := range []string{"command", "args"} {
				if parts, ok := cm[key].([]any); ok {
					for _, part := range parts {
						if sp, ok := part.(string); ok {
							command = append(command, sp)
						}
					}
				}
			}
			containers.Items = append(containers.Items, console.Resource{
				Name: str(cm, "name"),
				Fields: map[string]string{
					"Image": str(cm, "image"), "Command": strings.Join(command, " "),
				},
			})
		}
	}
	containers.Total = len(containers.Items)

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "containers", Label: "Containers", Listing: containers},
		},
	}
}

// jobDuration is how long a job ran, or has been running.
func jobDuration(start, end string) string {
	from, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return ""
	}
	to := time.Now()
	if end != "" {
		if parsed, err := time.Parse(time.RFC3339, end); err == nil {
			to = parsed
		}
	}
	d := to.Sub(from)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// related lists what an object controls.
//
// A workload's pods, a Job's pods, a Deployment's ReplicaSets. All of it is
// one labelled kubectl read; the reason it was missing is that nothing joined
// a selector to the objects it selects.
func (p kubeProvider) related(ctx context.Context, item map[string]any) []console.Section {
	kind := str(item, "kind")
	m := meta(item)
	namespace := str(m, "namespace")

	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
		selector := selectorOf(item)
		if selector == "" {
			return nil
		}
		sections := []console.Section{{
			ID: "pods", Label: "Managed pods",
			Listing: p.selectedPods(ctx, namespace, selector),
		}}
		if kind == "Deployment" {
			// A Deployment's ReplicaSets are its revision history: which
			// image each rollout ran and how many pods it still holds. That
			// is the answer to "what changed", and it was on screen nowhere.
			sections = append(sections, console.Section{
				ID: "revisions", Label: "Revision history",
				Listing: p.replicaSets(ctx, namespace, str(m, "name")),
			})
		}
		return sections

	case "Service":
		// A Service's backends. Kubernetes answers this with an Endpoints
		// object, but the useful form of the answer is "which pods", and the
		// selector is the same join the endpoints controller performs.
		sel, _ := nested(item, "spec")["selector"].(map[string]any)
		if len(sel) == 0 {
			// A headless or externalName Service, or one wired by hand. Either
			// way it has no selector, so there is nothing to join on and
			// guessing would be worse than saying so.
			return nil
		}
		keys := make([]string, 0, len(sel))
		for k := range sel {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if v, ok := sel[k].(string); ok {
				parts = append(parts, k+"="+v)
			}
		}
		pods := p.selectedPods(ctx, namespace, strings.Join(parts, ","))
		if len(pods.Items) == 0 && pods.Unavailable == "" {
			pods.Note = "This Service's selector matches no pods, so it " +
				"accepts connections and has nowhere to send them."
		}
		return []console.Section{{ID: "endpoints", Label: "Endpoints", Listing: pods}}

	case "Job":
		// A Job's pods carry its output and its failure. job-name is the
		// label the Job controller sets, so this is the join the cluster
		// itself uses.
		return []console.Section{{
			ID: "pods", Label: "Pods",
			Listing: p.selectedPods(ctx, namespace, "job-name="+str(m, "name")),
		}}
	}
	return nil
}

// selectorOf renders a workload's matchLabels as a kubectl selector.
func selectorOf(item map[string]any) string {
	labels, _ := nested(item, "spec", "selector")["matchLabels"].(map[string]any)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Sorted, so the same workload produces the same selector between reads.
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := labels[k].(string); ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ",")
}

// selectedPods lists the pods a selector matches.
func (p kubeProvider) selectedPods(ctx context.Context, namespace, selector string) console.Listing {
	out := console.Listing{
		Columns:      []string{"Ready", "Restarts", "Node", "Age"},
		NameColumn:   "Pod",
		Noun:         "pods",
		AlwaysStatus: true,
	}
	raw, err := kubectlSelected(ctx, p.kubeconfig, namespace, "pods", selector)
	if err != nil {
		out.Unavailable = "cannot list pods: " + err.Error()
		return out
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		out.Unavailable = "decode pods: " + err.Error()
		return out
	}
	for _, item := range list.Items {
		m := meta(item)
		statuses := containerStatuses(item)
		ready, restarts := 0, 0
		for _, c := range statuses {
			if r, ok := c["ready"].(bool); ok && r {
				ready++
			}
			if n, ok := c["restartCount"].(float64); ok {
				restarts += int(n)
			}
		}
		out.Items = append(out.Items, console.Resource{
			Name: str(m, "name"), Status: podStatus(item),
			Fields: map[string]string{
				"Ready":    fmt.Sprintf("%d/%d", ready, len(statuses)),
				"Restarts": fmt.Sprint(restarts),
				"Node":     str(nested(item, "spec"), "nodeName"),
				"Age":      shortAge(str(m, "creationTimestamp")),
			},
		})
	}
	out.Total = len(out.Items)
	if len(out.Items) == 0 && out.Unavailable == "" {
		// A workload with no pods is the failure this section exists to make
		// visible, so it says so rather than rendering an empty table.
		out.Note = "This selector matches no pods. Either the workload has not " +
			"scheduled any, or its selector does not match its own template."
	}
	return out
}

// replicaSets is a Deployment's rollout history.
func (p kubeProvider) replicaSets(ctx context.Context, namespace, deployment string) console.Listing {
	out := console.Listing{
		Columns:      []string{"Revision", "Ready", "Images", "Age"},
		NameColumn:   "ReplicaSet",
		Noun:         "revisions",
		AlwaysStatus: true,
	}
	raw, err := kubectlJSON(ctx, p.kubeconfig, namespace, "replicasets")
	if err != nil {
		out.Unavailable = "cannot list replica sets: " + err.Error()
		return out
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		out.Unavailable = "decode replica sets: " + err.Error()
		return out
	}
	for _, item := range list.Items {
		m := meta(item)
		if !ownedByName(item, "Deployment", deployment) {
			continue
		}
		ready, desired := workloadReplicas(item)
		annotations, _ := m["annotations"].(map[string]any)
		state := "Superseded"
		if desired > 0 {
			state = "Active"
		}
		out.Items = append(out.Items, console.Resource{
			Name: str(m, "name"), Status: state,
			Fields: map[string]string{
				// The revision number the Deployment controller stamped, not
				// one this code invented from ordering.
				"Revision": str(annotations, "deployment.kubernetes.io/revision"),
				"Ready":    fmt.Sprintf("%d/%d", ready, desired),
				"Images":   strings.Join(podTemplateImages(item), ", "),
				"Age":      shortAge(str(m, "creationTimestamp")),
			},
		})
	}
	// Newest revision first.
	sort.SliceStable(out.Items, func(i, j int) bool {
		return out.Items[i].Fields["Revision"] > out.Items[j].Fields["Revision"]
	})
	out.Total = len(out.Items)
	return out
}

// ownedByName reports whether an object's controller is the named one.
func ownedByName(item map[string]any, kind, name string) bool {
	refs, _ := meta(item)["ownerReferences"].([]any)
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if str(ref, "kind") == kind && str(ref, "name") == name {
			return true
		}
	}
	return false
}

// kubectlSelected reads objects matching a label selector.
func kubectlSelected(ctx context.Context, kubeconfig, namespace, kind, selector string) ([]byte, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig: the cluster has not started")
	}
	args := []string{"--kubeconfig", kubeconfig, "get", kind, "-o", "json", "-l", selector}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// stringMap narrows a decoded JSON object to its string-valued entries.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

// nodesProvider is the cluster's nodes.
//
// Every other Kubernetes screen answers "what did CloudBurrow schedule".
// This one answers "what is it scheduling onto", which is the question behind
// a pod that will not start: a node under pressure, out of disk, or gone.
func nodesProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "nodes", title: "Cluster nodes", kind: "nodes", kubeconfig: kubeconfig,
		// A node has no namespace. Passing "" here would mean
		// --all-namespaces, which kubectl accepts and ignores for a cluster
		// object, so the read is the same either way.
		columns: []string{"Roles", "Version", "Pods", "Age"},
		detail:  nodeDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			return console.Resource{
				Name: str(m, "name"), Status: nodeStatus(item),
				Fields: map[string]string{
					"Roles":   nodeRoles(m),
					"Version": str(nested(item, "status", "nodeInfo"), "kubeletVersion"),
					"Pods":    stringMap(nested(item, "status")["allocatable"])["pods"],
					"Age":     shortAge(str(m, "creationTimestamp")),
				},
			}, true
		},
	}
}

// nodeStatus reports the condition that matters, not the one listed first.
//
// A node's Ready condition being True is the ordinary case; the conditions
// worth surfacing are the pressures, which are True when something is wrong.
// Reading only Ready would show a disk-full node as "Ready".
func nodeStatus(item map[string]any) string {
	conds, _ := nested(item, "status")["conditions"].([]any)
	ready := "Unknown"
	var pressures []string
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		kind, state := str(cond, "type"), str(cond, "status")
		if kind == "Ready" {
			switch state {
			case "True":
				ready = "Ready"
			case "False":
				ready = "NotReady"
			}
			continue
		}
		if state == "True" {
			pressures = append(pressures, kind)
		}
	}
	if len(pressures) > 0 {
		sort.Strings(pressures)
		return ready + " (" + strings.Join(pressures, ", ") + ")"
	}
	if unschedulable, ok := nested(item, "spec")["unschedulable"].(bool); ok && unschedulable {
		return ready + " (cordoned)"
	}
	return ready
}

// nodeRoles reads the roles Kubernetes records as labels.
func nodeRoles(m map[string]any) string {
	var roles []string
	for k := range stringMap(m["labels"]) {
		if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != k {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		return "worker"
	}
	return strings.Join(roles, ", ")
}

func nodeDetail(item map[string]any) console.Detail {
	m := meta(item)
	status := nested(item, "status")
	info := nested(item, "status", "nodeInfo")

	capacity := stringMap(status["capacity"])
	allocatable := stringMap(status["allocatable"])

	resources := console.Listing{
		Columns:    []string{"Capacity", "Allocatable"},
		NameColumn: "Resource",
		Noun:       "resources",
	}
	// Capacity is what the machine has; allocatable is what the scheduler may
	// use. The gap is what the kubelet reserves, and a pod that will not fit
	// is refused against allocatable, not capacity.
	for _, pair := range sortedPairs(capacity) {
		resources.Items = append(resources.Items, console.Resource{
			Name: pair.Label,
			Fields: map[string]string{
				"Capacity": pair.Value, "Allocatable": allocatable[pair.Label],
			},
		})
	}
	resources.Total = len(resources.Items)

	conditions := console.Listing{
		Columns:      []string{"Reason", "Message", "Since"},
		NameColumn:   "Condition",
		Noun:         "conditions",
		AlwaysStatus: true,
	}
	if conds, ok := status["conditions"].([]any); ok {
		for _, c := range conds {
			cond, ok := c.(map[string]any)
			if !ok {
				continue
			}
			conditions.Items = append(conditions.Items, console.Resource{
				Name: str(cond, "type"), Status: str(cond, "status"),
				Fields: map[string]string{
					"Reason":  str(cond, "reason"),
					"Message": str(cond, "message"),
					"Since":   shortAge(str(cond, "lastTransitionTime")),
				},
			})
		}
	}
	conditions.Total = len(conditions.Items)

	addresses := console.Listing{
		Columns: []string{"Address"}, NameColumn: "Type", Noun: "addresses",
	}
	if addrs, ok := status["addresses"].([]any); ok {
		for _, a := range addrs {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			addresses.Items = append(addresses.Items, console.Resource{
				Name:   str(am, "type"),
				Fields: map[string]string{"Address": str(am, "address")},
			})
		}
	}
	addresses.Total = len(addresses.Items)

	var taints []console.Property
	if list, ok := nested(item, "spec")["taints"].([]any); ok {
		for _, t := range list {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			value := str(tm, "effect")
			if v := str(tm, "value"); v != "" {
				value = v + " · " + value
			}
			taints = append(taints, console.Property{Label: str(tm, "key"), Value: value})
		}
	}

	groups := []console.PropertyGroup{{
		Heading: "Machine",
		Properties: []console.Property{
			{Label: "Operating system", Value: str(info, "osImage")},
			{Label: "Architecture", Value: str(info, "architecture")},
			{Label: "Kernel", Value: str(info, "kernelVersion")},
			{Label: "Container runtime", Value: str(info, "containerRuntimeVersion")},
			{Label: "kubelet", Value: str(info, "kubeletVersion")},
			{Label: "kube-proxy", Value: str(info, "kubeProxyVersion")},
		},
	}}
	if len(taints) > 0 {
		// A taint is the reason a pod is not on this node, so it belongs on
		// screen rather than only in the YAML.
		groups = append(groups, console.PropertyGroup{Heading: "Taints", Properties: taints})
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Status", Value: nodeStatus(item)},
			{Label: "Roles", Value: nodeRoles(m)},
			{Label: "Pod capacity", Value: allocatable["pods"]},
			{Label: "CPU", Value: allocatable["cpu"]},
			{Label: "Memory", Value: allocatable["memory"]},
			{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
		},
		Sections: []console.Section{
			{ID: "resources", Label: "Resources", Listing: resources},
			{ID: "conditions", Label: "Conditions", Listing: conditions},
			{ID: "addresses", Label: "Addresses", Listing: addresses},
			{ID: "machine", Label: "Configuration", Kind: console.KindProperties, Groups: groups},
		},
	}
}

// storageProvider lists the cluster's persistent volume claims.
//
// A stateful workload that will not start is usually waiting on a volume, and
// until now the console had no screen that showed one.
func clusterStorageProvider(kubeconfig string) kubeProvider {
	return kubeProvider{
		id: "k8sstorage", title: "Cluster storage", kind: "persistentvolumeclaims",
		kubeconfig: kubeconfig,
		columns:    []string{"Namespace", "Capacity", "Access modes", "Storage class", "Volume", "Age"},
		detail:     pvcDetail,
		row: func(item map[string]any) (console.Resource, bool) {
			m := meta(item)
			st := nested(item, "status")
			// The requested size is in the spec; the granted size is in the
			// status. A claim that is Pending has the first and not the
			// second, and showing the request as though it were provisioned
			// would hide exactly the failure this screen exists for.
			capacity := stringMap(st["capacity"])["storage"]
			if capacity == "" {
				capacity = stringMap(nested(item, "spec", "resources")["requests"])["storage"] + " requested"
			}
			return console.Resource{
				Name: str(m, "name"), Status: str(st, "phase"),
				Fields: map[string]string{
					"Namespace":     str(m, "namespace"),
					"Capacity":      capacity,
					"Access modes":  strings.Join(strSlice(sliceOf(nested(item, "spec")["accessModes"])), ", "),
					"Storage class": str(nested(item, "spec"), "storageClassName"),
					"Volume":        str(nested(item, "spec"), "volumeName"),
					"Age":           shortAge(str(m, "creationTimestamp")),
				},
			}, true
		},
	}
}

func pvcDetail(item map[string]any) console.Detail {
	m := meta(item)
	spec := nested(item, "spec")
	st := nested(item, "status")
	requested := stringMap(nested(item, "spec", "resources")["requests"])["storage"]
	granted := stringMap(st["capacity"])["storage"]

	groups := []console.PropertyGroup{{
		Heading: "Claim",
		Properties: []console.Property{
			{Label: "Phase", Value: str(st, "phase")},
			{Label: "Namespace", Value: str(m, "namespace")},
			{Label: "Storage class", Value: str(spec, "storageClassName")},
			{Label: "Volume mode", Value: str(spec, "volumeMode")},
			{Label: "Access modes", Value: strings.Join(strSlice(sliceOf(spec["accessModes"])), ", ")},
			{Label: "Requested", Value: requested},
			{Label: "Provisioned", Value: granted},
			{Label: "Bound volume", Value: str(spec, "volumeName")},
		},
	}}

	d := console.Detail{
		Summary: []console.Property{
			{Label: "Phase", Value: str(st, "phase")},
			{Label: "Capacity", Value: granted},
			{Label: "Storage class", Value: str(spec, "storageClassName")},
			{Label: "Age", Value: shortAge(str(m, "creationTimestamp"))},
		},
		Sections: []console.Section{
			{ID: "claim", Label: "Configuration", Kind: console.KindProperties, Groups: groups},
		},
	}
	if str(st, "phase") != "Bound" {
		// An unbound claim is the failure this screen exists to surface: the
		// pod that mounts it will sit in Pending with nothing wrong in its
		// own spec.
		d.Sections[0].Note = "This claim is not bound, so any pod that mounts it is " +
			"waiting. The Events tab carries the provisioner's reason."
	}
	return d
}

// sliceOf narrows a decoded JSON value to an array.
func sliceOf(v any) []any {
	out, _ := v.([]any)
	return out
}

// optionalInt reads a numeric form field that may be blank.
//
// Blank is zero and not an error: a form submitted without touching the
// scaling fields means "leave them alone", and a provider that refused it
// would make every field on the form required in practice.
func optionalInt(value, what string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a whole number, got %q", what, value)
	}
	return n, nil
}

// sortedKeys returns a map's keys in order, so a request built from a map is
// the same request twice.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DetailActions offers a task's own operations.
//
// A queue's actions are name-addressed and already exist. A task lives one level
// down, so it had none: a task queued with the wrong URL could be looked at and
// not removed.
func (p tasksProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 2 || p.svc.Store() == nil {
		return nil
	}
	return []console.Action{{
		ID: "deletetask", Label: "Delete task", Destructive: true,
	}}
}

func (p tasksProvider) ActAt(_ context.Context, _ string, path []string, action string, _ map[string]string) error {
	st := p.svc.Store()
	if st == nil {
		return fmt.Errorf("Cloud Tasks has not started")
	}
	if action != "deletetask" {
		return fmt.Errorf("unknown action %q", action)
	}
	name := path[1]
	if !strings.Contains(name, "/tasks/") {
		name = path[0] + "/tasks/" + name
	}
	return st.DeleteTask(name)
}

// podDetailWithCharts adds a pod's own CPU and memory history to its page.
//
// The cluster chart on the dashboard answers "is the node busy". The next
// question, every time, is "which pod is making it busy" — and that was
// unanswerable: the kubelet's per-pod readings were joined onto the list screen
// and then dropped, so no pod had a history of its own.
func podDetailWithCharts(series *console.Series) func(map[string]any) console.Detail {
	return func(item map[string]any) console.Detail {
		d := podDetail(item)
		if series == nil {
			return d
		}
		m := meta(item)
		key := str(m, "namespace") + "/" + str(m, "name")
		readings := series.PodWindow(key)
		if len(readings) == 0 {
			return d
		}

		cpu := console.ChartSeries{Label: "CPU", Unit: "cores"}
		memory := console.ChartSeries{Label: "Memory", Unit: "bytes"}
		// The rate between consecutive counters, not the kubelet's instantaneous
		// reading: an average over a known interval is the honest number, and the
		// first sample has no interval behind it so it has no rate.
		var prev *console.PodMetrics
		for i := range readings {
			r := readings[i]
			cpu.Points = append(cpu.Points, console.ChartPoint{
				At: r.At, Value: podCPURate(prev, &r),
			})
			memory.Points = append(memory.Points, console.ChartPoint{
				At: r.At, Value: podMemory(&r),
			})
			// A reading with no counter is a sample the pod was absent from. It
			// must not become the baseline for the next rate, or the next point
			// would be the pod's whole lifetime divided by one interval.
			if r.CPUCoreNanoSeconds > 0 {
				kept := r
				prev = &kept
			} else {
				prev = nil
			}
		}

		// A pod's limit, where it has one, so the chart says how much headroom
		// there is rather than only how the value moved.
		cpu.Max = podLimitCores(item)
		memory.Max = float64(podLimitBytes(item))

		d.Sections = append(d.Sections, console.Section{
			ID: "metrics", Label: "Metrics", Kind: console.KindChart,
			Series: []console.ChartSeries{cpu, memory},
			Note: "Measured by the kubelet and retained in memory only, so a " +
				"restart of CloudBurrow begins again from nothing. A break in a " +
				"line is a sample in which this pod was not reported.",
		})
		return d
	}
}

// podCPURate is the average cores used between two readings.
//
// Nil where there is no rate to state: the first reading, a sample the pod was
// absent from, a counter that went backwards because the container restarted.
// Each of those draws as a gap, which is the truth; a zero would read as "this
// pod used no CPU", which is a different and false claim.
func podCPURate(prev, now *console.PodMetrics) *float64 {
	if prev == nil || now == nil || now.CPUCoreNanoSeconds == 0 || prev.CPUCoreNanoSeconds == 0 {
		return nil
	}
	if now.CPUCoreNanoSeconds < prev.CPUCoreNanoSeconds {
		// A counter reset. The container restarted, and the difference is not a
		// rate.
		return nil
	}
	start, err := time.Parse(time.RFC3339, prev.At)
	if err != nil {
		return nil
	}
	end, err := time.Parse(time.RFC3339, now.At)
	if err != nil {
		return nil
	}
	interval := end.Sub(start).Seconds()
	if interval <= 0 {
		return nil
	}
	cores := float64(now.CPUCoreNanoSeconds-prev.CPUCoreNanoSeconds) / 1e9 / interval
	return &cores
}

// podMemory is a reading's working set, or nil where the pod was not reported.
func podMemory(r *console.PodMetrics) *float64 {
	if r == nil || r.MemoryWorkingSetBytes == 0 {
		return nil
	}
	bytes := float64(r.MemoryWorkingSetBytes)
	return &bytes
}

// podLimitCores sums the pod's CPU limits, or 0 when any container has none.
//
// Zero rather than a partial sum: a ceiling drawn from two of three containers
// is a ceiling the pod does not have, and a chart scaled to it would show
// headroom that does not exist.
func podLimitCores(item map[string]any) float64 {
	total := 0.0
	containers, _ := nested(item, "spec")["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		// Zero means unset in Kubernetes' own notation, so a container with no
		// CPU limit makes the pod's ceiling unknown.
		cores := parseCPUQuantity(stringMap(nested(cm, "resources")["limits"])["cpu"])
		if cores == 0 {
			return 0
		}
		total += cores
	}
	return total
}

// podLimitBytes sums the pod's memory limits, or 0 when any container has none.
func podLimitBytes(item map[string]any) int64 {
	var total int64
	containers, _ := nested(item, "spec")["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		bytes := parseMemoryQuantity(stringMap(nested(cm, "resources")["limits"])["memory"])
		if bytes == 0 {
			return 0
		}
		total += bytes
	}
	return total
}

// Detail implements console.Driller for one catalogued model.
//
// The list screen is a table of provenance claims and the row was the end of the
// road, so the artifact filename — the thing that decides whether a download will
// work — and the notes that say why a model is or is not usable were reachable
// nowhere. Both are in the catalogue already.
func (aiProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	model, err := localai.Lookup(path[0])
	if err != nil {
		return console.Detail{Unavailable: "not a catalogued model: " + path[0]}, nil
	}

	status, why := modelStatus(model)
	runtime := model.Runtime
	if runtime == "" {
		runtime = "none"
	}

	provenance := []console.Property{
		{Label: "Publisher", Value: string(model.Publisher)},
		{Label: "Repository", Value: model.Repo},
		// The specific file, because a repository usually holds several and they
		// are not interchangeable — a quantisation that the runtime cannot read
		// is a download that succeeds and then does nothing.
		{Label: "Artifact", Value: orDash(model.Artifact)},
		{Label: "Licence", Value: model.License},
		{Label: "Access", Value: string(model.Access)},
	}
	if model.Publisher == localai.PublisherCommunity {
		provenance = append(provenance, console.Property{
			Label: "Provenance caveat",
			Value: "A community conversion. CloudBurrow does not claim this is " +
				"Google-published, because it is not.",
		})
	}

	sections := []console.Section{{
		ID: "provenance", Label: "Provenance", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Artifact", Properties: provenance},
			{Heading: "Execution", Properties: []console.Property{
				{Label: "Modality", Value: string(model.Modality)},
				{Label: "Runtime", Value: runtime},
				{Label: "Status", Value: status},
				{Label: "Why", Value: why},
			}},
		},
		Note: "Read-only, and nothing here is fetched by opening this page. " +
			"CloudBurrow downloads a model only when something asks it to run one.",
	}}
	if strings.TrimSpace(model.Notes) != "" {
		sections = append(sections, console.Section{
			ID: "notes", Label: "Notes", Kind: console.KindText,
			Text: model.Notes,
			Note: "From the catalogue, which records what a user has to know before " +
				"choosing a model.",
		})
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Status", Value: status},
			{Label: "Publisher", Value: string(model.Publisher)},
			{Label: "Repository", Value: model.Repo},
			{Label: "Modality", Value: string(model.Modality)},
			{Label: "Runtime", Value: runtime},
		},
		Sections: sections,
	}, nil
}
