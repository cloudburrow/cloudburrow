//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingPartialSuccessAndScopedPaging (#583), with the official
// cloud.google.com/go/logging/apiv2 client: partial_success writes the valid
// entries and reports the invalid one by index in
// WriteLogEntriesPartialErrors; an entry over 256 KiB is INVALID_ARGUMENT; a
// walk of small pages returns every entry once; and a page token replayed
// with another filter is INVALID_ARGUMENT. The codes follow Google's
// documentation (https://cloud.google.com/logging/quotas and the
// google.logging.v2 protos); documented, not measured against Google.
func TestLoggingPartialSuccessAndScopedPaging(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := vlogging.NewClient(ctx, option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := "projects/" + h.Project()
	logName := parent + "/logs/compat-fidelity"
	global := &mrpb.MonitoredResource{Type: "global"}
	text := func(s string) *loggingpb.LogEntry {
		return &loggingpb.LogEntry{Resource: global, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: s}}
	}

	orphan := text("orphan")
	orphan.Resource = nil
	_, err = c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName: logName, PartialSuccess: true,
		Entries: []*loggingpb.LogEntry{text("e0"), orphan, text("e2"), text("e3"), text("e4")},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("partial write = %v, want InvalidArgument", err)
	}
	var failed map[int32]bool
	for _, d := range st.Details() {
		if p, ok := d.(*loggingpb.WriteLogEntriesPartialErrors); ok {
			failed = map[int32]bool{}
			for i := range p.GetLogEntryErrors() {
				failed[i] = true
			}
		}
	}
	if len(failed) != 1 || !failed[1] {
		t.Errorf("WriteLogEntriesPartialErrors indexes = %v, want only 1", failed)
	}

	_, err = c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName: logName, Entries: []*loggingpb.LogEntry{text(strings.Repeat("x", 257<<10))}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an entry over 256 KiB = %v, want InvalidArgument", err)
	}

	filter := `logName = "` + logName + `"`
	it := c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{parent}, Filter: filter, PageSize: 2})
	var got []string
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLogEntries: %v", err)
		}
		got = append(got, e.GetTextPayload())
	}
	if fmt.Sprint(got) != "[e0 e2 e3 e4]" {
		t.Errorf("paged walk = %v, want [e0 e2 e3 e4] each once", got)
	}

	list := func(f string) *vlogging.LogEntryIterator {
		return c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{parent}, Filter: f})
	}
	var first []*loggingpb.LogEntry
	tok, err := iterator.NewPager(list(filter), 2, "").NextPage(&first)
	if err != nil || tok == "" {
		t.Fatalf("first page: %v, token %q", err, tok)
	}
	var replay []*loggingpb.LogEntry
	if _, err := iterator.NewPager(list(filter+" severity >= WARNING"), 2, tok).NextPage(&replay); status.Code(err) != codes.InvalidArgument {
		t.Errorf("token replayed with another filter = %v, want InvalidArgument", err)
	}
}
