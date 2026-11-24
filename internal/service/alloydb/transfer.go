package alloydb

// ExportCluster and ImportCluster (#2191), as Google documents them
// (docs.cloud.google.com/alloydb/docs/export-sql-file, export-csv-file,
// import-sql-file, import-csv-file and the REST reference of
// projects.locations.clusters.export and .import, read 2026-11-23):
//
//   - an export writes one object, gcs_destination.uri (gs://bucket/file),
//     from database, as SQL (pg_dump, with tables, schema_only,
//     clean_target_objects and if_exist_target_objects) or as CSV (the
//     rows of select_query); an object whose name ends in .gz is
//     compressed;
//   - an import reads gcs_uri, SQL (psql, which a \connect in the file can
//     move to another database) or CSV into table (and columns, every one
//     by default), as user when one is given, and answers the bytes it
//     downloaded; a .gz object is decompressed;
//   - CSV's field delimiter, quote and escape characters are hex ASCII
//     codes, with or without 0x ("2C"); the defaults are a comma, a double
//     quote, and the quote character;
//   - the cluster's service agent reads and writes the bucket.
//
// Both run on the cluster's primary instance: UNVERIFIED (the pages do not
// say which instance runs them), as is every error message here.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/export-sql-file.)

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"io"
	"strings"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// CSVFormat is a CSV file's delimiter, quote and escape characters.
type CSVFormat struct {
	Delimiter, Quote, Escape string
}

// SQLExport is what an SQL export dumps.
type SQLExport struct {
	Database   string
	Tables     []string
	SchemaOnly bool
	// Clean drops each object before creating it; IfExists with it drops
	// only those that exist.
	Clean, IfExists bool
}

// Transfers runs exports and imports with a server's own tools.
type Transfers interface {
	ExportSQL(ctx context.Context, server string, x SQLExport, w io.Writer) error
	ExportCSV(ctx context.Context, server, database, query string, f CSVFormat, w io.Writer) error
	ImportSQL(ctx context.Context, server, database, user string, r io.Reader) error
	ImportCSV(ctx context.Context, server, database, table string, columns []string, f CSVFormat, user string, r io.Reader) error
}

// Objects is the instance's Cloud Storage.
type Objects interface {
	// Read is an object's contents; a missing object or bucket is an
	// apierror NOT_FOUND.
	Read(ctx context.Context, bucket, object string) ([]byte, error)
	// Write writes an object, replacing one of the same name.
	Write(ctx context.Context, bucket, object string, data []byte) error
}

// gcsURI is a gs://bucket/object URI's bucket and object.
func gcsURI(field, uri string) (string, string, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" || strings.HasSuffix(object, "/") {
		return "", "", apierror.InvalidArgument("%s %q must be a Cloud Storage object, gs://bucketName/fileName", field, uri)
	}
	return bucket, object, nil
}

// csvChar is a CSV option's character, from its hex ASCII code ("2C" or
// "0x2C"); "" is def.
func csvChar(field, v, def string) (string, error) {
	if v == "" {
		return def, nil
	}
	h := strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X")
	if b, err := hex.DecodeString(h); err == nil && len(b) == 1 && b[0] < 0x80 {
		return string(b), nil
	}
	return "", apierror.InvalidArgument("%s %q must be one ASCII character's hex code, such as 2C", field, v)
}

func csvFormat(prefix, delim, quote, escape string) (CSVFormat, error) {
	var f CSVFormat
	var err error
	if f.Delimiter, err = csvChar(prefix+".field_delimiter", delim, ","); err != nil {
		return f, err
	}
	if f.Quote, err = csvChar(prefix+".quote_character", quote, `"`); err != nil {
		return f, err
	}
	if f.Escape, err = csvChar(prefix+".escape_character", escape, f.Quote); err != nil {
		return f, err
	}
	return f, nil
}

// transferTarget is the server of cluster's READY primary, for an export
// or import; the caller holds mu.
func (s *Server) transferTarget(name string) (*alloydbpb.Cluster, string, error) {
	c, err := s.cluster(name)
	if err != nil {
		return nil, "", err
	}
	if s.opts.Transfers == nil || s.opts.Objects == nil {
		return nil, "", apierror.FailedPrecondition("exports and imports go to and from this instance's Cloud Storage: start CloudBurrow with --services storage as well as alloydb")
	}
	if c.GetState() != alloydbpb.Cluster_READY {
		return nil, "", apierror.FailedPrecondition("invalid cluster state %s", c.GetState())
	}
	srv, err := s.primaryServer(name)
	if err != nil {
		return nil, "", err
	}
	return c, srv, nil
}

func (s *Server) ExportCluster(_ context.Context, req *alloydbpb.ExportClusterRequest) (*longrunningpb.Operation, error) {
	if clusterRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}", req.GetName())
	}
	if req.GetDatabase() == "" {
		return nil, apierror.InvalidArgument("database is required")
	}
	dest := req.GetGcsDestination().GetUri()
	if req.GetGcsDestination() == nil {
		return nil, apierror.InvalidArgument("gcs_destination is required")
	}
	bucket, object, err := gcsURI("gcs_destination.uri", dest)
	if err != nil {
		return nil, err
	}
	var run func(ctx context.Context, server string, w io.Writer) error
	switch {
	case req.GetCsvExportOptions() != nil:
		o := req.GetCsvExportOptions()
		query := strings.TrimSpace(o.GetSelectQuery())
		if query == "" {
			return nil, apierror.InvalidArgument("csv_export_options.select_query is required")
		}
		f, err := csvFormat("csv_export_options", o.GetFieldDelimiter(), o.GetQuoteCharacter(), o.GetEscapeCharacter())
		if err != nil {
			return nil, err
		}
		run = func(ctx context.Context, server string, w io.Writer) error {
			return s.opts.Transfers.ExportCSV(ctx, server, req.GetDatabase(), query, f, w)
		}
	case req.GetSqlExportOptions() != nil:
		o := req.GetSqlExportOptions()
		if o.GetIfExistTargetObjects() && !o.GetCleanTargetObjects() {
			return nil, apierror.InvalidArgument("sql_export_options.if_exist_target_objects needs clean_target_objects")
		}
		x := SQLExport{Database: req.GetDatabase(), Tables: o.GetTables(), SchemaOnly: o.GetSchemaOnly(),
			Clean: o.GetCleanTargetObjects(), IfExists: o.GetIfExistTargetObjects()}
		run = func(ctx context.Context, server string, w io.Writer) error {
			return s.opts.Transfers.ExportSQL(ctx, server, x, w)
		}
	default:
		return nil, apierror.InvalidArgument("one of csv_export_options or sql_export_options is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, server, err := s.transferTarget(req.GetName())
	if err != nil {
		return nil, err
	}
	op, err := s.startOp(locationOf(c.GetName()), "export", c.GetName(), "")
	if err != nil {
		return nil, err
	}
	s.background(op.Name, func(ctx context.Context) error {
		var buf bytes.Buffer
		var w io.Writer = &buf
		var gz *gzip.Writer
		if strings.HasSuffix(object, ".gz") {
			gz = gzip.NewWriter(&buf)
			w = gz
		}
		if err := run(ctx, server, w); err != nil {
			return transferError(err)
		}
		if gz != nil {
			if err := gz.Close(); err != nil {
				return err
			}
		}
		return s.opts.Objects.Write(ctx, bucket, object, buf.Bytes())
	}, func(err error) (proto.Message, error) {
		if err != nil {
			return nil, err
		}
		return &alloydbpb.ExportClusterResponse{Destination: &alloydbpb.ExportClusterResponse_GcsDestination{
			GcsDestination: &alloydbpb.GcsDestination{Uri: dest}}}, nil
	})
	return op, nil
}

func (s *Server) ImportCluster(_ context.Context, req *alloydbpb.ImportClusterRequest) (*longrunningpb.Operation, error) {
	if clusterRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}", req.GetName())
	}
	if req.GetGcsUri() == "" {
		return nil, apierror.InvalidArgument("gcs_uri is required")
	}
	bucket, object, err := gcsURI("gcs_uri", req.GetGcsUri())
	if err != nil {
		return nil, err
	}
	database, user := req.GetDatabase(), req.GetUser()
	var run func(ctx context.Context, server string, r io.Reader) error
	switch {
	case req.GetCsvImportOptions() != nil:
		o := req.GetCsvImportOptions()
		if o.GetTable() == "" {
			return nil, apierror.InvalidArgument("csv_import_options.table is required")
		}
		if database == "" {
			return nil, apierror.InvalidArgument("database is required for a CSV import")
		}
		f, err := csvFormat("csv_import_options", o.GetFieldDelimiter(), o.GetQuoteCharacter(), o.GetEscapeCharacter())
		if err != nil {
			return nil, err
		}
		run = func(ctx context.Context, server string, r io.Reader) error {
			return s.opts.Transfers.ImportCSV(ctx, server, database, o.GetTable(), o.GetColumns(), f, user, r)
		}
	case req.GetSqlImportOptions() != nil:
		if database == "" {
			// The file names its database with \connect; psql starts in
			// postgres. UNVERIFIED: the database Google starts in.
			// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/import-sql-file.)
			database = "postgres"
		}
		run = func(ctx context.Context, server string, r io.Reader) error {
			return s.opts.Transfers.ImportSQL(ctx, server, database, user, r)
		}
	default:
		return nil, apierror.InvalidArgument("one of csv_import_options or sql_import_options is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, server, err := s.transferTarget(req.GetName())
	if err != nil {
		return nil, err
	}
	op, err := s.startOp(locationOf(c.GetName()), "import", c.GetName(), "")
	if err != nil {
		return nil, err
	}
	var downloaded int64
	s.background(op.Name, func(ctx context.Context) error {
		data, err := s.opts.Objects.Read(ctx, bucket, object)
		if err != nil {
			return err
		}
		downloaded = int64(len(data))
		var r io.Reader = bytes.NewReader(data)
		if strings.HasSuffix(object, ".gz") {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return apierror.InvalidArgument("gs://%s/%s is not a gzip file: %v", bucket, object, err)
			}
			defer gz.Close()
			r = gz
		}
		return transferError(run(ctx, server, r))
	}, func(err error) (proto.Message, error) {
		if err != nil {
			return nil, err
		}
		return &alloydbpb.ImportClusterResponse{BytesDownloaded: downloaded}, nil
	})
	return op, nil
}

// transferError is a failed export's or import's error: the server's own
// message, as INVALID_ARGUMENT (Google's for a table that does not exist).
func transferError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*apierror.Error); ok {
		return err
	}
	return apierror.InvalidArgument("%v", err)
}
