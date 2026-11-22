package main

// Cloud Monitoring's 2026-11-22 console round (#2146), from Google's
// alerts/using-alerting-ui, alerts/manage-alerts, alerts/incidents-events,
// alerts/manage-snooze, uptime-checks, support/notification-options,
// groups and charts/dashboards pages (read 2026-11-22): Create policy in
// Google's three steps and words, Copy on a policy, Alerts for incidents
// with Close alert, Create uptime check in four steps with its alert
// policy, each channel type's own fields and Send test notification, a
// group's criteria, Copy dashboard, and Create snooze's start time and
// policies.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// Create policy's steps, as Google's page numbers them.
const (
	monStepSeries  = "Select the time series"
	monStepTrigger = "Configure trigger"
	monStepNotify  = "Notifications and name"
)

var (
	monRollingWindows = []string{"60", "120", "300", "600", "900", "1800", "3600", "21600", "86400"}
	monRetestWindows  = []string{"0", "60", "120", "180", "300", "600", "900", "1800", "3600", "86400"}
	monAutoClose      = []string{"1800", "3600", "21600", "43200", "86400", "259200", "604800"}
)

// humanSeconds is a duration as Google's selects name it: 5 min, 1 hr,
// 7 days.
func humanSeconds(n int) string {
	switch {
	case n%86400 == 0:
		if n == 86400 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n/86400)
	case n%3600 == 0:
		return fmt.Sprintf("%d hr", n/3600)
	case n%60 == 0:
		return fmt.Sprintf("%d min", n/60)
	}
	return fmt.Sprintf("%d sec", n)
}

// secondsLabels labels a select of seconds; zero, when given, names 0.
func secondsLabels(opts []string, zero string) map[string]string {
	out := map[string]string{}
	for _, o := range opts {
		n, _ := strconv.Atoi(o)
		if n == 0 && zero != "" {
			out[o] = zero
			continue
		}
		out[o] = humanSeconds(n)
	}
	return out
}

// enumLabels names an aligner or reducer as Google's selects do: mean,
// sum, 99th percentile.
func enumLabels(opts []string, prefix string) map[string]string {
	out := map[string]string{}
	for _, o := range opts {
		v := strings.ToLower(strings.TrimPrefix(o, prefix))
		if p, ok := strings.CutPrefix(v, "percentile_"); ok {
			v = p + "th percentile"
		}
		out[o] = strings.ReplaceAll(v, "_", " ")
	}
	return out
}

var policyComparisonLabels = map[string]string{
	"COMPARISON_GT": "Above threshold", "COMPARISON_LT": "Below threshold",
	"COMPARISON_GE": "Above or equal to threshold", "COMPARISON_LE": "Below or equal to threshold",
	"COMPARISON_EQ": "Equal to threshold", "COMPARISON_NE": "Not equal to threshold",
}

var policyMissingData = []string{"", "EVALUATION_MISSING_DATA_INACTIVE", "EVALUATION_MISSING_DATA_ACTIVE", "EVALUATION_MISSING_DATA_NO_OP"}

// policyFormFields are Create policy's fields in Google's steps.
func policyFormFields() []console.Field {
	metricKinds := "conditionType=threshold|absence"
	return []console.Field{
		{Name: "conditionType", Label: "Condition type", Type: "select", Options: []string{"threshold", "absence", "log-match", "promql"}, Default: "threshold",
			Section: monStepSeries, OptionLabels: map[string]string{"threshold": "Metric threshold", "absence": "Metric absence", "log-match": "Log match", "promql": "PromQL"},
			Help: "Metric threshold, metric absence, a match on log entries, or a PromQL query (the code editor's PromQL)."},
		{Name: "metric", Label: "Select a metric", Type: "text", Section: monStepSeries, ShowWhen: metricKinds, Help: "The metric type, such as custom.googleapis.com/shop/sales."},
		{Name: "resource", Label: "Resource type", Type: "text", Section: monStepSeries, ShowWhen: metricKinds, Help: "Such as global; empty for any."},
		{Name: "filter", Label: "Add filter", Type: "text", Section: monStepSeries, ShowWhen: metricKinds, Help: `Extra terms, such as metric.labels.env = "prod".`},
		{Name: "alignmentPeriod", Label: "Rolling window", Type: "select", Options: monRollingWindows, OptionLabels: secondsLabels(monRollingWindows, ""), Default: "60",
			Section: monStepSeries, ShowWhen: metricKinds},
		{Name: "aligner", Label: "Rolling window function", Type: "select", Options: explorerAligners, OptionLabels: enumLabels(explorerAligners, "ALIGN_"), Default: "ALIGN_NONE",
			Section: monStepSeries, ShowWhen: metricKinds, Help: "How each time series' points in the rolling window are combined; none compares the raw points."},
		{Name: "reducer", Label: "Time series aggregation", Type: "select", Options: explorerReducers, OptionLabels: enumLabels(explorerReducers, "REDUCE_"), Default: "REDUCE_NONE",
			Section: monStepSeries, ShowWhen: metricKinds},
		{Name: "groupBy", Label: "Time series group by", Type: "text", Section: monStepSeries, ShowWhen: metricKinds,
			Help: "Labels the aggregation keeps, comma separated, such as metric.labels.env."},
		{Name: "promqlQuery", Label: "PromQL query", Type: "textarea", Section: monStepSeries, ShowWhen: "conditionType=promql",
			Help: `The condition is met for each series the query returns, such as {"custom.googleapis.com/shop/backlog"} > 5.`},
		{Name: "evaluationInterval", Label: "Evaluation interval (seconds)", Type: "number", Default: "30", Section: monStepSeries,
			ShowWhen: "conditionType=promql", Help: "How often the query is evaluated: a multiple of 30 seconds."},
		{Name: "logFilter", Label: "Log query", Type: "text", Section: monStepSeries, ShowWhen: "conditionType=log-match",
			Help: `The log entries to alert on, a Logging query such as severity>=ERROR.`},

		{Name: "conditionName", Label: "Condition name", Type: "text", Section: monStepTrigger, Default: "Condition"},
		{Name: "trigger", Label: "Alert trigger", Type: "select", Options: []string{"any", "percent", "count", "all"}, Default: "any", Section: monStepTrigger,
			ShowWhen: metricKinds, OptionLabels: map[string]string{"any": "Any time series violates", "percent": "Percent of time series violates",
				"count": "Number of time series violates", "all": "All time series violate"}},
		{Name: "triggerPercent", Label: "Percent of time series", Type: "number", Default: "50", Section: monStepTrigger, ShowWhen: metricKinds + "&trigger=percent"},
		{Name: "triggerCount", Label: "Number of time series", Type: "number", Default: "1", Section: monStepTrigger, ShowWhen: metricKinds + "&trigger=count"},
		{Name: "comparison", Label: "Threshold position", Type: "select", Options: policyComparisons, OptionLabels: policyComparisonLabels, Default: "COMPARISON_GT",
			Section: monStepTrigger, ShowWhen: "conditionType=threshold"},
		{Name: "threshold", Label: "Threshold value", Type: "number", Default: "0", Section: monStepTrigger, ShowWhen: "conditionType=threshold"},
		{Name: "duration", Label: "Retest window", Type: "select", Options: monRetestWindows, OptionLabels: secondsLabels(monRetestWindows, "No retest"), Default: "0",
			Section: monStepTrigger, ShowWhen: "conditionType=threshold|absence|promql",
			Help: "How long the condition must hold before an alert opens; a metric absence needs at least 2 min."},
		{Name: "missingData", Label: "Evaluation missing data", Type: "select", Options: policyMissingData, Section: monStepTrigger, ShowWhen: "conditionType=threshold",
			OptionLabels: map[string]string{"": "Not set",
				"EVALUATION_MISSING_DATA_INACTIVE": "Missing data points treated as values that don't violate the policy condition",
				"EVALUATION_MISSING_DATA_ACTIVE":   "Missing data points treated as values that violate the policy condition",
				"EVALUATION_MISSING_DATA_NO_OP":    "Missing data points are ignored: the condition keeps its state"}},
		{Name: "rateLimit", Label: "Time between notifications (seconds)", Type: "number", Default: "300", Section: monStepTrigger, ShowWhen: "conditionType=log-match",
			Help: "At most one notification per period."},

		{Name: "channels", Label: "Notification channels", Type: "text", Section: monStepNotify,
			Help: "Channel IDs from Edit notification channels, comma separated."},
		{Name: "subject", Label: "Notification subject line", Type: "text", Section: monStepNotify,
			Help: "At most 255 characters; empty for Google's default subject."},
		{Name: "notifyOnClose", Label: "Notify on alert closure", Type: "checkbox", Default: "true", Section: monStepNotify, ShowWhen: "conditionType=threshold|absence|promql"},
		{Name: "autoClose", Label: "Alert autoclose duration", Type: "select", Options: monAutoClose, OptionLabels: secondsLabels(monAutoClose, ""), Default: "604800",
			Section: monStepNotify, Help: "An open alert with no data for this long is closed."},
		{Name: "renotify", Label: "Repeat notifications every (minutes)", Type: "number", Section: monStepNotify, ShowWhen: "conditionType=threshold|absence|promql",
			Help: "Empty to notify once. From 30 to 1440 minutes: an open alert's notification is sent again to the channels above."},
		{Name: "severity", Label: "Policy severity level", Type: "select", Options: []string{"SEVERITY_UNSPECIFIED", "CRITICAL", "ERROR", "WARNING"},
			OptionLabels: map[string]string{"SEVERITY_UNSPECIFIED": "No severity", "CRITICAL": "Critical", "ERROR": "Error", "WARNING": "Warning"},
			Default:      "SEVERITY_UNSPECIFIED", Section: monStepNotify},
		{Name: "userLabels", Label: "Policy user labels", Type: "map", Section: monStepNotify, Help: "Sent with each notification as policy_user_labels."},
		{Name: "documentation", Label: "Documentation", Type: "textarea", Section: monStepNotify, Help: "Markdown sent with each notification."},
		{Name: "displayName", Label: "Alert policy name", Type: "text", Required: true, Section: monStepNotify},
	}
}

// policyTrigger is the Alert trigger chosen; nil is Any time series
// violates, the API's default.
func policyTrigger(values map[string]string) (*monitoringpb.AlertPolicy_Condition_Trigger, error) {
	switch values["trigger"] {
	case "", "any":
		return nil, nil
	case "all":
		return &monitoringpb.AlertPolicy_Condition_Trigger{Type: &monitoringpb.AlertPolicy_Condition_Trigger_Percent{Percent: 100}}, nil
	case "percent":
		p, err := strconv.ParseFloat(strings.TrimSpace(values["triggerPercent"]), 64)
		if err != nil || p <= 0 || p > 100 {
			return nil, fmt.Errorf("percent of time series %q is not from 0 to 100", values["triggerPercent"])
		}
		return &monitoringpb.AlertPolicy_Condition_Trigger{Type: &monitoringpb.AlertPolicy_Condition_Trigger_Percent{Percent: p}}, nil
	case "count":
		n, err := strconv.Atoi(strings.TrimSpace(values["triggerCount"]))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("number of time series %q is not a whole number of at least 1", values["triggerCount"])
		}
		return &monitoringpb.AlertPolicy_Condition_Trigger{Type: &monitoringpb.AlertPolicy_Condition_Trigger_Count{Count: int32(n)}}, nil
	}
	return nil, fmt.Errorf("alert trigger %q is not any, percent, count or all", values["trigger"])
}

// applyPolicyGoogle sets what Create policy's Google steps add to the
// condition and policy policyFromForm made: group by, trigger, missing
// data, subject line, closure notifications, autoclose and user labels.
func applyPolicyGoogle(pol *monitoringpb.AlertPolicy, values map[string]string) error {
	cond := pol.GetConditions()[0]
	trig, err := policyTrigger(values)
	if err != nil {
		return err
	}
	var aggs []*monitoringpb.Aggregation
	if t := cond.GetConditionThreshold(); t != nil {
		t.Trigger, aggs = trig, t.GetAggregations()
		if v, ok := monitoringpb.AlertPolicy_Condition_EvaluationMissingData_value[values["missingData"]]; ok {
			t.EvaluationMissingData = monitoringpb.AlertPolicy_Condition_EvaluationMissingData(v)
		}
	}
	if a := cond.GetConditionAbsent(); a != nil {
		a.Trigger, aggs = trig, a.GetAggregations()
	}
	if gb := splitList(values["groupBy"]); len(gb) > 0 {
		if len(aggs) == 0 {
			return errors.New("time series group by needs a rolling window function other than none")
		}
		aggs[0].GroupByFields = gb
	}
	if s := strings.TrimSpace(values["subject"]); s != "" {
		if len([]rune(s)) > 255 {
			return errors.New("the notification subject line is longer than 255 characters")
		}
		if pol.Documentation == nil {
			pol.Documentation = &monitoringpb.AlertPolicy_Documentation{MimeType: "text/markdown"}
		}
		pol.Documentation.Subject = s
	}
	strategy := func() *monitoringpb.AlertPolicy_AlertStrategy {
		if pol.AlertStrategy == nil {
			pol.AlertStrategy = &monitoringpb.AlertPolicy_AlertStrategy{}
		}
		return pol.AlertStrategy
	}
	if v, ok := values["notifyOnClose"]; ok && cond.GetConditionMatchedLog() == nil {
		prompts := []monitoringpb.AlertPolicy_AlertStrategy_NotificationPrompt{monitoringpb.AlertPolicy_AlertStrategy_OPENED}
		if v == "true" {
			prompts = append(prompts, monitoringpb.AlertPolicy_AlertStrategy_CLOSED)
		}
		strategy().NotificationPrompts = prompts
	}
	if v := strings.TrimSpace(values["autoClose"]); v != "" {
		d, err := seconds("alert autoclose duration", v, 604800)
		if err != nil {
			return err
		}
		strategy().AutoClose = durationpb.New(d)
	}
	if v := strings.TrimSpace(values["userLabels"]); v != "" {
		m, err := console.ParseMap(v)
		if err != nil {
			return fmt.Errorf("policy user labels: %v", err)
		}
		pol.UserLabels = m
	}
	return nil
}

// CreateTitle and CreateSubmit are Google's: the page is Create alerting
// policy and ends in Create policy.
func (monitoringAlertingProvider) CreateTitle() string  { return "Create alerting policy" }
func (monitoringAlertingProvider) CreateSubmit() string { return "Create policy" }

var consoleFilterShape = regexp.MustCompile(`^metric\.type = "([^"\\]*)"(?: AND resource\.type = "([^"\\]*)")?(?: AND \((.*)\))?$`)

// CreateFormFrom implements console.CreateCopier: a policy's Copy opens
// Create policy holding its settings, named "Copy of" it, as Google's
// Policies list's Copy does.
func (p monitoringAlertingProvider) CreateFormFrom(ctx context.Context, project, from string) ([]console.Field, error) {
	s, err := p.server()
	if err != nil {
		return nil, err
	}
	pol, err := s.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + lastSegment(from)})
	if err != nil {
		return nil, err
	}
	if len(pol.GetConditions()) != 1 {
		return nil, fmt.Errorf("policy %s has %d conditions; the console copies a policy of one condition", from, len(pol.GetConditions()))
	}
	v := map[string]string{"displayName": "Copy of " + pol.GetDisplayName(), "severity": pol.GetSeverity().String(),
		"documentation": pol.GetDocumentation().GetContent(), "subject": pol.GetDocumentation().GetSubject(),
		"userLabels": console.FormatMap(pol.GetUserLabels()), "renotify": renotifyMinutes(pol)}
	var chans []string
	for _, c := range pol.GetNotificationChannels() {
		chans = append(chans, lastSegment(c))
	}
	v["channels"] = strings.Join(chans, ", ")
	if ac := pol.GetAlertStrategy().GetAutoClose(); ac != nil {
		v["autoClose"] = strconv.Itoa(int(ac.AsDuration().Seconds()))
	}
	if pr := pol.GetAlertStrategy().GetNotificationPrompts(); len(pr) > 0 {
		v["notifyOnClose"] = "false"
		for _, x := range pr {
			if x == monitoringpb.AlertPolicy_AlertStrategy_CLOSED {
				v["notifyOnClose"] = "true"
			}
		}
	}
	c := pol.GetConditions()[0]
	v["conditionName"] = c.GetDisplayName()
	series := func(filter string, aggs []*monitoringpb.Aggregation, trig *monitoringpb.AlertPolicy_Condition_Trigger, dur *durationpb.Duration) error {
		m := consoleFilterShape.FindStringSubmatch(filter)
		if m == nil {
			return fmt.Errorf("policy %s's filter was not written by Create policy; copy it through the API", from)
		}
		v["metric"], v["resource"], v["filter"] = m[1], m[2], m[3]
		if len(aggs) > 0 {
			v["aligner"], v["reducer"] = aggs[0].GetPerSeriesAligner().String(), aggs[0].GetCrossSeriesReducer().String()
			v["alignmentPeriod"] = strconv.Itoa(int(aggs[0].GetAlignmentPeriod().AsDuration().Seconds()))
			v["groupBy"] = strings.Join(aggs[0].GetGroupByFields(), ", ")
		}
		switch {
		case trig.GetPercent() == 100:
			v["trigger"] = "all"
		case trig.GetPercent() > 0:
			v["trigger"], v["triggerPercent"] = "percent", strconv.FormatFloat(trig.GetPercent(), 'f', -1, 64)
		case trig.GetCount() > 1:
			v["trigger"], v["triggerCount"] = "count", strconv.Itoa(int(trig.GetCount()))
		}
		v["duration"] = strconv.Itoa(int(dur.AsDuration().Seconds()))
		return nil
	}
	switch {
	case c.GetConditionThreshold() != nil:
		t := c.GetConditionThreshold()
		v["conditionType"], v["comparison"] = "threshold", t.GetComparison().String()
		v["threshold"] = strconv.FormatFloat(t.GetThresholdValue(), 'f', -1, 64)
		if t.GetEvaluationMissingData() != monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_UNSPECIFIED {
			v["missingData"] = t.GetEvaluationMissingData().String()
		}
		if err := series(t.GetFilter(), t.GetAggregations(), t.GetTrigger(), t.GetDuration()); err != nil {
			return nil, err
		}
	case c.GetConditionAbsent() != nil:
		a := c.GetConditionAbsent()
		v["conditionType"] = "absence"
		if err := series(a.GetFilter(), a.GetAggregations(), a.GetTrigger(), a.GetDuration()); err != nil {
			return nil, err
		}
	case c.GetConditionPrometheusQueryLanguage() != nil:
		q := c.GetConditionPrometheusQueryLanguage()
		v["conditionType"], v["promqlQuery"] = "promql", q.GetQuery()
		v["evaluationInterval"] = strconv.Itoa(int(q.GetEvaluationInterval().AsDuration().Seconds()))
		v["duration"] = strconv.Itoa(int(q.GetDuration().AsDuration().Seconds()))
	case c.GetConditionMatchedLog() != nil:
		v["conditionType"], v["logFilter"] = "log-match", c.GetConditionMatchedLog().GetFilter()
		if r := pol.GetAlertStrategy().GetNotificationRateLimit().GetPeriod(); r != nil {
			v["rateLimit"] = strconv.Itoa(int(r.AsDuration().Seconds()))
		}
	default:
		return nil, fmt.Errorf("policy %s's condition type cannot be copied in the console", from)
	}
	fields := policyFormFields()
	for i := range fields {
		if d, ok := v[fields[i].Name]; ok {
			fields[i].Default = d
		}
	}
	return fields, nil
}

// ---- Uptime checks: Google's four steps ----

const (
	upStepTarget   = "Target"
	upStepResponse = "Response validation"
	upStepAlert    = "Alert & notification"
	upStepFinal    = "Finalize"
)

var (
	uptimePeriodOptions = []string{"60", "300", "600", "900"}
	uptimeRegionOptions = []string{"USA_OREGON", "USA_IOWA", "USA_VIRGINIA", "EUROPE", "SOUTH_AMERICA", "ASIA_PACIFIC"}
	uptimeRegionLabels  = map[string]string{"USA_OREGON": "USA (Oregon)", "USA_IOWA": "USA (Iowa)", "USA_VIRGINIA": "USA (Virginia)",
		"EUROPE": "Europe", "SOUTH_AMERICA": "South America", "ASIA_PACIFIC": "Asia Pacific"}
	uptimeMatcherOptions = []string{"CONTAINS_STRING", "NOT_CONTAINS_STRING", "MATCHES_REGEX", "NOT_MATCHES_REGEX", "MATCHES_JSON_PATH", "NOT_MATCHES_JSON_PATH"}
	uptimeMatcherLabels  = map[string]string{"CONTAINS_STRING": "Contains", "NOT_CONTAINS_STRING": "Does not contain",
		"MATCHES_REGEX": "Matches regex", "NOT_MATCHES_REGEX": "Does not match regex",
		"MATCHES_JSON_PATH": "Matches JSONPath", "NOT_MATCHES_JSON_PATH": "Does not match JSONPath"}
	uptimeCodeClasses = []string{"STATUS_CLASS_2XX", "STATUS_CLASS_3XX", "STATUS_CLASS_4XX", "STATUS_CLASS_5XX", "STATUS_CLASS_ANY"}
)

// uptimeFormFields are Create uptime check's fields in Google's steps.
func uptimeFormFields() []console.Field {
	http := "protocol=HTTP|HTTPS"
	return []console.Field{
		{Name: "protocol", Label: "Protocol", Type: "select", Options: []string{"HTTP", "HTTPS", "TCP"}, Default: "HTTP", Section: upStepTarget},
		{Name: "resourceType", Label: "Resource type", Type: "select", Options: []string{"uptime_url"}, OptionLabels: map[string]string{"uptime_url": "URL"},
			Default: "uptime_url", Section: upStepTarget, Help: "A URL: CloudBurrow checks hosts on this machine only."},
		{Name: "host", Label: "Hostname", Type: "text", Required: true, Section: upStepTarget, Help: "A host on this machine, such as localhost or 127.0.0.1."},
		{Name: "port", Label: "Port", Type: "number", Section: upStepTarget, Help: "Empty for 80 (HTTP) or 443 (HTTPS); required for TCP."},
		{Name: "path", Label: "Path", Type: "text", Default: "/", Section: upStepTarget, ShowWhen: http},
		{Name: "period", Label: "Check frequency", Type: "select", Options: uptimePeriodOptions, OptionLabels: secondsLabels(uptimePeriodOptions, ""), Default: "60", Section: upStepTarget},
		{Name: "regions", Label: "Regions", Type: "text", Section: upStepTarget,
			Help: "More target options: empty for Global (every region), or at least 3 of " + strings.Join(uptimeRegionOptions, ", ") + ", comma separated."},
		{Name: "requestMethod", Label: "Request method", Type: "select", Options: []string{"GET", "POST"}, Default: "GET", Section: upStepTarget, ShowWhen: http},
		{Name: "contentType", Label: "Content type", Type: "select", Options: []string{"TYPE_UNSPECIFIED", "URL_ENCODED", "USER_PROVIDED"},
			OptionLabels: map[string]string{"TYPE_UNSPECIFIED": "Not set", "URL_ENCODED": "URL encoded", "USER_PROVIDED": "Custom"},
			Default:      "TYPE_UNSPECIFIED", Section: upStepTarget, ShowWhen: http + "&requestMethod=POST"},
		{Name: "body", Label: "Request body", Type: "textarea", Section: upStepTarget, ShowWhen: http + "&requestMethod=POST"},
		{Name: "headers", Label: "Custom headers", Type: "map", Section: upStepTarget, ShowWhen: http, Help: "Sent with each request, such as Host."},
		{Name: "authUser", Label: "Authentication: user name", Type: "text", Section: upStepTarget, ShowWhen: http, Help: "Basic authentication, optional."},
		{Name: "authPassword", Label: "Authentication: password", Type: "password", Section: upStepTarget, ShowWhen: http},
		{Name: "validateSsl", Label: "Validate SSL certificates", Type: "checkbox", Section: upStepTarget, ShowWhen: "protocol=HTTPS"},

		{Name: "timeout", Label: "Response timeout (seconds)", Type: "number", Default: "10", Section: upStepResponse, Help: "From 1 to 60 seconds."},
		{Name: "contentMatching", Label: "Content matching is enabled", Type: "checkbox", Section: upStepResponse},
		{Name: "matcher", Label: "Response content match type", Type: "select", Options: uptimeMatcherOptions, OptionLabels: uptimeMatcherLabels,
			Default: "CONTAINS_STRING", Section: upStepResponse, ShowWhen: "contentMatching"},
		{Name: "content", Label: "Response content", Type: "text", Section: upStepResponse, ShowWhen: "contentMatching"},
		{Name: "acceptedCodes", Label: "Acceptable HTTP response codes", Type: "select", Options: uptimeCodeClasses, Default: "STATUS_CLASS_2XX",
			OptionLabels: map[string]string{"STATUS_CLASS_2XX": "Response codes 2xx", "STATUS_CLASS_3XX": "Response codes 3xx", "STATUS_CLASS_4XX": "Response codes 4xx",
				"STATUS_CLASS_5XX": "Response codes 5xx", "STATUS_CLASS_ANY": "Any response code"}, Section: upStepResponse, ShowWhen: http},

		{Name: "createAlert", Label: "Create an alert", Type: "checkbox", Default: "true", Section: upStepAlert,
			Help: "Creates an alerting policy that opens an alert when the check fails."},
		{Name: "alertName", Label: "Alert name", Type: "text", Section: upStepAlert, ShowWhen: "createAlert", Help: "Empty for \"<Title> uptime failure\"."},
		{Name: "alertDuration", Label: "Duration", Type: "select", Options: []string{"60", "120", "300", "600", "900"},
			OptionLabels: secondsLabels([]string{"60", "120", "300", "600", "900"}, ""), Default: "60", Section: upStepAlert, ShowWhen: "createAlert"},
		{Name: "alertChannels", Label: "Notification channels", Type: "text", Section: upStepAlert, ShowWhen: "createAlert", Help: "Channel IDs, comma separated."},

		{Name: "displayName", Label: "Title", Type: "text", Required: true, Section: upStepFinal},
		{Name: "userLabels", Label: "User labels", Type: "map", Section: upStepFinal},
	}
}

// uptimeGoogleFromForm sets what Create uptime check's Google steps add to
// what uptimeFromForm read.
func uptimeGoogleFromForm(values map[string]string, c *monitoringpb.UptimeCheckConfig) error {
	c.SelectedRegions = nil
	for _, r := range splitList(values["regions"]) {
		v, ok := monitoringpb.UptimeCheckRegion_value[strings.ToUpper(r)]
		if !ok || v == 0 {
			return fmt.Errorf("region %q is not one of %s", r, strings.Join(uptimeRegionOptions, ", "))
		}
		c.SelectedRegions = append(c.SelectedRegions, monitoringpb.UptimeCheckRegion(v))
	}
	if values["contentMatching"] == "true" {
		content := values["content"]
		if content == "" {
			return errors.New("content matching needs the response content")
		}
		m, ok := monitoringpb.UptimeCheckConfig_ContentMatcher_ContentMatcherOption_value[values["matcher"]]
		if !ok {
			m = int32(monitoringpb.UptimeCheckConfig_ContentMatcher_CONTAINS_STRING)
		}
		c.ContentMatchers = []*monitoringpb.UptimeCheckConfig_ContentMatcher{{Content: content,
			Matcher: monitoringpb.UptimeCheckConfig_ContentMatcher_ContentMatcherOption(m)}}
	}
	if v := strings.TrimSpace(values["userLabels"]); v != "" {
		m, err := console.ParseMap(v)
		if err != nil {
			return fmt.Errorf("user labels: %v", err)
		}
		c.UserLabels = m
	}
	h := c.GetHttpCheck()
	if h == nil {
		return nil
	}
	if values["requestMethod"] == "POST" {
		h.RequestMethod = monitoringpb.UptimeCheckConfig_HttpCheck_POST
		h.Body = []byte(values["body"])
		if ct, ok := monitoringpb.UptimeCheckConfig_HttpCheck_ContentType_value[values["contentType"]]; ok {
			h.ContentType = monitoringpb.UptimeCheckConfig_HttpCheck_ContentType(ct)
		}
	} else if values["requestMethod"] == "GET" {
		h.RequestMethod = monitoringpb.UptimeCheckConfig_HttpCheck_GET
	}
	if v := strings.TrimSpace(values["headers"]); v != "" {
		m, err := console.ParseMap(v)
		if err != nil {
			return fmt.Errorf("custom headers: %v", err)
		}
		h.Headers = m
	}
	if u := strings.TrimSpace(values["authUser"]); u != "" {
		h.AuthInfo = &monitoringpb.UptimeCheckConfig_HttpCheck_BasicAuthentication{Username: u, Password: values["authPassword"]}
	}
	h.ValidateSsl = h.GetUseSsl() && values["validateSsl"] == "true"
	if cls, ok := monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_StatusClass_value[values["acceptedCodes"]]; ok &&
		cls != int32(monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_STATUS_CLASS_2XX) {
		h.AcceptedResponseStatusCodes = []*monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode{{
			StatusCode: &monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_StatusClass_{
				StatusClass: monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_StatusClass(cls)}}}
	}
	return nil
}

// uptimeAlertPolicy is the policy Create an alert makes, as Google's: the
// check's check_passed counted false over 20 minutes per checked
// resource, above 1 for the duration chosen.
func uptimeAlertPolicy(project string, check *monitoringpb.UptimeCheckConfig, values map[string]string) (*monitoringpb.AlertPolicy, error) {
	name := strings.TrimSpace(values["alertName"])
	if name == "" {
		name = check.GetDisplayName() + " uptime failure"
	}
	dur, err := seconds("duration", values["alertDuration"], 60)
	if err != nil {
		return nil, err
	}
	pol := &monitoringpb.AlertPolicy{DisplayName: name, Combiner: monitoringpb.AlertPolicy_OR, Enabled: wrapperspb.Bool(true),
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "Failure of uptime check_id " + lastSegment(check.GetName()),
			Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{
				Filter: fmt.Sprintf(`metric.type = "monitoring.googleapis.com/uptime_check/check_passed" AND metric.labels.check_id = %q AND resource.type = "uptime_url"`,
					lastSegment(check.GetName())),
				Aggregations: []*monitoringpb.Aggregation{{AlignmentPeriod: durationpb.New(20 * time.Minute),
					PerSeriesAligner: monitoringpb.Aggregation_ALIGN_NEXT_OLDER, CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_COUNT_FALSE,
					GroupByFields: []string{"resource.labels.host", "resource.labels.project_id"}}},
				Comparison: monitoringpb.ComparisonType_COMPARISON_GT, ThresholdValue: 1, Duration: durationpb.New(dur),
				Trigger: &monitoringpb.AlertPolicy_Condition_Trigger{Type: &monitoringpb.AlertPolicy_Condition_Trigger_Count{Count: 1}}}}}}}
	for _, c := range splitList(values["alertChannels"]) {
		if !strings.HasPrefix(c, "projects/") {
			c = "projects/" + project + "/notificationChannels/" + c
		}
		pol.NotificationChannels = append(pol.NotificationChannels, c)
	}
	return pol, nil
}

// ---- Notification channels: each type's own fields ----

// channelFormFields are Create channel's fields, each shown for the types
// that take it, labelled as Google's per-type dialogs label them.
func channelFormFields() []console.Field {
	return []console.Field{
		{Name: "type", Label: "Channel type", Type: "select", Required: true, Options: channelFormTypes, Default: "webhook_tokenauth",
			OptionLabels: map[string]string{"webhook_tokenauth": "Webhooks", "webhook_basicauth": "Webhooks with basic authentication", "pubsub": "Pub/Sub",
				"email": "Email", "sms": "SMS", "slack": "Slack", "pagerduty": "PagerDuty Services", "google_chat": "Google Chat"}},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "url", Label: "Endpoint URL", Type: "text", ShowWhen: "type=webhook_tokenauth|webhook_basicauth",
			Help: "A local URL, such as http://localhost:8080/alerts: CloudBurrow posts only on this machine."},
		{Name: "username", Label: "Username", Type: "text", ShowWhen: "type=webhook_basicauth"},
		{Name: "password", Label: "Password", Type: "password", ShowWhen: "type=webhook_basicauth"},
		{Name: "topic", Label: "Topic name", Type: "text", ShowWhen: "type=pubsub", Help: "projects/PROJECT/topics/TOPIC on this instance."},
		{Name: "email_address", Label: "Email address", Type: "text", ShowWhen: "type=email", Help: "Recorded only: no email is sent."},
		{Name: "number", Label: "Phone number", Type: "text", ShowWhen: "type=sms", Help: "Recorded only: no message is sent."},
		{Name: "channel_name", Label: "Slack Channel Name", Type: "text", ShowWhen: "type=slack", Help: "Such as #alerts. Recorded only: nothing is posted to Slack."},
		{Name: "auth_token", Label: "Slack token", Type: "password", ShowWhen: "type=slack"},
		{Name: "service_key", Label: "Service Key", Type: "password", ShowWhen: "type=pagerduty", Help: "The PagerDuty integration key. Recorded only."},
		{Name: "space", Label: "Chat Space ID", Type: "text", ShowWhen: "type=google_chat", Help: "Such as spaces/AAAA. Recorded only."},
	}
}

// ---- Groups: criteria ----

var (
	// Google's Name, Tag and Region criteria match resource metadata,
	// which this instance keeps none of (the group API refuses
	// resource.metadata.*), so the criteria offered are the two it
	// evaluates.
	groupCriterionTypes  = []string{"resource_type", "project"}
	groupCriterionLabels = map[string]string{"name": "Name", "tag": "Tag", "region": "Region", "resource_type": "Resource Type", "project": "Cloud Project"}
	groupOperators       = []string{"contains", "not_contains", "equals", "starts_with", "ends_with"}
	groupOperatorLabels  = map[string]string{"contains": "Contains", "not_contains": "Does not contain", "equals": "Equals",
		"starts_with": "Starts with", "ends_with": "Ends with"}
)

// groupFormFields are Create Group's fields: a name, criteria added with
// Add criterion, the operator combining them, or a filter typed in.
func groupFormFields() []console.Field {
	return []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Required: true, Section: "Group details"},
		{Name: "criteria", Label: "Criteria", Type: console.GroupFieldType, AddLabel: "Add criterion", Section: "Group details",
			Help: "Each criterion matches resources by Type and Operator.",
			Group: []console.Field{
				{Name: "type", Label: "Type", Type: "select", Options: groupCriterionTypes, OptionLabels: groupCriterionLabels, Default: "resource_type"},
				{Name: "operator", Label: "Operator", Type: "select", Options: groupOperators, OptionLabels: groupOperatorLabels, Default: "contains"},
				{Name: "value", Label: "Value", Type: "text"},
			}},
		{Name: "combine", Label: "Combine criteria operator", Type: "radio", Options: []string{"AND", "OR"}, Default: "AND", Section: "Group details"},
		{Name: "filter", Label: "Filter", Type: "text", Section: "Group details",
			Help: `Instead of criteria: a group filter, such as resource.type = "global".`},
		{Name: "parent", Label: "Parent group", Type: "text", Section: "Group details", Help: "Optional: a group ID."},
		{Name: "isCluster", Label: "Treat as a cluster", Type: "checkbox", Section: "Group details"},
	}
}

// groupFilterFromCriteria is the group filter Create Group's criteria
// make, in the API's group filter language: Name, Tag and Region are the
// resource metadata's name, user_labels and region, Resource Type the
// resource's type and Cloud Project its project_id label.
func groupFilterFromCriteria(values map[string]string) (string, error) {
	rows, err := console.ParseGroup(values["criteria"])
	if err != nil {
		return "", err
	}
	var terms []string
	for _, r := range rows {
		val := strings.TrimSpace(r["value"])
		if val == "" {
			continue
		}
		sel := map[string]string{"name": "resource.metadata.name", "region": "resource.metadata.region",
			"resource_type": "resource.type", "project": "resource.labels.project_id"}[r["type"]]
		if r["type"] == "tag" {
			k, v, ok := strings.Cut(val, "=")
			if !ok {
				return "", fmt.Errorf("tag %q is not key=value", val)
			}
			sel, val = fmt.Sprintf("resource.metadata.user_labels.%q", strings.TrimSpace(k)), strings.TrimSpace(v)
		}
		if sel == "" {
			return "", fmt.Errorf("criterion type %q is not one of Name, Tag, Region, Resource Type, Cloud Project", r["type"])
		}
		var term string
		switch r["operator"] {
		case "equals":
			term = fmt.Sprintf("%s = %q", sel, val)
		case "contains", "":
			term = fmt.Sprintf("%s = has_substring(%q)", sel, val)
		case "not_contains":
			term = fmt.Sprintf("%s != has_substring(%q)", sel, val)
		case "starts_with":
			term = fmt.Sprintf("%s = starts_with(%q)", sel, val)
		case "ends_with":
			term = fmt.Sprintf("%s = ends_with(%q)", sel, val)
		default:
			return "", fmt.Errorf("operator %q is not Contains, Does not contain, Equals, Starts with or Ends with", r["operator"])
		}
		terms = append(terms, term)
	}
	op := " AND "
	if values["combine"] == "OR" {
		op = " OR "
	}
	return strings.Join(terms, op), nil
}

// ---- Dashboards: Copy dashboard ----

// DetailActions is a dashboard's Copy dashboard, as Google's dashboard
// toolbar and list's More options offer it.
func (p monitoringDashboardsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	s, err := p.server()
	if err != nil || len(path) != 1 {
		return nil
	}
	d, err := s.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: "projects/" + project + "/dashboards/" + path[0]})
	if err != nil {
		return nil
	}
	return []console.Action{{ID: "copy", Label: "Copy dashboard", Submit: "Copy", Fields: []console.Field{
		{Name: "displayName", Label: "Dashboard name", Type: "text", Required: true, Default: "Copy of " + d.GetDisplayName()},
		{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(d.GetLabels())}}}}
}

func (p monitoringDashboardsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action != "copy" || len(path) != 1 {
		return fmt.Errorf("unknown action %q on a dashboard", action)
	}
	s, err := p.server()
	if err != nil {
		return err
	}
	d, err := s.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: "projects/" + project + "/dashboards/" + path[0]})
	if err != nil {
		return err
	}
	cp := proto.Clone(d).(*dashboardpb.Dashboard)
	cp.Name, cp.Etag = "", ""
	cp.DisplayName = strings.TrimSpace(values["displayName"])
	if cp.DisplayName == "" {
		return errors.New("the copy needs a dashboard name")
	}
	if v, ok := values["labels"]; ok {
		m, err := console.ParseMap(v)
		if err != nil {
			return fmt.Errorf("labels: %v", err)
		}
		cp.Labels = m
	}
	_, err = s.CreateDashboard(ctx, &dashboardpb.CreateDashboardRequest{Parent: "projects/" + project, Dashboard: cp})
	return err
}

// ---- Services: Google's Overview columns ----

// serviceTypeText is a service's Type as the Services Overview names it.
func serviceTypeText(sv *monitoringpb.Service) string {
	switch {
	case sv.GetCustom() != nil:
		return "Custom"
	case sv.GetAppEngine() != nil:
		return "App Engine"
	case sv.GetCloudEndpoints() != nil:
		return "Cloud Endpoints"
	case sv.GetCloudRun() != nil:
		return "Cloud Run"
	case sv.GetGkeService() != nil:
		return "GKE service"
	case sv.GetGkeWorkload() != nil:
		return "GKE workload"
	case sv.GetGkeNamespace() != nil:
		return "GKE namespace"
	case sv.GetClusterIstio() != nil, sv.GetMeshIstio() != nil, sv.GetIstioCanonicalService() != nil:
		return "Istio"
	case sv.GetBasicService() != nil:
		return sv.GetBasicService().GetServiceType()
	}
	return "—"
}

// firingSLOText is the conditions, joined, of the policies with an open
// alert: an SLO named in one (select_slo_burn_rate("<slo>"), as an SLO's
// alerting policy is written) has a firing alert.
func firingSLOText(ctx context.Context, s *monitoring.Server, project string) string {
	open := map[string]bool{}
	for _, in := range s.Incidents(project) {
		if in.State == monitoring.IncidentOpen {
			open[in.Policy] = true
		}
	}
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	for name := range open {
		pol, err := s.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: name})
		if err != nil {
			continue
		}
		for _, c := range pol.GetConditions() {
			b.WriteString(c.GetConditionThreshold().GetFilter())
			b.WriteString(c.GetConditionPrometheusQueryLanguage().GetQuery())
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- Snoozes: Create snooze ----

// snoozeFormFields are Create snooze's: its name, the alert policies it
// silences (at most 16), its start time and duration.
func snoozeFormFields() []console.Field {
	return []console.Field{
		{Name: "displayName", Label: "Snooze name", Type: "text", Required: true, Section: "Snooze details"},
		{Name: "policies", Label: "Alert policies", Type: "text", Required: true, Section: "Snooze criteria",
			Help: "Policy IDs from Alerting, comma separated: at most 16."},
		{Name: "start", Label: "Start time", Type: console.DateTimeFieldType, Section: "Snooze details", Help: "Empty to start now."},
		{Name: "minutes", Label: "Duration (minutes)", Type: "number", Required: true, Default: "60", Section: "Snooze details"},
	}
}

// snoozeInterval is the interval Create snooze's start and duration make.
func snoozeInterval(values map[string]string, now time.Time) (*monitoringpb.TimeInterval, error) {
	mins, err := strconv.Atoi(strings.TrimSpace(values["minutes"]))
	if err != nil || mins <= 0 {
		return nil, fmt.Errorf("duration %q is not a number of minutes", values["minutes"])
	}
	start := now
	if v := strings.TrimSpace(values["start"]); v != "" {
		if start, err = time.Parse(time.RFC3339, v); err != nil {
			return nil, fmt.Errorf("start time %q is not a time", v)
		}
		if start.Before(now) {
			start = now
		}
	}
	return &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(start.Add(time.Duration(mins) * time.Minute))}, nil
}
