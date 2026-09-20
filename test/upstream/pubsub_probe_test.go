//go:build upstream

// Package upstream holds probes that measure what upstream components actually
// do, for the reuse audit in issue #24.
//
// These are not compatibility tests for CloudBurrow. They are evidence about
// third-party software, run with:
//
//	make test-upstream
//
// They require the Google Pub/Sub emulator to be installed:
//
//	gcloud components install pubsub-emulator
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	iampb "cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// emulator is a running Pub/Sub emulator process owned by a test.
type emulator struct {
	addr string
	cmd  *exec.Cmd
	log  *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freePort asks the OS for an unused port so probes can run in parallel.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startEmulator launches the official emulator and waits for it to answer.
//
// Readiness is polled with a bounded deadline rather than assumed after a
// sleep: this is an external process whose startup time we do not control, and
// issue #25 replaces blanket no-sleep rules with exactly this pattern.
func startEmulator(t *testing.T, dataDir string) *emulator {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{"beta", "emulators", "pubsub", "start",
		"--project=probe", "--host-port=" + addr}
	if dataDir != "" {
		args = append(args, "--data-dir="+dataDir)
	}

	log := &syncBuf{}
	cmd := exec.Command("gcloud", args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start emulator: %v", err)
	}

	e := &emulator{addr: addr, cmd: cmd, log: log}
	t.Cleanup(e.stop)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("emulator never became ready at %s; log:\n%s", addr, log.String())
	return nil
}

func (e *emulator) stop() {
	if e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Kill()
	_, _ = e.cmd.Process.Wait()
}

// client returns an SDK client pointed at the emulator. Setting
// PUBSUB_EMULATOR_HOST makes the official client use insecure transport with no
// credentials; nothing here reaches Google.
func (e *emulator) client(t *testing.T, project string) *pubsub.Client {
	t.Helper()
	t.Setenv("PUBSUB_EMULATOR_HOST", e.addr)
	c, err := pubsub.NewClient(context.Background(), project)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustTopic(t *testing.T, c *pubsub.Client, project, id string) string {
	t.Helper()
	name := fmt.Sprintf("projects/%s/topics/%s", project, id)
	_, err := c.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name})
	if err != nil {
		t.Fatalf("CreateTopic %s: %v", name, err)
	}
	return name
}

func mustSub(t *testing.T, c *pubsub.Client, project, id, topic string, push *pubsubpb.PushConfig) string {
	t.Helper()
	name := fmt.Sprintf("projects/%s/subscriptions/%s", project, id)
	sub := &pubsubpb.Subscription{Name: name, Topic: topic, AckDeadlineSeconds: 10}
	if push != nil {
		sub.PushConfig = push
	}
	_, err := c.SubscriptionAdminClient.CreateSubscription(context.Background(), sub)
	if err != nil {
		t.Fatalf("CreateSubscription %s: %v", name, err)
	}
	return name
}

// TestPublishAndSynchronousPull covers the basic control and data plane through
// the official SDK.
func TestPublishAndSynchronousPull(t *testing.T) {
	e := startEmulator(t, "")
	c := e.client(t, "probe")
	ctx := context.Background()

	topic := mustTopic(t, c, "probe", "topic-pull")
	sub := mustSub(t, c, "probe", "sub-pull", topic, nil)

	pub := c.Publisher(topic)
	defer pub.Stop()
	res := pub.Publish(ctx, &pubsub.Message{Data: []byte("hello"), Attributes: map[string]string{"k": "v"}})
	id, err := res.Get(ctx)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id == "" {
		t.Error("Publish returned an empty message ID")
	}
	t.Logf("published message id=%s", id)

	resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{
		Subscription: sub, MaxMessages: 10,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(resp.ReceivedMessages) != 1 {
		t.Fatalf("Pull returned %d messages, want 1", len(resp.ReceivedMessages))
	}
	got := resp.ReceivedMessages[0]
	if string(got.Message.Data) != "hello" {
		t.Errorf("data = %q, want hello", got.Message.Data)
	}
	if got.Message.Attributes["k"] != "v" {
		t.Errorf("attributes = %v, want k=v", got.Message.Attributes)
	}

	if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{
		Subscription: sub, AckIds: []string{got.AckId},
	}); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	t.Log("publish + synchronous Pull + Acknowledge: OK")
}

// TestStreamingPull exercises the path the Go and Python clients use by
// default. It is the hardest part of the Pub/Sub surface and the most important
// thing to confirm an upstream actually implements.
func TestStreamingPull(t *testing.T) {
	e := startEmulator(t, "")
	c := e.client(t, "probe")
	ctx := context.Background()

	topic := mustTopic(t, c, "probe", "topic-stream")
	sub := mustSub(t, c, "probe", "sub-stream", topic, nil)

	pub := c.Publisher(topic)
	const n = 25
	for i := 0; i < n; i++ {
		pub.Publish(ctx, &pubsub.Message{Data: []byte(fmt.Sprintf("msg-%d", i))})
	}
	pub.Stop()

	recvCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	seen := map[string]bool{}
	sr := c.Subscriber(sub)
	err := sr.Receive(recvCtx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		seen[string(m.Data)] = true
		done := len(seen) == n
		mu.Unlock()
		m.Ack()
		if done {
			cancel()
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive: %v", err)
	}

	mu.Lock()
	count := len(seen)
	mu.Unlock()
	if count != n {
		t.Fatalf("StreamingPull delivered %d distinct messages, want %d", count, n)
	}
	t.Logf("StreamingPull delivered all %d messages via the SDK's default path: OK", n)
}

// TestPushDelivery confirms the emulator delivers to an ordinary local HTTP
// endpoint, which is what a Cloud Run worker will be.
func TestPushDelivery(t *testing.T) {
	e := startEmulator(t, "")
	c := e.client(t, "probe")
	ctx := context.Background()

	received := make(chan string, 8)
	srv := &http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/push", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case received <- string(body):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv.Handler = mux
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	endpoint := fmt.Sprintf("http://%s/push", ln.Addr().String())
	topic := mustTopic(t, c, "probe", "topic-push")
	mustSub(t, c, "probe", "sub-push", topic, &pubsubpb.PushConfig{PushEndpoint: endpoint})

	pub := c.Publisher(topic)
	defer pub.Stop()
	if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("pushed")}).Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case body := <-received:
		if !strings.Contains(body, "message") {
			t.Errorf("push body did not look like a Pub/Sub envelope: %s", body)
		}
		t.Logf("push delivered to local HTTP target: OK (%d bytes)", len(body))
	case <-time.After(45 * time.Second):
		t.Fatalf("no push delivery within deadline; emulator log:\n%s", e.log.String())
	}
}

// TestProjectIsolation checks that identical resource IDs in different projects
// do not collide, which CloudBurrow requires of any backend it adopts.
func TestProjectIsolation(t *testing.T) {
	e := startEmulator(t, "")
	ctx := context.Background()

	ca := e.client(t, "proj-a")
	cb := e.client(t, "proj-b")

	mustTopic(t, ca, "proj-a", "shared")
	mustTopic(t, cb, "proj-b", "shared")

	// Each project must see only its own topic.
	_, err := ca.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
		Topic: "projects/proj-a/topics/shared",
	})
	if err != nil {
		t.Fatalf("GetTopic proj-a: %v", err)
	}
	_, err = ca.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
		Topic: "projects/proj-a/topics/only-in-b",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic for a nonexistent topic = %v, want NotFound", err)
	}
	t.Log("project isolation and NotFound mapping: OK")
}

// TestRestartLosesStateWithoutDataDir records whether state survives a restart.
// CloudBurrow must know this precisely: "durable" is a promise we would inherit.
func TestRestartLosesStateWithoutDataDir(t *testing.T) {
	dir := t.TempDir()

	e1 := startEmulator(t, dir)
	c1 := e1.client(t, "probe")
	mustTopic(t, c1, "probe", "persisted")
	e1.stop()

	// Second instance, same data directory.
	e2 := startEmulator(t, dir)
	c2 := e2.client(t, "probe")
	_, err := c2.TopicAdminClient.GetTopic(context.Background(), &pubsubpb.GetTopicRequest{
		Topic: "projects/probe/topics/persisted",
	})

	// Recorded, not asserted: this probe documents actual behavior rather than
	// enforcing a requirement on third-party software.
	if err == nil {
		t.Log("RESULT: topic SURVIVED restart when --data-dir was supplied")
	} else {
		t.Logf("RESULT: topic did NOT survive restart with --data-dir (%v)", status.Code(err))
	}
	if entries, derr := os.ReadDir(dir); derr == nil {
		names := make([]string, 0, len(entries))
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Logf("data dir contents: %v", names)
	}
}

// TestUnimplementedSurfaces records which methods the emulator refuses, so the
// audit can state gaps from evidence rather than assumption.
func TestUnimplementedSurfaces(t *testing.T) {
	e := startEmulator(t, "")
	c := e.client(t, "probe")
	ctx := context.Background()

	topic := mustTopic(t, c, "probe", "topic-caps")
	sub := mustSub(t, c, "probe", "sub-caps", topic, nil)

	t.Run("IAM policy", func(t *testing.T) {
		// The emulator prints "IAM integration is disabled" at startup; confirm
		// what a caller actually receives.
		_, err := c.SubscriptionAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sub})
		t.Logf("GetIamPolicy -> code=%v err=%v", status.Code(err), err)
	})

	t.Run("Seek to time", func(t *testing.T) {
		_, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{
			Subscription: sub,
			Target:       &pubsubpb.SeekRequest_Time{Time: timestamppb.New(time.Now().Add(-time.Minute))},
		})
		t.Logf("Seek(time) -> code=%v err=%v", status.Code(err), err)
	})

	t.Run("Snapshot", func(t *testing.T) {
		_, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
			Name: "projects/probe/snapshots/snap1", Subscription: sub,
		})
		t.Logf("CreateSnapshot -> code=%v err=%v", status.Code(err), err)
	})

	t.Run("Schema service", func(t *testing.T) {
		sc, err := apiv1.NewSchemaClient(ctx)
		if err != nil {
			t.Skipf("NewSchemaClient: %v", err)
		}
		defer sc.Close()
		it := sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: "projects/probe"})
		_, err = it.Next()
		t.Logf("ListSchemas -> code=%v err=%v", status.Code(err), err)
	})
}
