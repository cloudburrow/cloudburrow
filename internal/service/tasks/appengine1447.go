package tasks

// App Engine targets (#1447): appEngineHttpRequest on a task and
// app_engine_routing_override on a queue.
//
// A CloudBurrow instance has no App Engine, and Google documents no mapping
// from an App Engine service to anything else (a Cloud Run service of the
// same name is not one). The task and the queue's override are stored as the
// API documents them, with the output-only AppEngineRouting.host computed as
// Google computes it.
//
// With --appengine-url (#1506), a local HTTP server stands in for the app:
// each attempt is sent there, at the task's relative_uri, as Google
// documents the request an App Engine handler receives
// (tasks/docs/creating-appengine-handlers, AppEngineHttpRequest): the task's
// method, body and headers, Host the routing's host, User-Agent
// "AppEngine-Google; (+http://code.google.com/appengine)", Content-Type
// application/octet-stream when the task sets none, and X-AppEngine-QueueName,
// -TaskName, -TaskRetryCount, -TaskExecutionCount, -TaskETA and, after a
// response, -TaskPreviousResponse; a task's own X-AppEngine-* and X-Google-*
// headers are replaced, as Google replaces them. A 2xx response succeeds, any
// other is retried, as for an HTTP task. Every service and version reaches
// the same server, which can tell them apart by Host.
//
// Without it, every attempt fails as a request to an App Engine app that
// does not exist fails on Google: HTTP 404, recorded in
// last_attempt.response_status as NOT_FOUND with the reason. The queue's
// retry_config then applies, as it does to any failed attempt, and a task
// that exhausts it is deleted, as on Google. Nothing is dropped silently:
// each attempt counts in dispatch_count and response_count, reaches the
// attempt observer (the console's attempt history) and, when the queue
// samples it, Cloud Logging.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AppEngineRouting mirrors google.cloud.tasks.v2.AppEngineRouting without
// its output-only host, which is computed (AppEngineHost).
type AppEngineRouting struct {
	Service  string `json:"service,omitempty"`
	Version  string `json:"version,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// AppEngineHTTPRequest is a task's App Engine target.
type AppEngineHTTPRequest struct {
	Method      string            `json:"method"`
	RelativeURI string            `json:"relativeUri"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        []byte            `json:"body,omitempty"`
	// Routing is the task's app_engine_routing, nil when unset. A queue's
	// app_engine_routing_override wins over it.
	Routing *AppEngineRouting `json:"routing,omitempty"`
}

// Attempt mirrors google.cloud.tasks.v2.Attempt: the task's last attempt.
// Responded is zero when no response arrived, in which case the API says
// response_status is meaningless; it is then left unset.
type Attempt struct {
	Scheduled  time.Time `json:"scheduled"`
	Dispatched time.Time `json:"dispatched"`
	Responded  time.Time `json:"responded,omitzero"`
	Code       int32     `json:"code"`
	Message    string    `json:"message,omitempty"`
}

// AppEngineAttemptStatus is the HTTP status every App Engine attempt fails
// with: the 404 an appspot.com host of an app that does not exist answers.
const AppEngineAttemptStatus = 404

// AppEngineHost is the host an App Engine task of project is sent to, as
// AppEngineRouting.host documents it: the app's domain, <app-id>.appspot.com
// with the project ID as the app ID, prefixed by the instance, version and
// service the routing names.
func AppEngineHost(project string, r AppEngineRouting) string {
	host := project + ".appspot.com"
	for _, part := range []string{r.Service, r.Version, r.Instance} {
		if part != "" {
			host = part + "." + host
		}
	}
	return host
}

// effectiveRouting is the routing a task is sent with: the queue's
// override when it has one, as AppEngineHttpRequest documents, else the
// task's own.
func effectiveRouting(task *AppEngineHTTPRequest, override *AppEngineRouting) AppEngineRouting {
	if override != nil {
		return *override
	}
	if task.Routing != nil {
		return *task.Routing
	}
	return AppEngineRouting{}
}

// projectOf is the project of a queue or task name.
func projectOf(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 2 || p[0] != "projects" {
		return ""
	}
	return p[1]
}

func routingFromProto(p *taskspb.AppEngineRouting) *AppEngineRouting {
	if p == nil || (p.GetService() == "" && p.GetVersion() == "" && p.GetInstance() == "") {
		return nil
	}
	return &AppEngineRouting{Service: p.GetService(), Version: p.GetVersion(), Instance: p.GetInstance()}
}

func routingToProto(r *AppEngineRouting, project string) *taskspb.AppEngineRouting {
	if r == nil {
		return nil
	}
	return &taskspb.AppEngineRouting{Service: r.Service, Version: r.Version, Instance: r.Instance,
		Host: AppEngineHost(project, *r)}
}

// appEngineFromProto validates a task's appEngineHttpRequest against the
// contract: relative_uri "must begin with /", and a body is allowed only for
// POST and PUT ("It is an error to set a body on a task with an incompatible
// HttpMethod").
func appEngineFromProto(p *taskspb.AppEngineHttpRequest) (*AppEngineHTTPRequest, error) {
	uri := p.GetRelativeUri()
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		return nil, apierror.InvalidArgument("appEngineHttpRequest.relativeUri %q must begin with /", uri)
	}
	method := p.GetHttpMethod()
	if method == taskspb.HttpMethod_HTTP_METHOD_UNSPECIFIED {
		method = taskspb.HttpMethod_POST
	}
	if len(p.GetBody()) > 0 && method != taskspb.HttpMethod_POST && method != taskspb.HttpMethod_PUT {
		return nil, apierror.InvalidArgument("appEngineHttpRequest.body is allowed only for POST and PUT, not %s", method)
	}
	return &AppEngineHTTPRequest{Method: method.String(), RelativeURI: uri, Headers: p.GetHeaders(),
		Body: p.GetBody(), Routing: routingFromProto(p.GetAppEngineRouting())}, nil
}

// appEngineToProto is a stored App Engine target as the API returns it, its
// routing's host the one the task is sent to under the queue's override.
func appEngineToProto(a *AppEngineHTTPRequest, taskName string, override *AppEngineRouting) *taskspb.AppEngineHttpRequest {
	method := taskspb.HttpMethod_POST
	if m, ok := taskspb.HttpMethod_value[a.Method]; ok {
		method = taskspb.HttpMethod(m)
	}
	project := projectOf(taskName)
	r := effectiveRouting(a, override)
	routing := &taskspb.AppEngineRouting{Host: AppEngineHost(project, r)}
	if a.Routing != nil {
		routing.Service, routing.Version, routing.Instance = a.Routing.Service, a.Routing.Version, a.Routing.Instance
	}
	return &taskspb.AppEngineHttpRequest{HttpMethod: method, RelativeUri: a.RelativeURI, Headers: a.Headers,
		Body: a.Body, AppEngineRouting: routing}
}

// attemptsToProto sets a task's first_attempt and last_attempt. Only
// dispatch_time of the first is kept, as the API documents.
func attemptsToProto(t Task, pt *taskspb.Task) {
	if !t.FirstAttempt.IsZero() {
		pt.FirstAttempt = &taskspb.Attempt{DispatchTime: timestamppb.New(t.FirstAttempt)}
	}
	a := t.LastAttempt
	if a == nil {
		return
	}
	pt.LastAttempt = &taskspb.Attempt{ScheduleTime: timestamppb.New(a.Scheduled), DispatchTime: timestamppb.New(a.Dispatched)}
	if !a.Responded.IsZero() {
		pt.LastAttempt.ResponseTime = timestamppb.New(a.Responded)
		pt.LastAttempt.ResponseStatus = &statuspb.Status{Code: a.Code, Message: a.Message}
	}
}

// appEngineReason is why an App Engine attempt failed, for
// last_attempt.response_status.message.
func appEngineReason(task Task, host string) string {
	return fmt.Sprintf("HTTP %d: App Engine is not available on this CloudBurrow instance, so no app serves %s%s",
		AppEngineAttemptStatus, host, task.AppEngineHTTPRequest.RelativeURI)
}

// attemptAppEngine records one failed attempt of an App Engine task and
// asks for a retry, as Dispatch does for an HTTP task's non-2xx response.
func (d *Dispatcher) attemptAppEngine(task *Task, started time.Time) error {
	var override *AppEngineRouting
	if q, err := d.store.GetQueue(task.Queue); err == nil {
		override = q.AppEngineRouting
	}
	host := AppEngineHost(projectOf(task.Name), effectiveRouting(task.AppEngineHTTPRequest, override))
	reason := appEngineReason(*task, host)
	task.ResponseCount++
	task.LastResponseCode = AppEngineAttemptStatus
	task.LastAttempt = &Attempt{Scheduled: task.ScheduleTime, Dispatched: started, Responded: d.clock.Now(),
		Code: int32(codes.NotFound), Message: reason}
	_ = d.store.UpdateTask(*task)
	d.report(*task, AppEngineAttemptStatus, nil)
	return fmt.Errorf("task %s: %s", task.Name, reason)
}

// appEngineTargetAddress is the targetAddress Cloud Logging records for an
// App Engine attempt.
func appEngineTargetAddress(task Task, q Queue) string {
	a := task.AppEngineHTTPRequest
	return a.Method + " https://" + AppEngineHost(projectOf(task.Name), effectiveRouting(a, q.AppEngineRouting)) + a.RelativeURI
}

// protoTask is a task as the API returns it, its App Engine host under its
// queue's override.
func (g *GRPCServer) protoTask(t Task) *taskspb.Task {
	var override *AppEngineRouting
	if t.AppEngineHTTPRequest != nil {
		if q, err := g.store.GetQueue(t.Queue); err == nil {
			override = q.AppEngineRouting
		}
	}
	return toProtoTaskIn(t, override)
}

// WithAppEngine sends App Engine attempts to base, the local server standing
// in for the app (--appengine-url, #1506), and returns the dispatcher.
func (d *Dispatcher) WithAppEngine(base string) *Dispatcher {
	d.appEngine = strings.TrimSuffix(base, "/")
	return d
}

// AppEngineUserAgent is the User-Agent Google sends an App Engine target.
const AppEngineUserAgent = "AppEngine-Google; (+http://code.google.com/appengine)"

// appEngineRequest is an App Engine attempt as the stand-in server gets it.
func (d *Dispatcher) appEngineRequest(ctx context.Context, task Task) (*http.Request, error) {
	a := task.AppEngineHTTPRequest
	var override *AppEngineRouting
	if q, err := d.store.GetQueue(task.Queue); err == nil {
		override = q.AppEngineRouting
	}
	req, err := http.NewRequestWithContext(ctx, a.Method, d.appEngine+a.RelativeURI, bytes.NewReader(a.Body))
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", task.Name, err)
	}
	for k, v := range a.Headers {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-appengine-") || strings.HasPrefix(lk, "x-google-") {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Host = AppEngineHost(projectOf(task.Name), effectiveRouting(a, override))
	req.Header.Set("User-Agent", AppEngineUserAgent)
	if req.Header.Get("Content-Type") == "" && len(a.Body) > 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("X-AppEngine-QueueName", task.Queue[strings.LastIndex(task.Queue, "/")+1:])
	req.Header.Set("X-AppEngine-TaskName", task.Name[strings.LastIndex(task.Name, "/")+1:])
	req.Header.Set("X-AppEngine-TaskRetryCount", strconv.Itoa(task.DispatchCount))
	req.Header.Set("X-AppEngine-TaskExecutionCount", strconv.Itoa(task.ResponseCount))
	req.Header.Set("X-AppEngine-TaskETA", strconv.FormatFloat(float64(task.ScheduleTime.UnixMicro())/1e6, 'f', 6, 64))
	if task.LastResponseCode != 0 {
		req.Header.Set("X-AppEngine-TaskPreviousResponse", strconv.Itoa(task.LastResponseCode))
	}
	return req, nil
}
