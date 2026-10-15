package secrets

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const topicA = "projects/demo/topics/a"

func scheduleServer(t *testing.T) (*GRPCServer, *Store, *fakePubSub) {
	t.Helper()
	s, _ := newClockStore(t)
	ps := &fakePubSub{topics: map[string]bool{topicA: true}}
	s.SetPublisher(ps.publish)
	return NewGRPCServer(s), s, ps
}

func countEvent(ps *fakePubSub, event string) int {
	n := 0
	for _, e := range ps.events() {
		if e == topicA+" "+event {
			n++
		}
	}
	return n
}

// Rotation (#1278): validated as documented, SECRET_ROTATE at
// next_rotation_time, which then advances by rotation_period.
func TestRotationNotifiesAndAdvances(t *testing.T) {
	t.Parallel()
	g, s, ps := scheduleServer(t)
	ctx := context.Background()
	now := s.Now()
	topics := []*secretmanagerpb.Topic{{Name: topicA}}
	create := func(id string, sec *secretmanagerpb.Secret) error {
		sec.Replication = automatic()
		_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: id, Secret: sec})
		return err
	}
	for name, sec := range map[string]*secretmanagerpb.Secret{
		"too-soon":    {Topics: topics, Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Minute))}},
		"short":       {Topics: topics, Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Hour)), RotationPeriod: durationpb.New(time.Minute)}},
		"no-topics":   {Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Hour))}},
		"period-only": {Topics: topics, Rotation: &secretmanagerpb.Rotation{RotationPeriod: durationpb.New(time.Hour)}},
		"ttl-short":   {VersionDestroyTtl: durationpb.New(time.Hour)},
	} {
		wantGRPCCode(t, create(name, sec), codes.InvalidArgument)
	}
	first := now.Add(10 * time.Minute)
	if err := create("rot", &secretmanagerpb.Secret{Topics: topics, Rotation: &secretmanagerpb.Rotation{
		NextRotationTime: timestamppb.New(first), RotationPeriod: durationpb.New(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	got, _ := g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets/rot"})
	if !got.GetRotation().GetNextRotationTime().AsTime().Equal(first) || got.GetRotation().GetRotationPeriod().AsDuration() != time.Hour {
		t.Fatalf("rotation = %v", got.GetRotation())
	}
	if _, err := s.Advance(9 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(ps, EventSecretRotate); n != 0 {
		t.Fatalf("rotated %d times before next_rotation_time", n)
	}
	if _, err := s.Advance(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(ps, EventSecretRotate); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	got, _ = g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets/rot"})
	if want := first.Add(time.Hour); !got.GetRotation().GetNextRotationTime().AsTime().Equal(want) {
		t.Errorf("next_rotation_time = %v, want %v", got.GetRotation().GetNextRotationTime().AsTime(), want)
	}
	// Clearing rotation by mask stops it.
	if _, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: got.Name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(2 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(ps, EventSecretRotate); n != 1 {
		t.Errorf("rotations after clearing = %d, want 1", n)
	}
	// Removing the topics of a rotating secret is refused.
	if _, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: got.Name,
		Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(s.Now().Add(time.Hour))}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation.next_rotation_time", "topics"}}}); err == nil {
		t.Error("rotation without topics was accepted")
	}
}

// version_destroy_ttl (#1278): destroy disables and schedules, enable
// cancels, and the version is destroyed at scheduled_destroy_time.
func TestDelayedVersionDestroy(t *testing.T) {
	t.Parallel()
	g, s, ps := scheduleServer(t)
	ctx := context.Background()
	sec, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "d",
		Secret: &secretmanagerpb.Secret{Replication: automatic(), Topics: []*secretmanagerpb.Topic{{Name: topicA}},
			VersionDestroyTtl: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.GetVersionDestroyTtl().AsDuration() != 24*time.Hour {
		t.Fatalf("version_destroy_ttl = %v", sec.GetVersionDestroyTtl())
	}
	for filter, want := range map[string]int{"version_destroy_ttl>3600s": 1, "version_destroy_ttl>86400s": 0, "rotation:*": 0} {
		l, err := g.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/demo", Filter: filter})
		if err != nil || len(l.GetSecrets()) != want {
			t.Errorf("filter %s = %d secrets, %v; want %d", filter, len(l.GetSecrets()), err, want)
		}
	}
	add := func() *secretmanagerpb.SecretVersion {
		v, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.Name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v1, v2 := add(), add()
	start := s.Now()
	d1, err := g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v1.Name})
	if err != nil {
		t.Fatal(err)
	}
	if d1.GetState() != secretmanagerpb.SecretVersion_DISABLED || !d1.GetScheduledDestroyTime().AsTime().Equal(start.Add(24*time.Hour)) {
		t.Fatalf("scheduled destroy = %v", d1)
	}
	if n := countEvent(ps, EventSecretVersionDestroyScheduled); n != 1 {
		t.Fatalf("DESTROY_SCHEDULED sent %d times", n)
	}
	if _, err := g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v2.Name}); err != nil {
		t.Fatal(err)
	}
	e2, err := g.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v2.Name})
	if err != nil {
		t.Fatal(err)
	}
	if e2.GetScheduledDestroyTime() != nil || e2.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		t.Fatalf("enable did not cancel: %v", e2)
	}
	if _, err := s.Advance(25 * time.Hour); err != nil {
		t.Fatal(err)
	}
	got1, _ := g.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v1.Name})
	if got1.GetState() != secretmanagerpb.SecretVersion_DESTROYED || got1.GetScheduledDestroyTime() != nil {
		t.Errorf("v1 after its scheduled time = %v", got1)
	}
	got2, _ := g.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v2.Name})
	if got2.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		t.Errorf("cancelled v2 = %v", got2)
	}
	if n := countEvent(ps, EventSecretVersionDestroy); n != 1 {
		t.Errorf("SECRET_VERSION_DESTROY sent %d times, want 1", n)
	}
	// The schedule bookkeeping is gone with it.
	stored, _ := s.GetSecret("demo", "d")
	if len(stored.ScheduledDestroys) != 0 {
		raw, _ := json.Marshal(stored)
		t.Errorf("scheduled destroys left: %s", raw)
	}
}
