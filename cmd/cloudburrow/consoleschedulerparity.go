package main

// Cloud Scheduler console parity (#1849): the jobs list's columns and tabs,
// the checked jobs' Pause and Resume, the row menu's Force run and Edit, and
// Create job as Google's three steps, Define the schedule, Configure the
// execution and Configure optional settings
// (https://docs.cloud.google.com/scheduler/docs/creating and
// https://docs.cloud.google.com/scheduler/docs/schedule-run-cron-job, read
// 2026-11-19).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
)

// schedulerListColumns are Google's jobs list columns; the state is also the
// status icon. Status of last execution is the column Google's quickstart
// names, reading Success.
var schedulerListColumns = []string{"Region", "State", "Description", "Frequency", "Target",
	"Last run", "Status of last execution", "Next run"}

// schedulerRow is a job's columns.
func schedulerRow(j scheduler.Job) map[string]string {
	region := "—"
	if parts := strings.Split(j.Name, "/"); len(parts) >= 4 {
		region = parts[3]
	}
	return map[string]string{
		"Region":                   region,
		"State":                    schedulerStateText(j.State),
		"Description":              orDash(j.Description),
		"Frequency":                j.Schedule + " (" + j.TimeZone + ")",
		"Target":                   schedulerTargetType(j),
		"Last run":                 schedulerTime(j.LastAttemptTime),
		"Status of last execution": schedulerLastStatus(j),
		"Next run":                 schedulerNext(j),
	}
}

func schedulerStateText(s scheduler.State) string {
	switch s {
	case scheduler.StateEnabled:
		return "Enabled"
	case scheduler.StatePaused:
		return "Paused"
	}
	return string(s)
}

// schedulerLastStatus is Success for an OK attempt, Failed with the code
// otherwise, and — before the first run.
func schedulerLastStatus(j scheduler.Job) string {
	if j.LastAttemptTime.IsZero() {
		return "—"
	}
	if codes.Code(uint32(j.LastCode)) == codes.OK {
		return "Success"
	}
	return "Failed (" + codes.Code(uint32(j.LastCode)).String() + ")"
}

// schedulerListTabs are the page's tabs: the Cloud Scheduler jobs, and App
// Engine cron jobs, which a cron.yaml deployed to App Engine makes; no App
// Engine runs here.
func schedulerListTabs(l *console.Listing) {
	l.Tabs = []console.Section{
		{ID: "jobs", Label: "Scheduler jobs"},
		{ID: "cron", Label: "App Engine cron jobs",
			Note: "Not available locally: App Engine cron jobs are made by deploying a cron.yaml to App Engine, " +
				"and this instance runs no App Engine."},
	}
	l.ListTab = "jobs"
	l.Summary = []console.PropertyGroup{{Heading: "App Engine cron jobs", Properties: []console.Property{
		{Label: "App Engine", Value: "Not available locally"},
	}}}
}

// Selection actions: Google checks jobs, then the header's Pause or Resume;
// Delete is the list's own.
const (
	schedulerPauseSelected  = "pausejobs"
	schedulerResumeSelected = "resumejobs"
)

// ListActions are the checked jobs' Pause and Resume (console.ListActor).
func (p schedulerProvider) ListActions(context.Context, string) []console.Action {
	if p.svc.Store() == nil {
		return nil
	}
	field := func() []console.Field {
		return []console.Field{{Name: "jobs", Label: "Jobs", Type: "textarea", Required: true,
			Help: "The jobs checked in the list, one per line."}}
	}
	return []console.Action{
		{ID: schedulerPauseSelected, Label: "Pause", SelectionField: "jobs", Fields: field()},
		{ID: schedulerResumeSelected, Label: "Resume", SelectionField: "jobs", Fields: field()},
	}
}

func (p schedulerProvider) actOnSelection(ctx context.Context, project, action string, values map[string]string) error {
	single := map[string]string{schedulerPauseSelected: "pause", schedulerResumeSelected: "resume"}[action]
	if single == "" {
		return fmt.Errorf("unknown action %q", action)
	}
	names := strings.Fields(values["jobs"])
	if len(names) == 0 {
		return errors.New("check at least one job")
	}
	for _, name := range names {
		if err := p.act(ctx, project, name, single); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// RowEditLabel puts Edit in each job's row menu, as Google's list edits the
// checked job.
func (schedulerProvider) RowEditLabel() string { return "Edit" }

// Create job's three steps.
const (
	schedulerStepSchedule  = "Define the schedule"
	schedulerStepExecution = "Configure the execution"
	schedulerStepOptional  = "Configure optional settings"
)

// Target type choices, as Google's list words them.
const (
	schedulerTargetHTTP      = "HTTP"
	schedulerTargetPubSub    = "Pub/Sub"
	schedulerTargetAppEngine = "App Engine HTTP"
)

var schedulerMethods = []string{"POST", "GET", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS"}

// schedulerCreateFields is Create job: Google's three steps, each target's
// fields shown only for its Target type.
func schedulerCreateFields() []console.Field {
	const http = "targetType=" + schedulerTargetHTTP
	const pubsub = "targetType=" + schedulerTargetPubSub
	const ae = "targetType=" + schedulerTargetAppEngine
	d := scheduler.DefaultRetryConfig()
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Section: schedulerStepSchedule,
			Help: "Unique in the project: letters, numbers, hyphens and underscores.", Pattern: `^[A-Za-z0-9_\-]{1,500}$`},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: schedulerRegions(),
			Section: schedulerStepSchedule,
			Help:    "The regions Locations.ListLocations lists; CloudBurrow does not place resources geographically."},
		{Name: "description", Label: "Description", Type: "text", Section: schedulerStepSchedule},
		{Name: "schedule", Label: "Frequency", Type: "text", Required: true, Default: "*/5 * * * *",
			Section: schedulerStepSchedule,
			Help:    "unix-cron format, e.g. \"0 9 * * 1\" for 09:00 every Monday."},
		{Name: "timeZone", Label: "Timezone", Type: "select", Required: true, Default: "Etc/UTC",
			Options: schedulerTimeZones(), Section: schedulerStepSchedule,
			Help: "The time zone the schedule is read in. Daylight saving time can make a job run twice or not at all."},

		{Name: "targetType", Label: "Target type", Type: "select", Required: true, Default: schedulerTargetHTTP,
			Options: []string{schedulerTargetHTTP, schedulerTargetPubSub, schedulerTargetAppEngine}, Section: schedulerStepExecution,
			Help: "What the job calls: an HTTP endpoint, a Pub/Sub topic, or an App Engine app (none runs here)."},
		{Name: "uri", Label: "URL", Type: "text", ShowWhen: http, Section: schedulerStepExecution,
			Help: "An http or https URL, reached from this machine."},
		{Name: "httpMethod", Label: "HTTP method", Type: "select", Default: "POST", Options: schedulerMethods,
			ShowWhen: http, Section: schedulerStepExecution},
		{Name: "headers", Label: "HTTP headers", Type: "map", ShowWhen: http, Section: schedulerStepExecution,
			Help: "Optional. One Name=value per line."},
		{Name: "body", Label: "Body", Type: "textarea", ShowWhen: http, Section: schedulerStepExecution,
			Help: "Optional. Allowed only with POST, PUT or PATCH."},
	}
	for _, f := range authTokenFields(schedulerStepExecution) {
		f.ShowWhen = http
		fields = append(fields, f)
	}
	fields = append(fields,
		console.Field{Name: "topic", Label: "Select a Cloud Pub/Sub topic", Type: "text", ShowWhen: pubsub,
			Section: schedulerStepExecution, Help: "projects/{project}/topics/{topic}, published to the local emulator."},
		console.Field{Name: "message", Label: "Message body", Type: "textarea", ShowWhen: pubsub, Section: schedulerStepExecution,
			Help: "The message data. A Pub/Sub target needs a body or at least one attribute."},
		console.Field{Name: "attributes", Label: "Message attributes", Type: "map", ShowWhen: pubsub,
			Section: schedulerStepExecution, Help: "Optional. One key=value per line."},

		console.Field{Name: "appEngineService", Label: "Service", Type: "text", Default: "default", ShowWhen: ae,
			Section: schedulerStepExecution, Help: "The App Engine service. This instance has no App Engine: each attempt " +
				"goes to the --appengine-url server standing in for the app, or fails with NOT_FOUND (HTTP 404)."},
		console.Field{Name: "appEngineVersion", Label: "Version", Type: "text", ShowWhen: ae, Section: schedulerStepExecution,
			Help: "Optional; empty is the default version."},
		console.Field{Name: "appEngineInstance", Label: "Instance", Type: "text", ShowWhen: ae, Section: schedulerStepExecution,
			Help: "Optional; empty is any available instance."},
		console.Field{Name: "relativeUri", Label: "URL", Type: "text", Default: "/", Pattern: `^/.*$`, ShowWhen: ae,
			Section: schedulerStepExecution, Help: "The relative URL, beginning with /."},
		console.Field{Name: "aeMethod", Label: "HTTP method", Type: "select", Default: "POST", Options: schedulerMethods,
			ShowWhen: ae, Section: schedulerStepExecution},
		console.Field{Name: "aeHeaders", Label: "HTTP headers", Type: "map", ShowWhen: ae, Section: schedulerStepExecution,
			Help: "Optional. One Name=value per line."},
		console.Field{Name: "aeBody", Label: "Body", Type: "textarea", ShowWhen: ae, Section: schedulerStepExecution,
			Help: "Optional. Allowed only with POST or PUT."},

		console.Field{Name: "retryCount", Label: "Max retry attempts", Type: "text", Required: true,
			Default: strconv.Itoa(d.RetryCount), Section: schedulerStepOptional, Pattern: `^[0-5]$`,
			Help: "0 to 5. 0 retries nothing: a failed run waits for the next scheduled one."},
		console.Field{Name: "maxRetryDuration", Label: "Max retry duration", Type: "text", Section: schedulerStepOptional,
			Help: "Optional. How long after the first attempt a run is retried; empty is unlimited. " + schedulerDurationHelp},
		console.Field{Name: "minBackoff", Label: "Min backoff duration", Type: "text", Required: true,
			Default: d.MinBackoff.String(), Section: schedulerStepOptional, Help: schedulerDurationHelp},
		console.Field{Name: "maxBackoff", Label: "Max backoff duration", Type: "text", Required: true,
			Default: d.MaxBackoff.String(), Section: schedulerStepOptional, Help: schedulerDurationHelp},
		console.Field{Name: "maxDoublings", Label: "Max doublings", Type: "number", Required: true,
			Default: strconv.Itoa(d.MaxDoublings), Section: schedulerStepOptional,
			Help: "How many times the wait between retries doubles before it grows linearly."},
		console.Field{Name: "attemptDeadline", Label: "Attempt deadline config", Type: "text", Default: "3m0s",
			ShowWhen: "targetType=" + schedulerTargetHTTP + "|" + schedulerTargetAppEngine, Section: schedulerStepOptional,
			Help: "How long one attempt may take, 15s to 30m. " + schedulerDurationHelp},
	)
	return fields
}

// createJob is CreateJob with the form's schedule, target and retries.
func (p schedulerProvider) createJob(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.svc.API()
	if api == nil {
		return "", errors.New(schedulerNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a job")
	}
	values = schedulerInferTarget(values)
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
	var errs []error
	method := func(key string) schedulerpb.HttpMethod {
		v := strings.ToUpper(strings.TrimSpace(values[key]))
		if v == "" {
			v = "POST"
		}
		m, ok := schedulerpb.HttpMethod_value[v]
		if !ok || m == int32(schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED) {
			errs = append(errs, fmt.Errorf("HTTP method %q is not one of %s", values[key], schedulerHTTPMethods))
		}
		return schedulerpb.HttpMethod(m)
	}
	headers := func(key string) map[string]string {
		h, err := console.ParseMap(values[key])
		if err != nil {
			errs = append(errs, fmt.Errorf("headers: %w", err))
		}
		return h
	}
	deadline := func() {
		if strings.TrimSpace(values["attemptDeadline"]) != "" {
			job.AttemptDeadline = durationpb.New(schedulerDuration(values, "attemptDeadline", "Attempt deadline config", &errs))
		}
	}
	if values["targetType"] != schedulerTargetHTTP {
		if auth, err := parseAuthToken(values); err != nil || auth.kind != "" {
			return "", fmt.Errorf("an auth header is for an HTTP target; a Pub/Sub or App Engine target takes none")
		}
	}
	switch values["targetType"] {
	case schedulerTargetHTTP:
		auth, err := parseAuthToken(values)
		if err != nil {
			return "", err
		}
		h := &schedulerpb.HttpTarget{Uri: strings.TrimSpace(values["uri"]), HttpMethod: method("httpMethod"),
			Headers: headers("headers"), Body: []byte(values["body"])}
		switch auth.kind {
		case "oidc":
			h.AuthorizationHeader = &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{
				ServiceAccountEmail: auth.email, Audience: auth.audience}}
		case "oauth":
			h.AuthorizationHeader = &schedulerpb.HttpTarget_OauthToken{OauthToken: &schedulerpb.OAuthToken{
				ServiceAccountEmail: auth.email, Scope: auth.scope}}
		}
		job.Target = &schedulerpb.Job_HttpTarget{HttpTarget: h}
		deadline()
	case schedulerTargetPubSub:
		attrs, err := console.ParseMap(values["attributes"])
		if err != nil {
			errs = append(errs, fmt.Errorf("message attributes: %w", err))
		}
		job.Target = &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: strings.TrimSpace(values["topic"]), Data: []byte(values["message"]), Attributes: attrs}}
	case schedulerTargetAppEngine:
		job.Target = &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{
			HttpMethod: method("aeMethod"), RelativeUri: strings.TrimSpace(values["relativeUri"]),
			Headers: headers("aeHeaders"), Body: []byte(values["aeBody"]),
			AppEngineRouting: &schedulerpb.AppEngineRouting{
				Service:  strings.TrimSpace(values["appEngineService"]),
				Version:  strings.TrimSpace(values["appEngineVersion"]),
				Instance: strings.TrimSpace(values["appEngineInstance"]),
			}}}
		deadline()
	default:
		return "", fmt.Errorf("target type %q is not one of %s, %s or %s", values["targetType"],
			schedulerTargetHTTP, schedulerTargetPubSub, schedulerTargetAppEngine)
	}
	retry := scheduler.DefaultRetryConfig()
	if strings.TrimSpace(values["retryCount"]) != "" {
		retry = scheduler.RetryConfig{
			RetryCount:       schedulerInt(values, "retryCount", "Max retry attempts", &errs),
			MaxRetryDuration: schedulerDuration(values, "maxRetryDuration", "Max retry duration", &errs),
			MinBackoff:       schedulerDuration(values, "minBackoff", "Min backoff duration", &errs),
			MaxBackoff:       schedulerDuration(values, "maxBackoff", "Max backoff duration", &errs),
			MaxDoublings:     schedulerInt(values, "maxDoublings", "Max doublings", &errs),
		}
	}
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	// The defaults are sent as no retry_config, as a job created through the
	// API without one, so it reads back the same (#591).
	if retry != scheduler.DefaultRetryConfig() {
		job.RetryConfig = &schedulerpb.RetryConfig{
			RetryCount:         int32(retry.RetryCount),
			MaxRetryDuration:   durationpb.New(retry.MaxRetryDuration),
			MinBackoffDuration: durationpb.New(retry.MinBackoff),
			MaxBackoffDuration: durationpb.New(retry.MaxBackoff),
			MaxDoublings:       int32(retry.MaxDoublings),
		}
	}
	created, err := api.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: job})
	if err != nil {
		return "", err
	}
	return created.GetName(), nil
}

// schedulerInferTarget reads a request without a Target type, in the
// earlier form's field names (an HTTP URL, a Pub/Sub topic or an App Engine
// relative URI, with one body field), as the target its filled field names.
func schedulerInferTarget(in map[string]string) map[string]string {
	if strings.TrimSpace(in["targetType"]) != "" {
		return in
	}
	values := make(map[string]string, len(in)+2)
	for k, v := range in {
		values[k] = v
	}
	uri, topic := strings.TrimSpace(values["uri"]), strings.TrimSpace(values["topic"])
	ae := strings.TrimSpace(values["appEngineRelativeUri"])
	switch {
	case uri != "" && topic == "" && ae == "":
		values["targetType"] = schedulerTargetHTTP
	case topic != "" && uri == "" && ae == "":
		values["targetType"] = schedulerTargetPubSub
		if values["message"] == "" {
			values["message"] = values["body"]
		}
	case ae != "" && uri == "" && topic == "":
		values["targetType"] = schedulerTargetAppEngine
		values["relativeUri"] = ae
		if values["aeBody"] == "" {
			values["aeBody"] = values["body"]
		}
	}
	return values
}

var (
	timeZonesOnce sync.Once
	timeZones     []string
)

// schedulerTimeZones is the Timezone list: the tz database's zones as the
// host's zone.tab lists them, each one this binary can load, with Etc/UTC
// and UTC first; a short list where the host has no zone.tab.
func schedulerTimeZones() []string {
	timeZonesOnce.Do(func() {
		seen := map[string]bool{}
		var names []string
		if f, err := os.Open("/usr/share/zoneinfo/zone.tab"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "#") {
					continue
				}
				if cols := strings.Split(line, "\t"); len(cols) >= 3 {
					names = append(names, cols[2])
				}
			}
			_ = f.Close()
		}
		if len(names) == 0 {
			names = []string{"Africa/Cairo", "Africa/Johannesburg", "America/Chicago", "America/Denver",
				"America/Los_Angeles", "America/New_York", "America/Sao_Paulo", "America/Toronto", "Asia/Dubai",
				"Asia/Kolkata", "Asia/Shanghai", "Asia/Singapore", "Asia/Tokyo", "Australia/Sydney",
				"Europe/Berlin", "Europe/London", "Europe/Paris", "Pacific/Auckland"}
		}
		sort.Strings(names)
		out := []string{"Etc/UTC", "UTC"}
		seen["Etc/UTC"], seen["UTC"] = true, true
		for _, n := range names {
			if seen[n] {
				continue
			}
			if _, err := time.LoadLocation(n); err == nil {
				seen[n] = true
				out = append(out, n)
			}
		}
		timeZones = out
	})
	return append([]string(nil), timeZones...)
}

// schedulerTimeZonesWith is the Timezone list holding zone, for a job made
// through the API with one the list lacks.
func schedulerTimeZonesWith(zone string) []string {
	zones := schedulerTimeZones()
	for _, z := range zones {
		if z == zone {
			return zones
		}
	}
	return append(zones, zone)
}

var (
	_ console.ListActor = schedulerProvider{}
	_ console.RowEditor = schedulerProvider{}
)
