package kms

import (
	"context"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// order_by accepts name and name desc; anything else is refused, filter is
// still UNIMPLEMENTED, and a page token is bound to its order (#407).
//
// unverified: google.cloud.kms.v1.KeyManagementService/ListKeyRings INVALID_ARGUMENT: an order_by other than name or name desc, or a page token from another order
func TestOrderByAndItsPageTokens(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: id}); err != nil {
			t.Fatal(err)
		}
	}
	code := func(err error) codes.Code { return status.Code(apierror.Wrap(err)) }
	if _, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, OrderBy: "create_time"}); code(err) != codes.InvalidArgument {
		t.Errorf("order_by create_time = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, Filter: "create_time>2020"}); code(err) != codes.Unimplemented {
		t.Errorf("a filter = %v, want UNIMPLEMENTED", err)
	}
	desc, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, OrderBy: "name desc"})
	if err != nil || len(desc.GetKeyRings()) != 3 || desc.GetKeyRings()[0].GetName() != loc+"/keyRings/c" {
		t.Errorf("name desc = %v, %v; want c first", desc.GetKeyRings(), err)
	}
	first, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, PageSize: 1})
	if err != nil || first.GetNextPageToken() == "" {
		t.Fatalf("first page: %v, %v", first, err)
	}
	if _, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, PageSize: 1, OrderBy: "name desc", PageToken: first.GetNextPageToken()}); code(err) != codes.InvalidArgument {
		t.Errorf("an ascending token reused with name desc = %v, want INVALID_ARGUMENT", err)
	}
}
