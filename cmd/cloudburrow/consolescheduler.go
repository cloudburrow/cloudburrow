package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// schedulerProvider is the Cloud Scheduler screen (#593).
//
// Cloud Scheduler runs in this process, as Cloud Tasks does. Reads come from
// the job store the gRPC service serves; every change is a call on that
// service's own methods, so pausing here is what an SDK's GetJob then reports
// and "Run now" is the RunJob an SDK would make.
//
// The list is the service's ListJobs, so the fault rules for it apply here
// too (#594), as they do to the Cloud Tasks screen.
type schedulerProvider struct {
	svc    *schedulerService
	faults *admin.Faults
}

func (schedulerProvider) ID() string    { return "scheduler" }
func (schedulerProvider) Title() string { return "Cloud Scheduler" }

const schedulerNotStarted = "Cloud Scheduler has not started"

func (p schedulerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, errors.New(schedulerNotStarted)
	}
	listing := console.Listing{
		Columns:      schedulerListColumns,
		Noun:         "jobs",
		AlwaysStatus: true,
	}
	schedulerListTabs(&listing)
	if project == "" {
		listing.Prompt = "Cloud Scheduler lists jobs per project. Choose one in the toolbar."
		return listing, nil
	}
	if err := p.faults.Apply(ctx, "scheduler", schedulerpb.CloudScheduler_ListJobs_FullMethodName, projectResource(project)); err != nil {
		return console.Listing{}, err
	}
	jobs, err := st.List("projects/" + project + "/locations/")
	if err != nil {
		return console.Listing{}, err
	}
	for _, j := range jobs {
		listing.Items = append(listing.Items, console.Resource{
			Name:   j.Name,
			Status: string(j.State),
			Fields: schedulerRow(j),
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	listing.SelectActions = p.ListActions(ctx, project)
	return listing, nil
}

func schedulerTargetType(j scheduler.Job) string {
	switch {
	case j.HTTP != nil:
		return "HTTP"
	case j.PubSub != nil:
		return "Pub/Sub"
	case j.AppEngine != nil:
		return "App Engine"
	}
	return "—"
}

func schedulerTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

// schedulerResult is the last attempt's outcome as a gRPC code name, the way
// Job.status carries it. A job that has never run has no result, not "OK".
func schedulerResult(j scheduler.Job) string {
	if j.LastAttemptTime.IsZero() {
		return "—"
	}
	return codes.Code(uint32(j.LastCode)).String()
}

// schedulerNext is when the job runs next. A paused job does not run, and
// showing its stale schedule time would say it will.
func schedulerNext(j scheduler.Job) string {
	if j.State == scheduler.StatePaused {
		return "paused"
	}
	return schedulerTime(j.ScheduleTime)
}

// schedulerJob checks a job name against the screen's project, so a
// hand-written URL cannot reach another project's job from this one.
func schedulerJob(project, name string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	if !strings.HasPrefix(name, "projects/"+project+"/locations/") || !strings.Contains(name, "/jobs/") {
		return fmt.Errorf("%s is not a job of project %s", name, project)
	}
	return nil
}

func (p schedulerProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	st := p.svc.Store()
	if st == nil {
		return console.Detail{Unavailable: schedulerNotStarted}, nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	if err := schedulerJob(project, path[0]); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	j, err := st.Get(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the job: " + consoleErr(err)}, nil
	}

	lastMessage := "—"
	if !j.LastAttemptTime.IsZero() {
		lastMessage = orDash(j.LastMessage)
	}
	run := console.Section{
		ID: "execution", Label: "Last run", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{
			Heading: "Last attempt",
			Properties: []console.Property{
				{Label: "Attempted", Value: schedulerTime(j.LastAttemptTime)},
				{Label: "Result", Value: schedulerResult(j)},
				{Label: "Message", Value: lastMessage},
				{Label: "Next run", Value: schedulerNext(j)},
			},
		}},
		// Said rather than left for someone to wonder about: Cloud Scheduler
		// keeps its history in Cloud Logging, and this store keeps only what
		// Job.status carries.
		Note: "Only the most recent attempt is recorded, as the Job resource's status " +
			"field carries it. Earlier attempts are not kept.",
	}

	var target []console.Property
	switch {
	case j.HTTP != nil:
		target = []console.Property{
			{Label: "Type", Value: "HTTP"},
			{Label: "URL", Value: j.HTTP.URI},
			{Label: "Method", Value: orDash(j.HTTP.Method)},
			{Label: "Body size", Value: fmt.Sprintf("%d bytes", len(j.HTTP.Body))},
		}
		for _, pair := range sortedPairs(redactHeaders(j.HTTP.Headers)) {
			target = append(target, console.Property{Label: "Header " + pair.Label, Value: pair.Value})
		}
		switch {
		case j.HTTP.OIDCToken != nil:
			target = append(target, authTokenProperties("oidc", j.HTTP.OIDCToken.ServiceAccountEmail, j.HTTP.OIDCToken.Audience, j.HTTP.URI)...)
		case j.HTTP.OAuthToken != nil:
			target = append(target, authTokenProperties("oauth", j.HTTP.OAuthToken.ServiceAccountEmail, j.HTTP.OAuthToken.Scope, j.HTTP.URI)...)
		}
	case j.PubSub != nil:
		target = []console.Property{
			{Label: "Type", Value: "Pub/Sub"},
			{Label: "Topic", Value: j.PubSub.Topic},
			{Label: "Data size", Value: fmt.Sprintf("%d bytes", len(j.PubSub.Data))},
		}
		for _, pair := range sortedPairs(j.PubSub.Attributes) {
			target = append(target, console.Property{Label: "Attribute " + pair.Label, Value: pair.Value})
		}
	case j.AppEngine != nil:
		target = []console.Property{
			{Label: "Type", Value: "App Engine"},
			{Label: "Method", Value: j.AppEngine.Method},
			{Label: "Relative URI", Value: j.AppEngine.RelativeURI},
			{Label: "Host", Value: scheduler.AppEngineHost(strings.Split(j.Name, "/")[1], j.AppEngine.Routing)},
			{Label: "Body size", Value: fmt.Sprintf("%d bytes", len(j.AppEngine.Body))},
			{Label: "Delivery", Value: "This instance has no App Engine: every attempt fails with NOT_FOUND (HTTP 404)."},
		}
		for _, pair := range sortedPairs(redactHeaders(j.AppEngine.Headers)) {
			target = append(target, console.Property{Label: "Header " + pair.Label, Value: pair.Value})
		}
	}
	targetSection := console.Section{
		ID: "target", Label: "Target", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Heading: "Target", Properties: target}},
		Note:   "Authorization and any header that names a token are redacted.",
	}

	r := j.Retry
	config := console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Job", Properties: []console.Property{
				{Label: "Resource name", Value: j.Name},
				{Label: "Description", Value: orDash(j.Description)},
				{Label: "Schedule", Value: j.Schedule},
				{Label: "Time zone", Value: j.TimeZone},
				{Label: "Attempt deadline", Value: j.AttemptDeadline.String()},
				{Label: "Last updated", Value: schedulerTime(j.UserUpdateTime)},
			}},
			{Heading: "Retries", Properties: []console.Property{
				{Label: "Retry count", Value: fmt.Sprint(r.RetryCount)},
				{Label: "Max retry duration", Value: r.MaxRetryDuration.String()},
				{Label: "Min backoff", Value: r.MinBackoff.String()},
				{Label: "Max backoff", Value: r.MaxBackoff.String()},
				{Label: "Max doublings", Value: fmt.Sprint(r.MaxDoublings)},
			}},
		},
	}

	var edit *console.EditForm
	if why := schedulerEditBlocked(j); why != "" {
		config.Note = why
	} else {
		edit = schedulerEditForm(j)
	}

	return console.Detail{
		Edit: edit,
		Summary: []console.Property{
			{Label: "State", Value: string(j.State)},
			{Label: "Schedule", Value: j.Schedule + " (" + j.TimeZone + ")"},
			{Label: "Target", Value: schedulerTargetType(j)},
			{Label: "Last run", Value: schedulerTime(j.LastAttemptTime)},
			{Label: "Next run", Value: schedulerNext(j)},
		},
		Sections: []console.Section{run, targetSection, config},
	}, nil
}

// schedulerActions follows the job's state: a paused job is not offered
// "Pause". Run now is offered either way, as RunJob runs a paused job.
func schedulerActions(state string) []console.Action {
	run := console.Action{ID: "run", Label: "Force run"}
	if strings.EqualFold(state, string(scheduler.StatePaused)) {
		return []console.Action{{ID: "resume", Label: "Resume"}, run}
	}
	return []console.Action{{ID: "pause", Label: "Pause"}, run}
}

func (schedulerProvider) Actions(r console.Resource) []console.Action {
	return schedulerActions(r.Status)
}

func (p schedulerProvider) Act(ctx context.Context, project, name, action string) error {
	return p.act(ctx, project, name, action)
}

func (p schedulerProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	st := p.svc.Store()
	if len(path) == 0 {
		return p.ListActions(context.Background(), project)
	}
	if st == nil || len(path) != 1 || schedulerJob(project, path[0]) != nil {
		return nil
	}
	j, err := st.Get(path[0])
	if err != nil {
		return nil
	}
	return schedulerActions(string(j.State))
}

func (p schedulerProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return p.actOnSelection(ctx, project, action, values)
	}
	if len(path) != 1 {
		return fmt.Errorf("Cloud Scheduler actions apply to a job")
	}
	return p.act(ctx, project, path[0], action)
}

func (p schedulerProvider) act(ctx context.Context, project, name, action string) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(schedulerNotStarted)
	}
	if err := schedulerJob(project, name); err != nil {
		return err
	}
	var err error
	switch action {
	case "pause":
		_, err = api.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: name})
	case "resume":
		_, err = api.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: name})
	case "run":
		_, err = api.RunJob(ctx, &schedulerpb.RunJobRequest{Name: name})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

func (p schedulerProvider) Delete(ctx context.Context, project, name string) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(schedulerNotStarted)
	}
	if err := schedulerJob(project, name); err != nil {
		return err
	}
	_, err := api.DeleteJob(ctx, &schedulerpb.DeleteJobRequest{Name: name})
	return err
}

// CreateForm is Create job, in Google's three steps (#1849): an HTTP,
// Pub/Sub or App Engine HTTP target. An App Engine job is stored and its
// attempts fail, as the help says: there is no App Engine on an instance
// (#1447).
func (schedulerProvider) CreateForm() (string, []console.Field) {
	return "Create job", schedulerCreateFields()
}

func (p schedulerProvider) CreateOnPage() bool { return true }

func (p schedulerProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	return p.createJob(ctx, project, values)
}

// schedulerRegions is what the Create job form offers as its Region: what the
// service's own ListLocations answers for a project with no jobs (#1237).
func schedulerRegions() []string {
	resp, err := scheduler.NewLocationsServer(scheduler.NewStore(store.NewMemory())).ListLocations(
		context.Background(), &locationpb.ListLocationsRequest{Name: "projects/console-regions", PageSize: 1000})
	if err != nil {
		return []string{"us-central1"}
	}
	out := make([]string, 0, len(resp.GetLocations()))
	for _, l := range resp.GetLocations() {
		out = append(out, l.GetLocationId())
	}
	return out
}
