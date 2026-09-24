//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// EnvBigQuery is the REST endpoint, as `cloudburrow env` exports it in
	// CLOUDBURROW_BIGQUERY_ENDPOINT.
	EnvBigQuery = "CLOUDBURROW_TEST_BIGQUERY"
	// EnvBigQueryStorage is the gRPC Storage Read API endpoint.
	EnvBigQueryStorage = "CLOUDBURROW_TEST_BIGQUERY_STORAGE"
	// EnvBigQueryProject is the instance's project: the emulator serves that
	// one project and no other, so a test's own unique project cannot be used.
	EnvBigQueryProject = "CLOUDBURROW_TEST_BIGQUERY_PROJECT"
)

// bigqueryClient returns the official client pointed at the emulator.
//
// The Go client reads no emulator variable — there is no BIGQUERY_EMULATOR_HOST
// in any official library — so the endpoint must be passed in client options.
// That is the ergonomic limit, demonstrated here rather than described.
func bigqueryClient(t *testing.T, h *Harness) (*bigquery.Client, string) {
	t.Helper()
	endpoint := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set: the emulator serves one project, and the test must use it", EnvBigQueryProject)
	}
	c, err := bigquery.NewClient(h.Context(), project,
		option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, project
}

type order struct {
	ID     int64   `bigquery:"id"`
	Region string  `bigquery:"region"`
	Amount float64 `bigquery:"amount"`
}

// seedOrders creates a dataset and table named after the test's own project
// ID, so runs cannot collide inside the one project the emulator serves, and
// streams four rows in.
func seedOrders(t *testing.T, h *Harness, c *bigquery.Client) (*bigquery.Dataset, *bigquery.Table) {
	t.Helper()
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{Description: "compat"}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })

	schema, err := bigquery.InferSchema(order{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := ds.Table("orders")
	if err := tbl.Create(h.Context(), &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	rows := []*order{{1, "eu", 10}, {2, "eu", 5.5}, {3, "us", 7}, {4, "us", 1}}
	if err := tbl.Inserter().Put(h.Context(), rows); err != nil {
		t.Fatalf("streaming insert: %v", err)
	}
	return ds, tbl
}

// TestBigQueryDatasetTableInsertAndQuery.
//
// BigQuery was absent (#277). Through the official Go client: a dataset and
// table are created and read back, rows are streamed in, a SELECT with WHERE
// and GROUP BY returns the right answer, and both are deleted.
func TestBigQueryDatasetTableInsertAndQuery(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, tbl := seedOrders(t, h, c)

	md, err := tbl.Metadata(h.Context())
	if err != nil {
		t.Fatalf("table metadata: %v", err)
	}
	var cols []string
	for _, f := range md.Schema {
		cols = append(cols, f.Name+":"+string(f.Type))
	}
	if strings.Join(cols, ",") != "id:INTEGER,region:STRING,amount:FLOAT" {
		t.Errorf("schema read back as %v", cols)
	}

	q := c.Query("SELECT region, SUM(amount) AS total, COUNT(*) AS n FROM `" + project + "." + ds.DatasetID +
		".orders` WHERE amount > 2 GROUP BY region ORDER BY region")
	it, err := q.Read(h.Context())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	type group struct {
		Region string  `bigquery:"region"`
		Total  float64 `bigquery:"total"`
		N      int64   `bigquery:"n"`
	}
	var got []group
	for {
		var g group
		err := it.Next(&g)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("read results: %v", err)
		}
		got = append(got, g)
	}
	want := []group{{"eu", 15.5, 2}, {"us", 7, 1}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("GROUP BY returned %+v, want %+v", got, want)
	}

	// Deletion, seen through the client: the table and then the dataset are
	// gone, not merely unlisted.
	if err := tbl.Delete(h.Context()); err != nil {
		t.Fatalf("delete table: %v", err)
	}
	if _, err := tbl.Metadata(h.Context()); !isNotFound(err) {
		t.Errorf("a deleted table still answers: %v", err)
	}
	if err := ds.Delete(h.Context()); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	if _, err := ds.Metadata(h.Context()); !isNotFound(err) {
		t.Errorf("a deleted dataset still answers: %v", err)
	}
}

// TestBigQueryStorageReadAPIStreamsArrowRows reads a table through the gRPC
// Storage Read API, the second port the service is tunnelled on, with the
// official Storage Read client.
//
// Arrow only, and the raw client only, because those are what work (measured,
// #277): the emulator fails an Avro session for any project ID containing a
// hyphen, and the high-level client's accelerated iterator
// (Client.EnableStorageReadClient) panics on its responses. Both are recorded
// in docs/compatibility.md rather than hidden by testing around them.
func TestBigQueryStorageReadAPIStreamsArrowRows(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := seedOrders(t, h, c)

	rc, err := bqstorage.NewBigQueryReadClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewBigQueryReadClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	session, err := rc.CreateReadSession(h.Context(), &storagepb.CreateReadSessionRequest{
		Parent: "projects/" + project,
		ReadSession: &storagepb.ReadSession{
			Table:      "projects/" + project + "/datasets/" + ds.DatasetID + "/tables/orders",
			DataFormat: storagepb.DataFormat_ARROW,
		},
		MaxStreamCount: 1,
	})
	if err != nil {
		t.Fatalf("CreateReadSession: %v", err)
	}
	if len(session.Streams) == 0 {
		t.Fatal("the read session has no streams")
	}
	var rows int64
	for _, st := range session.Streams {
		stream, err := rc.ReadRows(h.Context(), &storagepb.ReadRowsRequest{ReadStream: st.Name})
		if err != nil {
			t.Fatalf("ReadRows: %v", err)
		}
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if len(resp.GetArrowRecordBatch().GetSerializedRecordBatch()) == 0 && resp.RowCount > 0 {
				t.Error("rows were counted but no Arrow batch was sent")
			}
			rows += resp.RowCount
		}
	}
	if rows != 4 {
		t.Errorf("the Storage Read API returned %d rows, want the 4 inserted", rows)
	}
}

// TestBigQueryOtherProjectsAreNotFound records the single-project limit as
// the client sees it, so a change in it is noticed.
func TestBigQueryOtherProjectsAreNotFound(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	other := c.DatasetInProject(h.Project(), "never")
	err := other.Create(h.Context(), nil)
	if !isNotFound(err) {
		t.Errorf("creating a dataset in another project returned %v, want 404", err)
	}
}

func isNotFound(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}
