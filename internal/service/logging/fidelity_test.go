package logging

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var global = &mrpb.MonitoredResource{Type: "global"}

func textEntry(msg string) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: msg}}
}

func texts(st *Store) []string {
	var out []string
	for _, e := range st.snapshot() {
		out = append(out, e.GetTextPayload())
	}
	return out
}

// With partial_success, the valid entries are written and the invalid one is
// reported by index in WriteLogEntriesPartialErrors, as the proto documents.
func TestPartialSuccessWritesTheValidEntries(t *testing.T) {
	st := NewStore(100)
	s := NewServer(st)
	noResource := textEntry("orphan")
	req := &loggingpb.WriteLogEntriesRequest{
		LogName:        "projects/" + project + "/logs/app",
		PartialSuccess: true,
		Entries: []*loggingpb.LogEntry{
			{Resource: global, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "one"}},
			noResource,
			{Resource: global, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "three"}},
		},
	}
	_, err := s.WriteLogEntries(context.Background(), req)
	stErr, _ := status.FromError(err)
	if stErr.Code() != codes.InvalidArgument {
		t.Fatalf("WriteLogEntries = %v, want InvalidArgument", err)
	}
	var partial *loggingpb.WriteLogEntriesPartialErrors
	for _, d := range stErr.Details() {
		if p, ok := d.(*loggingpb.WriteLogEntriesPartialErrors); ok {
			partial = p
		}
	}
	if partial == nil || len(partial.GetLogEntryErrors()) != 1 {
		t.Fatalf("details = %v, want WriteLogEntriesPartialErrors with one entry", stErr.Details())
	}
	e1 := partial.GetLogEntryErrors()[1]
	if codes.Code(e1.GetCode()) != codes.InvalidArgument || !strings.Contains(e1.GetMessage(), "monitored resource") {
		t.Errorf("log_entry_errors[1] = %v, want InvalidArgument about the resource", e1)
	}
	if got := strings.Join(texts(st), ","); got != "one,three" {
		t.Errorf("stored %q, want one,three", got)
	}

	// Without partial_success the batch is all or nothing.
	st.Reset()
	req.PartialSuccess = false
	if _, err := s.WriteLogEntries(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("all-or-nothing write = %v, want InvalidArgument", err)
	}
	if n := len(st.snapshot()); n != 0 {
		t.Errorf("all-or-nothing write stored %d entries, want 0", n)
	}

	// Every entry failing: nothing written and no per-entry details.
	req.PartialSuccess = true
	req.Entries = []*loggingpb.LogEntry{noResource}
	_, err = s.WriteLogEntries(context.Background(), req)
	if stErr, _ := status.FromError(err); stErr.Code() != codes.InvalidArgument || len(stErr.Details()) != 0 {
		t.Errorf("all-failed partial write = %v (details %v), want InvalidArgument without details", err, stErr.Details())
	}
}

func TestEntryAndRequestLimits(t *testing.T) {
	st := NewStore(100)
	s := NewServer(st)
	fixed := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	write := func(es ...*loggingpb.LogEntry) error {
		_, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
			LogName: "projects/" + project + "/logs/app", Resource: global, Entries: es})
		return err
	}

	if err := write(textEntry(strings.Repeat("x", MaxEntryBytes))); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "256 KiB") {
		t.Errorf("an entry over 256 KiB = %v, want InvalidArgument naming the limit", err)
	}
	if err := write(textEntry(strings.Repeat("x", 200<<10))); err != nil {
		t.Errorf("a 200 KiB entry = %v, want it accepted", err)
	}
	var big []*loggingpb.LogEntry
	for i := 0; i < 50; i++ {
		big = append(big, textEntry(strings.Repeat("y", 250<<10)))
	}
	if err := write(big...); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "10 MB") {
		t.Errorf("a request over 10 MB = %v, want InvalidArgument naming the limit", err)
	}

	future := textEntry("future")
	future.Timestamp = timestamppb.New(fixed.Add(25 * time.Hour))
	if err := write(future); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "future") {
		t.Errorf("a timestamp 25h ahead = %v, want InvalidArgument", err)
	}
	soon := textEntry("soon")
	soon.Timestamp = timestamppb.New(fixed.Add(23 * time.Hour))
	old := textEntry("old")
	old.Timestamp = timestamppb.New(fixed.Add(-31 * 24 * time.Hour))
	st.Reset()
	if err := write(soon, old); err != nil {
		t.Fatalf("23h ahead and 31 days old = %v, want both accepted", err)
	}
	if got := strings.Join(texts(st), ","); got != "soon" {
		t.Errorf("stored %q, want only soon: entries older than retention are accepted but not stored", got)
	}

	for _, name := range []string{"projects/" + project + "/logs/a/b", "projects/" + project + "/logs/a%20b", "organizations/1/logs/a"} {
		_, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
			LogName: name, Resource: global, Entries: []*loggingpb.LogEntry{textEntry("x")}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("log name %q = %v, want InvalidArgument", name, err)
		}
	}
}

func list(t *testing.T, s *Server, filter, order, token string) *loggingpb.ListLogEntriesResponse {
	t.Helper()
	resp, err := s.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + project}, Filter: filter, OrderBy: order, PageSize: 2, PageToken: token})
	if err != nil {
		t.Fatalf("ListLogEntries(%q, %q): %v", filter, order, err)
	}
	return resp
}

func TestPageTokenIsScopedToTheListing(t *testing.T) {
	st := NewStore(100)
	s := NewServer(st)
	for i := 0; i < 5; i++ {
		if _, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
			LogName: "projects/" + project + "/logs/app", Resource: global,
			Entries: []*loggingpb.LogEntry{{Severity: 400, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: fmt.Sprint(i)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	tok := list(t, s, "severity >= WARNING", "", "").GetNextPageToken()
	if tok == "" {
		t.Fatal("no next page token")
	}
	for _, c := range []struct{ filter, order string }{
		{"severity >= ERROR", ""},
		{"severity >= WARNING", "timestamp desc"},
	} {
		_, err := s.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + project}, Filter: c.filter, OrderBy: c.order, PageToken: tok})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("token replayed with filter %q order %q = %v, want InvalidArgument", c.filter, c.order, err)
		}
	}
	for _, bad := range []string{"2", "not-a-token"} {
		_, err := s.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + project}, PageToken: bad})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("page_token %q = %v, want InvalidArgument", bad, err)
		}
	}
}

// Writing between pages must neither repeat nor skip an entry: the offset
// token this replaced did both when a newer entry shifted a descending walk.
func TestWriteBetweenPagesNeitherDuplicatesNorSkips(t *testing.T) {
	for _, order := range []string{"timestamp asc", "timestamp desc"} {
		t.Run(order, func(t *testing.T) {
			st := NewStore(100)
			s := NewServer(st)
			clock := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
			s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
			writeOne := func(msg string) {
				if _, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
					LogName: "projects/" + project + "/logs/app", Resource: global,
					Entries: []*loggingpb.LogEntry{textEntry(msg)}}); err != nil {
					t.Fatal(err)
				}
			}
			var want []string
			for i := 0; i < 5; i++ {
				writeOne(fmt.Sprint(i))
				want = append(want, fmt.Sprint(i))
			}
			// Two entries share a timestamp, so the insertId tiebreak matters.
			same := timestamppb.New(clock)
			if _, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
				LogName: "projects/" + project + "/logs/app", Resource: global,
				Entries: []*loggingpb.LogEntry{
					{Timestamp: same, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "5"}},
					{Timestamp: same, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "6"}},
				}}); err != nil {
				t.Fatal(err)
			}
			want = append(want, "5", "6")

			seen := map[string]int{}
			tok := ""
			for page := 0; ; page++ {
				resp := list(t, s, "", order, tok)
				for _, e := range resp.GetEntries() {
					seen[e.GetTextPayload()]++
				}
				if page == 0 {
					writeOne("late")
				}
				if tok = resp.GetNextPageToken(); tok == "" {
					break
				}
			}
			for _, w := range want {
				if seen[w] != 1 {
					t.Errorf("entry %s seen %d times, want once (all: %v)", w, seen[w], seen)
				}
			}
			if seen["late"] > 1 {
				t.Errorf("late entry seen %d times", seen["late"])
			}
		})
	}
}
