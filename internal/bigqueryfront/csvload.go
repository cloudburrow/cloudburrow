package bigqueryfront

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
// A load from Cloud Storage (sourceUris) is read by the front, which sends
// it on as an upload of the same job (#944, gcsload.go), so it is loaded
// the same way, each file's leading rows skipped. With the front given no
// Cloud Storage to read (--storage), it is 501 unless skipLeadingRows is 1,
// as it was before.
//
// A load with autodetect and no columns given is not changed, but for its
// CSV options (csvDialect): with no schema, it is checked in the table it
// made (autodetectLoad). With autodetect and columns given, by a schema or
// the table it loads into, BigQuery reads skipLeadingRows so: "0 -
// Instructs autodetect that there are no headers and data should be read
// starting from the first row"; "N > 0 - Autodetect skips N-1 rows and
// tries to detect headers in row N. If headers are not detected, row N is
// just skipped"; either way the first N rows are not loaded, which the
// front does as for any load with columns. Unset, "Autodetect tries to
// detect headers in the first row. If they are not detected, the row is
// read as data": the emulator drops it whatever it holds (measured, #945),
// and the front does not guess BigQuery's detection against a schema, so
// that is 501. The Go client does not send a skipLeadingRows of 0.

// csvLoad makes a CSV load load as BigQuery does (see above). It returns
// the request to send on, which is r or, for a load from Cloud Storage, an
// upload of it, the handler to send it through, which is next or one that
// answers with the failure the data's stream ended with, and, when the
// front reads the data, what it counts of it (countLoad); ok false means
// the load has been answered.
func (f front) csvLoad(w http.ResponseWriter, r *http.Request, job jobBody, next http.Handler) (*http.Request, http.Handler, *dataFailure, bool) {
	l := job.Configuration.Load
	skip, skipSet := skipLeadingRows(l.SkipLeadingRows)
	if skip < 0 {
		return r, next, nil, true
	}
	d, why := dialectOf(l.FieldDelimiter, l.Quote, l.AllowJaggedRows, l.NullMarker)
	if why != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load with "+why+". Nothing was loaded.")
		return r, next, nil, false
	}
	var opts struct {
		Configuration struct {
			Load csvOptions `json:"load"`
		} `json:"configuration"`
	}
	_ = decodeJob(r, &opts)
	cols := f.loadColumns(r, l.Schema, l.DestinationTable)
	d, code, reason, msg := d.withOptions(opts.Configuration.Load, l.NullMarker != nil, cols, skip) // #952
	if code != 0 {
		writeError(w, code, reason, msg)
		return r, next, nil, false
	}
	if l.Autodetect && len(cols) > 0 && !skipSet {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load with autodetect, "+
			"columns given (by its schema, or by the table it loads into) and no skipLeadingRows. BigQuery then decides "+
			"from the data whether the first row is a header, but the emulator behind CloudBurrow always drops the first "+
			"row (measured), and CloudBurrow does not guess BigQuery's decision. Nothing was loaded. Set skipLeadingRows "+
			"to 1 for a file with a header, or leave autodetect off for one without.")
		return r, next, nil, false
	}
	skipFirst, skipRest := skip, skip
	switch {
	case len(cols) > 0:
	case l.Autodetect:
		// The emulator takes the first row as the header; BigQuery looks
		// for one in each file, and the front drops each later file's.
		cols, skipFirst, skipRest = nil, 0, 1
	default:
		// No columns to name: the emulator's own answer stands.
		return r, next, nil, true
	}
	fail := &dataFailure{}
	out := r
	if len(l.SourceURIs) > 0 {
		if f.storage == nil {
			if skip == 1 && !d.optionsSet() || len(cols) == 0 && !d.optionsSet() {
				return r, next, nil, true
			}
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load from Cloud "+
				"Storage that the emulator behind CloudBurrow would not load as BigQuery does (it takes the first row of "+
				"each file as a header and ignores the load's CSV options), with no Cloud Storage for CloudBurrow to read "+
				"the files from. Nothing was loaded.")
			return r, next, nil, false
		}
		objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return r, next, nil, false
		}
		srcs := make([]func() (io.ReadCloser, error), len(objs))
		for i, o := range objs {
			srcs[i] = func() (io.ReadCloser, error) { return f.storage.open(r.Context(), o) }
		}
		body, err := readBody(r)
		if err == nil {
			locs := make([]string, len(objs))
			for i, o := range objs {
				locs[i] = o.uri()
			}
			out, err = f.gcsUpload(r, body, csvStream(srcs, locs, d, cols, skipFirst, skipRest, fail))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load job could not be read: "+err.Error())
			return r, next, nil, false
		}
	} else {
		if len(cols) == 0 && d.plain() {
			return r, next, nil, true
		}
		err := rewriteMedia(r, func(data io.Reader) io.ReadCloser {
			return csvStream([]func() (io.ReadCloser, error){func() (io.ReadCloser, error) { return io.NopCloser(data), nil }},
				nil, d, cols, skipFirst, skipRest, fail)
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return r, next, nil, false
		}
	}
	return out, f.reportFailure(out, next, job, fail, d.maxBad > 0), fail, true
}

// reportFailure returns next, answering the load sent as req with the
// failure its data's stream ended with, if it ended with one, in place of
// the emulator's answer: the stream was cut, so the emulator failed the
// load without loading anything. jobs.get then reports the job with that
// failure, as the emulator may have recorded it. With skipsBad (a load
// with maxBadRecords), a load the emulator failed is answered 501: the
// record it failed on may be one BigQuery would have left out (#952).
func (f front) reportFailure(req *http.Request, next http.Handler, job jobBody, fail *dataFailure, skipsBad bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != req {
			next.ServeHTTP(w, r)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		e := fail.get()
		if e == nil && skipsBad {
			var got map[string]any
			_ = json.Unmarshal(rec.body.Bytes(), &got)
			if msg, failed := queryFailure(rec, got); failed {
				e = &loadDataError{code: http.StatusNotImplemented, reason: "notImplemented", msg: "Not implemented here: " +
					"a CSV load with maxBadRecords that the emulator behind CloudBurrow failed (" + msg + "). BigQuery " +
					"leaves out up to maxBadRecords bad records, and the one the emulator failed on may be one of them, " +
					"but the emulator fails a load on its first bad value (measured). CloudBurrow leaves out the records " +
					"it can tell are bad (the wrong number of values, or no value for a REQUIRED column), not a value the " +
					"column's type refuses. Nothing was loaded."}
			}
		}
		if e == nil {
			rec.copyTo(w) // its counts are reported by countLoad (#960, #966)
			return
		}
		project := job.JobReference.ProjectID
		if project == "" {
			project = projectOf(f.base)
		}
		f.failed.add(project, job.JobReference.JobID, rowError{Reason: e.reason, Message: e.msg})
		writeError(w, e.code, e.reason, e.msg)
	})
}

// asLoadDataError returns err as a loadDataError, a 500 for one that is
// not.
func asLoadDataError(err error) *loadDataError {
	var e *loadDataError
	if errors.As(err, &e) {
		return e
	}
	return &loadDataError{code: http.StatusInternalServerError, reason: "internalError", msg: "cloudburrow: " + err.Error()}
}

// loadColumns returns the top-level columns a CSV load's values go to, in
// order: its schema's, or when it gives none, those of the table it loads
// into; or none.
func (f front) loadColumns(r *http.Request, schema *tableSchema, dest *tableRef) []field {
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
	return schema.Fields
}

// csvStream returns the data of srcs, each opened in turn when the stream
// reaches it, written as the emulator reads CSV: a header row of cols'
// names when cols are given, then each source's records without its
// first skipFirst (the first source) or skipRest (the others), read by d.
// With d plain, the records are passed on as they are. The stream starts
// when it is first read, and ends when it is closed. A loadDataError it
// ends with is also kept in fail; when it ends without one, fail keeps
// what it counted (#960): with d plain, only the sources and their bytes
// (#966). locs are the
// sources' gs:// URIs, or nil for an upload.
func csvStream(srcs []func() (io.ReadCloser, error), locs []string, d csvDialect, cols []field, skipFirst, skipRest int64, fail *dataFailure) io.ReadCloser {
	return lazyPipe(func(pw io.Writer) error {
		cw := csv.NewWriter(pw)
		if len(cols) > 0 {
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = c.Name
			}
			_ = cw.Write(names)
			cw.Flush()
			if err := cw.Error(); err != nil {
				return err
			}
		}
		st := &csvState{width: len(cols)}
		var inBytes int64
		for i, open := range srcs {
			skip := skipFirst
			if i > 0 {
				skip = skipRest
			}
			if i < len(locs) {
				st.loc = locs[i]
			}
			src, err := open()
			if err != nil {
				fail.set(asLoadDataError(err))
				return err
			}
			rc := &countingReader{ReadCloser: src, n: &inBytes}
			if d.plain() {
				if i > 0 {
					if _, err := io.WriteString(pw, "\n"); err != nil {
						rc.Close()
						return err
					}
				}
				_, err = io.Copy(pw, &skipReader{r: bufio.NewReader(rc), skip: skip})
			} else {
				err = dialectRecords(cw, rc, d, cols, skip, st)
				cw.Flush()
				if err == nil {
					err = cw.Error()
				}
			}
			rc.Close()
			if err != nil {
				var le *loadDataError
				if errors.As(err, &le) {
					fail.set(le)
				}
				return err
			}
		}
		if d.plain() {
			// The records were passed on unread: the rows are counted in
			// the table (countLoad, #966).
			fail.setCounts(loadCounts{inputFiles: int64(len(srcs)), inputFileBytes: inBytes, noRows: true, noBad: true})
		} else {
			rows := st.rows
			if len(cols) == 0 && rows > 0 {
				rows-- // the first record is the header the emulator reads the columns from
			}
			fail.setCounts(loadCounts{badRecords: st.bad, outputRows: rows, inputFiles: int64(len(srcs)),
				inputFileBytes: inBytes, errors: st.errs})
		}
		return nil
	})
}

// countingReader adds the bytes read through it to n.
type countingReader struct {
	io.ReadCloser
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.ReadCloser.Read(p)
	*c.n += int64(k)
	return k, err
}

// lazyPipe returns a reader of what write writes, run when it is first
// read. Closing it ends write, whose writes then fail.
func lazyPipe(write func(io.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	return &lazyReader{pr: pr, start: func() {
		go func() { _ = pw.CloseWithError(write(pw)) }()
	}}
}

type lazyReader struct {
	once  sync.Once
	pr    *io.PipeReader
	start func()
}

func (l *lazyReader) Read(p []byte) (int, error) {
	l.once.Do(l.start)
	return l.pr.Read(p)
}

func (l *lazyReader) Close() error {
	return l.pr.Close()
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
func rewriteMedia(r *http.Request, transform func(io.Reader) io.ReadCloser) error {
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
	boundary := multipart.NewWriter(io.Discard).Boundary()
	body := lazyPipe(func(pw io.Writer) error {
		mw := multipart.NewWriter(pw)
		if err := mw.SetBoundary(boundary); err != nil {
			return err
		}
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
		data := transform(media)
		defer data.Close()
		if _, err := io.Copy(p, data); err != nil {
			return err
		}
		return mw.Close()
	})
	params["boundary"] = boundary
	r.Header.Set("Content-Type", mime.FormatMediaType(mt, params))
	r.Body = body
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	return nil
}
