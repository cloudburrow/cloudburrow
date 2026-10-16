// Package queryfront answers, in the Datastore and Firestore pods' call
// front (internal/callfront), the two query RPCs Google's emulators do not
// serve, from calls they do serve (#1405), as the Pub/Sub front answers
// topic labels (#949):
//
//   - Datastore's RunAggregationQuery (COUNT with up_to, SUM, AVG): the
//     nested query is run through the emulator's RunQuery, paging through
//     every batch, and the aggregates are computed here with the semantics
//     Google documents for google.datastore.v1.AggregationQuery.
//   - Firestore's PartitionQuery: the collection-group query is run
//     through the emulator's RunQuery, selecting document names alone, and
//     split into at most partition_count cursors at document references,
//     evenly spaced in the query's order, paged by page_size and
//     page_token.
//
// Each is served over gRPC and over its REST binding on the emulator's
// port. Whatever cannot be answered faithfully (a GQL aggregation, explain
// options) is UNIMPLEMENTED (501 over REST), naming why. Every other call
// passes to the emulator unchanged.
package queryfront

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// upstream is a lazily dialled gRPC connection to the emulator.
type upstream struct {
	addr string
	once sync.Once
	conn *grpc.ClientConn
	err  error
	// dial overrides grpc.NewClient (tests).
	dial func(addr string) (*grpc.ClientConn, error)
}

func (u *upstream) get() (*grpc.ClientConn, error) {
	u.once.Do(func() {
		dial := u.dial
		if dial == nil {
			dial = func(addr string) (*grpc.ClientConn, error) {
				return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
					grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
			}
		}
		u.conn, u.err = dial(u.addr)
	})
	if u.err != nil {
		return nil, status.Errorf(codes.Unavailable, "the emulator at %s: %v", u.addr, u.err)
	}
	return u.conn, nil
}

// skipped are the headers a forwarded call sets for itself.
var skipped = map[string]bool{
	"content-type": true, "user-agent": true, "te": true, "host": true,
	":authority": true, "content-length": true, "connection": true,
}

// forward is ctx carrying the incoming call's metadata (its routing
// headers: database, resource prefix) out to the emulator.
func forward(ctx context.Context) context.Context {
	in, _ := metadata.FromIncomingContext(ctx)
	out := metadata.MD{}
	for k, v := range in {
		if skipped[k] || strings.HasPrefix(k, "grpc-") {
			continue
		}
		out[k] = v
	}
	return metadata.NewOutgoingContext(ctx, out)
}

// forwardHTTP is ctx carrying a REST call's routing headers to the emulator.
func forwardHTTP(ctx context.Context, r *http.Request) context.Context {
	out := metadata.MD{}
	for _, k := range []string{"X-Goog-Request-Params", "Google-Cloud-Resource-Prefix", "Authorization"} {
		if v := r.Header.Get(k); v != "" {
			out.Set(strings.ToLower(k), v)
		}
	}
	return metadata.NewIncomingContext(ctx, out)
}

func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// readJSON decodes a REST call's body into m.
func readJSON(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err == nil && len(b) > 0 {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
	}
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid JSON payload: %v", err)
	}
	return nil
}

func writeReply(w http.ResponseWriter, m proto.Message, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// writeError renders err as Google's REST APIs do:
// {"error": {"code", "message", "status"}}.
func writeError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := apierror.From(err).HTTPStatus()
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	body.Error.Code, body.Error.Message, body.Error.Status = code, st.Message(), rpccode.Code(st.Code()).String()
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
