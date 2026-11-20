package run

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestV1Authorize (#1879): the v1 API asks the authorizer about the v2
// request each v1 request stands for, with its Authorization header as
// the call's metadata, and answers a refusal as the v1 error.
func TestV1Authorize(t *testing.T) {
	var asked []string
	var bearer string
	UseV1Authorize(&V1Authorizer{
		Method: func(ctx context.Context, full string, _ proto.Message) error {
			asked = append(asked, full)
			md, _ := metadata.FromIncomingContext(ctx)
			if v := md.Get("authorization"); len(v) > 0 {
				bearer = v[0]
			}
			if strings.HasSuffix(full, "/GetService") {
				return status.Error(codes.PermissionDenied, "Permission 'run.services.get' denied")
			}
			return nil
		},
		Permission: func(_ context.Context, permission, _ string) error {
			asked = append(asked, permission)
			return nil
		},
	})
	t.Cleanup(func() { UseV1Authorize(nil) })
	srv := v1Server(t)
	get := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/apis/serving.knative.dev/v1/namespaces/p/services"); code != http.StatusOK {
		t.Fatalf("services.list = %d", code)
	}
	if code := get("/apis/serving.knative.dev/v1/namespaces/p/services/hello"); code != http.StatusForbidden {
		t.Fatalf("services.get refused by the authorizer = %d", code)
	}
	if code := get("/apis/serving.knative.dev/v1/namespaces/p/routes"); code != http.StatusOK {
		t.Fatalf("routes.list = %d", code)
	}
	want := []string{"/google.cloud.run.v2.Services/ListServices", "/google.cloud.run.v2.Services/GetService", "run.routes.list"}
	if strings.Join(asked, ",") != strings.Join(want, ",") || bearer != "Bearer tok" {
		t.Fatalf("asked %v with %q; want %v with the request's bearer", asked, bearer, want)
	}
}
