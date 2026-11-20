package datastoreversions

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const grpcService = "/google.datastore.v1.Datastore/"

// restCall is a Datastore REST data call.
var restCall = regexp.MustCompile(`^/v1/projects/([^/:]+):(lookup|runQuery|runAggregationQuery|beginTransaction|commit|rollback)$`)

// maxPeek is the largest request body read.
const maxPeek = 32 << 20

var restMethods = map[string]string{
	"lookup": "Lookup", "runQuery": "RunQuery", "runAggregationQuery": "RunAggregationQuery",
	"beginTransaction": "BeginTransaction", "commit": "Commit", "rollback": "Rollback",
}

func newRequest(method string) proto.Message {
	switch method {
	case "Lookup":
		return &pb.LookupRequest{}
	case "RunQuery":
		return &pb.RunQueryRequest{}
	case "RunAggregationQuery":
		return &pb.RunAggregationQueryRequest{}
	case "BeginTransaction":
		return &pb.BeginTransactionRequest{}
	case "Commit":
		return &pb.CommitRequest{}
	case "Rollback":
		return &pb.RollbackRequest{}
	}
	return nil
}

// Method is the Datastore method r calls ("Commit", "Lookup", ...), of
// those the log takes part in, or "".
func Method(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		m, ok := strings.CutPrefix(r.URL.Path, grpcService)
		if !ok || newRequest(m) == nil {
			return ""
		}
		return m
	}
	if r.Method != http.MethodPost {
		return ""
	}
	if m := restCall.FindStringSubmatch(r.URL.Path); m != nil {
		return restMethods[m[2]]
	}
	return ""
}

// Request decodes r's request, putting its body back: the method, the
// request (its project_id the REST path's when its body names none), and
// whether it could be read.
func Request(r *http.Request) (string, proto.Message, bool) {
	method := Method(r)
	if method == "" || r.Body == nil || r.ContentLength > maxPeek {
		return "", nil, false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	if err != nil || len(b) > maxPeek {
		return "", nil, false
	}
	req := newRequest(method)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		msg, ok := grpcMessage(b, r.Header.Get("Grpc-Encoding"))
		if !ok || proto.Unmarshal(msg, req) != nil {
			return "", nil, false
		}
		return method, req, true
	}
	if r.Header.Get("Content-Encoding") != "" {
		return "", nil, false
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-protobuf") {
		err = proto.Unmarshal(b, req)
	} else if len(bytes.TrimSpace(b)) > 0 {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, req)
	}
	if err != nil {
		return "", nil, false
	}
	m := req.ProtoReflect()
	if fd := m.Descriptor().Fields().ByName("project_id"); m.Get(fd).String() == "" {
		if p := restCall.FindStringSubmatch(r.URL.Path); p != nil {
			m.Set(fd, protoreflect.ValueOfString(p[1]))
		}
	}
	return method, req, true
}

// grpcMessage is the first message of a gRPC request body.
func grpcMessage(b []byte, encoding string) ([]byte, bool) {
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

// ReadOptions is a request's read options, a BeginTransaction's read-only
// read time as one.
func ReadOptions(req proto.Message) *pb.ReadOptions {
	switch x := req.(type) {
	case *pb.LookupRequest:
		return x.GetReadOptions()
	case *pb.RunQueryRequest:
		return x.GetReadOptions()
	case *pb.RunAggregationQueryRequest:
		return x.GetReadOptions()
	case *pb.BeginTransactionRequest:
		if rt := x.GetTransactionOptions().GetReadOnly().GetReadTime(); rt != nil {
			return &pb.ReadOptions{ConsistencyType: &pb.ReadOptions_ReadTime{ReadTime: rt}}
		}
	}
	return nil
}

// RequestTime is the read time a request carries, its read-only
// transaction's included, or the zero time; a transaction the log began
// is not one (its time was checked when it began).
func RequestTime(req proto.Message) time.Time {
	ro := ReadOptions(req)
	ts := ro.GetReadTime()
	if ts == nil {
		ts = ro.GetNewTransaction().GetReadOnly().GetReadTime()
	}
	if ts == nil || ts.CheckValid() != nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// Claims reports whether the front answers r for the log: every commit
// (recorded), a read at an earlier time, and the calls of a transaction
// the log began.
func (l *Log) Claims(r *http.Request) bool {
	method, req, ok := Request(r)
	if !ok {
		return method == "Commit"
	}
	switch x := req.(type) {
	case *pb.CommitRequest:
		return true
	case *pb.RollbackRequest:
		return IsTransaction(x.GetTransaction())
	}
	ro := ReadOptions(req)
	if IsTransaction(ro.GetTransaction()) {
		return true
	}
	t := RequestTime(req)
	return !t.IsZero() && !t.After(l.now())
}
