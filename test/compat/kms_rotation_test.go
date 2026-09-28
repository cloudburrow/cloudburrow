//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestKMSRotationSchedule (#816), over gRPC and REST through the official
// client: a rotation_period and next_rotation_time set at CreateCryptoKey,
// and changed and cleared through UpdateCryptoKey, are what GetCryptoKey and
// ListCryptoKeys read. A period below the 24-hour minimum (resources.proto),
// and a period without a next_rotation_time, are refused INVALID_ARGUMENT
// with the service's message naming the rule, and change nothing. When the
// key rotates is proven on a fake clock by the unit tests
// (TestKMSKeyRotatesOnSchedule, TestKMSRunRotatesAtNextRotationTime); the
// next_rotation_time here is a month away, so nothing rotates during the test.
// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey, google.cloud.kms.v1.KeyManagementService/GetCryptoKey
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: a rotation_period below 24h (message wording)
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey INVALID_ARGUMENT: a rotation_period below 24h, or with no next_rotation_time (message wording)
func TestKMSRotationSchedule(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	const month = 30 * 24 * time.Hour
	next := time.Now().UTC().Add(month).Truncate(time.Second)
	period := func(d time.Duration) *kmspb.CryptoKey_RotationPeriod {
		return &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(d)}
	}
	for variant, c := range kmsClients(t, h) {
		t.Run(variant, func(t *testing.T) {
			// message is the refusal's message over gRPC, where the client
			// returns the status; the REST client's error carries it in text.
			refused := func(what string, err error, want string) {
				t.Helper()
				if kmsCode(variant, err) != codes.InvalidArgument || !strings.Contains(err.Error(), want) {
					t.Errorf("%s = %v; want INVALID_ARGUMENT %q", what, err, want)
				}
				if st, ok := status.FromError(err); variant == "grpc" && (!ok || st.Message() != want) {
					t.Errorf("%s: message %q, want %q", what, st.Message(), want)
				}
			}
			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "rotation-" + variant})
			if err != nil {
				t.Fatalf("CreateKeyRing: %v", err)
			}
			_, err = c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "short",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, RotationSchedule: period(12 * time.Hour), NextRotationTime: timestamppb.New(next)}})
			refused("CreateCryptoKey with rotation_period 12h", err,
				"crypto_key.rotation_period 12h0m0s is out of range: it must be at least 24 hours and at most 876,000 hours")

			key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, RotationSchedule: period(month), NextRotationTime: timestamppb.New(next)}})
			if err != nil {
				t.Fatalf("CreateCryptoKey with a rotation schedule: %v", err)
			}
			if key.GetRotationPeriod().AsDuration() != month || !key.GetNextRotationTime().AsTime().Equal(next) {
				t.Errorf("CreateCryptoKey returned rotation_period %v, next_rotation_time %v", key.GetRotationPeriod(), key.GetNextRotationTime())
			}
			read := func() *kmspb.CryptoKey {
				t.Helper()
				k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
				if err != nil {
					t.Fatalf("GetCryptoKey: %v", err)
				}
				return k
			}
			if k := read(); k.GetRotationPeriod().AsDuration() != month || !k.GetNextRotationTime().AsTime().Equal(next) {
				t.Errorf("GetCryptoKey reads rotation_period %v, next_rotation_time %v; want %v and %v", k.GetRotationPeriod(), k.GetNextRotationTime(), month, next)
			}
			listed, err := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName()}).Next()
			if err != nil || listed.GetRotationPeriod().AsDuration() != month || !listed.GetNextRotationTime().AsTime().Equal(next) {
				t.Errorf("ListCryptoKeys returns %v (%v) without the schedule", listed, err)
			}

			upd := func(k *kmspb.CryptoKey, paths ...string) (*kmspb.CryptoKey, error) {
				k.Name = key.GetName()
				return c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: k, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
			}
			_, err = upd(&kmspb.CryptoKey{RotationSchedule: period(23 * time.Hour)}, "rotation_period")
			refused("UpdateCryptoKey with rotation_period 23h", err,
				"crypto_key.rotation_period 23h0m0s is out of range: it must be at least 24 hours and at most 876,000 hours")
			_, err = upd(&kmspb.CryptoKey{}, "next_rotation_time")
			refused("UpdateCryptoKey clearing next_rotation_time under a period", err,
				"crypto_key.next_rotation_time must be set when crypto_key.rotation_period is set")
			if k := read(); k.GetRotationPeriod().AsDuration() != month || k.GetNextRotationTime() == nil {
				t.Errorf("refused updates left rotation_period %v, next_rotation_time %v", k.GetRotationPeriod(), k.GetNextRotationTime())
			}

			if _, err := upd(&kmspb.CryptoKey{RotationSchedule: period(90 * 24 * time.Hour)}, "rotation_period"); err != nil {
				t.Fatalf("UpdateCryptoKey rotation_period 90d: %v", err)
			}
			if k := read(); k.GetRotationPeriod().AsDuration() != 90*24*time.Hour || !k.GetNextRotationTime().AsTime().Equal(next) {
				t.Errorf("after setting the period to 90d GetCryptoKey reads %v, %v", k.GetRotationPeriod(), k.GetNextRotationTime())
			}
			if _, err := upd(&kmspb.CryptoKey{}, "rotation_period", "next_rotation_time"); err != nil {
				t.Fatalf("UpdateCryptoKey clearing the schedule: %v", err)
			}
			if k := read(); k.GetRotationPeriod() != nil || k.GetNextRotationTime() != nil {
				t.Errorf("after clearing, GetCryptoKey reads %v, %v", k.GetRotationPeriod(), k.GetNextRotationTime())
			}
			if k := read(); k.GetPrimary().GetName() != key.GetPrimary().GetName() {
				t.Errorf("the primary moved from %s to %s with nothing due", key.GetPrimary().GetName(), k.GetPrimary().GetName())
			}
		})
	}
}
