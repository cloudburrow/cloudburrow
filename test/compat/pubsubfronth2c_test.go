//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// TestPubSubRESTOverH2CPriorKnowledge (#909): curl speaking plain-text
// HTTP/2 with prior knowledge (--http2-prior-knowledge) reaches the REST API
// on the Pub/Sub port, through the host tunnel, which carries gRPC on the
// same pooled HTTP/2 connections: a topic and a subscription are created
// over it, a ttl Google refuses is refused 400 by CloudBurrow's front, a
// subscription created with no policy reads back the 31-day default, and
// the official gRPC client, before and after, reads what curl made.
// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubRESTOverH2CPriorKnowledge(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not on PATH")
	}
	if out, _ := exec.Command(curl, "--version").Output(); !strings.Contains(string(out), "HTTP2") {
		t.Skipf("this curl has no HTTP/2:\n%s", out)
	}
	topicName := topic(t, h, c, "h2c-topic") // over gRPC first
	sub := "projects/" + h.Project() + "/subscriptions/h2c-sub"
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	do := func(method, path, body string) (string, int, map[string]any) {
		t.Helper()
		args := []string{"-s", "--http2-prior-knowledge", "-X", method, "-w", "\n%{http_version} %{http_code}",
			"-H", "Content-Type: application/json", "http://" + h.Endpoint(EnvPubSub) + path}
		if body != "" {
			args = append(args, "--data", body)
		}
		out, err := exec.CommandContext(ctx, curl, args...).Output()
		if err != nil {
			t.Fatalf("curl %s %s: %v", method, path, err)
		}
		s := string(out)
		i := strings.LastIndex(s, "\n")
		var version string
		var code int
		fmt.Sscanf(s[i+1:], "%s %d", &version, &code)
		got := map[string]any{}
		_ = json.Unmarshal([]byte(s[:i]), &got)
		return version, code, got
	}
	v, code, body := do(http.MethodGet, "/v1/"+topicName, "")
	if v != "2" || code != http.StatusOK || body["name"] != topicName {
		t.Errorf("GET the topic over h2c = HTTP/%s %d %v", v, code, body)
	}
	v, code, body = do(http.MethodPut, "/v1/"+sub, `{"topic":"`+topicName+`","expirationPolicy":{"ttl":"3600s"}}`)
	if e, _ := body["error"].(map[string]any); v != "2" || code != http.StatusBadRequest || e["status"] != "INVALID_ARGUMENT" {
		t.Errorf("a 1-hour ttl over h2c = HTTP/%s %d %v; want 400 INVALID_ARGUMENT", v, code, body)
	}
	v, code, body = do(http.MethodPut, "/v1/"+sub, `{"topic":"`+topicName+`","ackDeadlineSeconds":20}`)
	if v != "2" || code != http.StatusOK || body["ackDeadlineSeconds"] != float64(20) {
		t.Fatalf("PUT the subscription over h2c = HTTP/%s %d %v", v, code, body)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil || s.GetAckDeadlineSeconds() != 20 || s.GetExpirationPolicy().GetTtl().AsDuration().Hours() != 31*24 {
		t.Errorf("gRPC reads the subscription curl made as %v, %v; want a 20s deadline and the 31-day default", s, err)
	}
}

// TestPubSubRESTListOverFourMiB (#926): a REST list of subscriptions larger
// than the 4 MiB the front holds whole passes through, rewritten as it
// streams. 900 subscriptions with 64 labels each, made by the official
// client, one of them a push subscription, list in one page of about 4.9 MB
// (measured: 4859909 bytes) with pageSize=1000: every one is in it, the push
// subscription names its real endpoint, not the relay's, and the page ends
// whole. The list is read with a plain REST GET; no official client asks for
// a page this large.
// covers: google.pubsub.v1.Subscriber/ListSubscriptions
func TestPubSubRESTListOverFourMiB(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	control := h.Endpoint(EnvControl)
	topicName := topic(t, h, c, "big-list")
	t.Cleanup(func() { adminReset(t, control, "service=pubsub&project="+h.Project()) })
	labels := map[string]string{}
	for i := range 64 {
		labels[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", 63)
	}
	const n = 900
	const endpoint = "http://big-list.example.test/push"
	var wg sync.WaitGroup
	errs := make(chan error, n)
	sem := make(chan struct{}, 16)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s := &pubsubpb.Subscription{Name: fmt.Sprintf("projects/%s/subscriptions/big-%03d", h.Project(), i), Topic: topicName, Labels: labels}
			if i == n-1 {
				s.PushConfig = &pubsubpb.PushConfig{PushEndpoint: endpoint}
			}
			if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("CreateSubscription: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+h.Endpoint(EnvPubSub)+"/v1/projects/"+h.Project()+"/subscriptions?pageSize=1000", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the list = %d, %v (%d bytes)", resp.StatusCode, err, len(raw))
	}
	t.Logf("the page is %d bytes", len(raw))
	if len(raw) <= 4<<20 {
		t.Fatalf("the page is %d bytes, not over 4 MiB; the test no longer tests the streamed path", len(raw))
	}
	var page struct {
		Subscriptions []struct {
			Name       string `json:"name"`
			PushConfig struct {
				PushEndpoint string `json:"pushEndpoint"`
			} `json:"pushConfig"`
			Labels map[string]string `json:"labels"`
		} `json:"subscriptions"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("the page is not whole JSON: %v", err)
	}
	pushed := ""
	for _, s := range page.Subscriptions {
		if strings.HasSuffix(s.Name, fmt.Sprintf("big-%03d", n-1)) {
			pushed = s.PushConfig.PushEndpoint
		}
		if len(s.Labels) != 64 {
			t.Fatalf("%s lists %d labels, want 64", s.Name, len(s.Labels))
		}
	}
	if len(page.Subscriptions) != n || page.NextPageToken != "" || pushed != endpoint {
		t.Errorf("the page lists %d subscriptions (want %d), next page %q, the push endpoint as %q (want %q)",
			len(page.Subscriptions), n, page.NextPageToken, pushed, endpoint)
	}
}
