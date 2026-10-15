package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
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
// the double-quote quote, and UTF-8; another, a skipLeadingRows other than
// 1, records of different widths and a quoted value across lines in the
// sample are 501.

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
	if o.TimeZone == "" && o.DateFormat == "" && o.DatetimeFormat == "" && o.TimeFormat == "" && o.TimestampFormat == "" {
		return true // the emulator detects the columns (autodetectLoad)
	}
	dest := l.DestinationTable
	if dest == nil || dest.DatasetID == "" || dest.TableID == "" || dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) {
		return true // csvTimesOf refuses it, 501
	}
	if status, _ := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID)); status != http.StatusNotFound {
		return true
	}
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a CSV load with autodetect, "+
			"no schema, and a timeZone or a date or time format, into a new table, "+why+". Nothing was loaded. Give "+
			"the load a schema.")
		return false
	}
	times, code, why := csvTimesOf(o, false)
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
	if skip, set := skipLeadingRows(l.SkipLeadingRows); set && skip != 1 {
		return notImplemented("with a skipLeadingRows other than 1")
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
		sample, err = readRecords(bufio.NewReader(body), jsonSample+1)
		body.Close()
		if err != nil {
			return notImplemented("whose data CloudBurrow could not read (" + err.Error() + ")")
		}
	} else {
		var err error
		if sample, err = peekUpload(r, jsonSample+1); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return false
		}
	}
	fields, why := detectCSVSchema(sample, delim, times)
	if why != "" {
		return notImplemented(why)
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
		load["skipLeadingRows"] = 1
		return true
	}) {
		return notImplemented("whose job CloudBurrow could not change")
	}
	l.Schema = &tableSchema{Fields: fields}
	l.SkipLeadingRows = json.RawMessage("1")
	l.detected = true
	return true
}

// detectCSVSchema detects the columns of sample, CSV data with a header,
// by times' formats (above); or why it cannot.
func detectCSVSchema(sample []byte, delim rune, times *csvTimes) ([]field, string) {
	cr := csv.NewReader(bytes.NewReader(sample))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	var recs [][]string
	for len(recs) <= jsonSample {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "whose sampled records CloudBurrow could not read as CSV (" + err.Error() + ")"
		}
		recs = append(recs, rec)
	}
	if len(recs) < 2 {
		return nil, "with fewer than two records, from which CloudBurrow cannot tell whether the first row is a header"
	}
	header := recs[0]
	guesses := make([]*jsonGuess, len(header))
	for i, name := range header {
		guesses[i] = &jsonGuess{name: name, kinds: map[string]bool{}}
	}
	for _, rec := range recs[1:] {
		if len(rec) != len(header) {
			return nil, "whose records do not all have the header's number of values"
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
	fields := make([]field, len(header))
	typed := false
	for i, g := range guesses {
		if k, _ := times.detectKind(g.name); g.name == "" || k != "STRING" {
			return nil, "whose first row holds a value that is not a STRING, so BigQuery would load it as data and " +
				"name the columns itself"
		}
		typ, why := g.resolveType(g.name)
		if why != "" {
			return nil, why
		}
		if typ != "STRING" {
			typed = true
		}
		fields[i] = field{Name: g.name, Type: typ, Mode: "NULLABLE"}
	}
	if !typed {
		return nil, "whose columns are all STRING, so whether BigQuery takes the first row as the header is not documented"
	}
	return fields, ""
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
