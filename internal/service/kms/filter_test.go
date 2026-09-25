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

// The version state filter and the grammar around it (#408).
//
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions INVALID_ARGUMENT: a filter that does not parse
func TestVersionStateFilter(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "f"}); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: loc + "/keyRings/f", CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName(),
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{State: kmspb.CryptoKeyVersion_DISABLED}}); err != nil {
		t.Fatal(err)
	}
	v3, _ := s.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if _, err := s.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: v3.GetName()}); err != nil {
		t.Fatal(err)
	}
	list := func(filter string, size int32, token string) (*kmspb.ListCryptoKeyVersionsResponse, error) {
		return s.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName(), Filter: filter, PageSize: size, PageToken: token})
	}
	numbers := func(r *kmspb.ListCryptoKeyVersionsResponse) string {
		out := ""
		for _, v := range r.GetCryptoKeyVersions() {
			out += v.GetName()[len(v.GetName())-1:]
		}
		return out
	}
	for filter, want := range map[string]string{
		"state=ENABLED":                                     "1",
		"state!=DESTROY_SCHEDULED":                          "12",
		"NOT state=ENABLED":                                 "23",
		"-state=ENABLED":                                    "23",
		"state=ENABLED OR state=DISABLED":                   "12",
		"state!=ENABLED AND state!=DISABLED":                "3",
		"state!=ENABLED state!=DISABLED":                    "3",
		"(state=ENABLED OR state=DISABLED) state!=DISABLED": "1",
		`state="DISABLED"`:                                  "2",
	} {
		r, err := list(filter, 0, "")
		if err != nil || numbers(r) != want || r.GetTotalSize() != int32(len(want)) {
			t.Errorf("%s = %q total %d (%v); want %q", filter, numbers(r), r.GetTotalSize(), err, want)
		}
	}
	// Paging over a filtered list: one match per page, stable.
	first, err := list("state!=DESTROY_SCHEDULED", 1, "")
	if err != nil || numbers(first) != "1" || first.GetTotalSize() != 2 {
		t.Fatalf("first page = %q %d %v", numbers(first), first.GetTotalSize(), err)
	}
	second, err := list("state!=DESTROY_SCHEDULED", 1, first.GetNextPageToken())
	if err != nil || numbers(second) != "2" || second.GetNextPageToken() != "" {
		t.Errorf("second page = %q %q %v", numbers(second), second.GetNextPageToken(), err)
	}
	code := func(err error) codes.Code { return status.Code(apierror.Wrap(err)) }
	for filter, want := range map[string]codes.Code{
		"state=":           codes.InvalidArgument,
		"(state=ENABLED":   codes.InvalidArgument,
		"state ENABLED":    codes.InvalidArgument,
		`state="ENABLED`:   codes.InvalidArgument,
		"labels.env=prod":  codes.Unimplemented,
		"state:ENABLED":    codes.Unimplemented,
		"create_time>2020": codes.Unimplemented,
	} {
		if _, err := list(filter, 0, ""); code(err) != want {
			t.Errorf("%s = %v, want %s", filter, err, want)
		}
	}
	if _, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, Filter: "name:foo"}); code(err) != codes.Unimplemented {
		t.Errorf("name:foo on ListKeyRings = %v, want UNIMPLEMENTED", err)
	}
}
