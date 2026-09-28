package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// A NEWLINE_DELIMITED_JSON load into BYTES columns (#1065, #1075).
//
// The emulator CloudBurrow builds decodes a JSON load's base64 BYTES values
// itself (#1061; the pinned one stored the base64 text, and a NaN as NULL,
// which it now keeps too: storedvalues.go). The front still reads the
// records of a JSON load whose columns have BYTES (at any depth) as they
// pass through it, to check each value as BigQuery does: one in the
// URL-safe alphabet is sent in the standard one, and one that is not
// base64 fails the load before the emulator has loaded anything: the stream is cut, the
// emulator fails the load (it reads the whole of the data before it writes
// a row: its source, uploadContentHandler.Handle), and the front answers
// with why (reportFailure). A record that is not one JSON object on its
// line (BigQuery reads one per line) fails the load as invalid. The rest
// of each record is passed on as it was, and a record with no BYTES value
// is passed on unchanged.
//
// The columns are those the emulator loads the values into: the table's,
// when it exists, and otherwise the load's schema.
//
// A load from Cloud Storage is read by the front, as a CSV load is
// (gcsload.go), and sent to the emulator as an upload of the same job
// with the objects' records in order, so the job is one load, into one
// transaction. With no Cloud Storage given to the front (--storage), such
// a load is 501.

// jsonLoad reads a JSON load's data when its columns need it (above). It
// returns what csvLoad returns: the request to send on, the handler to
// send it through, what it counts of the data, and ok false when the load
// has been answered.
func (f front) jsonLoad(w http.ResponseWriter, r *http.Request, job jobBody, next http.Handler) (*http.Request, http.Handler, *dataFailure, bool) {
	l := job.Configuration.Load
	fields := f.jsonColumns(r, l.Schema, l.DestinationTable)
	if !storedValues(fields) {
		return r, next, nil, true
	}
	fail := &dataFailure{}
	out := r
	if len(l.SourceURIs) > 0 {
		if f.storage == nil {
			// The emulator decodes BYTES itself (#1061); the front reads
			// the records only to refuse one that is not base64 as
			// BigQuery does, which the emulator then refuses in its own
			// words instead.
			return r, next, nil, true
		}
		objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return r, next, nil, false
		}
		srcs := make([]func() (io.ReadCloser, error), len(objs))
		locs := make([]string, len(objs))
		for i, o := range objs {
			srcs[i] = func() (io.ReadCloser, error) { return f.storage.open(r.Context(), o) }
			locs[i] = o.uri()
		}
		body, err := readBody(r)
		if err == nil {
			out, err = f.gcsUpload(r, body, jsonStream(srcs, locs, fields, fail))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load job could not be read: "+err.Error())
			return r, next, nil, false
		}
	} else {
		err := rewriteMedia(r, func(data io.Reader) io.ReadCloser {
			return jsonStream([]func() (io.ReadCloser, error){func() (io.ReadCloser, error) { return io.NopCloser(data), nil }},
				nil, fields, fail)
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return r, next, nil, false
		}
	}
	return out, f.reportFailure(out, next, job, fail, false), fail, true
}

// jsonColumns returns the columns a JSON load's values go to: the
// destination table's, when it exists, as the emulator loads into them
// (its source: uploadContentHandler.Handle reads the table's schema), and
// otherwise the load's schema.
func (f front) jsonColumns(r *http.Request, schema *tableSchema, dest *tableRef) []field {
	if dest != nil && dest.DatasetID != "" && dest.TableID != "" && (dest.ProjectID == "" || dest.ProjectID == projectOf(f.base)) {
		status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
		var meta struct {
			Schema *tableSchema `json:"schema"`
		}
		if status == http.StatusOK && json.Unmarshal(got, &meta) == nil && meta.Schema != nil {
			return meta.Schema.Fields
		}
	}
	if schema == nil {
		return nil
	}
	return schema.Fields
}

// jsonStream returns the records of srcs, each opened in turn when the
// stream reaches it, with their values changed by fixValues, one record a
// line. It starts when first read, and ends when closed. A loadDataError
// it ends with is also kept in fail; when it ends without one, fail keeps
// the sources and their bytes (the rows are counted in the table,
// countLoad). locs are the sources' gs:// URIs, or nil for an upload.
func jsonStream(srcs []func() (io.ReadCloser, error), locs []string, fields []field, fail *dataFailure) io.ReadCloser {
	return lazyPipe(func(pw io.Writer) error {
		var inBytes int64
		for i, open := range srcs {
			src, err := open()
			if err != nil {
				fail.set(asLoadDataError(err))
				return err
			}
			loc := ""
			if i < len(locs) {
				loc = locs[i]
			}
			rc := &countingReader{ReadCloser: src, n: &inBytes}
			err = jsonRecords(pw, bufio.NewReader(rc), fields, loc)
			rc.Close()
			if err != nil {
				var le *loadDataError
				if errors.As(err, &le) {
					fail.set(le)
				}
				return err
			}
		}
		fail.setCounts(loadCounts{inputFiles: int64(len(srcs)), inputFileBytes: inBytes, noRows: true, noBad: true})
		return nil
	})
}

// jsonRecords writes the records of br to w, one a line, changed by
// fixValues. loc is the file's URI, or "" for an upload.
func jsonRecords(w io.Writer, br *bufio.Reader, fields []field, loc string) error {
	in := "the load's data"
	if loc != "" {
		in = loc
	}
	for line := 1; ; line++ {
		text, err := br.ReadBytes('\n')
		if len(text) > 0 {
			if werr := jsonRecord(w, text, fields, fmt.Sprintf("line %d of %s", line, in)); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// jsonRecord writes one line of a JSON load's data, where names it.
func jsonRecord(w io.Writer, text []byte, fields []field, where string) error {
	if len(bytes.TrimSpace(text)) == 0 {
		_, err := w.Write(text)
		return err
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	err := dec.Decode(&obj)
	if err == nil && dec.More() {
		err = errors.New("more than one JSON value on the line")
	}
	if err != nil || obj == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return &loadDataError{code: http.StatusBadRequest, reason: "invalid", msg: fmt.Sprintf(
			"Error while reading data, error message: JSON parsing error in %s: %v. A NEWLINE_DELIMITED_JSON "+
				"file holds one JSON object on each line. Nothing was loaded.", where, err)}
	}
	changed, p := fixValues(fields, obj, "a load's record, "+where, "")
	if p != nil {
		return p.loadError()
	}
	if !changed {
		_, err := w.Write(text)
		if err == nil && text[len(text)-1] != '\n' {
			_, err = io.WriteString(w, "\n")
		}
		return err
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}
