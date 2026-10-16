//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// advanceLoggingClock moves the Logging clock forward by d, a CloudBurrow
// extension (POST /cloudburrow/logging/v2/clock:advance, #1404), which
// writes the Cloud Storage sinks' batches whose hour has ended.
// The sink tests run in the emulators shard, beside BigQuery, under their own
// names, so the Logging, Storage and Pub/Sub suites stay in their shards; on
// a developer's instance the shared names do.
const (
	envSinkLogging = "CLOUDBURROW_TEST_SINK_LOGGING"
	envSinkStorage = "CLOUDBURROW_TEST_SINK_STORAGE"
	envSinkPubSub  = "CLOUDBURROW_TEST_SINK_PUBSUB"
)

// sinkClients are the official clients the sink tests use.
func sinkClients(t *testing.T, h *Harness) (*vlogging.Client, *vlogging.ConfigClient, *pubsub.Client, *storage.Client) {
	t.Helper()
	logEndpoint := runShardEndpoint(h, envSinkLogging, EnvLogging)
	opts := []option.ClientOption{option.WithEndpoint(logEndpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	lc, err := vlogging.NewClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	cc, err := vlogging.NewConfigClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	t.Setenv("PUBSUB_EMULATOR_HOST", runShardEndpoint(h, envSinkPubSub, EnvPubSub))
	ps, err := pubsub.NewClient(h.Context(), h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	se := runShardEndpoint(h, envSinkStorage, EnvStorage)
	t.Setenv("STORAGE_EMULATOR_HOST", se)
	sc, err := storage.NewClient(h.Context(), option.WithoutAuthentication(), option.WithEndpoint(se+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return lc, cc, ps, sc
}

func advanceLoggingClock(t *testing.T, h *Harness, d time.Duration) time.Time {
	t.Helper()
	body, _ := json.Marshal(map[string]float64{"seconds": d.Seconds()})
	base := runShardEndpoint(h, envSinkLogging, EnvLogging)
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	resp, err := http.Post(strings.TrimSuffix(base, "/")+"/cloudburrow/logging/v2/clock:advance", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("clock:advance: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Time time.Time `json:"time"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil || got.Time.IsZero() {
		t.Fatalf("clock:advance: %s %+v", resp.Status, got)
	}
	return got.Time.UTC()
}

// covers: google.logging.v2.ConfigServiceV2/CreateSink
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
//
// TestLoggingSinksDeliverToTheirDestinations (#1404): sinks made through
// the official ConfigClient deliver the entries written through the
// official Client, which the official Pub/Sub, BigQuery and Storage
// clients read back: one Pub/Sub message per entry (the LogEntry JSON, the
// logging.googleapis.com/timestamp attribute); rows in a date-sharded table
// LOG_ID_YYYYMMDD and in a partitioned table LOG_ID, jsonPayload's fields
// as RECORD columns; and, once the Logging clock passes the hour, the
// hour's object LOG_ID/YYYY/MM/DD/HH:00:00_HH:59:59_S0.json. A sink to
// another project puts the entries in that project's _Default bucket. A
// sink to a missing topic writes a logging.googleapis.com/sink_error entry
// with error_code topic_not_found. Every sink has the project's Logging
// service agent as writer_identity.
func TestLoggingSinksDeliverToTheirDestinations(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	bq, project := bigqueryClient(t, h)
	lc, cc, ps, sc := sinkClients(t, h)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	logID := "sinklog" + suffix
	topicName := topic(t, h, ps, "sink-topic-"+suffix)
	sub := subscription(t, h, ps, "sink-sub-"+suffix, topicName)
	dsSharded, dsParted := "sinkds"+suffix, "sinkdsp"+suffix
	for _, d := range []string{dsSharded, dsParted} {
		if err := bq.Dataset(d).Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
			t.Fatal(err)
		}
		d := d
		t.Cleanup(func() { _ = bq.Dataset(d).DeleteWithContents(context.Background()) })
	}
	bucket := "sink-bucket-" + suffix
	if err := sc.Bucket(bucket).Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(context.Background(), nil)
		for {
			a, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(a.Name).Delete(context.Background())
		}
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	other := "sink-other-" + suffix
	filter := fmt.Sprintf(`logName="projects/%s/logs/%s"`, project, logID)
	parent := "projects/" + project
	sinks := map[string]*loggingpb.LogSink{
		"to-topic-" + suffix:   {Destination: "pubsub.googleapis.com/" + topicName},
		"to-sharded-" + suffix: {Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/" + dsSharded},
		"to-parted-" + suffix: {Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/" + dsParted,
			Options: &loggingpb.LogSink_BigqueryOptions{BigqueryOptions: &loggingpb.BigQueryOptions{UsePartitionedTables: true}}},
		"to-gcs-" + suffix:     {Destination: "storage.googleapis.com/" + bucket},
		"to-project-" + suffix: {Destination: "logging.googleapis.com/projects/" + other},
		"to-missing-" + suffix: {Destination: "pubsub.googleapis.com/projects/" + project + "/topics/missing-" + suffix},
	}
	for id, sk := range sinks {
		sk.Name, sk.Filter = id, filter
		got, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: sk, UniqueWriterIdentity: true})
		if err != nil {
			t.Fatalf("CreateSink %s: %v", id, err)
		}
		if want := "serviceAccount:service-" + project + "@gcp-sa-logging.iam.gserviceaccount.com"; got.GetWriterIdentity() != want {
			t.Errorf("sink %s writer_identity %q, want %q", id, got.GetWriterIdentity(), want)
		}
		id := id
		t.Cleanup(func() {
			_ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + id})
		})
	}

	// The entries' time is the Logging clock's, which earlier tests may have
	// advanced, so they are received in the hour of their timestamp.
	now := advanceLoggingClock(t, h, 0)
	payload, _ := structpb.NewStruct(map[string]any{"message": "structured", "count": 7, "detail": map[string]any{"ok": true}})
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName:  parent + "/logs/" + logID,
		Resource: &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
		Entries: []*loggingpb.LogEntry{
			{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "plain " + suffix}, Timestamp: timestamppb.New(now), InsertId: "a" + suffix},
			{Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: payload}, Timestamp: timestamppb.New(now), InsertId: "b" + suffix},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Pub/Sub: two messages.
	var msgs []*pubsubpb.ReceivedMessage
	for deadline := time.Now().Add(30 * time.Second); len(msgs) < 2 && time.Now().Before(deadline); {
		resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, resp.GetReceivedMessages()...)
	}
	if len(msgs) != 2 {
		t.Fatalf("pulled %d messages, want 2", len(msgs))
	}
	var texts []string
	for _, m := range msgs {
		var e loggingpb.LogEntry
		if err := protojson.Unmarshal(m.GetMessage().GetData(), &e); err != nil {
			t.Fatalf("message data is not LogEntry JSON: %v: %s", err, m.GetMessage().GetData())
		}
		ts, err := time.Parse(time.RFC3339Nano, m.GetMessage().GetAttributes()["logging.googleapis.com/timestamp"])
		if err != nil || !ts.Equal(e.GetTimestamp().AsTime()) {
			t.Errorf("timestamp attribute %v (%v), entry %v", m.GetMessage().GetAttributes(), err, e.GetTimestamp().AsTime())
		}
		if e.GetLogName() != parent+"/logs/"+logID {
			t.Errorf("message logName %q", e.GetLogName())
		}
		texts = append(texts, e.GetTextPayload())
	}
	if !strings.Contains(strings.Join(texts, ","), "plain "+suffix) {
		t.Errorf("messages %v", texts)
	}

	// BigQuery: the date-sharded and the partitioned table.
	readTable := func(q string) []map[string]bigquery.Value {
		t.Helper()
		var rows []map[string]bigquery.Value
		var lastErr error
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			it, err := bq.Query(q).Read(ctx)
			if err != nil {
				lastErr = err
				continue
			}
			rows = nil
			for {
				var r map[string]bigquery.Value
				err := it.Next(&r)
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, r)
			}
			if len(rows) == 2 {
				return rows
			}
		}
		t.Fatalf("%s: %d rows, last error %v", q, len(rows), lastErr)
		return nil
	}
	for _, q := range []string{
		fmt.Sprintf("SELECT insertId, textPayload, jsonPayload.message AS m, jsonPayload.count AS c, jsonPayload.detail.ok AS ok, severity, logName FROM `%s.%s.%s_%s` ORDER BY insertId",
			project, dsSharded, logID, now.Format("20060102")),
		fmt.Sprintf("SELECT insertId, textPayload, jsonPayload.message AS m, jsonPayload.count AS c, jsonPayload.detail.ok AS ok, severity, logName FROM `%s.%s.%s` ORDER BY insertId",
			project, dsParted, logID),
	} {
		rows := readTable(q)
		if rows[0]["textPayload"] != "plain "+suffix || rows[1]["m"] != "structured" || rows[1]["c"] != 7.0 || rows[1]["ok"] != true ||
			rows[0]["severity"] != "DEFAULT" || rows[0]["logName"] != parent+"/logs/"+logID {
			t.Errorf("%s: rows %v", q, rows)
		}
	}
	md, err := bq.Dataset(dsParted).Table(logID).Metadata(ctx)
	if err != nil || md.TimePartitioning == nil || md.TimePartitioning.Field != "timestamp" {
		t.Errorf("the partitioned table: %+v, %v", md, err)
	}

	// Cloud Storage: nothing before the hour ends, then the hour's object.
	objects := func() map[string]string {
		out := map[string]string{}
		it := sc.Bucket(bucket).Objects(ctx, nil)
		for {
			a, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err := sc.Bucket(bucket).Object(a.Name).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			_, _ = b.ReadFrom(r)
			r.Close()
			out[a.Name] = b.String()
		}
	}
	if got := objects(); len(got) != 0 {
		t.Errorf("objects before the hour ended: %v", got)
	}
	advanceLoggingClock(t, h, time.Hour)
	hour := now.Truncate(time.Hour)
	name := fmt.Sprintf("%s/%s/%s_%s_S0.json", logID, hour.Format("2006/01/02"), hour.Format("15:04:05"), hour.Add(time.Hour-time.Second).Format("15:04:05"))
	got := objects()
	body, ok := got[name]
	if !ok || strings.Count(body, "\n") != 2 {
		t.Fatalf("objects %v, want %s with two entries", got, name)
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		var e loggingpb.LogEntry
		if err := protojson.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("object line is not LogEntry JSON: %v", err)
		}
	}

	// The project destination: the other project's _Default bucket.
	it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + other}, Filter: filter})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("projects/%s lists %d routed entries, want 2", other, n)
	}

	// The missing topic: a sink error entry.
	var errEntry *loggingpb.LogEntry
	for deadline := time.Now().Add(30 * time.Second); errEntry == nil && time.Now().Before(deadline); time.Sleep(time.Second) {
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{parent},
			Filter: fmt.Sprintf(`resource.type="logging_sink" AND resource.labels.name="to-missing-%s"`, suffix)})
		if e, err := it.Next(); err == nil {
			errEntry = e
		}
	}
	if errEntry == nil {
		t.Fatal("no sink_error entry for the missing topic")
	}
	if errEntry.GetLogName() != parent+"/logs/logging.googleapis.com%2Fsink_error" || errEntry.GetLabels()["error_code"] != "topic_not_found" ||
		errEntry.GetSeverity().String() != "ERROR" {
		t.Errorf("sink error entry %v", errEntry)
	}
}
