package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A NEWLINE_DELIMITED_JSON load with autodetect and no schema into a new
// table (#1324). The emulator answers it with 500 "runtime error: invalid
// memory address or nil pointer dereference" (measured), which the Go
// client retries until its deadline. So the front detects the schema
// itself, as BigQuery documents it, and sends the load on with it:
// https://cloud.google.com/bigquery/docs/schema-detect
//
//   - "BigQuery selects a random file in the data source and scans up to
//     the first 500 rows of data to use as a representative sample." The
//     front reads the first 500 records of the first file (an upload's
//     data, or the first object the load's URIs name).
//   - "BigQuery then examines each field and attempts to assign a data
//     type to that field based on the values in the sample." A JSON true
//     or false is BOOLEAN, an integer INTEGER, any other number FLOAT.
//     "BigQuery converts strings into Boolean, numeric, or date/time types
//     when possible": a string that is true or false, an integer, a number,
//     a DATE (YYYY-MM-DD), a TIME (HH:MM:SS[.SSSSSS]) or a TIMESTAMP
//     (YYYY-MM-DD HH:MM[:SS[.SSSSSS]], with T or a space, and a UTC offset,
//     Z or UTC, or none) is of that type, and any other string is STRING.
//   - "If a field value is a JSON object, then BigQuery loads the column as
//     a RECORD type. If a field value is an array, then BigQuery loads the
//     column as a repeated column." Every other column is NULLABLE, as is
//     every field of a RECORD.
//   - "If all of the rows in a column are empty, auto-detection will
//     default to STRING data type for the column."
//   - Columns are in the order the sample first names them; a name is
//     the same column in any case (column names are case-insensitive).
//
// Values of one column that are of different types: INTEGER and FLOAT
// make a FLOAT column, and strings of which some are not of another type
// a STRING one. Any other mix (a number and a string that is not one, a
// DATE and a TIMESTAMP, an object and a value, an array and a value) is
// 501, as BigQuery does not document what it detects; so are an array of
// arrays, an object only ever empty, an integer outside INT64, and a
// string that looks like a date or a time in a form other than those
// above (BigQuery "detects a wide array of timestamp formats", which it
// does not list in full).
//
// The detected names are held to columnNameCharacterMap as autodetectLoad
// holds a CSV's (STRICT: 400 invalid; V1 or V2, which would rename the
// column: 501). The load is then sent on with the detected schema, and so
// read as any load with a schema (jsonLoad), each TIMESTAMP value of a
// detected column written in the one form the emulator reads in every
// case, RFC 3339 in UTC (measured: it failed "2020-01-02T03:04Z" and
// "2020-01-02 03:04:05 -05:00", "failed to convert ... to time.Time").

// jsonSample is how many records of the data the schema is detected from.
const jsonSample = 500

// jsonDetect detects the schema of a NEWLINE_DELIMITED_JSON load with
// autodetect and no schema into a new table, and sets it in the job and in
// r (above). It returns false when it has answered the load.
func (f front) jsonDetect(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || !l.Autodetect || !strings.EqualFold(l.SourceFormat, "NEWLINE_DELIMITED_JSON") ||
		l.Schema != nil && len(l.Schema.Fields) > 0 {
		return true
	}
	dest := l.DestinationTable
	if dest == nil || dest.DatasetID == "" || dest.TableID == "" || dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) {
		return true
	}
	if status, _ := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID)); status != http.StatusNotFound {
		return true // an existing table keeps its schema
	}
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a NEWLINE_DELIMITED_JSON load "+
			"with autodetect into a new table "+why+". Nothing was loaded. Give the load a schema, or create the table first.")
		return false
	}
	var sample []byte
	var where string
	if len(l.SourceURIs) > 0 {
		if f.storage == nil {
			return notImplemented("from Cloud Storage, with no Cloud Storage for CloudBurrow to read the data from to " +
				"detect its schema")
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
		sample, err = readRecords(bufio.NewReader(body), jsonSample)
		body.Close()
		if err != nil {
			return notImplemented("whose data CloudBurrow could not read (" + err.Error() + ")")
		}
		where = objs[0].uri()
	} else {
		var err error
		if sample, err = peekUpload(r, jsonSample); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "The load's multipart request is invalid: "+err.Error())
			return false
		}
		where = "the load's data"
	}
	fields, code, msg := detectJSONSchema(sample, where)
	if code != 0 {
		if code == http.StatusNotImplemented {
			return notImplemented(msg)
		}
		writeError(w, code, "invalid", msg)
		return false
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
		return true
	}) {
		return notImplemented("whose job CloudBurrow could not change")
	}
	l.Schema = &tableSchema{Fields: fields}
	l.detected = true
	return true
}

// schemaJSON is fields as a TableSchema's fields.
func schemaJSON(fields []field) []any {
	out := make([]any, len(fields))
	for i, fl := range fields {
		m := map[string]any{"name": fl.Name, "type": fl.Type, "mode": fl.Mode}
		if len(fl.Fields) > 0 {
			m["fields"] = schemaJSON(fl.Fields)
		}
		out[i] = m
	}
	return out
}

// readRecords reads up to n records (non-blank lines) of br.
func readRecords(br *bufio.Reader, n int) ([]byte, error) {
	var out bytes.Buffer
	for got := 0; got < n; {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			got++
		}
		out.Write(line)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// peekUpload reads up to n records of the data of r, a multipart upload,
// and leaves r's body as it was. A body that is not a multipart upload
// has no records.
func peekUpload(r *http.Request, n int) ([]byte, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" || r.Header.Get("Content-Encoding") != "" {
		return nil, nil
	}
	var read bytes.Buffer
	rest := r.Body
	defer func() {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(read.Bytes()), rest), rest}
	}()
	mr := multipart.NewReader(io.TeeReader(rest, &read), params["boundary"])
	if _, err := mr.NextPart(); err != nil {
		return nil, err
	}
	media, err := mr.NextPart()
	if err != nil {
		return nil, err
	}
	return readRecords(bufio.NewReader(media), n)
}

// jsonGuess is what the sample tells of one column or field.
type jsonGuess struct {
	name     string
	kinds    map[string]bool // the types of its values
	native   bool            // a value that was not a string
	array    bool            // a value that was an array
	value    bool            // a value that was not an array
	fields   []*jsonGuess    // a RECORD's, in order
	byName   map[string]*jsonGuess
	sawEmpty bool // an empty object
}

// detectJSONSchema detects the schema of sample, NEWLINE_DELIMITED_JSON
// data found at where (above). It returns the columns, or the status and
// why the load is refused.
func detectJSONSchema(sample []byte, where string) ([]field, int, string) {
	root := &jsonGuess{kinds: map[string]bool{}}
	line := 0
	for _, text := range bytes.Split(sample, []byte("\n")) {
		line++
		if len(bytes.TrimSpace(text)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.UseNumber()
		v, err := decodeOrdered(dec)
		if err == nil {
			if _, err2 := dec.Token(); !errors.Is(err2, io.EOF) {
				err = errors.New("more than one JSON value on the line")
			}
		}
		obj, isObj := v.(*orderedObject)
		if err != nil || !isObj {
			if err == nil {
				err = errors.New("not a JSON object")
			}
			return nil, http.StatusBadRequest, fmt.Sprintf("Error while reading data, error message: JSON parsing error "+
				"in line %d of %s: %v. A NEWLINE_DELIMITED_JSON file holds one JSON object on each line. Nothing was "+
				"loaded.", line, where, err)
		}
		if msg := root.observeObject(obj, ""); msg != "" {
			return nil, http.StatusNotImplemented, msg
		}
	}
	if len(root.fields) == 0 {
		return nil, http.StatusNotImplemented, "whose data has no field to detect a column from"
	}
	fields, msg := root.resolveFields("")
	if msg != "" {
		return nil, http.StatusNotImplemented, msg
	}
	return fields, 0, ""
}

func (g *jsonGuess) observeObject(obj *orderedObject, prefix string) string {
	if len(obj.keys) == 0 {
		g.sawEmpty = true
	}
	if g.byName == nil {
		g.byName = map[string]*jsonGuess{}
	}
	for i, k := range obj.keys {
		key := strings.ToLower(k)
		c, ok := g.byName[key]
		if !ok {
			c = &jsonGuess{name: k, kinds: map[string]bool{}}
			g.byName[key] = c
			g.fields = append(g.fields, c)
		}
		if msg := c.observe(obj.vals[i], prefix+c.name, false); msg != "" {
			return msg
		}
	}
	return ""
}

func (g *jsonGuess) observe(v any, path string, inArray bool) string {
	if v == nil {
		return ""
	}
	if a, ok := v.([]any); ok {
		if inArray {
			return "whose field " + path + " holds an array of arrays, which BigQuery does not load"
		}
		g.array = true
		for _, e := range a {
			if msg := g.observe(e, path, true); msg != "" {
				return msg
			}
		}
		return ""
	}
	if !inArray {
		g.value = true
	}
	switch v := v.(type) {
	case *orderedObject:
		g.kinds["RECORD"] = true
		return g.observeObject(v, path+".")
	case bool:
		g.kinds["BOOLEAN"], g.native = true, true
	case json.Number:
		g.native = true
		s := v.String()
		if !strings.ContainsAny(s, ".eE") {
			if _, err := strconv.ParseInt(s, 10, 64); err != nil {
				return fmt.Sprintf("whose field %s holds the integer %s, outside INT64; what BigQuery detects for it is not "+
					"documented", path, s)
			}
			g.kinds["INTEGER"] = true
		} else {
			g.kinds["FLOAT"] = true
		}
	case string:
		k, msg := stringKind(v)
		if msg != "" {
			return fmt.Sprintf("whose field %s holds %q, %s", path, v, msg)
		}
		g.kinds[k] = true
	}
	return ""
}

var (
	jsonIntPattern   = regexp.MustCompile(`^[+-]?[0-9]+$`)
	jsonFloatPattern = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	jsonDatePattern  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	jsonTimePattern  = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?$`)
	jsonTSPattern    = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})[ T]([0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]{1,6})?)?)` +
		`(?: ?(Z|UTC|[+-][0-9]{2}(?::?[0-9]{2})?))?$`)
	// Anything else that starts as a date or a time.
	jsonDateLike = regexp.MustCompile(`^[0-9]{1,4}[-/.][0-9]{1,2}[-/.][0-9]{1,4}([ T]|$)|^[0-9]{1,2}:[0-9]{2}`)
)

// stringKind is the type of a JSON string's value (above), or why the
// load is 501.
func stringKind(s string) (string, string) {
	switch {
	case strings.EqualFold(s, "true"), strings.EqualFold(s, "false"):
		return "BOOLEAN", ""
	case jsonIntPattern.MatchString(s):
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return "", "an integer outside INT64; what BigQuery detects for it is not documented"
		}
		return "INTEGER", ""
	case jsonFloatPattern.MatchString(s):
		return "FLOAT", ""
	case jsonDatePattern.MatchString(s):
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return "DATE", ""
		}
	case jsonTimePattern.MatchString(s):
		if _, err := time.Parse("15:04:05.999999", s); err == nil {
			return "TIME", ""
		}
	default:
		if _, err := parseDetectedTimestamp(s); err == nil {
			return "TIMESTAMP", ""
		}
	}
	if jsonDateLike.MatchString(s) {
		return "", "which looks like a date or a time in a form CloudBurrow does not detect; BigQuery \"detects a wide " +
			"array of timestamp formats\" and does not list them all"
	}
	return "STRING", ""
}

// parseDetectedTimestamp reads a TIMESTAMP in a form stringKind detects.
func parseDetectedTimestamp(s string) (time.Time, error) {
	m := jsonTSPattern.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, errors.New("not a timestamp")
	}
	clock := m[2]
	if len(clock) == 5 {
		clock += ":00"
	}
	loc := time.UTC
	switch z := m[3]; {
	case z == "" || z == "Z" || z == "UTC":
	default:
		digits := strings.ReplaceAll(z[1:], ":", "")
		h, _ := strconv.Atoi(digits[:2])
		mins := 0
		if len(digits) == 4 {
			mins, _ = strconv.Atoi(digits[2:])
		}
		if h > 23 || mins > 59 {
			return time.Time{}, errors.New("not a UTC offset")
		}
		off := h*3600 + mins*60
		if z[0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	return time.ParseInLocation("2006-01-02 15:04:05.999999", m[1]+" "+clock, loc)
}

// resolveFields is the schema of g's fields.
func (g *jsonGuess) resolveFields(prefix string) ([]field, string) {
	out := make([]field, 0, len(g.fields))
	for _, c := range g.fields {
		path := prefix + c.name
		fl := field{Name: c.name, Mode: "NULLABLE"}
		if c.array {
			fl.Mode = "REPEATED"
			if c.value {
				return nil, "whose field " + path + " holds both arrays and other values; what BigQuery detects for it " +
					"is not documented"
			}
		}
		typ, msg := c.resolveType(path)
		if msg != "" {
			return nil, msg
		}
		fl.Type = typ
		if typ == "RECORD" {
			if len(c.fields) == 0 {
				return nil, "whose field " + path + " holds only empty objects; BigQuery has no RECORD without fields"
			}
			sub, msg := c.resolveFields(path + ".")
			if msg != "" {
				return nil, msg
			}
			fl.Fields = sub
		}
		out = append(out, fl)
	}
	return out, ""
}

// resolveType is the type of the values of the column at path (above).
func (g *jsonGuess) resolveType(path string) (string, string) {
	switch {
	case len(g.kinds) == 0:
		return "STRING", ""
	case len(g.kinds) == 1:
		for k := range g.kinds {
			return k, ""
		}
	case g.kinds["RECORD"]:
		return "", "whose field " + path + " holds both objects and other values; what BigQuery detects for it is not documented"
	}
	numeric := true
	for k := range g.kinds {
		if k != "INTEGER" && k != "FLOAT" {
			numeric = false
		}
	}
	if numeric {
		return "FLOAT", ""
	}
	if g.kinds["STRING"] && !g.native {
		return "STRING", ""
	}
	kinds := make([]string, 0, len(g.kinds))
	for _, k := range []string{"BOOLEAN", "INTEGER", "FLOAT", "DATE", "TIME", "TIMESTAMP", "STRING"} {
		if g.kinds[k] {
			kinds = append(kinds, k)
		}
	}
	return "", "whose field " + path + " holds values of the types " + strings.Join(kinds, ", ") + "; what BigQuery " +
		"detects for such a mix is not documented"
}

// orderedObject is a JSON object with its keys in order.
type orderedObject struct {
	keys []string
	vals []any
}

// decodeOrdered reads one JSON value of dec (with UseNumber), each object
// as an orderedObject.
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &orderedObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.keys, o.vals = append(o.keys, k), append(o.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	}
	return tok, nil
}

// detectedTimestamps writes each TIMESTAMP value of fields in obj, a
// record of a load whose schema the front detected, as RFC 3339 in UTC,
// which the emulator reads (above). It reports whether it changed any.
func detectedTimestamps(fields []field, obj map[string]any) bool {
	changed := false
	byName := map[string]field{}
	for _, fl := range fields {
		byName[strings.ToLower(fl.Name)] = fl
	}
	fix := func(fl field, v any) (any, bool) {
		switch v := v.(type) {
		case string:
			if strings.EqualFold(fl.Type, "TIMESTAMP") {
				if t, err := parseDetectedTimestamp(v); err == nil {
					return t.UTC().Format("2006-01-02T15:04:05.999999Z"), true
				}
			}
		case map[string]any:
			if isRecord(fl.Type) {
				return v, detectedTimestamps(fl.Fields, v)
			}
		}
		return v, false
	}
	for k, v := range obj {
		fl, ok := byName[strings.ToLower(k)]
		if !ok {
			continue
		}
		if a, ok := v.([]any); ok {
			for i, e := range a {
				if nv, c := fix(fl, e); c {
					a[i], changed = nv, true
				}
			}
			continue
		}
		if nv, c := fix(fl, v); c {
			obj[k], changed = nv, true
		}
	}
	return changed
}

// hasTimestamp reports whether fields have a TIMESTAMP at any depth.
func hasTimestamp(fields []field) bool {
	for _, fl := range fields {
		if strings.EqualFold(fl.Type, "TIMESTAMP") || hasTimestamp(fl.Fields) {
			return true
		}
	}
	return false
}
