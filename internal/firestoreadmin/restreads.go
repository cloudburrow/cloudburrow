package firestoreadmin

// REST gets and lists at a read time or in a transaction (#1942).
//
// Google's REST API takes a document get's and a list's consistency in
// query parameters: readTime, an RFC 3339 timestamp, or transaction, the
// base64 bytes BeginTransaction answered
// (https://docs.cloud.google.com/firestore/docs/reference/rest/v1/projects.databases.documents/get
// and .../list). The emulator's REST gateway refuses an RFC 3339 readTime
// ("Only timestamps past epoch are supported.", measured: it reads only
// readTime.seconds and readTime.nanos), and never answers a get or list
// that names a transaction (measured: no answer in 10 seconds, read-only
// or read-write). Its gRPC GetDocument and ListDocuments take both, so the
// front serves these two REST calls through them, after the edition guard
// has applied the version window to the read time.

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RESTReads serves the REST gets and lists the emulator's gateway cannot
// answer: those with a readTime or a transaction.
type RESTReads struct{ a *Admin }

// RESTReads is the intercept for REST gets and lists at a read time or in
// a transaction. It goes after the edition guard, which refuses a read
// time outside the window first.
func (a *Admin) RESTReads() *RESTReads { return &RESTReads{a: a} }

// restRead is a REST get or list r names: the database, the path under
// its documents, and whether it is a consistent read; ok is false for any
// other call.
func restRead(r *http.Request) (db, path string, ok bool) {
	if r.Method != http.MethodGet || strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		return "", "", false
	}
	m := restDocumentsPath.FindStringSubmatch(r.URL.Path)
	if m == nil || m[3] != "" || strings.Trim(m[2], "/") == "" {
		return "", "", false
	}
	q := r.URL.Query()
	if q.Get("readTime") == "" && q.Get("transaction") == "" {
		return "", "", false
	}
	return m[1], strings.Trim(m[2], "/"), true
}

// Claims reports whether r is a REST get or list at a read time or in a
// transaction.
func (s *RESTReads) Claims(r *http.Request) bool {
	_, _, ok := restRead(r)
	return ok
}

// ServeHTTP answers the get or list through the emulator's gRPC API.
func (s *RESTReads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	db, path, ok := restRead(r)
	if !ok {
		writeError(w, status.Error(codes.Internal, "the call was claimed but is not a REST read"))
		return
	}
	msg, err := s.serve(r, db, path)
	if err != nil {
		writeError(w, err)
		return
	}
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the answer: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// consistency reads a REST read's readTime or transaction.
func consistency(q url.Values) (*timestamppb.Timestamp, []byte, error) {
	rt, tx := q.Get("readTime"), q.Get("transaction")
	if rt != "" && tx != "" {
		return nil, nil, status.Error(codes.InvalidArgument, "Only one of readTime and transaction may be set.")
	}
	if rt != "" {
		t, err := time.Parse(time.RFC3339Nano, rt)
		if err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "Invalid value at 'read_time' (type.googleapis.com/google.protobuf.Timestamp), Field 'readTime', Invalid time format: %q", rt)
		}
		return timestamppb.New(t), nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(tx)
	if err != nil {
		if b, err = base64.URLEncoding.DecodeString(tx); err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "Invalid value at 'transaction' (TYPE_BYTES), Base64 decoding failed for %q", tx)
		}
	}
	return nil, b, nil
}

// serve is the get's document, or the list's response.
func (s *RESTReads) serve(r *http.Request, db, path string) (proto.Message, error) {
	q := r.URL.Query()
	rt, tx, err := consistency(q)
	if err != nil {
		return nil, err
	}
	c, err := s.a.docs()
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if auth := r.Header.Get("Authorization"); auth != "" {
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", auth))
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "google-cloud-resource-prefix", db)
	var mask *firestorepb.DocumentMask
	if fp := q["mask.fieldPaths"]; len(fp) > 0 {
		mask = &firestorepb.DocumentMask{FieldPaths: fp}
	}
	segs := strings.Split(path, "/")
	if len(segs)%2 == 0 {
		req := &firestorepb.GetDocumentRequest{Name: db + "/documents/" + path, Mask: mask}
		if rt != nil {
			req.ConsistencySelector = &firestorepb.GetDocumentRequest_ReadTime{ReadTime: rt}
		} else {
			req.ConsistencySelector = &firestorepb.GetDocumentRequest_Transaction{Transaction: tx}
		}
		return c.GetDocument(ctx, req)
	}
	parent := db + "/documents"
	if len(segs) > 1 {
		parent += "/" + strings.Join(segs[:len(segs)-1], "/")
	}
	req := &firestorepb.ListDocumentsRequest{Parent: parent, CollectionId: segs[len(segs)-1], Mask: mask,
		PageToken: q.Get("pageToken"), OrderBy: q.Get("orderBy")}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid value at 'page_size' (TYPE_INT32), %q", v)
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("showMissing"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid value at 'show_missing' (TYPE_BOOL), %q", v)
		}
		req.ShowMissing = b
	}
	if rt != nil {
		req.ConsistencySelector = &firestorepb.ListDocumentsRequest_ReadTime{ReadTime: rt}
	} else {
		req.ConsistencySelector = &firestorepb.ListDocumentsRequest_Transaction{Transaction: tx}
	}
	return c.ListDocuments(ctx, req)
}
