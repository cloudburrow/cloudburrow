package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// TestConsoleLoggingCopy (#1394): Copy log entries on _Default's page runs
// CopyLogEntries, which writes through storageObjectWriter to a Cloud
// Storage server; the page lists the copy with its state and count, and
// Cancel copy cancels one still scheduled.
func TestConsoleLoggingCopy(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
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
	if _, err := svc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{LogName: "projects/proj-1/logs/app", Timestamp: timestamppb.Now(),
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	p := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	bucket := "projects/proj-1/locations/global/buckets/_Default"
	ids := func() []string {
		var out []string
		for _, a := range p.DetailActions(ctx, "proj-1", []string{bucket}) {
			out = append(out, a.ID)
		}
		return out
	}
	if got := fmt.Sprint(ids()); !strings.Contains(got, copyEntriesAction) || strings.Contains(got, cancelCopyAction) {
		t.Fatalf("actions %s; want Copy log entries and no Cancel copy", got)
	}
	if err := p.ActAt(ctx, "proj-1", []string{bucket}, copyEntriesAction, map[string]string{"destination": "copies", "filter": `logName="projects/proj-1/logs/app"`}); err != nil {
		t.Fatal(err)
	}
	section := func() console.Listing {
		d, err := p.Detail(ctx, "proj-1", []string{bucket})
		if err != nil {
			t.Fatal(err)
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
	for time.Now().Before(deadline) {
		if l := section(); len(l.Items) == 1 && l.Items[0].Fields["State"] == "SUCCEEDED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	l := section()
	if len(l.Items) != 1 || l.Items[0].Fields["State"] != "SUCCEEDED" || l.Items[0].Fields["Entries copied"] != "1" ||
		l.Items[0].Fields["Destination"] != "storage.googleapis.com/copies" {
		t.Fatalf("copy operations %+v", l.Items)
	}
	resp, err := http.Get(hs.URL + "/storage/v1/b/copies/o")
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !regexp.MustCompile(`"name": ?"app/\d{4}/\d{2}/\d{2}/\d{2}:00:00_\d{2}:59:59_S0.json"`).Match(listed) {
		t.Errorf("the destination lists %s", listed)
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
		t.Fatalf("no Cancel copy while a copy is scheduled: %v", ids())
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
}
