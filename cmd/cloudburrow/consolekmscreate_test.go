package main

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Create key on a ring's page sets the destroy scheduled duration, which no
// later call can change, and the automatic rotation schedule, and the official
// client's GetCryptoKey reads back what the form was given (#852). A value
// CreateCryptoKey refuses is refused with its own message and creates
// nothing; a key created with the form's defaults keeps the 30-day default.
func TestKMSCreateKeyWithDestroyDurationAndRotation(t *testing.T) {
	ctx := context.Background()
	p, c := kmsEditFixture(t)
	const project = "create-opts"
	ring, err := p.Create(ctx, project, map[string]string{"keyRingId": "r", "location": "global"})
	if err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if _, err := p.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{
		"cryptoKeyId": "k", "labels": `{"env":"dev"}`, "destroyScheduledDuration": "2d",
		"rotationPeriod": "90d", "nextRotationTime": next.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/k"})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.GetDestroyScheduledDuration().AsDuration(); got != 48*time.Hour {
		t.Errorf("destroy scheduled duration = %v; want 48h", got)
	}
	if got := k.GetRotationPeriod().AsDuration(); got != 90*24*time.Hour {
		t.Errorf("rotation period = %v; want 90 days", got)
	}
	if got := k.GetNextRotationTime().AsTime(); !got.Equal(next) {
		t.Errorf("next rotation time = %v; want %v", got, next)
	}
	if k.GetLabels()["env"] != "dev" || k.GetPrimary().GetName() == "" {
		t.Errorf("labels %v, primary %q; want env=dev and version 1 primary", k.GetLabels(), k.GetPrimary().GetName())
	}

	// The form's own defaults: 30 days, no rotation.
	if _, err := p.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{
		"cryptoKeyId": "plain", "destroyScheduledDuration": "30d"}); err != nil {
		t.Fatalf("create key with the defaults: %v", err)
	}
	plain, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.GetDestroyScheduledDuration().AsDuration() != 30*24*time.Hour || plain.GetRotationPeriod() != nil ||
		plain.GetNextRotationTime() != nil {
		t.Errorf("a key created with the defaults reads %v, %v, %v; want 30 days and no rotation",
			plain.GetDestroyScheduledDuration(), plain.GetRotationPeriod(), plain.GetNextRotationTime())
	}

	// Out of range: CreateCryptoKey's own refusal, and no key.
	_, sdkErr := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "sdk-short",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(12 * time.Hour)}})
	want, _ := status.FromError(sdkErr)
	if want.Code() != codes.InvalidArgument {
		t.Fatalf("the official client's CreateCryptoKey with 12h = %v; want INVALID_ARGUMENT", sdkErr)
	}
	_, err = p.ActAtResult(ctx, project, []string{ring}, "createkey", map[string]string{
		"cryptoKeyId": "short", "destroyScheduledDuration": "12h"})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want.Message() {
		t.Errorf("console create with 12h = %v; want the official client's own %q", err, want.Message())
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/short"}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused create left a key behind: %v", err)
	}
	for _, bad := range []map[string]string{
		{"cryptoKeyId": "b1", "destroyScheduledDuration": "a week"},
		{"cryptoKeyId": "b2", "rotationPeriod": "monthly"},
		{"cryptoKeyId": "b3", "rotationPeriod": "30d", "nextRotationTime": "tomorrow"},
	} {
		if _, err := p.ActAtResult(ctx, project, []string{ring}, "createkey", bad); err == nil {
			t.Errorf("createkey %v was accepted", bad)
		}
	}
}
