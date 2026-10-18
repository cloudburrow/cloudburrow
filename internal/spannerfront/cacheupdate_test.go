package spannerfront_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFetchCacheUpdateIsAnEmptyCache (#1523): one update with the
// database's ID and nothing to cache, over gRPC and REST; NOT_FOUND for a
// missing database, INVALID_ARGUMENT for a malformed name.
func TestFetchCacheUpdateIsAnEmptyCache(t *testing.T) {
	addr, _ := serve(t)
	ctx := context.Background()
	c := spannerpb.NewSpannerClient(lastConn)
	fetch := func(db string) ([]*spannerpb.CacheUpdate, error) {
		stream, err := c.FetchCacheUpdate(ctx, &spannerpb.FetchCacheUpdateRequest{Database: db})
		if err != nil {
			return nil, err
		}
		var out []*spannerpb.CacheUpdate
		for {
			u, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			out = append(out, u)
		}
	}
	got, err := fetch("projects/p/instances/i/databases/d")
	if err != nil || len(got) != 1 || got[0].GetDatabaseId() == 0 || len(got[0].GetRange()) != 0 {
		t.Fatalf("FetchCacheUpdate = %v, %v, want one update with an ID and no ranges", got, err)
	}
	if _, err := fetch("projects/p/instances/i/databases/none"); status.Code(err) != codes.NotFound {
		t.Errorf("FetchCacheUpdate of a missing database = %v, want NotFound", err)
	}
	if _, err := fetch("projects/p/databases/d"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FetchCacheUpdate of a malformed name = %v, want InvalidArgument", err)
	}

	resp, err := http.Post("http://"+addr+"/v1/projects/p/instances/i/databases/d:cacheUpdate", "application/json", strings.NewReader(`{"maxRecipeCount":5}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 || len(body) != 1 || body[0]["databaseId"] == nil {
		t.Errorf("REST cacheUpdate = %d %v %v, want a JSON array of one update", resp.StatusCode, body, err)
	}
}
