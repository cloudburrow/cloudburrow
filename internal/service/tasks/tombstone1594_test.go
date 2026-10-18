package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func tombstones(t *testing.T, s *Store) []string {
	t.Helper()
	keys, err := s.db.List("tasks/tombstones/")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// A task created without a name leaves no tombstone when it goes (#1594):
// the soak's unnamed tasks left one each, which nothing ever removed, while
// a name the caller chose is still tombstoned.
func TestServerNamedTasksLeaveNoTombstone(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	g := NewGRPCServer(s)
	ctx := context.Background()
	http := &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://x/"}}
	for i := 0; i < 50; i++ {
		task, err := g.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{MessageType: http}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTask(task.GetName()); err != nil {
			t.Fatal(err)
		}
	}
	if got := tombstones(t, s); len(got) != 0 {
		t.Errorf("%d tombstones after 50 unnamed tasks, want none: %v", len(got), got[:1])
	}

	named := queueA + "/tasks/order-7"
	if _, err := g.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{Name: named, MessageType: http}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(named); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(t, s); len(got) != 1 || !strings.HasSuffix(got[0], "/order-7") {
		t.Errorf("%d tombstones after a named task, want its one", len(got))
	}
	if _, err := g.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: queueA, Task: &taskspb.Task{Name: named, MessageType: http}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing the named task's name = %v, want AlreadyExists", err)
	}
}

// Expired tombstones are removed though their names are never created
// again (#1594); before, one went only when its name came back.
func TestExpiredTombstonesAreSwept(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	mustQueue(t, s, queueA)
	for _, id := range []string{"a", "b", "c"} {
		name := TaskName(queueA, id)
		if _, err := s.CreateTask(Task{Name: name, HTTPRequest: &HTTPRequest{URL: "http://x/"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTask(name); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(tombstones(t, s)); got != 3 {
		t.Fatalf("%d tombstones, want 3", got)
	}

	// Past the window, the next deletion removes the three and leaves its own.
	now = now.Add(TombstoneTTL + time.Second)
	last := TaskName(queueA, "d")
	if _, err := s.CreateTask(Task{Name: last, HTTPRequest: &HTTPRequest{URL: "http://x/"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(last); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(t, s); len(got) != 1 || !strings.HasSuffix(got[0], "/d") {
		t.Errorf("tombstones after the sweep = %v, want d's alone", got)
	}
}
