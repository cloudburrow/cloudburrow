package kmsinventory

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/kms/inventory/apiv1/inventorypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const key = "projects/p/locations/us/keyRings/r/cryptoKeys/k"

type fakeKeys struct{}

func (fakeKeys) GetCryptoKey(_ context.Context, in *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if in.GetName() != key {
		return nil, apierror.NotFound("%s not found", in.GetName())
	}
	return &kmspb.CryptoKey{Name: key}, nil
}

func (fakeKeys) ListCryptoKeys(context.Context, *kmspb.ListCryptoKeysRequest) (*kmspb.ListCryptoKeysResponse, error) {
	return &kmspb.ListCryptoKeysResponse{CryptoKeys: []*kmspb.CryptoKey{{Name: key}}}, nil
}

func (fakeKeys) KeyRingsInProject(string) ([]*kmspb.KeyRing, error) {
	return []*kmspb.KeyRing{{Name: "projects/p/locations/us/keyRings/r"}}, nil
}

func server() *Server {
	src := func(_ context.Context, project string) ([]Resource, error) {
		if project != "p" {
			return nil, nil
		}
		return []Resource{
			{Name: "//storage.googleapis.com/b", ProjectID: "p", CloudProduct: "storage", ResourceType: "storage.googleapis.com/Bucket",
				Location: "us", Keys: []string{key}, Versions: []string{key + "/cryptoKeyVersions/1"}},
			{Name: "//bigquery.googleapis.com/projects/p/datasets/d", ProjectID: "p", CloudProduct: "bigquery",
				ResourceType: "bigquery.googleapis.com/Dataset", Location: "us", Keys: []string{key}},
			{Name: "//bigquery.googleapis.com/projects/p/datasets/other", ProjectID: "p", CloudProduct: "bigquery",
				ResourceType: "bigquery.googleapis.com/Dataset", Location: "us", Keys: []string{key + "2"}},
			{Name: "//untracked/x", ProjectID: "p", ResourceType: "untracked.googleapis.com/X", Keys: []string{key}},
		}, nil
	}
	return &Server{KMS: func() Keys { return fakeKeys{} }, Sources: func() []Source { return []Source{src} }}
}

func TestSummaryCountsWhatTheKeyProtects(t *testing.T) {
	s, err := server().GetProtectedResourcesSummary(context.Background(), &inventorypb.GetProtectedResourcesSummaryRequest{Name: key})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetResourceCount() != 2 || s.GetProjectCount() != 1 || s.GetCloudProducts()["storage"] != 1 || s.GetLocations()["us"] != 2 ||
		len(s.GetWarnings()) != 1 || s.GetWarnings()[0].GetWarningCode() != inventorypb.Warning_ORG_LESS_PROJECT_PARTIAL_DATA {
		t.Errorf("summary = %v", s)
	}
	v, err := server().GetProtectedResourcesSummary(context.Background(), &inventorypb.GetProtectedResourcesSummaryRequest{Name: key + "/cryptoKeyVersions/1"})
	if err != nil || v.GetResourceCount() != 1 {
		t.Errorf("version summary = %v, %v", v, err)
	}
	if _, err := server().GetProtectedResourcesSummary(context.Background(), &inventorypb.GetProtectedResourcesSummaryRequest{Name: key + "x"}); status.Code(err) != codes.NotFound {
		t.Errorf("missing key: %v", err)
	}
}

func TestSearchFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	r, err := server().SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/p", CryptoKey: key, PageSize: 1})
	if err != nil || len(r.GetProtectedResources()) != 1 || r.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", r, err)
	}
	r2, err := server().SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/p", CryptoKey: key, PageSize: 1, PageToken: r.GetNextPageToken()})
	if err != nil || len(r2.GetProtectedResources()) != 1 || r2.GetNextPageToken() != "" || r2.GetProtectedResources()[0].GetCryptoKeyVersion() == "" {
		t.Errorf("second page = %v, %v", r2, err)
	}
	f, err := server().SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/p", CryptoKey: key, ResourceTypes: []string{".*Bucket"}})
	if err != nil || len(f.GetProtectedResources()) != 1 {
		t.Errorf("filtered = %v, %v", f, err)
	}
	for _, bad := range []*inventorypb.SearchProtectedResourcesRequest{
		{Scope: "projects/p", CryptoKey: key, ResourceTypes: []string{"nothing.googleapis.com/X"}},
		{Scope: "projects/p", CryptoKey: key, ResourceTypes: []string{"("}},
		{Scope: "folders/1", CryptoKey: key},
		{Scope: "projects/p", CryptoKey: "keys/k"},
	} {
		if _, err := server().SearchProtectedResources(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v; want INVALID_ARGUMENT", bad, err)
		}
	}
}

func TestListCryptoKeysListsTheProject(t *testing.T) {
	r, err := server().ListCryptoKeys(context.Background(), &inventorypb.ListCryptoKeysRequest{Parent: "projects/p"})
	if err != nil || len(r.GetCryptoKeys()) != 1 {
		t.Errorf("ListCryptoKeys = %v, %v", r, err)
	}
	if _, err := server().ListCryptoKeys(context.Background(), &inventorypb.ListCryptoKeysRequest{Parent: "p"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad parent: %v", err)
	}
}
