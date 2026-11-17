package firestoreadmin

// Firestore with MongoDB compatibility (#1703): the commands Google lists
// as supported ("Behavior differences", Commands), over the database's
// Firestore documents. Each refuses the fields Google lists as
// unsupported for it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// unsupportedFields are the fields Google lists as unsupported, by command.
var unsupportedFields = map[string][]string{
	"find": {"max", "min", "returnKey", "showRecordId", "tailable", "oplogReplay", "noCursorTimeout", "awaitData",
		"allowPartialResults", "collation", "allowDiskUse", "allowDiskUsage", "let"},
	"aggregate":     {"collation", "let"},
	"findandmodify": {"collation", "let"},
	"count":         {"collation"},
	"distinct":      {"collation"},
	"getmore":       {"comment"},
	"listdatabases": {"authorizedDatabases"},
	"create": {"timeseries", "expireAfterSeconds", "clusteredIndex", "changeStreamPreAndPostImages", "size", "max",
		"storageEngine", "validator", "validationLevel", "validationAction", "indexOptionDefaults", "viewOn", "pipeline",
		"collation", "encryptedFields"},
}

func checkUnsupported(lname, name string, cmd bson.D) error {
	for _, f := range unsupportedFields[lname] {
		if v, ok := mql.LookupE(cmd, f); ok {
			if b, isBool := v.(bool); isBool && !b {
				continue
			}
			return mql.Errorf(115, "CommandNotSupported", "The %s field of the %s command is not supported.", f, name)
		}
	}
	return nil
}

func (s *MongoServer) command(ctx context.Context, mc *mongoConn, lname, name string, cmd bson.D) (bson.D, error) {
	if err := checkUnsupported(lname, name, cmd); err != nil {
		return nil, err
	}
	switch lname {
	case "find":
		return s.find(ctx, mc, cmd)
	case "getmore":
		return s.getMore(mc, cmd)
	case "killcursors":
		return s.killCursors(mc, cmd)
	case "count":
		return s.count(ctx, mc, cmd)
	case "distinct":
		return s.distinct(ctx, mc, cmd)
	case "aggregate":
		return s.aggregate(ctx, mc, cmd)
	case "insert":
		return s.insert(ctx, mc, cmd)
	case "update":
		return s.update(ctx, mc, cmd)
	case "delete":
		return s.delete(ctx, mc, cmd)
	case "findandmodify":
		return s.findAndModify(ctx, mc, cmd)
	case "listcollections":
		return s.listCollections(ctx, mc, cmd)
	case "listdatabases":
		if f, ok := mql.Lookup(cmd, "filter").(bson.D); ok && len(f) > 0 {
			return nil, mql.BadValue("filter must be empty if provided.")
		}
		if mc.db == nil {
			return bson.D{{Key: "databases", Value: bson.A{}}, {Key: "totalSize", Value: int64(0)}}, nil
		}
		return bson.D{{Key: "databases", Value: bson.A{bson.D{{Key: "name", Value: mc.db.id}, {Key: "sizeOnDisk", Value: int64(0)}, {Key: "empty", Value: false}}}},
			{Key: "totalSize", Value: int64(0)}}, nil
	case "create":
		if _, err := collName(cmd); err != nil {
			return nil, err
		}
		if mql.Truthy(mql.Lookup(cmd, "capped")) {
			return nil, mql.BadValue("capped must be false if provided.")
		}
		return bson.D{}, nil
	case "drop":
		return s.drop(ctx, mc, cmd)
	case "createindexes":
		return s.createIndexes(mc, cmd)
	case "listindexes":
		return s.listIndexes(mc, cmd)
	case "dropindexes":
		return s.dropIndexes(mc, cmd)
	case "explain":
		return s.explain(ctx, mc, cmd)
	}
	return nil, mql.Errorf(59, "CommandNotFound", "no such command: '%s'", name)
}

func collName(cmd bson.D) (string, error) {
	c, ok := cmd[0].Value.(string)
	if !ok {
		return "", mql.Errorf(73, "InvalidNamespace", "collection name has invalid type %s", mql.TypeName(cmd[0].Value))
	}
	return c, mql.CheckCollectionName(c)
}

func (mc *mongoConn) docsParent() string { return mc.db.name + "/documents" }

func (mc *mongoConn) docName(coll, id string) string { return mc.docsParent() + "/" + coll + "/" + id }

// fsErr is a Firestore call's error as a MongoDB error.
func fsErr(err error) error {
	st, _ := status.FromError(err)
	return mql.Errorf(1, "InternalError", "Firestore: %s", st.Message())
}

// load is every document of a collection, in document ID order.
func (s *MongoServer) load(ctx context.Context, mc *mongoConn, coll string) ([]bson.D, []*firestorepb.Document, error) {
	if mc.tx != nil {
		return s.txnLoad(ctx, mc, mc.tx, coll)
	}
	c, err := s.docs()
	if err != nil {
		return nil, nil, fsErr(err)
	}
	stream, err := c.RunQuery(asOwner(ctx), &firestorepb.RunQueryRequest{Parent: mc.docsParent(),
		QueryType: &firestorepb.RunQueryRequest_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
			From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: coll}}}}})
	if err != nil {
		return nil, nil, fsErr(err)
	}
	var out []bson.D
	var raw []*firestorepb.Document
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fsErr(err)
		}
		if r.GetDocument() != nil {
			out = append(out, mql.FromFirestoreDoc(r.GetDocument()))
			raw = append(raw, r.GetDocument())
		}
	}
	return out, raw, nil
}

func filterOf(cmd bson.D, key string) (bson.D, error) {
	v, ok := mql.LookupE(cmd, key)
	if !ok || v == nil {
		return bson.D{}, nil
	}
	f, ok := v.(bson.D)
	if !ok {
		return nil, mql.Errorf(14, "TypeMismatch", "BSON field '%s' is the wrong type '%s', expected type 'object'", key, mql.TypeName(v))
	}
	return f, nil
}

func intField(cmd bson.D, key string) int {
	n, _ := mql.NumberOf(mql.Lookup(cmd, key))
	return int(n)
}

// query is the documents of a collection that filter matches, sorted,
// skipped and limited.
func (s *MongoServer) query(ctx context.Context, mc *mongoConn, coll string, filter bson.D, sortV any, skip, limit int) ([]bson.D, []*firestorepb.Document, error) {
	docs, raw, err := s.queryScored(ctx, mc, coll, filter, sortV, skip, limit)
	for i := range docs {
		docs[i] = mql.StripTextScore(docs[i])
	}
	return docs, raw, err
}

// queryScored is query, a $text's matches carrying their text score
// (mql.TextScoreKey).
func (s *MongoServer) queryScored(ctx context.Context, mc *mongoConn, coll string, filter bson.D, sortV any, skip, limit int) ([]bson.D, []*firestorepb.Document, error) {
	text, err := s.textMatcher(mc, coll, filter, sortV)
	if err != nil {
		return nil, nil, err
	}
	docs, raw, err := s.load(ctx, mc, coll)
	if err != nil {
		return nil, nil, err
	}
	srt, err := mql.ParseSort(sortV)
	if err != nil {
		return nil, nil, err
	}
	var out []bson.D
	var outRaw []*firestorepb.Document
	for i, d := range docs {
		f := filter
		if text != nil {
			tm, score := text(d)
			f = mql.ReplaceText(filter, tm)
			if tm {
				d = append(append(bson.D{}, d...), bson.E{Key: mql.TextScoreKey, Value: score})
			}
		}
		m, err := mql.Matches(d, f)
		if err != nil {
			return nil, nil, err
		}
		if m {
			out = append(out, d)
			outRaw = append(outRaw, raw[i])
		}
	}
	if len(srt) > 0 {
		idx := make([]int, len(out))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(i, j int) bool { return srt.Less(out[idx[i]], out[idx[j]]) })
		so, sr := make([]bson.D, len(out)), make([]*firestorepb.Document, len(out))
		for i, k := range idx {
			so[i], sr[i] = out[k], outRaw[k]
		}
		out, outRaw = so, sr
	}
	if skip < 0 {
		return nil, nil, mql.BadValue("skip value must be non-negative, but received: %d", skip)
	}
	if skip >= len(out) {
		return nil, nil, nil
	}
	out, outRaw = out[skip:], outRaw[skip:]
	if limit > 0 && limit < len(out) {
		out, outRaw = out[:limit], outRaw[:limit]
	}
	return out, outRaw, nil
}

func (s *MongoServer) find(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	filter, err := filterOf(cmd, "filter")
	if err != nil {
		return nil, err
	}
	limit := intField(cmd, "limit")
	single := mql.Truthy(mql.Lookup(cmd, "singleBatch"))
	if limit < 0 {
		limit, single = -limit, true
	}
	if _, hint := mql.LookupE(cmd, "hint"); hint {
		if _, isText, _ := mql.FindText(filter); isText {
			return nil, mql.BadValue("$text cannot be used with hints.")
		}
	}
	docs, _, err := s.queryScored(ctx, mc, coll, filter, mql.Lookup(cmd, "sort"), intField(cmd, "skip"), limit)
	if err != nil {
		return nil, err
	}
	proj, _ := mql.Lookup(cmd, "projection").(bson.D)
	for i, d := range docs {
		if len(proj) > 0 {
			if d, err = mql.ProjectQuery(d, proj, filter); err != nil {
				return nil, err
			}
		}
		docs[i] = mql.StripTextScore(d)
	}
	bs := defaultBatch
	if v, ok := mql.LookupE(cmd, "batchSize"); ok {
		n, _ := mql.NumberOf(v)
		if n < 0 {
			return nil, mql.BadValue("batchSize value must be non-negative, but received: %v", v)
		}
		bs = int(n)
		if bs == 0 {
			bs = -1
		}
	}
	ns := mc.db.id + "." + coll
	var first bson.A
	var rest []bson.D
	if bs == -1 {
		first, rest = bson.A{}, docs
	} else {
		first, rest = batch(docs, bs)
	}
	var id int64
	if !single {
		id = s.newCursor(mc, ns, rest)
	}
	return cursorReply(id, ns, "firstBatch", first), nil
}

func (s *MongoServer) getMore(mc *mongoConn, cmd bson.D) (bson.D, error) {
	id, ok := cmd[0].Value.(int64)
	if !ok {
		return nil, mql.Errorf(14, "TypeMismatch", "getMore's cursor id must be a long")
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	c, ok := s.cursors[id]
	if !ok || c.db != mc.db.name {
		return nil, mql.Errorf(43, "CursorNotFound", "cursor id %d not found", id)
	}
	if coll, _ := mql.Lookup(cmd, "collection").(string); mc.db.id+"."+coll != c.ns {
		return nil, mql.Errorf(13, "Unauthorized", "Requested getMore on namespace '%s.%s', but cursor belongs to a different namespace %s", mc.db.id, coll, c.ns)
	}
	next, rest := batch(c.docs, intField(cmd, "batchSize"))
	c.docs = rest
	if len(rest) == 0 {
		delete(s.cursors, id)
		id = 0
	}
	return cursorReply(id, c.ns, "nextBatch", next), nil
}

func (s *MongoServer) killCursors(mc *mongoConn, cmd bson.D) (bson.D, error) {
	ids, _ := mql.Lookup(cmd, "cursors").(bson.A)
	killed, notFound := bson.A{}, bson.A{}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	for _, v := range ids {
		id, _ := v.(int64)
		if c, ok := s.cursors[id]; ok && c.db == mc.db.name {
			delete(s.cursors, id)
			killed = append(killed, id)
		} else {
			notFound = append(notFound, id)
		}
	}
	return bson.D{{Key: "cursorsKilled", Value: killed}, {Key: "cursorsNotFound", Value: notFound},
		{Key: "cursorsAlive", Value: bson.A{}}, {Key: "cursorsUnknown", Value: bson.A{}}}, nil
}

func (s *MongoServer) count(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	filter, err := filterOf(cmd, "query")
	if err != nil {
		return nil, err
	}
	limit := intField(cmd, "limit")
	if limit < 0 {
		limit = -limit
	}
	docs, _, err := s.query(ctx, mc, coll, filter, nil, intField(cmd, "skip"), limit)
	if err != nil {
		return nil, err
	}
	return bson.D{{Key: "n", Value: int32(len(docs))}}, nil
}

func (s *MongoServer) distinct(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	key, ok := mql.Lookup(cmd, "key").(string)
	if !ok {
		return nil, mql.Errorf(40414, "Location40414", "BSON field 'distinct.key' is missing but a required field")
	}
	filter, err := filterOf(cmd, "query")
	if err != nil {
		return nil, err
	}
	docs, _, err := s.query(ctx, mc, coll, filter, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	values := bson.A{}
	add := func(v any) {
		for _, x := range values {
			if mql.ValuesEqual(x, v) {
				return
			}
		}
		values = append(values, v)
	}
	for _, d := range docs {
		for _, v := range mql.PathValues(d, strings.Split(key, ".")) {
			if mql.TypeName(v) == "missing" {
				continue
			}
			if a, ok := v.(bson.A); ok {
				for _, e := range a {
					add(e)
				}
				continue
			}
			add(v)
		}
	}
	return bson.D{{Key: "values", Value: values}}, nil
}

func (s *MongoServer) aggregate(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	pipeline, ok := mql.Lookup(cmd, "pipeline").(bson.A)
	if !ok {
		return nil, mql.Errorf(14, "TypeMismatch", "'pipeline' option must be specified as an array")
	}
	coll, named := cmd[0].Value.(string)
	if !named {
		// A collection-less aggregation ({aggregate: 1}), whose first
		// stage is $documents: the one such stage Google supports.
		if n, isNum := mql.NumberOf(cmd[0].Value); !isNum || n != 1 {
			return nil, mql.Errorf(73, "InvalidNamespace", "Invalid {aggregate: %v} command: the aggregate must name a collection, or be 1", cmd[0].Value)
		}
	} else if err := mql.CheckCollectionName(coll); err != nil {
		return nil, err
	}
	if mql.Truthy(mql.Lookup(cmd, "explain")) {
		return nil, mql.Errorf(115, "CommandNotSupported", "Query Explain does not support commands which return a cursor: use the explain command, {explain: {aggregate: ...}}, not aggregate's explain option.")
	}
	cur, hasCursor := mql.Lookup(cmd, "cursor").(bson.D)
	if !hasCursor {
		return nil, mql.Errorf(9, "FailedToParse", "The 'cursor' option is required, except for aggregate with the explain argument")
	}
	var docs []bson.D
	var err error
	if named {
		if docs, _, err = s.load(ctx, mc, coll); err != nil {
			return nil, err
		}
	} else {
		if docs, pipeline, err = mql.DocumentsInput(pipeline); err != nil {
			return nil, err
		}
		coll = "$cmd.aggregate"
	}
	if named {
		if docs, pipeline, err = s.textStage(mc, coll, docs, pipeline); err != nil {
			return nil, err
		}
	}
	env := mql.NewEnv(func(other string) ([]bson.D, error) {
		if err := mql.CheckCollectionName(other); err != nil {
			return nil, err
		}
		d, _, err := s.load(ctx, mc, other)
		return d, err
	})
	out, err := mql.RunPipeline(env, docs, pipeline)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i] = mql.StripTextScore(out[i])
	}
	bs := defaultBatch
	if v, ok := mql.LookupE(cur, "batchSize"); ok {
		n, _ := mql.NumberOf(v)
		bs = int(n)
	}
	ns := mc.db.id + "." + coll
	var first bson.A
	var rest []bson.D
	if bs == 0 {
		first, rest = bson.A{}, out
	} else {
		first, rest = batch(out, bs)
	}
	return cursorReply(s.newCursor(mc, ns, rest), ns, "firstBatch", first), nil
}

// writeErr is a write command's error for statement i.
func writeErr(i int, err error) bson.D {
	var me *mql.Error
	if !errors.As(err, &me) {
		me = mql.Errorf(1, "InternalError", "%v", err)
	}
	return bson.D{{Key: "index", Value: int32(i)}, {Key: "code", Value: me.Code}, {Key: "errmsg", Value: me.Msg}}
}

func dupKey(db, coll, index string, key any) *mql.Error {
	return mql.Errorf(11000, "DuplicateKey", "E11000 duplicate key error collection: %s.%s index: %s dup key: %v", db, coll, index, key)
}

func (s *MongoServer) exists(ctx context.Context, mc *mongoConn, name string) (bool, error) {
	if mc.tx != nil {
		return s.txnExists(ctx, mc.tx, name)
	}
	c, err := s.docs()
	if err != nil {
		return false, fsErr(err)
	}
	_, err = c.GetDocument(asOwner(ctx), &firestorepb.GetDocumentRequest{Name: name})
	switch status.Code(err) {
	case codes.OK:
		return true, nil
	case codes.NotFound:
		return false, nil
	}
	return false, fsErr(err)
}

// uniqueIndexes are a collection's unique MongoDB indexes, by name.
func (s *MongoServer) uniqueIndexes(mc *mongoConn, coll string) map[string][]string {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	out := map[string][]string{}
	prefix := mc.db.name + "/collectionGroups/" + coll + "/indexes/"
	for n, x := range s.a.indexes {
		if !strings.HasPrefix(n, prefix) || !x.GetUnique() || x.GetApiScope() != adminpb.Index_MONGODB_COMPATIBLE_API {
			continue
		}
		var paths []string
		for _, f := range x.GetFields() {
			if f.GetFieldPath() != "__name__" {
				paths = append(paths, f.GetFieldPath())
			}
		}
		out[s.a.mongoIndexName(x)] = paths
	}
	return out
}

// checkUnique refuses doc when a unique index of the collection already
// holds its key in another document.
func (s *MongoServer) checkUnique(ctx context.Context, mc *mongoConn, coll string, doc bson.D) error {
	idx := s.uniqueIndexes(mc, coll)
	if len(idx) == 0 {
		return nil
	}
	docs, _, err := s.load(ctx, mc, coll)
	if err != nil {
		return err
	}
	id := mql.Lookup(doc, "_id")
	key := func(d bson.D, paths []string) bson.A {
		var k bson.A
		for _, p := range paths {
			v := mql.PathValues(d, strings.Split(p, "."))
			if len(v) == 1 && mql.TypeName(v[0]) != "missing" {
				k = append(k, v[0])
			} else {
				k = append(k, nil)
			}
		}
		return k
	}
	for name, paths := range idx {
		mine := key(doc, paths)
		for _, o := range docs {
			if mql.ValuesEqual(mql.Lookup(o, "_id"), id) {
				continue
			}
			if mql.ValuesEqual(key(o, paths), mine) {
				return dupKey(mc.db.id, coll, name, mine)
			}
		}
	}
	return nil
}

// write writes doc, a new document when updated is nil, else a
// replacement of the document as it was read.
func (s *MongoServer) write(ctx context.Context, mc *mongoConn, coll string, doc bson.D, updated *timestamppb.Timestamp) error {
	id, ok := mql.LookupE(doc, "_id")
	if !ok {
		return mql.Errorf(1, "InternalError", "a document without _id")
	}
	did, err := mql.DocID(id)
	if err != nil {
		return err
	}
	fields, err := mql.ToFirestoreDoc(doc)
	if err != nil {
		return err
	}
	if updated == nil {
		for _, alias := range mql.NumericAliases(id) {
			if alias == did {
				continue
			}
			if ex, err := s.exists(ctx, mc, mc.docName(coll, alias)); err != nil {
				return err
			} else if ex {
				return dupKey(mc.db.id, coll, "_id_", bson.D{{Key: "_id", Value: id}})
			}
		}
	}
	if err := s.checkUnique(ctx, mc, coll, doc); err != nil {
		return err
	}
	if mc.tx != nil {
		name := mc.docName(coll, did)
		if updated == nil {
			if ex, err := s.txnExists(ctx, mc.tx, name); err != nil {
				return err
			} else if ex {
				return dupKey(mc.db.id, coll, "_id_", bson.D{{Key: "_id", Value: id}})
			}
		}
		return s.txnPut(ctx, mc.tx, name, &firestorepb.Document{Name: name, Fields: fields})
	}
	pre := &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_Exists{Exists: false}}
	if updated != nil {
		pre = &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_UpdateTime{UpdateTime: updated}}
	}
	c, err := s.docs()
	if err != nil {
		return fsErr(err)
	}
	_, err = c.Commit(asOwner(ctx), &firestorepb.CommitRequest{Database: mc.db.name, Writes: []*firestorepb.Write{{
		Operation:       &firestorepb.Write_Update{Update: &firestorepb.Document{Name: mc.docName(coll, did), Fields: fields}},
		CurrentDocument: pre}}})
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.AlreadyExists:
		return dupKey(mc.db.id, coll, "_id_", bson.D{{Key: "_id", Value: id}})
	case codes.FailedPrecondition, codes.Aborted:
		if updated == nil {
			return dupKey(mc.db.id, coll, "_id_", bson.D{{Key: "_id", Value: id}})
		}
		return mql.Errorf(112, "WriteConflict", "The document changed while it was updated; retry the update.")
	case codes.InvalidArgument:
		st, _ := status.FromError(err)
		return mql.BadValue("%s", st.Message())
	}
	return fsErr(err)
}

func (s *MongoServer) remove(ctx context.Context, mc *mongoConn, name string) error {
	if mc.tx != nil {
		return s.txnPut(ctx, mc.tx, name, nil)
	}
	c, err := s.docs()
	if err != nil {
		return fsErr(err)
	}
	if _, err := c.Commit(asOwner(ctx), &firestorepb.CommitRequest{Database: mc.db.name,
		Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Delete{Delete: name}}}}); err != nil {
		return fsErr(err)
	}
	return nil
}

// withID is doc with an _id, a new ObjectId when it has none, first.
func withID(doc bson.D) bson.D {
	if id, ok := mql.LookupE(doc, "_id"); ok {
		out := bson.D{{Key: "_id", Value: id}}
		for _, e := range doc {
			if e.Key != "_id" {
				out = append(out, e)
			}
		}
		return out
	}
	return append(bson.D{{Key: "_id", Value: bson.NewObjectID()}}, doc...)
}

func ordered(cmd bson.D) bool {
	v, ok := mql.LookupE(cmd, "ordered")
	return !ok || mql.Truthy(v)
}

func (s *MongoServer) insert(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	docs, _ := mql.Lookup(cmd, "documents").(bson.A)
	if len(docs) > 100000 {
		return nil, mql.Errorf(10, "InvalidLength", "Write batch sizes must be between 1 and 100000. Got %d operations.", len(docs))
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n := 0
	var errs bson.A
	for i, v := range docs {
		d, ok := v.(bson.D)
		if !ok {
			errs = append(errs, writeErr(i, mql.BadValue("the document to insert must be an object")))
		} else if err := s.write(ctx, mc, coll, withID(d), nil); err != nil {
			errs = append(errs, writeErr(i, err))
		} else {
			n++
			continue
		}
		if ordered(cmd) {
			break
		}
	}
	resp := bson.D{{Key: "n", Value: int32(n)}}
	if len(errs) > 0 {
		resp = append(resp, bson.E{Key: "writeErrors", Value: errs})
	}
	return resp, nil
}

func (s *MongoServer) update(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	stmts, _ := mql.Lookup(cmd, "updates").(bson.A)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var n, modified int
	var errs, upserted bson.A
	for i, v := range stmts {
		st, _ := v.(bson.D)
		m, mod, up, err := s.updateOne(ctx, mc, coll, st)
		n += m
		modified += mod
		if up != nil {
			n++
			upserted = append(upserted, bson.D{{Key: "index", Value: int32(i)}, {Key: "_id", Value: up}})
		}
		if err != nil {
			errs = append(errs, writeErr(i, err))
			if ordered(cmd) {
				break
			}
		}
	}
	resp := bson.D{{Key: "n", Value: int32(n)}, {Key: "nModified", Value: int32(modified)}}
	if len(upserted) > 0 {
		resp = append(resp, bson.E{Key: "upserted", Value: upserted})
	}
	if len(errs) > 0 {
		resp = append(resp, bson.E{Key: "writeErrors", Value: errs})
	}
	return resp, nil
}

func (s *MongoServer) updateOne(ctx context.Context, mc *mongoConn, coll string, st bson.D) (matched, modified int, upsertedID any, err error) {
	if _, ok := mql.LookupE(st, "collation"); ok {
		return 0, 0, nil, mql.Errorf(115, "CommandNotSupported", "The collation field of an update statement is not supported.")
	}
	q, err := filterOf(st, "q")
	if err != nil {
		return 0, 0, nil, err
	}
	uc, err := updateContext(q, st)
	if err != nil {
		return 0, 0, nil, err
	}
	u := mql.Lookup(st, "u")
	multi := mql.Truthy(mql.Lookup(st, "multi"))
	if ud, ok := u.(bson.D); ok && multi && !mql.IsOperatorUpdate(ud) {
		return 0, 0, nil, mql.Errorf(9, "FailedToParse", "multi update is not supported for replacement-style update")
	}
	limit := 1
	if multi {
		limit = 0
	}
	docs, raw, err := s.query(ctx, mc, coll, q, mql.Lookup(st, "sort"), 0, limit)
	if err != nil {
		return 0, 0, nil, err
	}
	for i, d := range docs {
		nd, err := mql.ApplyUpdateWith(d, u, false, uc)
		if err != nil {
			return matched, modified, nil, err
		}
		matched++
		if mql.ValuesEqual(nd, d) && len(nd) == len(d) && sameOrder(nd, d) {
			continue
		}
		if err := s.write(ctx, mc, coll, nd, raw[i].GetUpdateTime()); err != nil {
			return matched, modified, nil, err
		}
		modified++
	}
	if matched == 0 && mql.Truthy(mql.Lookup(st, "upsert")) {
		nd, err := upsertDoc(q, u, uc)
		if err != nil {
			return 0, 0, nil, err
		}
		if err := s.write(ctx, mc, coll, nd, nil); err != nil {
			return 0, 0, nil, err
		}
		return 0, 0, mql.Lookup(nd, "_id"), nil
	}
	return matched, modified, nil, nil
}

// updateContext is what an update statement's positional operators read:
// its query and its arrayFilters.
func updateContext(q bson.D, st bson.D) (*mql.UpdateContext, error) {
	uc := &mql.UpdateContext{Query: q}
	if v, ok := mql.LookupE(st, "arrayFilters"); ok {
		af, ok := v.(bson.A)
		if !ok {
			return nil, mql.Errorf(14, "TypeMismatch", "BSON field 'arrayFilters' is the wrong type '%s', expected type 'array'", mql.TypeName(v))
		}
		uc.ArrayFilters = af
	}
	return uc, nil
}

func sameOrder(a, b bson.D) bool {
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
	}
	return true
}

// upsertDoc is the document an upsert inserts.
func upsertDoc(q bson.D, u any, uc *mql.UpdateContext) (bson.D, error) {
	if ud, ok := u.(bson.D); ok && !mql.IsOperatorUpdate(ud) {
		nd := mql.CloneDoc(ud)
		if _, ok := mql.LookupE(nd, "_id"); !ok {
			if id, ok := mql.LookupE(mql.UpsertBase(q), "_id"); ok {
				nd = append(bson.D{{Key: "_id", Value: id}}, nd...)
			}
		}
		return withID(nd), nil
	}
	nd, err := mql.ApplyUpdateWith(mql.UpsertBase(q), u, true, uc)
	if err != nil {
		return nil, err
	}
	return withID(nd), nil
}

func (s *MongoServer) delete(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	stmts, _ := mql.Lookup(cmd, "deletes").(bson.A)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n := 0
	var errs bson.A
	for i, v := range stmts {
		st, _ := v.(bson.D)
		if _, ok := mql.LookupE(st, "collation"); ok {
			errs = append(errs, writeErr(i, mql.Errorf(115, "CommandNotSupported", "The collation field of a delete statement is not supported.")))
			if ordered(cmd) {
				break
			}
			continue
		}
		q, err := filterOf(st, "q")
		if err == nil {
			var raw []*firestorepb.Document
			_, raw, err = s.query(ctx, mc, coll, q, nil, 0, intField(st, "limit"))
			for _, r := range raw {
				if err = s.remove(ctx, mc, r.GetName()); err != nil {
					break
				}
				n++
			}
		}
		if err != nil {
			errs = append(errs, writeErr(i, err))
			if ordered(cmd) {
				break
			}
		}
	}
	resp := bson.D{{Key: "n", Value: int32(n)}}
	if len(errs) > 0 {
		resp = append(resp, bson.E{Key: "writeErrors", Value: errs})
	}
	return resp, nil
}

func (s *MongoServer) findAndModify(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	q, err := filterOf(cmd, "query")
	if err != nil {
		return nil, err
	}
	uc, err := updateContext(q, cmd)
	if err != nil {
		return nil, err
	}
	remove := mql.Truthy(mql.Lookup(cmd, "remove"))
	u, hasU := mql.LookupE(cmd, "update")
	if remove == hasU {
		return nil, mql.Errorf(9, "FailedToParse", "Either an update or remove=true must be specified")
	}
	retNew := mql.Truthy(mql.Lookup(cmd, "new"))
	fields, _ := mql.Lookup(cmd, "fields").(bson.D)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	docs, raw, err := s.query(ctx, mc, coll, q, mql.Lookup(cmd, "sort"), 0, 1)
	if err != nil {
		return nil, err
	}
	out := func(d bson.D) (any, error) {
		if d == nil {
			return nil, nil
		}
		return mql.ProjectQuery(d, fields, q)
	}
	lastErr := bson.D{{Key: "n", Value: int32(0)}}
	if len(docs) == 0 {
		if !remove && mql.Truthy(mql.Lookup(cmd, "upsert")) {
			nd, err := upsertDoc(q, u, uc)
			if err != nil {
				return nil, err
			}
			if err := s.write(ctx, mc, coll, nd, nil); err != nil {
				return nil, err
			}
			var v any
			if retNew {
				if v, err = out(nd); err != nil {
					return nil, err
				}
			}
			return bson.D{{Key: "lastErrorObject", Value: bson.D{{Key: "n", Value: int32(1)}, {Key: "updatedExisting", Value: false},
				{Key: "upserted", Value: mql.Lookup(nd, "_id")}}}, {Key: "value", Value: v}}, nil
		}
		if !remove {
			lastErr = append(lastErr, bson.E{Key: "updatedExisting", Value: false})
		}
		return bson.D{{Key: "lastErrorObject", Value: lastErr}, {Key: "value", Value: nil}}, nil
	}
	d := docs[0]
	if remove {
		if err := s.remove(ctx, mc, raw[0].GetName()); err != nil {
			return nil, err
		}
		v, err := out(d)
		if err != nil {
			return nil, err
		}
		return bson.D{{Key: "lastErrorObject", Value: bson.D{{Key: "n", Value: int32(1)}}}, {Key: "value", Value: v}}, nil
	}
	nd, err := mql.ApplyUpdateWith(d, u, false, uc)
	if err != nil {
		return nil, err
	}
	if err := s.write(ctx, mc, coll, nd, raw[0].GetUpdateTime()); err != nil {
		return nil, err
	}
	ret := d
	if retNew {
		ret = nd
	}
	v, err := out(ret)
	if err != nil {
		return nil, err
	}
	return bson.D{{Key: "lastErrorObject", Value: bson.D{{Key: "n", Value: int32(1)}, {Key: "updatedExisting", Value: true}}}, {Key: "value", Value: v}}, nil
}

func (s *MongoServer) listCollections(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	if v, ok := mql.LookupE(cmd, "authorizedCollections"); ok && mql.Truthy(v) {
		return nil, mql.BadValue("authorizedCollections must be false if provided.")
	}
	filter, err := filterOf(cmd, "filter")
	if err != nil {
		return nil, err
	}
	c, err := s.docs()
	if err != nil {
		return nil, fsErr(err)
	}
	var ids []string
	token := ""
	for {
		r, err := c.ListCollectionIds(asOwner(ctx), &firestorepb.ListCollectionIdsRequest{Parent: mc.docsParent(), PageToken: token})
		if err != nil {
			return nil, fsErr(err)
		}
		ids = append(ids, r.GetCollectionIds()...)
		if token = r.GetNextPageToken(); token == "" {
			break
		}
	}
	sort.Strings(ids)
	nameOnly := mql.Truthy(mql.Lookup(cmd, "nameOnly"))
	var out []bson.D
	for _, id := range ids {
		d := bson.D{{Key: "name", Value: id}, {Key: "type", Value: "collection"}}
		if !nameOnly {
			d = append(d, bson.E{Key: "options", Value: bson.D{}}, bson.E{Key: "info", Value: bson.D{{Key: "readOnly", Value: false}}})
		}
		m, err := mql.Matches(d, filter)
		if err != nil {
			return nil, err
		}
		if m {
			out = append(out, d)
		}
	}
	ns := mc.db.id + ".$cmd.listCollections"
	first, rest := batch(out, 0)
	return cursorReply(s.newCursor(mc, ns, rest), ns, "firstBatch", first), nil
}

func (s *MongoServer) drop(ctx context.Context, mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, raw, err := s.load(ctx, mc, coll)
	if err != nil {
		return nil, err
	}
	for _, r := range raw {
		if err := s.remove(ctx, mc, r.GetName()); err != nil {
			return nil, err
		}
	}
	n := s.a.dropMongoIndexes(mc.db.name, coll, func(*adminpb.Index) bool { return true })
	return bson.D{{Key: "ns", Value: mc.db.id + "." + coll}, {Key: "nIndexesWas", Value: int32(n)}}, nil
}

var _ = fmt.Sprint
