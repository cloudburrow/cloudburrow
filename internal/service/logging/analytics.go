package logging

// Log Analytics linked datasets (#1423, #1435), to
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
//     the bucket holds, inside its retention, when the link is made, and,
//     as each is written, every entry written to it after. Each row also
//     lists, in LinkedViewsColumn, the bucket's log views whose filter
//     matches it (filter.go evaluates the filter);
//   - a view for each of the bucket's log views, named as it (_AllLogs and
//     each view made with CreateView), which selects the rows its log view
//     holds: those listing it, inside the bucket's retention.
//
// An entry reaches the table by a streaming insert made in the background
// after WriteLogEntries answers, as sink delivery is (deliver.go): it is
// queryable once that insert is done, typically well under a second.
// Google documents no delay of its own beyond "it might take several
// minutes before BigQuery recognizes the dataset".
//
// The dataset follows the bucket (resyncBucket): CreateView, UpdateView
// and DeleteView, a change of the bucket's retention, and DeleteLog of a
// log with entries in it make the dataset again, with the views the bucket
// now has and the entries it now holds, so an entry DeleteLog removed, or
// one past the retention, is gone from its table too. Between those, an
// entry that ages past the retention leaves every view as it leaves the
// bucket, by the views' own timestamp condition. That condition is the
// Logging clock's, not BigQuery's (#1602): Google ages an entry out by its
// `timestamp` ("used to compute the log entry's age and to enforce the
// logs retention period", the LogEntry reference), and here the Logging
// and BigQuery clocks are two, each moved by its own clock:advance. So a
// view reads the Logging clock as CURRENT_TIMESTAMP() less the skew of
// BigQuery's clock over it (LinkedClock), measured as the dataset is made;
// when the skew changes, as either clock is moved, KeepLinkedDatasets (and
// the Logging clock:advance) makes the dataset again. Google documents a
// 7-day grace period after a retention is shortened, in which expired
// entries "aren't deleted" but "You can't query or view" them
// (docs.cloud.google.com/logging/docs/buckets): here they leave the views
// at once, as they leave ListLogEntries. A log view filter
// CloudBurrow does not evaluate is refused UNIMPLEMENTED on a linked bucket
// (by CreateLink, CreateView and UpdateView), since its view could not
// hold what the log view holds.
//
// The datasets outlive restarts (KeepLinkedDatasets): the links are kept
// in the Logging store, and each link's dataset BigQuery no longer has, as
// after an emulator restart (the emulator keeps its data in memory), is
// made again and filled.
//
// The dataset is read-only to clients: the BigQuery front refuses every
// write to a linked dataset but the Logging server's own
// (bigqueryfront/loglinks.go). DeleteLink deletes the dataset.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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

// LinkedViewsColumn is the column of LinkedEntriesTable that lists, for
// each row, the IDs of the bucket's log views that hold the entry.
const LinkedViewsColumn = "_cloudburrow_views"

// LinkedView is one view of a linked dataset: its ID, the log view's, and
// its query.
type LinkedView struct {
	ID, Query string
}

// LinkedClock is a LinkedDatasets that also reads BigQuery's clock, the one
// CURRENT_TIMESTAMP() reads in a view (#1602).
type LinkedClock interface {
	Now(ctx context.Context) (time.Time, error)
}

// LinkedDatasets makes and fills linked datasets in the instance's
// BigQuery.
type LinkedDatasets interface {
	// Create makes dataset in project with LinkedEntriesTable, of
	// fields, and views over it.
	Create(ctx context.Context, project, dataset, description string, fields []Field, views []LinkedView) error
	// Append streams rows into the dataset's LinkedEntriesTable.
	Append(ctx context.Context, project, dataset string, rows []map[string]any, insertIDs []string) error
	// Delete deletes the dataset and what it holds; a missing one is no
	// error.
	Delete(ctx context.Context, project, dataset string) error
	// Exists reports whether the dataset exists.
	Exists(ctx context.Context, project, dataset string) (bool, error)
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
		{Name: LinkedViewsColumn, Type: "STRING", Repeated: true},
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

// linkSync keeps links' datasets in step with their buckets (#1435).
type linkSync struct {
	// mu makes one change of a linked dataset, or one append to it, at a
	// time, in write order.
	mu sync.Mutex
	// snap orders WriteLogEntries' storing of entries against a resync's
	// read of a bucket's entries, and guards gen: a resync bumps a
	// dataset's gen as it reads the entries, so the background append of
	// entries stored before (which the resync wrote) is skipped, and that of
	// entries stored after is not.
	snap sync.RWMutex
	gen  map[string]uint64
	// skew is, by dataset, how far BigQuery's clock was ahead of the
	// Logging clock when its views were made (#1602). Guarded by mu.
	skew map[string]time.Duration
}

// hold read-locks snap, for WriteLogEntries to store entries under, and
// returns the datasets' generations and the release.
func (l *linkSync) hold() (map[string]uint64, func()) {
	l.snap.RLock()
	return maps.Clone(l.gen), l.snap.RUnlock
}

func (l *linkSync) current(key string) uint64 {
	l.snap.RLock()
	defer l.snap.RUnlock()
	return l.gen[key]
}

// bucketView is one of a bucket's log views and its filter, nil when it
// has none.
type bucketView struct {
	id    string
	match pred
}

// bucketViews lists the bucket's log views, in ID order, with their
// filters compiled. The caller holds a.mu.
func (a *Admin) bucketViews(ref bucketRef) ([]bucketView, error) {
	keys, err := a.db.List(viewPrefix(ref))
	if err != nil {
		return nil, apierror.Internal(err, "listing views")
	}
	ids := map[string]bool{}
	if !a.allLogsDeleted(ref) {
		ids[viewAllLogs] = true
	}
	for _, k := range keys {
		if raw, err := hex.DecodeString(strings.TrimPrefix(k, viewPrefix(ref))); err == nil {
			ids[string(raw)] = true
		}
	}
	var out []bucketView
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		v, err := a.getView(ref, id)
		if err != nil {
			continue
		}
		bv := bucketView{id: id}
		if f := strings.TrimSpace(v.GetFilter()); f != "" {
			if bv.match, err = compileWith(f, a.ancestors); err != nil {
				return nil, linkedFilterRefusal(v.GetName(), f, err)
			}
		}
		out = append(out, bv)
	}
	return out, nil
}

func linkedFilterRefusal(view, filter string, err error) error {
	return apierror.Unimplemented("view %s: its filter %q is not one CloudBurrow evaluates, so the bucket's linked BigQuery dataset cannot hold its view: %s",
		view, filter, apierror.From(err).Message)
}

// checkLinkedFilter refuses a log view filter CloudBurrow does not
// evaluate on a bucket with a link. The caller holds a.mu.
func (a *Admin) checkLinkedFilter(ref bucketRef, v *loggingpb.LogView) error {
	f := strings.TrimSpace(v.GetFilter())
	if f == "" || a.linked == nil {
		return nil
	}
	if ls, err := a.links(ref); err != nil || len(ls) == 0 {
		return nil
	}
	if _, err := compileWith(f, a.ancestors); err != nil {
		return linkedFilterRefusal(v.GetName(), f, err)
	}
	return nil
}

// retentionDays is the bucket's retention. The caller holds a.mu.
func (a *Admin) retentionDays(ref bucketRef) int32 {
	if b, err := a.getBucket(ref); err == nil && b.GetRetentionDays() > 0 {
		return b.GetRetentionDays()
	}
	return defaultRetention
}

// linkedColumns are the columns a linked dataset's views select.
func linkedColumns() string {
	var cols []string
	for _, f := range linkedSchema() {
		if f.Name != LinkedViewsColumn {
			cols = append(cols, "`"+f.Name+"`")
		}
	}
	return strings.Join(cols, ", ")
}

// viewQueries are the dataset's views: one per log view, of the rows that
// list it, inside the bucket's retention by the Logging clock, which is
// BigQuery's CURRENT_TIMESTAMP() less skew (#1602).
func viewQueries(project, dataset string, views []bucketView, days int32, skew time.Duration) []LinkedView {
	now := "CURRENT_TIMESTAMP()"
	if s := int64(skew / time.Second); s != 0 {
		now = fmt.Sprintf("TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL %d SECOND)", s)
	}
	out := make([]LinkedView, 0, len(views))
	for _, v := range views {
		out = append(out, LinkedView{ID: v.id, Query: fmt.Sprintf(
			"SELECT %s FROM `%s.%s.%s` WHERE '%s' IN UNNEST(`%s`) AND `timestamp` >= TIMESTAMP_SUB(%s, INTERVAL %d DAY)",
			linkedColumns(), project, dataset, LinkedEntriesTable, v.id, LinkedViewsColumn, now, days)})
	}
	return out
}

// skewSlack is how far a measured skew may move before the views are made
// again: the measure itself takes a request to BigQuery.
const skewSlack = 2 * time.Second

// clockSkew is how far BigQuery's clock is ahead of the Logging clock,
// to the second; ok is false when it cannot be read, and the skew is then
// 0.
func (a *Admin) clockSkew(ctx context.Context, l LinkedDatasets) (time.Duration, bool) {
	c, isClock := l.(LinkedClock)
	if !isClock {
		return 0, false
	}
	bq, err := c.Now(ctx)
	if err != nil {
		return 0, false
	}
	return bq.Sub(a.logNow()).Round(time.Second), true
}

// logNow is the Logging clock (Server.clock), which retention is measured
// by: now, plus how far its clock:advance moved it.
func (a *Admin) logNow() time.Time {
	a.copies.mu.Lock()
	c := a.clock
	a.copies.mu.Unlock()
	if c != nil {
		return c()
	}
	return a.now()
}

// linkedRows are es, those inside the retention at now, as rows of the
// bucket's dataset, each listing the views that hold it.
func linkedRows(es []*loggingpb.LogEntry, views []bucketView, now time.Time, days int32) (rows []map[string]any, ids []string) {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, e := range es {
		if e.GetTimestamp().AsTime().Before(cutoff) {
			continue
		}
		in := []any{}
		for _, v := range views {
			if v.match == nil || v.match(e) {
				in = append(in, v.id)
			}
		}
		row := linkedRow(e)
		row[LinkedViewsColumn] = in
		rows, ids = append(rows, row), append(ids, e.GetInsertId())
	}
	return rows, ids
}

// appendRows streams rows into a dataset, in insertAll's recommended
// batches.
func appendRows(ctx context.Context, l LinkedDatasets, project, dataset string, rows []map[string]any, ids []string) error {
	var first error
	for start := 0; start < len(rows); start += 500 {
		end := min(start+500, len(rows))
		if err := l.Append(ctx, project, dataset, rows[start:end], ids[start:end]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// linkError is err, from making or filling a link's dataset, as the
// caller's error.
func linkError(project, dataset string, err error) error {
	var ae *apierror.Error
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, ErrProjectNotServed):
		return apierror.FailedPrecondition("a link makes a BigQuery dataset in the bucket's project, %s, which this instance's BigQuery does not serve: it serves one project", project)
	case errors.Is(err, ErrDatasetExists):
		return apierror.AlreadyExists("BigQuery dataset %s:%s already exists; a link makes its own dataset", project, dataset)
	}
	return &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("the linked BigQuery dataset %s:%s: %v", project, dataset, err)}
}

// syncLink makes link l's dataset, of bucket ref, with the bucket's views
// and the entries it holds: for the first time when fresh (an existing
// dataset is ErrDatasetExists), else again, in place of the one there. The
// caller holds a.mu.
func (a *Admin) syncLink(ctx context.Context, ref bucketRef, l *loggingpb.Link, fresh bool) error {
	project, dataset := linkedDataset(l)
	lr, err := parseLink("name", l.GetName())
	if dataset == "" || err != nil {
		return nil
	}
	views, err := a.bucketViews(ref)
	if err != nil {
		return err
	}
	days := a.retentionDays(ref)
	key := project + "." + dataset
	skew, _ := a.clockSkew(ctx, a.linked)
	a.linkSync.mu.Lock()
	defer a.linkSync.mu.Unlock()
	a.copies.mu.Lock()
	entriesOf := a.copies.entries
	a.copies.mu.Unlock()
	a.linkSync.snap.Lock()
	if a.linkSync.gen == nil {
		a.linkSync.gen = map[string]uint64{}
	}
	a.linkSync.gen[key]++
	var entries []*loggingpb.LogEntry
	if entriesOf != nil {
		entries = entriesOf(ref)
	}
	a.linkSync.snap.Unlock()
	if !fresh {
		if err := a.linked.Delete(ctx, project, dataset); err != nil {
			return linkError(project, dataset, err)
		}
	}
	if err := a.linked.Create(ctx, project, dataset, linkDatasetDescription(lr), linkedSchema(), viewQueries(project, dataset, views, days, skew)); err != nil {
		return linkError(project, dataset, err)
	}
	if a.linkSync.skew == nil {
		a.linkSync.skew = map[string]time.Duration{}
	}
	a.linkSync.skew[key] = skew
	rows, ids := linkedRows(entries, views, a.logNow(), days)
	if err := appendRows(ctx, a.linked, project, dataset, rows, ids); err != nil {
		if fresh {
			_ = a.linked.Delete(ctx, project, dataset)
		}
		return linkError(project, dataset, fmt.Errorf("writing the bucket's entries: %w", err))
	}
	return nil
}

// resyncBucket makes the datasets of the bucket's links again, after a
// change of its views, its retention or its entries. The caller holds
// a.mu.
func (a *Admin) resyncBucket(ctx context.Context, ref bucketRef) error {
	if a.linked == nil {
		return nil
	}
	ls, err := a.links(ref)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx) // a dataset left half made helps no one
	var first error
	for _, l := range ls {
		if err := a.syncLink(ctx, ref, l, false); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// allLinks are every link the store holds, with its bucket. The caller
// holds a.mu.
func (a *Admin) allLinks() []linkRef {
	keys, err := a.db.List("logging/links/")
	if err != nil {
		return nil
	}
	var out []linkRef
	for _, k := range keys {
		raw, err := a.db.Get(k)
		if err != nil {
			continue
		}
		l := &loggingpb.Link{}
		if protojson.Unmarshal(raw, l) != nil {
			continue
		}
		if ref, err := parseLink("name", l.GetName()); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

// EnsureLinkedDatasets makes again, and fills, each link's dataset that
// BigQuery no longer has: after an emulator restart, which loses what the
// emulator held, or an instance's restart with a new emulator. It also
// makes again each dataset whose views measure retention by a skew of the
// two clocks that is no longer theirs, or that this process did not make
// (#1602). It reports how many it made.
func (a *Admin) EnsureLinkedDatasets(ctx context.Context) int {
	a.mu.Lock()
	l, refs := a.linked, a.allLinks()
	a.mu.Unlock()
	if l == nil {
		return 0
	}
	made := 0
	var skew time.Duration
	measured := false
	if len(refs) > 0 {
		skew, measured = a.clockSkew(ctx, l) // once: a read of BigQuery's clock is a request
	}
	for _, ref := range refs {
		a.mu.Lock()
		link, err := a.getLink(ref)
		a.mu.Unlock()
		if err != nil {
			continue
		}
		project, dataset := linkedDataset(link)
		ok, err := l.Exists(ctx, project, dataset)
		if err != nil {
			continue // BigQuery not answering (yet)
		}
		if ok && !a.skewMoved(skew, measured, project+"."+dataset) {
			continue // the dataset is there, its views by the clocks' skew
		}
		a.mu.Lock()
		if _, err := a.getLink(ref); err == nil && a.syncLink(ctx, ref.bucket, link, !ok) == nil {
			made++
		}
		a.mu.Unlock()
	}
	return made
}

// skewMoved is whether the dataset key's views were made by a skew of
// BigQuery's clock over the Logging clock other than skew, or were not
// made by this process. A skew that was not measured has not moved.
func (a *Admin) skewMoved(skew time.Duration, measured bool, key string) bool {
	if !measured {
		return false
	}
	a.linkSync.mu.Lock()
	made, known := a.linkSync.skew[key]
	a.linkSync.mu.Unlock()
	return !known || (skew-made).Abs() > skewSlack
}

// KeepLinkedDatasets runs EnsureLinkedDatasets now and every period until
// ctx is done.
func (a *Admin) KeepLinkedDatasets(ctx context.Context, period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		a.EnsureLinkedDatasets(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// linkTarget is what one bucket's entries are appended to: its links, and
// what its rows need.
type linkTarget struct {
	links []*loggingpb.Link
	views []bucketView
	days  int32
}

// linksOf returns the links of each bucket (by name) any of the routed
// entries is in, reading each bucket's links once.
func (a *Admin) linksOf(buckets map[string]bucketRef) map[string]linkTarget {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.linked == nil {
		return nil
	}
	out := map[string]linkTarget{}
	for name, ref := range buckets {
		ls, err := a.links(ref)
		if err != nil || len(ls) == 0 {
			continue
		}
		views, err := a.bucketViews(ref)
		if err != nil {
			continue
		}
		out[name] = linkTarget{links: ls, views: views, days: a.retentionDays(ref)}
	}
	return out
}

// bucketsOf are the buckets each entry is in: its parent's _Default unless
// an exclusion kept it out, and those sinks routed it to.
func bucketsOf(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) (map[*loggingpb.LogEntry][]string, map[string]bucketRef) {
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
	return in, buckets
}

// linkWrites streams the entries just stored into the linked datasets of
// the buckets they are in, in the background; gens are the datasets'
// generations when they were stored.
func (s *Server) linkWrites(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route, gens map[string]uint64) {
	if s.admin == nil || len(es) == 0 {
		return
	}
	s.admin.mu.Lock()
	l := s.admin.linked
	s.admin.mu.Unlock()
	if l == nil {
		return
	}
	in, buckets := bucketsOf(es, routes)
	targets := s.admin.linksOf(buckets)
	if len(targets) == 0 {
		return
	}
	byBucket := map[string][]*loggingpb.LogEntry{}
	for _, e := range es {
		for _, b := range in[e] {
			if _, ok := targets[b]; ok {
				byBucket[b] = append(byBucket[b], e)
			}
		}
	}
	now := s.clock()
	s.d.wg.Add(1)
	go func() {
		defer s.d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.admin.linkSync.mu.Lock()
		defer s.admin.linkSync.mu.Unlock()
		for b, entries := range byBucket {
			t := targets[b]
			rows, ids := linkedRows(entries, t.views, now, t.days)
			for _, link := range t.links {
				project, dataset := linkedDataset(link)
				if dataset == "" || s.admin.linkSync.current(project+"."+dataset) != gens[project+"."+dataset] {
					continue // made again since, with these entries
				}
				_ = appendRows(ctx, l, project, dataset, rows, ids)
			}
		}
	}()
}

// resyncAfterDelete makes again the datasets of the links of the buckets
// es, entries DeleteLog just removed, were in.
func (s *Server) resyncAfterDelete(ctx context.Context, es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) {
	if s.admin == nil || len(es) == 0 {
		return
	}
	_, buckets := bucketsOf(es, routes)
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(buckets)) {
		_ = s.admin.resyncBucket(ctx, buckets[name])
	}
}
