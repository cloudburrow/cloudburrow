package main

// Cloud Tasks console parity (#1848): the Queues list's columns, Create queue
// with its configuration, the queue page's Pause queue, Resume queue, Purge
// queue and Delete queue, its Metrics and Logs tabs, and the checked queues'
// header actions, as Google's console has them
// (https://docs.cloud.google.com/tasks/docs/creating-queues,
// https://docs.cloud.google.com/tasks/docs/deleting-appengine-queues-and-tasks,
// https://docs.cloud.google.com/tasks/docs/monitor, read 2026-11-19).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// tasksListColumns are the Queues list's columns: Google's first, then
// CloudBurrow's Due now and Created. The state is also the status icon.
var tasksListColumns = []string{"Region", "State", "Tasks in queue", "Tasks running", "Max rate",
	"Max concurrent", "Max attempts", "Oldest task", "Due now", "Created"}

// tasksQueueRow is a queue's columns that do not need its tasks.
func tasksQueueRow(q tasks.Queue, running int) map[string]string {
	region := "—"
	if parts := strings.Split(q.Name, "/"); len(parts) >= 4 {
		region = parts[3]
	}
	rate := q.RateLimits.MaxDispatchesPerSecond
	if rate <= 0 {
		rate = tasks.DefaultRateLimits().MaxDispatchesPerSecond
	}
	attempts := fmt.Sprint(q.RetryConfig.MaxAttempts)
	if q.RetryConfig.MaxAttempts < 0 {
		attempts = "Unlimited"
	}
	return map[string]string{
		"Region":         region,
		"State":          tasksStateText(q.State),
		"Tasks in queue": "—",
		"Tasks running":  strconv.Itoa(running),
		"Max rate":       strconv.FormatFloat(rate, 'f', -1, 64) + "/s",
		"Max concurrent": fmt.Sprint(q.RateLimits.MaxConcurrentDispatches),
		"Max attempts":   attempts,
		"Oldest task":    "—",
		"Due now":        "—",
		"Created":        q.Created.Format(time.RFC3339),
	}
}

// tasksStateText is a queue state as the list's State column words it.
func tasksStateText(s tasks.State) string {
	switch s {
	case tasks.StateRunning:
		return "Running"
	case tasks.StatePaused:
		return "Paused"
	case tasks.StateDisabled:
		return "Disabled"
	}
	return string(s)
}

// tasksDepthFields fills the columns read from a queue's tasks: how many,
// how many are due, and when the oldest was created.
func tasksDepthFields(fields map[string]string, ts []tasks.Task, now time.Time) {
	ready := 0
	var oldest time.Time
	for _, t := range ts {
		if !t.ScheduleTime.After(now) {
			ready++
		}
		if oldest.IsZero() || t.Created.Before(oldest) {
			oldest = t.Created
		}
	}
	fields["Tasks in queue"] = strconv.Itoa(len(ts))
	fields["Due now"] = strconv.Itoa(ready)
	if !oldest.IsZero() {
		fields["Oldest task"] = oldest.Format(time.RFC3339)
	}
}

// tasksQueueActions are a queue row's menu: Pause queue or Resume queue by
// its state, and Purge queue. Delete is the list's own.
func tasksQueueActions(state string) []console.Action {
	purge := console.Action{ID: "purge", Label: "Purge queue", Destructive: true, Confirm: tasksPurgeConfirm}
	if strings.EqualFold(state, string(tasks.StatePaused)) {
		return []console.Action{{ID: "resume", Label: "Resume queue"}, purge}
	}
	return []console.Action{{ID: "pause", Label: "Pause queue"}, purge}
}

const tasksPurgeConfirm = "Purge deletes every task in the queue. Tasks created up to about a minute " +
	"after the purge may be deleted too, as on Cloud Tasks."

// tasksTaskActions are a task's Run now and Delete task, on its row in the
// queue's Tasks tab and on its own page.
func tasksTaskActions() []console.Action {
	return []console.Action{
		{ID: "runtask", Label: "Run now"},
		{ID: "deletetask", Label: "Delete task", Destructive: true},
	}
}

// Selection actions on the Queues list: Google selects queues, then the
// header's Pause queue, Resume queue or Purge queue.
const (
	tasksPauseSelected  = "pausequeues"
	tasksResumeSelected = "resumequeues"
	tasksPurgeSelected  = "purgequeues"
)

// ListActions are the checked queues' header actions (console.ListActor).
func (p tasksProvider) ListActions(context.Context, string) []console.Action {
	if p.svc.Store() == nil {
		return nil
	}
	field := func() []console.Field {
		return []console.Field{{Name: "queues", Label: "Queues", Type: "textarea", Required: true,
			Help: "The queues checked in the list, one per line."}}
	}
	return []console.Action{
		{ID: tasksPauseSelected, Label: "Pause queue", SelectionField: "queues", Fields: field()},
		{ID: tasksResumeSelected, Label: "Resume queue", SelectionField: "queues", Fields: field()},
		{ID: tasksPurgeSelected, Label: "Purge queue", SelectionField: "queues", Fields: field(),
			Destructive: true, Confirm: tasksPurgeConfirm},
	}
}

func (p tasksProvider) actOnSelection(ctx context.Context, project, action string, values map[string]string) error {
	var single string
	switch action {
	case tasksPauseSelected:
		single = "pause"
	case tasksResumeSelected:
		single = "resume"
	case tasksPurgeSelected:
		single = "purge"
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	names := strings.Fields(values["queues"])
	if len(names) == 0 {
		return errors.New("check at least one queue")
	}
	for _, name := range names {
		if err := tasksInProject(project, name); err != nil {
			return err
		}
		if err := p.Act(ctx, project, name, single); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// queuePageActions are the queue page's title line: Create task, Pause
// queue or Resume queue, Purge queue and Delete queue, then the Logs tab's
// Configure logging.
func (p tasksProvider) queuePageActions(name string) []console.Action {
	state := ""
	var q tasks.Queue
	if got, err := p.svc.Store().GetQueue(name); err == nil {
		q, state = got, string(got.State)
	}
	pause := console.Action{ID: "pausequeue", Label: "Pause queue"}
	if strings.EqualFold(state, string(tasks.StatePaused)) {
		pause = console.Action{ID: "resumequeue", Label: "Resume queue"}
	}
	return []console.Action{
		{ID: "createtask", Label: "Create task", Fields: tasksCreateTaskFields()},
		pause,
		{ID: "purgequeue", Label: "Purge queue", Destructive: true, Confirm: tasksPurgeConfirm},
		{ID: "deletequeue", Label: "Delete queue", Destructive: true, Leaves: true,
			Confirm: "Deleting the queue deletes its tasks. A queue of the same name cannot be created for 3 days " +
				"after, as on Cloud Tasks."},
		tasksLoggingAction(q),
	}
}

func tasksQueuePageAction(action string) bool {
	switch action {
	case "pausequeue", "resumequeue", "purgequeue", "deletequeue", "logging":
		return true
	}
	return false
}

func (p tasksProvider) actOnQueue(ctx context.Context, project, name, action string, values map[string]string) error {
	if err := tasksInProject(project, name); err != nil {
		return err
	}
	api := p.api()
	if api == nil {
		return errors.New("Cloud Tasks has not started")
	}
	var err error
	switch action {
	case "pausequeue":
		_, err = api.PauseQueue(ctx, &cloudtaskspb.PauseQueueRequest{Name: name})
	case "resumequeue":
		_, err = api.ResumeQueue(ctx, &cloudtaskspb.ResumeQueueRequest{Name: name})
	case "purgequeue":
		_, err = api.PurgeQueue(ctx, &cloudtaskspb.PurgeQueueRequest{Name: name})
	case "deletequeue":
		_, err = api.DeleteQueue(ctx, &cloudtaskspb.DeleteQueueRequest{Name: name})
	case "logging":
		err = p.setLogging(ctx, name, values)
	}
	return err
}

// tasksLoggingAction is Configure logging on the Logs tab: logging on, with
// the fraction of attempts written, or off.
func tasksLoggingAction(q tasks.Queue) console.Action {
	on, ratio := "Off", "1.0"
	if q.Logging != nil && q.Logging.SamplingRatio > 0 {
		on, ratio = "On", strconv.FormatFloat(q.Logging.SamplingRatio, 'f', -1, 64)
	}
	return console.Action{ID: "logging", Label: "Configure logging", Fields: []console.Field{
		{Name: "logging", Label: "Logs", Type: "radio", Required: true, Default: on, Options: []string{"On", "Off"},
			Help: "On writes the queue's dispatch attempts to Cloud Logging, in the log " +
				"cloudtasks.googleapis.com/task_operations_log."},
		{Name: "samplingRatio", Label: "Sampling ratio", Type: "text", Default: ratio, ShowWhen: "logging=On",
			Pattern: `^(0(\.[0-9]+)?|1(\.0+)?)$`,
			Help:    "The fraction of attempts written, from 0.0 to 1.0. 1.0 logs every attempt."},
	}}
}

// setLogging is UpdateQueue of stackdriver_logging_config alone.
func (p tasksProvider) setLogging(ctx context.Context, name string, values map[string]string) error {
	ratio := 0.0
	if values["logging"] == "On" {
		v, err := strconv.ParseFloat(strings.TrimSpace(values["samplingRatio"]), 64)
		if err != nil || v <= 0 || v > 1 {
			return fmt.Errorf("sampling ratio %q is not a number above 0 and at most 1.0", values["samplingRatio"])
		}
		ratio = v
	}
	_, err := p.api().UpdateQueue(ctx, &cloudtaskspb.UpdateQueueRequest{
		Queue: &cloudtaskspb.Queue{Name: name,
			StackdriverLoggingConfig: &cloudtaskspb.StackdriverLoggingConfig{SamplingRatio: ratio}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stackdriver_logging_config"}},
	})
	return err
}

// tasksLogsSection is the queue's Logs tab: its lines in the instance's
// Cloud Logging, with Configure logging at the top, where Google's console
// turns a queue's logging on or off.
func tasksLogsSection(q tasks.Queue) console.Section {
	return console.Section{ID: "logs", Label: "Logs", Kind: console.KindLogs,
		Note:    "Logging: " + tasksSamplingText(q) + ".",
		Actions: []console.Action{tasksLoggingAction(q)}}
}

// tasksMetricsSection is the Metrics tab: Cloud Monitoring's charts, which
// no local service provides.
func tasksMetricsSection() console.Section {
	return console.Section{ID: "metrics", Label: "Metrics", Kind: console.KindProperties,
		Note: "Not available locally: a queue's metrics are Cloud Monitoring time series, and this instance " +
			"does not run Cloud Monitoring. The Tasks tab and the Queues list show its depth.",
		Groups: []console.PropertyGroup{{Heading: "Metrics", Properties: []console.Property{
			{Label: "Cloud Monitoring", Value: "Not available locally"},
		}}}}
}

// tasksCreateQueueFields is Create queue: Queue name, the Region list, then
// the queue's configuration, as Edit queue holds it, with the service's
// defaults.
func tasksCreateQueueFields() []console.Field {
	fields := []console.Field{
		{Name: "name", Label: "Queue name", Type: "text", Required: true,
			Help:    "Letters, numbers and hyphens, starting with a letter; at most 100.",
			Pattern: `^[A-Za-z][A-Za-z0-9\-]{0,99}$`},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1",
			Options: append([]string(nil), locations.Regions...),
			Help:    "Where the queue is. CloudBurrow does not place resources geographically."},
	}
	defaults := tasks.Queue{RetryConfig: tasks.DefaultRetryConfig(), RateLimits: tasks.DefaultRateLimits()}
	for _, f := range tasksQueueEditForm(defaults).Fields {
		if f.Immutable {
			continue
		}
		fields = append(fields, f)
	}
	return fields
}

// CreateLayout draws Create queue as one page under its section headings.
func (tasksProvider) CreateLayout() string { return console.CreateLayoutPage }

// createQueue is CreateQueue with the form's configuration.
func (p tasksProvider) createQueue(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.api()
	if api == nil {
		return "", errors.New("Cloud Tasks has not started")
	}
	location := strings.TrimSpace(values["location"])
	if location == "" {
		location = "us-central1"
	}
	parent := "projects/" + project + "/locations/" + location
	// A form that sends no configuration (an older client, a test) gets the
	// service's defaults, as CreateQueue with none does.
	for k, v := range map[string]string{"maxDispatchesPerSecond": "0", "maxConcurrentDispatches": "0",
		"maxAttempts": "0", "maxDoublings": "0"} {
		if strings.TrimSpace(values[k]) == "" {
			values[k] = v
		}
	}
	q, err := tasksQueueFromForm(parent+"/queues/"+strings.TrimSpace(values["name"]), values)
	if err != nil {
		return "", err
	}
	created, err := api.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: parent, Queue: q})
	if err != nil {
		return "", err
	}
	return created.GetName(), nil
}

var _ console.ListActor = tasksProvider{}
var _ console.CreateLayouter = tasksProvider{}

// titleActions are the queue page's actions beside its title: all but
// Configure logging, which is drawn on the Logs tab.
func (p tasksProvider) titleActions(name string) []console.Action {
	var out []console.Action
	for _, a := range p.queuePageActions(name) {
		if a.ID != "logging" {
			out = append(out, a)
		}
	}
	return out
}

// DeleteLabel names the row menu's Delete, as Google's Delete queue.
func (tasksProvider) DeleteLabel() string { return "Delete queue" }

var _ console.DeleteLabeler = tasksProvider{}
