//go:build compat

package compat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryUndeleteKeepsExternalTables (#1522): BigQuery's undelete
// restores a dataset "to the state it was in when it was deleted"
// (https://cloud.google.com/bigquery/docs/restore-deleted-datasets), its
// external tables among it; an external table's data is not BigQuery's but
// its source's (https://cloud.google.com/bigquery/docs/external-tables), so
// what comes back is its definition. An external table over a CSV object
// in Cloud Storage, through datasets.delete with deleteContents and
// datasets.undelete: tables.get reads it back EXTERNAL with its source
// URIs, and a query reads its rows from the source. A copy job of a deleted
// external table at a time it existed fails invalid, as a copy of a live
// one does. Before, a deletion kept no external table (its type was none
// the front kept), and the undeleted dataset had none.
//
// covers: bigquery.tables.insert, bigquery.tables.get, bigquery.datasets.delete, bigquery.datasets.undelete, bigquery.jobs.insert
func TestBigQueryUndeleteKeepsExternalTables(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	bucket := loadBucket(t, h, "bq-ext-undelete", map[string]string{"rows.csv": "id,name\n1,apples\n2,pears\n"})
	advanceBigQueryClock(t, h, 0)
	ctx := h.Context()
	ds := c.Dataset("compat_undelete_ext_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	uri := "gs://" + bucket.BucketName() + "/rows.csv"
	edc := &bigquery.ExternalDataConfig{SourceFormat: bigquery.CSV, SourceURIs: []string{uri},
		Options: &bigquery.CSVOptions{SkipLeadingRows: 1},
		Schema:  bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}}
	if err := ds.Table("ext").Create(ctx, &bigquery.TableMetadata{ExternalDataConfig: edc}); err != nil {
		t.Fatalf("create the external table: %v", err)
	}
	read := func() string {
		t.Helper()
		it, err := c.Query("SELECT id, name FROM `" + ds.DatasetID + ".ext` ORDER BY id").Read(ctx)
		if err != nil {
			t.Fatalf("read the external table: %v", err)
		}
		return strings.Join(readRows(t, it, "external table"), "; ")
	}
	want := read()
	if !strings.Contains(want, "apples") || !strings.Contains(want, "pears") {
		t.Fatalf("the external table reads %q", want)
	}
	advanceBigQueryClock(t, h, time.Second)
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("delete the dataset: %v", err)
	}
	if err := undeleteDataset(ctx, t, h, ds.DatasetID); err != nil {
		t.Fatalf("datasets.undelete: %v", err)
	}
	md, err := ds.Table("ext").Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get of the undeleted external table: %v", err)
	}
	if md.Type != bigquery.ExternalTable || md.ExternalDataConfig == nil ||
		len(md.ExternalDataConfig.SourceURIs) != 1 || md.ExternalDataConfig.SourceURIs[0] != uri {
		t.Errorf("the undeleted table: type %q, external %+v; want EXTERNAL over %s", md.Type, md.ExternalDataConfig, uri)
	}
	if got := read(); got != want {
		t.Errorf("the undeleted external table reads %q, want %q", got, want)
	}

	// A copy at a time it existed, of the table deleted alone.
	existed := advanceBigQueryClock(t, h, time.Second)
	advanceBigQueryClock(t, h, time.Second)
	if err := ds.Table("ext").Delete(ctx); err != nil {
		t.Fatalf("delete the external table: %v", err)
	}
	src := ds.Table(fmt.Sprintf("ext@%d", existed.UnixMilli()))
	job, err := ds.Table("restored").CopierFrom(src).Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	var e *googleapi.Error
	var be *bigquery.Error
	switch {
	case err == nil:
		t.Errorf("a copy of the deleted external table: no error")
	case errors.As(err, &e) && e.Code == http.StatusNotImplemented:
		t.Errorf("a copy of the deleted external table: %v, want invalid", err)
	case errors.As(err, &be) && be.Reason != "invalid":
		t.Errorf("a copy of the deleted external table: reason %q (%v), want invalid", be.Reason, err)
	}
}

// undeleteDataset sends datasets.undelete of dataset through the official
// discovery client.
func undeleteDataset(ctx context.Context, t *testing.T, h *Harness, dataset string) error {
	svc := bigqueryService(t, h)
	_, err := svc.Datasets.Undelete(strings.TrimSpace(os.Getenv(EnvBigQueryProject)), dataset, nil).Context(ctx).Do()
	return err
}

// TestBigQueryUndeleteKeepsRoutinesTheFrontDidNotSee (#1522): a function
// routines.insert made before the front started, which the front never
// saw, comes back with its dataset. Made here through the official Go
// client sent to the emulator itself, past the front (a port-forward to
// the pod's emulator port), as a function made before the front started
// is: the front's routines.get does not find it. After datasets.delete
// with deleteContents and datasets.undelete, it runs, and routines.get
// reads it back with its body and arguments. Before, the front kept only
// the functions it knew of, and the emulator's routines.get and
// routines.list were stubs ("unsupported"); bigquery-emulator patch 0053
// keeps each Routine routines.insert makes.
//
// covers: bigquery.routines.insert, bigquery.routines.get, bigquery.datasets.delete, bigquery.datasets.undelete, bigquery.jobs.query
func TestBigQueryUndeleteKeepsRoutinesTheFrontDidNotSee(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := forwardBigQueryEmulator(t, h)
	advanceBigQueryClock(t, h, 0)
	ctx := h.Context()
	name := "compat_undelete_unseen_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(name)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	direct, err := bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery.NewClient of the emulator: %v", err)
	}
	t.Cleanup(func() { _ = direct.Close() })
	meta := &bigquery.RoutineMetadata{
		Type:       bigquery.ScalarFunctionRoutine,
		Language:   "SQL",
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
		Body:       "x * 3",
	}
	if err := direct.Dataset(name).Routine("thrice").Create(ctx, meta); err != nil {
		t.Fatalf("routines.insert sent to the emulator: %v", err)
	}
	call := func() (string, error) {
		it, err := c.Query("SELECT `" + name + ".thrice`(7)").Read(ctx)
		if err != nil {
			return "", err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return "", err
		}
		return fmt.Sprint(row[0]), nil
	}
	if got, err := call(); err != nil || got != "21" {
		t.Fatalf("the function before the deletion: %q, %v", got, err)
	}
	if _, err := ds.Routine("thrice").Metadata(ctx); err == nil {
		t.Logf("the front's routines.get found the function made past it")
	}
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("delete the dataset: %v", err)
	}
	if _, err := call(); err == nil {
		t.Errorf("the function of the deleted dataset still runs")
	}
	if err := undeleteDataset(ctx, t, h, name); err != nil {
		t.Fatalf("datasets.undelete: %v", err)
	}
	if got, err := call(); err != nil || got != "21" {
		t.Errorf("the function after the undelete: %q, %v; want 21", got, err)
	}
	md, err := ds.Routine("thrice").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get after the undelete: %v", err)
	}
	if md.Body != "x * 3" || len(md.Arguments) != 1 || md.Arguments[0].Name != "x" || md.Type != bigquery.ScalarFunctionRoutine {
		t.Errorf("routines.get after the undelete: body %q, type %q, %d arguments", md.Body, md.Type, len(md.Arguments))
	}
	var ids []string
	it := ds.Routines(ctx)
	for {
		r, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("routines.list: %v", err)
		}
		ids = append(ids, r.RoutineID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "thrice" {
		t.Errorf("routines.list after the undelete: %v", ids)
	}
}

// forwardBigQueryEmulator port-forwards a local port to the BigQuery
// pod's emulator port, past the front, and returns its address.
func forwardBigQueryEmulator(t *testing.T, h *Harness) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the emulator is reached through the cluster", envKubeconfig)
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "port-forward", "--address", "127.0.0.1", "deployment/bigquery", ":9051")
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("kubectl port-forward: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(out)
		re := regexp.MustCompile(`Forwarding from (127\.0\.0\.1:[0-9]+)`)
		for s.Scan() {
			if m := re.FindStringSubmatch(s.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward did not say its port")
	}
	return ""
}

// TestBigQueryChangeHistoryOfChangeNamedColumns (#1522): a table whose
// column is named _CHANGE_NOTE keeps its change history. BigQuery reserves
// only the column name prefixes _CHANGE_SEQUENCE_NUMBER, _CHANGE_TYPE and
// _CHANGE_TIMESTAMP ("Column names can't use any of the following
// prefixes", https://cloud.google.com/bigquery/docs/schemas), which
// tables.insert and CREATE TABLE refuse 400; another name that starts
// _CHANGE_ is the table's own. APPENDS and CHANGES read its inserts,
// update and deletion with the column's values, and FOR SYSTEM_TIME AS OF
// a time before the update reads the row as it was. Before, the engine
// kept no history of a table with any column whose name starts _CHANGE_
// (googlesqlite patch 0036), so APPENDS read nothing; googlesqlite patch
// 0063 leaves out only the change table's own names.
//
// covers: bigquery.tables.insert, bigquery.jobs.query
func TestBigQueryChangeHistoryOfChangeNamedColumns(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	advanceBigQueryClock(t, h, 0)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".noted`"
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(sql string) string {
		t.Helper()
		return strings.Join(changeRows(t, rows(t, ctx, c, sql)), "; ")
	}
	for _, col := range []string{"_CHANGE_TYPE_X", "_change_timestamp", "_CHANGE_SEQUENCE_NUMBER"} {
		err := ds.Table("reserved").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: col, Type: bigquery.StringFieldType}}})
		var e *googleapi.Error
		if err == nil || !errors.As(err, &e) || e.Code != http.StatusBadRequest {
			t.Errorf("tables.insert of a column %s: %v, want 400", col, err)
			_ = ds.Table("reserved").Delete(ctx)
		}
	}
	if err := ds.Table("noted").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "product", Type: bigquery.StringFieldType}, {Name: "_CHANGE_NOTE", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatalf("tables.insert with a column _CHANGE_NOTE: %v", err)
	}
	run("ALTER TABLE " + ref + " SET OPTIONS (enable_change_history = TRUE)")
	run("INSERT INTO " + ref + " VALUES ('apples', 'first'), ('pears', 'second')")
	advanceBigQueryClock(t, h, time.Second)
	before := currentMicros(t, h, c)
	advanceBigQueryClock(t, h, time.Second)
	run("UPDATE " + ref + " SET _CHANGE_NOTE = 'updated' WHERE product = 'apples'")
	run("DELETE " + ref + " WHERE product = 'pears'")
	if got := read("SELECT product, _CHANGE_NOTE, _CHANGE_TYPE FROM APPENDS(TABLE " + ref + ", NULL, NULL) ORDER BY product"); got != "apples first INSERT; pears second INSERT" {
		t.Errorf("APPENDS: %q", got)
	}
	if got := read("SELECT product, _CHANGE_NOTE, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", " + before + ", NULL) ORDER BY product, _CHANGE_TYPE"); got != "apples first DELETE; apples updated UPDATE; pears second DELETE" {
		t.Errorf("CHANGES after the inserts: %q", got)
	}
	if got := read("SELECT product, _CHANGE_NOTE FROM " + ref + " FOR SYSTEM_TIME AS OF " + before + " ORDER BY product"); got != "apples first; pears second" {
		t.Errorf("FOR SYSTEM_TIME AS OF before the update: %q", got)
	}
}

// TestBigQueryAlterSchemaTimeTravelLetsGoFirst (#1522): ALTER SCHEMA … SET
// OPTIONS (max_time_travel_hours = …) changes the dataset's time travel
// window as datasets.patch does (#1503,
// https://cloud.google.com/bigquery/docs/time-travel: a lengthened window
// does not bring back what the shorter one let go). In a dataset of a
// 48-hour window, 49 hours on (the instance's clock), ALTER SCHEMA makes
// it 168 hours: datasets.get reads 168, and APPENDS and CHANGES from before
// the 48 hours read nothing of what the shorter window let go, while the
// row still there is read by FOR SYSTEM_TIME AS OF, through jobs.query and
// jobs.insert. Before, the history outside the old window was read again.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.get
func TestBigQueryAlterSchemaTimeTravelLetsGoFirst(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	advanceBigQueryClock(t, h, 0)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		ds := c.Dataset(fmt.Sprintf("%s_alter_window_%v", strings.ReplaceAll(h.Project(), "-", "_"), insert))
		if err := ds.Create(ctx, &bigquery.DatasetMetadata{MaxTimeTravel: 48 * time.Hour}); err != nil {
			t.Fatalf("create dataset: %v", err)
		}
		t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
		ref := "`" + ds.DatasetID + ".kept`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		read := func(sql string) string {
			t.Helper()
			return strings.Join(changeRows(t, rows(t, ctx, c, sql)), "; ")
		}
		run("CREATE TABLE " + ref + " (product STRING, inventory INT64) OPTIONS(enable_change_history = TRUE)")
		start := currentMicros(t, h, c)
		run("INSERT INTO " + ref + " VALUES ('apples', 10), ('pears', 20)")
		run("DELETE " + ref + " WHERE product = 'pears'")
		advanceBigQueryClock(t, h, 49*time.Hour)
		mid := currentMicros(t, h, c)
		run("INSERT INTO " + ref + " VALUES ('figs', 3)")
		run("ALTER SCHEMA `" + ds.DatasetID + "` SET OPTIONS (max_time_travel_hours = 168)")
		md, err := ds.Metadata(ctx)
		if err != nil {
			t.Fatalf("datasets.get: %v", err)
		}
		if md.MaxTimeTravel != 168*time.Hour {
			t.Errorf("insert %v: the window after ALTER SCHEMA: %v, want 168h", insert, md.MaxTimeTravel)
		}
		if got := read("SELECT product, inventory, _CHANGE_TYPE FROM APPENDS(TABLE " + ref + ", " + start + ", NULL) ORDER BY product"); got != "figs 3 INSERT" {
			t.Errorf("insert %v: APPENDS from before the 48-hour window, after ALTER SCHEMA made it 168 hours: %q", insert, got)
		}
		if got := read("SELECT product, inventory, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", " + start +
			", TIMESTAMP_ADD(" + start + ", INTERVAL 1 DAY))"); got != "" {
			t.Errorf("insert %v: CHANGES from before the 48-hour window: %q", insert, got)
		}
		if got := read("SELECT product, inventory FROM " + ref + " FOR SYSTEM_TIME AS OF " + mid + " ORDER BY product"); got != "apples 10" {
			t.Errorf("insert %v: FOR SYSTEM_TIME AS OF before the new insert: %q", insert, got)
		}
	}
}
