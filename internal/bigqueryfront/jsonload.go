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
	"strings"
	"time"
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
// transaction: since #1079 whatever its columns, as the emulator loads
// each object it reads in a transaction of its own. With no Cloud Storage
// given to the front (--storage), such a load into BYTES or FLOAT64
// columns is 501, and so is any of several objects.

// jsonLoad reads a JSON load's data when its columns need it (above). It
// returns what csvLoad returns: the request to send on, the handler to
// send it through, what it counts of the data, and ok false when the load
// has been answered.
func (f front) jsonLoad(w http.ResponseWriter, r *http.Request, job jobBody, next http.Handler) (*http.Request, http.Handler, *dataFailure, bool) {
	l := job.Configuration.Load
	fields := f.jsonColumns(r, l.Schema, l.DestinationTable)
	stamps := l.detected && hasTimestamp(fields)                                                                // #1324, jsondetect.go
	opts := jsonReadOptions{fields: fields, stamps: stamps, unknown: len(fields) > 0 && !l.IgnoreUnknownValues} // #1339
	opts.known = fields
	if l.Schema != nil {
		// A field the load's schema adds to the table's is not unknown.
		opts.known = knownFields(fields, l.Schema.Fields)
	}
	if n, set := skipLeadingRows(l.MaxBadRecords); set {
		if n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid maxBadRecords %d: it cannot be negative.", n))
			return r, next, nil, false
		}
		opts.maxBad = n
	}
	read := storedValues(fields) || stamps || opts.unknown || opts.maxBad > 0 && len(fields) > 0 || // #1457: badJSONValue
		hasJSONColumn(fields) // #1457: jsonColumnValues
	if !read && (len(l.SourceURIs) == 0 || f.storage == nil && !severalObjects(l.SourceURIs)) {
		// #1079: a load from Cloud Storage is read here, as one.
		return r, next, nil, true
	}
	fail := &dataFailure{}
	out := r
	if len(l.SourceURIs) > 0 {
		if f.storage == nil && !storedValues(fields) && !stamps && severalObjects(l.SourceURIs) {
			writeError(w, http.StatusNotImplemented, "notImplemented", severalObjectsMsg)
			return r, next, nil, false
		}
		if f.storage == nil {
			// The emulator decodes BYTES itself (#1061); the front reads
			// the records only to refuse one that is not base64 as
			// BigQuery does, which the emulator then refuses in its own
			// words instead. A field outside the columns is then dropped
			// by the emulator (#1339): with no Cloud Storage given to the
			// front, the emulator has none to read the objects from
			// either.
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
			out, err = f.gcsUpload(r, body, jsonStream(srcs, locs, opts, fail))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load job could not be read: "+err.Error())
			return r, next, nil, false
		}
	} else {
		err := rewriteMedia(r, func(data io.Reader) io.ReadCloser {
			return jsonStream([]func() (io.ReadCloser, error){func() (io.ReadCloser, error) { return io.NopCloser(data), nil }},
				nil, opts, fail)
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return r, next, nil, false
		}
	}
	return out, f.reportFailure(out, next, job, fail, opts.maxBad > 0), fail, true
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
// stream reaches it, read by o (jsonRecord), one record a line. It starts
// when first read, and ends when closed. A loadDataError it ends with is
// also kept in fail; when it ends without one, fail keeps the sources and
// their bytes, and with o.unknown the bad records left out (the rows are
// counted in the table, countLoad). locs are the sources' gs:// URIs, or
// nil for an upload.
func jsonStream(srcs []func() (io.ReadCloser, error), locs []string, o jsonReadOptions, fail *dataFailure) io.ReadCloser {
	return lazyPipe(func(pw io.Writer) error {
		var inBytes int64
		st := &jsonState{}
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
			err = jsonRecords(pw, bufio.NewReader(rc), o, st, loc)
			rc.Close()
			if err != nil {
				var le *loadDataError
				if errors.As(err, &le) {
					fail.set(le)
				}
				return err
			}
		}
		fail.setCounts(loadCounts{inputFiles: int64(len(srcs)), inputFileBytes: inBytes, noRows: true, noBad: !o.unknown,
			badRecords: st.bad, errors: st.errs})
		return nil
	})
}

// jsonReadOptions are how a JSON load's records are read: the columns
// they go to, whether a detected TIMESTAMP is rewritten (stamps), whether
// a field outside the columns makes the record bad (unknown, #1339), and
// how many bad records are left out (maxBadRecords).
type jsonReadOptions struct {
	fields, known   []field // known: the fields a record may have
	stamps, unknown bool
	maxBad          int64
}

// jsonState is what reading a load's records keeps from one file to the
// next: the bad records left out, and the first maxListedBadRecords of
// them as errors.
type jsonState struct {
	bad  int64
	errs []rowError
}

// jsonRecords writes the records of br to w, one a line, read by o. loc is
// the file's URI, or "" for an upload.
func jsonRecords(w io.Writer, br *bufio.Reader, o jsonReadOptions, st *jsonState, loc string) error {
	in := "the load's data"
	if loc != "" {
		in = loc
	}
	for line := 1; ; line++ {
		text, err := br.ReadBytes('\n')
		if len(text) > 0 {
			if werr := jsonRecord(w, text, o, st, loc, fmt.Sprintf("line %d of %s", line, in)); werr != nil {
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

// jsonRecord writes one line of a JSON load's data, where names it and loc
// is its file's URI. With o.stamps, each TIMESTAMP value is written as the
// emulator reads it (#1324, detectedTimestamps). With o.unknown, a record
// with a field outside the columns is bad (#1339, unknownField): it is
// left out, up to o.maxBad of them, and one more fails the load.
func jsonRecord(w io.Writer, text []byte, o jsonReadOptions, st *jsonState, loc, where string) error {
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
	if o.unknown {
		name, isNull := unknownField(o.known, obj, "")
		if isNull {
			return &loadDataError{code: http.StatusNotImplemented, reason: "notImplemented", msg: fmt.Sprintf(
				"Not implemented here: a NEWLINE_DELIMITED_JSON record (%s) whose field %s, which is not in the "+
					"schema, is null. BigQuery treats a record with a field outside the schema as bad unless "+
					"ignoreUnknownValues is set, but does not document whether a null one counts. Nothing was loaded. "+
					"Set ignoreUnknownValues, or leave the field out.", where, name)}
		}
		if name != "" {
			return st.badRecord(o, loc, fmt.Sprintf("JSON parsing error in %s: No such field: %s. "+
				"ignoreUnknownValues is false, so the record is bad", where, name))
		}
	}
	boolsChanged := false
	if o.maxBad > 0 {
		var bad string
		if bad, boolsChanged = badJSONValue(o.fields, obj); bad != "" {
			return st.badRecord(o, loc, fmt.Sprintf("JSON parsing error in %s: %s", where, bad))
		}
	}
	changed, p := fixValues(o.fields, obj, "a load's record, "+where, "")
	changed = changed || boolsChanged
	if jsonColumnValues(o.fields, obj) { // #1457
		changed = true
	}
	if p != nil {
		return p.loadError()
	}
	if infinityRecords(o.fields, obj) { // #1077, infinity.go
		changed = true
	}
	if o.stamps && detectedTimestamps(o.fields, obj) {
		changed = true
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

// badRecord counts a bad record, why it is bad, of the file at loc: it
// is left out, or, one more than o.maxBad, fails the load.
func (st *jsonState) badRecord(o jsonReadOptions, loc, why string) error {
	st.bad++
	if st.bad > o.maxBad {
		return &loadDataError{code: http.StatusBadRequest, reason: "invalid", msg: fmt.Sprintf(
			"Error while reading data, error message: JSON table encountered too many errors, giving up. "+
				"%s (%d bad records, and maxBadRecords is %d). Nothing was loaded.", why, st.bad, o.maxBad)}
	}
	if len(st.errs) < maxListedBadRecords {
		st.errs = append(st.errs, rowError{Reason: "invalid", Location: loc, Message: fmt.Sprintf(
			"Error while reading data, error message: %s. It was left out (%d of maxBadRecords %d).",
			why, st.bad, o.maxBad)})
	}
	return nil
}

// badJSONValue returns why a record of a JSON load with maxBadRecords is
// bad for a top-level value its column refuses, or "", and whether it
// rewrote a BOOL (#1457). "Number of errors allowed" is "the maximum
// number of rows containing errors that can be ignored", and the JSON
// loading page's "Data types" says what each type takes: a BOOL "1 or 0,
// true or false, t or f, yes or no, or y or n (all case insensitive)",
// written as a JSON boolean for the emulator; a DATE "must be in the
// format YYYY-MM-DD"; a TIME "HH:MM:SS[.SSSSSS]"; and a repeated field
// "must be a JSON array or null ... The array itself cannot contain null
// values". An object or array for a column that is neither a RECORD nor
// REPEATED, and an INTEGER, FLOAT, NUMERIC or BIGNUMERIC that is no
// number, are bad too. A value it passes that the emulator still refuses
// leaves the load 501, as before (reportFailure).
func badJSONValue(fields []field, obj map[string]any) (string, bool) {
	changed := false
	for k, v := range obj {
		var f *field
		for i := range fields {
			if strings.EqualFold(fields[i].Name, k) {
				f = &fields[i]
				break
			}
		}
		if f == nil || v == nil {
			continue
		}
		typ := strings.ToUpper(f.Type)
		if typ == "RECORD" || typ == "STRUCT" || typ == "JSON" || typ == "GEOGRAPHY" || typ == "RANGE" {
			continue
		}
		values := []any{v}
		if strings.EqualFold(f.Mode, "REPEATED") {
			arr, ok := v.([]any)
			if !ok {
				return fmt.Sprintf("the REPEATED field %s is not a JSON array", k), changed
			}
			values = arr
		}
		for i, e := range values {
			why := ""
			switch e := e.(type) {
			case nil:
				why = "null inside an array, which \"cannot contain null values\""
			case map[string]any, []any:
				why = "a JSON object or array"
			default:
				text := fmt.Sprint(e)
				switch typ {
				case "BOOL", "BOOLEAN":
					var b bool
					switch strings.ToLower(text) {
					case "1", "true", "t", "yes", "y":
						b = true
					case "0", "false", "f", "no", "n":
					default:
						why = "not a boolean (1 or 0, true or false, t or f, yes or no, y or n)"
					}
					if why == "" {
						if _, isBool := e.(bool); !isBool {
							changed = true
							if strings.EqualFold(f.Mode, "REPEATED") {
								values[i] = b
							} else {
								obj[k] = b
							}
						}
					}
				case "INTEGER", "INT64", "FLOAT", "FLOAT64", "NUMERIC", "BIGNUMERIC":
					if _, isBool := e.(bool); isBool || !looksNumeric(text) {
						why = "not a number"
					}
				case "DATE":
					if _, err := time.Parse("2006-01-02", text); err != nil || !csvDate.MatchString(text) {
						why = "not a date in the format YYYY-MM-DD"
					}
				case "TIME":
					if !validTime(csvTime.FindStringSubmatch(text)) {
						why = "not a time in the format HH:MM:SS[.SSSSSS]"
					}
				}
			}
			if why != "" {
				return fmt.Sprintf("the value of the %s field %s is %s", typ, k, why), changed
			}
		}
	}
	return "", changed
}

// hasJSONColumn reports whether fields hold a JSON column, at any depth.
func hasJSONColumn(fields []field) bool {
	for _, f := range fields {
		switch strings.ToUpper(f.Type) {
		case "JSON":
			return true
		case "RECORD", "STRUCT":
			if hasJSONColumn(f.Fields) {
				return true
			}
		}
	}
	return false
}

// jsonColumnValues writes each value of a JSON column in obj, at any
// depth, as its JSON text, and reports whether it wrote one (#1457). A
// JSON column of a NEWLINE_DELIMITED_JSON load holds the field's value
// whatever its type: in the JSON loading page's example, "address" is a
// STRING, a STRUCT or an ARRAY, and "is loaded into a column with type
// JSON that allows it to hold the mixed types". The emulator reads a JSON
// column's value as JSON text, and failed a string that is not (measured:
// "123 First Avenue", 400 "invalid character 'F' after top-level value").
func jsonColumnValues(fields []field, obj map[string]any) bool {
	changed := false
	for k, v := range obj {
		if v == nil {
			continue
		}
		var f *field
		for i := range fields {
			if strings.EqualFold(fields[i].Name, k) {
				f = &fields[i]
				break
			}
		}
		if f == nil {
			continue
		}
		repeated := strings.EqualFold(f.Mode, "REPEATED")
		switch strings.ToUpper(f.Type) {
		case "JSON":
			text := func(e any) any {
				if e == nil {
					return nil
				}
				b, err := json.Marshal(e)
				if err != nil {
					return e
				}
				return string(b)
			}
			if arr, ok := v.([]any); ok && repeated {
				for i, e := range arr {
					arr[i] = text(e)
				}
			} else {
				obj[k] = text(v)
			}
			changed = true
		case "RECORD", "STRUCT":
			if !hasJSONColumn(f.Fields) {
				continue
			}
			recs := []any{v}
			if arr, ok := v.([]any); ok && repeated {
				recs = arr
			}
			for _, r := range recs {
				if m, ok := r.(map[string]any); ok && jsonColumnValues(f.Fields, m) {
					changed = true
				}
			}
		}
	}
	return changed
}
