package queryfront

import (
	"context"
	"io"
	"net/http"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The calls of a named database the front answers (databases.go, #1885):
// each passes to the emulator through DatabasesClient. RunQuery and
// RunAggregationQuery are datastore.go's and explain.go's.

// Lookup passes a lookup in a named database to its emulator project.
func (d *Datastore) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.Lookup(forward(ctx), req)
}

// BeginTransaction begins a transaction in a named database's emulator
// project.
func (d *Datastore) BeginTransaction(ctx context.Context, req *pb.BeginTransactionRequest) (*pb.BeginTransactionResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.BeginTransaction(forward(ctx), req)
}

// Commit commits in a named database's emulator project.
func (d *Datastore) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.Commit(forward(ctx), req)
}

// Rollback rolls back in a named database's emulator project.
func (d *Datastore) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.Rollback(forward(ctx), req)
}

// AllocateIds allocates in a named database's emulator project.
func (d *Datastore) AllocateIds(ctx context.Context, req *pb.AllocateIdsRequest) (*pb.AllocateIdsResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.AllocateIds(forward(ctx), req)
}

// ReserveIds reserves in a named database's emulator project.
func (d *Datastore) ReserveIds(ctx context.Context, req *pb.ReserveIdsRequest) (*pb.ReserveIdsResponse, error) {
	c, err := d.emulator()
	if err != nil {
		return nil, err
	}
	return c.ReserveIds(forward(ctx), req)
}

// serveDatabaseREST answers a REST call naming a named database, in the
// body's form: JSON, or the protobuf the Java client sends.
func (d *Datastore) serveDatabaseREST(w http.ResponseWriter, r *http.Request) {
	m := datastoreDataREST.FindStringSubmatch(r.URL.Path)
	binary := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-protobuf")
	if d.sawDatabase != nil {
		if res := namesDatabase(r); res != "" {
			d.sawDatabase(res)
		}
	}
	var req request
	switch m[2] {
	case "lookup":
		req = &pb.LookupRequest{}
	case "runQuery":
		req = &pb.RunQueryRequest{}
	case "runAggregationQuery":
		req = &pb.RunAggregationQueryRequest{}
	case "beginTransaction":
		req = &pb.BeginTransactionRequest{}
	case "commit":
		req = &pb.CommitRequest{}
	case "rollback":
		req = &pb.RollbackRequest{}
	case "allocateIds":
		req = &pb.AllocateIdsRequest{}
	default:
		req = &pb.ReserveIdsRequest{}
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxPeek))
	if err == nil && len(b) > 0 {
		if binary {
			err = proto.Unmarshal(b, req)
		} else {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, req)
		}
	}
	if err != nil {
		writeDatabaseError(w, binary, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err))
		return
	}
	req.ProtoReflect().Set(req.ProtoReflect().Descriptor().Fields().ByName("project_id"), protoreflect.ValueOfString(m[1]))
	ctx := forwardHTTP(r.Context(), r)
	var resp proto.Message
	switch x := req.(type) {
	case *pb.LookupRequest:
		resp, err = d.Lookup(ctx, x)
	case *pb.RunQueryRequest:
		resp, err = d.RunQuery(ctx, x)
	case *pb.RunAggregationQueryRequest:
		resp, err = d.RunAggregationQuery(ctx, x)
	case *pb.BeginTransactionRequest:
		resp, err = d.BeginTransaction(ctx, x)
	case *pb.CommitRequest:
		resp, err = d.Commit(ctx, x)
	case *pb.RollbackRequest:
		resp, err = d.Rollback(ctx, x)
	case *pb.AllocateIdsRequest:
		resp, err = d.AllocateIds(ctx, x)
	case *pb.ReserveIdsRequest:
		resp, err = d.ReserveIds(ctx, x)
	}
	if err != nil {
		writeDatabaseError(w, binary, err)
		return
	}
	if !binary {
		writeReply(w, resp, nil)
		return
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		writeDatabaseError(w, binary, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// writeDatabaseError is writeError, or, to a protobuf call, the
// google.rpc.Status the Java client reads.
func writeDatabaseError(w http.ResponseWriter, binary bool, err error) {
	if !binary {
		writeError(w, err)
		return
	}
	st, _ := status.FromError(err)
	b, _ := proto.Marshal(&statuspb.Status{Code: int32(st.Code()), Message: st.Message()})
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(apierror.From(err).HTTPStatus())
	_, _ = w.Write(b)
}
