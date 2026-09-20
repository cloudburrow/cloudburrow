// Package lro tracks long-running operations and exposes pending, completed
// and failed states.
//
// Cloud Run's admin API returns operations for create and update, so a client
// polls until one reports done. All three terminal states must be reachable:
// an implementation that can only ever succeed would let a caller write code
// that never handles failure.
package lro

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound means no such operation.
var ErrNotFound = errors.New("operation not found")

// State is an operation's lifecycle state.
type State int

const (
	StatePending State = iota
	StateSucceeded
	StateFailed
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateSucceeded:
		return "succeeded"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Operation is a long-running operation.
type Operation struct {
	Name     string
	Target   string
	State    State
	Error    error
	Response any
	Created  time.Time
	Updated  time.Time
}

// Done reports whether the operation reached a terminal state.
func (o Operation) Done() bool { return o.State != StatePending }

// Store tracks operations in memory.
//
// Operations are process-scoped: they describe work this instance is doing, so
// losing them on restart is correct rather than a gap.
type Store struct {
	mu  sync.Mutex
	ops map[string]*Operation
	now func() time.Time
	seq int
}

// NewStore returns a store. now is injectable so tests control timestamps
// without sleeping.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ops: map[string]*Operation{}, now: now}
}

// Create registers a pending operation for a target resource.
func (s *Store) Create(parent, target string) *Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	name := fmt.Sprintf("%s/operations/op-%d", parent, s.seq)
	op := &Operation{
		Name:    name,
		Target:  target,
		State:   StatePending,
		Created: s.now(),
		Updated: s.now(),
	}
	s.ops[name] = op
	return cloneOp(op)
}

// Succeed marks an operation completed.
func (s *Store) Succeed(name string, response any) error {
	return s.finish(name, StateSucceeded, response, nil)
}

// Fail marks an operation failed. The cause is preserved so a poller learns
// why, not merely that it did not work.
func (s *Store) Fail(name string, cause error) error {
	return s.finish(name, StateFailed, nil, cause)
}

func (s *Store) finish(name string, state State, response any, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if op.Done() {
		// Terminal states are final; a second transition would let a caller
		// observe an operation moving backwards.
		return fmt.Errorf("operation %s is already %s", name, op.State)
	}
	op.State, op.Response, op.Error, op.Updated = state, response, cause, s.now()
	return nil
}

// Get returns a copy of an operation.
func (s *Store) Get(name string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return *cloneOp(op), nil
}

// List returns every operation under a parent, in deterministic order.
func (s *Store) List(parent string) []Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Operation
	for name, op := range s.ops {
		if parent == "" || len(name) > len(parent) && name[:len(parent)] == parent {
			out = append(out, *cloneOp(op))
		}
	}
	// Stable order so repeated listings agree.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func cloneOp(o *Operation) *Operation {
	c := *o
	return &c
}
