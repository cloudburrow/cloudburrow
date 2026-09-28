package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// A CSV load whose columns are given, by the load's schema or by the table
// it loads into, and not detected (#931).
//
// BigQuery reads every row of such a file as data unless told otherwise:
// skipLeadingRows is "The number of rows at the top of a CSV file that
// BigQuery will skip when loading the data. The default value is 0."
// Each value goes to the column at its position in the schema.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad
//
// The emulator ignores skipLeadingRows and always takes the first row of
// the file as a header (measured against the pinned image, and in its
// source, server/handler.go uploadContentHandler.Handle): a file of three
// rows with a schema and no header loaded two, the first one lost without
// an error; a file of one row loaded none; with skipLeadingRows 2 the load
// failed; and when the first row's values are all names of the table's
// columns, it maps the columns by those names instead of by position.
//
// So for a load whose data the front passes on (a multipart upload, or a
// resumable one the front sends as one, resumable), the front drops the
// rows BigQuery skips and puts a header of its own in front of the rest:
// the columns' names in the schema's order, which the emulator then takes
// as the header and maps position for position, as BigQuery does. Every
// row that remains is loaded. The data is passed on as a stream, not read
// into memory.
//
// A load from Cloud Storage (sourceUris) is read by the emulator itself, so
// the front cannot change its data: unless skipLeadingRows is 1, which the
// emulator's header then stands for, it is 501 rather than a load that
// loses the first row of each file.
//
// A load with autodetect is not changed: with no schema, it is checked in
// the table it made (autodetectLoad), and whether BigQuery takes its first
// row as a header depends on the data.

// csvLoad makes a CSV load with its columns given load as BigQuery does
// (see above), and reports whether r is still to be sent; false means the
// load has been answered.
func (f front) csvLoad(w http.ResponseWriter, r *http.Request, job jobBody) bool {
	l := job.Configuration.Load
	skip, _ := skipLeadingRows(l.SkipLeadingRows)
	if skip < 0 {
		return true
	}
	header := f.loadColumns(r, l.Schema, l.DestinationTable)
	if len(header) == 0 {
		// No columns to name: the emulator's own answer stands.
		return true
	}
	if len(l.SourceURIs) > 0 {
		if skip == 1 {
			return true
		}
		writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: a CSV load from "+
			"Cloud Storage with a schema (or into an existing table) and skipLeadingRows %d. BigQuery skips %d rows of "+
			"each file and loads the rest, but the emulator behind CloudBurrow always takes the first row of each file "+
			"as a header and drops it (measured), so the load would lose rows. Nothing was loaded. Load the file from "+
			"the client (a multipart or resumable upload, which CloudBurrow loads as BigQuery does), or give the "+
			"files a header row and skipLeadingRows 1.", skip, skip))
		return false
	}
	if err := rewriteMedia(r, func(data io.Reader) io.Reader { return csvData(data, header, skip) }); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
		return false
	}
	return true
}

// loadColumns returns the top-level column names a CSV load's values go
// to, in order: its schema's, or when it gives none, those of the table
// it loads into; or none.
func (f front) loadColumns(r *http.Request, schema *tableSchema, dest *tableRef) []string {
	if schema == nil || len(schema.Fields) == 0 {
		if dest == nil || dest.DatasetID == "" || dest.TableID == "" {
			return nil
		}
		status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
		var meta struct {
			Schema *tableSchema `json:"schema"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || meta.Schema == nil {
			return nil
		}
		schema = meta.Schema
	}
	names := make([]string, 0, len(schema.Fields))
	for _, fl := range schema.Fields {
		names = append(names, fl.Name)
	}
	return names
}

// csvData returns data without its first skip records, after a header
// row of names.
func csvData(data io.Reader, names []string, skip int64) io.Reader {
	var head bytes.Buffer
	cw := csv.NewWriter(&head)
	_ = cw.Write(names)
	cw.Flush()
	return io.MultiReader(&head, &skipReader{r: bufio.NewReader(data), skip: skip})
}

// skipReader reads r after its first skip CSV records. A record ends at a
// newline outside a quoted field; an empty line is not a record, as
// encoding/csv, which the emulator reads the data with, ignores it.
type skipReader struct {
	r       *bufio.Reader
	skip    int64
	skipped bool
}

func (s *skipReader) Read(p []byte) (int, error) {
	if !s.skipped {
		s.skipped = true
		if err := skipRecords(s.r, s.skip); err != nil {
			return 0, err
		}
	}
	return s.r.Read(p)
}

// skipRecords reads n CSV records from r. The end of the data ends the
// last one.
func skipRecords(r *bufio.Reader, n int64) error {
	quoted, content := false, false
	for n > 0 {
		b, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case b == '"':
			quoted, content = !quoted, true
		case b == '\n' && !quoted:
			if content {
				n--
			}
			content = false
		case b == '\r' && !quoted:
		default:
			content = true
		}
	}
	return nil
}

// rewriteMedia replaces the data of r, a multipart upload of a job and its
// data, by transform of it. The job, the first part, is kept as it is; the
// data, the second, is passed through transform as it is read, so the new
// body is a stream of unknown length. A body that is not a multipart one
// is left alone. An error means the multipart body could not be read, and
// r's body is then spent.
func rewriteMedia(r *http.Request, transform func(io.Reader) io.Reader) error {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" || r.Header.Get("Content-Encoding") != "" {
		return nil
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	meta, err := mr.NextPart()
	if err != nil {
		return err
	}
	job, err := io.ReadAll(io.LimitReader(meta, maxJobPart+1))
	if err != nil {
		return err
	}
	if len(job) > maxJobPart {
		return errors.New("the job's part is too large")
	}
	media, err := mr.NextPart()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			p, err := mw.CreatePart(meta.Header)
			if err != nil {
				return err
			}
			if _, err := p.Write(job); err != nil {
				return err
			}
			if p, err = mw.CreatePart(media.Header); err != nil {
				return err
			}
			if _, err := io.Copy(p, transform(media)); err != nil {
				return err
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	params["boundary"] = mw.Boundary()
	r.Header.Set("Content-Type", mime.FormatMediaType(mt, params))
	r.Body = pr
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	return nil
}
