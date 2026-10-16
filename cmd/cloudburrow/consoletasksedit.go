package main

// Cloud Tasks: Edit queue and Create task (#784).
//
// A queue's rate limits and retry parameters were shown and fixed, and a queue
// could not be given a task: UpdateQueue and CreateTask are verified with the
// official client, and the console offered neither. Both go through the
// service's own API — the same GRPCServer methods an SDK's call reaches — so
// the console accepts exactly what the API accepts and refuses what it refuses,
// with its message.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// api is the Cloud Tasks API the gRPC and JSON ports serve, over the same
// store. It holds nothing but the store, so one built per call is the same
// server.
func (p tasksProvider) api() *tasks.GRPCServer {
	st := p.svc.Store()
	if st == nil {
		return nil
	}
	return tasks.NewGRPCServer(st)
}

// tasksEditPaths are the update_mask paths the edit form sends: every field it
// holds, by name, so nothing the form does not show is replaced.
var tasksEditPaths = []string{
	"rate_limits.max_dispatches_per_second",
	"rate_limits.max_concurrent_dispatches",
	"retry_config.max_attempts",
	"retry_config.min_backoff",
	"retry_config.max_backoff",
	"retry_config.max_doublings",
	"retry_config.max_retry_duration",
	"stackdriver_logging_config",
	"http_target",
}

// tasksDurationHelp is the help every duration field carries.
const tasksDurationHelp = "A duration such as 0.1s, 10s, 5m or 1h."

// tasksQueueConfigGroups is a queue's Configuration tab: what the edit form
// changes, and the burst size the API derives from the rate.
func tasksQueueConfigGroups(q tasks.Queue) []console.PropertyGroup {
	r, l := q.RetryConfig, q.RateLimits
	retryFor := "Unlimited"
	if r.MaxRetryDuration > 0 {
		retryFor = r.MaxRetryDuration.String()
	}
	attempts := fmt.Sprint(r.MaxAttempts)
	if r.MaxAttempts < 0 {
		attempts = "Unlimited"
	}
	return []console.PropertyGroup{
		{Heading: "Rate limits", Properties: []console.Property{
			{Label: "Max dispatches per second", Value: strconv.FormatFloat(l.MaxDispatchesPerSecond, 'f', -1, 64)},
			{Label: "Max burst size", Value: fmt.Sprint(tasks.BurstSize(l.MaxDispatchesPerSecond))},
			{Label: "Max concurrent dispatches", Value: fmt.Sprint(l.MaxConcurrentDispatches)},
		}},
		{Heading: "Retry parameters", Properties: []console.Property{
			{Label: "Max attempts", Value: attempts},
			{Label: "Max retry duration", Value: retryFor},
			{Label: "Min backoff", Value: r.MinBackoff.String()},
			{Label: "Max backoff", Value: r.MaxBackoff.String()},
			{Label: "Max doublings", Value: fmt.Sprint(r.MaxDoublings)},
		}},
		{Heading: "Logging", Properties: []console.Property{
			{Label: "Sampling ratio", Value: tasksSamplingText(q)},
		}},
		tasksHTTPTargetGroup(q),
		{Heading: "Queue", Properties: []console.Property{
			{Label: "Resource name", Value: q.Name},
			{Label: "State", Value: string(q.State)},
		}},
	}
}

// tasksSamplingText is a queue's stackdriver_logging_config as its page
// shows it (#1256).
func tasksSamplingText(q tasks.Queue) string {
	if q.Logging == nil || q.Logging.SamplingRatio == 0 {
		return "0 (no attempt is logged)"
	}
	return strconv.FormatFloat(q.Logging.SamplingRatio, 'f', -1, 64) + " of attempts written to Cloud Logging"
}

// tasksSamplingDefault is the Sampling ratio field's current value.
func tasksSamplingDefault(q tasks.Queue) string {
	if q.Logging == nil {
		return ""
	}
	return strconv.FormatFloat(q.Logging.SamplingRatio, 'f', -1, 64)
}

// tasksQueueEditForm is Edit queue, prefilled from the stored queue.
//
// Only what UpdateQueue applies here: the rate and concurrency limits and the
// five retry parameters, and the logging sampling ratio (#1256). Max burst
// size is shown and not sent, because the API derives it from the rate and
// refuses it as output only. The queue's HTTP target (#1282) is on it; App
// Engine routing is refused as UNIMPLEMENTED by this instance's API, so it
// is not on the form at all.
func tasksQueueEditForm(q tasks.Queue) *console.EditForm {
	r, l := q.RetryConfig, q.RateLimits
	retryFor := ""
	if r.MaxRetryDuration > 0 {
		retryFor = r.MaxRetryDuration.String()
	}
	const rates, retries, logs, target = "Rate limits", "Retry parameters", "Logging", "HTTP target"
	tv := tasksHTTPTargetValues(q)
	return &console.EditForm{
		Label: "Edit queue",
		Fields: []console.Field{
			{Name: "maxDispatchesPerSecond", Label: "Max dispatches per second", Type: "text", Required: true,
				Section: rates, Pattern: `^[0-9]+(\.[0-9]+)?$`,
				Default: strconv.FormatFloat(l.MaxDispatchesPerSecond, 'f', -1, 64),
				Help:    "Tasks dispatched per second, at most 500. 0 sets the default, 500."},
			{Name: "maxBurstSize", Label: "Max burst size", Type: "text", Section: rates, Immutable: true,
				Default: fmt.Sprint(tasks.BurstSize(l.MaxDispatchesPerSecond)),
				Help:    "Output only: Cloud Tasks sets it from max dispatches per second."},
			{Name: "maxConcurrentDispatches", Label: "Max concurrent dispatches", Type: "number", Required: true,
				Section: rates, Default: fmt.Sprint(l.MaxConcurrentDispatches),
				Help: "Tasks in flight at once, at most 5000. 0 sets the default, 1000."},
			{Name: "maxAttempts", Label: "Max attempts", Type: "number", Required: true,
				Section: retries, Default: fmt.Sprint(r.MaxAttempts),
				Help: "Attempts per task, the first included. -1 is unlimited; 0 sets the default, 100."},
			{Name: "maxRetryDuration", Label: "Max retry duration", Type: "text",
				Section: retries, Default: retryFor,
				Help: "Optional. How long after its first attempt a task is retried; empty or 0 is unlimited. " +
					tasksDurationHelp},
			{Name: "minBackoff", Label: "Min backoff", Type: "text", Required: true,
				Section: retries, Default: r.MinBackoff.String(), Help: tasksDurationHelp},
			{Name: "maxBackoff", Label: "Max backoff", Type: "text", Required: true,
				Section: retries, Default: r.MaxBackoff.String(), Help: tasksDurationHelp},
			{Name: "maxDoublings", Label: "Max doublings", Type: "number", Required: true,
				Section: retries, Default: fmt.Sprint(r.MaxDoublings),
				Help: "How many times the interval between retries doubles before it grows linearly."},
			{Name: "targetURI", Label: "URI override", Type: "text", Section: target,
				Default: tv.uri, Pattern: `^(https?://[^/?#\s]+[^\s]*)?$`,
				Help: "Optional. A URL whose scheme, host, port, path and query replace the task's on each dispatch " +
					"(http_target.uri_override); empty for none."},
			{Name: "targetMode", Label: "URI override mode", Type: "text", Section: target,
				Default: tv.mode, Pattern: `^(ALWAYS|IF_NOT_EXISTS)?$`,
				Help: "ALWAYS (the default) rewrites every task's URL; IF_NOT_EXISTS only a task created without one."},
			{Name: "targetMethod", Label: "HTTP method override", Type: "text", Section: target,
				Default: tv.method, Pattern: `^(POST|GET|HEAD|PUT|DELETE|PATCH|OPTIONS)?$`,
				Help: "Optional. One of " + tasksHTTPMethods + ", replacing each task's."},
			{Name: "targetHeaders", Label: "Header overrides", Type: "map", Section: target, Default: tv.headers,
				Help: "Optional. One Name=value per line, set on every dispatch over the task's own."},
			{Name: "targetOidcAccount", Label: "OIDC token service account", Type: "text", Section: target,
				Default: tv.oidc,
				Help:    "Optional. Every dispatch carries an ID token for this account, replacing the task's own token."},
			{Name: "samplingRatio", Label: "Sampling ratio", Type: "text", Section: logs,
				Default: tasksSamplingDefault(q), Pattern: `^(0(\.[0-9]+)?|1(\.0+)?)$`,
				Help: "Optional. The fraction of dispatch attempts written to Cloud Logging, from 0.0 to 1.0, " +
					"in the log cloudtasks.googleapis.com/task_operations_log. Empty or 0 logs none."},
		},
		Note: "Saved through UpdateQueue, and applied to tasks already in the queue. The queue's " +
			"name and region cannot be changed, and its state changes with Pause and Resume. App Engine " +
			"routing is not implemented on this instance, so it is not offered. Sampled attempts are " +
			"written to Cloud Logging only when the instance serves it.",
	}
}

// Edit implements console.Editor: UpdateQueue with the form's fields, named
// in the update mask one by one.
func (p tasksProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a queue can be edited: a task is not changed after it is created")
	}
	api := p.api()
	if api == nil {
		return errors.New("Cloud Tasks has not started")
	}
	name := path[0]
	if err := tasksInProject(project, name); err != nil {
		return err
	}
	// UpdateQueue creates a queue that does not exist, as the API documents.
	// An edit is of the queue on screen, so one deleted meanwhile is reported
	// rather than made again.
	if _, err := api.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: name}); err != nil {
		return err
	}

	var errs []error
	rate := parseFloatField(values, "maxDispatchesPerSecond", "Max dispatches per second", &errs)
	concurrent := parseInt32Field(values, "maxConcurrentDispatches", "Max concurrent dispatches", &errs)
	attempts := parseInt32Field(values, "maxAttempts", "Max attempts", &errs)
	doublings := parseInt32Field(values, "maxDoublings", "Max doublings", &errs)
	minBackoff := parseDurationField(values, "minBackoff", "Min backoff", &errs)
	maxBackoff := parseDurationField(values, "maxBackoff", "Max backoff", &errs)
	retryFor := parseDurationField(values, "maxRetryDuration", "Max retry duration", &errs)
	var logging *cloudtaskspb.StackdriverLoggingConfig
	if strings.TrimSpace(values["samplingRatio"]) != "" {
		logging = &cloudtaskspb.StackdriverLoggingConfig{
			SamplingRatio: parseFloatField(values, "samplingRatio", "Sampling ratio", &errs)}
	}
	target, err := tasksHTTPTargetFromForm(values)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	_, err = api.UpdateQueue(ctx, &cloudtaskspb.UpdateQueueRequest{
		Queue: &cloudtaskspb.Queue{
			Name: name,
			RateLimits: &cloudtaskspb.RateLimits{
				MaxDispatchesPerSecond:  rate,
				MaxConcurrentDispatches: concurrent,
			},
			RetryConfig: &cloudtaskspb.RetryConfig{
				MaxAttempts:      attempts,
				MinBackoff:       durationpb.New(minBackoff),
				MaxBackoff:       durationpb.New(maxBackoff),
				MaxDoublings:     doublings,
				MaxRetryDuration: durationpb.New(retryFor),
			},
			StackdriverLoggingConfig: logging,
			HttpTarget:               target,
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: tasksEditPaths},
	})
	return err
}

// tasksHTTPMethods are the methods Create task offers: HttpMethod's values,
// less the unspecified one.
const tasksHTTPMethods = "POST, GET, HEAD, PUT, DELETE, PATCH or OPTIONS"

// tasksCreateTaskFields is Create task on a queue's page. A function so the
// pattern check in consolepatterns_test.go reads the fields shipped.
//
// An HTTP target, with an OIDC or OAuth token (#1243), or an App Engine
// target (#1447), which is stored and whose every attempt fails: there is
// no App Engine on an instance.
func tasksCreateTaskFields() []console.Field {
	return append([]console.Field{
		{Name: "target", Label: "Target", Type: "select", Required: true, Default: "HTTP",
			Options: []string{"HTTP", "App Engine"},
			Help: "HTTP sends the task to the URL. App Engine stores an appEngineHttpRequest; " +
				"this instance has no App Engine, so each attempt fails with NOT_FOUND (HTTP 404) and is retried."},
		{Name: "url", Label: "URL", Type: "url",
			Help: "HTTP target: the full URL the task is sent to, http or https."},
		{Name: "relativeUri", Label: "Relative URI", Type: "text", Pattern: `^/.*$`,
			Help: "App Engine target: the path and query, beginning with /; empty is /."},
		{Name: "appEngineService", Label: "App Engine service", Type: "text",
			Help: "App Engine target: optional; empty is the default service."},
		{Name: "appEngineVersion", Label: "App Engine version", Type: "text",
			Help: "App Engine target: optional; empty is the default version."},
		{Name: "httpMethod", Label: "HTTP method", Type: "text", Required: true, Default: "POST",
			Pattern: `^(POST|GET|HEAD|PUT|DELETE|PATCH|OPTIONS)$`,
			Help:    "One of " + tasksHTTPMethods + ", in capitals."},
		{Name: "headers", Label: "Headers", Type: "map",
			Help: "Optional. One Name=value per line."},
		{Name: "body", Label: "Body", Type: "textarea",
			Help: "Optional. Sent as UTF-8 bytes. A GET, HEAD, DELETE or OPTIONS task has no body."},
		{Name: "scheduleTime", Label: "Schedule time", Type: "text",
			Help: "Optional. An RFC 3339 time such as 2026-09-27T15:04:05Z; empty dispatches it now."},
		{Name: "taskId", Label: "Task ID", Type: "text", Pattern: `^[A-Za-z0-9_\-]{1,500}$`,
			Help: "Optional. Letters, digits, hyphens and underscores; empty lets Cloud Tasks name it."},
	}, authTokenFields("")...)
}

// createTask is CreateTask with the form's HTTP request.
func (p tasksProvider) createTask(ctx context.Context, project, queue string, values map[string]string) error {
	api := p.api()
	if api == nil {
		return errors.New("Cloud Tasks has not started")
	}
	if err := tasksInProject(project, queue); err != nil {
		return err
	}
	method, ok := cloudtaskspb.HttpMethod_value[strings.ToUpper(strings.TrimSpace(values["httpMethod"]))]
	if !ok || method == int32(cloudtaskspb.HttpMethod_HTTP_METHOD_UNSPECIFIED) {
		return fmt.Errorf("HTTP method %q is not one of %s", values["httpMethod"], tasksHTTPMethods)
	}
	headers, err := console.ParseMap(values["headers"])
	if err != nil {
		return fmt.Errorf("headers: %w", err)
	}
	auth, err := parseAuthToken(values)
	if err != nil {
		return err
	}
	var task *cloudtaskspb.Task
	if values["target"] == "App Engine" {
		task = &cloudtaskspb.Task{MessageType: &cloudtaskspb.Task_AppEngineHttpRequest{AppEngineHttpRequest: &cloudtaskspb.AppEngineHttpRequest{
			HttpMethod: cloudtaskspb.HttpMethod(method), RelativeUri: strings.TrimSpace(values["relativeUri"]),
			Headers: headers, Body: []byte(values["body"]),
			AppEngineRouting: &cloudtaskspb.AppEngineRouting{
				Service: strings.TrimSpace(values["appEngineService"]), Version: strings.TrimSpace(values["appEngineVersion"])},
		}}}
		if auth.kind != "" {
			return errors.New("an App Engine task carries no OIDC or OAuth token")
		}
	} else {
		if strings.TrimSpace(values["url"]) == "" {
			return errors.New("an HTTP task needs a URL")
		}
		task = &cloudtaskspb.Task{MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: tasksFormHTTPRequest(values, method, headers, auth)}}
	}
	if id := strings.TrimSpace(values["taskId"]); id != "" {
		task.Name = queue + "/tasks/" + id
	}
	if at := strings.TrimSpace(values["scheduleTime"]); at != "" {
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return fmt.Errorf("schedule time %q is not an RFC 3339 time such as 2026-09-27T15:04:05Z", at)
		}
		task.ScheduleTime = timestamppb.New(when)
	}
	_, err = api.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: queue, Task: task})
	return err
}

// tasksFormHTTPRequest is the form's HTTP target.
func tasksFormHTTPRequest(values map[string]string, method int32, headers map[string]string, auth authToken) *cloudtaskspb.HttpRequest {
	hr := &cloudtaskspb.HttpRequest{
		Url:        strings.TrimSpace(values["url"]),
		HttpMethod: cloudtaskspb.HttpMethod(method),
		Headers:    headers,
		Body:       []byte(values["body"]),
	}
	switch auth.kind {
	case "oidc":
		hr.AuthorizationHeader = &cloudtaskspb.HttpRequest_OidcToken{OidcToken: &cloudtaskspb.OidcToken{
			ServiceAccountEmail: auth.email, Audience: auth.audience}}
	case "oauth":
		hr.AuthorizationHeader = &cloudtaskspb.HttpRequest_OauthToken{OauthToken: &cloudtaskspb.OAuthToken{
			ServiceAccountEmail: auth.email, Scope: auth.scope}}
	}
	return hr
}

// tasksInProject refuses a queue outside the project on screen: a request
// built for one project must not change another's queue.
func tasksInProject(project, queue string) error {
	if project != "" && !strings.HasPrefix(queue, "projects/"+project+"/") {
		return status.Errorf(codes.InvalidArgument, "queue %s is not in project %s", queue, project)
	}
	return nil
}

func parseFloatField(values map[string]string, key, label string, errs *[]error) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(values[key]), 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a number", label, values[key]))
	}
	return v
}

func parseInt32Field(values map[string]string, key, label string, errs *[]error) int32 {
	v, err := strconv.ParseInt(strings.TrimSpace(values[key]), 10, 32)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a whole number", label, values[key]))
	}
	return int32(v)
}

// parseDurationField reads a duration field; empty is zero, which for the
// optional max retry duration is unlimited and for a backoff is the default.
func parseDurationField(values map[string]string, key, label string, errs *[]error) time.Duration {
	raw := strings.TrimSpace(values[key])
	if raw == "" || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a duration; %s", label, values[key], tasksDurationHelp))
	}
	return d
}

var _ console.Editor = tasksProvider{}
