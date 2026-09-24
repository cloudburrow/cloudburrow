// Package scheduler implements Cloud Scheduler (google.cloud.scheduler.v1)
// for local development (#302).
//
// Google publishes no Cloud Scheduler emulator, so this is built, as Cloud
// Tasks was (#24): jobs with cron schedules in any IANA time zone, delivered
// to HTTP targets or published to the local Pub/Sub emulator, with retries on
// the same schedule Cloud Tasks computes. App Engine targets and OIDC/OAuth
// tokens are refused as UNIMPLEMENTED rather than accepted and ignored.
package scheduler

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// State is a job's state, spelled as the API spells it.
type State string

const (
	StateEnabled State = "ENABLED"
	StatePaused  State = "PAUSED"
)

// HTTPTarget is an HTTP job's request.
type HTTPTarget struct {
	URI     string            `json:"uri"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// PubSubTarget is a Pub/Sub job's message.
type PubSubTarget struct {
	Topic      string            `json:"topic"`
	Data       []byte            `json:"data,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// RetryConfig is a job's retry policy, as the API defines it.
type RetryConfig struct {
	RetryCount       int           `json:"retryCount"`
	MaxRetryDuration time.Duration `json:"maxRetryDuration"`
	MinBackoff       time.Duration `json:"minBackoff"`
	MaxBackoff       time.Duration `json:"maxBackoff"`
	MaxDoublings     int           `json:"maxDoublings"`
}

// DefaultRetryConfig is the service's default: no retries, 5s to 1h
// backoff, 5 doublings, no retry deadline.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MinBackoff: 5 * time.Second, MaxBackoff: time.Hour, MaxDoublings: 5}
}

// Job is one scheduled job.
type Job struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Schedule    string        `json:"schedule"`
	TimeZone    string        `json:"timeZone"`
	State       State         `json:"state"`
	HTTP        *HTTPTarget   `json:"http,omitempty"`
	PubSub      *PubSubTarget `json:"pubsub,omitempty"`
	Retry       RetryConfig   `json:"retry"`
	// AttemptDeadline bounds one HTTP attempt.
	AttemptDeadline time.Duration `json:"attemptDeadline"`
	UserUpdateTime  time.Time     `json:"userUpdateTime"`
	// ScheduleTime is when the job next runs.
	ScheduleTime    time.Time `json:"scheduleTime"`
	LastAttemptTime time.Time `json:"lastAttemptTime,omitempty"`
	// LastCode and LastMessage are the last attempt's outcome, a gRPC code
	// number and message, as Job.status carries them.
	LastCode    int32  `json:"lastCode"`
	LastMessage string `json:"lastMessage,omitempty"`
}

const keyPrefix = "scheduler/jobs/"

// Store holds jobs in a store.Store, so persistence follows the instance's
// mode like every other service here.
type Store struct {
	mu sync.Mutex
	db store.Store
}

// NewStore returns a store over db.
func NewStore(db store.Store) *Store { return &Store{db: db} }

// Backing is the underlying store, for resets and snapshots.
func (s *Store) Backing() store.Store { return s.db }

func key(name string) string { return keyPrefix + strings.ReplaceAll(name, "/", "~") }

func (s *Store) put(j Job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return apierror.Internal(err, "encode job")
	}
	if err := s.db.Put(key(j.Name), b); err != nil {
		return apierror.Internal(err, "store job")
	}
	return nil
}

func (s *Store) get(name string) (Job, error) {
	b, err := s.db.Get(key(name))
	if err != nil {
		return Job{}, apierror.NotFound("job %s not found", name)
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return Job{}, apierror.Internal(err, "decode job %s", name)
	}
	return j, nil
}

// Create adds a job; one of that name must not exist.
func (s *Store) Create(j Job) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.get(j.Name); err == nil {
		return Job{}, apierror.AlreadyExists("job %s already exists", j.Name)
	}
	return j, s.put(j)
}

// Get returns one job.
func (s *Store) Get(name string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(name)
}

// Update applies fn to a job under the lock and stores the result.
func (s *Store) Update(name string, fn func(*Job) error) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.get(name)
	if err != nil {
		return Job{}, err
	}
	if err := fn(&j); err != nil {
		return Job{}, err
	}
	return j, s.put(j)
}

// Delete removes a job.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.get(name); err != nil {
		return err
	}
	if err := s.db.Delete(key(name)); err != nil {
		return apierror.Internal(err, "delete job %s", name)
	}
	return nil
}

// List returns every job whose name starts with prefix, ordered by name.
func (s *Store) List(prefix string) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.db.List(keyPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list jobs")
	}
	var out []Job
	for _, k := range keys {
		name := strings.ReplaceAll(strings.TrimPrefix(k, keyPrefix), "~", "/")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if j, err := s.get(name); err == nil {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

// Reset removes every job.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.db.List(keyPrefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.db.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
