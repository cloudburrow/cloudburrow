package queryfront

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	pb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const firestorePartition = "/google.firestore.v1.Firestore/PartitionQuery"

var firestorePartitionREST = regexp.MustCompile(`^/v1/(projects/[^/]+/databases/[^/]+/documents(?:/.+)?):partitionQuery$`)

// Firestore answers PartitionQuery in front of the Firestore emulator.
type Firestore struct {
	pb.UnimplementedFirestoreServer
	up   upstream
	grpc *grpc.Server
	// client overrides the emulator's client (tests).
	client pb.FirestoreClient
}

// NewFirestore returns the front for the Firestore emulator at emulator
// (host:port).
func NewFirestore(emulator string) *Firestore {
	f := &Firestore{up: upstream{addr: emulator}}
	f.grpc = grpc.NewServer()
	pb.RegisterFirestoreServer(f.grpc, f)
	return f
}

// Claims reports whether r is a PartitionQuery call, over gRPC or REST.
func (f *Firestore) Claims(r *http.Request) bool {
	if isGRPC(r) {
		return r.URL.Path == firestorePartition
	}
	return r.Method == http.MethodPost && firestorePartitionREST.MatchString(r.URL.Path)
}

// ServeHTTP answers a call Claims claimed.
func (f *Firestore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isGRPC(r) {
		f.grpc.ServeHTTP(w, r)
		return
	}
	req := &pb.PartitionQueryRequest{}
	if err := readJSON(r, req); err != nil {
		writeError(w, err)
		return
	}
	req.Parent = firestorePartitionREST.FindStringSubmatch(r.URL.Path)[1]
	resp, err := f.PartitionQuery(forwardHTTP(r.Context(), r), req)
	writeReply(w, resp, err)
}

func (f *Firestore) emulator() (pb.FirestoreClient, error) {
	if f.client != nil {
		return f.client, nil
	}
	conn, err := f.up.get()
	if err != nil {
		return nil, err
	}
	return pb.NewFirestoreClient(conn), nil
}

const nameField = "__name__"

// partitionable checks the query is one Google partitions: a collection
// group (all descendants) ordered by name ascending alone, with no
// filters, limits, offsets or cursors.
func partitionable(q *pb.StructuredQuery) error {
	if q == nil {
		return status.Error(codes.InvalidArgument, "PartitionQuery needs a structured_query")
	}
	if len(q.GetFrom()) != 1 || !q.GetFrom()[0].GetAllDescendants() {
		return status.Error(codes.InvalidArgument, "PartitionQuery needs a collection group query: one collection selector with all_descendants")
	}
	if q.GetWhere() != nil || q.GetOffset() != 0 || q.GetLimit() != nil || q.GetStartAt() != nil || q.GetEndAt() != nil || q.GetFindNearest() != nil {
		return status.Error(codes.InvalidArgument, "PartitionQuery does not support filters, limits, offsets, start or end cursors, or vector search")
	}
	for _, o := range q.GetOrderBy() {
		if o.GetField().GetFieldPath() != nameField || o.GetDirection() == pb.StructuredQuery_DESCENDING {
			return status.Error(codes.InvalidArgument, "PartitionQuery supports ordering by __name__ ascending alone")
		}
	}
	if len(q.GetOrderBy()) > 1 {
		return status.Error(codes.InvalidArgument, "PartitionQuery supports ordering by __name__ ascending alone")
	}
	return nil
}

// splitPoints is at most count indexes into n ordered documents that split
// them into partitions as even as whole documents allow; none when there
// are too few documents to split.
func splitPoints(n int, count int64) []int {
	p := int64(n - 1)
	if count < p {
		p = count
	}
	if p <= 0 {
		return nil
	}
	out := make([]int, 0, p)
	for i := int64(1); i <= p; i++ {
		out = append(out, int(i*int64(n)/(p+1)))
	}
	return out
}

func pageToken(off int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("partition:" + strconv.Itoa(off)))
}

func pageOffset(tok string) (int, error) {
	if tok == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	s, ok := strings.CutPrefix(string(b), "partition:")
	off, aerr := strconv.Atoi(s)
	if err != nil || !ok || aerr != nil || off < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "invalid page_token %q", tok)
	}
	return off, nil
}

// PartitionQuery runs the collection-group query through the emulator,
// for the documents' names alone, and returns cursors that split them.
func (f *Firestore) PartitionQuery(ctx context.Context, req *pb.PartitionQueryRequest) (*pb.PartitionQueryResponse, error) {
	q := req.GetStructuredQuery()
	if err := partitionable(q); err != nil {
		return nil, err
	}
	if req.GetPartitionCount() <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "partition_count must be positive, got %d", req.GetPartitionCount())
	}
	if req.GetPageSize() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "page_size must not be negative, got %d", req.GetPageSize())
	}
	off, err := pageOffset(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	client, err := f.emulator()
	if err != nil {
		return nil, err
	}
	name := &pb.StructuredQuery_FieldReference{FieldPath: nameField}
	run := &pb.RunQueryRequest{
		Parent: req.GetParent(),
		QueryType: &pb.RunQueryRequest_StructuredQuery{StructuredQuery: &pb.StructuredQuery{
			Select:  &pb.StructuredQuery_Projection{Fields: []*pb.StructuredQuery_FieldReference{name}},
			From:    q.GetFrom(),
			OrderBy: []*pb.StructuredQuery_Order{{Field: name, Direction: pb.StructuredQuery_ASCENDING}},
		}},
	}
	if rt := req.GetReadTime(); rt != nil {
		run.ConsistencySelector = &pb.RunQueryRequest_ReadTime{ReadTime: rt}
	}
	stream, err := client.RunQuery(forward(ctx), run)
	if err != nil {
		return nil, err
	}
	var names []string
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if d := resp.GetDocument(); d != nil {
			names = append(names, d.GetName())
		}
	}
	points := splitPoints(len(names), req.GetPartitionCount())
	if off > len(points) {
		off = len(points)
	}
	end := len(points)
	if s := int(req.GetPageSize()); s > 0 && off+s < end {
		end = off + s
	}
	out := &pb.PartitionQueryResponse{}
	for _, i := range points[off:end] {
		out.Partitions = append(out.Partitions, &pb.Cursor{Values: []*pb.Value{{ValueType: &pb.Value_ReferenceValue{ReferenceValue: names[i]}}}})
	}
	if end < len(points) {
		out.NextPageToken = pageToken(end)
	}
	return out, nil
}
