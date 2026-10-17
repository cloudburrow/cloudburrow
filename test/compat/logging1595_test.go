//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.LoggingServiceV2/ListLogEntries, google.logging.v2.ConfigServiceV2/CreateBucket, google.logging.v2.ConfigServiceV2/CreateSink
//
// TestLoggingOldTimestampsFollowEachBucketsRetention (#1595), with the
// official cloud.google.com/go/logging/apiv2 clients. Google's quotas page
// says the oldest timestamp an entry may have is "Determined by the
// retention period of the log bucket", and that older entries are still
// routed to sinks (https://docs.cloud.google.com/logging/quotas): an entry
// 40 days old is accepted, is not in the _Default bucket (30 days), and a
// sink routes it to a bucket of 60 days, where it is read.
func TestLoggingOldTimestampsFollowEachBucketsRetention(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := loggingClientOptions(h)
	lc, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	parent := "projects/" + h.Project()
	global := parent + "/locations/global"
	suffix := fmt.Sprint(time.Now().UnixNano())
	logID := "old-" + suffix

	bucket, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: "long-" + suffix,
		Bucket: &loggingpb.LogBucket{RetentionDays: 60}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteBucket(context.Background(), &loggingpb.DeleteBucketRequest{Name: bucket.GetName()})
	})
	sinkID := "to-long-" + suffix
	if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{
		Name: sinkID, Destination: "logging.googleapis.com/" + bucket.GetName(), Filter: fmt.Sprintf(`logName="%s/logs/%s"`, parent, logID)}}); err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + sinkID})
	})

	_, err = lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName: parent + "/logs/" + logID, Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{
			{Timestamp: timestamppb.New(time.Now().Add(-40 * 24 * time.Hour)), Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "forty days old"}},
		},
	})
	if err != nil {
		t.Fatalf("WriteLogEntries of a 40-day-old entry: %v", err)
	}
	read := func(resource string) []string {
		t.Helper()
		var got []string
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{resource},
			Filter: fmt.Sprintf(`logName="%s/logs/%s" AND timestamp>="%s"`, parent, logID, time.Now().Add(-50*24*time.Hour).UTC().Format(time.RFC3339))})
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListLogEntries(%s): %v", resource, err)
			}
			got = append(got, e.GetTextPayload())
		}
		return got
	}
	if got := read(parent); len(got) != 0 {
		t.Errorf("the _Default bucket (30 days) has %q; want nothing older than its retention", got)
	}
	if got := read(bucket.GetName() + "/views/_AllLogs"); len(got) != 1 || got[0] != "forty days old" {
		t.Errorf("the 60-day bucket has %q; want the entry the sink routed there", got)
	}
}
