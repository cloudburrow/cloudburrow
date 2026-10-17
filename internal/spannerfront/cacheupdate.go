package spannerfront

import (
	"context"
	"hash/fnv"
	"io"
	"net/http"
	"regexp"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// FetchCacheUpdate (#1523). Google documents it as the call that warms a
// client's location-aware routing cache, "key recipes and server
// information for a given database", streamed because an update can be
// larger than one message (google/spanner/v1/spanner.proto). The emulator
// is one server, with no splits or replicas to route a key to, so the
// front answers, for a database that exists, one CacheUpdate with the
// database's ID and no ranges, groups or recipes: a cache with nothing in
// it, which leaves a client sending every call to the endpoint it has, as
// it does with no cache. The emulator answers it UNIMPLEMENTED with an
// empty message.

const fetchCacheUpdate = "/google.spanner.v1.Spanner/FetchCacheUpdate"

// restCacheUpdate is its REST binding, POST /v1/{database}:cacheUpdate.
var restCacheUpdate = regexp.MustCompile(`^/v1/(` + pDB + `):cacheUpdate$`)

// cacheData is the Spanner data service as far as the front serves it.
type cacheData struct {
	spannerpb.UnimplementedSpannerServer
	s *Instances
}

func (d cacheData) FetchCacheUpdate(req *spannerpb.FetchCacheUpdateRequest, stream spannerpb.Spanner_FetchCacheUpdateServer) error {
	u, err := d.s.cacheUpdate(stream.Context(), req)
	if err != nil {
		return err
	}
	return stream.Send(u)
}

// cacheUpdate is the one update the front sends for req.
func (s *Instances) cacheUpdate(ctx context.Context, req *spannerpb.FetchCacheUpdateRequest) (*spannerpb.CacheUpdate, error) {
	if !databaseNameRe.MatchString(req.GetDatabase()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid FetchCacheUpdate request: database %q is not projects/{project}/instances/{instance}/databases/{database}.", req.GetDatabase())
	}
	if req.GetMaxRecipeCount() < 0 || req.GetMaxRangeCount() < 0 {
		return nil, status.Error(codes.InvalidArgument, "Invalid FetchCacheUpdate request: max_recipe_count and max_range_count cannot be negative.")
	}
	db, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: req.GetDatabase()})
	if err != nil {
		return nil, err
	}
	return &spannerpb.CacheUpdate{DatabaseId: databaseID(db)}, nil
}

// databaseID is the database's internal ID: a database made again under
// the same name has a new one, as Google's does, since its creation time
// is part of it.
func databaseID(db *databasepb.Database) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, db.GetName())
	_, _ = io.WriteString(h, db.GetCreateTime().AsTime().String())
	if id := h.Sum64(); id != 0 {
		return id
	}
	return 1
}

// serveRESTCacheUpdate answers the REST binding: a server-streaming
// method's REST response is the JSON array of its messages.
func (s *Instances) serveRESTCacheUpdate(w http.ResponseWriter, r *http.Request, database string) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "reading the request: %v", err))
		return
	}
	req := &spannerpb.FetchCacheUpdateRequest{}
	if len(b) > 0 {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, req); err != nil {
			writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
			return
		}
	}
	req.Database = database
	u, err := s.cacheUpdate(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	out, err := protojson.Marshal(u)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write([]byte("["))
	_, _ = w.Write(out)
	_, _ = w.Write([]byte("]"))
}
