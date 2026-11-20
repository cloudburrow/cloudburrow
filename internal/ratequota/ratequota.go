// Package ratequota enforces the per-project, per-minute request quotas
// Google documents for an API (#2063): each call is charged to its
// project's quota metric, and a call past the minute's limit is refused as
// Google refuses it, RESOURCE_EXHAUSTED (HTTP 429), "Quota exceeded for
// quota metric '<metric>' and limit '<limit>' of service '<service>' for
// consumer 'project_number:<number>'.", with a google.rpc.ErrorInfo
// (reason RATE_LIMIT_EXCEEDED, domain googleapis.com, metadata service,
// consumer, quota_metric, quota_limit, quota_limit_value, quota_location)
// and a google.rpc.Help link to the console's Quotas page. The ErrorInfo's
// shape is error_reason.proto's own RATE_LIMIT_EXCEEDED example; the
// message is the text Google's front end answers every API's rate quota
// with (UNVERIFIED for a given API: its quotas page gives none).
//
// The minute is the calendar minute, as Google's per-minute quotas refill.
//
// Limits are each metric's documented default unless configured: the
// configuration file's requestQuotas sets one for every project, and the
// admin API (GET, PUT and DELETE /admin/quotas) one for one project or
// every project while the instance runs, so a test can lower a limit in a
// project of its own without touching another's.
package ratequota

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Metric is one quota: a service's metric and its per-minute,
// per-project limit.
type Metric struct {
	// Service is the API, "parametermanager.googleapis.com".
	Service string `json:"service"`
	// Name is the metric, "parametermanager.googleapis.com/read_requests".
	Name string `json:"metric"`
	// Title is its display name, "Read requests".
	Title string `json:"title"`
	// Limit is the limit's name, "ReadRequestsPerMinutePerProject", and
	// LimitTitle its display name, "Read requests per minute per project".
	Limit      string `json:"limit"`
	LimitTitle string `json:"limitTitle"`
	// Default is Google's documented limit.
	Default int64 `json:"default"`
}

// Quota is a metric with the limit in force.
type Quota struct {
	Metric
	// Value is the limit in force for every project; Projects the
	// projects with a limit of their own, by number.
	Value    int64            `json:"value"`
	Projects map[string]int64 `json:"projects,omitempty"`
}

type key struct{ metric, project string }

type window struct {
	minute time.Time
	used   int64
}

// Limiter charges calls to their projects' quotas.
type Limiter struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu        sync.Mutex
	metrics   map[string]Metric
	overrides map[key]int64
	windows   map[key]*window
}

// New is a Limiter for no metrics yet.
func New() *Limiter {
	return &Limiter{metrics: map[string]Metric{}, overrides: map[key]int64{}, windows: map[key]*window{}}
}

// Register adds metrics, at their defaults.
func (l *Limiter) Register(ms ...Metric) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range ms {
		l.metrics[m.Name] = m
	}
}

// Set sets metric's limit, per minute, for project ("" every project); a
// negative limit is refused.
func (l *Limiter) Set(metric, project string, limit int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.metrics[metric]; !ok {
		return fmt.Errorf("unknown quota metric %q", metric)
	}
	if limit < 0 {
		return fmt.Errorf("quota %s: a limit is a count of requests per minute, 0 or more, not %d", metric, limit)
	}
	l.overrides[key{metric, consumerOf(project)}] = limit
	return nil
}

// Clear restores metric's default for project ("" every project's own
// limit); metric "" clears every metric's.
func (l *Limiter) Clear(metric, project string) {
	project = consumerOf(project)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.overrides {
		if (metric == "" || k.metric == metric) && k.project == project {
			delete(l.overrides, k)
		}
	}
}

// consumerOf is the project number a project (an ID or a number) is
// charged as, "" for "".
func consumerOf(project string) string {
	if project == "" {
		return ""
	}
	return strconv.FormatInt(resource.ProjectNumber(project), 10)
}

// limitLocked is metric's limit in the project numbered project.
func (l *Limiter) limitLocked(m Metric, project string) int64 {
	if v, ok := l.overrides[key{m.Name, project}]; ok {
		return v
	}
	if v, ok := l.overrides[key{m.Name, ""}]; ok {
		return v
	}
	return m.Default
}

// Quotas lists the metrics and their limits, by name.
func (l *Limiter) Quotas() []Quota {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Quota, 0, len(l.metrics))
	for _, m := range l.metrics {
		q := Quota{Metric: m, Value: l.limitLocked(m, "")}
		for k, v := range l.overrides {
			if k.metric == m.Name && k.project != "" {
				if q.Projects == nil {
					q.Projects = map[string]int64{}
				}
				q.Projects[k.project] = v
			}
		}
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Charge counts one call to metric in project, nil while the minute's
// limit holds, else Google's RESOURCE_EXHAUSTED. A nil Limiter, an
// unknown metric and a call with no project charge nothing.
func (l *Limiter) Charge(metric, project string) error {
	if l == nil || project == "" {
		return nil
	}
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	minute := now.Truncate(time.Minute)
	consumer := consumerOf(project)
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.metrics[metric]
	if !ok {
		return nil
	}
	// Projects are charged by number, so an ID and its number share one
	// quota, as on Google.
	k := key{metric, consumer}
	w := l.windows[k]
	if w == nil || !w.minute.Equal(minute) {
		w = &window{minute: minute}
		l.windows[k] = w
	}
	limit := l.limitLocked(m, consumer)
	if w.used >= limit {
		return Exceeded(m, consumer, limit)
	}
	w.used++
	return nil
}

// Reset forgets the minute's counts.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.windows = map[key]*window{}
}

const (
	// Reason and Domain are the ErrorInfo's.
	Reason = "RATE_LIMIT_EXCEEDED"
	Domain = "googleapis.com"
)

// Exceeded is Google's refusal of a call past m's limit in the project
// numbered number.
func Exceeded(m Metric, number string, limit int64) error {
	msg := fmt.Sprintf("Quota exceeded for quota metric '%s' and limit '%s' of service '%s' for consumer 'project_number:%s'.",
		m.Title, m.LimitTitle, m.Service, number)
	info, _ := anypb.New(&errdetails.ErrorInfo{Reason: Reason, Domain: Domain, Metadata: map[string]string{
		"service":           m.Service,
		"consumer":          "projects/" + number,
		"quota_metric":      m.Name,
		"quota_limit":       m.Limit,
		"quota_limit_value": strconv.FormatInt(limit, 10),
		"quota_location":    "global",
		"quota_unit":        "1/min/{project}",
	}})
	help, _ := anypb.New(&errdetails.Help{Links: []*errdetails.Help_Link{{
		Description: "Request a higher quota limit.",
		Url:         "https://cloud.google.com/docs/quotas/help/request_increase",
	}}})
	return &apierror.Error{Code: codes.ResourceExhausted, Message: msg, Reason: "rateLimitExceeded", Details: []*anypb.Any{info, help}}
}
