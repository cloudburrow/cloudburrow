package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func writeConsoleEntries(t *testing.T, svc *loggingService, es ...*loggingpb.LogEntry) {
	t.Helper()
	if _, err := svc.api.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
		Resource: &monitoredres.MonitoredResource{Type: "global"}, Entries: es}); err != nil {
		t.Fatal(err)
	}
}

func textEntry(log, text string, sev string, ts time.Time) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{LogName: log, Timestamp: timestamppb.New(ts),
		Severity: ltype.LogSeverity(ltype.LogSeverity_value[sev]), Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}
}

// TestConsoleLogBucketsParentScope (#1411): the Log storage screen's parent
// picker offers the billing accounts Logging holds entries for; picked, the
// screen lists that billing account's buckets, its _Default's page offers
// Copy log entries, which copies through CopyLogEntries and is listed with
// its state, and Cancel copy on one still scheduled. Another project in the
// address is refused.
func TestConsoleLogBucketsParentScope(t *testing.T) {
	_, svc := loggingConsole(t)
	gcs, err := storage.NewServer(storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(gcs)
	defer hs.Close()
	host := strings.TrimPrefix(hs.URL, "http://")
	svc.admin.UseStorage(storageObjectWriter{src: storageSourceFetcher{addr: func() string { return host }}, client: http.DefaultClient})
	if resp, err := http.Post(hs.URL+"/storage/v1/b?project=proj-1", "application/json", strings.NewReader(`{"name":"copies"}`)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("bucket insert: %v %v", resp, err)
	}
	const billing = "billingAccounts/0A1B2C-3D4E5F-6A7B8C"
	writeConsoleEntries(t, svc, textEntry(billing+"/logs/charges", "billed", "INFO", time.Now()))

	p := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	plain, err := p.List(context.Background(), "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Scope == nil || plain.Scope.Selected != "projects/proj-1" || !strings.Contains(fmt.Sprint(plain.Scope.Options), billing) {
		t.Fatalf("the picker is %+v; want the project selected and %s offered", plain.Scope, billing)
	}
	ctx := console.WithScopeValue(context.Background(), bucketScopeKey, billing)
	l, err := p.List(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	bucket := billing + "/locations/global/buckets/_Default"
	if !strings.Contains(fmt.Sprint(l.Items), bucket) || l.Scope.Selected != billing {
		t.Fatalf("the billing account's buckets are %v (scope %+v)", l.Items, l.Scope)
	}
	var offered bool
	for _, a := range p.DetailActions(ctx, "proj-1", []string{bucket}) {
		offered = offered || a.ID == copyEntriesAction
	}
	if !offered {
		t.Fatal("the billing account's _Default offers no Copy log entries")
	}
	if err := p.ActAt(ctx, "proj-1", []string{bucket}, copyEntriesAction, map[string]string{"destination": "gs://copies"}); err != nil {
		t.Fatal(err)
	}
	section := func() console.Listing {
		d, err := p.Detail(ctx, "proj-1", []string{bucket})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("detail: %v %s", err, d.Unavailable)
		}
		for _, s := range d.Sections {
			if s.ID == "copies" {
				return s.Listing
			}
		}
		t.Fatal("no Copy operations section")
		return console.Listing{}
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(fmt.Sprint(section().Items), "SUCCEEDED") {
		time.Sleep(100 * time.Millisecond)
	}
	if it := section().Items; len(it) != 1 || it[0].Fields["State"] != "SUCCEEDED" || it[0].Fields["Entries copied"] != "1" {
		t.Fatalf("copy operations %+v", it)
	}
	if err := p.ActAt(ctx, "proj-1", []string{bucket}, copyEntriesAction, map[string]string{"destination": "copies"}); err != nil {
		t.Fatal(err)
	}
	var cancel *console.Action
	for _, a := range p.DetailActions(ctx, "proj-1", []string{bucket}) {
		if a.ID == cancelCopyAction {
			cancel = &a
		}
	}
	if cancel == nil {
		t.Fatal("no Cancel copy while a copy is scheduled")
	}
	if err := p.ActAt(ctx, "proj-1", []string{bucket}, cancelCopyAction, map[string]string{"operation": cancel.Fields[0].Default}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(fmt.Sprint(section().Items), "CANCELLED") {
		time.Sleep(50 * time.Millisecond)
	}
	if got := fmt.Sprint(section().Items); !strings.Contains(got, "CANCELLED") {
		t.Errorf("after Cancel copy, the operations are %s", got)
	}

	// The project's bucket is not the billing account's, and another
	// project is not the screen's.
	if _, err := p.get(ctx, "proj-1", "projects/proj-1/locations/global/buckets/_Default"); err == nil {
		t.Error("the billing account scope reads the project's bucket")
	}
	other := console.WithScopeValue(context.Background(), bucketScopeKey, "projects/proj-2")
	if l, _ := p.List(other, "proj-1"); l.Prompt == "" {
		t.Errorf("another project's buckets are listed: %+v", l.Items)
	}
}

// TestConsoleLogExplorerThroughView (#1410): /api/logs/buckets lists a
// project's buckets with their retention and views; /api/logs?view= reads
// through ListLogEntries, so the view's filter applies, and so does the
// bucket's retention; /api/stream?view= sends that backlog, then what
// TailLogEntries sends through the view as it is written.
func TestConsoleLogExplorerThroughView(t *testing.T) {
	st := logging.NewStore(100)
	svc := &loggingService{store: st, api: logging.NewServer(st), admin: logging.NewAdmin(store.NewMemory())}
	svc.api.UseAdmin(svc.admin)
	srv := console.New("127.0.0.1:0", nil)
	srv.SetLogViewReader(svc.logViewReader(nil))
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	ctx := context.Background()
	bucket := "projects/proj-1/locations/global/buckets/_Default"
	if _, err := svc.admin.ConfigServer().CreateView(ctx, &loggingpb.CreateViewRequest{Parent: bucket, ViewId: "errors",
		View: &loggingpb.LogView{Filter: "severity>=ERROR"}}); err != nil {
		t.Fatal(err)
	}
	writeConsoleEntries(t, svc,
		textEntry("projects/proj-1/logs/app", "fine", "INFO", time.Now()),
		textEntry("projects/proj-1/logs/app", "broke", "ERROR", time.Now()),
		textEntry("projects/proj-1/logs/app", "broke long ago", "ERROR", time.Now().Add(-40*24*time.Hour)))

	get := func(path string) (int, string) {
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("/api/logs/buckets?parent=projects/proj-1")
	if code != 200 || !strings.Contains(body, `"retentionDays":30`) || !strings.Contains(body, bucket+"/views/errors") ||
		!strings.Contains(body, "severity\\u003e=ERROR") {
		t.Fatalf("buckets = %d %s", code, body)
	}
	view := bucket + "/views/errors"
	code, body = get("/api/logs?view=" + url.QueryEscape(view))
	var got struct{ Entries []console.Entry }
	if err := json.Unmarshal([]byte(body), &got); code != 200 || err != nil {
		t.Fatalf("logs = %d %s", code, body)
	}
	if len(got.Entries) != 1 || got.Entries[0].Message != "broke" {
		t.Fatalf("through the view, read %+v; want the one recent error (the view's filter and 30 days' retention)", got.Entries)
	}
	if code, body := get("/api/logs?view=" + url.QueryEscape(bucket+"/views/nope")); code != http.StatusBadRequest || !strings.Contains(body, "nope") {
		t.Errorf("a missing view = %d %s", code, body)
	}

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, hs.URL+"/api/stream?view="+url.QueryEscape(view), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(want string) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q", want)
				}
				if strings.HasPrefix(l, "data:") && strings.Contains(l, want) {
					return
				}
				if strings.Contains(l, "fine") {
					t.Fatalf("the stream sent an entry the view's filter hides: %s", l)
				}
			case <-timeout:
				t.Fatalf("no %q on the stream", want)
			}
		}
	}
	next(`"broke"`)
	// The tail opens after the backlog; give it a moment, then write.
	time.Sleep(200 * time.Millisecond)
	writeConsoleEntries(t, svc,
		textEntry("projects/proj-1/logs/app", "fine again", "INFO", time.Now()),
		textEntry("projects/proj-1/logs/app", "tailed error", "ERROR", time.Now()))
	next("tailed error")
}
