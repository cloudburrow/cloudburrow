package main

// Cloud Scheduler: Edit job (#795).
//
// UpdateJob is verified with the official client, and the console offered no
// way to call it: a job's schedule, time zone, target and retries could only
// be changed by deleting it and creating it again. The edit goes through the
// in-process service's own UpdateJob, the method an SDK's call reaches, so the
// console accepts what the API accepts and refuses what it refuses, with its
// message.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
)

// schedulerHTTPMethods are the methods an HTTP target takes: HttpMethod's
// values, less the unspecified one.
const schedulerHTTPMethods = "POST, GET, HEAD, PUT, DELETE, PATCH or OPTIONS"

const schedulerDurationHelp = "A duration such as 10s, 5m or 1h."

// schedulerNameImmutable is the refusal of a request that renames the job:
// UpdateJob addresses the job by its name and cannot change it.
const schedulerNameImmutable = "a job's name and region cannot be changed; create a new job instead"

// schedulerSensitiveHeader is a header the job's page redacts. Its value is
// not put in the form, where it would be on screen, and an edit keeps it.
func schedulerSensitiveHeader(k string) bool {
	return redactHeaders(map[string]string{k: ""})[k] == "[redacted]"
}

// schedulerEditBlocked says why a job cannot be edited from the form, or "".
// A body or message that is not UTF-8 text cannot be held by a text field,
// and saving the form would replace it with what the field showed.
func schedulerEditBlocked(j scheduler.Job) string {
	switch {
	case j.HTTP != nil && !utf8.Valid(j.HTTP.Body):
		return "Edit is not offered: this job's body is not UTF-8 text, which the form cannot hold. Use the API."
	case j.PubSub != nil && !utf8.Valid(j.PubSub.Data):
		return "Edit is not offered: this job's message data is not UTF-8 text, which the form cannot hold. Use the API."
	case j.AppEngine != nil && !utf8.Valid(j.AppEngine.Body):
		return "Edit is not offered: this job's body is not UTF-8 text, which the form cannot hold. Use the API."
	case j.HTTP == nil && j.PubSub == nil && j.AppEngine == nil:
		return "Edit is not offered: this job has no HTTP, Pub/Sub or App Engine target."
	}
	return ""
}

// schedulerEditForm is Edit job, prefilled from the stored job: the Create
// job form's fields, with the name, region and target type shown and fixed,
// and the target's own fields for its type only.
func schedulerEditForm(j scheduler.Job) *console.EditForm {
	i := strings.LastIndex(j.Name, "/jobs/")
	id, location := j.Name[i+len("/jobs/"):], ""
	if parts := strings.Split(j.Name[:i], "/"); len(parts) == 4 {
		location = parts[3]
	}
	r := j.Retry
	retryFor := ""
	if r.MaxRetryDuration > 0 {
		retryFor = r.MaxRetryDuration.String()
	}
	const target, retries = "Target", "Retries"
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Default: id, Immutable: true,
			Help: "A job's name cannot be changed."},
		{Name: "location", Label: "Region", Type: "text", Default: location, Immutable: true,
			Help: "A job's region is part of its name and cannot be changed."},
		{Name: "description", Label: "Description", Type: "text", Default: j.Description},
		{Name: "schedule", Label: "Frequency", Type: "text", Required: true, Default: j.Schedule,
			Help: "unix-cron format, e.g. \"0 9 * * 1\" for 09:00 every Monday."},
		{Name: "timeZone", Label: "Time zone", Type: "text", Required: true, Default: j.TimeZone,
			Help: "A tz database name such as Etc/UTC or America/New_York."},
	}
	if j.HTTP != nil {
		shown := map[string]string{}
		for k, v := range j.HTTP.Headers {
			if !schedulerSensitiveHeader(k) {
				shown[k] = v
			}
		}
		fields = append(fields,
			console.Field{Name: "targetType", Label: "Target type", Type: "text", Default: "HTTP", Immutable: true,
				Section: target, Help: "A job's target type cannot be changed from the console."},
			console.Field{Name: "uri", Label: "URL", Type: "text", Required: true, Default: j.HTTP.URI, Section: target,
				Help: "An http or https URL, reached from this machine."},
			console.Field{Name: "httpMethod", Label: "HTTP method", Type: "text", Required: true, Default: j.HTTP.Method,
				Section: target, Pattern: `^(POST|GET|HEAD|PUT|DELETE|PATCH|OPTIONS)$`,
				Help: "One of " + schedulerHTTPMethods + ", in capitals."},
			console.Field{Name: "headers", Label: "Headers", Type: "map", Default: console.FormatMap(shown), Section: target,
				Help: "Optional. One Name=value per line. Authorization and headers naming a token, secret or API key " +
					"are not shown and are kept as they are; name one here to replace its value."},
			console.Field{Name: "body", Label: "Body", Type: "textarea", Default: string(j.HTTP.Body), Section: target,
				Help: "Optional. Allowed only with POST, PUT or PATCH."},
			console.Field{Name: "attemptDeadline", Label: "Attempt deadline", Type: "text", Required: true,
				Default: j.AttemptDeadline.String(), Section: target,
				Help: "How long one attempt may take, 15s to 30m. " + schedulerDurationHelp},
		)
		var tok authToken
		switch {
		case j.HTTP.OIDCToken != nil:
			tok = authToken{kind: "oidc", email: j.HTTP.OIDCToken.ServiceAccountEmail, audience: j.HTTP.OIDCToken.Audience}
		case j.HTTP.OAuthToken != nil:
			tok = authToken{kind: "oauth", email: j.HTTP.OAuthToken.ServiceAccountEmail, scope: j.HTTP.OAuthToken.Scope}
		}
		fields = append(fields, authTokenFieldsFor(target, tok)...)
	} else if a := j.AppEngine; a != nil {
		fields = append(fields, schedulerAppEngineEditFields(a, j.AttemptDeadline, target)...)
	} else {
		fields = append(fields,
			console.Field{Name: "targetType", Label: "Target type", Type: "text", Default: "Pub/Sub", Immutable: true,
				Section: target, Help: "A job's target type cannot be changed from the console."},
			console.Field{Name: "topic", Label: "Pub/Sub topic", Type: "text", Required: true, Default: j.PubSub.Topic,
				Section: target, Help: "projects/{project}/topics/{topic}, published to the local emulator."},
			console.Field{Name: "body", Label: "Message body", Type: "textarea", Default: string(j.PubSub.Data), Section: target,
				Help: "The message data. A Pub/Sub target needs data or at least one attribute."},
			console.Field{Name: "attributes", Label: "Attributes", Type: "map", Default: console.FormatMap(j.PubSub.Attributes),
				Section: target, Help: "Optional. One key=value per line."},
		)
	}
	fields = append(fields,
		console.Field{Name: "retryCount", Label: "Max retry attempts", Type: "text", Required: true,
			Default: strconv.Itoa(r.RetryCount), Section: retries, Pattern: `^[0-5]$`,
			Help: "0 to 5. 0 retries nothing: a failed run waits for the next scheduled one."},
		console.Field{Name: "maxRetryDuration", Label: "Max retry duration", Type: "text", Default: retryFor, Section: retries,
			Help: "Optional. How long after the first attempt a run is retried; empty is unlimited. " + schedulerDurationHelp},
		console.Field{Name: "minBackoff", Label: "Min backoff duration", Type: "text", Required: true,
			Default: r.MinBackoff.String(), Section: retries, Help: schedulerDurationHelp},
		console.Field{Name: "maxBackoff", Label: "Max backoff duration", Type: "text", Required: true,
			Default: r.MaxBackoff.String(), Section: retries, Help: schedulerDurationHelp},
		console.Field{Name: "maxDoublings", Label: "Max doublings", Type: "number", Required: true,
			Default: strconv.Itoa(r.MaxDoublings), Section: retries,
			Help: "How many times the wait between retries doubles before it grows linearly. 0 sets the default, 5."},
	)
	return &console.EditForm{
		Label:  "Edit job",
		Fields: fields,
		Note: "Saved through UpdateJob; the next run follows the new schedule. The name, region and target type " +
			"cannot be changed, and the state changes with Pause and Resume.",
	}
}

// Edit implements console.Editor: UpdateJob with the form's fields, the
// update mask naming each field the form holds.
func (p schedulerProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("Cloud Scheduler edits apply to a job")
	}
	api, st := p.svc.API(), p.svc.Store()
	if api == nil || st == nil {
		return errors.New(schedulerNotStarted)
	}
	name := path[0]
	if err := schedulerJob(project, name); err != nil {
		return err
	}
	cur, err := st.Get(name)
	if err != nil {
		return err
	}
	if why := schedulerEditBlocked(cur); why != "" {
		return errors.New(why)
	}
	form := schedulerEditForm(cur)
	for _, f := range form.Fields {
		if v, ok := values[f.Name]; ok && f.Immutable && strings.TrimSpace(v) != f.Default {
			if f.Name == "targetType" {
				return errors.New("a job's target type cannot be changed from the console")
			}
			return errors.New(schedulerNameImmutable)
		}
	}

	var errs []error
	job := &schedulerpb.Job{
		Name:        name,
		Description: values["description"],
		Schedule:    strings.TrimSpace(values["schedule"]),
		TimeZone:    strings.TrimSpace(values["timeZone"]),
	}
	paths := []string{"description", "schedule", "time_zone"}
	if cur.HTTP != nil {
		method, ok := schedulerpb.HttpMethod_value[strings.ToUpper(strings.TrimSpace(values["httpMethod"]))]
		if !ok || method == int32(schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED) {
			errs = append(errs, fmt.Errorf("HTTP method %q is not one of %s", values["httpMethod"], schedulerHTTPMethods))
		}
		headers, err := console.ParseMap(values["headers"])
		if err != nil {
			errs = append(errs, fmt.Errorf("headers: %w", err))
		}
		// The redacted headers were not in the form; they are kept unless
		// the form names one.
		for k, v := range cur.HTTP.Headers {
			if _, named := headers[k]; !named && schedulerSensitiveHeader(k) {
				headers[k] = v
			}
		}
		h := &schedulerpb.HttpTarget{
			Uri:        strings.TrimSpace(values["uri"]),
			HttpMethod: schedulerpb.HttpMethod(method),
			Headers:    headers,
			Body:       []byte(values["body"]),
		}
		auth, err := parseAuthToken(values)
		if err != nil {
			errs = append(errs, err)
		}
		switch auth.kind {
		case "oidc":
			h.AuthorizationHeader = &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{
				ServiceAccountEmail: auth.email, Audience: auth.audience}}
		case "oauth":
			h.AuthorizationHeader = &schedulerpb.HttpTarget_OauthToken{OauthToken: &schedulerpb.OAuthToken{
				ServiceAccountEmail: auth.email, Scope: auth.scope}}
		}
		job.Target = &schedulerpb.Job_HttpTarget{HttpTarget: h}
		job.AttemptDeadline = durationpb.New(schedulerDuration(values, "attemptDeadline", "Attempt deadline", &errs))
		paths = append(paths, "http_target", "attempt_deadline")
	} else if cur.AppEngine != nil {
		job.Target = &schedulerpb.Job_AppEngineHttpTarget{
			AppEngineHttpTarget: schedulerAppEngineFromForm(values, cur.AppEngine, &errs)}
		job.AttemptDeadline = durationpb.New(schedulerDuration(values, "attemptDeadline", "Attempt deadline", &errs))
		paths = append(paths, "app_engine_http_target", "attempt_deadline")
	} else {
		attrs, err := console.ParseMap(values["attributes"])
		if err != nil {
			errs = append(errs, fmt.Errorf("attributes: %w", err))
		}
		job.Target = &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName:  strings.TrimSpace(values["topic"]),
			Data:       []byte(values["body"]),
			Attributes: attrs,
		}}
		paths = append(paths, "pubsub_target")
	}

	retry := scheduler.RetryConfig{
		RetryCount:       schedulerInt(values, "retryCount", "Max retry attempts", &errs),
		MaxRetryDuration: schedulerDuration(values, "maxRetryDuration", "Max retry duration", &errs),
		MinBackoff:       schedulerDuration(values, "minBackoff", "Min backoff duration", &errs),
		MaxBackoff:       schedulerDuration(values, "maxBackoff", "Max backoff duration", &errs),
		MaxDoublings:     schedulerInt(values, "maxDoublings", "Max doublings", &errs),
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// A job created with no retry_config retries by the defaults and returns
	// none; the Terraform provider plans clean against that (#591). Saving
	// the defaults the form showed is not a change, so the mask leaves
	// retry_config alone and the job keeps returning none.
	if !cur.RetryUnset || retry != scheduler.DefaultRetryConfig() {
		job.RetryConfig = &schedulerpb.RetryConfig{
			RetryCount:         int32(retry.RetryCount),
			MaxRetryDuration:   durationpb.New(retry.MaxRetryDuration),
			MinBackoffDuration: durationpb.New(retry.MinBackoff),
			MaxBackoffDuration: durationpb.New(retry.MaxBackoff),
			MaxDoublings:       int32(retry.MaxDoublings),
		}
		paths = append(paths, "retry_config")
	}

	_, err = api.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{Job: job, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	return err
}

func schedulerInt(values map[string]string, key, label string, errs *[]error) int {
	v, err := strconv.ParseInt(strings.TrimSpace(values[key]), 10, 32)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a whole number", label, values[key]))
	}
	return int(v)
}

// schedulerDuration reads a duration field; empty is zero, which for max
// retry duration is unlimited and for a backoff is the default.
func schedulerDuration(values map[string]string, key, label string, errs *[]error) time.Duration {
	raw := strings.TrimSpace(values[key])
	if raw == "" || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a duration; %s", label, values[key], schedulerDurationHelp))
	}
	return d
}

// schedulerAppEngineEditFields are an App Engine job's target on Edit job
// (#1447): the relative URI, method, headers, body and routing UpdateJob
// takes. The routing's host is output only and is shown on the job's page.
func schedulerAppEngineEditFields(a *scheduler.AppEngineTarget, deadline time.Duration, section string) []console.Field {
	shown := map[string]string{}
	for k, v := range a.Headers {
		if !schedulerSensitiveHeader(k) {
			shown[k] = v
		}
	}
	return []console.Field{
		{Name: "targetType", Label: "Target type", Type: "text", Default: "App Engine", Immutable: true,
			Section: section, Help: "A job's target type cannot be changed from the console."},
		{Name: "relativeUri", Label: "Relative URI", Type: "text", Required: true, Default: a.RelativeURI,
			Section: section, Pattern: `^/.*$`,
			Help: "The path and query, beginning with /. There is no App Engine here: each attempt goes to the " +
				"--appengine-url server standing in for the app, or, without one, fails with NOT_FOUND (HTTP 404)."},
		{Name: "httpMethod", Label: "HTTP method", Type: "text", Required: true, Default: a.Method,
			Section: section, Pattern: `^(POST|GET|HEAD|PUT|DELETE|PATCH|OPTIONS)$`,
			Help: "One of " + schedulerHTTPMethods + ", in capitals."},
		{Name: "headers", Label: "Headers", Type: "map", Default: console.FormatMap(shown), Section: section,
			Help: "Optional. One Name=value per line. Authorization and headers naming a token, secret or API key " +
				"are not shown and are kept as they are; name one here to replace its value."},
		{Name: "body", Label: "Body", Type: "textarea", Default: string(a.Body), Section: section,
			Help: "Optional. Allowed only with POST or PUT."},
		{Name: "appEngineService", Label: "App Engine service", Type: "text", Default: a.Routing.Service,
			Section: section, Help: "Optional; empty is the default service."},
		{Name: "appEngineVersion", Label: "App Engine version", Type: "text", Default: a.Routing.Version,
			Section: section, Help: "Optional; empty is the default version."},
		{Name: "appEngineInstance", Label: "App Engine instance", Type: "text", Default: a.Routing.Instance,
			Section: section, Help: "Optional; empty is any available instance."},
		{Name: "attemptDeadline", Label: "Attempt deadline", Type: "text", Required: true,
			Default: deadline.String(), Section: section,
			Help: "How long one attempt may take, 15s to 30m. " + schedulerDurationHelp},
	}
}

// schedulerAppEngineFromForm is the App Engine target Edit job sends. The
// redacted headers were not in the form; they are kept unless it names one.
func schedulerAppEngineFromForm(values map[string]string, cur *scheduler.AppEngineTarget, errs *[]error) *schedulerpb.AppEngineHttpTarget {
	method, ok := schedulerpb.HttpMethod_value[strings.ToUpper(strings.TrimSpace(values["httpMethod"]))]
	if !ok || method == int32(schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED) {
		*errs = append(*errs, fmt.Errorf("HTTP method %q is not one of %s", values["httpMethod"], schedulerHTTPMethods))
	}
	headers, err := console.ParseMap(values["headers"])
	if err != nil {
		*errs = append(*errs, fmt.Errorf("headers: %w", err))
	}
	if headers == nil {
		headers = map[string]string{}
	}
	for k, v := range cur.Headers {
		if _, named := headers[k]; !named && schedulerSensitiveHeader(k) {
			headers[k] = v
		}
	}
	return &schedulerpb.AppEngineHttpTarget{
		HttpMethod:  schedulerpb.HttpMethod(method),
		RelativeUri: strings.TrimSpace(values["relativeUri"]),
		Headers:     headers,
		Body:        []byte(values["body"]),
		AppEngineRouting: &schedulerpb.AppEngineRouting{
			Service:  strings.TrimSpace(values["appEngineService"]),
			Version:  strings.TrimSpace(values["appEngineVersion"]),
			Instance: strings.TrimSpace(values["appEngineInstance"]),
		},
	}
}

var _ console.Editor = schedulerProvider{}
