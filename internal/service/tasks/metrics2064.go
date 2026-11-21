package tasks

// Cloud Tasks' Cloud Monitoring metrics (#2064). Google documents four, on
// the cloud_tasks_queue monitored resource
// (https://docs.cloud.google.com/monitoring/api/metrics_gcp_c, read
// 2026-11-20):
//
//   - cloudtasks.googleapis.com/api/request_count, DELTA INT64: "Count of
//     Cloud Tasks API calls", labels api_method and response_code;
//   - cloudtasks.googleapis.com/queue/depth, GAUGE INT64: "The number of
//     tasks in the queue";
//   - cloudtasks.googleapis.com/queue/task_attempt_count, DELTA INT64:
//     "Count of task attempts broken down by response code", label
//     response_code;
//   - cloudtasks.googleapis.com/queue/task_attempt_delays, DELTA
//     DISTRIBUTION in ms: "Delay between each scheduled attempt time and
//     actual attempt time".
//
// The store records the API calls and the attempts as they happen; Cloud
// Monitoring drains them into points (monitoring.TasksBuiltin) and reads
// each queue's depth from the store when it samples.

import (
	"strings"
	"sync"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MetricKind is what a MetricEvent counts.
type MetricKind int

const (
	// MetricAPICall is one Cloud Tasks API call naming a queue or a task.
	MetricAPICall MetricKind = iota + 1
	// MetricAttempt is one dispatch attempt.
	MetricAttempt
)

// MetricEvent is one API call or dispatch attempt, for Cloud Monitoring.
type MetricEvent struct {
	Kind MetricKind
	Time time.Time
	// Queue is the queue's full name, projects/P/locations/L/queues/Q.
	Queue string
	// TargetType is the queue's target, the resource's target_type label.
	TargetType string
	// Method is an API call's method, such as CreateTask.
	Method string
	// Code is the canonical code as Google's response_code label writes
	// it, lower case: "ok", "not_found".
	Code string
	// Delay is an attempt's actual time less its scheduled time.
	Delay time.Duration
}

// maxPendingMetrics bounds the events waiting for Cloud Monitoring, so an
// instance with no Monitoring reading them keeps a fixed amount.
const maxPendingMetrics = 100000

// metricsLog holds the events not yet drained.
type metricsLog struct {
	mu      sync.Mutex
	pending []MetricEvent
}

func (m *metricsLog) add(e MetricEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, e)
	if over := len(m.pending) - maxPendingMetrics; over > 0 {
		m.pending = append(m.pending[:0:0], m.pending[over:]...)
	}
}

// DrainMetrics returns the API calls and attempts recorded since the last
// drain, oldest first, and forgets them. One reader drains: the instance's
// Cloud Monitoring.
func (s *Store) DrainMetrics() []MetricEvent {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	out := s.metrics.pending
	s.metrics.pending = nil
	return out
}

// QueueDepth is a queue's task count now.
type QueueDepth struct {
	Queue, TargetType string
	Tasks             int
}

// Depths are every queue's task counts now, for queue/depth.
func (s *Store) Depths() ([]QueueDepth, error) {
	queues, err := s.AllQueues()
	if err != nil {
		return nil, err
	}
	out := make([]QueueDepth, 0, len(queues))
	for _, q := range queues {
		ts, err := s.ListTasks(q.Name)
		if err != nil {
			continue
		}
		out = append(out, QueueDepth{Queue: q.Name, TargetType: targetType(q), Tasks: len(ts)})
	}
	return out, nil
}

// targetType is a queue's target_type: APP_ENGINE for a queue routed to
// App Engine, HTTP otherwise, the TaskActivityLog targetType values its
// task_operations_log entries carry on the same resource (#1256). Google's
// resource list names the label ("The target type the queue is
// dispatching to") without listing its values.
func targetType(q Queue) string {
	if q.AppEngineRouting != nil {
		return "APP_ENGINE"
	}
	return "HTTP"
}

// codeLabel is a canonical code as the response_code label writes it.
func codeLabel(c codes.Code) string {
	return strings.ToLower(rpccode.Code(c).String())
}

// queueNameIn is the queue a resource name is or is under, "" for none.
func queueNameIn(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 6 || p[0] != "projects" || p[2] != "locations" || p[4] != "queues" || p[5] == "" {
		return ""
	}
	return strings.Join(p[:6], "/")
}

// requestQueue is the queue an API request names, "" for none.
func requestQueue(req any) string {
	switch r := req.(type) {
	case interface{ GetQueue() *taskspb.Queue }:
		return queueNameIn(r.GetQueue().GetName())
	case interface{ GetName() string }:
		return queueNameIn(r.GetName())
	case interface{ GetParent() string }:
		return queueNameIn(r.GetParent())
	case interface{ GetResource() string }:
		return queueNameIn(r.GetResource())
	}
	return ""
}

// called records one API call that names a queue; a call naming none
// (ListQueues, a location's) is not on any queue's resource.
func (g *GRPCServer) called(method string, req any, err error) {
	q := requestQueue(req)
	if q == "" || g.store == nil {
		return
	}
	tt := "HTTP"
	if stored, gerr := g.store.GetQueue(q); gerr == nil {
		tt = targetType(stored)
	}
	g.store.metrics.add(MetricEvent{Kind: MetricAPICall, Time: g.store.now(), Queue: q, TargetType: tt,
		Method: method, Code: codeLabel(status.Code(err))})
}

// recordAttempt records one dispatch attempt of task, started at started,
// whose outcome is code.
func (d *Dispatcher) recordAttempt(task Task, started time.Time, code codes.Code) {
	q := queueNameIn(task.Name)
	if q == "" {
		return
	}
	// The attempt's own target, as its log entry names it.
	tt := "HTTP"
	if task.AppEngineHTTPRequest != nil {
		tt = "APP_ENGINE"
	}
	delay := time.Duration(0)
	if !task.ScheduleTime.IsZero() && started.After(task.ScheduleTime) {
		delay = started.Sub(task.ScheduleTime)
	}
	d.store.metrics.add(MetricEvent{Kind: MetricAttempt, Time: started, Queue: q, TargetType: tt,
		Code: codeLabel(code), Delay: delay})
}
