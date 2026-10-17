package kms

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKAJRefusal (#1506): no policy, or one allowing
// CUSTOMER_INITIATED_ACCESS, allows a call; one without it, or with no
// reasons, refuses it.
func TestKAJRefusal(t *testing.T) {
	for _, c := range []struct {
		p    *kajPolicy
		want codes.Code
	}{
		{nil, codes.OK},
		{&kajPolicy{Reasons: []string{"CUSTOMER_INITIATED_ACCESS"}}, codes.OK},
		{&kajPolicy{Reasons: []string{"GOOGLE_INITIATED_SERVICE"}}, codes.FailedPrecondition},
		{&kajPolicy{}, codes.FailedPrecondition},
	} {
		if got := status.Code(kajRefusal(cryptoKey{Name: "k", KAJPolicy: c.p})); got != c.want {
			t.Errorf("policy %+v: %v; want %v", c.p, got, c.want)
		}
	}
}
