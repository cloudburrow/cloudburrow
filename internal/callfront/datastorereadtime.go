package callfront

// Datastore reads at a past read time (#1942).
//
// Google serves a Datastore mode database's entities at an earlier
// read_time, within its version window (an hour, seven days with
// point-in-time recovery). The Datastore emulator keeps no earlier
// versions: measured, a Lookup at a read_time before a write answers the
// entity as written, with no update_time. So that a read never silently
// answers today's entities for an earlier time, the front refuses a
// Lookup, RunQuery, RunAggregationQuery or BeginTransaction whose read time
// is more than readTimeSlack in the past, UNIMPLEMENTED, naming why; a
// read time of about now is served as now.

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// readTimeSlack is how far in the past a read time may be and still be
// served, as the emulator's entities now.
const readTimeSlack = 5 * time.Second

// datastoreReadTimes refuses Datastore reads at an earlier read time.
type datastoreReadTimes struct{ now func() time.Time }

func newDatastoreReadTimes() *datastoreReadTimes {
	return &datastoreReadTimes{now: time.Now}
}

var datastoreReads = map[string]func() proto.Message{
	"Lookup":              func() proto.Message { return &pb.LookupRequest{} },
	"RunQuery":            func() proto.Message { return &pb.RunQueryRequest{} },
	"RunAggregationQuery": func() proto.Message { return &pb.RunAggregationQueryRequest{} },
	"BeginTransaction":    func() proto.Message { return &pb.BeginTransactionRequest{} },
}

// datastoreReadTime is the read time of a decoded Datastore read, or nil.
func datastoreReadTime(m proto.Message) *timestamppb.Timestamp {
	var ro *pb.ReadOptions
	switch x := m.(type) {
	case *pb.LookupRequest:
		ro = x.GetReadOptions()
	case *pb.RunQueryRequest:
		ro = x.GetReadOptions()
	case *pb.RunAggregationQueryRequest:
		ro = x.GetReadOptions()
	case *pb.BeginTransactionRequest:
		return x.GetTransactionOptions().GetReadOnly().GetReadTime()
	}
	if rt := ro.GetReadTime(); rt != nil {
		return rt
	}
	return ro.GetNewTransaction().GetReadOnly().GetReadTime()
}

// peekBody reads r's body, putting it back.
func peekBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.ContentLength > maxPeek {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
	return b, err == nil && len(b) <= maxPeek
}

// firstGRPCMessage is the first message of a gRPC request body.
func firstGRPCMessage(b []byte, encoding string) ([]byte, bool) {
	if len(b) < 5 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if uint64(len(b)-5) < uint64(n) {
		return nil, false
	}
	msg := b[5 : 5+n]
	if b[0] == 0 {
		return msg, true
	}
	if encoding != "gzip" {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(msg))
	if err != nil {
		return nil, false
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxPeek))
	return out, err == nil
}

// readTime is the read time r's request carries, or the zero time.
func (d *datastoreReadTimes) readTime(r *http.Request) time.Time {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		mk, ok := datastoreReads[strings.TrimPrefix(r.URL.Path, datastoreServicePrefix)]
		if !ok || !strings.HasPrefix(r.URL.Path, datastoreServicePrefix) {
			return time.Time{}
		}
		b, ok := peekBody(r)
		if !ok {
			return time.Time{}
		}
		msg, ok := firstGRPCMessage(b, r.Header.Get("Grpc-Encoding"))
		req := mk()
		if !ok || proto.Unmarshal(msg, req) != nil {
			return time.Time{}
		}
		if ts := datastoreReadTime(req); ts != nil && ts.CheckValid() == nil {
			return ts.AsTime()
		}
		return time.Time{}
	}
	m := restCall.FindStringSubmatch(r.URL.Path)
	if r.Method != http.MethodPost || m == nil || r.Header.Get("Content-Encoding") != "" {
		return time.Time{}
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, ":")+1:]
	if method != "lookup" && method != "runQuery" && method != "runAggregationQuery" && method != "beginTransaction" {
		return time.Time{}
	}
	b, ok := peekBody(r)
	if !ok {
		return time.Time{}
	}
	type readOnly struct {
		ReadTime string `json:"readTime"`
	}
	var body struct {
		ReadOptions struct {
			ReadTime       string `json:"readTime"`
			NewTransaction struct {
				ReadOnly readOnly `json:"readOnly"`
			} `json:"newTransaction"`
		} `json:"readOptions"`
		TransactionOptions struct {
			ReadOnly readOnly `json:"readOnly"`
		} `json:"transactionOptions"`
	}
	if json.Unmarshal(b, &body) != nil {
		return time.Time{}
	}
	for _, s := range []string{body.ReadOptions.ReadTime, body.ReadOptions.NewTransaction.ReadOnly.ReadTime, body.TransactionOptions.ReadOnly.ReadTime} {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (d *datastoreReadTimes) refusal(r *http.Request) error {
	t := d.readTime(r)
	if t.IsZero() || !t.Before(d.now().Add(-readTimeSlack)) {
		return nil
	}
	return status.Errorf(codes.Unimplemented, "a read at read_time %s is not served here: the Datastore emulator keeps no earlier versions of its entities, so reads are served at the current time alone", t.UTC().Format(time.RFC3339Nano))
}

// Claims reports whether r is a read at an earlier read time.
func (d *datastoreReadTimes) Claims(r *http.Request) bool { return d.refusal(r) != nil }

// ServeHTTP answers a call Claims claimed with its refusal.
func (d *datastoreReadTimes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := d.refusal(r)
	if err == nil {
		err = status.Error(codes.Unavailable, "the read was claimed but not refused; retry it")
	}
	st, _ := status.FromError(err)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		netfwd.WriteGRPCStatus(w, int(st.Code()), st.Message())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	code := apierror.From(err).HTTPStatus()
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": st.Message(), "status": rpccode.Code(st.Code()).String()}})
}
