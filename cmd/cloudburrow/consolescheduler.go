package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
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

var schedulerColumns = []string{"Schedule", "Time zone", "Target", "Last run", "Last result", "Next run"}

func (p schedulerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, errors.New(schedulerNotStarted)
	}
	listing := console.Listing{
		Columns:      schedulerColumns,
		NameColumn:   "Job",
		Noun:         "jobs",
		AlwaysStatus: true,
	}
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
			Fields: map[string]string{
				"Schedule":    j.Schedule,
				"Time zone":   j.TimeZone,
				"Target":      schedulerTargetType(j),
				"Last run":    schedulerTime(j.LastAttemptTime),
				"Last result": schedulerResult(j),
				"Next run":    schedulerNext(j),
			},
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func schedulerTargetType(j scheduler.Job) string {
	switch {
	case j.HTTP != nil:
		return "HTTP"
	case j.PubSub != nil:
		return "Pub/Sub"
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
	case j.PubSub != nil:
		target = []console.Property{
			{Label: "Type", Value: "Pub/Sub"},
			{Label: "Topic", Value: j.PubSub.Topic},
			{Label: "Data size", Value: fmt.Sprintf("%d bytes", len(j.PubSub.Data))},
		}
		for _, pair := range sortedPairs(j.PubSub.Attributes) {
			target = append(target, console.Property{Label: "Attribute " + pair.Label, Value: pair.Value})
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

	return console.Detail{
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
	if st == nil || len(path) != 1 || schedulerJob(project, path[0]) != nil {
		return nil
	}
	j, err := st.Get(path[0])
	if err != nil {
		return nil
	}
	return schedulerActions(string(j.State))
}

func (p schedulerProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
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

// CreateForm is the Create job form. It offers the two targets that work
// locally; the help names the ones the API refuses, so the refusal is on the
// form before it is in an error.
func (schedulerProvider) CreateForm() (string, []console.Field) {
	return "Create job", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true,
			Help: "Letters, numbers, hyphens and underscores.", Pattern: `^[A-Za-z0-9_\-]{1,500}$`},
		{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1",
			Help: "Any location string; CloudBurrow does not place resources geographically."},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "schedule", Label: "Frequency", Type: "text", Required: true, Default: "*/5 * * * *",
			Help: "unix-cron format, e.g. \"0 9 * * 1\" for 09:00 every Monday."},
		{Name: "timeZone", Label: "Time zone", Type: "text", Default: "Etc/UTC",
			Help: "An IANA time zone name."},
		{Name: "uri", Label: "HTTP URL", Type: "text", Section: "Target",
			Help: "For an HTTP target: an http or https URL, reached from this machine with POST. " +
				"Give either this or a Pub/Sub topic."},
		{Name: "topic", Label: "Pub/Sub topic", Type: "text", Section: "Target",
			Help: "For a Pub/Sub target: projects/{project}/topics/{topic}, published to the local emulator."},
		{Name: "body", Label: "Body or message", Type: "textarea", Section: "Target",
			Help: "The HTTP body, or the Pub/Sub message data (required for a Pub/Sub target). " +
				"App Engine targets and OIDC or OAuth tokens are not implemented locally and are not offered."},
	}
}

func (p schedulerProvider) CreateOnPage() bool { return true }

func (p schedulerProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.svc.API()
	if api == nil {
		return "", errors.New(schedulerNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a job")
	}
	location := strings.TrimSpace(values["location"])
	if location == "" {
		location = "us-central1"
	}
	parent := "projects/" + project + "/locations/" + location
	job := &schedulerpb.Job{
		Name:        parent + "/jobs/" + strings.TrimSpace(values["name"]),
		Description: values["description"],
		Schedule:    strings.TrimSpace(values["schedule"]),
		TimeZone:    strings.TrimSpace(values["timeZone"]),
	}
	uri, topic := strings.TrimSpace(values["uri"]), strings.TrimSpace(values["topic"])
	switch {
	case uri != "" && topic != "":
		return "", fmt.Errorf("give either an HTTP URL or a Pub/Sub topic, not both")
	case uri != "":
		job.Target = &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: uri, HttpMethod: schedulerpb.HttpMethod_POST, Body: []byte(values["body"]),
		}}
	case topic != "":
		job.Target = &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: topic, Data: []byte(values["body"]),
		}}
	default:
		return "", fmt.Errorf("a job needs a target: an HTTP URL or a Pub/Sub topic")
	}
	created, err := api.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: job})
	if err != nil {
		return "", err
	}
	return created.GetName(), nil
}
