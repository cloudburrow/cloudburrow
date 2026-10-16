package logging

// Log Analytics linked datasets (#1423), to
// docs.cloud.google.com/logging/docs/analyze/query-linked-dataset: "A
// linked BigQuery dataset is a read-only pointer to your log bucket", and
// "any log views within the log bucket are automatically mapped to virtual
// views in the dataset", each named as its log view.
//
// Here CreateLink makes, in the instance's BigQuery, the dataset the link
// names, in the bucket's project, with:
//
//   - a table the Logging server fills, LinkedEntriesTable, which holds the
//     bucket's entries in Log Analytics' schema (linkedSchema): every entry
//     the bucket held when the link was made, and, as each is written,
//     every entry written to it after;
//   - the view _AllLogs, SELECT * of that table, which is what a query
//     names. Log views with a filter are not mapped (a Logging filter is
//     not translated into SQL here); only _AllLogs is made.
//
// An entry reaches the table by a streaming insert made in the background
// after WriteLogEntries answers, as sink delivery is (deliver.go): it is
// queryable once that insert is done, typically well under a second.
// Google documents no delay of its own beyond "it might take several
// minutes before BigQuery recognizes the dataset".
//
// The dataset is read-only to clients: the BigQuery front refuses every
// write to a dataset with the LinkedDatasetLabel label but the Logging
// server's own (bigqueryfront/loglinks.go). DeleteLink deletes the dataset.
// A bucket's retention and a view's deletion are not applied to the rows
// already in the table.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// LinkedEntriesTable is the table of a linked dataset that holds the
// bucket's entries; the _AllLogs view selects from it.
const LinkedEntriesTable = "_cloudburrow_log_entries"

// LinkedDatasetLabel marks a dataset as a Log Analytics linked dataset,
// which the BigQuery front keeps read-only; its value is "true".
const LinkedDatasetLabel = "cloudburrow_logging_link"

// LinkedWriterHeader is the header the Logging server's own requests to
// BigQuery carry, the only writes the front lets into a linked dataset.
const LinkedWriterHeader = "X-Cloudburrow-Logging-Link"

// ErrDatasetExists is what LinkedDatasets.Create wraps when the dataset
// already exists.
var ErrDatasetExists = errors.New("dataset already exists")

// ErrProjectNotServed is what LinkedDatasets.Create wraps when the
// instance's BigQuery does not serve the project.
var ErrProjectNotServed = errors.New("project not served by BigQuery")

// LinkedDatasets makes and fills linked datasets in the instance's
// BigQuery.
type LinkedDatasets interface {
	// Create makes dataset in project with LinkedEntriesTable, of
	// linkedSchema's fields, and the _AllLogs view over it.
	Create(ctx context.Context, project, dataset, description string, fields []Field) error
	// Append streams rows into the dataset's LinkedEntriesTable.
	Append(ctx context.Context, project, dataset string, rows []map[string]any, insertIDs []string) error
	// Delete deletes the dataset and what it holds.
	Delete(ctx context.Context, project, dataset string) error
}

// UseLinkedDatasets sets where links make their datasets; nil (the
// default) is an instance without BigQuery, where CreateLink is refused.
func (a *Admin) UseLinkedDatasets(l LinkedDatasets) {
	a.mu.Lock()
	a.linked = l
	a.mu.Unlock()
}

// linkedDataset is the BigQuery project and dataset ID of a link.
func linkedDataset(l *loggingpb.Link) (project, dataset string) {
	// bigquery.googleapis.com/projects/P/datasets/D
	parts := strings.Split(l.GetBigqueryDataset().GetDatasetId(), "/")
	if len(parts) != 5 {
		return "", ""
	}
	return parts[2], parts[4]
}

// linkedSchema is Log Analytics' schema of a log view, as its "Schema"
// reference lists it: LogEntry's fields in snake_case, labels, the
// resource's labels and the payloads as JSON.
func linkedSchema() []Field {
	s := func(n string) Field { return Field{Name: n, Type: "STRING"} }
	i := func(n string) Field { return Field{Name: n, Type: "INTEGER"} }
	b := func(n string) Field { return Field{Name: n, Type: "BOOLEAN"} }
	j := func(n string) Field { return Field{Name: n, Type: "JSON"} }
	r := func(n string, fs ...Field) Field { return Field{Name: n, Type: "RECORD", Fields: fs} }
	return []Field{
		{Name: "timestamp", Type: "TIMESTAMP"},
		{Name: "receive_timestamp", Type: "TIMESTAMP"},
		s("log_name"), s("log_id"), s("severity"), i("severity_number"), s("insert_id"),
		r("resource", s("type"), j("labels")),
		j("labels"), j("json_payload"), j("proto_payload"), s("text_payload"),
		r("http_request", s("request_method"), s("request_url"), i("request_size"), i("status"), i("response_size"),
			s("user_agent"), s("remote_ip"), s("server_ip"), s("referer"), Field{Name: "latency", Type: "FLOAT"},
			b("cache_lookup"), b("cache_hit"), b("cache_validated_with_origin_server"), i("cache_fill_bytes"), s("protocol")),
		r("operation", s("id"), s("producer"), b("first"), b("last")),
		s("trace"), s("span_id"), b("trace_sampled"),
		r("source_location", s("file"), i("line"), s("function")),
		r("split", s("uid"), i("index"), i("total_splits")),
	}
}

// jsonOf is a protobuf JSON value as plain Go values, for a JSON column.
func jsonOf(m interface {
	GetFields() map[string]*structpb.Value
}) map[string]any {
	if m == nil || len(m.GetFields()) == 0 {
		return nil
	}
	out := make(map[string]any, len(m.GetFields()))
	for k, v := range m.GetFields() {
		out[k] = v.AsInterface()
	}
	return out
}

func stringMap(m map[string]string) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// linkedRow is e as a row of linkedSchema; fields with no value are left
// out (NULL).
func linkedRow(e *loggingpb.LogEntry) map[string]any {
	row := map[string]any{
		"timestamp": e.GetTimestamp().AsTime().UTC().Format(bqTime),
		"log_name":  e.GetLogName(),
		"severity":  e.GetSeverity().String(),
		// LogSeverity's numbers: DEFAULT 0, DEBUG 100, ... EMERGENCY 800.
		"severity_number": int64(e.GetSeverity()),
		"insert_id":       e.GetInsertId(),
	}
	if e.GetReceiveTimestamp() != nil {
		row["receive_timestamp"] = e.GetReceiveTimestamp().AsTime().UTC().Format(bqTime)
	}
	if _, id, ok := strings.Cut(e.GetLogName(), "/logs/"); ok {
		row["log_id"] = unescapeLogID(id)
	}
	if res := e.GetResource(); res != nil {
		r := map[string]any{"type": res.GetType()}
		if l := stringMap(res.GetLabels()); l != nil {
			r["labels"] = l
		}
		row["resource"] = r
	}
	if l := stringMap(e.GetLabels()); l != nil {
		row["labels"] = l
	}
	switch p := e.GetPayload().(type) {
	case *loggingpb.LogEntry_TextPayload:
		row["text_payload"] = p.TextPayload
	case *loggingpb.LogEntry_JsonPayload:
		if m := jsonOf(p.JsonPayload); m != nil {
			row["json_payload"] = m
		} else {
			row["json_payload"] = map[string]any{}
		}
	case *loggingpb.LogEntry_ProtoPayload:
		var m map[string]any
		if raw, err := protojson.Marshal(p.ProtoPayload); err == nil && json.Unmarshal(raw, &m) == nil {
			row["proto_payload"] = m
		}
	}
	if h := e.GetHttpRequest(); h != nil {
		hr := map[string]any{}
		str := func(n, v string) {
			if v != "" {
				hr[n] = v
			}
		}
		num := func(n string, v int64) {
			if v != 0 {
				hr[n] = v
			}
		}
		str("request_method", h.GetRequestMethod())
		str("request_url", h.GetRequestUrl())
		num("request_size", h.GetRequestSize())
		num("status", int64(h.GetStatus()))
		num("response_size", h.GetResponseSize())
		str("user_agent", h.GetUserAgent())
		str("remote_ip", h.GetRemoteIp())
		str("server_ip", h.GetServerIp())
		str("referer", h.GetReferer())
		if l := h.GetLatency(); l != nil {
			hr["latency"] = l.AsDuration().Seconds()
		}
		if h.GetCacheLookup() {
			hr["cache_lookup"] = true
		}
		if h.GetCacheHit() {
			hr["cache_hit"] = true
		}
		if h.GetCacheValidatedWithOriginServer() {
			hr["cache_validated_with_origin_server"] = true
		}
		num("cache_fill_bytes", h.GetCacheFillBytes())
		str("protocol", h.GetProtocol())
		row["http_request"] = hr
	}
	if o := e.GetOperation(); o != nil {
		row["operation"] = map[string]any{"id": o.GetId(), "producer": o.GetProducer(), "first": o.GetFirst(), "last": o.GetLast()}
	}
	if e.GetTrace() != "" {
		row["trace"] = e.GetTrace()
	}
	if e.GetSpanId() != "" {
		row["span_id"] = e.GetSpanId()
	}
	if e.GetTraceSampled() {
		row["trace_sampled"] = true
	}
	if l := e.GetSourceLocation(); l != nil {
		row["source_location"] = map[string]any{"file": l.GetFile(), "line": l.GetLine(), "function": l.GetFunction()}
	}
	if sp := e.GetSplit(); sp != nil {
		row["split"] = map[string]any{"uid": sp.GetUid(), "index": int64(sp.GetIndex()), "total_splits": int64(sp.GetTotalSplits())}
	}
	return row
}

func unescapeLogID(id string) string {
	// A log ID is URL-encoded in the log name: compute.googleapis.com%2Factivity_log.
	return strings.NewReplacer("%2F", "/", "%2f", "/").Replace(id)
}

// appendLinked streams es into the dataset of each link.
func appendLinked(ctx context.Context, l LinkedDatasets, links []*loggingpb.Link, es []*loggingpb.LogEntry) error {
	if len(es) == 0 {
		return nil
	}
	rows := make([]map[string]any, len(es))
	ids := make([]string, len(es))
	for i, e := range es {
		rows[i], ids[i] = linkedRow(e), e.GetInsertId()
	}
	var first error
	for _, link := range links {
		project, dataset := linkedDataset(link)
		if dataset == "" {
			continue
		}
		for start := 0; start < len(rows); start += 500 { // insertAll's recommended batch
			end := min(start+500, len(rows))
			if err := l.Append(ctx, project, dataset, rows[start:end], ids[start:end]); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// linksOf returns the links of each bucket (by name) any of the routed
// entries is in, reading each bucket's links once.
func (a *Admin) linksOf(buckets map[string]bucketRef) map[string][]*loggingpb.Link {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.linked == nil {
		return nil
	}
	out := map[string][]*loggingpb.Link{}
	for name, ref := range buckets {
		if ls, err := a.links(ref); err == nil && len(ls) > 0 {
			out[name] = ls
		}
	}
	return out
}

// linkWrites streams the entries just stored into the linked datasets of
// the buckets they are in, in the background.
func (s *Server) linkWrites(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) {
	if s.admin == nil || len(es) == 0 {
		return
	}
	s.admin.mu.Lock()
	l := s.admin.linked
	s.admin.mu.Unlock()
	if l == nil {
		return
	}
	// The buckets each entry is in: its parent's _Default unless an
	// exclusion kept it out, and those sinks routed it to.
	in := map[*loggingpb.LogEntry][]string{}
	buckets := map[string]bucketRef{}
	for _, e := range es {
		r := routes[e]
		if r == nil || !r.hidden {
			ref := bucketRef{entryParent(e), "global", bucketDefault}
			buckets[ref.name()] = ref
			in[e] = append(in[e], ref.name())
		}
		if r != nil {
			for _, b := range r.buckets {
				if ref, err := parseBucket("bucket", b); err == nil {
					buckets[b] = ref
					in[e] = append(in[e], b)
				}
			}
		}
	}
	links := s.admin.linksOf(buckets)
	if len(links) == 0 {
		return
	}
	byBucket := map[string][]*loggingpb.LogEntry{}
	for _, e := range es {
		for _, b := range in[e] {
			if links[b] != nil {
				byBucket[b] = append(byBucket[b], e)
			}
		}
	}
	s.d.wg.Add(1)
	go func() {
		defer s.d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.d.linkMu.Lock()
		defer s.d.linkMu.Unlock()
		for b, entries := range byBucket {
			_ = appendLinked(ctx, l, links[b], entries)
		}
	}()
}
