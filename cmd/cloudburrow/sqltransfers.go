package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	args := []string{"--single-transaction", "--hex-blob", "--routines", "--triggers", "--events",
		"--set-gtid-purged=OFF", "--no-tablespaces", "--default-character-set=utf8mb4"}
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
