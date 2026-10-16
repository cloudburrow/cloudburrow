package scheduler

// App Engine targets (#1447): a job with app_engine_http_target.
//
// A CloudBurrow instance has no App Engine, and Google documents no mapping
// from an App Engine service to anything else, so a job is never delivered
// elsewhere. The job is stored as the API documents it, with the output-only
// AppEngineRouting.host computed as Google computes it, and every attempt
// fails as a request to an App Engine app that does not exist fails on
// Google, HTTP 404: Job.status is NOT_FOUND with the reason, and the job's
// retry_config applies as to any failed attempt. Nothing is dropped
// silently: each attempt sets last_attempt_time and status, and reaches the
// attempt observer (the console's attempt history).

import (
	"fmt"
	"strings"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AppEngineRouting mirrors google.cloud.scheduler.v1.AppEngineRouting
// without its output-only host.
type AppEngineRouting struct {
	Service  string `json:"service,omitempty"`
	Version  string `json:"version,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// AppEngineTarget is an App Engine job's request.
type AppEngineTarget struct {
	Method      string            `json:"method"`
	RelativeURI string            `json:"relativeUri"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        []byte            `json:"body,omitempty"`
	Routing     AppEngineRouting  `json:"routing"`
}

// AppEngineAttemptStatus is the HTTP status every App Engine attempt fails
// with: the 404 an appspot.com host of an app that does not exist answers.
const AppEngineAttemptStatus = 404

// AppEngineHost is the host an App Engine job of project is sent to, as
// AppEngineRouting.host documents it: <app-id>.appspot.com, the project ID
// as the app ID, prefixed by the instance, version and service named.
func AppEngineHost(project string, r AppEngineRouting) string {
	host := project + ".appspot.com"
	for _, part := range []string{r.Service, r.Version, r.Instance} {
		if part != "" {
			host = part + "." + host
		}
	}
	return host
}

// errNoAppEngine is an App Engine attempt's failure; Job.status is
// NOT_FOUND for it.
type errNoAppEngine struct{ url string }

func (e errNoAppEngine) Error() string {
	return fmt.Sprintf("HTTP %d: App Engine is not available on this CloudBurrow instance, so no app serves %s",
		AppEngineAttemptStatus, e.url)
}

func projectOf(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 2 {
		return ""
	}
	return p[1]
}

// appEngineFromProto validates app_engine_http_target against the
// contract: relative_uri "must begin with /" and empty means "/", and "A
// request body is allowed only if the HTTP method is POST or PUT".
func appEngineFromProto(t *schedulerpb.AppEngineHttpTarget) (*AppEngineTarget, error) {
	uri := t.GetRelativeUri()
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		return nil, apierror.InvalidArgument("app_engine_http_target.relative_uri %q must begin with /", uri)
	}
	method := t.GetHttpMethod()
	if method == schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED {
		method = schedulerpb.HttpMethod_POST
	}
	if len(t.GetBody()) > 0 && method != schedulerpb.HttpMethod_POST && method != schedulerpb.HttpMethod_PUT {
		return nil, apierror.InvalidArgument("app_engine_http_target.body is allowed only when http_method is POST or PUT, not %s", method)
	}
	r := t.GetAppEngineRouting()
	return &AppEngineTarget{Method: method.String(), RelativeURI: uri, Headers: t.GetHeaders(), Body: t.GetBody(),
		Routing: AppEngineRouting{Service: r.GetService(), Version: r.GetVersion(), Instance: r.GetInstance()}}, nil
}

func appEngineToProto(a *AppEngineTarget, job string) *schedulerpb.AppEngineHttpTarget {
	return &schedulerpb.AppEngineHttpTarget{
		HttpMethod: schedulerpb.HttpMethod(schedulerpb.HttpMethod_value[a.Method]), RelativeUri: a.RelativeURI,
		Headers: a.Headers, Body: a.Body,
		AppEngineRouting: &schedulerpb.AppEngineRouting{Service: a.Routing.Service, Version: a.Routing.Version,
			Instance: a.Routing.Instance, Host: AppEngineHost(projectOf(job), a.Routing)},
	}
}

// attemptAppEngine is one attempt of an App Engine job: it fails.
func attemptAppEngine(j Job) (int, error) {
	return AppEngineAttemptStatus, errNoAppEngine{url: "https://" + AppEngineHost(projectOf(j.Name), j.AppEngine.Routing) + j.AppEngine.RelativeURI}
}
