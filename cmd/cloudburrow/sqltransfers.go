package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlTransferer runs the Admin API's imports and exports (#1380) with the
// servers' own tools, inside each server's pod, as the backups do:
// pg_dump, psql and COPY for PostgreSQL; mysqldump, mysql, SELECT ... INTO
// OUTFILE and LOAD DATA INFILE for MySQL, as Cloud SQL does.
type sqlTransferer struct{ d *sqlDumper }

func (x sqlTransferer) exec(t sqladmin.DumpTarget) func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
	return x.d.exec(x.d.deployment(t))
}

func (x sqlTransferer) pgUser(t sqladmin.DumpTarget, user string) string {
	if user != "" {
		return user
	}
	p := x.d.archiver(t).(*postgresSnapshotter)
	return p.user()
}

func (x sqlTransferer) mysql(t sqladmin.DumpTarget) *mysqlArchiver {
	return &mysqlArchiver{exec: x.exec(t), bare: t.Created}
}

func (x sqlTransferer) ExportSQL(ctx context.Context, t sqladmin.DumpTarget, e sqladmin.SQLExport, w io.Writer) error {
	if !t.MySQL {
		args := []string{"pg_dump", "-U", x.pgUser(t, ""), "-d", e.Databases[0], "--no-owner", "--no-acl"}
		if e.SchemaOnly {
			args = append(args, "--schema-only")
		}
		if e.Clean {
			args = append(args, "--clean")
		}
		if e.IfExists {
			args = append(args, "--if-exists")
		}
		for _, tb := range e.Tables {
			args = append(args, "--table="+tb)
		}
		return x.exec(t)(ctx, nil, w, args...)
	}
	m := x.mysql(t)
	gtid := "--set-gtid-purged=OFF"
	if e.MasterData == 1 {
		// Cloud SQL sets --set-gtid-purged=ON with masterData 1; AUTO is ON
		// on a server with GTIDs, as every Cloud SQL server has them, and
		// writes no GTID_PURGED on one without, where ON would fail.
		gtid = "--set-gtid-purged=AUTO"
	}
	args := []string{"--single-transaction", "--hex-blob", "--routines", "--triggers", "--events",
		gtid, "--no-tablespaces", "--default-character-set=utf8mb4"}
	if e.MasterData != 0 {
		// MySQL 8.4's name for --master-data: the binary log coordinates
		// as CHANGE REPLICATION SOURCE TO, commented out for 2.
		args = append(args, fmt.Sprintf("--source-data=%d", e.MasterData))
	}
	if e.SchemaOnly {
		args = append(args, "--no-data")
	}
	switch {
	case len(e.Tables) > 0:
		args = append(append(args, e.Databases[0]), e.Tables...)
	case len(e.Databases) > 0:
		args = append(append(args, "--databases"), e.Databases...)
	default:
		names, err := m.databases(ctx)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("the instance has no database to export")
		}
		args = append(append(args, "--databases"), names...)
	}
	return m.sh(ctx, nil, w, "mysqldump", args...)
}

var _ sqladmin.ParallelTransferer = sqlTransferer{}

// shQuote quotes s for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// jsString is s as a JavaScript string literal, for MySQL Shell's --js.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ExportParallel is a parallel export (#1421), the directory Cloud SQL
// writes, as a tar stream of its files: pg_dump -Fd -j for PostgreSQL,
// MySQL Shell's util.dumpSchemas (or dumpTables) with threads for MySQL.
// The directory is made in the server's container and removed after.
func (x sqlTransferer) ExportParallel(ctx context.Context, t sqladmin.DumpTarget, e sqladmin.SQLExport, w io.Writer) error {
	threads := strconv.Itoa(e.Threads)
	if !t.MySQL {
		dump := "pg_dump -U " + shQuote(x.pgUser(t, "")) + " -d " + shQuote(e.Databases[0]) + " -Fd -j " + threads + " -f \"$d/dump\""
		if e.SchemaOnly {
			dump += " --schema-only"
		}
		if e.Clean {
			dump += " --clean"
		}
		if e.IfExists {
			dump += " --if-exists"
		}
		for _, tb := range e.Tables {
			dump += " " + shQuote("--table="+tb)
		}
		script := `set -e; d=$(mktemp -d); trap 'rm -rf "$d"' EXIT; ` + dump + ` >&2; tar -C "$d/dump" -cf - .`
		return x.exec(t)(ctx, nil, w, "sh", "-c", script)
	}
	m := x.mysql(t)
	dbs := e.Databases
	if len(dbs) == 0 {
		var err error
		if dbs, err = m.databases(ctx); err != nil {
			return err
		}
		if len(dbs) == 0 {
			return fmt.Errorf("the instance has no database to export")
		}
	}
	opts := fmt.Sprintf("{threads: %d, ddlOnly: %t, showProgress: false, consistent: true}", e.Threads, e.SchemaOnly)
	var call string
	if len(e.Tables) > 0 {
		tbs, _ := json.Marshal(e.Tables)
		call = "util.dumpTables(" + jsString(dbs[0]) + ", " + string(tbs) + ", D, " + opts + ")"
	} else {
		names, _ := json.Marshal(dbs)
		call = "util.dumpSchemas(" + string(names) + ", D, " + opts + ")"
	}
	script := mysqlRootEnv + `set -e; d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
LC_ALL=C mysqlsh --no-wizard --js --uri root@127.0.0.1:3306 --password="$MYSQL_PWD" -e "var D = '$d/dump'; ` + strings.ReplaceAll(call, `"`, `\"`) + `" >&2
tar -C "$d/dump" -cf - .`
	return m.exec(ctx, nil, w, "sh", "-c", script)
}

// ImportParallel restores a parallel export's directory, read as a tar
// stream: pg_restore -j for PostgreSQL, util.loadDump for MySQL, which
// needs local_infile on while it loads.
func (x sqlTransferer) ImportParallel(ctx context.Context, t sqladmin.DumpTarget, i sqladmin.SQLImport, r io.Reader) error {
	threads := strconv.Itoa(i.Threads)
	if !t.MySQL {
		restore := "pg_restore -U " + shQuote(x.pgUser(t, i.User)) + " -d " + shQuote(i.Database) + " -j " + threads + " --no-owner --no-acl --exit-on-error"
		if i.Clean {
			restore += " --clean"
		}
		if i.IfExists {
			restore += " --if-exists"
		}
		script := `set -e; d=$(mktemp -d); trap 'rm -rf "$d"' EXIT; tar -C "$d" -xf -; test -f "$d/toc.dat" || { echo "the folder holds no pg_dump directory (toc.dat)" >&2; exit 1; }; ` + restore + ` "$d"`
		return x.exec(t)(ctx, r, io.Discard, "sh", "-c", script)
	}
	m := x.mysql(t)
	opts := fmt.Sprintf("{threads: %d, showProgress: false, ignoreVersion: true, progressFile: '', loadUsers: false}", i.Threads)
	if i.Database != "" {
		opts = fmt.Sprintf("{threads: %d, showProgress: false, ignoreVersion: true, progressFile: '', loadUsers: false, schema: %s}", i.Threads, jsString(i.Database))
	}
	script := mysqlRootEnv + `set -e; d=$(mktemp -d)
was=$(mysql $MYSQL_LOCAL -N -B -e 'SELECT @@GLOBAL.local_infile')
trap 'rm -rf "$d"; mysql $MYSQL_LOCAL -e "SET GLOBAL local_infile = $was" || true' EXIT
tar -C "$d" -xf -
test -f "$d/@.json" || { echo "the folder holds no MySQL Shell dump (@.json)" >&2; exit 1; }
mysql $MYSQL_LOCAL -e 'SET GLOBAL local_infile = 1'
LC_ALL=C mysqlsh --no-wizard --js --uri root@127.0.0.1:3306 --password="$MYSQL_PWD" -e "util.loadDump('$d', ` + strings.ReplaceAll(opts, `"`, `\"`) + `)" >&2`
	return m.exec(ctx, r, io.Discard, "sh", "-c", script)
}

func (x sqlTransferer) ImportSQL(ctx context.Context, t sqladmin.DumpTarget, database, user string, r io.Reader) error {
	if !t.MySQL {
		return x.exec(t)(ctx, r, io.Discard, "psql", "-U", x.pgUser(t, user), "-d", database, "-v", "ON_ERROR_STOP=1", "-q", "-f", "-")
	}
	var args []string
	if database != "" {
		args = append(args, database)
	}
	return x.mysql(t).sh(ctx, r, io.Discard, "mysql", args...)
}

func csvDefaults(o sqladmin.CSVOptions) sqladmin.CSVOptions {
	if o.Fields == "" {
		o.Fields = ","
	}
	if o.Quote == "" {
		o.Quote = `"`
	}
	if o.Escape == "" {
		o.Escape = `"`
	}
	if o.Lines == "" {
		o.Lines = "\n"
	}
	return o
}

func pgLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// pgCopyOptions is COPY's WITH clause for o.
func pgCopyOptions(o sqladmin.CSVOptions) string {
	o = csvDefaults(o)
	return fmt.Sprintf("(FORMAT csv, DELIMITER %s, QUOTE %s, ESCAPE %s)", pgLiteral(o.Fields), pgLiteral(o.Quote), pgLiteral(o.Escape))
}

func mysqlLiteral(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return "'" + r.Replace(s) + "'"
}

// mysqlCSVClause is the FIELDS and LINES clause of INTO OUTFILE and LOAD
// DATA, as Cloud SQL writes them.
func mysqlCSVClause(o sqladmin.CSVOptions) string {
	o = csvDefaults(o)
	return fmt.Sprintf("CHARACTER SET 'utf8mb4' FIELDS TERMINATED BY %s OPTIONALLY ENCLOSED BY %s ESCAPED BY %s LINES TERMINATED BY %s",
		mysqlLiteral(o.Fields), mysqlLiteral(o.Quote), mysqlLiteral(o.Escape), mysqlLiteral(o.Lines))
}

// mysqlFilesDir is the server's secure_file_priv: the one directory INTO
// OUTFILE writes to and LOAD DATA INFILE reads from.
func mysqlFilesDir(ctx context.Context, m *mysqlArchiver) (string, error) {
	out, err := m.query(ctx, "SELECT @@secure_file_priv")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if dir == "" || dir == "NULL" {
		return "", fmt.Errorf("the server's secure_file_priv is %q, so it reads and writes no files", dir)
	}
	return strings.TrimRight(dir, "/"), nil
}

func scratchName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "cloudburrow-" + hex.EncodeToString(b) + ".csv"
}

func (x sqlTransferer) ExportCSV(ctx context.Context, t sqladmin.DumpTarget, database, query string, o sqladmin.CSVOptions, w io.Writer) error {
	query = strings.TrimRight(strings.TrimSpace(query), ";")
	if !t.MySQL {
		stmt := "COPY (" + query + ") TO STDOUT WITH " + pgCopyOptions(o)
		return x.exec(t)(ctx, nil, w, "psql", "-U", x.pgUser(t, ""), "-d", database, "-v", "ON_ERROR_STOP=1", "-q", "-c", stmt)
	}
	m := x.mysql(t)
	dir, err := mysqlFilesDir(ctx, m)
	if err != nil {
		return err
	}
	file := dir + "/" + scratchName()
	defer func() { _ = m.exec(context.Background(), nil, io.Discard, "rm", "-f", file) }()
	var args []string
	if database != "" {
		args = append(args, database)
	}
	args = append(args, "-e", query+" INTO OUTFILE "+mysqlLiteral(file)+" "+mysqlCSVClause(o))
	if err := m.sh(ctx, nil, io.Discard, "mysql", args...); err != nil {
		return err
	}
	return m.exec(ctx, nil, w, "cat", file)
}

func (x sqlTransferer) ImportCSV(ctx context.Context, t sqladmin.DumpTarget, database, table string, columns []string, o sqladmin.CSVOptions, user string, r io.Reader) error {
	if !t.MySQL {
		target := table
		if len(columns) > 0 {
			target += " (" + strings.Join(columns, ", ") + ")"
		}
		stmt := "COPY " + target + " FROM STDIN WITH " + pgCopyOptions(o)
		return x.exec(t)(ctx, r, io.Discard, "psql", "-U", x.pgUser(t, user), "-d", database, "-v", "ON_ERROR_STOP=1", "-q", "-c", stmt)
	}
	m := x.mysql(t)
	dir, err := mysqlFilesDir(ctx, m)
	if err != nil {
		return err
	}
	file := dir + "/" + scratchName()
	defer func() { _ = m.exec(context.Background(), nil, io.Discard, "rm", "-f", file) }()
	if err := m.exec(ctx, r, io.Discard, "sh", "-c", `cat > "$1"`, "sh", file); err != nil {
		return err
	}
	stmt := "LOAD DATA INFILE " + mysqlLiteral(file) + " INTO TABLE " + table + " " + mysqlCSVClause(o)
	if len(columns) > 0 {
		stmt += " (" + strings.Join(columns, ", ") + ")"
	}
	return m.sh(ctx, nil, io.Discard, "mysql", database, "-e", stmt)
}

// sqlObjects is the instance's Cloud Storage, through its JSON API.
type sqlObjects struct{ f storageSourceFetcher }

func (o sqlObjects) Read(ctx context.Context, bucket, object string) ([]byte, error) {
	rc, err := o.f.Fetch(ctx, bucket, object, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Create writes a new object with ifGenerationMatch=0: Cloud SQL's export
// fails when the file already exists.
func (o sqlObjects) Create(ctx context.Context, bucket, object string, data []byte) error {
	u := o.f.base() + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&ifGenerationMatch=0&name=" + url.QueryEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := o.f.client.Do(req)
	if err != nil {
		return fmt.Errorf("write gs://%s/%s: %v", bucket, object, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusPreconditionFailed:
		return sqladmin.Errorf(http.StatusConflict, "gs://%s/%s already exists; an export does not overwrite a file", bucket, object)
	case http.StatusNotFound:
		return sqladmin.Errorf(http.StatusNotFound, "the bucket %s does not exist in this instance's Cloud Storage", bucket)
	}
	return fmt.Errorf("write gs://%s/%s: HTTP %d %s", bucket, object, resp.StatusCode, strings.TrimSpace(string(b)))
}

// List names the bucket's objects under prefix, through the JSON API's
// objects.list, every page.
func (o sqlObjects) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	var names []string
	token := ""
	for {
		u := o.f.base() + "/storage/v1/b/" + url.PathEscape(bucket) + "/o?fields=items(name),nextPageToken&prefix=" + url.QueryEscape(prefix)
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := o.f.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list gs://%s/%s: %v", bucket, prefix, err)
		}
		var page struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, sqladmin.Errorf(http.StatusNotFound, "the bucket %s does not exist in this instance's Cloud Storage", bucket)
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("list gs://%s/%s: HTTP %d %s", bucket, prefix, resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 512)])))
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("list gs://%s/%s: %v", bucket, prefix, err)
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if token = page.NextPageToken; token == "" {
			return names, nil
		}
	}
}
