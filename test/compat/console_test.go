//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// EnvConsole points at the local console.
const EnvConsole = "CLOUDBURROW_TEST_CONSOLE"

func consoleAddr(t *testing.T, h *Harness) string {
	t.Helper()
	return h.Endpoint(EnvConsole)
}

func consoleDo(t *testing.T, addr, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestConsoleCreatedBucketIsVisibleToTheOfficialSDK is the claim that matters:
// the console is not a second system. A resource created through it is a real
// resource, indistinguishable from one an SDK created.
func TestConsoleCreatedBucketIsVisibleToTheOfficialSDK(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()

	bucket := h.Project() + "-console"
	code, body := consoleDo(t, addr, http.MethodPost,
		"/api/resources/storage?project="+h.Project(),
		fmt.Sprintf(`{"name":%q}`, bucket))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, body)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })

	attrs, err := sc.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatalf("the official SDK cannot see the console-created bucket: %v", err)
	}
	if attrs.Name != bucket {
		t.Errorf("attrs.Name = %q, want %q", attrs.Name, bucket)
	}

	// And it is writable, so it is a real bucket rather than a record of one.
	w := sc.Bucket(bucket).Object("proof.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("written through the SDK")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSDKCreatedTopicAppearsInTheConsole is the same claim from the other
// direction.
func TestSDKCreatedTopicAppearsInTheConsole(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)

	name := topic(t, h, ps, "console-visible-"+h.Project())

	code, body := consoleDo(t, addr, http.MethodGet,
		"/api/resources/pubsub?project="+h.Project(), "")
	if code != http.StatusOK {
		t.Fatalf("console list = %d: %s", code, body)
	}
	if !strings.Contains(body, name) {
		t.Errorf("the console did not show the SDK-created topic %s: %s", name, body)
	}
}

// TestConsoleCreatedTopicIsVisibleToTheOfficialSDK closes the round trip.
func TestConsoleCreatedTopicIsVisibleToTheOfficialSDK(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()

	id := "console-made-" + h.Project()
	code, body := consoleDo(t, addr, http.MethodPost,
		"/api/resources/pubsub?project="+h.Project(), fmt.Sprintf(`{"name":%q}`, id))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, body)
	}
	name := fmt.Sprintf("projects/%s/topics/%s", h.Project(), id)
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(),
			&pubsubpb.DeleteTopicRequest{Topic: name})
	})

	it := ps.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{
		Project: "projects/" + h.Project(),
	})
	found := false
	for {
		tp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTopics: %v", err)
		}
		if tp.GetName() == name {
			found = true
		}
	}
	if !found {
		t.Errorf("the official SDK does not see the console-created topic %s", name)
	}

	// Deleting through the console must delete the real topic.
	code, body = consoleDo(t, addr, http.MethodDelete,
		"/api/resources/pubsub?project="+h.Project()+"&name="+name, "")
	if code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, body)
	}
	if _, err := ps.TopicAdminClient.GetTopic(ctx,
		&pubsubpb.GetTopicRequest{Topic: name}); err == nil {
		t.Error("the topic survived a console delete")
	}
}

// A duplicate must surface the service's own message, not a generic failure:
// the constraint that was violated is the useful part.
func TestConsoleReportsTheServiceMessageOnFailure(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)

	id := "console-dup-" + h.Project()
	name := fmt.Sprintf("projects/%s/topics/%s", h.Project(), id)
	if _, err := ps.TopicAdminClient.CreateTopic(h.Context(),
		&pubsubpb.Topic{Name: name}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(),
			&pubsubpb.DeleteTopicRequest{Topic: name})
	})

	code, body := consoleDo(t, addr, http.MethodPost,
		"/api/resources/pubsub?project="+h.Project(), fmt.Sprintf(`{"name":%q}`, id))
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "AlreadyExists") {
		t.Errorf("the status code was lost: %s", body)
	}
	// The transport envelope must not reach the screen.
	if strings.Contains(body, "rpc error") || strings.Contains(body, "desc =") {
		t.Errorf("the gRPC envelope reached the screen: %s", body)
	}
}

// A queue's available actions follow its state: an action that would do
// nothing is indistinguishable from one that is broken.
func TestConsoleQueueActionsFollowState(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)

	id := "console-queue"
	code, body := consoleDo(t, addr, http.MethodPost,
		"/api/resources/tasks?project="+h.Project(),
		fmt.Sprintf(`{"name":%q,"location":"us-central1"}`, id))
	if code != http.StatusOK {
		t.Fatalf("create queue = %d: %s", code, body)
	}
	name := fmt.Sprintf("projects/%s/locations/us-central1/queues/%s", h.Project(), id)
	t.Cleanup(func() {
		_, _ = consoleDo(t, addr, http.MethodDelete,
			"/api/resources/tasks?project="+h.Project()+"&name="+name, "")
	})

	actions := queueActions(t, addr, h.Project())
	if !contains(actions, "pause") {
		t.Fatalf("a running queue offers %v, want pause", actions)
	}

	code, body = consoleDo(t, addr, http.MethodPost,
		"/api/actions/tasks?project="+h.Project(),
		fmt.Sprintf(`{"Name":%q,"Action":"pause"}`, name))
	if code != http.StatusOK {
		t.Fatalf("pause = %d: %s", code, body)
	}

	actions = queueActions(t, addr, h.Project())
	if !contains(actions, "resume") {
		t.Errorf("a paused queue offers %v, want resume", actions)
	}
	if contains(actions, "pause") {
		t.Errorf("a paused queue still offers pause: %v", actions)
	}
}

func queueActions(t *testing.T, addr, project string) []string {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet,
		"/api/resources/tasks?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("list queues = %d: %s", code, body)
	}
	var listing struct {
		Items []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Actions []struct {
				ID          string `json:"id"`
				Destructive bool   `json:"destructive"`
			} `json:"actions"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(listing.Items) == 0 {
		t.Fatal("no queues listed")
	}
	var out []string
	for _, a := range listing.Items[0].Actions {
		out = append(out, a.ID)
		if a.ID == "purge" && !a.Destructive {
			t.Error("purge is not marked destructive, so the client cannot confirm it")
		}
	}
	return out
}

func contains(items []string, want string) bool {
	for _, i := range items {
		if i == want {
			return true
		}
	}
	return false
}

// The console must not accept a create it cannot scope, because a resource in
// the wrong project is worse than one that was not created.
func TestConsoleRefusesAnUnscopedCreate(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)

	code, body := consoleDo(t, addr, http.MethodPost,
		"/api/resources/storage", `{"name":"orphan-bucket"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unscoped create = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "project") {
		t.Errorf("the reason does not mention the project: %s", body)
	}
}

// Ensure the storage client is genuinely the official one.
var _ = storage.Client{}
