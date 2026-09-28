//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// consoleJobPage is the part of a Job history page these tests read.
type consoleJobPage struct {
	Unavailable string
	Prompt      string
	Summary     []struct{ Label, Value string }
	Actions     []struct{ ID string }
	Sections    []struct {
		ID     string
		Text   string
		Groups []struct {
			Heading    string
			Properties []struct{ Label, Value string }
		}
		Listing struct {
			Note  string
			Items []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
}

// property is the value of a property of the page's details, "" if absent.
func (p consoleJobPage) property(heading, label string) string {
	for _, s := range p.Sections {
		for _, g := range s.Groups {
			if g.Heading != heading {
				continue
			}
			for _, pr := range g.Properties {
				if pr.Label == label {
					return pr.Value
				}
			}
		}
	}
	return ""
}

func (p consoleJobPage) section(id string) (text string, rows []map[string]string) {
	for _, s := range p.Sections {
		if s.ID == id {
			for _, it := range s.Listing.Items {
				row := map[string]string{"#": it.Name}
				for k, v := range it.Fields {
					row[k] = v
				}
				rows = append(rows, row)
			}
			return s.Text, rows
		}
	}
	return "", nil
}

func consoleJob(t *testing.T, addr, project, id string) consoleJobPage {
	t.Helper()
	q := url.Values{"project": {project}, "name": {id}}
	var page consoleJobPage
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery-jobs?"+q.Encode(), "", &page)
	if page.Unavailable != "" || page.Prompt != "" {
		t.Fatalf("the job %s's page: %+v", id, page)
	}
	return page
}

// consoleJobResult performs a load or an export through the console API and
// returns the job its result names.
func consoleJobResult(t *testing.T, addr, project string, path []string, action string, values map[string]string) (string, map[string]string, string) {
	t.Helper()
	code, out := consoleAct(t, addr, "bigquery", project, path, action, values)
	if code != http.StatusOK {
		t.Fatalf("console %s on %v = %d: %s", action, path, code, out)
	}
	var res struct {
		Result struct {
			Note  string
			Items []struct {
				Name   string
				Link   string
				Fields map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Result.Items) != 1 {
		t.Fatalf("console %s on %v answered %s (%v), want its job", action, path, out, err)
	}
	it := res.Result.Items[0]
	if want := "/bigquery-jobs/" + url.PathEscape(it.Name) + "?project=" + url.QueryEscape(project); it.Link != want {
		t.Errorf("the job %s links to %q, want %q", it.Name, it.Link, want)
	}
	return it.Name, it.Fields, res.Result.Note
}

// readRows is every row of a table, each rendered as JSON, sorted.
func readRowsSorted(t *testing.T, ctx context.Context, tbl *bigquery.Table) []string {
	t.Helper()
	var got []string
	it := tbl.Read(ctx)
	for {
		var r []bigquery.Value
		err := it.Next(&r)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", tbl.TableID, err)
		}
		b, _ := json.Marshal(r)
		got = append(got, string(b))
	}
	sort.Strings(got)
	return got
}

// TestConsoleBigQueryLoadExportAndJobHistory (#993).
//
// Through the console API, each outcome read back with the official Go
// client (and, for badRecords, which that client's LoadStatistics lacks,
// the official generated client): Load from Cloud Storage on a dataset's
// page makes a table from a CSV in the instance's Cloud Storage with the
// form's schema, skipLeadingRows and maxBadRecords, leaving the bad record
// out, and answers with the job, linked to its page; on the table's page a
// pipe-delimited file with a \N null marker is appended. A load of a URI
// that does not exist is refused in the API's words and makes nothing, and
// a CSV option on a JSON load is refused before anything is sent. Export to
// Cloud Storage writes the table to an object the storage client reads back,
// with the form's delimiter and header; a JSON export of a NULL is the API's
// 501, in its words. Job history lists both jobs, newest first, with their
// type and state; a load job's page shows its configuration, its load
// statistics (output rows, bad records, input files and bytes, as jobs.get
// reports them) and the bad record in status.errors; an export's page its
// destination and files. jobs.cancel of a done job leaves it done, and no
// page offers Cancel job on one. Delete job, on an emulator job and on a job
// the front ran, leaves jobs.get 404. Another project's Job history is the
// one-project prompt.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.list, bigquery.jobs.cancel, bigquery.jobs.delete
func TestConsoleBigQueryLoadExportAndJobHistory(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	sc := instanceStorage(t, h)
	gen, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	q := "?project=" + url.QueryEscape(project)
	ds := validationDataset(t, h, c)
	bucketName := strings.ReplaceAll(h.Project(), "_", "-") + "-bq-console"
	bucket := sc.Bucket(bucketName)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(context.Background(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	put := func(name, data string) {
		t.Helper()
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write gs://%s/%s: %v", bucketName, name, err)
		}
	}
	put("people.csv", "id,name\n1,ann\n2,bob\n9,too,wide\n")
	put("more.csv", "3|\\N\n")

	// Load from Cloud Storage on the dataset's page, into a new table.
	schema := `[{"name":"id","type":"INTEGER","mode":"NULLABLE"},{"name":"name","type":"STRING","mode":"NULLABLE"}]`
	loadID, fields, note := consoleJobResult(t, addr, project, []string{ds.DatasetID}, "load", map[string]string{
		"tableId": "people", "uris": "gs://" + bucketName + "/people.csv", "format": "CSV",
		"writeDisposition": "WRITE_APPEND", "schema": schema, "skipLeadingRows": "1", "maxBadRecords": "1",
		"fieldDelimiter": ",", "quote": `"`, "encoding": "UTF-8", "sourceColumnMatch": "POSITION",
	})
	if fields["State"] != "Succeeded" || fields["Output rows"] != "2" || fields["Input files"] != "1" {
		t.Errorf("the load answered %v, want Succeeded with 2 output rows from 1 file", fields)
	}
	if !strings.Contains(note, "left out 1 bad record;") {
		t.Errorf("the load's note is %q, want the one bad record named", note)
	}
	tbl := ds.Table("people")
	if got := readRowsSorted(t, ctx, tbl); strings.Join(got, " ") != `[1,"ann"] [2,"bob"]` {
		t.Errorf("the loaded table reads %v, want the two good rows", got)
	}
	job, err := c.JobFromID(ctx, loadID)
	if err != nil {
		t.Fatalf("the console's load job %s: %v", loadID, err)
	}
	st := job.LastStatus()
	if st.State != bigquery.Done || st.Err() != nil || len(st.Errors) != 1 {
		t.Errorf("the load job reads back %+v, want DONE with the one bad record", st)
	}
	cfg, err := job.Config()
	if err != nil {
		t.Fatalf("the load job's configuration: %v", err)
	}
	// The front loads a CSV from Cloud Storage as an upload of the same
	// job (#944), so the job reads back with its options and without its
	// sourceUris (#998).
	if lc, ok := cfg.(*bigquery.LoadConfig); !ok || lc.Dst.TableID != "people" || lc.WriteDisposition != bigquery.WriteAppend {
		t.Errorf("the load job's configuration reads back %#v", cfg)
	}

	// On the table's page: a pipe-delimited file with a null marker,
	// appended.
	_, fields, _ = consoleJobResult(t, addr, project, []string{ds.DatasetID, "people"}, "load", map[string]string{
		"uris": "gs://" + bucketName + "/more.csv", "format": "CSV", "writeDisposition": "WRITE_APPEND",
		"fieldDelimiter": "|", "nullMarker": `\N`,
	})
	if fields["Output rows"] != "1" {
		t.Errorf("the second load answered %v, want 1 output row", fields)
	}
	if got := readRowsSorted(t, ctx, tbl); strings.Join(got, " ") != `[1,"ann"] [2,"bob"] [3,null]` {
		t.Errorf("after the second load the table reads %v", got)
	}

	// Refusals: the API's for a URI that does not exist, the form's for a
	// CSV option on a JSON load. Neither makes a table.
	code, out := consoleAct(t, addr, "bigquery", project, []string{ds.DatasetID}, "load", map[string]string{
		"tableId": "missing", "uris": "gs://" + bucketName + "/nope.csv", "schema": schema})
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, "nope.csv") || strings.Contains(msg, "googleapi") {
		t.Errorf("a load of a missing object = %d %q, want the API's refusal naming it", code, msg)
	}
	code, out = consoleAct(t, addr, "bigquery", project, []string{ds.DatasetID}, "load", map[string]string{
		"tableId": "json", "uris": "gs://" + bucketName + "/people.csv", "format": "NEWLINE_DELIMITED_JSON",
		"fieldDelimiter": "|"})
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, "fieldDelimiter applies to CSV only") {
		t.Errorf("a JSON load with a field delimiter = %d %q", code, msg)
	}
	for _, name := range []string{"missing", "json"} {
		if _, err := ds.Table(name).Metadata(ctx); err == nil {
			t.Errorf("a refused load made the table %s", name)
		}
	}

	// Export to Cloud Storage, with a delimiter and a header row.
	exportID, fields, _ := consoleJobResult(t, addr, project, []string{ds.DatasetID, "people"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/out/people.csv", "format": "CSV", "compression": "NONE",
		"fieldDelimiter": "|", "header": "true"})
	if fields["State"] != "Succeeded" || fields["Files written"] != "1" {
		t.Errorf("the export answered %v", fields)
	}
	r, err := bucket.Object("out/people.csv").NewReader(ctx)
	if err != nil {
		t.Fatalf("the export's object: %v", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 4 || lines[0] != "id|name" {
		t.Errorf("the export wrote %q", b)
	} else {
		body := lines[1:]
		sort.Strings(body)
		if strings.Join(body, " ") != "1|ann 2|bob 3|" {
			t.Errorf("the export's rows are %q", body)
		}
	}
	code, out = consoleAct(t, addr, "bigquery", project, []string{ds.DatasetID, "people"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/out/people.json", "format": "NEWLINE_DELIMITED_JSON", "compression": "NONE",
		"fieldDelimiter": ",", "header": "true"})
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, "Not implemented here") || !strings.Contains(msg, "NULL") {
		t.Errorf("a JSON export of a NULL = %d %q, want the API's 501 in its words", code, msg)
	}

	// Job history: both jobs, newest first.
	var list consoleListing
	consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery-jobs"+q, "", &list)
	if list.Unavailable != "" || list.Prompt != "" {
		t.Fatalf("Job history: %+v", list)
	}
	at := map[string]int{}
	for i, it := range list.Items {
		at[it.Name] = i
		switch it.Name {
		case loadID:
			if it.Fields["Type"] != "LOAD" || it.Fields["Created"] == "—" {
				t.Errorf("Job history lists the load as %v", it.Fields)
			}
		case exportID:
			if it.Fields["Type"] != "EXTRACT" {
				t.Errorf("Job history lists the export as %v", it.Fields)
			}
		}
	}
	li, lok := at[loadID]
	ei, eok := at[exportID]
	if !lok || !eok || ei > li {
		t.Errorf("Job history lists the load at %d (%v) and the export at %d (%v), want both, the export first", li, lok, ei, eok)
	}

	page := consoleJob(t, addr, project, loadID)
	generated, err := gen.Jobs.Get(project, loadID).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.get %s: %v", loadID, err)
	}
	stats := generated.Statistics.Load
	for label, want := range map[string]int64{
		"Output rows": stats.OutputRows, "Bad records": stats.BadRecords,
		"Input files": stats.InputFiles, "Input file bytes": stats.InputFileBytes,
	} {
		if got := page.property("Statistics", label); got != jsonInt(want) {
			t.Errorf("the load's page shows %s %q; jobs.get reports %d", label, got, want)
		}
	}
	if stats.OutputRows != 2 || stats.BadRecords != 1 || stats.InputFiles != 1 {
		t.Errorf("jobs.get reports the load's statistics as %+v", stats)
	}
	if got := page.property("Load", "Destination table"); got != project+"."+ds.DatasetID+".people" {
		t.Errorf("the load's page names the destination %q", got)
	}
	if got := page.property("Load", "Header rows to skip"); got != "1" {
		t.Errorf("the load's page shows %q header rows skipped", got)
	}
	if _, errs := page.section("errors"); len(errs) != 1 || errs[0]["Reason"] != generated.Status.Errors[0].Reason ||
		errs[0]["Message"] != generated.Status.Errors[0].Message {
		t.Errorf("the load's page lists the errors %v; jobs.get reports %+v", errs, generated.Status.Errors[0])
	}
	if text, _ := page.section("configuration"); !strings.Contains(text, `"skipLeadingRows": 1`) || !strings.Contains(text, `"maxBadRecords": 1`) {
		t.Errorf("the load's configuration tab reads %q", text)
	}
	if ids := page.actionIDs(); strings.Join(ids, ",") != "delete" {
		t.Errorf("a done load job's page offers %v, want Delete job alone", ids)
	}

	xpage := consoleJob(t, addr, project, exportID)
	if got := xpage.property("Export", "Destination URIs"); got != "gs://"+bucketName+"/out/people.csv" {
		t.Errorf("the export's page names the destination %q", got)
	}
	if got := xpage.property("Statistics", "Files written per URI"); got != "1" {
		t.Errorf("the export's page shows %q files written", got)
	}

	// jobs.cancel of a done job, the emulator's and the front's, through the
	// client: each stays done.
	for _, id := range []string{loadID, exportID} {
		j, err := c.JobFromID(ctx, id)
		if err != nil {
			t.Fatalf("jobs.get %s: %v", id, err)
		}
		if err := j.Cancel(ctx); err != nil {
			t.Errorf("jobs.cancel of the done job %s: %v", id, err)
		}
		if st, err := j.Status(ctx); err != nil || st.State != bigquery.Done || st.Err() != nil {
			t.Errorf("after jobs.cancel the job %s reads %+v (%v)", id, st, err)
		}
	}

	// Delete job: an emulator job and one the front ran.
	for _, id := range []string{loadID, exportID} {
		code, out := consoleAct(t, addr, "bigquery-jobs", project, []string{id}, "delete", nil)
		if code != http.StatusOK {
			t.Fatalf("console Delete job %s = %d: %s", id, code, out)
		}
		if _, err := c.JobFromID(ctx, id); !isNotFound(err) {
			t.Errorf("after Delete job, jobs.get of %s = %v, want 404", id, err)
		}
	}

	var other consoleListing
	consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery-jobs?project=another-project", "", &other)
	if !strings.Contains(other.Prompt, project) || len(other.Items) != 0 {
		t.Errorf("another project's Job history = %+v, want the prompt naming %s", other, project)
	}
}

func (p consoleJobPage) actionIDs() []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.ID)
	}
	return out
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
