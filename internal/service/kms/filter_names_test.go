package kms

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// name filters on rings and keys, labels filters on keys (#409).
//
// unverified: google.cloud.kms.v1.KeyManagementService/ListCryptoKeys INVALID_ARGUMENT: a filter that does not parse
func TestNameAndLabelFilters(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	for _, id := range []string{"alpha", "beta", "alphabet"} {
		if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: id}); err != nil {
			t.Fatal(err)
		}
	}
	ring := loc + "/keyRings/alpha"
	for id, labels := range map[string]map[string]string{"prod": {"env": "prod"}, "dev": {"env": "dev", "team": "alpha"}, "bare": nil} {
		if _, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Labels: labels}}); err != nil {
			t.Fatal(err)
		}
	}
	short := func(n string) string { return n[strings.LastIndex(n, "/")+1:] }
	rings := func(f string) (string, error) {
		r, err := s.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, Filter: f})
		var out []string
		for _, x := range r.GetKeyRings() {
			out = append(out, short(x.GetName()))
		}
		return strings.Join(out, ","), err
	}
	keys := func(f string) (string, error) {
		r, err := s.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring, Filter: f})
		var out []string
		for _, x := range r.GetCryptoKeys() {
			out = append(out, short(x.GetName()))
		}
		return strings.Join(out, ","), err
	}
	for f, want := range map[string]string{
		"name=" + loc + "/keyRings/alpha":  "alpha",
		"name!=" + loc + "/keyRings/alpha": "alphabet,beta",
		"name:ALPHA":                       "alpha,alphabet",
		"name:alpha OR name:beta":          "alpha,alphabet,beta",
	} {
		if got, err := rings(f); err != nil || got != want {
			t.Errorf("rings %s = %q (%v), want %q", f, got, err, want)
		}
	}
	for f, want := range map[string]string{
		"labels.env=prod":         "prod",
		"labels.env:pro":          "prod",
		"labels.team=alpha":       "dev",
		"NOT labels.env=prod":     "bare,dev",
		"name:dev":                "dev",
		"labels.env=dev name:dev": "dev",
		"labels.missing=anything": "",
	} {
		if got, err := keys(f); err != nil || got != want {
			t.Errorf("keys %s = %q (%v), want %q", f, got, err, want)
		}
	}
	code := func(err error) codes.Code { return status.Code(apierror.Wrap(err)) }
	for f, want := range map[string]codes.Code{
		"create_time>2020": codes.Unimplemented,
		"labels.env!=prod": codes.Unimplemented,
		"labels.env=":      codes.InvalidArgument,
	} {
		if _, err := keys(f); code(err) != want {
			t.Errorf("keys %s = %v, want %s", f, err, want)
		}
	}
	if _, err := rings("labels.env=prod"); code(err) != codes.Unimplemented {
		t.Errorf("a labels filter on key rings = %v, want UNIMPLEMENTED: key rings have no labels", err)
	}
}
