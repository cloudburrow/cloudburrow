package firestoreadmin

// Point-in-time recovery (#1942).
//
// Google keeps a database's versions for its version retention period: an
// hour, or seven days with point-in-time recovery enabled. A read at a
// read_time within the past hour may be at any microsecond; an older one
// must be a whole minute, within the seven days, and never earlier than
// the database's earliestVersionTime, which is an hour before the time PITR
// was enabled while that is less than seven days ago
// (https://docs.cloud.google.com/firestore/native/docs/pitr). Exports at a
// snapshot_time and clones read at a whole minute in the same window
// (https://docs.cloud.google.com/firestore/native/docs/use-pitr), an export
// only with PITR enabled.
//
// The Firestore emulator keeps every version of its documents for as long
// as it runs (measured: a GetDocument, RunQuery or ListDocuments at an
// earlier read_time answers the document as it was then, a deleted one
// included), so the versions themselves are the emulator's. What the front
// adds is Google's window: a read whose read_time is outside it is refused
// here, by the guard (edition.go), before the emulator sees it, and an
// export or clone is read at its snapshot time. The emulator keeps no
// versions across its own restart, when the documents are imported again
// (documents.go), so earliestVersionTime is never earlier than that import.
// Nor does it drop old versions: they stay in its heap until it restarts.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// recentVersions is how far back a read may be at any microsecond; an
// older one must be at a whole minute.
const recentVersions = hour

// notePITR records when db's point-in-time recovery was enabled, if it is
// now and was not (wasEnabled), and forgets it when it is disabled. a.mu is
// held.
func (a *Admin) notePITR(db *adminpb.Database, wasEnabled bool) {
	if db.PointInTimeRecoveryEnablement != adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED {
		delete(a.pitrSince, db.Name)
		return
	}
	if !wasEnabled {
		a.pitrSince[db.Name] = a.now()
	}
}

// versionWindow is db's version retention period and the earliest time its
// documents can be read at. a.mu is held.
func (a *Admin) versionWindow(db *adminpb.Database) (time.Duration, time.Time) {
	now := a.now()
	retention := hour
	earliest := now.Add(-hour)
	if db.PointInTimeRecoveryEnablement == adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED {
		retention = week
		earliest = now.Add(-week)
		// Enabled less than seven days ago: an hour before it was.
		if since, ok := a.pitrSince[db.Name]; ok && since.Add(-hour).After(earliest) {
			earliest = since.Add(-hour)
		}
	}
	// A database created here has no versions from before it was; the
	// (default) database is described from the project's first call, but
	// its documents may be older.
	if c := db.CreateTime.AsTime(); !strings.HasSuffix(db.Name, "/databases/"+DefaultDatabase) && earliest.Before(c) {
		earliest = c
	}
	// The Firestore emulator's history is a Native mode database's; a
	// Datastore mode database's versions are the Datastore front's,
	// which applies its own start too (#1967).
	if !isDatastoreMode(db) && earliest.Before(a.historySince) {
		earliest = a.historySince
	}
	return retention, earliest
}

// sawImport records that the emulator has just been given its documents
// back: it holds no versions from before now.
func (a *Admin) sawImport(at time.Time) {
	a.mu.Lock()
	a.historySince = at
	a.mu.Unlock()
}

// errTooOld is Google's refusal of a read outside the version window
// (UNVERIFIED code: the page names the condition, "the read_time is too
// old", not the status).
func errTooOld(t time.Time, earliest time.Time) error {
	return status.Errorf(codes.FailedPrecondition, "The requested 'read_time' (%s) is too old: reads older than one hour must be at a whole minute, and none may be earlier than the database's earliest version time (%s).",
		t.Format(time.RFC3339Nano), earliest.Format(time.RFC3339Nano))
}

// checkReadTime refuses a read of the database dbName at t outside its
// version window. A database the API does not know is the emulator's to
// answer, as is a time in the future (the emulator refuses it: "The
// requested 'read_time' cannot be in the future.").
func (a *Admin) checkReadTime(dbName string, t time.Time) error {
	a.mu.Lock()
	db, err := a.database(dbName)
	if err != nil {
		a.mu.Unlock()
		return nil
	}
	_, earliest := a.versionWindow(db)
	now := a.now()
	a.mu.Unlock()
	return ReadTimeRefusal(t, now, earliest)
}

// ReadTimeRefusal is the refusal of a read at t, now, of a database whose
// earliest version time is earliest, or nil: any microsecond within the
// past hour, a whole minute before it, never before earliest. A time in
// the future is the emulator's to refuse. The Datastore front applies it
// too (#1967).
func ReadTimeRefusal(t, now, earliest time.Time) error {
	if t.After(now) {
		return nil
	}
	if t.Before(earliest) || (t.Before(now.Add(-recentVersions)) && !t.Equal(t.Truncate(time.Minute))) {
		return errTooOld(t, earliest)
	}
	return nil
}

// Window is the database's earliest version time and whether its
// point-in-time recovery is enabled, if the API knows it.
func (a *Admin) Window(name string) (time.Time, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, ok := a.databases[name]
	if !ok || db.DeleteTime != nil {
		return time.Time{}, false, false
	}
	_, earliest := a.versionWindow(db)
	return earliest, db.PointInTimeRecoveryEnablement == adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED, true
}

// checkSnapshotTime checks an export's or clone's snapshot time: a whole
// minute, not in the future, within the database's version window; an
// export's (needPITR) only with point-in-time recovery enabled.
func (a *Admin) checkSnapshotTime(dbName, field string, ts *timestamppb.Timestamp, needPITR bool) error {
	if err := ts.CheckValid(); err != nil {
		return status.Errorf(codes.InvalidArgument, "%s is invalid: %v", field, err)
	}
	at := ts.AsTime()
	if !at.Truncate(time.Minute).Equal(at) {
		return status.Errorf(codes.InvalidArgument, "%s must be a whole minute.", field)
	}
	a.mu.Lock()
	db, err := a.database(dbName)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	pitr := db.PointInTimeRecoveryEnablement == adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED
	_, earliest := a.versionWindow(db)
	now := a.now()
	a.mu.Unlock()
	if needPITR && !pitr {
		return status.Error(codes.FailedPrecondition, "Point-in-Time Recovery must be enabled in order to execute exports at a snapshot timestamp")
	}
	if at.After(now) {
		return status.Errorf(codes.InvalidArgument, "%s is in the future.", field)
	}
	// earliest is rarely a whole minute: the first whole minute at or
	// after it is the earliest snapshot.
	if at.Before(earliest) {
		return status.Errorf(codes.FailedPrecondition, "%s %s is earlier than the database's earliest version time %s.",
			field, at.Format(time.RFC3339), earliest.Format(time.RFC3339Nano))
	}
	return nil
}

// readTimeOf is the read time a read request carries: its read_time, or
// the read_only.read_time of the transaction it begins (new_transaction,
// or BeginTransaction's options); nil for none.
func readTimeOf(m protoreflect.Message) *timestamppb.Timestamp {
	fields := m.Descriptor().Fields()
	if fd := fields.ByName("read_time"); fd != nil && fd.Message() != nil && m.Has(fd) {
		ts, _ := m.Get(fd).Message().Interface().(*timestamppb.Timestamp)
		return ts
	}
	for _, n := range []protoreflect.Name{"new_transaction", "options"} {
		if fd := fields.ByName(n); fd != nil && fd.Message() != nil && m.Has(fd) {
			if o, ok := m.Get(fd).Message().Interface().(*firestorepb.TransactionOptions); ok && o.GetReadOnly().GetReadTime() != nil {
				return o.GetReadOnly().GetReadTime()
			}
		}
	}
	return nil
}

// readRequestDatabase is the database a read request names.
func readRequestDatabase(m protoreflect.Message) string {
	fields := m.Descriptor().Fields()
	for _, n := range []protoreflect.Name{"database", "parent", "name"} {
		if fd := fields.ByName(n); fd != nil && fd.Kind() == protoreflect.StringKind {
			if db := databaseOf(m.Get(fd).String()); db != "" {
				return db
			}
		}
	}
	return ""
}

// The reads that carry a read time, over gRPC.
var readTimeGRPC = map[string]func() proto.Message{
	"GetDocument":         func() proto.Message { return &firestorepb.GetDocumentRequest{} },
	"BatchGetDocuments":   func() proto.Message { return &firestorepb.BatchGetDocumentsRequest{} },
	"ListDocuments":       func() proto.Message { return &firestorepb.ListDocumentsRequest{} },
	"RunQuery":            func() proto.Message { return &firestorepb.RunQueryRequest{} },
	"RunAggregationQuery": func() proto.Message { return &firestorepb.RunAggregationQueryRequest{} },
	"ListCollectionIds":   func() proto.Message { return &firestorepb.ListCollectionIdsRequest{} },
	"PartitionQuery":      func() proto.Message { return &firestorepb.PartitionQueryRequest{} },
	"BeginTransaction":    func() proto.Message { return &firestorepb.BeginTransactionRequest{} },
}

// checkRequestReadTime is the refusal of a decoded read request, or nil.
func (a *Admin) checkRequestReadTime(req proto.Message) error {
	m := req.ProtoReflect()
	ts := readTimeOf(m)
	if ts == nil || ts.CheckValid() != nil {
		return nil
	}
	db := readRequestDatabase(m)
	if db == "" {
		return nil
	}
	return a.checkReadTime(db, ts.AsTime())
}

// readTimeRefusal decodes r, a read over gRPC or REST, putting its body
// back, and is its refusal when its read time is outside the window.
func (g *EditionGuard) readTimeRefusal(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		if !strings.HasPrefix(r.URL.Path, firestoreService) {
			return nil
		}
		mk, ok := readTimeGRPC[strings.TrimPrefix(r.URL.Path, firestoreService)]
		if !ok || r.Body == nil {
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, maxGuardedBody+1))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
		if err != nil || len(b) > maxGuardedBody {
			return nil
		}
		msg, ok := grpcMessage(b, r.Header.Get("Grpc-Encoding"))
		req := mk()
		if !ok || proto.Unmarshal(msg, req) != nil {
			return nil
		}
		return g.a.checkRequestReadTime(req)
	}
	m := restDocumentsPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return nil
	}
	var raw string
	switch {
	case r.Method == http.MethodGet:
		raw = r.URL.Query().Get("readTime")
	case r.Method == http.MethodPost && m[3] != "" && r.Body != nil:
		b, err := io.ReadAll(io.LimitReader(r.Body, maxGuardedBody+1))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
		if err != nil || len(b) > maxGuardedBody {
			return nil
		}
		var body struct {
			ReadTime       string `json:"readTime"`
			NewTransaction struct {
				ReadOnly struct {
					ReadTime string `json:"readTime"`
				} `json:"readOnly"`
			} `json:"newTransaction"`
			Options struct {
				ReadOnly struct {
					ReadTime string `json:"readTime"`
				} `json:"readOnly"`
			} `json:"options"`
		}
		if json.Unmarshal(b, &body) != nil {
			return nil
		}
		raw = body.ReadTime
		if raw == "" {
			raw = body.NewTransaction.ReadOnly.ReadTime
		}
		if raw == "" {
			raw = body.Options.ReadOnly.ReadTime
		}
	}
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil // the emulator's to refuse
	}
	return g.a.checkReadTime(m[1], t)
}

// The reads the guard claimed for their read time, answered with the
// refusal.

func (e editionRefusals) refuseRead(req proto.Message) error {
	if err := e.g.a.checkRequestReadTime(req); err != nil {
		return err
	}
	return status.Error(codes.Internal, "the call was claimed but not refused")
}

func (e editionRefusals) GetDocument(_ context.Context, r *firestorepb.GetDocumentRequest) (*firestorepb.Document, error) {
	return nil, e.refuseRead(r)
}

func (e editionRefusals) BatchGetDocuments(r *firestorepb.BatchGetDocumentsRequest, _ firestorepb.Firestore_BatchGetDocumentsServer) error {
	return e.refuseRead(r)
}

func (e editionRefusals) ListDocuments(_ context.Context, r *firestorepb.ListDocumentsRequest) (*firestorepb.ListDocumentsResponse, error) {
	return nil, e.refuseRead(r)
}

func (e editionRefusals) RunQuery(r *firestorepb.RunQueryRequest, _ firestorepb.Firestore_RunQueryServer) error {
	return e.refuseRead(r)
}

func (e editionRefusals) RunAggregationQuery(r *firestorepb.RunAggregationQueryRequest, _ firestorepb.Firestore_RunAggregationQueryServer) error {
	return e.refuseRead(r)
}

func (e editionRefusals) ListCollectionIds(_ context.Context, r *firestorepb.ListCollectionIdsRequest) (*firestorepb.ListCollectionIdsResponse, error) {
	return nil, e.refuseRead(r)
}

func (e editionRefusals) PartitionQuery(_ context.Context, r *firestorepb.PartitionQueryRequest) (*firestorepb.PartitionQueryResponse, error) {
	return nil, e.refuseRead(r)
}

func (e editionRefusals) BeginTransaction(_ context.Context, r *firestorepb.BeginTransactionRequest) (*firestorepb.BeginTransactionResponse, error) {
	return nil, e.refuseRead(r)
}
