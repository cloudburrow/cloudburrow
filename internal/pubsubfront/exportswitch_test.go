package pubsubfront

import (
	"context"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #2051: UpdateSubscription switches a pull subscription to BigQuery and
// back, the front keeping the export configuration the emulator refuses;
// the delivery starts and stops with it, and the state file keeps it.
func TestUpdateSubscriptionSwitchesToBigQueryAndBack(t *testing.T) {
	fx, d := exportFixture(t)
	d.columns = []bqField{{Name: "data", Type: "STRING"}}
	topic := fx.topic(t, "sw")
	name := subName("sw")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := fx.client.SubscriptionAdminClient
	update := func(s *pubsubpb.Subscription, paths ...string) (*pubsubpb.Subscription, error) {
		s.Name = name
		return admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{Subscription: s,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	if _, err := update(&pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.missing"}},
		"bigquery_config"); status.Code(err) != codes.NotFound {
		t.Errorf("to a missing table: %v, want NotFound", err)
	}
	s, err := update(&pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t"}}, "bigquery_config")
	if err != nil || s.GetBigqueryConfig().GetTable() != "p.d.t" || s.GetBigqueryConfig().GetState() != pubsubpb.BigQueryConfig_ACTIVE {
		t.Fatalf("pull to BigQuery: %v, %v", s, err)
	}
	if g, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); err != nil || g.GetBigqueryConfig().GetTable() != "p.d.t" {
		t.Errorf("GetSubscription: %v, %v", g, err)
	}
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte("row")})
	waitFor(t, "the row", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.rows) == 1 })
	if _, err := update(&pubsubpb.Subscription{EnableExactlyOnceDelivery: true}, "enable_exactly_once_delivery"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("exactly-once on the switched subscription: %v, want InvalidArgument", err)
	}

	s, err = update(&pubsubpb.Subscription{}, "bigquery_config")
	if err != nil || s.GetBigqueryConfig() != nil {
		t.Fatalf("BigQuery to pull: %v, %v", s, err)
	}
	waitFor(t, "the delivery to stop", func() bool {
		fx.front.exp.mu.Lock()
		defer fx.front.exp.mu.Unlock()
		_, running := fx.front.exp.workers[name]
		return !running
	})
	publish(t, fx, topic, &pubsubpb.PubsubMessage{Data: []byte("pulled")})
	r, err := admin.Pull(ctx, &pubsubpb.PullRequest{Subscription: name, MaxMessages: 10})
	if err != nil || len(r.GetReceivedMessages()) != 1 || string(r.GetReceivedMessages()[0].GetMessage().GetData()) != "pulled" {
		t.Errorf("pull after the switch back: %v, %v", r, err)
	}
	d.mu.Lock()
	if len(d.rows) != 1 {
		t.Errorf("%d rows, want the first only", len(d.rows))
	}
	d.mu.Unlock()
}

// The kept configuration survives a restart of the front, by its state file.
func TestExportSwitchIsKeptInTheStateFile(t *testing.T) {
	f, err := New("127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	name := "projects/" + project + "/subscriptions/kept"
	f.statePath = t.TempDir() + "/state.json"
	f.setExport(name, &pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t"}})
	g, err := New("127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.KeepState(f.statePath); err != nil {
		t.Fatal(err)
	}
	s := &pubsubpb.Subscription{Name: name, CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: "b"}}
	if !g.withExport(s) || s.GetBigqueryConfig().GetTable() != "p.d.t" || s.GetCloudStorageConfig() != nil {
		t.Errorf("restored configuration = %v", s)
	}
	g.forget(name)
	if s := (&pubsubpb.Subscription{Name: name}); g.withExport(s) {
		t.Errorf("a deleted subscription kept its configuration: %v", s)
	}
}
