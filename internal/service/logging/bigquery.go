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
//     sourceLocation, split). Fields users name (jsonPayload's, labels',
//     resource labels') are lowercased, with unsupported characters made
//     underscores and anything before the first letter or digit dropped, at
//     most 128 characters. A top-level @type of type.googleapis.com/abc.Xyz
//     names the payload column jsonpayload_abc_xyz.
//   - A JSON string is STRING, a number FLOAT, a bool BOOLEAN, an object a
//     RECORD and a list REPEATED of its first element's type; null and empty
//     objects and lists add no column.
//   - "The first log entry received by BigQuery determines the schema"; a
//     later entry's new fields are added. An entry that would change a
//     field's type is a schema mismatch: Google writes it to an
//     export_errors table, whose schema it does not document, so here it is
//     not written and the sink records the error.
//   - protoPayload is not exported (its mapping is per message type).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/types/known/structpb"
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
			return nil, fmt.Errorf("%w: field %s is %s in the table, not %s", errSchemaMismatch, w.Name, describe(h), describe(w))
		}
		if h.Type == "RECORD" {
			m, err := mergeFields(h.Fields, w.Fields)
			if err != nil {
				return nil, fmt.Errorf("%s.%w", w.Name, err)
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

// streamToBigQuery streams entries into their tables, making and growing
// each, and counts the rows written. An entry with a schema mismatch is
// left out; the first error is returned after the rest are written.
func streamToBigQuery(ctx context.Context, w TableWriter, project, dataset string, partitioned bool, entries []*loggingpb.LogEntry) (int, error) {
	type table struct {
		rows []row
		ids  []string
	}
	tables := map[string]*table{}
	var order []string
	for _, e := range entries {
		n := tableName(e, partitioned)
		t := tables[n]
		if t == nil {
			t = &table{}
			tables[n] = t
			order = append(order, n)
		}
		t.rows = append(t.rows, entryRow(e))
		t.ids = append(t.ids, e.GetInsertId())
	}
	var first error
	note := func(err error) {
		if first == nil {
			first = err
		}
	}
	written := 0
	for _, n := range order {
		t := tables[n]
		have, found, err := w.TableSchema(ctx, project, dataset, n)
		if err != nil {
			note(err)
			continue
		}
		schema := have
		var rows []map[string]any
		var ids []string
		for i, r := range t.rows {
			m, err := mergeFields(schema, r.fields)
			if err != nil {
				note(fmt.Errorf("table %s, entry %s: %w", n, t.ids[i], err))
				continue
			}
			schema = m
			rows = append(rows, r.values)
			ids = append(ids, t.ids[i])
		}
		if len(rows) == 0 {
			continue
		}
		switch {
		case !found:
			err = w.CreateTable(ctx, project, dataset, n, schema, partitioned)
		case !sameFields(have, schema):
			err = w.ExtendTable(ctx, project, dataset, n, schema)
		}
		if err == nil {
			err = w.InsertRows(ctx, project, dataset, n, rows, ids)
		}
		if err != nil {
			note(fmt.Errorf("table %s: %w", n, err))
			continue
		}
		written += len(rows)
	}
	return written, first
}
