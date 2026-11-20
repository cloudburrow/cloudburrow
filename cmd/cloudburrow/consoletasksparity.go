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
	if strings.EqualFold(state, string(tasks.StatePaused)) {
		return []console.Action{tasksResume("resume"), tasksPurge("purge")}
	}
	return []console.Action{tasksPause("pause"), tasksPurge("purge")}
}

// Google's prompts, from
// https://docs.cloud.google.com/tasks/docs/deleting-appengine-queues-and-tasks
// (read 2026-11-20): Pause queue, "At the prompt, click Pause"; Resume
// queue, no prompt; Purge queue, "At the prompt, click Purge"; Delete
// queue, "type the queue name, and then click Confirm"; Delete task, "At
// the prompt, click Delete". Nothing is typed but the queue's name.
const (
	tasksPauseConfirm = "The queue stops dispatching its tasks until it is resumed. Tasks can still be " +
		"added to it."
	tasksPurgeConfirm = "Purge deletes every task in the queue. Tasks created up to about a minute " +
		"after the purge may be deleted too, as on Cloud Tasks."
	tasksDeleteQueueConfirm = "Deleting the queue deletes its tasks. A queue of the same name cannot be " +
		"created for 3 days after, as on Cloud Tasks."
	tasksDeleteTaskConfirm = "The task is deleted and is not dispatched again."
)

// tasksPause is Pause queue under id, confirmed by Pause alone.
func tasksPause(id string) console.Action {
	return console.Action{ID: id, Label: "Pause queue", Confirm: tasksPauseConfirm,
		Submit: "Pause", ConfirmStyle: console.DeleteConfirmClick}
}

// tasksResume is Resume queue under id, performed on click.
func tasksResume(id string) console.Action {
	return console.Action{ID: id, Label: "Resume queue"}
}

// tasksPurge is Purge queue under id, confirmed by Purge alone.
func tasksPurge(id string) console.Action {
	return console.Action{ID: id, Label: "Purge queue", Destructive: true, Confirm: tasksPurgeConfirm,
		Submit: "Purge", ConfirmStyle: console.DeleteConfirmClick}
}

// tasksTaskActions are a task's Run now and Delete task, on its row in the
// queue's Tasks tab and on its own page; Delete task asks for Delete alone.
func tasksTaskActions() []console.Action {
	return []console.Action{
		{ID: "runtask", Label: "Run now"},
		{ID: "deletetask", Label: "Delete task", Destructive: true, Confirm: tasksDeleteTaskConfirm,
			Submit: "Delete", ConfirmStyle: console.DeleteConfirmClick},
	}
}

// tasksDeleteSelected is the Tasks tab's header Delete task over the
// checked tasks: "Select the checkbox for the task ... Click Delete task.
// At the prompt, click Delete."
const tasksDeleteSelected = "deletetasks"

func tasksDeleteTasksAction() console.Action {
	return console.Action{ID: tasksDeleteSelected, Label: "Delete task", Destructive: true,
		SelectionField: "tasks", Fields: []console.Field{{Name: "tasks", Type: "hidden"}},
		ConfirmTitle: "Delete task?", Confirm: "The checked tasks are deleted and are not dispatched again.",
		Submit: "Delete", ConfirmStyle: console.DeleteConfirmClick}
}

// deleteTasks deletes the checked tasks of queue, each a task ID or its
// full name inside the queue.
func (p tasksProvider) deleteTasks(project, queue string, values map[string]string) error {
	if err := tasksInProject(project, queue); err != nil {
		return err
	}
	names := strings.Fields(values["tasks"])
	if len(names) == 0 {
		return errors.New("check at least one task")
	}
	for _, n := range names {
		full := n
		if !strings.Contains(n, "/tasks/") {
			full = queue + "/tasks/" + n
		}
		if !strings.HasPrefix(full, queue+"/tasks/") {
			return fmt.Errorf("%s is not a task of %s", n, queue)
		}
		if err := p.svc.Store().DeleteTask(full); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// DeleteWords name the list's Delete queue over the checked queues and its
// prompt's Confirm (console.DeleteWorder).
func (tasksProvider) DeleteWords() console.DeleteWords {
	return console.DeleteWords{Selected: "Delete queue", Submit: "Confirm"}
}

var _ console.DeleteWorder = tasksProvider{}

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
	// The checked queues, as hidden as Google's checkboxes: the header
	// button acts at once, or after its prompt, with no form between.
	sel := func(a console.Action) console.Action {
		a.SelectionField = "queues"
		a.Fields = []console.Field{{Name: "queues", Type: "hidden"}}
		return a
	}
	return []console.Action{
		sel(tasksPause(tasksPauseSelected)),
		sel(tasksResume(tasksResumeSelected)),
		sel(tasksPurge(tasksPurgeSelected)),
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
// Enable logs or Disable logs and the Tasks tab's Delete task.
func (p tasksProvider) queuePageActions(name string) []console.Action {
	state := ""
	var q tasks.Queue
	if got, err := p.svc.Store().GetQueue(name); err == nil {
		q, state = got, string(got.State)
	}
	pause := tasksPause("pausequeue")
	if strings.EqualFold(state, string(tasks.StatePaused)) {
		pause = tasksResume("resumequeue")
	}
	return []console.Action{
		{ID: "createtask", Label: "Create task", Fields: tasksCreateTaskFields()},
		pause,
		tasksPurge("purgequeue"),
		{ID: "deletequeue", Label: "Delete queue", Destructive: true, Leaves: true,
			Confirm: tasksDeleteQueueConfirm, Submit: "Confirm"},
		tasksLoggingAction(q),
		tasksDeleteTasksAction(),
	}
}

func tasksQueuePageAction(action string) bool {
	switch action {
	case "pausequeue", "resumequeue", "purgequeue", "deletequeue", tasksEnableLogs, tasksDisableLogs, tasksDeleteSelected:
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
	case tasksDeleteSelected:
		err = p.deleteTasks(project, name, values)
	case "pausequeue":
		_, err = api.PauseQueue(ctx, &cloudtaskspb.PauseQueueRequest{Name: name})
	case "resumequeue":
		_, err = api.ResumeQueue(ctx, &cloudtaskspb.ResumeQueueRequest{Name: name})
	case "purgequeue":
		_, err = api.PurgeQueue(ctx, &cloudtaskspb.PurgeQueueRequest{Name: name})
	case "deletequeue":
		_, err = api.DeleteQueue(ctx, &cloudtaskspb.DeleteQueueRequest{Name: name})
	case tasksEnableLogs:
		err = p.setSampling(ctx, name, 1)
	case tasksDisableLogs:
		err = p.setSampling(ctx, name, 0)
	}
	return err
}

// tasksLoggingAction is the Logs tab's Enable logs, or Disable logs when
// the queue logs, as https://docs.cloud.google.com/tasks/docs/monitor
// (read 2026-11-20) has them: "Click Enable logs and, to confirm, click
// Enable", and "Disable logs. To confirm, click Disable". The sampling
// ratio is Edit queue's.
func tasksLoggingAction(q tasks.Queue) console.Action {
	if q.Logging != nil && q.Logging.SamplingRatio > 0 {
		return console.Action{ID: tasksDisableLogs, Label: "Disable logs", Submit: "Disable",
			ConfirmStyle: console.DeleteConfirmClick,
			Confirm:      "The queue's dispatch attempts stop being written to Cloud Logging. Entries already written are kept."}
	}
	return console.Action{ID: tasksEnableLogs, Label: "Enable logs", Submit: "Enable",
		ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Every dispatch attempt of the queue is written to Cloud Logging, in the log " +
			"cloudtasks.googleapis.com/task_operations_log (sampling ratio 1.0; Edit queue changes it)."}
}

// The Logs tab's actions.
const (
	tasksEnableLogs  = "enablelogs"
	tasksDisableLogs = "disablelogs"
)

// setSampling is UpdateQueue of stackdriver_logging_config alone.
func (p tasksProvider) setSampling(ctx context.Context, name string, ratio float64) error {
	_, err := p.api().UpdateQueue(ctx, &cloudtaskspb.UpdateQueueRequest{
		Queue: &cloudtaskspb.Queue{Name: name,
			StackdriverLoggingConfig: &cloudtaskspb.StackdriverLoggingConfig{SamplingRatio: ratio}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stackdriver_logging_config"}},
	})
	return err
}

// tasksLogsSection is the queue's Logs tab: its lines in the instance's
// Cloud Logging, with Enable logs or Disable logs at the top, where Google's
// console turns a queue's logging on or off.
func tasksLogsSection(q tasks.Queue) console.Section {
	return console.Section{ID: "logs", Label: "Logs", Kind: console.KindLogs,
		Note:    "Logging: " + tasksSamplingText(q) + ".",
		Actions: []console.Action{tasksLoggingAction(q)}}
}

// tasksMetricsSection is the Metrics tab: Google's queue metrics, from
// https://docs.cloud.google.com/tasks/docs/monitor (read 2026-11-20): API
// requests, Queue depth, Task attempt count and Task attempt delays. They
// are Cloud Monitoring time series, which no local service provides; the
// queue's depth now is read from its tasks.
func tasksMetricsSection(depth int) console.Section {
	na := "Not available locally (Cloud Monitoring)"
	return console.Section{ID: "metrics", Label: "Metrics", Kind: console.KindProperties,
		Note: "A queue's metrics are Cloud Monitoring time series, and this instance does not run " +
			"Cloud Monitoring, so no chart is drawn. Queue depth is the number of tasks in the queue now.",
		Groups: []console.PropertyGroup{{Heading: "Metrics", Properties: []console.Property{
			{Label: "API requests", Value: na},
			{Label: "Queue depth", Value: strconv.Itoa(depth) + " now; " + na + " over time"},
			{Label: "Task attempt count", Value: na},
			{Label: "Task attempt delays", Value: na},
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
// the Logs tab's and the Tasks tab's, which are drawn on those tabs.
func (p tasksProvider) titleActions(name string) []console.Action {
	var out []console.Action
	for _, a := range p.queuePageActions(name) {
		if a.ID != tasksEnableLogs && a.ID != tasksDisableLogs && a.ID != tasksDeleteSelected {
			out = append(out, a)
		}
	}
	return out
}

// DeleteLabel names the row menu's Delete, as Google's Delete queue.
func (tasksProvider) DeleteLabel() string { return "Delete queue" }

var _ console.DeleteLabeler = tasksProvider{}
