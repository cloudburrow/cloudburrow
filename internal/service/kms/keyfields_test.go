package kms

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// destroy_scheduled_duration accepts whole seconds from 24h to 120d (#399).
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: destroy_scheduled_duration out of range, negative or fractional
func TestDestroyScheduledDurationRange(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "ds"})
	cases := map[string]struct {
		d    *durationpb.Duration
		want codes.Code
	}{
		"23h59m59s": {durationpb.New(24*time.Hour - time.Second), codes.InvalidArgument},
		"120d+1s":   {durationpb.New(120*24*time.Hour + time.Second), codes.InvalidArgument},
		"-1s":       {durationpb.New(-time.Second), codes.InvalidArgument},
		"0.5s":      {&durationpb.Duration{Nanos: 500000000}, codes.InvalidArgument},
		"24h":       {durationpb.New(24 * time.Hour), codes.OK},
		"120d":      {durationpb.New(120 * 24 * time.Hour), codes.OK},
	}
	i := 0
	for name, c2 := range cases {
		i++
		k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k" + string(rune('a'+i)),
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: c2.d}})
		if status.Code(err) != c2.want {
			t.Errorf("%s: %v, want %s", name, err, c2.want)
		}
		if c2.want == codes.OK && k.GetDestroyScheduledDuration().AsDuration() != c2.d.AsDuration() {
			t.Errorf("%s: echoed %v", name, k.GetDestroyScheduledDuration())
		}
	}
}
