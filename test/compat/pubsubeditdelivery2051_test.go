//go:build compat

package compat

// Changing an existing subscription's delivery type (#2051), through the
// official pubsub/v2 client's UpdateSubscription, as Google documents it:
// "Clear the configuration for the current delivery type and set the
// configuration for the new type"
// (https://docs.cloud.google.com/pubsub/docs/manage-subscriptions). A pull
// subscription switched to BigQuery writes the next message as a row; then
// switched to Cloud Storage it writes an object, and the table gets no more
// rows; then switched back to pull, the next message is pulled. It runs
// beside BigQuery and Cloud Storage, as the export tests do (#1874).

import (
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestPubSubUpdateSubscriptionSwitchesDeliveryType(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, sc := exportClients(t, h)
	ctx := h.Context()
	admin := ps.SubscriptionAdminClient

	ds := bq.Dataset("sw_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	table := ds.Table("rows")
	if err := table.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "data", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	tableID := bqProject + "." + ds.DatasetID + "." + table.TableID
	bucket := sc.Bucket(h.Project() + "-switch")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(ctx, nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(ctx)
		}
		_ = bucket.Delete(ctx)
	})

	topic := "projects/" + h.Project() + "/topics/switch"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	sub := "projects/" + h.Project() + "/subscriptions/switch"
	if _, err := admin.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	publish := func(d string) {
		t.Helper()
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(d)}}}); err != nil {
			t.Fatal(err)
		}
	}
	update := func(s *pubsubpb.Subscription, paths ...string) (*pubsubpb.Subscription, error) {
		s.Name = sub
		return admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{Subscription: s,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	rows := func() []string {
		var out []string
		it := table.Read(ctx)
		for {
			var row map[string]bigquery.Value
			if err := it.Next(&row); err != nil {
				return out
			}
			out = append(out, row["data"].(string))
		}
	}

	// Refused as on create: a missing table, two deliveries at once.
	if _, err := update(&pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{
		Table: bqProject + "." + ds.DatasetID + ".missing"}}, "bigquery_config"); status.Code(err) != codes.NotFound {
		t.Errorf("to a missing table: %v, want NotFound", err)
	}
	if _, err := update(&pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID},
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: bucket.BucketName()}},
		"bigquery_config", "cloud_storage_config"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("to BigQuery and Cloud Storage at once: %v, want InvalidArgument", err)
	}
	if s, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil ||
		s.GetBigqueryConfig() != nil || s.GetCloudStorageConfig() != nil {
		t.Fatalf("after the refusals: %v, %v; want a pull subscription still", s, err)
	}

	// Pull to BigQuery.
	s, err := update(&pubsubpb.Subscription{BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID}}, "bigquery_config")
	if err != nil {
		t.Fatalf("pull to BigQuery: %v", err)
	}
	if s.GetBigqueryConfig().GetTable() != tableID || s.GetBigqueryConfig().GetState() != pubsubpb.BigQueryConfig_ACTIVE {
		t.Errorf("pull to BigQuery answered %v", s.GetBigqueryConfig())
	}
	if g, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil || g.GetBigqueryConfig().GetTable() != tableID {
		t.Errorf("GetSubscription after the switch to BigQuery: %v, %v", g, err)
	}
	publish("to-bigquery")
	eventually1874(t, "the row in BigQuery", 60*time.Second, func() bool {
		r := rows()
		return len(r) == 1 && r[0] == "to-bigquery"
	})

	// BigQuery to Cloud Storage, in one update clearing one and setting the
	// other.
	s, err = update(&pubsubpb.Subscription{CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: bucket.BucketName(),
		FilenamePrefix: "switch/", OutputFormat: &pubsubpb.CloudStorageConfig_TextConfig_{TextConfig: &pubsubpb.CloudStorageConfig_TextConfig{}}}},
		"bigquery_config", "cloud_storage_config")
	if err != nil {
		t.Fatalf("BigQuery to Cloud Storage: %v", err)
	}
	if s.GetBigqueryConfig() != nil || s.GetCloudStorageConfig().GetBucket() != bucket.BucketName() ||
		s.GetCloudStorageConfig().GetMaxDuration().AsDuration() != 5*time.Minute {
		t.Errorf("BigQuery to Cloud Storage answered %v", s)
	}
	// The BigQuery delivery stops at the next poll.
	time.Sleep(2 * time.Second)
	publish("to-storage")
	objects := func() []string {
		var out []string
		it := bucket.Objects(ctx, &storage.Query{Prefix: "switch/"})
		for {
			o, err := it.Next()
			if err == iterator.Done || err != nil {
				return out
			}
			out = append(out, o.Name)
		}
	}
	time.Sleep(3 * time.Second)
	advanceExportClock(t, h, 6*time.Minute)
	eventually1874(t, "the object in Cloud Storage", 30*time.Second, func() bool { return len(objects()) == 1 })
	r, err := bucket.Object(objects()[0]).NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "to-storage" {
		t.Errorf("object = %q, want the message", b)
	}
	if got := rows(); len(got) != 1 {
		t.Errorf("rows after the switch to Cloud Storage = %v, want the first only", got)
	}

	// Cloud Storage back to pull.
	s, err = update(&pubsubpb.Subscription{}, "cloud_storage_config")
	if err != nil {
		t.Fatalf("Cloud Storage to pull: %v", err)
	}
	if s.GetBigqueryConfig() != nil || s.GetCloudStorageConfig() != nil || s.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("Cloud Storage to pull answered %v", s)
	}
	time.Sleep(2 * time.Second)
	publish("to-pull")
	eventually1874(t, "the pulled message", 30*time.Second, func() bool {
		r, err := admin.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil || len(r.GetReceivedMessages()) == 0 {
			return false
		}
		m := r.GetReceivedMessages()[0]
		if string(m.GetMessage().GetData()) != "to-pull" {
			t.Errorf("pulled %q, want to-pull", m.GetMessage().GetData())
		}
		_ = admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m.GetAckId()}})
		return true
	})
	l := admin.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + h.Project()})
	for {
		ls, err := l.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ls.GetName() == sub && (ls.GetBigqueryConfig() != nil || ls.GetCloudStorageConfig() != nil) {
			t.Errorf("ListSubscriptions after the switch to pull: %v", ls)
		}
	}

	// An export subscription Pub/Sub created as one switches to pull too,
	// and exactly-once delivery is refused on an export, as Google refuses
	// it.
	made := "projects/" + h.Project() + "/subscriptions/switch-made"
	if _, err := admin.CreateSubscription(ctx, &pubsubpb.Subscription{Name: made, Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: made, EnableExactlyOnceDelivery: true},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"enable_exactly_once_delivery"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("exactly-once on a BigQuery subscription: %v, want InvalidArgument", err)
	}
	if s, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: made}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bigquery_config"}}}); err != nil ||
		s.GetBigqueryConfig() != nil {
		t.Errorf("a created BigQuery subscription to pull: %v, %v", s, err)
	}
	publish("made-pull")
	eventually1874(t, "the pull from the former BigQuery subscription", 30*time.Second, func() bool {
		r, err := admin.Pull(ctx, &pubsubpb.PullRequest{Subscription: made, MaxMessages: 10})
		if err != nil {
			return false
		}
		for _, m := range r.GetReceivedMessages() {
			_ = admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: made, AckIds: []string{m.GetAckId()}})
			if string(m.GetMessage().GetData()) == "made-pull" {
				return true
			}
		}
		return false
	})

	// And on to push, which the emulator applies on the copy it made as a
	// BigQuery subscription, then back to pull.
	pushed, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: made, PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:9/push"}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"push_config"}}})
	if err != nil || pushed.GetPushConfig().GetPushEndpoint() != "http://127.0.0.1:9/push" || pushed.GetBigqueryConfig() != nil {
		t.Errorf("the former BigQuery subscription to push: %v, %v", pushed, err)
	}
	if _, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: made, BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"bigquery_config"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a push subscription to BigQuery without clearing its endpoint: %v, want InvalidArgument", err)
	}
	if s, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: made, BigqueryConfig: &pubsubpb.BigQueryConfig{Table: tableID}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"push_config", "bigquery_config"}}}); err != nil ||
		s.GetPushConfig().GetPushEndpoint() != "" || s.GetBigqueryConfig().GetTable() != tableID {
		t.Errorf("push to BigQuery, clearing the endpoint: %v, %v", s, err)
	}
}
