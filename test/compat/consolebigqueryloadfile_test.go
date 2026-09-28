//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// consoleLoadFile posts Load from a file through the console's action
// upload route, as the form does: the action's request, then the file.
func consoleLoadFile(t *testing.T, addr, project string, path []string, values map[string]string, filename string, data []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormField("request")
	_ = json.NewEncoder(w).Encode(map[string]any{"Path": path, "Action": "loadfile", "Values": values})
	part, _ := mw.CreateFormFile("file", filename)
	_, _ = part.Write(data)
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api/actions/bigquery/upload?project="+url.QueryEscape(project), &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// loadFileJob is the job a successful Load from a file answered with.
func loadFileJob(t *testing.T, code int, out, project string) (string, map[string]string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("Load from a file = %d: %s", code, out)
	}
	var res struct {
		Result struct {
			Items []struct {
				Name   string
				Link   string
				Fields map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Result.Items) != 1 {
		t.Fatalf("Load from a file answered %s (%v), want its job", out, err)
	}
	it := res.Result.Items[0]
	if want := "/bigquery-jobs/" + url.PathEscape(it.Name) + "?project=" + url.QueryEscape(project); it.Link != want {
		t.Errorf("the job %s links to %q, want %q", it.Name, it.Link, want)
	}
	return it.Name, it.Fields
}

// TestConsoleBigQueryLoadFromAFile (#999).
//
// Through the console API's action upload route, each outcome read back
// with the official Go client: Load from a file on a dataset's page makes a
// table from a CSV with the form's schema and one header row to skip, and
// answers with the job, linked to its page; on the table's page a
// newline-delimited JSON file is appended, and a CSV of more than the
// console's 4 MiB upload chunk, which the console sends as a resumable
// upload, replaces the rows with WRITE_TRUNCATE. The job reads back DONE,
// loading into the table, and its console page says LOAD with its output
// rows. A CSV option on a JSON load is refused before anything is sent, and
// a file the API cannot read is refused in its words; neither changes the
// table. Offered only on the served project: another project is refused.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get
func TestConsoleBigQueryLoadFromAFile(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := ds.Table("uploaded")

	schema := `[{"name":"id","type":"INTEGER","mode":"NULLABLE"},{"name":"name","type":"STRING","mode":"NULLABLE"}]`
	code, out := consoleLoadFile(t, addr, project, []string{ds.DatasetID}, map[string]string{
		"tableId": "uploaded", "format": "CSV", "writeDisposition": "WRITE_APPEND", "schema": schema,
		"skipLeadingRows": "1", "fieldDelimiter": ",", "quote": `"`, "encoding": "UTF-8", "sourceColumnMatch": "POSITION",
	}, "people.csv", []byte("id,name\n1,ann\n2,bob\n"))
	loadID, fields := loadFileJob(t, code, out, project)
	if fields["State"] != "Succeeded" || fields["Output rows"] != "2" {
		t.Errorf("the load answered %v, want Succeeded with 2 output rows", fields)
	}
	if got := readRowsSorted(t, ctx, tbl); strings.Join(got, " ") != `[1,"ann"] [2,"bob"]` {
		t.Errorf("the loaded table reads %v, want the file's two rows", got)
	}
	job, err := c.JobFromID(ctx, loadID)
	if err != nil {
		t.Fatalf("the console's load job %s: %v", loadID, err)
	}
	if st := job.LastStatus(); st.State != bigquery.Done || st.Err() != nil {
		t.Errorf("the load job reads back %+v, want DONE", st)
	}
	if cfg, err := job.Config(); err != nil {
		t.Errorf("the load job's configuration: %v", err)
	} else if lc, ok := cfg.(*bigquery.LoadConfig); !ok || lc.Dst.DatasetID != ds.DatasetID || lc.Dst.TableID != "uploaded" {
		t.Errorf("the load job's configuration reads back %#v", cfg)
	}
	page := consoleJob(t, addr, project, loadID)
	if page.property("Job", "Type") != "LOAD" || page.property("Statistics", "Output rows") != "2" {
		t.Errorf("the load job's page shows type %q and output rows %q", page.property("Job", "Type"),
			page.property("Statistics", "Output rows"))
	}

	// On the table's page: JSON appended.
	code, out = consoleLoadFile(t, addr, project, []string{ds.DatasetID, "uploaded"}, map[string]string{
		"format": "NEWLINE_DELIMITED_JSON", "writeDisposition": "WRITE_APPEND",
	}, "more.json", []byte(`{"id":3,"name":"cy"}`+"\n"))
	if _, fields := loadFileJob(t, code, out, project); fields["Output rows"] != "1" {
		t.Errorf("the JSON load answered %v, want 1 output row", fields)
	}
	if got := readRowsSorted(t, ctx, tbl); strings.Join(got, " ") != `[1,"ann"] [2,"bob"] [3,"cy"]` {
		t.Errorf("after the JSON load the table reads %v", got)
	}

	// Past one upload chunk: a resumable upload, replacing the rows.
	var big bytes.Buffer
	pad := strings.Repeat("x", 4000)
	n := 0
	for big.Len() <= 5<<20 {
		n++
		fmt.Fprintf(&big, "%d,%s\n", n, pad)
	}
	code, out = consoleLoadFile(t, addr, project, []string{ds.DatasetID, "uploaded"}, map[string]string{
		"format": "CSV", "writeDisposition": "WRITE_TRUNCATE",
	}, "big.csv", big.Bytes())
	if _, fields := loadFileJob(t, code, out, project); fields["Output rows"] != strconv.Itoa(n) {
		t.Errorf("the %d-byte load answered %v, want %d output rows", big.Len(), fields, n)
	}
	it, err := c.Query("SELECT COUNT(*), MIN(id), MAX(id), MIN(LENGTH(name)) FROM `" + ds.DatasetID + ".uploaded`").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var counts []bigquery.Value
	if err := it.Next(&counts); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(counts), fmt.Sprint([]bigquery.Value{int64(n), int64(1), int64(n), int64(len(pad))}); got != want {
		t.Errorf("after the WRITE_TRUNCATE load the table holds %v, want %d rows of the file", counts, n)
	}

	// Refusals change nothing.
	code, out = consoleLoadFile(t, addr, project, []string{ds.DatasetID, "uploaded"}, map[string]string{
		"format": "NEWLINE_DELIMITED_JSON", "fieldDelimiter": "|"}, "bad.json", []byte(`{"id":9}`+"\n"))
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, "fieldDelimiter applies to CSV only") {
		t.Errorf("a JSON load with a field delimiter = %d %q", code, msg)
	}
	code, out = consoleLoadFile(t, addr, project, []string{ds.DatasetID, "uploaded"}, map[string]string{
		"format": "CSV", "writeDisposition": "WRITE_APPEND"}, "wide.csv", []byte("1,a,too,wide\n"))
	if msg := consoleError(t, out); code != http.StatusBadRequest || msg == "" || strings.Contains(msg, "googleapi") {
		t.Errorf("a CSV the table cannot hold = %d %q, want the API's refusal in its words", code, msg)
	}
	it, err = c.Query("SELECT COUNT(*) FROM `" + ds.DatasetID + ".uploaded`").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := it.Next(&counts); err != nil || counts[0] != int64(n) {
		t.Errorf("after the refused loads the table holds %v rows (%v), want %d", counts, err, n)
	}
	code, out = consoleLoadFile(t, addr, project+"-other", []string{ds.DatasetID}, map[string]string{
		"tableId": "elsewhere"}, "a.csv", []byte("1,a\n"))
	if code != http.StatusBadRequest {
		t.Errorf("Load from a file in another project = %d: %s", code, out)
	}
}

// TestConsoleBigQueryJobPageShowsDMLRows (#1041).
//
// A DML statement run through the official Go client (Query.Run, Job.Wait)
// reports its statement type and the rows it changed (#1008); the job's
// page in Job history shows the same statement type, affected rows and
// dmlStats counts the client reads from jobs.get, each only when reported: a
// DELETE that matched nothing shows 0 affected rows and no deleted count,
// and a SELECT no row counts.
//
// covers: bigquery.jobs.get
func TestConsoleBigQueryJobPageShowsDMLRows(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	run := func(sql string) (string, *bigquery.QueryStatistics) {
		t.Helper()
		q := c.Query(sql)
		q.DefaultDatasetID = ds.DatasetID
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st, err := job.Wait(ctx)
		if err != nil || st.Err() != nil {
			t.Fatalf("%s: %v %v", sql, err, st.Err())
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		if qs == nil {
			t.Fatalf("%s: the job reports no query statistics", sql)
		}
		return job.ID(), qs
	}
	for _, sql := range []string{
		"INSERT INTO orders (id, region, amount) VALUES (5, 'ap', 2), (6, 'ap', 3)",
		"UPDATE orders SET region = 'eu2' WHERE region = 'eu'",
		"MERGE orders o USING (SELECT 4 AS id, 'x' AS region UNION ALL SELECT 7, 'nz') s ON o.id = s.id " +
			"WHEN MATCHED THEN UPDATE SET region = s.region " +
			"WHEN NOT MATCHED THEN INSERT (id, region, amount) VALUES (s.id, s.region, 0)",
		"DELETE FROM orders WHERE id = 6",
		"DELETE FROM orders WHERE id = 42",
	} {
		id, qs := run(sql)
		page := consoleJob(t, addr, project, id)
		if got := page.property("Statistics", "Statement type"); got != qs.StatementType || got == "SELECT" {
			t.Errorf("%s: the page's statement type is %q, the client reads %q", sql, got, qs.StatementType)
		}
		if got := page.property("Statistics", "Affected rows"); got != strconv.FormatInt(qs.NumDMLAffectedRows, 10) {
			t.Errorf("%s: the page's affected rows are %q, the client reads %d", sql, got, qs.NumDMLAffectedRows)
		}
		var d bigquery.DMLStatistics
		if qs.DMLStats != nil {
			d = *qs.DMLStats
		}
		for label, n := range map[string]int64{"Inserted rows": d.InsertedRowCount, "Updated rows": d.UpdatedRowCount, "Deleted rows": d.DeletedRowCount} {
			want := ""
			if n != 0 {
				want = strconv.FormatInt(n, 10)
			}
			if got := page.property("Statistics", label); got != want {
				t.Errorf("%s: the page's %s are %q, the client reads %d", sql, label, got, n)
			}
		}
	}
	id, qs := run("SELECT COUNT(*) FROM orders")
	page := consoleJob(t, addr, project, id)
	if got := page.property("Statistics", "Statement type"); got != qs.StatementType || got != "SELECT" {
		t.Errorf("a SELECT's page shows statement type %q, the client reads %q", got, qs.StatementType)
	}
	for _, label := range []string{"Affected rows", "Inserted rows", "Updated rows", "Deleted rows"} {
		if got := page.property("Statistics", label); got != "" {
			t.Errorf("a SELECT's page shows %s %q", label, got)
		}
	}
}
