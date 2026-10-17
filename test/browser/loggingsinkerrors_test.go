//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const envBigQuery = "CLOUDBURROW_TEST_BIGQUERY"

// TestLoggingAdminSinkExportErrorsThroughTheBrowser (#1451), in the run
// shard, whose instance serves Cloud Logging and BigQuery: Create sink on the
// Log router, to a BigQuery dataset, makes the sink the official
// ConfigClient's GetSink reads. Of two entries written with the official
// Logging client whose field n is a number and then a string, the second
// cannot go in its table; the sink's row then says one entry is in the
// dataset's export_errors table, and the official BigQuery client reads that
// entry from export_errors_YYYYMMDD, naming the sink.
func TestLoggingAdminSinkExportErrorsThroughTheBrowser(t *testing.T) {
	needService(t, "logging-sinks")
	needService(t, "bigquery")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	bqAddr := strings.TrimSpace(os.Getenv(envBigQuery))
	if bqAddr == "" {
		t.Skipf("%s is not set: the export_errors table is read through the BigQuery API", envBigQuery)
	}
	if !strings.HasPrefix(bqAddr, "http") {
		bqAddr = "http://" + bqAddr
	}
	ctx := context.Background()
	// The BigQuery emulator serves one project: the instance's default.
	project := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_BIGQUERY_PROJECT"))
	if project == "" {
		project = servedProject(t)
	}
	lopts := []option.ClientOption{option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	lc, err := vlogging.NewClient(ctx, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	cc, err := vlogging.NewConfigClient(ctx, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	bq, err := bigquery.NewClient(ctx, project, option.WithEndpoint(bqAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Close() })

	suffix := fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)
	ds, logID, sinkID := "browser_xerr_"+suffix, "browser-xerr-"+suffix, "browser-xerr-"+suffix
	parent := "projects/" + project
	sink := parent + "/sinks/" + sinkID
	if err := bq.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("create the dataset with the BigQuery client: %v", err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(context.Background()) })
	t.Cleanup(func() { _ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: sink}) })

	p := open(t)
	p.navigate("/logs/sinks?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent.startsWith("Create"))`)
	p.eval(`[...document.querySelectorAll("#view button.primary")].find((b) => b.textContent.startsWith("Create")).click()`, nil)
	p.waitFor(`document.querySelector("#view #f-destination, .modal.is-open #f-destination") !== null`)
	scope := "#view"
	var inModal bool
	p.eval(`document.querySelector(".modal.is-open #f-destination") !== null`, &inModal)
	if inModal {
		scope = ".modal.is-open"
	}
	filter := fmt.Sprintf(`logName="%s/logs/%s"`, parent, logID)
	setField(p, scope, "f-name", sinkID)
	setField(p, scope, "f-destination", "bigquery.googleapis.com/projects/"+project+"/datasets/"+ds)
	setField(p, scope, "f-filter", filter)
	p.run(chromedp.Click(scope+` button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/sinks" && [...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, sinkID))

	got, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: sink})
	if err != nil || got.GetDestination() != "bigquery.googleapis.com/projects/"+project+"/datasets/"+ds || got.GetFilter() != filter {
		t.Fatalf("GetSink after Create sink = %v, %v", got, err)
	}

	now := time.Now().UTC()
	num, _ := structpb.NewStruct(map[string]any{"n": 1})
	str, _ := structpb.NewStruct(map[string]any{"n": "one" + suffix})
	res := &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}}
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: []*loggingpb.LogEntry{
		{LogName: parent + "/logs/" + logID, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: num}, Timestamp: timestamppb.New(now), InsertId: "a" + suffix},
		{LogName: parent + "/logs/" + logID, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: str}, Timestamp: timestamppb.New(now), InsertId: "b" + suffix},
	}}); err != nil {
		t.Fatalf("WriteLogEntries: %v", err)
	}

	// The sink's row reports the entry that went to export_errors; Refresh
	// until it does.
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, sinkID)
	deadline := time.Now().Add(45 * time.Second)
	for {
		var said bool
		p.eval(fmt.Sprintf(`(%s || { textContent: "" }).textContent.includes("1 in the dataset's export_errors table")`, row), &said)
		if said {
			break
		}
		if time.Now().After(deadline) {
			var text string
			p.eval(fmt.Sprintf(`(%s || { textContent: "" }).textContent`, row), &text)
			t.Fatalf("the sink's row never said an entry is in export_errors: %q", text)
		}
		p.clickText("#view button", "Refresh")
		time.Sleep(time.Second)
	}

	table := "export_errors_" + now.Format("20060102")
	var rows []map[string]bigquery.Value
	for deadline := time.Now().Add(30 * time.Second); len(rows) != 1 && time.Now().Before(deadline); time.Sleep(time.Second) {
		it, err := bq.Query(fmt.Sprintf("SELECT sink, insertId, logName, logEntry FROM `%s.%s.%s`", project, ds, table)).Read(ctx)
		if err != nil {
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
	}
	if len(rows) != 1 {
		t.Fatalf("%s.%s has %d rows, want the one mismatched entry", ds, table, len(rows))
	}
	r := rows[0]
	if entry, _ := r["logEntry"].(string); r["sink"] != sink || r["insertId"] != "b"+suffix || r["logName"] != parent+"/logs/"+logID || !strings.Contains(entry, "one"+suffix) {
		t.Errorf("the export_errors row = %v; want the second entry, from %s", r, sink)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/logging-sinks?project="+project, ""); code != http.StatusOK || !strings.Contains(body, "export_errors table") {
		t.Errorf("the console API's sink listing = %d: %s", code, body)
	}
}
