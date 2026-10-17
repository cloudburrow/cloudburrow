package bigqueryfront

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Hive-partitioned JSON loads and GeoJSON loads (#1547).
//
// The pinned emulator loaded a JSON load with hivePartitioningOptions or
// jsonExtension GEOJSON as if neither were given (measured, #1457). So the
// front reads such a load's data itself (from the instance's Cloud
// Storage, gcsload.go, or the upload), writes each record as the plain
// newline-delimited JSON record BigQuery documents it as, and sends the
// records on as the data of an upload of the same job, without those two
// options: the path every other JSON load takes (jsonLoad, jsonDetect).
// The client's configuration.load is put back in the job's answers
// (clientLoad, #998).
//
// hivePartitioningOptions, as JobConfigurationLoad and HivePartitioningOptions
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad)
// and "Loading externally partitioned data"
// (https://cloud.google.com/bigquery/docs/hive-partitioned-loads-gcs, read
// 2026-10-17) document it:
//
//   - "Hive partition keys appear as normal columns": each record gets one
//     member a key, its value the file's directory "key=value" after the
//     prefix ("Data is loaded with dt and val columns included").
//   - sourceUriPrefix: "a common prefix for all source uris must be
//     required. The prefix must end immediately before the partition key
//     encoding begins", with or without its "/" for AUTO and STRINGS; a
//     load without one, or a file not under it, is 400 invalid.
//   - mode AUTO: "Key names and types are automatically detected", of
//     "STRING, INTEGER, DATE, and TIMESTAMP"; STRINGS: "Key names are
//     automatically converted to STRING type"; CUSTOM: "partition key
//     schema is encoded in the source URI prefix", as
//     gs://bucket/path_to_table/{dt:DATE}/{country:STRING}/{id:INTEGER},
//     and "The values must validly parse as the specified type" (400
//     invalid when one does not). UNVERIFIED: no mode is read as AUTO, and
//     AUTO gives a key the first of INTEGER, DATE, TIMESTAMP that every
//     value of it parses as, and STRING otherwise (the page names the
//     types, not how a key whose values differ is typed); CUSTOM's types
//     are the four AUTO detects (the page lists no others), and another is
//     501.
//   - "If the partition key names are not encoded in the directory path,
//     partition schema detection fails", and "Files where the schema is
//     not in a consistent order also fail detection": a directory that is
//     not key=value, or keys not the same in the same order in every file
//     (CUSTOM's, for CUSTOM), is 400 invalid; so are more than "ten
//     partition keys per table", and a record with a member named as a key
//     ("Hive partitioning keys and the columns in the underlying files
//     cannot overlap").
//   - requirePartitionFilter: "Hive-partitioned loads with
//     require_partition_filter explicitly set to true will fail": 400
//     invalid.
//   - "Currently supported formats are: JSON, CSV, ORC, Avro and Parquet",
//     and "Requesting hive partitioning on an unsupported format will lead
//     to an error": another format is 400 invalid. CSV is 501 here: the
//     front adds the keys to JSON records, and to the rows of a Parquet or
//     Avro load it carries out itself (hiveFiles, parquetload.go,
//     avroload.go), one with no schema in the job (the keys are columns
//     after the file's).
//
// UNVERIFIED: the keys are columns after the file's columns (the page does
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/hive-partitioned-loads-gcs
// not say where; HivePartitioningOptions.fields lists them "in the order
// they were inferred"). A schema the job gives that has no column for a
// key gets one at its end, NULLABLE, of the key's type; one it has is
// typed by the schema (STRING, INTEGER, DATE or TIMESTAMP; another is
// 501). A detected schema (autodetect, jsondetect.go) has each key of its
// own type. A key's value is read from the path with %XX escapes decoded
// (UNVERIFIED: Hive's own escaping; the page shows no escaped value).
//
// jsonExtension GEOJSON, as JobConfigurationLoad ("To load newline-delimited
// GeoJSON, specify GEOJSON (and source_format must be set to
// NEWLINE_DELIMITED_JSON)", 400 invalid otherwise) and "Loading GeoJSON
// data" (https://cloud.google.com/bigquery/docs/geospatial-data, read
// 2026-10-17) document it: each line is a Feature object; "type ... must
// be Feature. BigQuery validates the value but does not include it in the
// table schema" (400 invalid otherwise); "geometry ... a GeoJSON Geometry
// object or null. BigQuery converts this member into a GEOGRAPHY value";
// "properties ... any JSON object or null. If the value isn't null, then
// BigQuery loads each member of the JSON object as a separate table
// column"; "id ... either a string or a number. BigQuery loads this value
// into a column named id"; other members "BigQuery converts ... directly
// into table columns". UNVERIFIED: the geometry's column is named geometry
// (the page names the member, not the column), a NULL geometry is a NULL
// GEOGRAPHY, the columns are in the Feature's order with the properties'
// members in place of properties, and with autodetect the geometry column
// is GEOGRAPHY whatever its values. A property named as a column the
// Feature already makes (geometry, id or another member) is 501, as the
// page does not say which wins.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/geospatial-data
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/hive-partitioned-loads-gcs

// hiveKey is one partition key: its name and type.
type hiveKey struct{ name, typ string }

// variantLoad is the part of a load's job this file reads.
type variantLoad struct {
	SourceFormat  string   `json:"sourceFormat"`
	SourceURIs    []string `json:"sourceUris"`
	JSONExtension string   `json:"jsonExtension"`
	Hive          *struct {
		Mode                   string `json:"mode"`
		SourceURIPrefix        string `json:"sourceUriPrefix"`
		RequirePartitionFilter bool   `json:"requirePartitionFilter"`
	} `json:"hivePartitioningOptions"`
	Schema *tableSchema `json:"schema"`
}

// maxHiveKeys is "a maximum of ten partition keys per table".
const maxHiveKeys = 10

// loadVariant carries out the reading above for the load in r, when it is
// hive-partitioned or GeoJSON. It returns the request to send on (r, when
// the load is neither), the client's configuration.load to answer with
// (or nil), a function to call on the job once its schema is detected
// (or nil), and ok false when it has answered w.
func (f front) loadVariant(w http.ResponseWriter, r *http.Request) (*http.Request, json.RawMessage, func(*http.Request, *jobBody), bool) {
	var job struct {
		Configuration struct {
			Load *variantLoad `json:"load"`
		} `json:"configuration"`
	}
	if !decodeJob(r, &job) || job.Configuration.Load == nil {
		return r, nil, nil, true
	}
	l := job.Configuration.Load
	ext := strings.ToUpper(l.JSONExtension)
	geo := ext == "GEOJSON"
	if ext != "" && ext != "JSON_EXTENSION_UNSPECIFIED" && !geo {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid jsonExtension %q: it is GEOJSON or "+
			"JSON_EXTENSION_UNSPECIFIED.", l.JSONExtension))
		return r, nil, nil, false
	}
	format := strings.ToUpper(l.SourceFormat)
	if geo && format != "NEWLINE_DELIMITED_JSON" {
		writeError(w, http.StatusBadRequest, "invalid", "jsonExtension GEOJSON is a load option of sourceFormat "+
			"NEWLINE_DELIMITED_JSON (\"source_format must be set to NEWLINE_DELIMITED_JSON\"), not "+l.SourceFormat+".")
		return r, nil, nil, false
	}
	if !geo && l.Hive == nil {
		return r, nil, nil, true
	}
	fail := func(code int, msg string) (*http.Request, json.RawMessage, func(*http.Request, *jobBody), bool) {
		reason := "invalid"
		if code == http.StatusNotImplemented {
			reason, msg = "notImplemented", "Not implemented here: "+msg
		}
		writeError(w, code, reason, msg+" Nothing was loaded.")
		return r, nil, nil, false
	}
	var keys []hiveKey
	var base string
	if h := l.Hive; h != nil {
		switch format {
		case "NEWLINE_DELIMITED_JSON":
		case "PARQUET", "AVRO":
			if l.Schema != nil && len(l.Schema.Fields) > 0 {
				return fail(http.StatusNotImplemented, "a hive-partitioned "+format+" load with a schema: CloudBurrow "+
					"reads the file by its own schema, with the keys after its columns.")
			}
		case "CSV", "ORC":
			return fail(http.StatusNotImplemented, "a hive-partitioned "+format+" load: CloudBurrow adds the partition "+
				"keys to NEWLINE_DELIMITED_JSON records and Parquet and Avro rows only.")
		default:
			return fail(http.StatusBadRequest, "Hive partitioning is not supported for sourceFormat "+l.SourceFormat+
				": \"Currently supported formats are: JSON, CSV, ORC, Avro and Parquet.\"")
		}
		if h.RequirePartitionFilter {
			return fail(http.StatusBadRequest, "Invalid hivePartitioningOptions: \"Hive-partitioned loads with "+
				"require_partition_filter explicitly set to true will fail\"; requirePartitionFilter is for external tables.")
		}
		var custom []hiveKey
		var msg string
		base, custom, msg = hivePrefix(h.Mode, h.SourceURIPrefix)
		if msg != "" {
			code := http.StatusBadRequest
			if strings.HasPrefix(msg, "!") {
				code, msg = http.StatusNotImplemented, msg[1:]
			}
			return fail(code, msg)
		}
		if len(l.SourceURIs) == 0 {
			return fail(http.StatusBadRequest, "A hive-partitioned load reads its partition keys from its files' paths: "+
				"give it sourceUris under the sourceUriPrefix, not an upload.")
		}
		keys = custom
	}
	if len(l.SourceURIs) > 0 && f.storage == nil {
		writeNoStorage(w, l.SourceURIs)
		return r, nil, nil, false
	}

	// The job, and the client's configuration.load.
	var jm map[string]any
	if !decodeJob(r, &jm) {
		return fail(http.StatusBadRequest, "cloudburrow: could not read the job.")
	}
	conf, _ := jm["configuration"].(map[string]any)
	load, _ := conf["load"].(map[string]any)
	kept, _ := json.Marshal(load)

	// The files, each with its keys' values.
	type source struct {
		open func() (io.ReadCloser, error)
		loc  string
		vals []string
	}
	var srcs []source
	var upload *spooled
	if len(l.SourceURIs) > 0 {
		objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return r, nil, nil, false
		}
		var names []string
		for i, o := range objs {
			s := source{open: func() (io.ReadCloser, error) { return f.storage.open(r.Context(), o) }, loc: o.uri()}
			if l.Hive != nil {
				n, vals, msg := hivePath(o.uri(), base)
				if msg == "" && i > 0 && !sameNames(n, names) {
					msg = fmt.Sprintf("The partition keys of %s (%s) are not those of %s (%s): \"Files where the schema "+
						"is not in a consistent order also fail detection.\"", o.uri(), strings.Join(n, "/"), objs[0].uri(),
						strings.Join(names, "/"))
				}
				if msg == "" && len(n) > maxHiveKeys {
					msg = fmt.Sprintf("%s has %d partition keys; a table has \"a maximum of ten partition keys\".", o.uri(), len(n))
				}
				if msg != "" {
					return fail(http.StatusBadRequest, msg)
				}
				names, s.vals = n, vals
			}
			srcs = append(srcs, s)
		}
		if l.Hive != nil {
			if strings.EqualFold(l.Hive.Mode, "CUSTOM") {
				if !sameNames(names, keyNames(keys)) {
					return fail(http.StatusBadRequest, fmt.Sprintf("The partition keys of %s (%s) are not the "+
						"sourceUriPrefix's (%s): CUSTOM \"partition key schema is encoded in the source URI prefix\".",
						objs[0].uri(), strings.Join(names, "/"), strings.Join(keyNames(keys), "/")))
				}
			} else {
				keys = make([]hiveKey, len(names))
				for i, n := range names {
					keys[i] = hiveKey{name: n, typ: "STRING"}
					if !strings.EqualFold(l.Hive.Mode, "STRINGS") {
						vals := make([]string, len(srcs))
						for j, s := range srcs {
							vals[j] = s.vals[i]
						}
						keys[i].typ = hiveDetect(vals)
					}
				}
			}
		}
	} else {
		s, err := spoolUpload(r)
		if err != nil {
			return fail(http.StatusBadRequest, "The load's multipart request is invalid: "+err.Error())
		}
		upload = s
		srcs = []source{{open: func() (io.ReadCloser, error) { return io.NopCloser(io.NewSectionReader(s.file, 0, s.size)), nil }}}
	}
	if upload != nil {
		defer upload.Close()
	}

	// A schema the job gives types the keys it has, and gets those it has
	// not (above).
	if l.Hive != nil && l.Schema != nil && len(l.Schema.Fields) > 0 {
		fields, _ := load["schema"].(map[string]any)["fields"].([]any)
		for i, k := range keys {
			found := false
			for _, fl := range l.Schema.Fields {
				if strings.EqualFold(fl.Name, k.name) {
					t := hiveType(fl.Type)
					if t == "" || fl.Mode == "REPEATED" {
						return fail(http.StatusNotImplemented, fmt.Sprintf("a hive-partitioned load whose schema makes "+
							"partition key %s a %s %s column: BigQuery documents the keys' types STRING, INTEGER, DATE and "+
							"TIMESTAMP only.", k.name, fl.Mode, fl.Type))
					}
					keys[i].typ, found = t, true
				}
			}
			if !found {
				fields = append(fields, map[string]any{"name": k.name, "type": k.typ, "mode": "NULLABLE"})
			}
		}
		load["schema"] = map[string]any{"fields": fields}
	}
	for _, s := range srcs {
		for i, k := range keys {
			if _, ok := hiveValue(s.vals[i], k.typ); !ok {
				return fail(http.StatusBadRequest, fmt.Sprintf("Partition key %s of %s is %q, which is not a %s: \"The "+
					"values must validly parse as the specified type.\"", k.name, s.loc, s.vals[i], k.typ))
			}
		}
	}

	if format == "PARQUET" || format == "AVRO" {
		// The Parquet or Avro load the front carries out adds the keys to
		// each file's rows (parquet.go, parquetload.go, avroload.go).
		h := &hiveFiles{keys: keys, vals: map[string][]string{}}
		for _, s := range srcs {
			h.vals[s.loc] = s.vals
		}
		return r.WithContext(context.WithValue(r.Context(), hiveFilesKey{}, h)), nil, nil, true
	}

	// The records, read now, so that a record BigQuery refuses fails the
	// load before anything is loaded.
	tmp, err := os.CreateTemp("", "bigquery-variant-*")
	if err != nil {
		return fail(http.StatusInternalServerError, "cloudburrow: "+err.Error())
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	bw := bufio.NewWriter(tmp)
	for _, s := range srcs {
		var extra []byte
		for i, k := range keys {
			v, _ := hiveValue(s.vals[i], k.typ)
			b, _ := json.Marshal(k.name)
			extra = append(append(append(extra, ','), b...), ':')
			extra = append(extra, v...)
		}
		rc, err := s.open()
		if err != nil {
			e := asLoadDataError(err)
			writeError(w, e.code, e.reason, e.msg)
			return r, nil, nil, false
		}
		where := s.loc
		if where == "" {
			where = "the load's data"
		}
		msg := variantRecords(bw, bufio.NewReader(rc), geo, keys, extra, where)
		rc.Close()
		if msg != "" {
			code := http.StatusBadRequest
			if strings.HasPrefix(msg, "!") {
				code, msg = http.StatusNotImplemented, msg[1:]
			}
			return fail(code, msg)
		}
	}
	if err := bw.Flush(); err != nil {
		return fail(http.StatusInternalServerError, "cloudburrow: "+err.Error())
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return fail(http.StatusInternalServerError, "cloudburrow: "+err.Error())
	}
	delete(load, "hivePartitioningOptions")
	delete(load, "jsonExtension")
	body, err := json.Marshal(jm)
	if err != nil {
		return fail(http.StatusInternalServerError, "cloudburrow: "+err.Error())
	}
	data := &spooled{Reader: io.NewSectionReader(tmp, 0, size), file: tmp, size: size}
	out, err := f.gcsUpload(r, body, data)
	if err != nil {
		return fail(http.StatusBadRequest, "The load job could not be read: "+err.Error())
	}
	done = true
	fix := func(r *http.Request, job *jobBody) {
		jl := job.Configuration.Load
		if jl == nil || !jl.detected || jl.Schema == nil {
			return
		}
		types := map[string]string{}
		for _, k := range keys {
			types[strings.ToLower(k.name)] = k.typ
		}
		if geo {
			types["geometry"] = "GEOGRAPHY"
		}
		for i, fl := range jl.Schema.Fields {
			if t, ok := types[strings.ToLower(fl.Name)]; ok {
				jl.Schema.Fields[i].Type, jl.Schema.Fields[i].Fields = t, nil
			}
		}
		raw := schemaJSON(jl.Schema.Fields)
		editJob(r, func(j map[string]any) bool {
			conf, _ := j["configuration"].(map[string]any)
			load, _ := conf["load"].(map[string]any)
			if load == nil {
				return false
			}
			load["schema"] = map[string]any{"fields": raw}
			return true
		})
	}
	return out, kept, fix, true
}

// hivePrefix reads a sourceUriPrefix of mode (above): the prefix without
// its encoded schema and its last "/", and CUSTOM's keys, or why it is
// refused (a reason beginning "!" is 501).
func hivePrefix(mode, prefix string) (string, []hiveKey, string) {
	switch strings.ToUpper(mode) {
	case "", "AUTO", "STRINGS", "CUSTOM":
	default:
		return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.mode %q: it is AUTO, STRINGS or CUSTOM.", mode)
	}
	if prefix == "" {
		return "", nil, "Invalid hivePartitioningOptions: \"When hive partition detection is requested, a common " +
			"prefix for all source uris must be required\"; give its sourceUriPrefix."
	}
	if !strings.HasPrefix(prefix, "gs://") {
		return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.sourceUriPrefix %q: it is a gs:// URI.", prefix)
	}
	if !strings.EqualFold(mode, "CUSTOM") {
		if strings.Contains(prefix, "{") {
			return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.sourceUriPrefix %q: a key schema is encoded "+
				"in the prefix for mode CUSTOM only.", prefix)
		}
		return strings.TrimRight(prefix, "/"), nil, ""
	}
	i := strings.Index(prefix, "/{")
	if i < 0 {
		return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.sourceUriPrefix %q: \"CUSTOM detection requires "+
			"encoding the partitioning schema immediately after the common prefix\", as "+
			"gs://bucket/path_to_table/{dt:DATE}/{country:STRING}/{id:INTEGER}.", prefix)
	}
	var keys []hiveKey
	for _, seg := range strings.Split(strings.TrimRight(prefix[i+1:], "/"), "/") {
		inner, ok := strings.CutPrefix(seg, "{")
		inner, ok2 := strings.CutSuffix(inner, "}")
		name, typ, ok3 := strings.Cut(inner, ":")
		if !ok || !ok2 || !ok3 || name == "" {
			return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.sourceUriPrefix %q: %q is not {KEY:TYPE}.", prefix, seg)
		}
		t := hiveType(typ)
		if t == "" {
			return "", nil, fmt.Sprintf("!a CUSTOM partition key %s of type %s: BigQuery documents the types STRING, "+
				"INTEGER, DATE and TIMESTAMP only.", name, typ)
		}
		keys = append(keys, hiveKey{name: name, typ: t})
	}
	if len(keys) > maxHiveKeys {
		return "", nil, fmt.Sprintf("Invalid hivePartitioningOptions.sourceUriPrefix: %d partition keys; a table has "+
			"\"a maximum of ten partition keys\".", len(keys))
	}
	return prefix[:i], keys, ""
}

// hiveType is t as a partition key's type, or "" when it is not one.
func hiveType(t string) string {
	switch strings.ToUpper(t) {
	case "STRING":
		return "STRING"
	case "INTEGER", "INT64":
		return "INTEGER"
	case "DATE":
		return "DATE"
	case "TIMESTAMP":
		return "TIMESTAMP"
	}
	return ""
}

// hivePath reads the keys of uri, a file under base: their names and
// values, or why it is refused.
func hivePath(uri, base string) ([]string, []string, string) {
	rest, ok := strings.CutPrefix(uri, base+"/")
	if !ok {
		return nil, nil, fmt.Sprintf("%s is not under the sourceUriPrefix %s: hive partitioning assumes \"a common "+
			"source URI prefix for all URIs that ends immediately before partition encoding\".", uri, base)
	}
	segs := strings.Split(rest, "/")
	var names, vals []string
	for _, seg := range segs[:len(segs)-1] {
		k, v, ok := strings.Cut(seg, "=")
		if !ok || k == "" {
			return nil, nil, fmt.Sprintf("Directory %q of %s is not key=value: \"If the partition key names are not "+
				"encoded in the directory path, partition schema detection fails.\"", seg, uri)
		}
		if u, err := url.PathUnescape(v); err == nil {
			v = u
		}
		names, vals = append(names, k), append(vals, v)
	}
	return names, vals, ""
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func keyNames(keys []hiveKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.name
	}
	return out
}

// hiveDetect is AUTO's type of a key whose values are vals (above).
func hiveDetect(vals []string) string {
	for _, t := range []string{"INTEGER", "DATE", "TIMESTAMP"} {
		all := true
		for _, v := range vals {
			if _, ok := hiveValue(v, t); !ok {
				all = false
				break
			}
		}
		if all {
			return t
		}
	}
	return "STRING"
}

var hiveIntPattern = regexp.MustCompile(`^[+-]?[0-9]+$`)

// hiveTimestampLayouts are the TIMESTAMP forms a key's value is read in:
// a date and a time, with T or a space, and a UTC offset or none, as
// "/time=2018-10-18 16:00:00+00/" (UNVERIFIED beyond that example).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/hive-partitioned-loads-gcs
var hiveTimestampLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z07", "2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999 MST", "2006-01-02 15:04", "2006-01-02T15:04",
}

// hiveValue is v, a key's value, as the JSON value of a column of type t,
// and whether it is one.
func hiveValue(v, t string) ([]byte, bool) {
	switch t {
	case "INTEGER":
		if !hiveIntPattern.MatchString(v) {
			return nil, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, false
		}
		return []byte(strconv.FormatInt(n, 10)), true
	case "DATE":
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return nil, false
		}
		b, _ := json.Marshal(d.Format("2006-01-02"))
		return b, true
	case "TIMESTAMP":
		for _, layout := range hiveTimestampLayouts {
			if ts, err := time.Parse(layout, v); err == nil {
				b, _ := json.Marshal(ts.UTC().Format(time.RFC3339Nano))
				return b, true
			}
		}
		return nil, false
	}
	b, _ := json.Marshal(v)
	return b, true
}

// variantRecords writes the records of br, at where, to w, one a line:
// each GeoJSON Feature (geo) as its record, and each with extra, the
// keys' members, each beginning with a comma. It returns why the data is
// refused, or "" (a reason beginning "!" is 501).
func variantRecords(w io.Writer, br *bufio.Reader, geo bool, keys []hiveKey, extra []byte, where string) string {
	line := 0
	for {
		text, err := br.ReadBytes('\n')
		if len(text) > 0 {
			line++
			text = bytes.TrimSpace(text)
			if len(text) > 0 {
				names, vals, msg := objectMembers(text)
				if msg != "" {
					return fmt.Sprintf("Error while reading data, error message: JSON parsing error in row starting at "+
						"line %d of %s: %s", line, where, msg)
				}
				if geo {
					if names, vals, msg = geoRecord(names, vals); msg != "" {
						if strings.HasPrefix(msg, "!") {
							return "!" + fmt.Sprintf("a GeoJSON load whose Feature at line %d of %s has %s", line, where, msg[1:])
						}
						return fmt.Sprintf("Error while reading data, error message: GeoJSON parsing error in row "+
							"starting at line %d of %s: %s", line, where, msg)
					}
				}
				for _, k := range keys {
					for _, n := range names {
						if strings.EqualFold(n, k.name) {
							return fmt.Sprintf("The record at line %d of %s has a field %s, which is a partition key: "+
								"\"Hive partitioning keys and the columns in the underlying files cannot overlap.\"",
								line, where, n)
						}
					}
				}
				var out bytes.Buffer
				out.WriteByte('{')
				for i, n := range names {
					if i > 0 {
						out.WriteByte(',')
					}
					b, _ := json.Marshal(n)
					out.Write(b)
					out.WriteByte(':')
					out.Write(vals[i])
				}
				if len(names) == 0 && len(extra) > 0 {
					out.Write(extra[1:])
				} else {
					out.Write(extra)
				}
				out.WriteString("}\n")
				if _, err := w.Write(out.Bytes()); err != nil {
					return "cloudburrow: " + err.Error()
				}
			}
		}
		if err == io.EOF {
			return ""
		}
		if err != nil {
			return fmt.Sprintf("cloudburrow: could not read %s: %v", where, err)
		}
	}
}

// objectMembers reads text, one JSON object, as its members' names and
// raw values in order, or why it is not one.
func objectMembers(text []byte) ([]string, []json.RawMessage, string) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, nil, "the record is not a JSON object"
	}
	var names []string
	var vals []json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err.Error()
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err.Error()
		}
		names, vals = append(names, k), append(vals, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err.Error()
	}
	if dec.More() {
		return nil, nil, "more than one JSON value on the line"
	}
	return names, vals, ""
}

// geoRecord is the record of a GeoJSON Feature of members names and vals
// (above), or why it is refused.
func geoRecord(names []string, vals []json.RawMessage) ([]string, []json.RawMessage, string) {
	var outN []string
	var outV []json.RawMessage
	sawType := false
	add := func(n string, v json.RawMessage) string {
		for _, o := range outN {
			if strings.EqualFold(o, n) {
				return "!two members that are column " + n + " (the GeoJSON page does not say which is loaded)"
			}
		}
		outN, outV = append(outN, n), append(outV, v)
		return ""
	}
	for i, n := range names {
		v := vals[i]
		switch n {
		case "type":
			var s string
			if json.Unmarshal(v, &s) != nil || s != "Feature" {
				return nil, nil, fmt.Sprintf("member type is %s; for feature objects, the value must be Feature", v)
			}
			sawType = true
			continue
		case "geometry":
			if string(v) != "null" {
				gn, _, msg := objectMembers(v)
				if msg != "" || !containsName(gn, "type") {
					return nil, nil, "member geometry is not a GeoJSON Geometry object or null"
				}
				b, _ := json.Marshal(string(v))
				v = b
			}
		case "id":
			if t := firstByte(v); t != '"' && t != '-' && (t < '0' || t > '9') {
				return nil, nil, fmt.Sprintf("member id is %s; it is either a string or a number", v)
			}
		case "properties":
			if string(v) == "null" {
				continue
			}
			pn, pv, msg := objectMembers(v)
			if msg != "" {
				return nil, nil, "member properties is not a JSON object or null"
			}
			for j, p := range pn {
				if msg := add(p, pv[j]); msg != "" {
					return nil, nil, msg
				}
			}
			continue
		}
		if msg := add(n, v); msg != "" {
			return nil, nil, msg
		}
	}
	if !sawType {
		return nil, nil, "no member type; for feature objects, the value must be Feature"
	}
	return outN, outV, ""
}

func containsName(names []string, n string) bool {
	for _, o := range names {
		if o == n {
			return true
		}
	}
	return false
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// hiveFiles are a hive-partitioned Parquet or Avro load's keys, and each file's
// values of them by its gs:// URI, kept in its request's context.
type hiveFiles struct {
	keys []hiveKey
	vals map[string][]string
}

type hiveFilesKey struct{}

// hiveFilesOf is the hiveFiles of r's load, or nil.
func hiveFilesOf(r *http.Request) *hiveFiles {
	h, _ := r.Context().Value(hiveFilesKey{}).(*hiveFiles)
	return h
}

// fields are the keys as NULLABLE columns.
func (h *hiveFiles) fields() []field {
	out := make([]field, len(h.keys))
	for i, k := range h.keys {
		out[i] = field{Name: k.name, Type: k.typ, Mode: "NULLABLE"}
	}
	return out
}

// literal is the SQL literal of key name's value in the file at uri, and
// whether name is a key.
func (h *hiveFiles) literal(uri, name string) (string, bool) {
	for i, k := range h.keys {
		if !strings.EqualFold(k.name, name) {
			continue
		}
		v := h.vals[uri][i]
		switch k.typ {
		case "INTEGER":
			return v, true
		case "DATE":
			return "DATE " + sqlString(v), true
		case "TIMESTAMP":
			b, _ := hiveValue(v, k.typ)
			var ts string
			_ = json.Unmarshal(b, &ts)
			return "TIMESTAMP " + sqlString(ts), true
		}
		return sqlString(v), true
	}
	return "", false
}
