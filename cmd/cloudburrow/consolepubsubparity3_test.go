package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// infoTab is a row's info panel tab by ID.
func infoTab(r console.Resource, id string) (console.InfoTab, bool) {
	for _, t := range r.Info {
		if t.ID == id {
			return t, true
		}
	}
	return console.InfoTab{}, false
}

// The Topics, Subscriptions and Snapshots lists open the info panel on a
// checked row (#1847): a topic's and a subscription's Permissions and Labels
// tabs, a snapshot's Labels; each Labels tab saves through the action route's
// edit-labels, which the official client reads back, and a label against
// Google's rules is refused.
func TestPubSubListsInfoPanelLabelsAndPermissions(t *testing.T) {
	ctx := context.Background()
	const project = "info-proj"
	addr, _, c := newSnapshotFake(t, project)
	topics := pubsubProvider{endpoint: addr}
	subs := pubsubSubscriptionsProvider{endpoint: addr}
	snaps := pubsubSnapshotsProvider{endpoint: addr}
	topic := "projects/" + project + "/topics/t"
	sub := "projects/" + project + "/subscriptions/s"
	snap := "projects/" + project + "/snapshots/n"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, Labels: map[string]string{"team": "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sub}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what        string
		list        func() (console.Listing, error)
		permissions bool
	}{
		{"Topics", func() (console.Listing, error) { return topics.List(ctx, project) }, true},
		{"Subscriptions", func() (console.Listing, error) { return subs.List(ctx, project) }, true},
		{"Snapshots", func() (console.Listing, error) { return snaps.List(ctx, project) }, false},
	} {
		l, err := tc.list()
		if err != nil || !l.InfoOnSelect || len(l.Items) != 1 {
			t.Fatalf("%s list = %+v, %v; want one row, its checkbox opening the info panel", tc.what, l, err)
		}
		row := l.Items[0]
		if p, ok := infoTab(row, "permissions"); ok != tc.permissions || (ok && !p.Permissions) {
			t.Errorf("%s row's Permissions tab = %+v (%v); want %v", tc.what, p, ok, tc.permissions)
		}
		lt, ok := infoTab(row, "labels")
		if !ok || lt.Form == nil || lt.Form.ID != actEditLabels || !lt.Form.Linked || lt.Form.Submit != "Save" {
			t.Errorf("%s row's Labels tab = %+v; want its Save form", tc.what, lt)
		}
	}
	if l, _ := topics.List(ctx, project); l.Items[0].Info[1].Properties[0] != (console.Property{Label: "team", Value: "a"}) {
		t.Errorf("the topic's Labels tab shows %+v; want team=a", l.Items[0].Info[1].Properties)
	}

	// Each Labels tab's action is offered at the row's path and applied.
	offered := func(acts []console.Action) bool {
		for _, a := range acts {
			if a.ID == actEditLabels && a.Linked {
				return true
			}
		}
		return false
	}
	if !offered(topics.DetailActions(ctx, project, []string{topic})) || !offered(subs.DetailActions(ctx, project, []string{sub})) ||
		!offered(snaps.DetailActions(ctx, project, []string{snap})) {
		t.Fatal("a Labels tab's edit-labels is not offered at its row's path")
	}
	if err := topics.ActAt(ctx, project, []string{topic}, actEditLabels, map[string]string{"labels": `{"env":"dev"}`}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); len(got.GetLabels()) != 1 || got.GetLabels()["env"] != "dev" {
		t.Errorf("after the topic's Labels tab GetTopic reads %v", got.GetLabels())
	}
	if err := subs.ActAt(ctx, project, []string{sub}, actEditLabels, map[string]string{"labels": `{"env":"qa"}`}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); got.GetLabels()["env"] != "qa" {
		t.Errorf("after the subscription's Labels tab GetSubscription reads %v", got.GetLabels())
	}
	// Emptied, the labels are removed.
	if err := topics.ActAt(ctx, project, []string{topic}, actEditLabels, map[string]string{"labels": ""}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); len(got.GetLabels()) != 0 {
		t.Errorf("after emptying the topic's Labels tab GetTopic reads %v", got.GetLabels())
	}
	if err := topics.ActAt(ctx, project, []string{topic}, actEditLabels, map[string]string{"labels": `{"Bad":"x"}`}); err == nil {
		t.Error("a label key with a capital letter was accepted")
	}
	if snaps.DetailActions(ctx, project, []string{"projects/other/snapshots/n"}) != nil {
		t.Error("another project's snapshot is offered edit-labels")
	}
}

// Edit topic's page reads the retention as days, hours and minutes, sends
// a schema only while Use a schema is checked, and shows the encryption
// fixed (#1847).
func TestPubSubEditTopicPageValues(t *testing.T) {
	form := topicEditForm(&pubsubpb.Topic{Name: "projects/p/topics/t", MessageRetentionDuration: durationpb.New(26*time.Hour + 30*time.Minute),
		SchemaSettings: &pubsubpb.SchemaSettings{Schema: "projects/p/schemas/s", Encoding: pubsubpb.Encoding_BINARY}, KmsKeyName: "k"})
	got := map[string]console.Field{}
	for _, f := range form.Fields {
		got[f.Name] = f
	}
	if got["retentionDays"].Default != "1" || got["retentionHours"].Default != "2" || got["retentionMinutes"].Default != "30" ||
		got["enableRetention"].Default != "true" || got["useSchema"].Default != "true" || got["schemaEncoding"].Default != encodingBinary ||
		!got["encryption"].Immutable || got["encryption"].Default != encryptionKMS || !got["kmsKeyName"].Immutable {
		t.Errorf("Edit topic's fields = %+v", got)
	}
	out, err := topicEditValues(map[string]string{"useSchema": "false", "schema": "s", "schemaEncoding": encodingBinary,
		"enableRetention": "true", "retentionDays": "0", "retentionHours": "1", "retentionMinutes": "0", "labels": ""})
	if err != nil || out["schema"] != "" || out["messageRetention"] != time.Hour.String() || out["labels"] != "" {
		t.Errorf("topicEditValues = %v, %v", out, err)
	}
	if _, has := out["useSchema"]; has {
		t.Errorf("topicEditValues kept the page's own fields: %v", out)
	}
	out, _ = topicEditValues(map[string]string{"useSchema": "true", "schema": "s", "schemaEncoding": encodingBinary, "enableRetention": "false"})
	if out["schema"] != "s" || out["schemaEncoding"] != "BINARY" {
		t.Errorf("topicEditValues with a schema = %v", out)
	}
}

// expirationDefaults is Google's 31 days for no policy, Never expire for
// one without a ttl, and whole days up.
func TestPubSubExpirationDefaults(t *testing.T) {
	for _, tc := range []struct {
		p          *pubsubpb.ExpirationPolicy
		mode, days string
	}{
		{nil, expireAfterDays, "31"},
		{&pubsubpb.ExpirationPolicy{}, expireNever, "31"},
		{&pubsubpb.ExpirationPolicy{Ttl: durationpb.New(72 * time.Hour)}, expireAfterDays, "3"},
		{&pubsubpb.ExpirationPolicy{Ttl: durationpb.New(36 * time.Hour)}, expireAfterDays, ""},
	} {
		if m, d := expirationDefaults(tc.p); m != tc.mode || d != tc.days {
			t.Errorf("expirationDefaults(%v) = %s %s; want %s %s", tc.p, m, d, tc.mode, tc.days)
		}
	}
	// A 36h period (#2095): no whole number of days, so the days are
	// empty and the field says the period exactly; saved with the days
	// untouched it is left as it is.
	cur := &pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t",
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(36 * time.Hour)}}
	if got := expirationWords(cur.GetExpirationPolicy()); got != "36 hours" {
		t.Errorf("a 36h period is listed as %q; want 36 hours", got)
	}
	for _, f := range subscriptionEditForm(cur).Fields {
		if f.Name == "expirationDays" && (f.Default != "" || !strings.HasPrefix(f.Help, "Now 36 hours, not a whole number of days") ||
			f.Pattern != `^[0-9]{1,5}$`) {
			t.Errorf("Edit's Days for a 36h period is %+v; want empty, saying 36 hours, whole days only", f)
		}
	}
	out, err := subscriptionEditValues("p", cur, map[string]string{"deliveryType": deliveryPull, "expirationMode": expireAfterDays, "expirationDays": ""})
	if _, has := out["expiration"]; err != nil || has {
		t.Errorf("an untouched 36h period sends %v, %v; want no expiration", out, err)
	}
	out, _ = subscriptionEditValues("p", cur, map[string]string{"deliveryType": deliveryPull, "expirationMode": expireAfterDays, "expirationDays": "2"})
	if out["expiration"] != "2d" {
		t.Errorf("2 days typed over a 36h period sends %v; want 2d", out)
	}
	out, _ = subscriptionEditValues("p", cur, map[string]string{"deliveryType": deliveryPull, "expirationMode": expireAfterDays, "expirationDays": "5"})
	if out["expiration"] != "5d" || out["pushEndpoint"] != "" {
		t.Errorf("a changed period sends %v; want 5d and no push endpoint", out)
	}
}
