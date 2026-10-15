package tasks

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A queue's stackdriver_logging_config (#1256): with a sampling_ratio above
// 0, that fraction of dispatch attempts is written to the instance's Cloud
// Logging as Cloud Tasks writes them on Google
// (docs.cloud.google.com/tasks/docs/logging): log
// `cloudtasks.googleapis.com/task_operations_log`, monitored resource
// `cloud_tasks_queue` (labels project_id, location, queue_id, target_type),
// and a jsonPayload of `@type`
// type.googleapis.com/google.cloud.tasks.logging.v1.TaskActivityLog, `task`
// and `attemptResponseLog` (attemptDuration, dispatchCount, maxAttempts,
// responseCount, retryTime, scheduleTime, status, targetAddress,
// targetType). Google documents the log, the resource and the sampling; the
// payload's field names are those of entries Google wrote (observed, not
// documented field by field).

// LoggingConfig is a queue's stackdriver_logging_config.
type LoggingConfig struct {
	// SamplingRatio is the fraction of operations logged, 0 to 1.
	SamplingRatio float64 `json:"samplingRatio"`
}

// LogWriter writes one entry to the instance's Cloud Logging.
type LogWriter func(ctx context.Context, entry *loggingpb.LogEntry) error

// TaskOperationsLog is the log Cloud Tasks writes attempts to, unescaped.
const TaskOperationsLog = "cloudtasks.googleapis.com/task_operations_log"

const taskActivityLogType = "type.googleapis.com/google.cloud.tasks.logging.v1.TaskActivityLog"

// WithLogging writes sampled attempts to w and returns the dispatcher.
func (d *Dispatcher) WithLogging(w LogWriter) *Dispatcher {
	d.logs = w
	return d
}

// checkSamplingRatio holds sampling_ratio to its documented range.
func checkSamplingRatio(r float64) error {
	if r < 0 || r > 1 || r != r {
		return fmt.Errorf("queue.stackdriver_logging_config.sampling_ratio %v must be between 0.0 and 1.0", r)
	}
	return nil
}

// sampled reports whether an attempt on a queue with c is logged.
func sampled(c *LoggingConfig) bool {
	switch {
	case c == nil || c.SamplingRatio <= 0:
		return false
	case c.SamplingRatio >= 1:
		return true
	}
	return rand.Float64() < c.SamplingRatio
}

// attemptStatus is an attempt's outcome as a google.rpc.Code name: the
// response's HTTP status converted, or for no response, why there was none.
func attemptStatus(statusCode int, err error) codes.Code {
	if statusCode == 0 {
		if errors.Is(err, context.DeadlineExceeded) {
			return codes.DeadlineExceeded
		}
		return codes.Unavailable
	}
	if statusCode >= 200 && statusCode < 300 {
		return codes.OK
	}
	switch statusCode {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case 499:
		return codes.Canceled
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	}
	if statusCode >= 400 && statusCode < 500 {
		return codes.FailedPrecondition
	}
	return codes.Internal
}

// queueParts splits projects/P/locations/L/queues/Q.
func queueParts(name string) (project, location, queue string) {
	p := strings.Split(name, "/")
	if len(p) != 6 {
		return "", "", ""
	}
	return p[1], p[3], p[5]
}

// AttemptLogEntry is the entry Cloud Tasks writes for one attempt of task
// on q. retry is when the next attempt is due, zero for none.
func AttemptLogEntry(q Queue, task Task, statusCode int, err error, started time.Time, took time.Duration, retry time.Time) *loggingpb.LogEntry {
	project, location, queue := queueParts(q.Name)
	code := attemptStatus(statusCode, err)
	attempt := map[string]any{
		"attemptDuration": strconv.FormatFloat(took.Seconds(), 'f', -1, 64) + "s",
		"dispatchCount":   strconv.Itoa(task.DispatchCount),
		"responseCount":   strconv.Itoa(task.ResponseCount),
		"maxAttempts":     float64(q.RetryConfig.MaxAttempts),
		"scheduleTime":    task.ScheduleTime.UTC().Format(time.RFC3339Nano),
		"status":          rpccode.Code(code).String(),
		"targetType":      "HTTP",
	}
	if task.HTTPRequest != nil {
		method := task.HTTPRequest.Method
		if method == "" {
			method = http.MethodPost
		}
		attempt["targetAddress"] = method + " " + task.HTTPRequest.URL
	}
	if !retry.IsZero() {
		attempt["retryTime"] = retry.UTC().Format(time.RFC3339Nano)
	}
	payload, _ := structpb.NewStruct(map[string]any{
		"@type":              taskActivityLogType,
		"task":               task.Name,
		"attemptResponseLog": attempt,
	})
	severity := ltype.LogSeverity_INFO
	if code != codes.OK {
		severity = ltype.LogSeverity_ERROR
	}
	return &loggingpb.LogEntry{
		LogName: "projects/" + project + "/logs/" + url.PathEscape(TaskOperationsLog),
		Resource: &monitoredres.MonitoredResource{Type: "cloud_tasks_queue", Labels: map[string]string{
			"project_id": project, "location": location, "queue_id": queue, "target_type": "HTTP",
		}},
		Timestamp: timestamppb.New(started),
		Severity:  severity,
		Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
	}
}

// logAttempt writes one attempt to Cloud Logging when its queue samples it.
// A write that fails is dropped: logging never fails a dispatch.
func (d *Dispatcher) logAttempt(ctx context.Context, task Task, statusCode int, err error, started time.Time) {
	if d.logs == nil {
		return
	}
	q, qerr := d.store.GetQueue(queueOf(task.Name))
	if qerr != nil || !sampled(q.Logging) {
		return
	}
	now := d.clock.Now()
	var retry time.Time
	if statusCode < 200 || statusCode >= 300 {
		first := task.FirstAttempt
		if first.IsZero() {
			first = started
		}
		if b := Backoff(q.RetryConfig); b.ShouldRetryAfter(task.DispatchCount, now.Sub(first)) {
			retry = now.Add(b.Delay(task.DispatchCount))
		}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = d.logs(wctx, AttemptLogEntry(q, task, statusCode, err, started, now.Sub(started), retry))
}
