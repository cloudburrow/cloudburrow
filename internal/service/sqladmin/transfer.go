package sqladmin

// Imports and exports (#1380): instances.export writes a SQL dump
// (pg_dump / mysqldump) or the CSV of a query to the instance's own Cloud
// Storage, and instances.import loads one from it, as operations of type
// EXPORT and IMPORT that operations.cancel can stop while they run.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// CSVOptions are csvExportOptions and csvImportOptions, each character
// decoded from the hex Cloud SQL takes ("2C" is a comma). Empty is the
// default: Cloud SQL's are a comma, a double quote as both quote and
// escape, and a newline.
type CSVOptions struct {
	Fields, Quote, Escape, Lines string
}

// SQLExport is what a SQL export dumps.
type SQLExport struct {
	Databases  []string
	Tables     []string
	SchemaOnly bool
	// Clean and IfExists are postgresExportOptions'.
	Clean, IfExists bool
	// MasterData is mysqlExportOptions.masterData: 1 writes the binary log
	// coordinates as a CHANGE REPLICATION SOURCE TO statement, 2 as a
	// comment (mysqldump --source-data).
	MasterData int
	// Threads is a parallel export's threads.
	Threads int
}

// SQLImport is what a parallel SQL import restores.
type SQLImport struct {
	Database, User string
	Threads        int
	// Clean and IfExists are postgresImportOptions' (pg_restore's flags).
	Clean, IfExists bool
}

// ParallelTransferer is a Transferer that also runs parallel exports and
// imports (#1421): a directory of files, which ExportParallel writes and
// ImportParallel reads as a tar stream of the directory's files: pg_dump's
// directory format (-Fd -j) for PostgreSQL, MySQL Shell's dump
// (util.dumpSchemas) for MySQL, as Cloud SQL writes them.
type ParallelTransferer interface {
	ExportParallel(ctx context.Context, t DumpTarget, x SQLExport, w io.Writer) error
	ImportParallel(ctx context.Context, t DumpTarget, x SQLImport, r io.Reader) error
}

// Transferer runs the dumps and loads of imports and exports in a server.
type Transferer interface {
	ExportSQL(ctx context.Context, t DumpTarget, x SQLExport, w io.Writer) error
	// ImportSQL runs the file's statements in database, which is empty when
	// the file names its own; user is importUser (PostgreSQL), or empty.
	ImportSQL(ctx context.Context, t DumpTarget, database, user string, r io.Reader) error
	ExportCSV(ctx context.Context, t DumpTarget, database, query string, o CSVOptions, w io.Writer) error
	ImportCSV(ctx context.Context, t DumpTarget, database, table string, columns []string, o CSVOptions, user string, r io.Reader) error
}

// Objects is the instance's Cloud Storage.
type Objects interface {
	// Read returns the object's contents; a missing one is a NOT_FOUND
	// error.
	Read(ctx context.Context, bucket, object string) ([]byte, error)
	// Create writes a new object, and fails if it already exists.
	Create(ctx context.Context, bucket, object string, data []byte) error
	// List names the objects whose names start with prefix.
	List(ctx context.Context, bucket, prefix string) ([]string, error)
}

type transfers struct {
	t   Transferer
	obj Objects
	mu  sync.Mutex
	// cancels are the running imports' and exports' cancel functions, by
	// operation name.
	cancels map[string]context.CancelFunc
}

// EnableTransfers serves instances.import and instances.export over t and
// the instance's Cloud Storage.
func (s *Server) EnableTransfers(t Transferer, obj Objects) {
	s.emu.Lock()
	s.transfers = &transfers{t: t, obj: obj, cancels: map[string]context.CancelFunc{}}
	s.emu.Unlock()
}

// errCancelled is what an operation cancelled by operations.cancel failed
// with: Cloud SQL's operation error is CANCEL_SUCCESSFUL.
var errCancelled = &apiError{code: http.StatusOK, status: "CANCEL_SUCCESSFUL", msg: "Operation successfully cancelled"}

func parseGCSURI(uri string) (bucket, object string, err error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	b, o, _ := strings.Cut(rest, "/")
	if !ok || b == "" || o == "" {
		return "", "", Errorf(http.StatusBadRequest, "Invalid request: uri %q is not of the form gs://bucketName/fileName.", uri)
	}
	return b, o, nil
}

// csvChar decodes one csv*Options character: Cloud SQL takes its ASCII code
// in hex ("2C"); a single literal character is taken as itself.
func csvChar(field, v string) (string, error) {
	switch len(v) {
	case 0, 1:
		return v, nil
	case 2:
		if b, err := hex.DecodeString(v); err == nil {
			return string(b), nil
		}
	}
	return "", Errorf(http.StatusBadRequest, "Invalid request: %s %q is not a character's hex ASCII code, such as 2C.", field, v)
}

type csvOptsJSON struct {
	SelectQuery        string   `json:"selectQuery"`
	Table              string   `json:"table"`
	Columns            []string `json:"columns"`
	EscapeCharacter    string   `json:"escapeCharacter"`
	QuoteCharacter     string   `json:"quoteCharacter"`
	FieldsTerminatedBy string   `json:"fieldsTerminatedBy"`
	LinesTerminatedBy  string   `json:"linesTerminatedBy"`
}

func (c *csvOptsJSON) options() (CSVOptions, error) {
	var o CSVOptions
	var err error
	for _, f := range []struct {
		name, v string
		dst     *string
	}{{"fieldsTerminatedBy", c.FieldsTerminatedBy, &o.Fields}, {"quoteCharacter", c.QuoteCharacter, &o.Quote},
		{"escapeCharacter", c.EscapeCharacter, &o.Escape}, {"linesTerminatedBy", c.LinesTerminatedBy, &o.Lines}} {
		if *f.dst, err = csvChar(f.name, f.v); err != nil {
			return o, err
		}
	}
	return o, nil
}

func (s *Server) noTransfers(what string) error {
	return unimplemented(what, "this server was started without the instance's Cloud Storage (docs/cloudsql.md)")
}

const bakWhy = "BAK and TDE files are SQL Server's, and CloudBurrow serves PostgreSQL and MySQL only"

func (s *Server) exportInstance(r *http.Request, base, project string, e Engine) (any, error) {
	if s.transfers == nil {
		return nil, s.noTransfers("instances.export")
	}
	var req struct {
		ExportContext *struct {
			URI              string       `json:"uri"`
			FileType         string       `json:"fileType"`
			Databases        []string     `json:"databases"`
			Offload          bool         `json:"offload"`
			CSVExportOptions *csvOptsJSON `json:"csvExportOptions"`
			SQLExportOptions *struct {
				Tables                []string `json:"tables"`
				SchemaOnly            bool     `json:"schemaOnly"`
				Parallel              bool     `json:"parallel"`
				Threads               int64    `json:"threads"`
				PostgresExportOptions *struct {
					Clean    bool `json:"clean"`
					IfExists bool `json:"ifExists"`
				} `json:"postgresExportOptions"`
				MysqlExportOptions *struct {
					MasterData int64 `json:"masterData"`
				} `json:"mysqlExportOptions"`
			} `json:"sqlExportOptions"`
			BakExportOptions any `json:"bakExportOptions"`
			TdeExportOptions any `json:"tdeExportOptions"`
		} `json:"exportContext"`
	}
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if err := decodeBytes(raw, &req); err != nil {
		return nil, err
	}
	x := req.ExportContext
	if x == nil {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: exportContext.")
	}
	bucket, object, err := parseGCSURI(x.URI)
	if err != nil {
		return nil, err
	}
	t := s.target(e)
	ft := x.FileType
	if ft == "" || ft == "SQL_FILE_TYPE_UNSPECIFIED" {
		ft = "SQL"
	}
	var run func(ctx context.Context, w io.Writer) error
	switch ft {
	case "BAK", "TDE":
		return nil, unimplemented("exports of fileType "+ft, bakWhy)
	case "SQL":
		sx := SQLExport{Databases: x.Databases}
		if o := x.SQLExportOptions; o != nil {
			if o.Threads < 0 {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: sqlExportOptions.threads must be positive.")
			}
			if o.Threads > 0 && !o.Parallel {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: sqlExportOptions.threads applies only to a parallel export (sqlExportOptions.parallel).")
			}
			if o.Parallel {
				sx.Threads = int(o.Threads)
				if sx.Threads == 0 {
					sx.Threads = defaultThreads
				}
			}
			if m := o.MysqlExportOptions; m != nil && m.MasterData != 0 {
				if !t.MySQL {
					return nil, Errorf(http.StatusBadRequest, "Invalid request: mysqlExportOptions are for MySQL instances only.")
				}
				if m.MasterData != 1 && m.MasterData != 2 {
					return nil, Errorf(http.StatusBadRequest, "Invalid request: mysqlExportOptions.masterData must be 1 or 2 (mysqldump --source-data), not %d.", m.MasterData)
				}
				if o.Parallel {
					return nil, Errorf(http.StatusBadRequest, "Invalid request: mysqlExportOptions.masterData applies to a mysqldump export, not a parallel one.")
				}
				sx.MasterData = int(m.MasterData)
			}
			sx.Tables, sx.SchemaOnly = o.Tables, o.SchemaOnly
			if p := o.PostgresExportOptions; p != nil {
				sx.Clean, sx.IfExists = p.Clean, p.IfExists
			}
		}
		if sx.Threads > 0 {
			return s.exportParallel(base, project, e, t, raw, bucket, object, sx)
		}
		if !t.MySQL && len(sx.Databases) != 1 {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: a PostgreSQL SQL export names exactly one database "+
				"(all user databases are exported only by a parallel export).")
		}
		if len(sx.Tables) > 0 && len(sx.Databases) != 1 {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: tables are exported from one and only one database.")
		}
		if !t.MySQL && len(sx.Tables) > 1 {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: a PostgreSQL export names only one table.")
		}
		run = func(ctx context.Context, w io.Writer) error { return s.transfers.t.ExportSQL(ctx, t, sx, w) }
	case "CSV":
		c := x.CSVExportOptions
		if c == nil || strings.TrimSpace(c.SelectQuery) == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: csvExportOptions.selectQuery is required for a CSV export.")
		}
		o, err := c.options()
		if err != nil {
			return nil, err
		}
		if !t.MySQL && o.Lines != "" && o.Lines != "\n" {
			return nil, unimplemented("linesTerminatedBy other than 0A on PostgreSQL", "PostgreSQL's COPY ends every row with a newline")
		}
		if len(x.Databases) > 1 {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: a CSV export reads one database.")
		}
		db := ""
		if len(x.Databases) == 1 {
			db = x.Databases[0]
		}
		if db == "" && !t.MySQL {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: a PostgreSQL CSV export names its database in databases.")
		}
		q := c.SelectQuery
		run = func(ctx context.Context, w io.Writer) error { return s.transfers.t.ExportCSV(ctx, t, db, q, o, w) }
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: fileType %q is not SQL, CSV or BAK.", ft)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	gz := strings.HasSuffix(object, ".gz")
	ctxJSON := map[string]any{}
	_ = decodeBytes(raw, &struct {
		ExportContext *map[string]any `json:"exportContext"`
	}{&ctxJSON})
	ctxJSON["kind"] = "sql#exportContext"
	ctxJSON["fileType"] = ft
	return s.transfer(base, project, e, "EXPORT", "exportContext", ctxJSON, func(ctx context.Context) error {
		var buf bytes.Buffer
		var w io.Writer = &buf
		var zw *gzip.Writer
		if gz {
			zw = gzip.NewWriter(&buf)
			w = zw
		}
		if err := run(ctx, w); err != nil {
			return err
		}
		if zw != nil {
			if err := zw.Close(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return errCancelled
		}
		return s.transfers.obj.Create(ctx, bucket, object, buf.Bytes())
	}), nil
}

func (s *Server) importInstance(r *http.Request, base, project string, e Engine) (any, error) {
	if s.transfers == nil {
		return nil, s.noTransfers("instances.import")
	}
	var req struct {
		ImportContext *struct {
			URI              string       `json:"uri"`
			FileType         string       `json:"fileType"`
			Database         string       `json:"database"`
			ImportUser       string       `json:"importUser"`
			CSVImportOptions *csvOptsJSON `json:"csvImportOptions"`
			SQLImportOptions *struct {
				Parallel              bool  `json:"parallel"`
				Threads               int64 `json:"threads"`
				PostgresImportOptions *struct {
					Clean    bool `json:"clean"`
					IfExists bool `json:"ifExists"`
				} `json:"postgresImportOptions"`
			} `json:"sqlImportOptions"`
		} `json:"importContext"`
	}
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if err := decodeBytes(raw, &req); err != nil {
		return nil, err
	}
	x := req.ImportContext
	if x == nil {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: importContext.")
	}
	bucket, object, err := parseGCSURI(x.URI)
	if err != nil {
		return nil, err
	}
	t := s.target(e)
	ft := x.FileType
	if ft == "" || ft == "SQL_FILE_TYPE_UNSPECIFIED" {
		ft = "SQL"
	}
	if x.ImportUser != "" && t.MySQL {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: importUser is for PostgreSQL instances only.")
	}
	var run func(ctx context.Context, r io.Reader) error
	switch ft {
	case "BAK", "TDE":
		return nil, unimplemented("imports of fileType "+ft, bakWhy)
	case "SQL":
		if o := x.SQLImportOptions; o != nil {
			if o.Threads < 0 {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: sqlImportOptions.threads must be positive.")
			}
			if o.Threads > 0 && !o.Parallel {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: sqlImportOptions.threads applies only to a parallel import (sqlImportOptions.parallel).")
			}
			pg := o.PostgresImportOptions
			if pg != nil && (pg.Clean || pg.IfExists) && (!o.Parallel || t.MySQL) {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: postgresImportOptions.clean and ifExists apply only to a parallel import into a PostgreSQL instance.")
			}
			if o.Parallel {
				xi := SQLImport{Database: x.Database, User: x.ImportUser, Threads: int(o.Threads)}
				if xi.Threads == 0 {
					xi.Threads = defaultThreads
				}
				if pg != nil {
					xi.Clean, xi.IfExists = pg.Clean, pg.IfExists
				}
				return s.importParallel(base, project, e, t, raw, bucket, object, xi)
			}
		}
		if !t.MySQL && x.Database == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: importContext.database is required for a PostgreSQL SQL import.")
		}
		db, user := x.Database, x.ImportUser
		run = func(ctx context.Context, r io.Reader) error { return s.transfers.t.ImportSQL(ctx, t, db, user, r) }
	case "CSV":
		c := x.CSVImportOptions
		if c == nil || c.Table == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: csvImportOptions.table is required for a CSV import.")
		}
		if x.Database == "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: importContext.database is required for a CSV import.")
		}
		o, err := c.options()
		if err != nil {
			return nil, err
		}
		if !t.MySQL && o.Lines != "" && o.Lines != "\n" {
			return nil, unimplemented("linesTerminatedBy other than 0A on PostgreSQL", "PostgreSQL's COPY reads rows ended by a newline")
		}
		db, table, cols, user := x.Database, c.Table, c.Columns, x.ImportUser
		run = func(ctx context.Context, r io.Reader) error {
			return s.transfers.t.ImportCSV(ctx, t, db, table, cols, o, user, r)
		}
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: fileType %q is not SQL, CSV or BAK.", ft)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	ctxJSON := map[string]any{}
	_ = decodeBytes(raw, &struct {
		ImportContext *map[string]any `json:"importContext"`
	}{&ctxJSON})
	ctxJSON["kind"] = "sql#importContext"
	ctxJSON["fileType"] = ft
	return s.transfer(base, project, e, "IMPORT", "importContext", ctxJSON, func(ctx context.Context) error {
		data, err := s.transfers.obj.Read(ctx, bucket, object)
		if err != nil {
			return err
		}
		var rd io.Reader = bytes.NewReader(data)
		if strings.HasSuffix(object, ".gz") && ft == "SQL" {
			zr, err := gzip.NewReader(rd)
			if err != nil {
				return Errorf(http.StatusBadRequest, "%s ends in .gz but is not gzip: %v", x.URI, err)
			}
			rd = zr
		}
		if err := run(ctx, rd); err != nil {
			if ctx.Err() != nil {
				return errCancelled
			}
			return err
		}
		return nil
	}), nil
}

// transfer runs fn in the background as a cancellable operation of opType.
func (s *Server) transfer(base, project string, e Engine, opType, ctxKey string, ctxJSON map[string]any, fn func(context.Context) error) map[string]any {
	op := s.pending(base, project, e.Instance(), opType)
	id := op["name"].(string)
	s.mu.Lock()
	s.ops[id][ctxKey] = ctxJSON
	s.mu.Unlock()
	op[ctxKey] = ctxJSON
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	tr := s.transfers
	tr.mu.Lock()
	tr.cancels[id] = cancel
	tr.mu.Unlock()
	go func() {
		err := fn(ctx)
		if err != nil && ctx.Err() == context.Canceled {
			err = errCancelled
		}
		tr.mu.Lock()
		delete(tr.cancels, id)
		tr.mu.Unlock()
		cancel()
		s.finish(id, err)
	}()
	return op
}

// cancelOperation is operations.cancel. Cloud SQL cancels imports and
// exports while they run; any other operation, or one already done, cannot
// be cancelled.
func (s *Server) cancelOperation(project, name string) (any, error) {
	op, err := s.operation(project, name)
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	if s.transfers != nil {
		s.transfers.mu.Lock()
		cancel = s.transfers.cancels[name]
		s.transfers.mu.Unlock()
	}
	// Cloud SQL's documented messages; their HTTP status is not documented.
	if t := op["operationType"]; t != "IMPORT" && t != "EXPORT" {
		return nil, Errorf(http.StatusBadRequest, "You can't cancel operation %s because Cloud SQL doesn't support the cancellation of an %v operation.", name, t)
	}
	if op["status"] == "DONE" || cancel == nil {
		return nil, Errorf(http.StatusBadRequest, "You can't cancel operation %s because this operation isn't in progress.", name)
	}
	cancel()
	return map[string]any{}, nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request body: %v", err)
	}
	return b, nil
}

func decodeBytes(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	r, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	return decode(r, v)
}

// defaultThreads is a parallel export's or import's threads when the
// request names none.
const defaultThreads = 4

// maxParallelFiles bounds the files a parallel import reads.
const maxParallelFiles = 10000

func (s *Server) parallelTransferer(what string) (ParallelTransferer, error) {
	p, ok := s.transfers.t.(ParallelTransferer)
	if !ok {
		return nil, unimplemented(what, "this server's transferer runs no parallel dumps")
	}
	return p, nil
}

// folder is a parallel transfer's uri as a folder: the object name prefix
// the directory's files are written under, ending in a slash.
func folder(object string) string { return strings.TrimRight(object, "/") + "/" }

// exportParallel is a parallel export (#1421): the dump directory's files
// are written under the uri, a folder, which must be empty, as Cloud SQL
// requires.
func (s *Server) exportParallel(base, project string, e Engine, t DumpTarget, raw []byte, bucket, object string, x SQLExport) (any, error) {
	p, err := s.parallelTransferer("parallel exports")
	if err != nil {
		return nil, err
	}
	if !t.MySQL && len(x.Databases) != 1 {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a PostgreSQL parallel export names exactly one database.")
	}
	if len(x.Tables) > 0 && len(x.Databases) != 1 {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: tables are exported from one and only one database.")
	}
	if strings.HasSuffix(object, ".gz") {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a parallel export's uri is a folder, not a file: %s.", object)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	dir := folder(object)
	return s.transfer(base, project, e, "EXPORT", "exportContext", transferContext(raw, "exportContext", "sql#exportContext"), func(ctx context.Context) error {
		names, err := s.transfers.obj.List(ctx, bucket, dir)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return Errorf(http.StatusConflict, "The folder gs://%s/%s is not empty; a parallel export writes to an empty folder.", bucket, dir)
		}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := p.ExportParallel(ctx, t, x, pw)
			pw.CloseWithError(err)
			done <- err
		}()
		files, rerr := untar(pr)
		if rerr == nil {
			// tar pads its archive past the end marker untar stops at.
			_, _ = io.Copy(io.Discard, pr)
		}
		_ = pr.CloseWithError(rerr)
		if err := <-done; err != nil {
			return err
		}
		if rerr != nil {
			return rerr
		}
		if err := ctx.Err(); err != nil {
			return errCancelled
		}
		for _, f := range files {
			if err := s.transfers.obj.Create(ctx, bucket, dir+f.name, f.data); err != nil {
				return err
			}
		}
		return nil
	}), nil
}

// importParallel is a parallel import (#1421) of the folder a parallel
// export wrote.
func (s *Server) importParallel(base, project string, e Engine, t DumpTarget, raw []byte, bucket, object string, x SQLImport) (any, error) {
	p, err := s.parallelTransferer("parallel imports")
	if err != nil {
		return nil, err
	}
	if !t.MySQL && x.Database == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: importContext.database is required for a PostgreSQL SQL import.")
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	dir := folder(object)
	return s.transfer(base, project, e, "IMPORT", "importContext", transferContext(raw, "importContext", "sql#importContext"), func(ctx context.Context) error {
		names, err := s.transfers.obj.List(ctx, bucket, dir)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return Errorf(http.StatusNotFound, "The folder gs://%s/%s holds no files to import.", bucket, dir)
		}
		if len(names) > maxParallelFiles {
			return Errorf(http.StatusBadRequest, "The folder gs://%s/%s holds more than %d files.", bucket, dir, maxParallelFiles)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, n := range names {
			rel := strings.TrimPrefix(n, dir)
			if rel == "" || strings.HasSuffix(rel, "/") || strings.Contains(rel, "..") {
				continue
			}
			data, err := s.transfers.obj.Read(ctx, bucket, n)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		if err := p.ImportParallel(ctx, t, x, &buf); err != nil {
			if ctx.Err() != nil {
				return errCancelled
			}
			return err
		}
		return nil
	}), nil
}

// transferContext is the request's context, as the operation reports it.
func transferContext(raw []byte, key, kind string) map[string]any {
	m := map[string]any{}
	var wrap map[string]json.RawMessage
	if decodeBytes(raw, &wrap) == nil {
		_ = json.Unmarshal(wrap[key], &m)
	}
	m["kind"] = kind
	m["fileType"] = "SQL"
	return m
}

type tarFile struct {
	name string
	data []byte
}

// untar reads a tar stream's regular files, by their paths in it.
func untar(r io.Reader) ([]tarFile, error) {
	tr := tar.NewReader(r)
	var out []tarFile
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || strings.Contains(name, "..") {
			continue
		}
		out = append(out, tarFile{name, data})
	}
}
