package main

import (
	"context"
	"maps"
	"regexp"
	"strings"
	"testing"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// #962: Create topic and Create subscription offer labels, and all four
// Pub/Sub forms hold them to Google's rules: the field carries the key and
// value patterns and the 64-label limit the browser checks, and a label
// Google refuses is refused before anything is sent, with the front's
// message. Good labels are created and read back by the official client.
func TestPubSubFormsHoldLabelsToGoogleRules(t *testing.T) {
	ctx := context.Background()
	const project = "label-rules"
	addr, c := pubsubEditFixture(t, project)
	p := pubsubProvider{endpoint: addr}

	check := func(where string, f console.Field) {
		t.Helper()
		if f.Type != "map" || f.KeyPattern != pubsubfront.LabelKeyPattern || f.ValuePattern != pubsubfront.LabelValuePattern ||
			f.MaxEntries != pubsubfront.MaxLabels || f.KeyHelp == "" || f.ValueHelp == "" || !strings.Contains(f.Help, "64 labels") {
			t.Errorf("%s: labels field = %+v; want Google's rules on it", where, f)
		}
	}
	_, fields := p.CreateForm()
	var found bool
	for _, f := range fields {
		if f.Name == "labels" {
			check("Create topic", f)
			found = true
		}
	}
	if !found {
		t.Error("Create topic offers no labels")
	}
	found = false
	for _, f := range subscriptionCreateFields() {
		if f.Name == "labels" {
			check("Create subscription", f)
			found = true
		}
	}
	if !found {
		t.Error("Create subscription offers no labels")
	}

	for _, bad := range []string{`{"Env":"dev"}`, `{"env":"a b"}`, `{"1k":"v"}`} {
		if _, err := p.Create(ctx, project, map[string]string{"name": "refused", "labels": bad}); err == nil ||
			!strings.Contains(err.Error(), "labels:") {
			t.Errorf("Create topic with %s: %v, want the labels refused", bad, err)
		}
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/refused"}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused topic was created: %v", err)
	}

	want := map[string]string{"env": "dev", "日本": "東京"}
	topic, err := p.Create(ctx, project, map[string]string{"name": "orders", "labels": console.FormatMap(want)})
	if err != nil {
		t.Fatalf("Create topic with good labels: %v", err)
	}
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); err != nil || !maps.Equal(got.GetLabels(), want) {
		t.Errorf("the topic reads %v, %v; want %v", got.GetLabels(), err, want)
	}

	sub := map[string]string{"name": "orders-sub", "ackDeadline": "10", "messageRetention": "7d", "maxDeliveryAttempts": "5"}
	sub["labels"] = `{"team":"A"}`
	if _, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription, sub); err == nil || !strings.Contains(err.Error(), `"A"`) {
		t.Errorf("Create subscription with an uppercase value: %v, want it refused naming the value", err)
	}
	sub["labels"] = `{"team":"a"}`
	if _, err := p.ActAtResult(ctx, project, []string{topic}, actCreateSubscription, sub); err != nil {
		t.Fatalf("Create subscription with good labels: %v", err)
	}
	subName := "projects/" + project + "/subscriptions/orders-sub"
	if s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName}); err != nil ||
		s.GetLabels()["team"] != "a" {
		t.Errorf("the subscription reads %v, %v; want team=a", s.GetLabels(), err)
	}

	for name, d := range map[string]console.Driller{topic: p, subName: pubsubSubscriptionsProvider{endpoint: addr}} {
		form := pubsubEditOf(t, d, project, name)
		f, ok := fieldNamed(form, "labels")
		if !ok {
			t.Fatalf("the edit form of %s has no labels", name)
		}
		check("Edit "+name, f)
		values := pubsubSubmitted(form)
		values["labels"] = `{"ok":"1","Bad":"2"}`
		if err := d.(console.Editor).Edit(ctx, project, []string{name}, values); err == nil || !strings.Contains(err.Error(), `"Bad"`) {
			t.Errorf("Edit %s with a bad key: %v, want it refused naming the key", name, err)
		}
	}
}

// The browser compiles a map field's key and value patterns with the v
// flag, as it does a pattern attribute, so each must be valid there (a
// literal '-' escaped) and in Go, where the rules are checked too.
func TestMapFieldPatternsAreValidInTheBrowser(t *testing.T) {
	t.Parallel()
	for service, fields := range createForms(t) {
		for _, f := range fields {
			for _, pat := range []string{f.KeyPattern, f.ValuePattern} {
				if pat == "" {
					continue
				}
				if f.Type != "map" {
					t.Errorf("%s/%s is a %q field carrying a key or value pattern, which only a map field checks", service, f.Name, f.Type)
				}
				for _, class := range characterClasses(pat) {
					if pos, bad := unescapedLiteralHyphen(class); bad {
						t.Errorf("%s/%s pattern %q has an unescaped literal '-' at %d of [%s]", service, f.Name, pat, pos, class)
					}
				}
				if _, err := regexp.Compile(pat); err != nil {
					t.Errorf("%s/%s pattern %q does not compile: %v", service, f.Name, pat, err)
				}
			}
		}
	}
}
