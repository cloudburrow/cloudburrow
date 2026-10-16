//go:build compat

package compat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	vlogging "cloud.google.com/go/logging/apiv2"
)

// viewTexts are the text payloads the official Client reads through
// resource names, sorted.
func viewTexts(t *testing.T, ctx context.Context, lc *vlogging.Client, names ...string) []string {
	t.Helper()
	var out []string
	it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: names})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLogEntries(%v): %v", names, err)
		}
		out = append(out, e.GetTextPayload())
	}
	sort.Strings(out)
	return out
}

// TestConsoleLogsExplorerThroughView (#1410).
//
// The Logs Explorer's "Through a log view" scope, driven through the
// console's API as the page drives it: /api/logs/buckets lists the
// project's buckets with their retention and views, as the official
// ConfigClient does; /api/logs?view= reads what the official Client's
// ListLogEntries reads through the view (its filter and the bucket's one-day
// retention applied); /api/stream?view= streams that backlog, then, through
// TailLogEntries, what the official Client writes that the view matches
// and nothing else. The bucket's page shows its retention and links each
// view to the Explorer reading through it.
//
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries, google.logging.v2.LoggingServiceV2/TailLogEntries, google.logging.v2.ConfigServiceV2/ListViews
func TestConsoleLogsExplorerThroughView(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := consoleAddr(t, h)
	logID := fmt.Sprintf("cview-%d", time.Now().UnixNano()%1e9)
	bucket, view := loggingViewFixture(t, h, logID)
	lc := loggingAPIClient(t, h)
	project := h.Project()

	var buckets struct {
		Buckets []struct {
			Name          string `json:"name"`
			RetentionDays int32  `json:"retentionDays"`
			Views         []struct{ Name, Filter string }
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/logs/buckets?"+url.Values{"parent": {"projects/" + project}}.Encode(), "", &buckets)
	found := false
	for _, b := range buckets.Buckets {
		if b.Name != bucket {
			continue
		}
		found = true
		if b.RetentionDays != 1 || !strings.Contains(fmt.Sprint(b.Views), view) {
			t.Errorf("the console lists %+v; want one day's retention and view %s", b, view)
		}
	}
	if !found {
		t.Fatalf("the console's buckets %+v do not include %s", buckets.Buckets, bucket)
	}

	var logs struct {
		Entries []struct{ Message string }
	}
	consoleJSON(t, addr, http.MethodGet, "/api/logs?"+url.Values{"view": {view}}.Encode(), "", &logs)
	var got []string
	for _, e := range logs.Entries {
		got = append(got, e.Message)
	}
	sort.Strings(got)
	want := viewTexts(t, ctx, lc, view)
	if fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(got) != "[gce-now]" {
		t.Errorf("through the view the console reads %v, the official Client %v; want [gce-now]", got, want)
	}

	var d struct {
		Summary []struct{ Label, Value string }
		consoleDetail
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/logging-buckets?"+url.Values{"project": {project}, "name": {bucket}}.Encode(), "", &d)
	if !strings.Contains(fmt.Sprint(d.Summary), "{Retention 1 days}") {
		t.Errorf("the bucket's page summary is %+v; want Retention 1 days", d.Summary)
	}

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, "http://"+addr+"/api/stream?"+url.Values{"view": {view}}.Encode(), nil)
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
		timeout := time.After(20 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q", want)
				}
				if !strings.HasPrefix(l, "data:") {
					continue
				}
				var e struct{ Message string }
				_ = json.Unmarshal([]byte(strings.TrimPrefix(l, "data:")), &e)
				if e.Message == want {
					return
				}
				if strings.HasPrefix(e.Message, "global-") {
					t.Fatalf("the stream sent %q, which the view's filter hides", e.Message)
				}
			case <-timeout:
				t.Fatalf("no %q on the stream", want)
			}
		}
	}
	next("gce-now")
	time.Sleep(500 * time.Millisecond)
	for _, e := range []struct{ typ, text string }{{"global", "global-tail"}, {"gce_instance", "gce-tail"}} {
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/" + project + "/logs/" + logID,
			Resource: &mrpb.MonitoredResource{Type: e.typ, Labels: map[string]string{}},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: e.text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	next("gce-tail")
}

// TestConsoleLoggingCopyFromParents (#1411).
//
// Copy log entries on a folder's, the organization's and a billing
// account's _Default bucket page, chosen with ?parent= as the Log storage
// screen's picker sends it: the picker offers the folder Resource Manager
// holds, the organization and the billing account the official Client
// wrote to; each copy is an operation the official ConfigClient lists under
// the parent, done with the one entry copied, and the page's Copy
// operations section shows it SUCCEEDED. Cancel copy on a copy still
// scheduled cancels the operation the ConfigClient reads back.
//
// covers: google.logging.v2.ConfigServiceV2/CopyLogEntries
func TestConsoleLoggingCopyFromParents(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	lc := loggingAPIClient(t, h)
	cc, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano())
	dest := sc.Bucket(project + "-clogcopy")
	if err := dest.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for n := range copyBucketObjects(t, context.Background(), dest) {
			_ = dest.Object(n).Delete(context.Background())
		}
		_ = dest.Delete(context.Background())
	})
	folder, _ := loggingParentsFixture(t, h, "ccp-")
	billing := fmt.Sprintf("billingAccounts/0C%04X-%06X-%06X", time.Now().UnixNano()%0xFFFF, time.Now().UnixNano()%0xFFFFFF, (time.Now().UnixNano()/7)%0xFFFFFF)

	for _, parent := range []string{folder, localOrg, billing} {
		log := parent + "/logs/ccopy-" + strings.SplitN(parent, "/", 2)[0] + "-" + suffix
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: log, Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "console copy from " + parent}}}}); err != nil {
			t.Fatalf("WriteLogEntries to %s: %v", log, err)
		}
		var list struct {
			consoleListing
			Scope struct {
				Selected string
				Options  []struct{ ID string }
			}
		}
		q := url.Values{"project": {project}, "parent": {parent}}
		consoleJSON(t, addr, http.MethodGet, "/api/resources/logging-buckets?"+q.Encode(), "", &list)
		bucket := parent + "/locations/global/buckets/_Default"
		if list.Scope.Selected != parent || !strings.Contains(fmt.Sprint(list.Scope.Options), parent) {
			t.Errorf("the picker is %+v; want %s offered and selected", list.Scope, parent)
		}
		if !strings.Contains(fmt.Sprint(list.Items), bucket) {
			t.Fatalf("the Log storage screen for %s lists %+v", parent, list.Items)
		}

		act := func(action string, values map[string]string) {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Action": action, "Values": values})
			if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/logging-buckets?"+q.Encode(), string(body)); code != http.StatusOK {
				t.Fatalf("%s on %s = %d: %s", action, bucket, code, out)
			}
		}
		act("copy-entries", map[string]string{"destination": dest.BucketName(), "filter": fmt.Sprintf(`logName="%s"`, log)})
		ops := func() []*longrunningpb.Operation {
			var out []*longrunningpb.Operation
			it := cc.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent + "/locations/global"})
			for {
				op, err := it.Next()
				if err == iterator.Done {
					return out
				}
				if err != nil {
					t.Fatalf("ListOperations(%s): %v", parent, err)
				}
				md := &loggingpb.CopyLogEntriesMetadata{}
				if op.GetMetadata().UnmarshalTo(md) == nil && md.GetRequest().GetName() == bucket {
					out = append(out, op)
				}
			}
		}
		var done *longrunningpb.Operation
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && done == nil; time.Sleep(200 * time.Millisecond) {
			for _, op := range ops() {
				md := &loggingpb.CopyLogEntriesMetadata{}
				_ = op.GetMetadata().UnmarshalTo(md)
				if op.GetDone() && strings.Contains(md.GetRequest().GetFilter(), log) {
					done = op
				}
			}
		}
		if done == nil {
			t.Fatalf("the ConfigClient lists no finished copy from %s", bucket)
		}
		r := &loggingpb.CopyLogEntriesResponse{}
		if err := done.GetResponse().UnmarshalTo(r); err != nil || r.GetLogEntriesCopiedCount() != 1 {
			t.Errorf("the copy from %s = %v, %v; want 1 entry copied", bucket, r, err)
		}
		var d consoleDetail
		consoleJSON(t, addr, http.MethodGet, "/api/detail/logging-buckets?"+url.Values{"project": {project}, "parent": {parent}, "name": {bucket}}.Encode(), "", &d)
		if s := fmt.Sprint(d.section(t, "copies").Items); !strings.Contains(s, "SUCCEEDED") {
			t.Errorf("the page's copy operations for %s are %s", bucket, s)
		}
	}

	// Cancel copy, on the folder's bucket, while the copy is scheduled.
	q := url.Values{"project": {project}, "parent": {folder}}
	bucket := folder + "/locations/global/buckets/_Default"
	body, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Action": "copy-entries", "Values": map[string]string{"destination": dest.BucketName()}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/logging-buckets?"+q.Encode(), string(body)); code != http.StatusOK {
		t.Fatalf("copy-entries = %d: %s", code, out)
	}
	var running string
	it := cc.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: folder + "/locations/global"})
	for {
		op, err := it.Next()
		if err != nil {
			break
		}
		if !op.GetDone() {
			running = op.GetName()
		}
	}
	if running == "" {
		t.Fatal("the ConfigClient lists no scheduled copy")
	}
	body, _ = json.Marshal(map[string]any{"Path": []string{bucket}, "Action": "cancel-copy",
		"Values": map[string]string{"operation": running[strings.LastIndex(running, "/")+1:]}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/logging-buckets?"+q.Encode(), string(body)); code != http.StatusOK {
		t.Fatalf("cancel-copy = %d: %s", code, out)
	}
	var state loggingpb.OperationState
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		op, err := cc.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: running})
		if err != nil {
			t.Fatal(err)
		}
		md := &loggingpb.CopyLogEntriesMetadata{}
		_ = op.GetMetadata().UnmarshalTo(md)
		if state = md.GetState(); state == loggingpb.OperationState_OPERATION_STATE_CANCELLED {
			break
		}
	}
	if state != loggingpb.OperationState_OPERATION_STATE_CANCELLED {
		t.Errorf("after Cancel copy, the ConfigClient reads %s", state)
	}
}
