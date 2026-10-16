package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// POST /v2/entries:tail (#1410) streams a JSON array of
// TailLogEntriesResponse, opened at once and one element per send, of a
// view's matching entries.
func TestTailOverJSON(t *testing.T) {
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	if _, err := a.ConfigServer().CreateView(context.Background(), &loggingpb.CreateViewRequest{
		Parent: "projects/proj-1/locations/global/buckets/_Default", ViewId: "app", View: &loggingpb.LogView{Filter: `LOG_ID("app")`}}); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { srv.Register(g); a.Register(g) }))
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/v2/entries:tail",
		strings.NewReader(`{"resourceNames":["projects/proj-1/locations/global/buckets/_Default/views/app"],"bufferWindow":"0s"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, err := r.ReadString('\n'); resp.StatusCode != 200 || err != nil || line != "[\n" {
		t.Fatalf("the tail opened %d %q, %v; want 200 and [", resp.StatusCode, line, err)
	}
	for _, log := range []string{"other", "app"} {
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{LogName: "projects/proj-1/logs/" + log,
			Resource: &monitoredres.MonitoredResource{Type: "global"}, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log}}}}); err != nil {
			t.Fatal(err)
		}
	}
	var got struct {
		Entries []struct{ TextPayload string }
	}
	dec := json.NewDecoder(r)
	if err := dec.Decode(&got); err != nil || len(got.Entries) != 1 || got.Entries[0].TextPayload != "app" {
		t.Errorf("the first element = %+v, %v; want the app entry only", got, err)
	}
}
