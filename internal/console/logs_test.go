package console

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func recorder(t *testing.T) *Recorder {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	return NewRecorder(50, func() time.Time { now = now.Add(time.Second); return now })
}

// Redaction happens on the way in. An entry stored with a token in it has
// already been written somewhere a later change might expose.
func TestCredentialsAreRedactedBeforeStorage(t *testing.T) {
	t.Parallel()
	r := recorder(t)

	for _, tc := range []struct{ in, mustNotContain string }{
		{"refreshed ya29.a0AdMD6xYZsecretvalue for the caller", "ya29.a0AdMD6xYZsecretvalue"},
		{"minted cbl_WVJjeoMQabcdef", "cbl_WVJjeoMQabcdef"},
		{"Authorization: Bearer abc123def", "abc123def"},
		{"api_key=supersecret123", "supersecret123"},
		{"password: hunter2hunter2", "hunter2hunter2"},
		{"-----BEGIN PRIVATE KEY-----", "BEGIN PRIVATE KEY"},
		{"token eyJhbGciOiJSUzI1NiIs.eyJzdWIiOiIxMjM0NTY.signature", "eyJhbGciOiJSUzI1NiIs"},
	} {
		got := r.Log(Entry{Message: tc.in})
		if strings.Contains(got.Message, tc.mustNotContain) {
			t.Errorf("a credential survived redaction: %q -> %q", tc.in, got.Message)
		}
		if !strings.Contains(got.Message, "REDACTED") {
			t.Errorf("redaction left no marker: %q -> %q", tc.in, got.Message)
		}
	}

	// An ordinary line must survive intact, or the view becomes useless.
	plain := r.Log(Entry{Message: "worker processed 12 messages"})
	if plain.Message != "worker processed 12 messages" {
		t.Errorf("an ordinary line was altered: %q", plain.Message)
	}
}

// An application that logs a whole request body would fill the buffer with
// one payload and push out everything that explains it.
func TestLongMessagesAreTruncatedAndMarked(t *testing.T) {
	t.Parallel()
	r := recorder(t)

	got := r.Log(Entry{Message: strings.Repeat("x", MaxMessageBytes*2)})
	if len(got.Message) > MaxMessageBytes+32 {
		t.Errorf("message is %d bytes, want it truncated", len(got.Message))
	}
	if !strings.Contains(got.Message, "truncated") {
		t.Error("a truncated line is not marked, so it reads as the whole line")
	}
}

// Unbounded growth in a development tool eventually becomes the reason the
// machine runs out of memory.
func TestRecorderIsBounded(t *testing.T) {
	t.Parallel()
	r := NewRecorder(10, nil)
	for i := 0; i < 100; i++ {
		r.Log(Entry{Message: "line"})
	}
	if got := len(r.Entries(Filter{})); got != 10 {
		t.Errorf("kept %d entries, want the 10 limit", got)
	}
	// The newest must be kept, not the oldest.
	entries := r.Entries(Filter{})
	if entries[len(entries)-1].ID != 100 {
		t.Errorf("the newest entry is %d, want 100", entries[len(entries)-1].ID)
	}
}

func TestFiltersNarrowTheView(t *testing.T) {
	t.Parallel()
	r := recorder(t)
	r.Log(Entry{Severity: SeverityInfo, Source: "run/a", Project: "p1", Message: "started"})
	r.Log(Entry{Severity: SeverityError, Source: "run/a", Project: "p1", Message: "crashed"})
	r.Log(Entry{Severity: SeverityInfo, Source: "tasks", Project: "p2", Message: "dispatched"})

	if got := len(r.Entries(Filter{MinSeverity: SeverityError})); got != 1 {
		t.Errorf("severity filter returned %d, want 1", got)
	}
	if got := len(r.Entries(Filter{Source: "run/"})); got != 2 {
		t.Errorf("source filter returned %d, want 2", got)
	}
	if got := len(r.Entries(Filter{Contains: "CRASH"})); got != 1 {
		t.Errorf("text filter is case sensitive: got %d", got)
	}

	// One project's activity must never appear under another.
	p1 := r.Entries(Filter{Project: "p1"})
	if len(p1) != 2 {
		t.Fatalf("project filter returned %d, want 2", len(p1))
	}
	for _, e := range p1 {
		if e.Project != "p1" {
			t.Errorf("project p1 returned an entry from %q", e.Project)
		}
	}
}

// An entry with no project must not be attributed to one, because
// attributing it would be a guess.
func TestUnattributedEntriesAreNotShownUnderAProject(t *testing.T) {
	t.Parallel()
	r := recorder(t)
	r.Log(Entry{Message: "cluster ready"})

	if got := len(r.Entries(Filter{Project: "p1"})); got != 0 {
		t.Errorf("an unattributed entry appeared under a project: %d", got)
	}
	if got := len(r.Entries(Filter{})); got != 1 {
		t.Errorf("an unattributed entry is invisible everywhere: %d", got)
	}
}

// A reconnecting client resumes rather than replaying everything, which
// would make a reconnect look like a flood of new activity.
func TestSinceResumesRatherThanReplaying(t *testing.T) {
	t.Parallel()
	r := recorder(t)
	for i := 0; i < 5; i++ {
		r.Log(Entry{Message: "line"})
	}
	got := r.Entries(Filter{Since: 3})
	if len(got) != 2 {
		t.Fatalf("resume returned %d entries, want 2", len(got))
	}
	if got[0].ID != 4 {
		t.Errorf("resume started at %d, want 4", got[0].ID)
	}
}

// The verdict comes from the backend. An operation is not successful because
// a call returned.
func TestOperationsRecordTheBackendsVerdict(t *testing.T) {
	t.Parallel()
	r := recorder(t)

	id := r.StartOperation("create", "bucket-a", "p1")
	ops := r.Operations("p1")
	if len(ops) != 1 || ops[0].State != OperationPending {
		t.Fatalf("a started operation is %+v, want PENDING", ops)
	}

	r.FinishOperation(id, OperationFailed, "AlreadyExists: bucket exists")
	ops = r.Operations("p1")
	if ops[0].State != OperationFailed {
		t.Errorf("state = %s, want FAILED", ops[0].State)
	}
	if !strings.Contains(ops[0].Error, "AlreadyExists") {
		t.Errorf("the cause was lost: %q", ops[0].Error)
	}
	if ops[0].Ended.IsZero() {
		t.Error("a finished operation has no end time")
	}
	// Another project must not see it.
	if got := len(r.Operations("p2")); got != 0 {
		t.Errorf("project p2 sees %d of p1's operations", got)
	}
}

// A failed operation's cause must be redacted too: it is a log line by
// another name.
func TestOperationErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	r := recorder(t)
	id := r.StartOperation("create", "x", "p1")
	r.FinishOperation(id, OperationFailed, "rejected token ya29.leakedsecretvalue")

	if strings.Contains(r.Operations("p1")[0].Error, "ya29.leakedsecretvalue") {
		t.Error("a credential survived in an operation error")
	}
}

// --- streaming ---------------------------------------------------------

func TestStreamSendsBacklogThenLiveEntries(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	s.logs.Log(Entry{Message: "before the client connected"})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)
	backlog := readEvent(t, reader)
	if !strings.Contains(backlog, "before the client connected") {
		t.Errorf("the backlog was not sent: %q", backlog)
	}
	// Every event carries an id, which is what makes Last-Event-ID resume
	// work at all.
	if !strings.Contains(backlog, "id: ") {
		t.Errorf("the event carries no id: %q", backlog)
	}

	s.logs.Log(Entry{Message: "after the client connected"})
	live := readEvent(t, reader)
	if !strings.Contains(live, "after the client connected") {
		t.Errorf("a live entry did not arrive: %q", live)
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	for i := 0; i < 3; i++ {
		s.logs.Log(Entry{Message: "old"})
	}
	s.logs.Log(Entry{Message: "wanted"})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	event := readEvent(t, bufio.NewReader(resp.Body))
	if strings.Contains(event, `"message":"old"`) {
		t.Errorf("a resumed stream replayed entries the client already had: %q", event)
	}
	if !strings.Contains(event, "wanted") {
		t.Errorf("the gap was not replayed: %q", event)
	}
}

func TestStreamHonoursFilters(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	s.logs.Log(Entry{Severity: SeverityInfo, Message: "routine"})
	s.logs.Log(Entry{Severity: SeverityError, Message: "the failure"})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stream?severity=ERROR")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	event := readEvent(t, bufio.NewReader(resp.Body))
	if strings.Contains(event, "routine") {
		t.Errorf("a filtered-out entry was streamed: %q", event)
	}
	if !strings.Contains(event, "the failure") {
		t.Errorf("the matching entry was not streamed: %q", event)
	}
}

// A stalled browser tab must not stop the process from logging.
func TestASlowWatcherDoesNotBlockLogging(t *testing.T) {
	t.Parallel()
	r := NewRecorder(1000, nil)
	_, stop := r.Watch(1)
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			r.Log(Entry{Message: "line"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on a watcher that is not reading")
	}
}

// readEvent reads one SSE event block, skipping keepalive comments.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var block strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no event arrived; read so far: %q", block.String())
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v (so far %q)", err, block.String())
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if line == "\n" {
			if block.Len() > 0 {
				return block.String()
			}
			continue
		}
		block.WriteString(line)
	}
}

func TestSeverityOrdering(t *testing.T) {
	t.Parallel()
	if !SeverityError.AtLeast(SeverityWarning) {
		t.Error("ERROR is not at least WARNING")
	}
	if SeverityInfo.AtLeast(SeverityError) {
		t.Error("INFO was treated as at least ERROR")
	}
}

// A nil recorder must be safe: components log unconditionally.
func TestNilRecorderIsSafe(t *testing.T) {
	t.Parallel()
	var r *Recorder
	r.Log(Entry{Message: "x"})
	r.FinishOperation("op-1", OperationFailed, "x")
	if r.Entries(Filter{}) != nil || r.Operations("") != nil {
		t.Error("a nil recorder returned data")
	}
	if r.StartOperation("k", "r", "p") != "" {
		t.Error("a nil recorder started an operation")
	}
}

// TestHistogramBucketsBySeverityOverTheSpanItIsGiven.
//
// A list of the most recent lines cannot answer "when did the errors start". The
// buckets have to cover the span the entries actually occupy: a fixed interval
// would give a screen showing ten seconds of logs forty empty columns.
func TestHistogramBucketsBySeverityOverTheSpanItIsGiven(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var entries []Entry
	// Forty seconds of INFO, then four errors in the last second.
	for i := 0; i < 40; i++ {
		entries = append(entries, Entry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Severity:  SeverityInfo,
		})
	}
	for i := 0; i < 4; i++ {
		entries = append(entries, Entry{
			Timestamp: base.Add(39*time.Second + time.Duration(i)*100*time.Millisecond),
			Severity:  SeverityError,
		})
	}

	buckets := histogram(entries)
	if len(buckets) != HistogramBuckets {
		t.Fatalf("buckets = %d, want %d", len(buckets), HistogramBuckets)
	}
	total := 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != len(entries) {
		t.Fatalf("buckets hold %d entries, want all %d — an entry fell outside every bucket",
			total, len(entries))
	}
	// The errors are at the end, so they belong in the last bucket rather than
	// past it: the newest entry lands exactly on the upper bound.
	if got := buckets[len(buckets)-1].Counts[SeverityError]; got != 4 {
		t.Fatalf("last bucket holds %d errors, want 4", got)
	}
	// The first bucket holds info and no errors, which is the whole point of
	// keeping the counts by severity rather than summing them.
	if buckets[0].Counts[SeverityError] != 0 {
		t.Error("an error was counted in the first bucket")
	}

	// Every entry at the same instant is one bucket, not a division by zero.
	same := histogram([]Entry{
		{Timestamp: base, Severity: SeverityInfo},
		{Timestamp: base, Severity: SeverityError},
	})
	if len(same) != 1 || same[0].Total != 2 {
		t.Fatalf("instantaneous entries = %+v", same)
	}

	// No entries is an empty slice rather than nil, so a client that iterates
	// before checking does not fall over.
	if got := histogram(nil); got == nil || len(got) != 0 {
		t.Fatalf("histogram(nil) = %v, want an empty slice", got)
	}
}
