//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/chromedp/chromedp"
)

const envPubSub = "CLOUDBURROW_TEST_PUBSUB"

// pubsubSDK is the official pubsub/v2 client against the instance's Pub/Sub,
// through PUBSUB_EMULATOR_HOST as an SDK user points it there; the test skips
// when the instance's Pub/Sub endpoint is not set.
func pubsubSDK(t *testing.T, project string) *pubsub.Client {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envPubSub)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the official Pub/Sub client", envPubSub)
	}
	t.Setenv("PUBSUB_EMULATOR_HOST", addr)
	c, err := pubsub.NewClient(context.Background(), project)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestPubSubDetachedSubscriptionEditThroughTheBrowser (#1406, #1446), in the
// storage shard, whose instance serves Pub/Sub. On a subscription's page,
// Detach from topic asks for the subscription's name, and typed, detaches
// it: the official client's GetSubscription reads it detached, and the page
// says it receives no messages and offers no snapshot action. Edit
// subscription is still offered on the detached subscription, with its topic
// disabled; a new ack deadline and labels saved through it are what
// GetSubscription reads, still detached.
func TestPubSubDetachedSubscriptionEditThroughTheBrowser(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	project := uniqueProject(t)
	c := pubsubSDK(t, project)
	ctx := context.Background()
	p := open(t)
	topic := "projects/" + project + "/topics/browser-detach"
	sub := "projects/" + project + "/subscriptions/browser-detach-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-detach","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})
	pageButton := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === %q)`, label)
	}

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	p.waitFor(pageButton("Detach from topic"))
	p.clickText("#view .page-actions button", "Detach from topic")
	p.waitFor(`document.querySelector("#confirm-input") !== null`)
	var warning string
	p.eval(`document.querySelector("#confirm-input").closest("form").textContent`, &warning)
	if !strings.Contains(warning, "receives nothing published") {
		t.Errorf("the detach confirmation reads %q; want it to say the subscription receives nothing more", warning)
	}
	// The confirmation adds "This cannot be undone." itself; the action's own
	// text said it too, so it read twice (fixed with this test).
	if n := strings.Count(warning, "This cannot be undone."); n != 1 {
		t.Errorf("the detach confirmation says %d times that it cannot be undone: %q", n, warning)
	}
	p.run(chromedp.SendKeys(`#confirm-input`, sub, chromedp.ByQuery))
	p.clickText(`form:has(#confirm-input) .modal-actions button`, "Detach from topic")
	p.waitFor(`document.querySelector(".modal") === null`)

	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatalf("GetSubscription after the browser's detach: %v", err)
	}
	if !got.GetDetached() {
		t.Fatalf("after Detach from topic, GetSubscription reads detached=false: %v", got)
	}

	// The page reloads detached: it says so, and offers no snapshot.
	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	p.waitFor(`document.querySelector("#view").textContent.includes("it receives no messages")`)
	p.waitFor(pageButton("Edit subscription"))
	var offers bool
	p.eval(pageButton("Create snapshot")+` || `+pageButton("Detach from topic"), &offers)
	if offers {
		t.Error("a detached subscription's page still offers Create snapshot or Detach from topic")
	}

	// Edit subscription on the detached subscription (#1446).
	p.clickText("#view .page-actions button", "Edit subscription")
	p.waitFor(`document.querySelector(".modal.is-open #f-ackDeadline") !== null`)
	var topicDisabled bool
	p.eval(`document.querySelector(".modal.is-open #f-topic").disabled`, &topicDisabled)
	if !topicDisabled {
		t.Error("Edit subscription on a detached subscription lets its topic be changed")
	}
	set := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, v), nil)
	}
	set("f-ackDeadline", "45")
	set("f-labels", "team=blue")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub-subscriptions"); len(sent) != 1 {
		t.Errorf("the edit sent %d PATCHes, want 1: %v", len(sent), sent)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
		if err != nil {
			t.Fatalf("GetSubscription after the browser's edit: %v", err)
		}
		if got.GetAckDeadlineSeconds() == 45 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got.GetAckDeadlineSeconds() != 45 || got.GetLabels()["team"] != "blue" || !got.GetDetached() {
		t.Errorf("after Edit subscription, GetSubscription reads ack %d, labels %v, detached %v; want 45, team=blue, detached",
			got.GetAckDeadlineSeconds(), got.GetLabels(), got.GetDetached())
	}
}
