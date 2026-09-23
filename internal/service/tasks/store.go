// Package tasks implements Cloud Tasks queues, tasks and HTTP dispatch.
//
// This is the one MVP service CloudBurrow implements itself: the upstream
// audit (#24) found no official emulator and no viable community
// implementation. Everything here is measured against the published
// google.cloud.tasks.v2 contract, never against another emulator's behaviour.
package tasks

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// State is a queue's dispatch state, mirroring Queue.State in the contract.
type State string

const (
	StateRunning  State = "RUNNING"
	StatePaused   State = "PAUSED"
	StateDisabled State = "DISABLED"
)

// RetryConfig mirrors google.cloud.tasks.v2.RetryConfig.
//
// Cloud Tasks retry is its own mechanism, deliberately not shared with Pub/Sub
// redelivery (docs/architecture.md §8).
type RetryConfig struct {
	MaxAttempts  int           `json:"maxAttempts"`
	MinBackoff   time.Duration `json:"minBackoff"`
	MaxBackoff   time.Duration `json:"maxBackoff"`
	MaxDoublings int           `json:"maxDoublings"`
}

// DefaultRetryConfig matches the service defaults: 100 attempts, 0.1s to 1h.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:  100,
		MinBackoff:   100 * time.Millisecond,
		MaxBackoff:   time.Hour,
		MaxDoublings: 16,
	}
}

// RateLimits mirrors google.cloud.tasks.v2.RateLimits.
type RateLimits struct {
	MaxDispatchesPerSecond  float64 `json:"maxDispatchesPerSecond"`
	MaxConcurrentDispatches int     `json:"maxConcurrentDispatches"`
}

// DefaultRateLimits matches the service defaults.
func DefaultRateLimits() RateLimits {
	return RateLimits{MaxDispatchesPerSecond: 500, MaxConcurrentDispatches: 1000}
}

// Queue is a task queue.
type Queue struct {
	Name        string      `json:"name"`
	State       State       `json:"state"`
	RetryConfig RetryConfig `json:"retryConfig"`
	RateLimits  RateLimits  `json:"rateLimits"`
	Created     time.Time   `json:"created"`
}

// HTTPRequest is a task's HTTP target.
type HTTPRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// Task is a queued unit of work.
type Task struct {
	Name         string       `json:"name"`
	Queue        string       `json:"queue"`
	HTTPRequest  *HTTPRequest `json:"httpRequest,omitempty"`
	ScheduleTime time.Time    `json:"scheduleTime"`
	Created      time.Time    `json:"created"`
	// DispatchCount counts attempts already made.
	DispatchCount int `json:"dispatchCount"`
	// ResponseCount counts attempts that produced a response, successful or not.
	ResponseCount int `json:"responseCount"`
	// LastResponseCode is the status of the most recent attempt, 0 if none.
	LastResponseCode int `json:"lastResponseCode"`
}

// Store holds queues and tasks.
//
// Keys are full resource names, so project and location isolation is
// structural: two queues with the same ID in different projects cannot collide
// (internal/resource).
type Store struct {
	mu sync.RWMutex
	db store.Store
}

// NewStore wraps a metadata store.
func NewStore(db store.Store) *Store { return &Store{db: db} }

func queueKey(name string) string { return "tasks/queues/" + name }
func taskKey(name string) string  { return "tasks/tasks/" + name }

// CreateQueue stores a new queue.
func (s *Store) CreateQueue(q Queue) (Queue, error) {
	n, err := resource.Parse(q.Name)
	if err != nil {
		return Queue{}, apierror.InvalidArgument("%v", err)
	}
	if n.Collection != "queues" {
		return Queue{}, apierror.InvalidArgument("%q is not a queue name", q.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Get(queueKey(q.Name)); err == nil {
		return Queue{}, apierror.AlreadyExists("queue %s already exists", q.Name)
	}

	if q.State == "" {
		q.State = StateRunning
	}
	if q.RetryConfig.MaxAttempts == 0 {
		q.RetryConfig = DefaultRetryConfig()
	}
	if q.RateLimits.MaxConcurrentDispatches == 0 {
		q.RateLimits = DefaultRateLimits()
	}
	if q.Created.IsZero() {
		q.Created = time.Now().UTC()
	}
	return q, s.put(queueKey(q.Name), q)
}

// GetQueue returns a queue.
func (s *Store) GetQueue(name string) (Queue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var q Queue
	if err := s.get(queueKey(name), &q); err != nil {
		return Queue{}, apierror.NotFound("queue %s not found", name)
	}
	return q, nil
}

// AllQueues returns every queue in every project, sorted by name.
//
// Separate from ListQueues because an empty parent there builds a prefix that
// matches nothing. A reset that used it would delete no queues and report
// success, which is worse than failing.
func (s *Store) AllQueues() ([]Queue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys, err := s.db.List("tasks/queues/")
	if err != nil {
		return nil, apierror.Internal(err, "list queues")
	}
	var out []Queue
	for _, k := range keys {
		var q Queue
		if err := s.get(k, &q); err == nil {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListQueues returns every queue under a parent, sorted by name.
//
// An empty parent returns nothing rather than everything: a caller that wants
// every queue must say so via AllQueues, so a missing parent cannot silently
// widen the scope of a listing.
func (s *Store) ListQueues(parent string) ([]Queue, error) {
	if parent == "" {
		return nil, apierror.InvalidArgument("parent is required; use AllQueues to list every project")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys, err := s.db.List(queueKey(parent + "/queues/"))
	if err != nil {
		return nil, apierror.Internal(err, "list queues")
	}
	var out []Queue
	for _, k := range keys {
		var q Queue
		if err := s.get(k, &q); err == nil {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteQueue removes a queue and every task in it.
//
// Deleting the tasks too is required: leaving them would let a later queue of
// the same name inherit work it never accepted.
func (s *Store) DeleteQueue(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Get(queueKey(name)); err != nil {
		return apierror.NotFound("queue %s not found", name)
	}
	taskKeys, err := s.db.List(taskKey(name + "/tasks/"))
	if err != nil {
		return apierror.Internal(err, "list tasks")
	}
	ops := []store.Op{{Kind: store.OpDelete, Key: queueKey(name)}}
	for _, k := range taskKeys {
		ops = append(ops, store.Op{Kind: store.OpDelete, Key: k})
	}
	if err := s.db.Commit(ops); err != nil {
		return apierror.Internal(err, "delete queue")
	}
	return nil
}

// SetQueueState pauses or resumes a queue.
func (s *Store) SetQueueState(name string, state State) (Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var q Queue
	if err := s.get(queueKey(name), &q); err != nil {
		return Queue{}, apierror.NotFound("queue %s not found", name)
	}
	q.State = state
	return q, s.put(queueKey(name), q)
}

// PurgeQueue removes every task without deleting the queue.
func (s *Store) PurgeQueue(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Get(queueKey(name)); err != nil {
		return apierror.NotFound("queue %s not found", name)
	}
	keys, err := s.db.List(taskKey(name + "/tasks/"))
	if err != nil {
		return apierror.Internal(err, "list tasks")
	}
	ops := make([]store.Op, 0, len(keys))
	for _, k := range keys {
		ops = append(ops, store.Op{Kind: store.OpDelete, Key: k})
	}
	if len(ops) == 0 {
		return nil
	}
	if err := s.db.Commit(ops); err != nil {
		return apierror.From(err)
	}
	return nil
}

// CreateTask queues a task.
func (s *Store) CreateTask(t Task) (Task, error) {
	n, err := resource.Parse(t.Name)
	if err != nil {
		return Task{}, apierror.InvalidArgument("%v", err)
	}
	if n.Collection != "tasks" || n.ParentCollection != "queues" {
		return Task{}, apierror.InvalidArgument("%q is not a task name", t.Name)
	}
	if t.HTTPRequest == nil || t.HTTPRequest.URL == "" {
		return Task{}, apierror.InvalidArgument("task requires an httpRequest with a url")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	queue := t.Queue
	if queue == "" {
		queue = n.LocationName() + "/queues/" + n.ParentID
		t.Queue = queue
	}
	if _, err := s.db.Get(queueKey(queue)); err != nil {
		return Task{}, apierror.NotFound("queue %s not found", queue)
	}
	if _, err := s.db.Get(taskKey(t.Name)); err == nil {
		return Task{}, apierror.AlreadyExists("task %s already exists", t.Name)
	}

	if t.HTTPRequest.Method == "" {
		t.HTTPRequest.Method = "POST"
	}
	now := time.Now().UTC()
	if t.Created.IsZero() {
		t.Created = now
	}
	if t.ScheduleTime.IsZero() {
		t.ScheduleTime = now
	}
	return t, s.put(taskKey(t.Name), t)
}

// GetTask returns a task.
func (s *Store) GetTask(name string) (Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var t Task
	if err := s.get(taskKey(name), &t); err != nil {
		return Task{}, apierror.NotFound("task %s not found", name)
	}
	return t, nil
}

// ListTasks returns tasks in a queue, sorted by name.
func (s *Store) ListTasks(queue string) ([]Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys, err := s.db.List(taskKey(queue + "/tasks/"))
	if err != nil {
		return nil, apierror.Internal(err, "list tasks")
	}
	var out []Task
	for _, k := range keys {
		var t Task
		if err := s.get(k, &t); err == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteTask removes a task.
func (s *Store) DeleteTask(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Get(taskKey(name)); err != nil {
		return apierror.NotFound("task %s not found", name)
	}
	if err := s.db.Delete(taskKey(name)); err != nil {
		return apierror.From(err)
	}
	return nil
}

// UpdateTask replaces a stored task, used to record dispatch outcomes.
func (s *Store) UpdateTask(t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.put(taskKey(t.Name), t)
}

// DueTasks returns tasks scheduled at or before now, across running queues.
//
// Paused and disabled queues are skipped rather than filtered later, so a
// paused queue genuinely stops dispatching.
func (s *Store) DueTasks(now time.Time) ([]Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys, err := s.db.List("tasks/tasks/")
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	var out []Task
	for _, k := range keys {
		var t Task
		if err := s.get(k, &t); err != nil {
			continue
		}
		if t.ScheduleTime.After(now) {
			continue
		}
		ok, seen := running[t.Queue]
		if !seen {
			var q Queue
			ok = s.get(queueKey(t.Queue), &q) == nil && q.State == StateRunning
			running[t.Queue] = ok
		}
		if ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// put stores a value.
//
// The error is checked before wrapping: apierror.From returns a typed nil
// pointer for a nil input, and returning that through an error interface would
// make every successful write look like a failure.
func (s *Store) put(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encode")
	}
	if err := s.db.Put(key, b); err != nil {
		return apierror.From(err)
	}
	return nil
}

func (s *Store) get(key string, v any) error {
	b, err := s.db.Get(key)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// TaskName builds a task resource name from its queue and ID.
func TaskName(queue, id string) string {
	return fmt.Sprintf("%s/tasks/%s", strings.TrimSuffix(queue, "/"), id)
}
