package bigqueryfront

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/bqiceberg"
)

// BigQuery tables for Apache Iceberg (#1427,
// https://cloud.google.com/bigquery/docs/iceberg-tables).
//
// CREATE TABLE ... WITH CONNECTION ... OPTIONS (file_format = 'PARQUET',
// table_format = 'ICEBERG', storage_uri = 'gs://...'), and tables.insert
// with a biglakeConfiguration (connectionId, storageUri, fileFormat
// PARQUET, tableFormat ICEBERG), make one. The engine keeps serving its
// queries; the front writes its files to the instance's Cloud Storage
// with the Apache Iceberg Go library (internal/bqiceberg):
//
//   - the table's data, as Parquet data files under storage_uri/data/,
//     after each request that changed its rows (DML, a load or query job
//     into it, tabledata.insertAll; a Storage Write API append at the
//     next REST request): each change is one Iceberg snapshot whose data
//     files hold the table's rows then, the earlier files kept;
//   - EXPORT TABLE METADATA FROM t: the table's current Iceberg metadata,
//     storage_uri/metadata/vN.metadata.json, its snapshot's manifest list
//     and manifests, which an Iceberg reader opens. As in BigQuery, the
//     metadata is in storage only once exported.
//
// The instance has no BigQuery Connection API: the connection is kept as
// biglakeConfiguration.connectionId, never checked, and the bucket is
// written with the instance's own access. Columns of a type outside
// bqiceberg.IcebergType (BIGNUMERIC, JSON, GEOGRAPHY, INTERVAL, RANGE, a
// RECORD or REPEATED column) are 501 at CREATE.

// icebergTable is one table for Apache Iceberg the front writes the files
// of.
type icebergTable struct {
	Base     string `json:"base"`
	Dataset  string `json:"dataset"`
	Table    string `json:"table"`
	Location string `json:"location"`
	Created  string `json:"created"`
	// Print is the hash of the rows and schema last written.
	Print string `json:"print,omitempty"`
	// Failed is the hash of the rows and schema whose files could not be
	// written, already answered as the request's error (syncAll).
	Failed string `json:"failed,omitempty"`
}

// icebergTables are the tables for Apache Iceberg, by streamKey.
type icebergTables struct {
	mu    sync.Mutex
	m     map[string]icebergTable
	path  string
	store *bqiceberg.Store
	logf  func(string, ...any)
}

func icebergStateFile(dir string) string { return filepath.Join(dir, "iceberg.json") }

// keep restores the tables from dir and keeps them and the files' mirror
// there.
func (t *icebergTables) keep(dir string, logf func(string, ...any)) {
	t.path, t.logf = icebergStateFile(dir), logf
	t.store = &bqiceberg.Store{Root: filepath.Join(dir, "iceberg")}
	if b, err := os.ReadFile(t.path); err == nil {
		_ = json.Unmarshal(b, &t.m)
	}
}

func (t *icebergTables) save() {
	if t.path == "" {
		return
	}
	b, _ := json.Marshal(t.m)
	if err := os.WriteFile(t.path, b, 0o644); err != nil && t.logf != nil {
		t.logf("bigquery front: iceberg tables: %v", err)
	}
}

func (t *icebergTables) any() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m) > 0
}

// add registers dataset.table of the front f, made at created.
func (t *icebergTables) add(f front, dataset, table, location, created string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]icebergTable{}
	}
	if t.store == nil {
		dir, err := os.MkdirTemp("", "cloudburrow-iceberg-")
		if err != nil {
			return
		}
		t.store = &bqiceberg.Store{Root: dir}
	}
	t.store.Forget(location) // a table made again of the URI starts anew
	t.m[streamKey(projectOf(f.base), dataset, table)] = icebergTable{Base: f.base, Dataset: dataset, Table: table,
		Location: strings.TrimSuffix(location, "/"), Created: created}
	t.save()
}

// icebergConfig reads a table resource's biglakeConfiguration: ok when
// its tableFormat is ICEBERG, msg why BigQuery refuses it (400), or "".
func icebergConfig(raw json.RawMessage) (uri string, ok bool, msg string) {
	var c struct {
		ConnectionID string `json:"connectionId"`
		StorageURI   string `json:"storageUri"`
		FileFormat   string `json:"fileFormat"`
		TableFormat  string `json:"tableFormat"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return "", false, ""
	}
	if c.ConnectionID != "" && c.StorageURI != "" {
		// file_format and table_format are "Required for managed tables",
		// and "The default is PARQUET" and "ICEBERG" (DDL reference,
		// table_option_list, read 2026-10-17): a connection and a
		// storage_uri make a table for Apache Iceberg (#1456).
		c.FileFormat = cmp.Or(c.FileFormat, "PARQUET")
		c.TableFormat = cmp.Or(c.TableFormat, "ICEBERG")
	}
	if c.TableFormat == "" && c.FileFormat == "" {
		return "", false, "" // storage_uri alone, with no connection: kept (tableoptions.go)
	}
	switch {
	case !strings.EqualFold(c.TableFormat, "ICEBERG"):
		return "", false, "table_format must be ICEBERG"
	case !strings.EqualFold(c.FileFormat, "PARQUET"):
		return "", false, "file_format must be PARQUET for a table of table_format ICEBERG"
	case c.ConnectionID == "":
		return "", false, "A BigQuery table for Apache Iceberg needs a connection (WITH CONNECTION, biglakeConfiguration.connectionId)"
	case !strings.HasPrefix(c.StorageURI, "gs://") || !strings.Contains(strings.TrimPrefix(c.StorageURI, "gs://"), "/"):
		return "", false, "storage_uri must be a gs:// URI of a bucket and a path, gs://bucket/path"
	}
	return c.StorageURI, true, ""
}

// icebergColumns are the bqiceberg columns of fields, or the first column
// it does not write.
func icebergColumns(fields []field) ([]bqiceberg.Column, string) {
	cols := make([]bqiceberg.Column, len(fields))
	for i, fl := range fields {
		c := bqiceberg.Column{Name: fl.Name, Type: strings.ToUpper(fl.Type), Required: strings.EqualFold(fl.Mode, "REQUIRED")}
		if p, err := strconv.Atoi(string(fl.Precision)); err == nil && p > 0 {
			c.Precision = p
			c.Scale, _ = strconv.Atoi(string(fl.Scale))
		}
		if strings.EqualFold(fl.Mode, "REPEATED") {
			return nil, fl.Name + " (ARRAY<" + fl.Type + ">)"
		}
		if _, ok := bqiceberg.IcebergType(c); !ok {
			return nil, fl.Name + " (" + fl.Type + ")"
		}
		cols[i] = c
	}
	return cols, ""
}

// icebergValue is a tabledata.list value of c as bqiceberg takes it.
func icebergValue(c bqiceberg.Column, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("a %T value", v)
	}
	switch c.Type {
	case "INT64", "INTEGER":
		return strconv.ParseInt(s, 10, 64)
	case "FLOAT64", "FLOAT":
		switch s {
		case "Infinity":
			s = "+Inf"
		case "-Infinity":
			s = "-Inf"
		}
		return strconv.ParseFloat(s, 64)
	case "BOOL", "BOOLEAN":
		return strconv.ParseBool(s)
	case "STRING":
		return s, nil
	case "BYTES":
		return base64.StdEncoding.DecodeString(s)
	case "NUMERIC":
		scale := 9
		if c.Precision > 0 {
			scale = c.Scale
		}
		n, err := unscaled(s, scale) // avrorows.go
		if err != nil {
			return nil, err
		}
		return new(big.Int).Set(n), nil
	case "DATE":
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, err
		}
		return int32(d.Unix() / 86400), nil
	case "DATETIME":
		t, err := parseDateTime(s)
		return t.UnixMicro(), err
	case "TIMESTAMP":
		t, err := parseTimestamp(s)
		return t.UnixMicro(), err
	case "TIME":
		return parseTimeOfDay(s)
	}
	return nil, fmt.Errorf("a %s value", c.Type)
}

// syncAll writes the files of every table for Apache Iceberg whose rows
// changed since they were last written. r is the request just served. It
// returns the first table whose files could not be written, for the first
// time of its rows then, as the HTTP status and BigQuery error to answer
// instead of the request's own answer (Wrap), or 0: in BigQuery the
// table's data is only in its files, so a write whose files fail is no
// success (#1427).
func (t *icebergTables) syncAll(r *http.Request, mk func(base string) front) (int, rowError) {
	if !t.any() {
		return 0, rowError{}
	}
	t.mu.Lock()
	keys := make([]string, 0, len(t.m))
	for k := range t.m {
		keys = append(keys, k)
	}
	t.mu.Unlock()
	sort.Strings(keys)
	status, why := 0, rowError{}
	for _, k := range keys {
		it, msg, fresh := t.syncFresh(r, mk, k)
		if msg == "" || !fresh || status != 0 || strings.HasPrefix(msg, "the table no longer exists") {
			continue
		}
		status, why = icebergWriteError(it, msg)
	}
	return status, why
}

// icebergWriteError is the answer to a request after which its table's
// files could not be written (msg, sync's).
func icebergWriteError(it icebergTable, msg string) (int, rowError) {
	status, reason := http.StatusBadRequest, "invalid"
	switch {
	case strings.HasPrefix(msg, "the column "):
		status, reason = http.StatusNotImplemented, "notImplemented"
	case strings.Contains(msg, "Cloud Storage answered 404"):
		status, reason = http.StatusNotFound, "notFound"
	case strings.Contains(msg, "Cloud Storage answered 403"):
		status, reason = http.StatusForbidden, "accessDenied"
	}
	return status, rowError{Reason: reason, Message: "The BigQuery table for Apache Iceberg " + it.Dataset + "." + it.Table +
		": its files could not be written to " + it.Location + ": " + msg + ". The engine keeps the change; the files are " +
		"written again at the table's next change and at EXPORT TABLE METADATA."}
}

// sync writes key's files if its rows changed; it returns the table, and
// why it could not be written, or "".
func (t *icebergTables) sync(r *http.Request, mk func(base string) front, key string) (icebergTable, string) {
	it, msg, _ := t.syncFresh(r, mk, key)
	return it, msg
}

// syncFresh is sync; fresh when msg is the first failure to write the
// table's rows then (Failed), so that a bucket that stays unwritable fails
// the request that changed the rows, not every request after it.
func (t *icebergTables) syncFresh(r *http.Request, mk func(base string) front, key string) (icebergTable, string, bool) {
	it, print, msg := t.write(r, mk, key)
	if msg == "" || print == "" {
		// Before its rows were read (a column of a type not written, a
		// read that failed): answered at EXPORT TABLE METADATA alone.
		return it, msg, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.m[key]
	if !ok || cur.Created != it.Created {
		return it, msg, true
	}
	fresh := cur.Failed != print
	cur.Failed = print
	t.m[key] = cur
	t.save()
	return it, msg, fresh
}

// write writes key's files if its rows changed; it returns the table, the
// hash of its rows ("" before they were read), and why its files could not
// be written, or "".
func (t *icebergTables) write(r *http.Request, mk func(base string) front, key string) (icebergTable, string, string) {
	t.mu.Lock()
	it, ok := t.m[key]
	t.mu.Unlock()
	if !ok {
		return it, "", "not a BigQuery table for Apache Iceberg"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	r = r.Clone(ctx)
	r.URL.RawQuery = ""
	f := mk(it.Base)
	status, got := f.get(r, tablePath(it.Dataset, it.Table))
	var meta struct {
		CreationTime string `json:"creationTime"`
		Schema       struct {
			Fields []field `json:"fields"`
		} `json:"schema"`
		Biglake json.RawMessage `json:"biglakeConfiguration"`
	}
	if status == http.StatusNotFound || status == http.StatusOK && json.Unmarshal(got, &meta) == nil && meta.CreationTime != it.Created {
		t.mu.Lock() // dropped, or made again without the configuration
		delete(t.m, key)
		t.save()
		t.mu.Unlock()
		return it, "", "the table no longer exists"
	}
	if status != http.StatusOK {
		return it, "", errorMessage(got, status)
	}
	cols, bad := icebergColumns(meta.Schema.Fields)
	if bad != "" {
		return it, "", "the column " + bad + ", of a type CloudBurrow does not write to Apache Iceberg"
	}
	rows, status, got := f.tableRows(r, it.Dataset, it.Table)
	if status != http.StatusOK {
		return it, "", errorMessage(got, status)
	}
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(meta.Schema.Fields)
	_ = json.NewEncoder(h).Encode(rows)
	print := hex.EncodeToString(h.Sum(nil))
	if print == it.Print {
		return it, print, ""
	}
	conv := make([][]any, len(rows))
	for i, row := range rows {
		conv[i] = make([]any, len(cols))
		for j, c := range cols {
			var v any
			if j < len(row) {
				v = row[j]
			}
			x, err := icebergValue(c, v)
			if err != nil {
				return it, print, fmt.Sprintf("row %d, column %s: %v", i+1, c.Name, err)
			}
			conv[i][j] = x
		}
	}
	data, err := t.store.Write(ctx, it.Location, cols, conv)
	if err != nil {
		return it, print, err.Error()
	}
	for _, uri := range data {
		if msg := t.uploadFile(ctx, f, uri, "application/octet-stream"); msg != "" {
			return it, print, msg
		}
	}
	t.mu.Lock()
	if cur, ok := t.m[key]; ok && cur.Created == it.Created {
		cur.Print, cur.Failed = print, ""
		t.m[key] = cur
		t.save()
	}
	t.mu.Unlock()
	return it, print, ""
}

func (t *icebergTables) uploadFile(ctx context.Context, f front, uri, contentType string) string {
	b, err := os.ReadFile(t.store.LocalPath(uri))
	if err != nil {
		return err.Error()
	}
	if err := f.upload(ctx, uri, contentType, b); err != nil {
		if t.logf != nil {
			t.logf("bigquery front: iceberg: writing %s: %v", uri, err)
		}
		return "writing " + uri + ": " + err.Error()
	}
	return ""
}

// exportMetadataStatement reads sql as EXPORT TABLE METADATA FROM path.
func exportMetadataStatement(sql string) ([]string, bool) {
	t, lexed := lex(sql)
	if !lexed {
		return nil, false
	}
	if n := len(t); n > 0 && t[n-1].punct(";") {
		t = t[:n-1]
	}
	if len(t) < 5 || !t[0].is("EXPORT") || !t[1].is("TABLE") || !t[2].is("METADATA") || !t[3].is("FROM") {
		return nil, false
	}
	p, i := path(t, 4)
	if len(p) == 0 || i != len(t) {
		return nil, false
	}
	return p, true
}

// serveExportMetadata carries out EXPORT TABLE METADATA FROM p (above).
func (f front) serveExportMetadata(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, p []string) {
	failed := func(code int, reason, msg string) {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
	}
	project := projectOf(f.base)
	ds, table, ok := tableOf(q, p)
	if !ok || len(p) == 3 && p[0] != project {
		failed(http.StatusBadRequest, "invalidQuery", "Table \""+strings.Join(p, ".")+"\" must be qualified with a dataset (e.g. dataset.table).")
		return
	}
	key := streamKey(project, ds, table)
	mk := func(string) front { return f }
	f.icebergs.mu.Lock()
	_, known := f.icebergs.m[key]
	f.icebergs.mu.Unlock()
	if !known {
		if status, got := f.get(r, tablePath(ds, table)); status != http.StatusOK {
			failed(status, "notFound", errorMessage(got, status))
			return
		}
		failed(http.StatusBadRequest, "invalidQuery", "EXPORT TABLE METADATA is supported only for BigQuery tables for Apache Iceberg; "+
			project+":"+ds+"."+table+" is not one")
		return
	}
	it, msg := f.icebergs.sync(r, mk, key)
	if strings.HasPrefix(msg, "the column ") {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: EXPORT TABLE METADATA of "+
			ds+"."+table+": "+msg+". Nothing was written.")
		return
	}
	if msg != "" {
		failed(http.StatusInternalServerError, "internalError", "EXPORT TABLE METADATA: "+msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	files, _, err := f.icebergs.store.Metadata(ctx, it.Location)
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("no metadata was written")
	}
	if err != nil {
		failed(http.StatusInternalServerError, "internalError", "EXPORT TABLE METADATA: "+err.Error())
		return
	}
	for i, uri := range files {
		ct := "application/octet-stream"
		if i == 0 {
			ct = "application/json"
		}
		if msg := f.icebergs.uploadFile(ctx, f, uri, ct); msg != "" {
			failed(http.StatusInternalServerError, "internalError", "EXPORT TABLE METADATA: "+msg)
			return
		}
	}
	if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	// statementType: the REST reference's JobStatistics2.statementType
	// (checked 2026-10-16) names no value for EXPORT TABLE METADATA, nor
	// do the EXPORT TABLE METADATA and INFORMATION_SCHEMA.JOBS pages; the
	// front answers EXPORT_DATA, the documented value of the nearest
	// statement, as BigQuery's own is UNVERIFIED (docs/compatibility.md).
	f.forward(w, r, jobText{query: q.Query, ddl: &ownDDL{target: tableRef{ProjectID: project, DatasetID: ds, TableID: table},
		statementType: "EXPORT_DATA"}})
}

// withIcebergDefaults sets tables.insert's body, b, as r's with its
// biglakeConfiguration's fileFormat and tableFormat defaulted
// (icebergConfig, #1456), when either is missing.
func withIcebergDefaults(r *http.Request, b []byte) {
	var t map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&t) != nil || t == nil {
		return
	}
	var c map[string]any
	if json.Unmarshal(t["biglakeConfiguration"], &c) != nil || c == nil || c["fileFormat"] != nil && c["tableFormat"] != nil {
		return
	}
	if c["fileFormat"] == nil {
		c["fileFormat"] = "PARQUET"
	}
	if c["tableFormat"] == nil {
		c["tableFormat"] = "ICEBERG"
	}
	t["biglakeConfiguration"], _ = json.Marshal(c)
	if out, err := json.Marshal(t); err == nil {
		setBody(r, out)
	}
}

// insertIcebergTable is tables.insert (insertTable): of a
// biglakeConfiguration of tableFormat ICEBERG, the configuration is
// checked and the table registered (above).
func (f front) insertIcebergTable(w http.ResponseWriter, r *http.Request, dataset string) {
	b, err := readBody(r)
	var body struct {
		Biglake json.RawMessage `json:"biglakeConfiguration"`
		Schema  *struct {
			Fields []field `json:"fields"`
		} `json:"schema"`
	}
	if err != nil || json.Unmarshal(b, &body) != nil || len(body.Biglake) == 0 {
		f.insertTable(w, r, dataset, "", false)
		return
	}
	uri, ok, msg := icebergConfig(body.Biglake)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	if !ok {
		f.insertTable(w, r, dataset, "", false)
		return
	}
	if body.Schema != nil {
		if _, bad := icebergColumns(body.Schema.Fields); bad != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", icebergColumnRefused(bad))
			return
		}
	}
	withIcebergDefaults(r, b)
	rec := newRecorder()
	f.insertTable(rec, r, dataset, "", false)
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.body.Bytes())
	var made struct {
		CreationTime   string `json:"creationTime"`
		TableReference struct {
			TableID string `json:"tableId"`
		} `json:"tableReference"`
	}
	if status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), &made) == nil && made.TableReference.TableID != "" {
		f.icebergs.add(f, dataset, made.TableReference.TableID, uri, made.CreationTime)
	}
}

// checkIceberg is why BigQuery refuses d's table for Apache Iceberg
// (icebergConfig), or "".
func (d ddlOptions) checkIceberg() string {
	conf := map[string]any{}
	format := false
	for _, o := range d.more {
		if part, ok := o.value.(map[string]any); ok && o.key == "biglakeConfiguration" {
			for k, v := range part {
				conf[k] = v
				format = format || k == "fileFormat" || k == "tableFormat" || k == "storageUri" && d.connection != ""
			}
		}
	}
	if !format {
		if d.connection != "" {
			return "WITH CONNECTION is for a BigQuery table for Apache Iceberg: OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = ...)"
		}
		return ""
	}
	conf["connectionId"] = d.connection
	b, _ := json.Marshal(conf)
	_, _, msg := icebergConfig(b)
	return msg
}

// icebergColumnRefused is the 501 of bad, icebergColumns's, at the table's
// making.
func icebergColumnRefused(bad string) string {
	return "Not implemented here: the column " + bad + " of a BigQuery table for Apache Iceberg: CloudBurrow writes " +
		"INT64, FLOAT64, BOOL, STRING, BYTES, NUMERIC, DATE, DATETIME, TIME and TIMESTAMP columns to Apache Iceberg. " +
		"Nothing was made."
}

// iceberg is whether d makes a BigQuery table for Apache Iceberg: its
// OPTIONS give a file_format or table_format, or WITH CONNECTION and a
// storage_uri (#1456: the formats' defaults).
func (d ddlOptions) iceberg() bool {
	if d.alter || d.view {
		return false
	}
	for _, o := range d.more {
		if part, ok := o.value.(map[string]any); ok && o.key == "biglakeConfiguration" {
			if _, ok := part["tableFormat"]; ok {
				return true
			}
			if _, ok := part["fileFormat"]; ok {
				return true
			}
			if _, ok := part["storageUri"]; ok && d.connection != "" { // the formats' defaults (icebergConfig)
				return true
			}
		}
	}
	return false
}

// icebergColumnsOf is the CREATE TABLE form's check of tables.insert's
// (insertIcebergTable): of d, a table for Apache Iceberg, the 501 of its
// first column of a type CloudBurrow does not write, from the statement's
// column list, else from the schema of its AS query (run with LIMIT 0);
// or "", nothing having been made.
func (f front) icebergColumnsOf(r *http.Request, q queryOptions, d ddlOptions) string {
	var fields []field
	if len(d.columns) > 0 {
		cols := d.columns
		for i := 0; i < len(cols); {
			end := skipTo(cols, i, ",")
			if end > len(cols) {
				end = len(cols)
			}
			c := cols[i:end]
			i = end + 1
			if len(c) < 2 || !isName(c[0]) {
				continue
			}
			ft, _, what := fieldType(c, 1)
			if what != "" {
				continue // the engine answers the statement as written
			}
			ft["name"] = c[0].text
			var fl field
			if b, err := json.Marshal(ft); err == nil && json.Unmarshal(b, &fl) == nil {
				fields = append(fields, fl)
			}
		}
	} else if query := ctasQuery(q.Query); query != "" {
		legacy := false
		req, err := json.Marshal(queryOptions{Query: "SELECT * FROM (" + query + "\n) LIMIT 0", UseLegacySQL: &legacy,
			DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
		if err != nil {
			return ""
		}
		status, got := f.send(r, http.MethodPost, "/queries", req)
		var res struct {
			Schema struct {
				Fields []field `json:"fields"`
			} `json:"schema"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
			return "" // the statement fails as it does
		}
		fields = res.Schema.Fields
	}
	if _, bad := icebergColumns(fields); bad != "" {
		return icebergColumnRefused(bad)
	}
	return ""
}

// ctasQuery is the query of sql, a CREATE TABLE ... AS query (its first AS
// outside parentheses), or "".
func ctasQuery(sql string) string {
	t, lexed := lex(sql)
	if !lexed {
		return ""
	}
	if n := len(t); n > 0 && t[n-1].punct(";") {
		t = t[:n-1]
	}
	for j := 0; j < len(t); j++ {
		if t[j].punct("(") || t[j].punct("[") {
			j = skipTo(t, j+1, ")", "]")
			continue
		}
		if t[j].is("AS") && j+1 < len(t) {
			return sql[t[j].end:t[len(t)-1].end]
		}
	}
	return ""
}
