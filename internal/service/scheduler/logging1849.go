package scheduler

import (
	"context"
	"errors"
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

// Execution logs (#1849). "Cloud Scheduler publishes logs at the start and
// end of each job execution", entries of `@type`
// type.googleapis.com/google.cloud.scheduler.logging.AttemptStarted and
// ...AttemptFinished, a failure's carrying `debugInfo`
// (docs.cloud.google.com/scheduler/docs/troubleshooting and
// /scheduler/docs/viewing-logs, read 2026-11-20). They go to the instance's
// Cloud Logging under the monitored resource `cloud_scheduler_job` (labels
// project_id, location, job_id; docs.cloud.google.com/monitoring/api/resources), log `cloudscheduler.googleapis.com/executions`.
// Google documents the two types, debugInfo and the resource type; the log
// name and the other payload fields (jobName, scheduledTime, targetType, url,
// topicName, status) are those of entries Google writes, as remembered, not
// documented field by field.

// LogWriter writes one entry to the instance's Cloud Logging.
type LogWriter func(ctx context.Context, entry *loggingpb.LogEntry) error

// ExecutionsLog is the log the attempts are written to, unescaped.
const ExecutionsLog = "cloudscheduler.googleapis.com/executions"

const (
	attemptStartedType  = "type.googleapis.com/google.cloud.scheduler.logging.AttemptStarted"
	attemptFinishedType = "type.googleapis.com/google.cloud.scheduler.logging.AttemptFinished"
)

// WithLogging writes each attempt's start and end to w.
func (r *Runner) WithLogging(w LogWriter) *Runner { r.logs = w; return r }

// jobParts splits projects/P/locations/L/jobs/J.
func jobParts(name string) (project, location, job string) {
	p := strings.Split(name, "/")
	if len(p) != 6 {
		return "", "", ""
	}
	return p[1], p[3], p[5]
}

// logTargetType is the target's type as the entries name it.
func logTargetType(j Job) string {
	switch {
	case j.HTTP != nil:
		return "HTTP"
	case j.PubSub != nil:
		return "PUB_SUB"
	case j.AppEngine != nil:
		return "APP_ENGINE_HTTP"
	}
	return "TYPE_UNSPECIFIED"
}

// attemptPayload is the fields both entries carry.
func attemptPayload(j Job, typ string, scheduled time.Time) map[string]any {
	p := map[string]any{
		"@type":         typ,
		"jobName":       j.Name,
		"scheduledTime": scheduled.UTC().Format(time.RFC3339Nano),
		"targetType":    logTargetType(j),
	}
	switch {
	case j.HTTP != nil:
		p["url"] = j.HTTP.URI
	case j.PubSub != nil:
		p["topicName"] = j.PubSub.Topic
	case j.AppEngine != nil:
		project, _, _ := jobParts(j.Name)
		p["url"] = "https://" + AppEngineHost(project, j.AppEngine.Routing) + j.AppEngine.RelativeURI
	}
	return p
}

func attemptEntry(j Job, payload map[string]any, at time.Time, severity ltype.LogSeverity) *loggingpb.LogEntry {
	project, location, job := jobParts(j.Name)
	s, _ := structpb.NewStruct(payload)
	return &loggingpb.LogEntry{
		LogName: "projects/" + project + "/logs/" + url.PathEscape(ExecutionsLog),
		Resource: &monitoredres.MonitoredResource{Type: "cloud_scheduler_job", Labels: map[string]string{
			"project_id": project, "location": location, "job_id": job,
		}},
		Timestamp: timestamppb.New(at),
		Severity:  severity,
		Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: s},
	}
}

// AttemptStartedEntry is the entry written as an attempt of j, scheduled
// for scheduled, starts at at.
func AttemptStartedEntry(j Job, scheduled, at time.Time) *loggingpb.LogEntry {
	return attemptEntry(j, attemptPayload(j, attemptStartedType, scheduled), at, ltype.LogSeverity_INFO)
}

// AttemptFinishedEntry is the entry written as the attempt ends: an HTTP
// target's response status in httpRequest, and for a failure its status as
// a google.rpc.Code name and debugInfo, at ERROR.
func AttemptFinishedEntry(j Job, scheduled, at time.Time, statusCode int, err error) *loggingpb.LogEntry {
	payload := attemptPayload(j, attemptFinishedType, scheduled)
	severity := ltype.LogSeverity_INFO
	if err != nil {
		severity = ltype.LogSeverity_ERROR
		payload["status"] = rpccode.Code(attemptCode(statusCode, err)).String()
		info := err.Error()
		if statusCode != 0 {
			info = "URL_ERROR-ERROR_OTHER. Original HTTP response code number = " + strconv.Itoa(statusCode)
		}
		payload["debugInfo"] = info
	}
	e := attemptEntry(j, payload, at, severity)
	if statusCode != 0 {
		e.HttpRequest = &ltype.HttpRequest{Status: int32(statusCode)}
	}
	return e
}

// attemptCode is a failed attempt's outcome as a gRPC code: the response's
// HTTP status converted, as google.rpc.Code maps them, or for no response
// UNKNOWN (NOT_FOUND where no App Engine runs).
func attemptCode(statusCode int, err error) codes.Code {
	if statusCode == 0 {
		if errors.As(err, new(errNoAppEngine)) {
			return codes.NotFound
		}
		return codes.Unknown
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

// logAttempt writes e when the runner logs. A write that fails is dropped:
// logging never fails an attempt.
func (r *Runner) logAttempt(ctx context.Context, e *loggingpb.LogEntry) {
	if r.logs == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = r.logs(wctx, e)
}
