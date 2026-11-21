package main

// Cloud Scheduler console parity, round 3 (#1849): a job's logs. Google's
// list has "In the row of the job that you want to view the logs for, click
// Actions > View logs", which opens the Logs Explorer
// (https://docs.cloud.google.com/scheduler/docs/viewing-logs, read
// 2026-11-20); the runner writes each attempt's AttemptStarted and
// AttemptFinished to the instance's Cloud Logging
// (internal/service/scheduler/logging1849.go). The job's page has the same
// entries on its Logs tab.

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// schedulerLogFilter is the Logs Explorer query for a job's execution logs.
func schedulerLogFilter(name string) string {
	_, location, job := schedulerJobParts(name)
	return `resource.type="cloud_scheduler_job"` + "\n" +
		`resource.labels.job_id="` + job + `"` + "\n" +
		`resource.labels.location="` + location + `"`
}

// schedulerJobParts splits projects/P/locations/L/jobs/J.
func schedulerJobParts(name string) (project, location, job string) {
	p := strings.Split(name, "/")
	if len(p) != 6 {
		return "", "", ""
	}
	return p[1], p[3], p[5]
}

// schedulerViewLogsHref is the row menu's View logs: the Logs Explorer on
// the job's entries.
func schedulerViewLogsHref(name string) string {
	project, _, _ := schedulerJobParts(name)
	q := url.Values{"query": {schedulerLogFilter(name)}}
	if project != "" {
		q.Set("project", project)
	}
	return "/logs?" + q.Encode()
}

// schedulerViewLogs is the row menu's View logs action.
func schedulerViewLogs(name string) console.Action {
	return console.Action{ID: "viewlogs", Label: "View logs", Href: schedulerViewLogsHref(name)}
}

// schedulerLogsSection is the job's Logs tab: its AttemptStarted and
// AttemptFinished entries, newest first, from the instance's Cloud Logging.
func (p schedulerProvider) logsSection(ctx context.Context, name string) console.Section {
	sec := console.Section{ID: "logs", Label: "Logs",
		Note: "Each attempt's start and end, as Cloud Scheduler writes them to Cloud Logging. " +
			"View logs in the list's row menu opens them in the Logs Explorer."}
	if p.logs == nil || p.logs.api == nil {
		sec.Unavailable = "Cloud Logging is not served by this instance, so the job's execution logs are not kept."
		return sec
	}
	project, _, _ := schedulerJobParts(name)
	l := console.Listing{Columns: []string{"Severity", "Log entry", "Summary"}, NameColumn: "Time", Noun: "log entries"}
	resp, err := p.logs.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + project},
		Filter:        strings.ReplaceAll(schedulerLogFilter(name), "\n", " AND "),
		OrderBy:       "timestamp desc", PageSize: 200})
	if err != nil {
		sec.Unavailable = "cannot read the logs: " + consoleErr(err)
		return sec
	}
	for _, e := range resp.GetEntries() {
		f := e.GetJsonPayload().GetFields()
		typ := f["@type"].GetStringValue()
		typ = typ[strings.LastIndex(typ, ".")+1:]
		summary := f["url"].GetStringValue()
		if t := f["topicName"].GetStringValue(); t != "" {
			summary = t
		}
		if s := f["status"].GetStringValue(); s != "" {
			summary += ": " + s
		}
		if d := f["debugInfo"].GetStringValue(); d != "" {
			summary += " (" + d + ")"
		}
		if code := e.GetHttpRequest().GetStatus(); code != 0 && f["status"] == nil {
			summary += ": HTTP " + strconv.Itoa(int(code))
		}
		l.Items = append(l.Items, console.Resource{
			Name:   e.GetTimestamp().AsTime().UTC().Format(time.RFC3339Nano),
			Status: e.GetSeverity().String(),
			Fields: map[string]string{"Severity": e.GetSeverity().String(), "Log entry": orDash(typ), "Summary": orDash(summary)},
		})
	}
	l.Total = len(l.Items)
	sec.Listing = l
	return sec
}
