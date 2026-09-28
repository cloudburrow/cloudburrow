package pubsubfront

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// labelCases are labels Google's rules take and refuse
// (https://cloud.google.com/pubsub/docs/labels).
var labelCases = []struct {
	name   string
	labels map[string]string
	ok     bool
}{
	{"none", nil, true},
	{"plain", map[string]string{"env": "dev", "team_a": "x-1", "k": ""}, true},
	{"63-character key and value", map[string]string{"k" + strings.Repeat("a", 62): strings.Repeat("v", 63)}, true},
	{"international", map[string]string{"größe": "ñandú", "日本": "東京", "ключ": "значение"}, true},
	{"digits", map[string]string{"a1": "123", "k": "٣"}, true},
	{"64 labels", manyLabels(64), true},

	{"65 labels", manyLabels(65), false},
	{"empty key", map[string]string{"": "v"}, false},
	{"64-character key", map[string]string{"k" + strings.Repeat("a", 63): "v"}, false},
	{"64-character value", map[string]string{"k": strings.Repeat("v", 64)}, false},
	{"uppercase key", map[string]string{"Env": "dev"}, false},
	{"uppercase value", map[string]string{"env": "Dev"}, false},
	{"key starts with a digit", map[string]string{"1env": "dev"}, false},
	{"key starts with _", map[string]string{"_env": "dev"}, false},
	{"key starts with -", map[string]string{"-env": "dev"}, false},
	{"dot", map[string]string{"app.kubernetes.io": "x"}, false},
	{"space in value", map[string]string{"env": "a b"}, false},
	{"slash in value", map[string]string{"env": "a/b"}, false},
	{"uppercase international", map[string]string{"k": "Ñ"}, false},
	{"invalid UTF-8", map[string]string{"k": "\xff"}, false},
}

func manyLabels(n int) map[string]string {
	l := map[string]string{}
	for i := range n {
		l[fmt.Sprintf("k%02d", i)] = "v"
	}
	return l
}

// #962: CheckLabels takes what Google's rules take, and refuses the rest
// with INVALID_ARGUMENT, the code Google gives an invalid argument.
func TestCheckLabels(t *testing.T) {
	for _, c := range labelCases {
		err := CheckLabels(c.labels)
		if c.ok && err != nil {
			t.Errorf("%s: %v, want it taken", c.name, err)
		}
		if !c.ok && status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", c.name, err)
		}
	}
}

// The patterns the console's forms check with give the answers CheckLabels
// gives, label by label; the count is the form's maxEntries.
func TestLabelPatternsAgreeWithCheckLabels(t *testing.T) {
	key, value := regexp.MustCompile(LabelKeyPattern), regexp.MustCompile(LabelValuePattern)
	for _, c := range labelCases {
		if len(c.labels) > MaxLabels {
			continue
		}
		for k, v := range c.labels {
			if !utf8.ValidString(k) || !utf8.ValidString(v) {
				continue // a form's value is always UTF-8
			}
			one := map[string]string{k: v}
			byPattern := key.MatchString(k) && value.MatchString(v)
			if byCheck := CheckLabels(one) == nil; byPattern != byCheck {
				t.Errorf("%s: %q=%q: the patterns say %v, CheckLabels %v", c.name, k, v, byPattern, byCheck)
			}
		}
	}
}

// #962: CreateTopic, CreateSubscription, and an UpdateTopic or
// UpdateSubscription of labels, are refused INVALID_ARGUMENT over gRPC for
// labels Google refuses, and the emulator never sees them.
func TestGRPCRefusesLabelsGoogleRefuses(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topics, subs := fx.client.TopicAdminClient, fx.client.SubscriptionAdminClient
	bad := map[string]string{"Env": "dev"}
	topic := "projects/" + project + "/topics/bad-labels"
	if _, err := topics.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, Labels: bad}); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), `"Env"`) {
		t.Errorf("CreateTopic with a bad key: %v, want INVALID_ARGUMENT naming it", err)
	}
	if _, err := fx.upstream.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); status.Code(err) != codes.NotFound {
		t.Errorf("the refused topic reached the emulator: %v", err)
	}
	good := fx.topic(t, "good")
	if _, err := subs.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName("bad"), Topic: good,
		Labels: manyLabels(65)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSubscription with 65 labels: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("ok"), Topic: good, Labels: manyLabels(64)}); err != nil {
		t.Fatalf("CreateSubscription with 64 labels: %v", err)
	}
	if _, err := topics.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: good,
		Labels: map[string]string{"env": "a b"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateTopic with a bad value: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := subs.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{Subscription: &pubsubpb.Subscription{
		Name: subName("ok"), Labels: map[string]string{"1k": "v"}, AckDeadlineSeconds: 30},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "ack_deadline_seconds"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSubscription with a bad key: %v, want INVALID_ARGUMENT", err)
	}
	if _, ok := fx.front.keptLabels(good); ok {
		t.Error("a refused UpdateTopic kept its labels")
	}
	s, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("ok")})
	if err != nil || s.GetAckDeadlineSeconds() == 30 {
		t.Errorf("a refused UpdateSubscription reached the emulator: %v, %v", s.GetAckDeadlineSeconds(), err)
	}
	// Labels outside the mask are not the update's, and are not checked.
	if _, err := subs.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{Subscription: &pubsubpb.Subscription{
		Name: subName("ok"), Labels: bad, AckDeadlineSeconds: 20},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds"}}}); err != nil {
		t.Errorf("an update not naming labels: %v", err)
	}
}

// #962: the REST API refuses the same labels 400 INVALID_ARGUMENT, before
// the emulator is sent anything: PUT of a topic or a subscription, and a
// PATCH of labels.
func TestRESTRefusesLabelsGoogleRefuses(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("s"), Topic: topic}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/" + project
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, base + "/topics/new", `{"labels":{"Env":"dev"}}`},
		{http.MethodPut, base + "/subscriptions/new", `{"topic":"` + topic + `","labels":{"env":"a.b"}}`},
		{http.MethodPatch, base + "/topics/t", `{"topic":{"labels":{"1k":"v"}},"updateMask":"labels"}`},
		{http.MethodPatch, base + "/subscriptions/s?updateMask=labels", `{"subscription":{"labels":{"k":"` + strings.Repeat("v", 64) + `"}}}`},
	} {
		code, body := fx.rest(t, c.method, c.path, c.body)
		e, _ := body["error"].(map[string]any)
		if code != http.StatusBadRequest || e["status"] != "INVALID_ARGUMENT" {
			t.Errorf("%s %s: %d %v, want 400 INVALID_ARGUMENT", c.method, c.path, code, body)
		}
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the emulator was sent %d calls, want none", n)
	}
	if code, _ := fx.rest(t, http.MethodPut, base+"/topics/new", `{"labels":{"env":"dev"}}`); code != http.StatusOK {
		t.Errorf("PUT a topic with good labels = %d", code)
	}
}
