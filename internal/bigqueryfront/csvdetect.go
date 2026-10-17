package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A CSV load with autodetect, no schema, into a new table, and a
// timeZone, dateFormat, datetimeFormat, timeFormat or timestampFormat
// (#1338).
//
// BigQuery's detection reads the formats: "Schema autodetection will also
// decide DATE column type based on this format instead of the existing
// format", and "If this value is present, this format is the only
// compatible DATE format" (loading-data-cloud-storage-csv, the same for
// DATETIME, TIME and TIMESTAMP); timeZone applies to "timestamp values
// that have no specific time zone". The emulator detects a CSV's columns
// by its own rules and reads none of these options (measured, #1325). So
// for such a load the front detects the schema itself, as it does for a
// JSON load (jsondetect.go), from the first 500 records after the header
// of the first file ("scans up to the first 500 rows", schema-detect):
//
//   - a value its type's format parses is of that type; with a format set,
//     a value in the type's default form is not (it is "the only
//     compatible" one), and is a STRING;
//   - any other value is typed as a JSON string is (stringKind): BOOLEAN,
//     INTEGER, FLOAT, DATE, TIME, TIMESTAMP or STRING; an empty value is
//     NULL and says nothing;
//   - a column's values resolve as a JSON field's do: INTEGER with FLOAT
//     is FLOAT, a STRING with others is STRING, a column only ever empty is
//     STRING, every column NULLABLE, and any other mix is 501.
//
// The first row is the header, its values the names, when each is a
// STRING and some column below it is not, BigQuery's rule (headerDiffers);
// otherwise, as BigQuery would load the row as data and name the columns
// itself, the load is 501. The names are held to columnNameCharacterMap as
// a JSON load's are. The load is then sent with the detected schema and
// skipLeadingRows 1, and read as any CSV load with columns (csvLoad), its
// values converted by the formats and timeZone (csvtimes.go).
//
// Only the default dialect is detected: a fieldDelimiter of one character,
// the double-quote quote, and UTF-8; another, records of different widths
// and a quoted value across lines in the sample are 501.
//
// skipLeadingRows and the header (#1457). BigQuery documents both: "When
// autodetect is on, the behavior is the following: skipLeadingRows
// unspecified - Autodetect tries to detect headers in the first row. If
// they are not detected, the row is read as data. Otherwise data is read
// starting from the second row. skipLeadingRows is 0 - Instructs
// autodetect that there are no headers and data should be read starting
// from the first row. skipLeadingRows = N > 0 - Autodetect skips N-1 rows
// and tries to detect headers in row N. If headers are not detected, row N
// is just skipped. Otherwise row N is used to extract column names for the
// detected schema" (JobConfigurationLoad); a header is detected "If the
// first line contains only strings, and the other lines contain other data
// types", and not when "all of the data fields are strings"; without one,
// BigQuery "assigns generic column names such as string_field_1"
// (schema-detect). The emulator takes the first row as the header
// whatever it holds and ignores skipLeadingRows (measured, #919). So the
// front detects the schema itself as above for a load with a
// skipLeadingRows other than 1, and for any load whose header row it
// finds BigQuery would not take as one: the columns are then named
// <type>_field_<position from 0>, the type in lower case as GoogleSQL
// names it (string, int64, double, bool, date, time, datetime, timestamp;
// only string_field_ is documented), and the row is data (unset) or
// skipped (N). A load whose header both read the same way is still sent
// to the emulator, which detects its columns as before.

// csvDetect detects the schema of a load described above, and sets it in
// the job and in r. It returns false when it has answered the load.
func (f front) csvDetect(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || !l.Autodetect || !(l.SourceFormat == "" || strings.EqualFold(l.SourceFormat, "CSV")) ||
		l.Schema != nil && len(l.Schema.Fields) > 0 {
		return true
	}
	var opts struct {
		Configuration struct {
			Load csvOptions `json:"load"`
		} `json:"configuration"`
	}
	_ = decodeJob(r, &opts)
	o := opts.Configuration.Load
	skip, skipSet := skipLeadingRows(l.SkipLeadingRows)
	if skip < 0 {
		return true
	}
	// must: the emulator cannot detect this load's columns as BigQuery
	// does; otherwise the front detects them only when the header row is
	// one BigQuery would not take (#1457), and leaves any load it cannot
	// read to the emulator, as before.
	must := o.TimeZone != "" || o.DateFormat != "" || o.DatetimeFormat != "" || o.TimeFormat != "" ||
		o.TimestampFormat != "" || skipSet && skip != 1
	dest := l.DestinationTable
	if dest == nil || dest.DatasetID == "" || dest.TableID == "" || dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) {
		return true // csvTimesOf refuses it, 501
	}
	if status, _ := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID)); status != http.StatusNotFound {
		return true
	}
	notImplemented := func(why string) bool {
		if !must {
			return true // the emulator detects the columns (autodetectLoad)
		}
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load with autodetect, "+
			"no schema, into a new table, "+why+". Nothing was loaded. Give the load a schema.")
		return false
	}
	times, code, why := csvTimesOf(o, false)
	if times == nil {
		times = &csvTimes{loc: time.UTC, formats: map[string]string{}}
	}
	times.now = f.now // #1487
	if code != 0 {
		if code == http.StatusNotImplemented {
			return notImplemented(why)
		}
		writeError(w, code, "invalid", why)
		return false
	}
	delim := ','
	if l.FieldDelimiter != "" {
		d := l.FieldDelimiter
		if d == `\t` || strings.EqualFold(d, "tab") {
			d = "\t"
		}
		if utf8.RuneCountInString(d) != 1 {
			return notImplemented("whose fieldDelimiter is not one character")
		}
		delim, _ = utf8.DecodeRuneInString(d)
	}
	if l.Quote != nil && *l.Quote != `"` || o.Encoding != "" && !strings.EqualFold(o.Encoding, "UTF-8") {
		return notImplemented("with a quote or an encoding of its own")
	}
	var sample []byte
	if len(l.SourceURIs) > 0 {
		if f.storage == nil {
			return notImplemented("from Cloud Storage, with no Cloud Storage for CloudBurrow to read the data from")
		}
		objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return false
		}
		body, err := f.storage.open(r.Context(), objs[0])
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return false
		}
		sample, err = readRecords(bufio.NewReader(body), int(skip)+jsonSample+1)
		body.Close()
		if err != nil {
			return notImplemented("whose data CloudBurrow could not read (" + err.Error() + ")")
		}
	} else {
		var err error
		if sample, err = peekUpload(r, int(skip)+jsonSample+1); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return false
		}
	}
	if len(l.SourceURIs) == 0 && sample == nil {
		return notImplemented("whose data CloudBurrow could not read")
	}
	fields, send, header, why := detectCSVHeader(sample, delim, times, skip, skipSet)
	if why != "" {
		return notImplemented(why)
	}
	if !must && header {
		return true // the emulator's header is BigQuery's
	}
	charMap := strings.ToUpper(l.ColumnNameCharacterMap)
	check := checkColumnName
	if charMap == "V1" {
		check = checkClassicColumnName
	}
	if bad := checkNames(fields, "", nil, check); bad != "" {
		if charMap == "V1" || charMap == "V2" {
			return notImplemented("under columnNameCharacterMap " + charMap + ", which would rename a detected column: " +
				bad + " CloudBurrow does not rename it")
		}
		writeError(w, http.StatusBadRequest, "invalid", bad+" The name was detected from the load's data (autodetect); "+
			"columnNameCharacterMap STRICT, the default, refuses it rather than renaming it. Nothing was loaded.")
		return false
	}
	raw := schemaJSON(fields)
	if !editJob(r, func(j map[string]any) bool {
		conf, _ := j["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["schema"] = map[string]any{"fields": raw}
		load["skipLeadingRows"] = send
		return true
	}) {
		return notImplemented("whose job CloudBurrow could not change")
	}
	l.Schema = &tableSchema{Fields: fields}
	l.SkipLeadingRows = json.RawMessage(strconv.FormatInt(send, 10))
	l.detected = true
	return true
}

// detectCSVSchema detects the columns of sample, CSV data with a header,
// by times' formats (above); or why it cannot.
func detectCSVSchema(sample []byte, delim rune, times *csvTimes) ([]field, string) {
	fields, _, header, why := detectCSVHeader(sample, delim, times, 1, true)
	if why == "" && !header {
		return nil, "whose first row is not a header BigQuery would detect"
	}
	return fields, why
}

// detectCSVHeader detects the columns of sample under skipLeadingRows skip
// (set when skipSet), as described above. It returns the columns, the
// skipLeadingRows to send the load with, and whether a header row named
// them; or why it cannot.
func detectCSVHeader(sample []byte, delim rune, times *csvTimes, skip int64, skipSet bool) ([]field, int64, bool, string) {
	cr := csv.NewReader(bytes.NewReader(sample))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	var recs [][]string
	for int64(len(recs)) <= skip+jsonSample {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, false, "whose sampled records CloudBurrow could not read as CSV (" + err.Error() + ")"
		}
		recs = append(recs, rec)
	}
	first := int64(0) // the record headers are looked for in, -1 for none
	switch {
	case !skipSet:
	case skip == 0:
		first = -1
	default:
		first = skip - 1
	}
	if first >= int64(len(recs)) || first < 0 && len(recs) == 0 {
		return nil, 0, false, "with no record after the rows skipLeadingRows skips, from which to detect the columns"
	}
	var data [][]string
	var head []string
	if first >= 0 {
		head, data = recs[first], recs[first+1:]
	} else {
		data = recs
	}
	width := len(head)
	if head == nil {
		width = len(data[0])
	}
	types := func(rows [][]string) ([]string, string) {
		guesses := make([]*jsonGuess, width)
		for i := range guesses {
			guesses[i] = &jsonGuess{name: fmt.Sprintf("column %d", i+1), kinds: map[string]bool{}}
		}
		for _, rec := range rows {
			if len(rec) != width {
				return nil, "whose records do not all have the same number of values"
			}
			for i, v := range rec {
				if v == "" {
					continue
				}
				k, why := times.detectKind(v)
				if why != "" {
					return nil, "with a value " + v + " " + why
				}
				guesses[i].kinds[k] = true
			}
		}
		out := make([]string, width)
		for i, g := range guesses {
			typ, why := g.resolveType(g.name)
			if why != "" {
				return nil, why
			}
			out[i] = typ
		}
		return out, ""
	}
	header := false
	if head != nil && len(data) > 0 {
		typs, why := types(data)
		if why != "" {
			return nil, 0, false, why
		}
		header = true
		typed := false
		for i, name := range head {
			if k, _ := times.detectKind(name); name == "" || k != "STRING" {
				header = false
			}
			if typs[i] != "STRING" {
				typed = true
			}
		}
		if header = header && typed; header {
			fields := make([]field, width)
			for i, name := range head {
				fields[i] = field{Name: name, Type: typs[i], Mode: "NULLABLE"}
			}
			return fields, first + 1, true, ""
		}
	}
	send := first + 1 // the header row looked for in is skipped (N)
	if !skipSet {
		send, data = 0, recs // it is data
	}
	if len(data) == 0 {
		return nil, 0, false, "with no data row, from which to detect the columns' types"
	}
	typs, why := types(data)
	if why != "" {
		return nil, 0, false, why
	}
	fields := make([]field, width)
	for i, typ := range typs {
		fields[i] = field{Name: genericColumnName(typ, i), Type: typ, Mode: "NULLABLE"}
	}
	return fields, send, false, ""
}

// genericColumnName is the name BigQuery's autodetect gives column i of
// type typ when it finds no header (above).
func genericColumnName(typ string, i int) string {
	prefix := map[string]string{"STRING": "string", "INTEGER": "int64", "FLOAT": "double", "BOOLEAN": "bool",
		"DATE": "date", "TIME": "time", "DATETIME": "datetime", "TIMESTAMP": "timestamp"}[typ]
	if prefix == "" {
		prefix = strings.ToLower(typ)
	}
	return fmt.Sprintf("%s_field_%d", prefix, i)
}

// detectKind is the type autodetect gives v under t's formats (above).
func (t *csvTimes) detectKind(v string) (string, string) {
	for _, typ := range []string{"DATE", "DATETIME", "TIME", "TIMESTAMP"} {
		if _, ok := t.formats[typ]; ok && t.convert([]string{v}, []field{{Name: "v", Type: typ}}) == "" {
			return typ, ""
		}
	}
	k, why := stringKind(v)
	if why != "" {
		return "", why
	}
	if _, ok := t.formats[k]; ok {
		return "STRING", "" // the format is the only compatible one
	}
	return k, ""
}
