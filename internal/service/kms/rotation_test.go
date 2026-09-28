package kms

import (
	"context"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

var rotationStart = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func periodOf(d time.Duration) *kmspb.CryptoKey_RotationPeriod {
	return &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(d)}
}

func rotationFixture(t *testing.T, ringID string) (*Server, *sched.FakeClock, *kms.KeyManagementClient, string) {
	t.Helper()
	clock := sched.NewFakeClock(rotationStart)
	srv := NewServerWithClock(store.NewMemory(), clock)
	c := clientOf(t, srv)
	ring, err := c.CreateKeyRing(context.Background(), &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: ringID})
	if err != nil {
		t.Fatal(err)
	}
	return srv, clock, c, ring.GetName()
}

// CreateCryptoKey and UpdateCryptoKey take rotation_period and
// next_rotation_time under resources.proto's rules, return them from
// GetCryptoKey and ListCryptoKeys, and refuse a period outside 24h to
// 876,000h, and a period without a next_rotation_time, with INVALID_ARGUMENT
// naming the rule (#816).
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: a rotation_period below 24h or above 876,000h, or without next_rotation_time (message wording)
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey INVALID_ARGUMENT: a rotation_period below 24h, or a schedule left with a period and no next_rotation_time (message wording)
func TestKMSRotationScheduleValidation(t *testing.T) {
	ctx := context.Background()
	_, _, c, ring := rotationFixture(t, "rv")
	sym := kmspb.CryptoKey_ENCRYPT_DECRYPT
	nrt := timestamppb.New(rotationStart.Add(time.Hour))

	const (
		belowMin = "crypto_key.rotation_period 23h59m59s is out of range: it must be at least 24 hours and at most 876,000 hours"
		needNext = "crypto_key.next_rotation_time must be set when crypto_key.rotation_period is set"
	)
	for id, c2 := range map[string]struct {
		key  *kmspb.CryptoKey
		want string
	}{
		"below-min":  {&kmspb.CryptoKey{Purpose: sym, RotationSchedule: periodOf(24*time.Hour - time.Second), NextRotationTime: nrt}, belowMin},
		"above-max":  {&kmspb.CryptoKey{Purpose: sym, RotationSchedule: periodOf(876001 * time.Hour), NextRotationTime: nrt}, "crypto_key.rotation_period 876001h0m0s is out of range: it must be at least 24 hours and at most 876,000 hours"},
		"zero":       {&kmspb.CryptoKey{Purpose: sym, RotationSchedule: periodOf(0), NextRotationTime: nrt}, "crypto_key.rotation_period 0s is out of range: it must be at least 24 hours and at most 876,000 hours"},
		"no-next":    {&kmspb.CryptoKey{Purpose: sym, RotationSchedule: periodOf(30 * 24 * time.Hour)}, needNext},
		"bad-period": {&kmspb.CryptoKey{Purpose: sym, RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: &durationpb.Duration{Seconds: 86400, Nanos: -1}}, NextRotationTime: nrt}, ""},
	} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id, CryptoKey: c2.key})
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument || (c2.want != "" && st.Message() != c2.want) {
			t.Errorf("CreateCryptoKey %s = %v; want INVALID_ARGUMENT %q", id, err, c2.want)
		}
	}

	// The minimum and maximum are allowed; the schedule reads back.
	for id, d := range map[string]time.Duration{"min": 24 * time.Hour, "max": 876000 * time.Hour} {
		if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: sym, RotationSchedule: periodOf(d), NextRotationTime: nrt}}); err != nil {
			t.Errorf("CreateCryptoKey with rotation_period %s: %v", d, err)
		}
	}
	got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/min"})
	if err != nil || got.GetRotationPeriod().AsDuration() != 24*time.Hour || !got.GetNextRotationTime().AsTime().Equal(nrt.AsTime()) {
		t.Errorf("GetCryptoKey reads rotation_period %v, next_rotation_time %v (%v)", got.GetRotationPeriod(), got.GetNextRotationTime(), err)
	}
	it := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring})
	for k, err := it.Next(); err == nil; k, err = it.Next() {
		if k.GetRotationPeriod() == nil || k.GetNextRotationTime() == nil {
			t.Errorf("ListCryptoKeys returns %s without its schedule", k.GetName())
		}
	}

	// next_rotation_time alone is allowed (the proto requires it only with a
	// period).
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "once",
		CryptoKey: &kmspb.CryptoKey{Purpose: sym, NextRotationTime: nrt}}); err != nil {
		t.Errorf("CreateCryptoKey with next_rotation_time and no period: %v", err)
	}

	// UpdateCryptoKey.
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "upd", CryptoKey: &kmspb.CryptoKey{Purpose: sym}})
	if err != nil {
		t.Fatal(err)
	}
	upd := func(k *kmspb.CryptoKey, paths ...string) (*kmspb.CryptoKey, error) {
		k.Name = key.GetName()
		return c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: k, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	check := func(what string, err error, want string) {
		t.Helper()
		if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want {
			t.Errorf("%s = %v; want INVALID_ARGUMENT %q", what, err, want)
		}
	}
	_, err = upd(&kmspb.CryptoKey{RotationSchedule: periodOf(30 * 24 * time.Hour)}, "rotation_period")
	check("a period on a key with no next_rotation_time", err, needNext)
	_, err = upd(&kmspb.CryptoKey{RotationSchedule: periodOf(24*time.Hour - time.Second), NextRotationTime: nrt}, "rotation_period", "next_rotation_time")
	check("a period below the minimum", err, belowMin)
	if g, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); g.GetRotationPeriod() != nil || g.GetNextRotationTime() != nil {
		t.Errorf("a refused update left the schedule %v %v", g.GetRotationPeriod(), g.GetNextRotationTime())
	}
	u, err := upd(&kmspb.CryptoKey{RotationSchedule: periodOf(30 * 24 * time.Hour), NextRotationTime: nrt}, "rotation_period", "next_rotation_time")
	if err != nil || u.GetRotationPeriod().AsDuration() != 30*24*time.Hour || !u.GetNextRotationTime().AsTime().Equal(nrt.AsTime()) {
		t.Fatalf("setting the schedule = %v, %v", u, err)
	}
	// A period alone, now that next_rotation_time is set.
	if u, err := upd(&kmspb.CryptoKey{RotationSchedule: periodOf(7 * 24 * time.Hour)}, "rotation_period"); err != nil ||
		u.GetRotationPeriod().AsDuration() != 7*24*time.Hour || !u.GetNextRotationTime().AsTime().Equal(nrt.AsTime()) {
		t.Errorf("changing the period alone = %v, %v", u, err)
	}
	_, err = upd(&kmspb.CryptoKey{}, "next_rotation_time")
	check("clearing next_rotation_time under a period", err, needNext)
	if u, err := upd(&kmspb.CryptoKey{}, "rotation_period", "next_rotation_time"); err != nil || u.GetRotationPeriod() != nil || u.GetNextRotationTime() != nil {
		t.Errorf("clearing the schedule = %v, %v", u, err)
	}
}

// A key rotates at next_rotation_time on the Server's clock: a new ENABLED
// version becomes primary, and next_rotation_time advances by the period.
// Nothing happens a second early. A key that was due several periods ago
// rotates once and moves to the next time on its schedule. A rotation by hand
// leaves next_rotation_time alone; next_rotation_time without a period
// rotates once and is cleared (#816).
func TestKMSKeyRotatesOnSchedule(t *testing.T) {
	ctx := context.Background()
	srv, clock, c, ring := rotationFixture(t, "rs")
	const week = 7 * 24 * time.Hour
	first := rotationStart.Add(time.Hour)
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, RotationSchedule: periodOf(week), NextRotationTime: timestamppb.New(first)}})
	if err != nil {
		t.Fatal(err)
	}
	state := func() (string, time.Time) {
		t.Helper()
		k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		return k.GetPrimary().GetName(), k.GetNextRotationTime().AsTime()
	}
	sweep := func() time.Time {
		t.Helper()
		next, err := srv.Sweep()
		if err != nil {
			t.Fatal(err)
		}
		return next
	}

	if next := sweep(); !next.Equal(first) {
		t.Errorf("Sweep reports the next due time %v; want next_rotation_time %v", next, first)
	}
	clock.Advance(time.Hour - time.Second)
	sweep()
	if p, _ := state(); p != versionName(key.GetName(), 1) {
		t.Fatalf("a second before next_rotation_time the primary is %s", p)
	}

	clock.Advance(time.Second)
	if next := sweep(); !next.Equal(first.Add(week)) {
		t.Errorf("after the rotation Sweep reports %v; want %v", next, first.Add(week))
	}
	p, nrt := state()
	if p != versionName(key.GetName(), 2) || !nrt.Equal(first.Add(week)) {
		t.Fatalf("at next_rotation_time: primary %s, next_rotation_time %v; want version 2 and %v", p, nrt, first.Add(week))
	}
	v2, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: p})
	if err != nil || v2.GetState() != kmspb.CryptoKeyVersion_ENABLED || !v2.GetCreateTime().AsTime().Equal(first) ||
		v2.GetAlgorithm() != kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
		t.Errorf("the rotated version = %v, %v", v2, err)
	}
	if enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); err != nil || enc.GetName() != p {
		t.Errorf("Encrypt after rotation used %s (%v); want %s", enc.GetName(), err, p)
	}

	// By hand: a new version made primary does not move next_rotation_time.
	v3, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "3"}); err != nil {
		t.Fatal(err)
	}
	if p, nrt := state(); p != v3.GetName() || !nrt.Equal(first.Add(week)) {
		t.Errorf("after a rotation by hand: primary %s, next_rotation_time %v; want %s and %v", p, nrt, v3.GetName(), first.Add(week))
	}

	// Due three periods ago: one rotation, and the next time after now.
	clock.Advance(3*week + time.Minute)
	sweep()
	if p, nrt := state(); p != versionName(key.GetName(), 4) || !nrt.Equal(first.Add(4*week)) {
		t.Errorf("after missing three rotations: primary %s, next_rotation_time %v; want version 4 and %v", p, nrt, first.Add(4*week))
	}
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()})
	n := 0
	for _, err := it.Next(); err == nil; _, err = it.Next() {
		n++
	}
	if n != 4 {
		t.Errorf("the key has %d versions; want 4 (1, rotated 2, by hand 3, rotated 4)", n)
	}

	// next_rotation_time and no period: once, then no schedule.
	once, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "once",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, NextRotationTime: timestamppb.New(clock.Now().Add(time.Hour))}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	sweep()
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: once.GetName()}); err != nil ||
		k.GetPrimary().GetName() != versionName(once.GetName(), 2) || k.GetNextRotationTime() != nil {
		t.Errorf("a one-off next_rotation_time: primary %s, next_rotation_time %v (%v)", k.GetPrimary().GetName(), k.GetNextRotationTime(), err)
	}
}

// Run arms its timer for next_rotation_time and rotates when the clock gets
// there, without a call to Sweep; a schedule set by UpdateCryptoKey re-arms
// it (#816).
func TestKMSRunRotatesAtNextRotationTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv, clock, c, ring := rotationFixture(t, "run")
	done := make(chan struct{})
	go func() { defer close(done); srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	due := rotationStart.Add(2 * time.Hour)
	if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
		CryptoKey:  &kmspb.CryptoKey{Name: key.GetName(), RotationSchedule: periodOf(24 * time.Hour), NextRotationTime: timestamppb.New(due)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period", "next_rotation_time"}}}); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("Run to arm a timer for next_rotation_time", func() bool { return clock.HasWaiterAt(due) })
	clock.Advance(2 * time.Hour)
	waitFor("the rotation", func() bool {
		k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
		return err == nil && k.GetPrimary().GetName() == versionName(key.GetName(), 2) &&
			k.GetNextRotationTime().AsTime().Equal(due.Add(24*time.Hour))
	})
}
