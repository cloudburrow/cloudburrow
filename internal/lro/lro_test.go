package lro

import (
	"errors"
	"testing"
	"time"
)

// clock is injected so tests control timestamps without sleeping.
func fixedClock() func() time.Time {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

// All three states must be reachable. An implementation that can only succeed
// would let a caller write code that never handles failure.
func TestAllStatesAreReachable(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	parent := "projects/p/locations/l"

	pending := s.Create(parent, "services/a")
	if pending.State != StatePending || pending.Done() {
		t.Errorf("new operation = %v, want pending", pending.State)
	}

	ok := s.Create(parent, "services/b")
	if err := s.Succeed(ok.Name, "result"); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	got, _ := s.Get(ok.Name)
	if got.State != StateSucceeded || !got.Done() {
		t.Errorf("succeeded operation = %v", got.State)
	}
	if got.Response != "result" {
		t.Errorf("response = %v, want result", got.Response)
	}

	bad := s.Create(parent, "services/c")
	cause := errors.New("image pull failed")
	if err := s.Fail(bad.Name, cause); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ = s.Get(bad.Name)
	if got.State != StateFailed || !got.Done() {
		t.Errorf("failed operation = %v", got.State)
	}
	// The cause must survive: a poller needs to learn *why*, not just that it
	// did not work.
	if !errors.Is(got.Error, cause) {
		t.Errorf("error = %v, want %v", got.Error, cause)
	}
}

// Terminal states are final. Allowing a second transition would let a caller
// observe an operation moving backwards.
func TestTerminalStatesAreFinal(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	op := s.Create("projects/p/locations/l", "t")
	if err := s.Succeed(op.Name, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(op.Name, errors.New("late")); err == nil {
		t.Error("a succeeded operation was allowed to fail afterwards")
	}
	if err := s.Succeed(op.Name, nil); err == nil {
		t.Error("a succeeded operation was allowed to succeed twice")
	}
	got, _ := s.Get(op.Name)
	if got.State != StateSucceeded {
		t.Errorf("state changed after terminal: %v", got.State)
	}
}

func TestUnknownOperation(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	if _, err := s.Get("projects/p/locations/l/operations/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Succeed("missing", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Succeed(missing) = %v, want ErrNotFound", err)
	}
}

// Names must be unique and scoped to their parent, so two operations cannot
// collide and a listing cannot leak across projects.
func TestOperationNamesAreUniqueAndScoped(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	a := s.Create("projects/a/locations/l", "x")
	b := s.Create("projects/a/locations/l", "y")
	c := s.Create("projects/other/locations/l", "z")

	if a.Name == b.Name {
		t.Errorf("duplicate operation name %q", a.Name)
	}
	listA := s.List("projects/a/locations/l")
	if len(listA) != 2 {
		t.Errorf("List(project a) = %d operations, want 2", len(listA))
	}
	for _, op := range listA {
		if op.Name == c.Name {
			t.Error("listing leaked an operation from another project")
		}
	}
}

// Returned operations must be copies; a caller mutating one must not corrupt
// the store.
func TestGetReturnsACopy(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	op := s.Create("projects/p/locations/l", "t")
	got, _ := s.Get(op.Name)
	got.State = StateFailed

	again, _ := s.Get(op.Name)
	if again.State != StatePending {
		t.Error("mutating a returned operation changed the store")
	}
}

func TestListIsDeterministic(t *testing.T) {
	t.Parallel()
	s := NewStore(fixedClock())
	for i := 0; i < 10; i++ {
		s.Create("projects/p/locations/l", "t")
	}
	first := s.List("projects/p/locations/l")
	for i := 0; i < 5; i++ {
		next := s.List("projects/p/locations/l")
		for j := range first {
			if first[j].Name != next[j].Name {
				t.Fatalf("listing order varied between calls at %d", j)
			}
		}
	}
}
