package logging

// The BigQuery sink's tables and rows (#1404), to
// docs.cloud.google.com/logging/docs/export/bigquery:
//
//   - The table is the log ID with every character but letters, digits and
//     underscores made an underscore ("apache-access" is apache_access,
//     compute.googleapis.com/activity_log compute_googleapis_com_activity_log),
//     suffixed _YYYYMMDD with the entry's UTC date unless the sink uses
//     partitioned tables, whose one table is partitioned on timestamp.
//   - LogEntry fields keep their names (logName, resource, textPayload,
//     jsonPayload, timestamp, receiveTimestamp, severity, insertId,
//     httpRequest, labels, operation, trace, spanId, traceSampled,
//     sourceLocation, split); httpRequest's cache flags are BOOLEAN and
//     cacheFillBytes INTEGER, as its other counts (#1451), and its latency
//     FLOAT seconds (#1506): Google's Log Analytics migration guide
//     (cloud.google.com/blog/products/data-analytics/moving-to-log-analytics-for-bigquery-export-users)
//     names httpRequest.latency's sink type FLOAT and has it read as
//     latency.nanos / POW(10,9) after, so every Duration is FLOAT
//     seconds here. Fields users name (jsonPayload's, labels',
//     resource labels') are lowercased, with unsupported characters made
//     underscores and anything before the first letter or digit dropped, at
//     most 128 characters. A top-level @type of type.googleapis.com/abc.Xyz
//     names the payload column jsonpayload_abc_xyz.
//   - A JSON string is STRING, a number FLOAT, a bool BOOLEAN, an object a
//     RECORD and a list REPEATED of its first element's type; null and empty
//     objects and lists add no column.
//   - "The first log entry received by BigQuery determines the schema"; a
//     later entry's new fields are added. An entry that would change a
//     field's type is a schema mismatch, and when a batch's new fields would
//     pass BigQuery's 10,000 columns every entry of the batch is: as Google
//     documents (#1451), those entries are written to an error table in the
//     same dataset, export_errors_YYYYMMDD of the entry's UTC date or, with
//     partitioned tables, export_errors, whose columns are logEntry (the
//     LogEntry JSON as a string), schemaErrorDetail, sink (the sink's
//     resource name) and the entry's logName, timestamp, receiveTimestamp,
//     severity, insertId, trace and resourceType; its first row makes its
//     schema, as Google says. The column types are UNVERIFIED (STRING, and
//     TIMESTAMP for the two times).
//   - protoPayload (#1451): a top-level @type of type.googleapis.com/abc.Xyz
//     names the column protopayload_abc_xyz, with the types Google shortens
//     (google.cloud.audit.AuditLog is AuditLog, so protopayload_auditlog;
//     google.cloud.bigquery.logging.v1.AuditData v1.bigquery, ...); an App
//     Engine RequestLog's column is protoPayload. Its fields are the
//     message's: an audit log's keep their JSON names, its request,
//     response and metadata are JSON strings named requestJson,
//     responseJson and metadataJson, and its serviceData is
//     servicedata_<shortened type>; other types' fields are lowercased, as
//     Google's @type table shows; a Duration is FLOAT seconds, as
//     httpRequest.latency is. A type the instance does not know and Any
//     fields but serviceData are left out: Google documents no column for
//     them (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/logging/docs/export/bigquery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	// The payload types Google's BigQuery export names (#1451), so their
	// Any values resolve.
	_ "google.golang.org/genproto/googleapis/appengine/legacy"
	_ "google.golang.org/genproto/googleapis/appengine/logging/v1"
	_ "google.golang.org/genproto/googleapis/cloud/audit"
	_ "google.golang.org/genproto/googleapis/cloud/bigquery/logging/v1"
	_ "google.golang.org/genproto/googleapis/iam/v1/logging"
)

// Field is one BigQuery column. Type is STRING, INTEGER, FLOAT, BOOLEAN,
// TIMESTAMP or RECORD.
type Field struct {
	Name     string
	Type     string
	Repeated bool
	Fields   []Field
}

var errSchemaMismatch = errors.New("schema mismatch")

// mismatchError is a field whose type an entry would change, worded as
// BigQuery words a schema update that changes a type.
type mismatchError struct {
	path       string
	have, want Field
}

func (e *mismatchError) Error() string {
	return fmt.Sprintf("Field %s has changed type from %s to %s", e.path, describe(e.have), describe(e.want))
}

func (e *mismatchError) Is(target error) bool { return target == errSchemaMismatch }

var notColumnChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// tableName is the table a sink streams e into.
func tableName(e *loggingpb.LogEntry, partitioned bool) string {
	_, logID, _ := resource.ParseLogName(e.GetLogName())
	if u, err := url.PathUnescape(logID); err == nil {
		logID = u
	}
	t := notColumnChar.ReplaceAllString(logID, "_")
	if !partitioned {
		t += "_" + e.GetTimestamp().AsTime().UTC().Format("20060102")
	}
	return t
}

// userField normalizes a field name a user chose; "" is no column.
func userField(name string) string {
	n := strings.ToLower(notColumnChar.ReplaceAllString(name, "_"))
	n = strings.TrimLeftFunc(n, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	if len(n) > 128 {
		n = n[:128]
	}
	return n
}

// row is one entry as a row and the fields it needs.
type row struct {
	values map[string]any
	fields []Field
}

func (r *row) set(name, typ string, v any) {
	r.values[name] = v
	r.fields = append(r.fields, Field{Name: name, Type: typ})
}

func (r *row) record(name string, sub row) {
	if len(sub.fields) == 0 {
		return
	}
	r.values[name] = sub.values
	r.fields = append(r.fields, Field{Name: name, Type: "RECORD", Fields: sub.fields})
}

func newRow() row { return row{values: map[string]any{}} }

func stringLabels(m map[string]string) row {
	r := newRow()
	for k, v := range m {
		if n := userField(k); n != "" {
			r.set(n, "STRING", v)
		}
	}
	return r
}

const bqTime = "2006-01-02 15:04:05.999999 UTC"

// entryRow maps e onto the export schema.
func entryRow(e *loggingpb.LogEntry) row {
	r := newRow()
	r.set("logName", "STRING", e.GetLogName())
	if res := e.GetResource(); res != nil {
		sub := newRow()
		sub.set("type", "STRING", res.GetType())
		sub.record("labels", stringLabels(res.GetLabels()))
		r.record("resource", sub)
	}
	switch p := e.GetPayload().(type) {
	case *loggingpb.LogEntry_TextPayload:
		r.set("textPayload", "STRING", p.TextPayload)
	case *loggingpb.LogEntry_JsonPayload:
		fields := p.JsonPayload.GetFields()
		name := "jsonPayload"
		if t := fields["@type"].GetStringValue(); t != "" {
			name = "jsonpayload_" + userField(strings.TrimPrefix(t, "type.googleapis.com/"))
		}
		sub := newRow()
		for k, v := range fields {
			if k == "@type" {
				continue
			}
			jsonField(&sub, k, v)
		}
		r.record(name, sub)
	case *loggingpb.LogEntry_ProtoPayload:
		protoPayload(&r, p.ProtoPayload)
	}
	r.set("timestamp", "TIMESTAMP", e.GetTimestamp().AsTime().UTC().Format(bqTime))
	if e.GetReceiveTimestamp() != nil {
		r.set("receiveTimestamp", "TIMESTAMP", e.GetReceiveTimestamp().AsTime().UTC().Format(bqTime))
	}
	r.set("severity", "STRING", e.GetSeverity().String())
	r.set("insertId", "STRING", e.GetInsertId())
	if h := e.GetHttpRequest(); h != nil {
		sub := newRow()
		str := func(n, v string) {
			if v != "" {
				sub.set(n, "STRING", v)
			}
		}
		num := func(n string, v int64) {
			if v != 0 {
				sub.set(n, "INTEGER", v)
			}
		}
		str("requestMethod", h.GetRequestMethod())
		str("requestUrl", h.GetRequestUrl())
		num("requestSize", h.GetRequestSize())
		num("status", int64(h.GetStatus()))
		num("responseSize", h.GetResponseSize())
		str("userAgent", h.GetUserAgent())
		str("remoteIp", h.GetRemoteIp())
		str("serverIp", h.GetServerIp())
		str("referer", h.GetReferer())
		str("protocol", h.GetProtocol())
		flag := func(n string, v bool) {
			if v {
				sub.set(n, "BOOLEAN", true)
			}
		}
		flag("cacheLookup", h.GetCacheLookup())
		flag("cacheHit", h.GetCacheHit())
		flag("cacheValidatedWithOriginServer", h.GetCacheValidatedWithOriginServer())
		num("cacheFillBytes", h.GetCacheFillBytes())
		if l := h.GetLatency(); l != nil {
			sub.set("latency", "FLOAT", l.AsDuration().Seconds())
		}
		r.record("httpRequest", sub)
	}
	r.record("labels", stringLabels(e.GetLabels()))
	if o := e.GetOperation(); o != nil {
		sub := newRow()
		sub.set("id", "STRING", o.GetId())
		sub.set("producer", "STRING", o.GetProducer())
		sub.set("first", "BOOLEAN", o.GetFirst())
		sub.set("last", "BOOLEAN", o.GetLast())
		r.record("operation", sub)
	}
	if e.GetTrace() != "" {
		r.set("trace", "STRING", e.GetTrace())
	}
	if e.GetSpanId() != "" {
		r.set("spanId", "STRING", e.GetSpanId())
	}
	if e.GetTraceSampled() {
		r.set("traceSampled", "BOOLEAN", true)
	}
	if l := e.GetSourceLocation(); l != nil {
		sub := newRow()
		sub.set("file", "STRING", l.GetFile())
		sub.set("line", "INTEGER", l.GetLine())
		sub.set("function", "STRING", l.GetFunction())
		r.record("sourceLocation", sub)
	}
	if sp := e.GetSplit(); sp != nil {
		sub := newRow()
		sub.set("uid", "STRING", sp.GetUid())
		sub.set("index", "INTEGER", int64(sp.GetIndex()))
		sub.set("totalSplits", "INTEGER", int64(sp.GetTotalSplits()))
		r.record("split", sub)
	}
	return r
}

// jsonValue maps one JSON value: its column type, value and subfields.
func jsonValue(v *structpb.Value) (typ string, val any, sub []Field, ok bool) {
	switch x := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return "STRING", x.StringValue, nil, true
	case *structpb.Value_NumberValue:
		return "FLOAT", x.NumberValue, nil, true
	case *structpb.Value_BoolValue:
		return "BOOLEAN", x.BoolValue, nil, true
	case *structpb.Value_StructValue:
		r := newRow()
		for k, fv := range x.StructValue.GetFields() {
			jsonField(&r, k, fv)
		}
		if len(r.fields) == 0 {
			return "", nil, nil, false
		}
		return "RECORD", r.values, r.fields, true
	}
	return "", nil, nil, false
}

func jsonField(r *row, name string, v *structpb.Value) {
	n := userField(name)
	if n == "" {
		return
	}
	if l, isList := v.GetKind().(*structpb.Value_ListValue); isList {
		var typ string
		var sub []Field
		var vals []any
		for _, el := range l.ListValue.GetValues() {
			t, val, s, ok := jsonValue(el)
			if !ok {
				continue
			}
			if typ == "" {
				typ, sub = t, s
			} else if t != typ {
				continue
			} else if t == "RECORD" {
				sub, _ = mergeFields(sub, s)
			}
			vals = append(vals, val)
		}
		if typ == "" {
			return
		}
		r.values[n] = vals
		r.fields = append(r.fields, Field{Name: n, Type: typ, Repeated: true, Fields: sub})
		return
	}
	t, val, sub, ok := jsonValue(v)
	if !ok {
		return
	}
	r.values[n] = val
	r.fields = append(r.fields, Field{Name: n, Type: t, Fields: sub})
}

// mergeFields adds want's fields to have, or reports the first whose type
// would change.
func mergeFields(have, want []Field) ([]Field, error) {
	out := append([]Field(nil), have...)
	for _, w := range want {
		i := -1
		for j := range out {
			if strings.EqualFold(out[j].Name, w.Name) {
				i = j
				break
			}
		}
		if i < 0 {
			out = append(out, w)
			continue
		}
		h := out[i]
		if h.Type != w.Type || h.Repeated != w.Repeated {
			return nil, &mismatchError{path: w.Name, have: h, want: w}
		}
		if h.Type == "RECORD" {
			m, err := mergeFields(h.Fields, w.Fields)
			if err != nil {
				var me *mismatchError
				if errors.As(err, &me) {
					me.path = w.Name + "." + me.path
				}
				return nil, err
			}
			h.Fields = m
			out[i] = h
		}
	}
	return out, nil
}

func describe(f Field) string {
	if f.Repeated {
		return "REPEATED " + f.Type
	}
	return f.Type
}

func sameFields(a, b []Field) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Type != b[i].Type || a[i].Repeated != b[i].Repeated || !sameFields(a[i].Fields, b[i].Fields) {
			return false
		}
	}
	return true
}

// maxColumns is BigQuery's limit of columns in a table's schema, nested
// ones included.
const maxColumns = 10000

func countColumns(fs []Field) int {
	n := len(fs)
	for _, f := range fs {
		n += countColumns(f.Fields)
	}
	return n
}

// failed is an entry written to the error table, and why.
type failed struct {
	e      *loggingpb.LogEntry
	detail string
}

// errorsTable is the error table of an entry's table.
func errorsTable(e *loggingpb.LogEntry, partitioned bool) string {
	if partitioned {
		return "export_errors"
	}
	return "export_errors_" + e.GetTimestamp().AsTime().UTC().Format("20060102")
}

// errorRow is the error table's row for f, written by sink.
func errorRow(f failed, sink string) row {
	r := newRow()
	js, _ := protojson.Marshal(f.e)
	r.set("logEntry", "STRING", string(js))
	r.set("schemaErrorDetail", "STRING", f.detail)
	r.set("sink", "STRING", sink)
	r.set("logName", "STRING", f.e.GetLogName())
	r.set("timestamp", "TIMESTAMP", f.e.GetTimestamp().AsTime().UTC().Format(bqTime))
	if f.e.GetReceiveTimestamp() != nil {
		r.set("receiveTimestamp", "TIMESTAMP", f.e.GetReceiveTimestamp().AsTime().UTC().Format(bqTime))
	}
	r.set("severity", "STRING", f.e.GetSeverity().String())
	r.set("insertId", "STRING", f.e.GetInsertId())
	if f.e.GetTrace() != "" {
		r.set("trace", "STRING", f.e.GetTrace())
	}
	if t := f.e.GetResource().GetType(); t != "" {
		r.set("resourceType", "STRING", t)
	}
	return r
}

// tableRows are the rows streamed into one table.
type tableRows struct {
	rows    []row
	ids     []string
	entries []*loggingpb.LogEntry
}

// writeTable makes or grows a table for rows and streams them. Rows whose
// fields would change a column's type, or every row when the schema would
// pass maxColumns, are returned instead of written.
func writeTable(ctx context.Context, w TableWriter, project, dataset, name string, partitioned bool, t *tableRows) (int, []failed, error) {
	have, found, err := w.TableSchema(ctx, project, dataset, name)
	if err != nil {
		return 0, nil, err
	}
	schema := have
	var rows []map[string]any
	var ids []string
	var bad []failed
	for i, r := range t.rows {
		m, err := mergeFields(schema, r.fields)
		if err != nil {
			bad = append(bad, failed{t.entries[i], fmt.Sprintf("Provided Schema does not match Table %s:%s.%s. %v.", project, dataset, name, err)})
			continue
		}
		schema = m
		rows = append(rows, r.values)
		ids = append(ids, t.ids[i])
	}
	if n := countColumns(schema); n > maxColumns {
		bad = bad[:0]
		for _, e := range t.entries {
			bad = append(bad, failed{e, fmt.Sprintf("Too many total leaf fields: %d, max allowed field count: %d", n, maxColumns)})
		}
		return 0, bad, nil
	}
	if len(rows) == 0 {
		return 0, bad, nil
	}
	switch {
	case !found:
		err = w.CreateTable(ctx, project, dataset, name, schema, partitioned)
	case !sameFields(have, schema):
		err = w.ExtendTable(ctx, project, dataset, name, schema)
	}
	if err == nil {
		err = w.InsertRows(ctx, project, dataset, name, rows, ids)
	}
	if err != nil {
		return 0, bad, fmt.Errorf("table %s: %w", name, err)
	}
	return len(rows), bad, nil
}

// streamToBigQuery streams entries into their tables, making and growing
// each, and counts the rows written. An entry with a schema mismatch is
// written to the dataset's error table instead, and reported as
// errSchemaMismatch; the first error is returned after the rest are
// written. It counts the rows written to their tables and to the error
// table.
func streamToBigQuery(ctx context.Context, w TableWriter, sink, project, dataset string, partitioned bool, entries []*loggingpb.LogEntry) (written, rejected int, err error) {
	tables := map[string]*tableRows{}
	var order []string
	add := func(n string, r row, e *loggingpb.LogEntry) {
		t := tables[n]
		if t == nil {
			t = &tableRows{}
			tables[n] = t
			order = append(order, n)
		}
		t.rows = append(t.rows, r)
		t.ids = append(t.ids, e.GetInsertId())
		t.entries = append(t.entries, e)
	}
	for _, e := range entries {
		add(tableName(e, partitioned), entryRow(e), e)
	}
	var first error
	note := func(err error) {
		if first == nil {
			first = err
		}
	}
	var bad []failed
	for _, n := range order {
		k, b, err := writeTable(ctx, w, project, dataset, n, partitioned, tables[n])
		written += k
		bad = append(bad, b...)
		if err != nil {
			note(err)
		}
	}
	if len(bad) == 0 {
		return written, 0, first
	}
	note(fmt.Errorf("%w: %d entries written to the error table: %s", errSchemaMismatch, len(bad), bad[0].detail))
	tables, order = map[string]*tableRows{}, nil
	for _, f := range bad {
		add(errorsTable(f.e, partitioned), errorRow(f, sink), f.e)
	}
	for _, n := range order {
		k, _, err := writeTable(ctx, w, project, dataset, n, partitioned, tables[n])
		rejected += k
		if err != nil {
			note(err)
		}
	}
	return written, rejected, first
}

// auditLogType is the audit log payload, whose fields Google maps by name.
const auditLogType = "google.cloud.audit.AuditLog"

// shortTypes are the payload types Google shortens in column names
// (docs.cloud.google.com/logging/docs/export/bigquery, audit log fields).
var shortTypes = map[string]string{
	auditLogType:                                 "AuditLog",
	"google.appengine.legacy.AuditData":          "legacy.appengine",
	"google.appengine.v1alpha.AuditData":         "v1alpha.appengine",
	"google.appengine.v1beta.AuditData":          "v1beta.appengine",
	"google.appengine.v1beta4.AuditData":         "v1beta4.appengine",
	"google.appengine.v1beta5.AuditData":         "v1beta5.appengine",
	"google.appengine.v1.AuditData":              "v1.appengine",
	"google.cloud.bigquery.logging.v1.AuditData": "v1.bigquery",
	"google.iam.v1.logging.AuditData":            "v1.iam",
	"google.iam.admin.v1.AuditData":              "v1.iam.admin",
	"google.type.Money":                          "money",
}

// typeColumn is the column suffix of a payload type: its shortened name,
// lowercased with unsupported characters made underscores.
func typeColumn(full string) string {
	if s, ok := shortTypes[full]; ok {
		full = s
	}
	return userField(full)
}

// protoPayload adds the protoPayload column, or nothing for a type the
// instance does not know.
func protoPayload(r *row, a *anypb.Any) {
	m, err := a.UnmarshalNew()
	if err != nil {
		return
	}
	full := string(m.ProtoReflect().Descriptor().FullName())
	name := "protopayload_" + typeColumn(full)
	if full == "google.appengine.logging.v1.RequestLog" {
		name = "protoPayload"
	}
	audit := full == auditLogType
	r.record(name, messageRow(m.ProtoReflect(), audit, audit))
}

// messageRow maps a message's set fields. keep keeps their JSON names, as
// an audit log's are, else they are lowercased; audit applies the audit
// log's own mappings to m's fields.
func messageRow(m protoreflect.Message, keep, audit bool) row {
	r := newRow()
	fds := m.Descriptor().Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		if !m.Has(fd) {
			continue
		}
		name := fd.JSONName()
		if !keep {
			name = userField(name)
		}
		v := m.Get(fd)
		if audit && fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
			switch {
			case fd.Message().FullName() == "google.protobuf.Struct" && (name == "request" || name == "response" || name == "metadata"):
				js, err := protojson.Marshal(v.Message().Interface())
				if err == nil {
					r.set(name+"Json", "STRING", string(js))
				}
				continue
			case name == "serviceData" && fd.Message().FullName() == "google.protobuf.Any":
				if inner, err := v.Message().Interface().(*anypb.Any).UnmarshalNew(); err == nil {
					r.record("servicedata_"+typeColumn(string(inner.ProtoReflect().Descriptor().FullName())),
						messageRow(inner.ProtoReflect(), true, false))
				}
				continue
			}
		}
		if name == "" {
			continue
		}
		switch {
		case fd.IsMap():
			sub := newRow()
			type kv struct {
				k string
				v protoreflect.Value
			}
			var kvs []kv
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				kvs = append(kvs, kv{k.String(), mv})
				return true
			})
			sort.Slice(kvs, func(i, j int) bool { return kvs[i].k < kvs[j].k })
			for _, x := range kvs {
				if n := userField(x.k); n != "" {
					if t, val, fs, ok := protoValue(fd.MapValue(), x.v, keep); ok {
						sub.values[n] = val
						sub.fields = append(sub.fields, Field{Name: n, Type: t, Fields: fs})
					}
				}
			}
			r.record(name, sub)
		case fd.IsList():
			l := v.List()
			var typ string
			var fs []Field
			var vals []any
			for j := 0; j < l.Len(); j++ {
				t, val, sub, ok := protoValue(fd, l.Get(j), keep)
				if !ok {
					continue
				}
				if typ == "" {
					typ, fs = t, sub
				} else if t == "RECORD" {
					fs, _ = mergeFields(fs, sub)
				}
				vals = append(vals, val)
			}
			if typ != "" {
				r.values[name] = vals
				r.fields = append(r.fields, Field{Name: name, Type: typ, Repeated: true, Fields: fs})
			}
		default:
			if t, val, fs, ok := protoValue(fd, v, keep); ok {
				r.values[name] = val
				r.fields = append(r.fields, Field{Name: name, Type: t, Fields: fs})
			}
		}
	}
	return r
}

// protoValue maps one value of fd's kind.
func protoValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, keep bool) (typ string, val any, sub []Field, ok bool) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return "STRING", v.String(), nil, true
	case protoreflect.BoolKind:
		return "BOOLEAN", v.Bool(), nil, true
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return "STRING", string(ev.Name()), nil, true
		}
		return "INTEGER", int64(v.Enum()), nil, true
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "INTEGER", v.Int(), nil, true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "INTEGER", int64(v.Uint()), nil, true
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "FLOAT", v.Float(), nil, true
	case protoreflect.BytesKind:
		return "STRING", base64.StdEncoding.EncodeToString(v.Bytes()), nil, true
	case protoreflect.MessageKind, protoreflect.GroupKind:
		msg := v.Message().Interface()
		switch x := msg.(type) {
		case *timestamppb.Timestamp:
			return "TIMESTAMP", x.AsTime().UTC().Format(bqTime), nil, true
		case *durationpb.Duration:
			return "FLOAT", x.AsDuration().Seconds(), nil, true
		case *structpb.Struct:
			return jsonValue(structpb.NewStructValue(x))
		case *structpb.Value:
			return jsonValue(x)
		case *structpb.ListValue:
			return "", nil, nil, false // a list of lists has no column
		}
		switch fd.Message().FullName() {
		case "google.protobuf.Any", "google.protobuf.FieldMask", "google.protobuf.Empty":
			return "", nil, nil, false
		}
		if w := wrapperValue(msg); w != nil {
			vf := w.ProtoReflect().Descriptor().Fields().ByName("value")
			return protoValue(vf, w.ProtoReflect().Get(vf), keep)
		}
		r := messageRow(v.Message(), keep, false)
		if len(r.fields) == 0 {
			return "", nil, nil, false
		}
		return "RECORD", r.values, r.fields, true
	}
	return "", nil, nil, false
}

// wrapperValue is msg when it is one of the google.protobuf wrappers.
func wrapperValue(msg proto.Message) proto.Message {
	n := string(msg.ProtoReflect().Descriptor().FullName())
	if strings.HasPrefix(n, "google.protobuf.") && strings.HasSuffix(n, "Value") {
		return msg
	}
	return nil
}
