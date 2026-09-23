package tasks

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// serve starts the Cloud Tasks gRPC service and returns a real client
// connection to it, so the tests exercise the wire format rather than calling
// the handlers directly.
func serve(t *testing.T) taskspb.CloudTasksClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	NewGRPCServer(NewStore(store.NewMemory())).Register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return taskspb.NewCloudTasksClient(conn)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestGRPCQueueLifecycle(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)

	q, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{
		Parent: loc,
		Queue:  &taskspb.Queue{Name: queueA},
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if q.GetName() != queueA {
		t.Errorf("name = %q", q.GetName())
	}
	if q.GetState() != taskspb.Queue_RUNNING {
		t.Errorf("state = %v, want RUNNING", q.GetState())
	}
	// Contract defaults must be returned, not zero values.
	if q.GetRetryConfig().GetMaxAttempts() == 0 || q.GetRateLimits().GetMaxConcurrentDispatches() == 0 {
		t.Errorf("defaults missing from the response: %+v", q)
	}

	got, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA})
	if err != nil || got.GetName() != queueA {
		t.Fatalf("GetQueue = %v, %v", got, err)
	}

	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v, want AlreadyExists", status.Code(err))
	}

	if _, err := c.DeleteQueue(cc, &taskspb.DeleteQueueRequest{Name: queueA}); err != nil {
		t.Fatalf("DeleteQueue: %v", err)
	}
	if _, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA}); status.Code(err) != codes.NotFound {
		t.Errorf("GetQueue after delete = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCErrorCodes(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)

	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"missing queue", func() error {
			_, err := c.GetQueue(cc, &taskspb.GetQueueRequest{Name: queueA})
			return err
		}, codes.NotFound},
		{"malformed parent", func() error {
			_, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: "nonsense"})
			return err
		}, codes.InvalidArgument},
		{"queue name outside parent", func() error {
			_, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{
				Parent: loc,
				Queue:  &taskspb.Queue{Name: "projects/other-project/locations/us-central1/queues/q"},
			})
			return err
		}, codes.InvalidArgument},
		{"task in missing queue", func() error {
			_, err := c.CreateTask(cc, &taskspb.CreateTaskRequest{
				Parent: queueA,
				Task:   &taskspb.Task{MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://x/"}}},
			})
			return err
		}, codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := status.Code(tt.call()); got != tt.want {
				t.Errorf("code = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGRPCTaskLifecycle(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)

	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}

	task, err := c.CreateTask(cc, &taskspb.CreateTaskRequest{
		Parent: queueA,
		Task: &taskspb.Task{
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
				Url:        "http://127.0.0.1:1/work",
				HttpMethod: taskspb.HttpMethod_PUT,
				Body:       []byte("payload"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// A name is generated when the caller omits one, as the contract permits.
	if task.GetName() == "" {
		t.Fatal("no task name was generated")
	}
	if got := task.GetHttpRequest().GetHttpMethod(); got != taskspb.HttpMethod_PUT {
		t.Errorf("method = %v, want PUT; the request shape did not round-trip", got)
	}

	got, err := c.GetTask(cc, &taskspb.GetTaskRequest{Name: task.GetName()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if string(got.GetHttpRequest().GetBody()) != "payload" {
		t.Errorf("body did not round-trip: %q", got.GetHttpRequest().GetBody())
	}

	list, err := c.ListTasks(cc, &taskspb.ListTasksRequest{Parent: queueA})
	if err != nil || len(list.GetTasks()) != 1 {
		t.Fatalf("ListTasks = %d, %v", len(list.GetTasks()), err)
	}

	if _, err := c.DeleteTask(cc, &taskspb.DeleteTaskRequest{Name: task.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetTask(cc, &taskspb.GetTaskRequest{Name: task.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTask after delete = %v, want NotFound", status.Code(err))
	}
}

// App Engine targets are out of scope. Reporting that is better than accepting
// a task that would never be dispatched.
func TestGRPCAppEngineTargetIsUnimplemented(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	_, err := c.CreateTask(cc, &taskspb.CreateTaskRequest{
		Parent: queueA,
		Task: &taskspb.Task{MessageType: &taskspb.Task_AppEngineHttpRequest{
			AppEngineHttpRequest: &taskspb.AppEngineHttpRequest{RelativeUri: "/work"},
		}},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("App Engine target = %v, want Unimplemented", status.Code(err))
	}
}

func TestGRPCPauseResume(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: queueA}}); err != nil {
		t.Fatal(err)
	}
	paused, err := c.PauseQueue(cc, &taskspb.PauseQueueRequest{Name: queueA})
	if err != nil {
		t.Fatal(err)
	}
	if paused.GetState() != taskspb.Queue_PAUSED {
		t.Errorf("state = %v, want PAUSED", paused.GetState())
	}
	resumed, err := c.ResumeQueue(cc, &taskspb.ResumeQueueRequest{Name: queueA})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.GetState() != taskspb.Queue_RUNNING {
		t.Errorf("state = %v, want RUNNING", resumed.GetState())
	}
}

func TestGRPCListPagination(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	for i := 0; i < 7; i++ {
		if _, err := c.CreateQueue(cc, &taskspb.CreateQueueRequest{
			Parent: loc,
			Queue:  &taskspb.Queue{Name: fmt.Sprintf("%s/queues/queue-%02d", loc, i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	token := ""
	for pages := 0; pages < 10; pages++ {
		resp, err := c.ListQueues(cc, &taskspb.ListQueuesRequest{Parent: loc, PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatalf("ListQueues: %v", err)
		}
		for _, q := range resp.GetQueues() {
			seen[q.GetName()]++
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if len(seen) != 7 {
		t.Errorf("saw %d distinct queues across pages, want 7", len(seen))
	}
	for n, count := range seen {
		if count != 1 {
			t.Errorf("queue %s appeared %d times", n, count)
		}
	}
}

// An unimplemented method must say so, not return a plausible empty success.
func TestGRPCUnimplementedMethodsAreHonest(t *testing.T) {
	t.Parallel()
	c := serve(t)
	cc := ctx(t)
	if _, err := c.RunTask(cc, &taskspb.RunTaskRequest{Name: TaskName(queueA, "t")}); status.Code(err) != codes.Unimplemented {
		t.Errorf("RunTask = %v, want Unimplemented", status.Code(err))
	}
	if _, err := c.UpdateQueue(cc, &taskspb.UpdateQueueRequest{Queue: &taskspb.Queue{Name: queueA}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateQueue = %v, want Unimplemented", status.Code(err))
	}
}
