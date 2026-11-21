package firestoreadmin

// Document reads, writes and deletes, and query statistics (#2048).
//
// Google's Usage page charts Cloud Monitoring's
// firestore.googleapis.com/document/read_ops_count (by type, LOOKUP or
// QUERY), write_ops_count (by op, CREATE or UPDATE) and delete_ops_count:
// "the number of successful document reads", writes and deletes. Its Query
// insights take their data from the executions of listDocuments,
// listCollectionIds, runQuery, runAggregationQuery, partitionQuery and
// executePipeline (docs.cloud.google.com/firestore/native/docs/
// query-insights). The front sees every one of these calls, so it counts
// them itself as they pass: the documents a call's answer carries (a
// lookup's found documents, a query's results), the writes a successful
// commit applied, as CREATE when the document did not exist before and
// UPDATE when it did, and the deletes of documents that existed (a delete
// of a missing document is Google's DELETE_NOOP, not counted). Whether a
// write's document existed is asked of the emulator, as the owner, just
// before the write passes, in the write's transaction when it has one.
//
// Counts are kept per database in one-minute buckets for a day, and served
// at UsagePath to the CLI, which answers Cloud Monitoring's time series
// and the console's Usage and Query insights pages from them.
//
// The bidirectional Listen and Write streams (snapshot listeners, the
// mobile SDKs' writes) are counted as their frames pass (usagestream.go,
// #2050). Not counted: the web SDK's WebChannel, whose framing the front
// does not decode.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// UsagePath serves the counts: GET UsagePath?since=RFC3339 answers
// {"minutes": [UsageMinute...]}, oldest first.
const UsagePath = "/_cloudburrow/firestore-usage"

// usageRetention is how long minute buckets are kept.
const usageRetention = 24 * time.Hour

// maxCapture bounds a REST answer the front reads to count it.
const maxCapture = 32 << 20

// UsageMinute is one minute's counts, by database name
// (projects/P/databases/D).
type UsageMinute struct {
	Start     time.Time                 `json:"start"`
	Databases map[string]*DatabaseUsage `json:"databases"`
}

// DatabaseUsage is a database's counts in one minute.
type DatabaseUsage struct {
	// Reads by type: LOOKUP or QUERY.
	Reads map[string]int64 `json:"reads,omitempty"`
	// Writes by op: CREATE or UPDATE.
	Writes  map[string]int64 `json:"writes,omitempty"`
	Deletes int64            `json:"deletes,omitempty"`
	// Queries by Shape.
	Queries map[string]*QueryStat `json:"queries,omitempty"`
}

// QueryStat is one query shape's executions in one minute.
type QueryStat struct {
	// Shape is the normalized query text: the query's structure, its
	// values replaced by "?".
	Shape string `json:"shape"`
	// Method is the API method: RunQuery, RunAggregationQuery,
	// ListDocuments, ListCollectionIds or PartitionQuery.
	Method     string `json:"method"`
	Executions int64  `json:"executions"`
	Errors     int64  `json:"errors,omitempty"`
	DurationNs int64  `json:"durationNs"`
	// Results are the results the executions returned.
	Results int64 `json:"results"`
	// DocumentsScanned are the documents the executions read: a
	// Standard edition query is served from an index alone, so each
	// document it scans is a result it returns.
	DocumentsScanned int64 `json:"documentsScanned"`
	// ResultCounts and ScannedCounts are each execution's, the first
	// maxPerExecution of the minute, for Cloud Monitoring's per-query
	// distributions.
	ResultCounts  []int64 `json:"resultCounts,omitempty"`
	ScannedCounts []int64 `json:"scannedCounts,omitempty"`
}

// maxPerExecution bounds the executions a minute keeps one by one.
const maxPerExecution = 1000

// Usage counts the documents calls through the front.
type Usage struct {
	a   *Admin
	now func() time.Time

	mu      sync.Mutex
	minutes []*UsageMinute
}

// Usage is the front's document and query counts.
func (a *Admin) Usage() *Usage {
	a.usageOnce.Do(func() { a.usage = &Usage{a: a, now: time.Now} })
	return a.usage
}

func (u *Usage) bucket(db string) *DatabaseUsage {
	t := u.now().UTC().Truncate(time.Minute)
	var m *UsageMinute
	if n := len(u.minutes); n > 0 && u.minutes[n-1].Start.Equal(t) {
		m = u.minutes[n-1]
	} else {
		m = &UsageMinute{Start: t, Databases: map[string]*DatabaseUsage{}}
		u.minutes = append(u.minutes, m)
		cut := t.Add(-usageRetention)
		i := 0
		for i < len(u.minutes) && u.minutes[i].Start.Before(cut) {
			i++
		}
		u.minutes = u.minutes[i:]
	}
	d := m.Databases[db]
	if d == nil {
		d = &DatabaseUsage{}
		m.Databases[db] = d
	}
	return d
}

func (u *Usage) addReads(db, typ string, n int64) {
	if n <= 0 || db == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.bucket(db)
	if d.Reads == nil {
		d.Reads = map[string]int64{}
	}
	d.Reads[typ] += n
}

func (u *Usage) addWrites(db string, creates, updates, deletes int64) {
	if db == "" || creates+updates+deletes == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.bucket(db)
	if creates+updates > 0 && d.Writes == nil {
		d.Writes = map[string]int64{}
	}
	if creates > 0 {
		d.Writes["CREATE"] += creates
	}
	if updates > 0 {
		d.Writes["UPDATE"] += updates
	}
	d.Deletes += deletes
}

func (u *Usage) addQuery(db, method, shape string, ok bool, dur time.Duration, results, scanned int64) {
	if db == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.bucket(db)
	if d.Queries == nil {
		d.Queries = map[string]*QueryStat{}
	}
	k := method + "\x00" + shape
	q := d.Queries[k]
	if q == nil {
		q = &QueryStat{Shape: shape, Method: method}
		d.Queries[k] = q
	}
	q.Executions++
	if !ok {
		q.Errors++
	}
	q.DurationNs += int64(dur)
	q.Results += results
	q.DocumentsScanned += scanned
	if len(q.ResultCounts) < maxPerExecution {
		q.ResultCounts = append(q.ResultCounts, results)
		q.ScannedCounts = append(q.ScannedCounts, scanned)
	}
}

// Minutes are the buckets that start at or after since, copied.
func (u *Usage) Minutes(since time.Time) []UsageMinute {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []UsageMinute
	for _, m := range u.minutes {
		if m.Start.Before(since.Truncate(time.Minute)) {
			continue
		}
		b, _ := json.Marshal(m)
		var c UsageMinute
		_ = json.Unmarshal(b, &c)
		out = append(out, c)
	}
	return out
}

func (u *Usage) serve(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "since: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = t
	}
	minutes := u.Minutes(since)
	if minutes == nil {
		minutes = []UsageMinute{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"minutes": minutes})
}

// usageCall is one documents call the front counts.
type usageCall struct {
	method string // the RPC's name, as in GetDocument
	db     string // projects/P/databases/D
	grpc   bool
	req    proto.Message
}

var restDocs = regexp.MustCompile(`^/v1/(projects/[^/]+/databases/[^/]+)/documents(/[^:]*)?(?::([A-Za-z]+))?$`)

// classify names r's method and database, or reports false for a call the
// front does not count.
func classify(r *http.Request) (usageCall, bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		m, ok := strings.CutPrefix(r.URL.Path, "/google.firestore.v1.Firestore/")
		if !ok || newRequest(m) == nil {
			return usageCall{}, false
		}
		return usageCall{method: m, grpc: true, req: newRequest(m)}, true
	}
	g := restDocs.FindStringSubmatch(r.URL.Path)
	if g == nil {
		return usageCall{}, false
	}
	c := usageCall{db: g[1]}
	segs := 0
	if p := strings.Trim(g[2], "/"); p != "" {
		segs = len(strings.Split(p, "/"))
	}
	verbs := map[string]string{"batchGet": "BatchGetDocuments", "runQuery": "RunQuery",
		"runAggregationQuery": "RunAggregationQuery", "commit": "Commit", "batchWrite": "BatchWrite",
		"listCollectionIds": "ListCollectionIds", "partitionQuery": "PartitionQuery"}
	switch {
	case g[3] != "":
		c.method = verbs[g[3]]
		if r.Method != http.MethodPost {
			c.method = ""
		}
	case r.Method == http.MethodGet && segs > 0 && segs%2 == 0:
		c.method = "GetDocument"
	case r.Method == http.MethodGet && segs%2 == 1:
		c.method = "ListDocuments"
	case r.Method == http.MethodPatch && segs > 0 && segs%2 == 0:
		c.method = "UpdateDocument"
	case r.Method == http.MethodDelete && segs > 0 && segs%2 == 0:
		c.method = "DeleteDocument"
	case r.Method == http.MethodPost && segs%2 == 1:
		c.method = "CreateDocument"
	}
	if c.method == "" {
		return usageCall{}, false
	}
	c.req = newRequest(c.method)
	return c, true
}

func newRequest(method string) proto.Message {
	switch method {
	case "GetDocument":
		return &firestorepb.GetDocumentRequest{}
	case "ListDocuments":
		return &firestorepb.ListDocumentsRequest{}
	case "BatchGetDocuments":
		return &firestorepb.BatchGetDocumentsRequest{}
	case "RunQuery":
		return &firestorepb.RunQueryRequest{}
	case "RunAggregationQuery":
		return &firestorepb.RunAggregationQueryRequest{}
	case "PartitionQuery":
		return &firestorepb.PartitionQueryRequest{}
	case "ListCollectionIds":
		return &firestorepb.ListCollectionIdsRequest{}
	case "Commit":
		return &firestorepb.CommitRequest{}
	case "BatchWrite":
		return &firestorepb.BatchWriteRequest{}
	case "CreateDocument":
		return &firestorepb.CreateDocumentRequest{}
	case "UpdateDocument":
		return &firestorepb.UpdateDocumentRequest{}
	case "DeleteDocument":
		return &firestorepb.DeleteDocumentRequest{}
	}
	return nil
}

func newResponse(method string) proto.Message {
	switch method {
	case "ListDocuments":
		return &firestorepb.ListDocumentsResponse{}
	case "BatchGetDocuments":
		return &firestorepb.BatchGetDocumentsResponse{}
	case "RunQuery":
		return &firestorepb.RunQueryResponse{}
	case "RunAggregationQuery":
		return &firestorepb.RunAggregationQueryResponse{}
	case "PartitionQuery":
		return &firestorepb.PartitionQueryResponse{}
	case "ListCollectionIds":
		return &firestorepb.ListCollectionIdsResponse{}
	case "BatchWrite":
		return &firestorepb.BatchWriteResponse{}
	}
	return nil
}

// Wrap counts the documents calls next answers, and serves UsagePath.
func (u *Usage) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == UsagePath && r.Method == http.MethodGet {
			u.serve(w, r)
			return
		}
		if m, ok := strings.CutPrefix(r.URL.Path, "/google.firestore.v1.Firestore/"); ok && streamMethods[m] &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			u.wrapStream(m, next, w, r)
			return
		}
		c, ok := classify(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCapture+1))
		if err != nil || len(body) > maxCapture {
			// Too large to count: pass it on whole.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
			next.ServeHTTP(w, r)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		if !decodeRequest(&c, r, body) {
			next.ServeHTTP(w, r)
			return
		}
		existed := u.existing(r.Context(), c)
		cw := &captureWriter{ResponseWriter: w, grpc: c.grpc, code: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(cw, r)
		u.count(c, cw, existed, time.Since(start))
	})
}

// decodeRequest reads the request message, and its database, from body.
func decodeRequest(c *usageCall, r *http.Request, body []byte) bool {
	if c.grpc {
		if len(body) < 5 || body[0] != 0 {
			return false
		}
		n := binary.BigEndian.Uint32(body[1:5])
		if int(n) > len(body)-5 || proto.Unmarshal(body[5:5+n], c.req) != nil {
			return false
		}
	} else {
		docPath := strings.TrimPrefix(r.URL.Path, "/v1/")
		if i := strings.LastIndexByte(docPath, ':'); i > 0 && !strings.Contains(docPath[i:], "/") {
			docPath = docPath[:i]
		}
		switch m := c.req.(type) {
		case *firestorepb.GetDocumentRequest:
			m.Name = docPath
		case *firestorepb.DeleteDocumentRequest:
			m.Name = docPath
		case *firestorepb.ListDocumentsRequest:
			m.Parent, m.CollectionId = splitCollection(docPath)
		case *firestorepb.UpdateDocumentRequest:
			m.Document = &firestorepb.Document{}
			if len(body) > 0 && (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, m.Document) != nil {
				return false
			}
			m.Document.Name = docPath
		case *firestorepb.CreateDocumentRequest:
			m.Parent, m.CollectionId = splitCollection(docPath)
		default:
			if len(body) > 0 && (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, c.req) != nil {
				return false
			}
		}
	}
	if c.db == "" {
		c.db = usageDatabase(c.req)
	}
	return c.db != ""
}

// splitCollection is a collection path's parent and collection ID.
func splitCollection(p string) (string, string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return p, ""
	}
	return p[:i], p[i+1:]
}

var dbPrefix = regexp.MustCompile(`^projects/[^/]+/databases/[^/]+`)

func usageDatabase(m proto.Message) string {
	var s string
	switch m := m.(type) {
	case *firestorepb.GetDocumentRequest:
		s = m.GetName()
	case *firestorepb.ListDocumentsRequest:
		s = m.GetParent()
	case *firestorepb.BatchGetDocumentsRequest:
		s = m.GetDatabase()
	case *firestorepb.RunQueryRequest:
		s = m.GetParent()
	case *firestorepb.RunAggregationQueryRequest:
		s = m.GetParent()
	case *firestorepb.PartitionQueryRequest:
		s = m.GetParent()
	case *firestorepb.ListCollectionIdsRequest:
		s = m.GetParent()
	case *firestorepb.CommitRequest:
		s = m.GetDatabase()
	case *firestorepb.BatchWriteRequest:
		s = m.GetDatabase()
	case *firestorepb.CreateDocumentRequest:
		s = m.GetParent()
	case *firestorepb.UpdateDocumentRequest:
		s = m.GetDocument().GetName()
	case *firestorepb.DeleteDocumentRequest:
		s = m.GetName()
	}
	return dbPrefix.FindString(s)
}

// writes are a write call's writes: the document each names, and whether
// it is a delete.
func writes(m proto.Message) (names []string, deletes []bool, txn []byte) {
	add := func(ws []*firestorepb.Write) {
		for _, w := range ws {
			switch {
			case w.GetDelete() != "":
				names, deletes = append(names, w.GetDelete()), append(deletes, true)
			case w.GetUpdate() != nil:
				names, deletes = append(names, w.GetUpdate().GetName()), append(deletes, false)
			case w.GetTransform() != nil:
				names, deletes = append(names, w.GetTransform().GetDocument()), append(deletes, false)
			}
		}
	}
	switch m := m.(type) {
	case *firestorepb.CommitRequest:
		add(m.GetWrites())
		txn = m.GetTransaction()
	case *firestorepb.BatchWriteRequest:
		add(m.GetWrites())
	case *firestorepb.UpdateDocumentRequest:
		names, deletes = []string{m.GetDocument().GetName()}, []bool{false}
	case *firestorepb.DeleteDocumentRequest:
		names, deletes = []string{m.GetName()}, []bool{true}
	}
	return names, deletes, txn
}

// existing asks the emulator which of a write call's documents exist.
func (u *Usage) existing(ctx context.Context, c usageCall) map[string]bool {
	names, _, txn := writes(c.req)
	if len(names) == 0 {
		return nil
	}
	cl, err := u.a.docs()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(asOwner(context.WithoutCancel(ctx)), 2*time.Second)
	defer cancel()
	req := &firestorepb.BatchGetDocumentsRequest{Database: c.db, Documents: uniq(names)}
	if len(txn) > 0 {
		req.ConsistencySelector = &firestorepb.BatchGetDocumentsRequest_Transaction{Transaction: txn}
	}
	s, err := cl.BatchGetDocuments(ctx, req)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for {
		res, err := s.Recv()
		if err != nil {
			break
		}
		if f := res.GetFound(); f != nil {
			out[f.GetName()] = true
		}
	}
	return out
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// count records a call's answer.
func (u *Usage) count(c usageCall, cw *captureWriter, existed map[string]bool, dur time.Duration) {
	ok := cw.ok()
	var msgs []proto.Message
	if f := newResponse(c.method); f != nil {
		msgs = cw.messages(c.method)
	}
	switch c.method {
	case "GetDocument":
		if ok {
			u.addReads(c.db, "LOOKUP", 1)
		}
	case "BatchGetDocuments":
		var n int64
		for _, m := range msgs {
			if m.(*firestorepb.BatchGetDocumentsResponse).GetFound() != nil {
				n++
			}
		}
		u.addReads(c.db, "LOOKUP", n)
	case "RunQuery":
		var n int64
		for _, m := range msgs {
			if m.(*firestorepb.RunQueryResponse).GetDocument() != nil {
				n++
			}
		}
		u.addReads(c.db, "QUERY", n)
		q := c.req.(*firestorepb.RunQueryRequest)
		u.addQuery(c.db, c.method, queryShape(q.GetParent(), q.GetStructuredQuery()), ok, dur, n, n)
	case "RunAggregationQuery":
		var n int64
		for _, m := range msgs {
			if m.(*firestorepb.RunAggregationQueryResponse).GetResult() != nil {
				n++
			}
		}
		q := c.req.(*firestorepb.RunAggregationQueryRequest)
		sq := q.GetStructuredAggregationQuery()
		shape := queryShape(q.GetParent(), sq.GetStructuredQuery())
		var aggs []string
		for _, a := range sq.GetAggregations() {
			switch {
			case a.GetCount() != nil:
				aggs = append(aggs, "count()")
			case a.GetSum() != nil:
				aggs = append(aggs, "sum("+a.GetSum().GetField().GetFieldPath()+")")
			case a.GetAvg() != nil:
				aggs = append(aggs, "avg("+a.GetAvg().GetField().GetFieldPath()+")")
			}
		}
		shape += " | aggregate " + strings.Join(aggs, ", ")
		// An aggregation reads index entries, not documents.
		u.addQuery(c.db, c.method, shape, ok, dur, n, 0)
	case "ListDocuments":
		var n int64
		for _, m := range msgs {
			n += int64(len(m.(*firestorepb.ListDocumentsResponse).GetDocuments()))
		}
		u.addReads(c.db, "QUERY", n)
		q := c.req.(*firestorepb.ListDocumentsRequest)
		u.addQuery(c.db, c.method, collectionPath(q.GetParent(), q.GetCollectionId(), false), ok, dur, n, n)
	case "ListCollectionIds":
		var n int64
		for _, m := range msgs {
			n += int64(len(m.(*firestorepb.ListCollectionIdsResponse).GetCollectionIds()))
		}
		q := c.req.(*firestorepb.ListCollectionIdsRequest)
		u.addQuery(c.db, c.method, "collections of "+normalizedParent(q.GetParent()), ok, dur, n, 0)
	case "PartitionQuery":
		var n int64
		for _, m := range msgs {
			n += int64(len(m.(*firestorepb.PartitionQueryResponse).GetPartitions()))
		}
		q := c.req.(*firestorepb.PartitionQueryRequest)
		u.addQuery(c.db, c.method, "partition "+queryShape(q.GetParent(), q.GetStructuredQuery()), ok, dur, n, 0)
	case "CreateDocument":
		if ok {
			u.addWrites(c.db, 1, 0, 0)
		}
	case "Commit", "BatchWrite", "UpdateDocument", "DeleteDocument":
		names, _, _ := writes(c.req)
		applied := make([]bool, len(names))
		for i := range applied {
			applied[i] = ok
		}
		if c.method == "BatchWrite" && len(msgs) > 0 {
			st := msgs[0].(*firestorepb.BatchWriteResponse).GetStatus()
			for i := range applied {
				applied[i] = i < len(st) && st[i].GetCode() == 0
			}
		}
		u.countWrites(c.db, c.req, applied, existed)
	}
}

// countWrites counts a write call's applied writes (all of them for a nil
// applied), given the documents that existed before it.
func (u *Usage) countWrites(db string, req proto.Message, applied []bool, existed map[string]bool) {
	names, dels, _ := writes(req)
	var creates, updates, deletes int64
	// A document written twice in one commit is created once.
	state := map[string]bool{}
	for k, v := range existed {
		state[k] = v
	}
	for i, n := range names {
		if applied != nil && !applied[i] {
			continue
		}
		switch {
		case dels[i]:
			if state[n] {
				deletes++
			}
			state[n] = false
		case state[n]:
			updates++
		default:
			creates++
			state[n] = true
		}
	}
	u.addWrites(db, creates, updates, deletes)
}

// queryShape is a query's normalized text: its collection, the fields it
// filters on and how, its order and whether it is limited, with every
// value replaced by "?".
func queryShape(parent string, q *firestorepb.StructuredQuery) string {
	var parts []string
	from := q.GetFrom()
	switch {
	case len(from) == 0:
		parts = append(parts, normalizedParent(parent))
	default:
		parts = append(parts, collectionPath(parent, from[0].GetCollectionId(), from[0].GetAllDescendants()))
	}
	if s := q.GetSelect(); s != nil {
		var fs []string
		for _, f := range s.GetFields() {
			fs = append(fs, f.GetFieldPath())
		}
		parts = append(parts, "select "+strings.Join(fs, ", "))
	}
	if w := q.GetWhere(); w != nil {
		parts = append(parts, "where "+filterShape(w))
	}
	if len(q.GetOrderBy()) > 0 {
		var os []string
		for _, o := range q.GetOrderBy() {
			os = append(os, o.GetField().GetFieldPath()+" "+strings.ToLower(o.GetDirection().String()))
		}
		parts = append(parts, "order by "+strings.Join(os, ", "))
	}
	if q.GetStartAt() != nil || q.GetEndAt() != nil {
		parts = append(parts, "cursor ?")
	}
	if q.GetOffset() > 0 {
		parts = append(parts, "offset ?")
	}
	if q.GetLimit() != nil {
		parts = append(parts, "limit ?")
	}
	return strings.Join(parts, " | ")
}

var ops = map[string]string{"LESS_THAN": "<", "LESS_THAN_OR_EQUAL": "<=", "GREATER_THAN": ">",
	"GREATER_THAN_OR_EQUAL": ">=", "EQUAL": "==", "NOT_EQUAL": "!=", "ARRAY_CONTAINS": "array-contains",
	"IN": "in", "ARRAY_CONTAINS_ANY": "array-contains-any", "NOT_IN": "not-in"}

func filterShape(f *firestorepb.StructuredQuery_Filter) string {
	switch {
	case f.GetFieldFilter() != nil:
		ff := f.GetFieldFilter()
		op := ops[ff.GetOp().String()]
		if op == "" {
			op = strings.ToLower(ff.GetOp().String())
		}
		return ff.GetField().GetFieldPath() + " " + op + " ?"
	case f.GetUnaryFilter() != nil:
		uf := f.GetUnaryFilter()
		return uf.GetField().GetFieldPath() + " " + strings.ToLower(strings.ReplaceAll(uf.GetOp().String(), "_", " "))
	case f.GetCompositeFilter() != nil:
		cf := f.GetCompositeFilter()
		var subs []string
		for _, s := range cf.GetFilters() {
			subs = append(subs, filterShape(s))
		}
		j := " and "
		if cf.GetOp() == firestorepb.StructuredQuery_CompositeFilter_OR {
			j = " or "
		}
		s := strings.Join(subs, j)
		if len(subs) > 1 && cf.GetOp() == firestorepb.StructuredQuery_CompositeFilter_OR {
			s = "(" + s + ")"
		}
		return s
	}
	return "?"
}

// normalizedParent is a parent's path under its database's documents, its
// document IDs replaced by "*": "/" for the root.
func normalizedParent(parent string) string {
	_, rest, _ := strings.Cut(parent, "/documents")
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) == 1 && segs[0] == "" {
		return "/"
	}
	for i := 1; i < len(segs); i += 2 {
		segs[i] = "*"
	}
	return "/" + strings.Join(segs, "/")
}

func collectionPath(parent, id string, group bool) string {
	if group {
		return "collection group " + id
	}
	p := normalizedParent(parent)
	if p == "/" {
		return "/" + id
	}
	return p + "/" + id
}

// captureWriter passes an answer on and keeps what the front counts: a
// gRPC answer's messages as they arrive, a REST answer's body.
type captureWriter struct {
	http.ResponseWriter
	grpc    bool
	code    int
	buf     bytes.Buffer
	frames  [][]byte
	partial []byte
	over    bool
}

func (w *captureWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.grpc {
		w.partial = append(w.partial, b...)
		for len(w.partial) >= 5 {
			n := int(binary.BigEndian.Uint32(w.partial[1:5]))
			if len(w.partial) < 5+n {
				break
			}
			if w.partial[0] == 0 {
				w.frames = append(w.frames, append([]byte(nil), w.partial[5:5+n]...))
			}
			w.partial = w.partial[5+n:]
		}
	} else if !w.over {
		if w.buf.Len()+len(b) > maxCapture {
			w.over = true
			w.buf.Reset()
		} else {
			w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ok reports a successful answer.
func (w *captureWriter) ok() bool {
	if !w.grpc {
		return w.code >= 200 && w.code < 300
	}
	h := w.ResponseWriter.Header()
	for _, k := range []string{"Grpc-Status", http.TrailerPrefix + "Grpc-Status"} {
		if v := h.Get(k); v != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			return err == nil && n == 0
		}
	}
	return w.code == http.StatusOK
}

// messages are the answer's messages.
func (w *captureWriter) messages(method string) []proto.Message {
	var out []proto.Message
	if w.grpc {
		for _, f := range w.frames {
			m := newResponse(method)
			if proto.Unmarshal(f, m) == nil {
				out = append(out, m)
			}
		}
		return out
	}
	if w.over || w.code < 200 || w.code >= 300 {
		return nil
	}
	b := bytes.TrimSpace(w.buf.Bytes())
	var raws []json.RawMessage
	if len(b) > 0 && b[0] == '[' {
		if json.Unmarshal(b, &raws) != nil {
			return nil
		}
	} else {
		raws = []json.RawMessage{b}
	}
	for _, r := range raws {
		m := newResponse(method)
		if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(r, m) == nil {
			out = append(out, m)
		}
	}
	return out
}
