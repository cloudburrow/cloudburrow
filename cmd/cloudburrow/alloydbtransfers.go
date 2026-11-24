package main

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// alloyDBTransfers runs AlloyDB's exports and imports (#2191) in each
// server's pod with PostgreSQL's own tools, as Cloud SQL's do
// (sqlTransferer): pg_dump and psql for SQL, COPY for CSV.
type alloyDBTransfers struct{ e *alloyDBEngine }

var _ alloydb.Transfers = alloyDBTransfers{}

func (x alloyDBTransfers) t() sqlTransferer { return sqlTransferer{d: x.e.dump} }

func csvOf(f alloydb.CSVFormat) sqladmin.CSVOptions {
	return sqladmin.CSVOptions{Fields: f.Delimiter, Quote: f.Quote, Escape: f.Escape}
}

func (x alloyDBTransfers) ExportSQL(ctx context.Context, server string, e alloydb.SQLExport, w io.Writer) error {
	return x.t().ExportSQL(ctx, x.e.target(server), sqladmin.SQLExport{Databases: []string{e.Database}, Tables: e.Tables,
		SchemaOnly: e.SchemaOnly, Clean: e.Clean, IfExists: e.IfExists}, w)
}

func (x alloyDBTransfers) ExportCSV(ctx context.Context, server, database, query string, f alloydb.CSVFormat, w io.Writer) error {
	return x.t().ExportCSV(ctx, x.e.target(server), database, query, csvOf(f), w)
}

func (x alloyDBTransfers) ImportSQL(ctx context.Context, server, database, user string, r io.Reader) error {
	return x.t().ImportSQL(ctx, x.e.target(server), database, user, r)
}

func (x alloyDBTransfers) ImportCSV(ctx context.Context, server, database, table string, columns []string, f alloydb.CSVFormat, user string, r io.Reader) error {
	return x.t().ImportCSV(ctx, x.e.target(server), database, table, columns, csvOf(f), user, r)
}

// alloyDBObjects is the instance's Cloud Storage, through its JSON API.
type alloyDBObjects struct{ f storageSourceFetcher }

var _ alloydb.Objects = alloyDBObjects{}

func (o alloyDBObjects) Read(ctx context.Context, bucket, object string) ([]byte, error) {
	rc, err := o.f.Fetch(ctx, bucket, object, 0)
	if err != nil {
		if e, ok := err.(*apierror.Error); ok && e.HTTPStatus() == http.StatusNotFound {
			return nil, apierror.NotFound("gs://%s/%s was not found in this instance's Cloud Storage", bucket, object)
		}
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Write replaces the object, as AlloyDB's export does. UNVERIFIED:
// whether Google's export overwrites an existing object.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/export-sql-file.)
func (o alloyDBObjects) Write(ctx context.Context, bucket, object string, data []byte) error {
	err := o.f.Put(ctx, "", bucket, object, data, false)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return apierror.NotFound("the bucket %s does not exist in this instance's Cloud Storage", bucket)
	}
	if err != nil {
		return apierror.FailedPrecondition("%v", err)
	}
	return nil
}
