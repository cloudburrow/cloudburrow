//go:build compat

package compat

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// advanceSecretsClock moves Secret Manager's clock forward (#1278) and
// returns the time after it, so the documented minimums (a rotation five
// minutes ahead, a one-hour period, a one-day version_destroy_ttl) pass in a
// moment. It is a CloudBurrow method on the Secret Manager port, not part of
// the Secret Manager API; zero reads the clock.
func advanceSecretsClock(t *testing.T, h *Harness, d time.Duration) time.Time {
	t.Helper()
	conn, err := grpc.NewClient(h.Endpoint(EnvSecrets), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var now timestamppb.Timestamp
	if err := conn.Invoke(h.Context(), secrets.ClockMethod, durationpb.New(d), &now); err != nil {
		t.Fatalf("advance the Secret Manager clock by %s: %v", d, err)
	}
	return now.AsTime()
}

// TestSecretManagerRotationAndDelayedDestroy (#1278): through the official
// client, Secret.rotation and version_destroy_ttl are validated as Google
// documents, stored and returned; at next_rotation_time SECRET_ROTATE is
// published to the secret's topic and next_rotation_time advances by
// rotation_period; DestroySecretVersion on a secret with a
// version_destroy_ttl disables the version with a scheduled_destroy_time
// and publishes SECRET_VERSION_DESTROY_SCHEDULED, EnableSecretVersion
// cancels it, and at that time the version is destroyed.
//
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/DestroySecretVersion, google.cloud.secretmanager.v1.SecretManagerService/EnableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion
func TestSecretManagerRotationAndDelayedDestroy(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sm := secretsClient(t, h)
	ps := pubsubClient(t, h)
	topicName := topic(t, h, ps, "secret-schedule-"+h.Project())
	sub := subscription(t, h, ps, "secret-schedule-sub-"+h.Project(), topicName)
	topics := []*secretmanagerpb.Topic{{Name: topicName}}
	now := advanceSecretsClock(t, h, 0)

	for name, sec := range map[string]*secretmanagerpb.Secret{
		"rotation within five minutes": {Topics: topics, Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Minute))}},
		"period under an hour": {Topics: topics, Rotation: &secretmanagerpb.Rotation{
			NextRotationTime: timestamppb.New(now.Add(time.Hour)), RotationPeriod: durationpb.New(time.Minute)}},
		"rotation without topics":   {Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Hour))}},
		"period without next":       {Topics: topics, Rotation: &secretmanagerpb.Rotation{RotationPeriod: durationpb.New(time.Hour)}},
		"destroy ttl under one day": {VersionDestroyTtl: durationpb.New(time.Hour)},
	} {
		sec.Replication = autoReplication()
		_, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "sched-refused", Secret: sec})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: CreateSecret = %v; want INVALID_ARGUMENT", name, err)
		}
	}

	id := "sched-rotate"
	name := secretsParent(h) + "/secrets/" + id
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	first := now.Add(10 * time.Minute).Truncate(time.Second)
	created, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Topics: topics,
			Rotation:          &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(first), RotationPeriod: durationpb.New(time.Hour)},
			VersionDestroyTtl: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatalf("CreateSecret with rotation and version_destroy_ttl: %v", err)
	}
	if !created.GetRotation().GetNextRotationTime().AsTime().Equal(first) || created.GetRotation().GetRotationPeriod().AsDuration() != time.Hour ||
		created.GetVersionDestroyTtl().AsDuration() != 24*time.Hour {
		t.Errorf("created rotation %v, version_destroy_ttl %v", created.GetRotation(), created.GetVersionDestroyTtl())
	}

	advanceSecretsClock(t, h, 11*time.Minute)
	got, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if want := first.Add(time.Hour); !got.GetRotation().GetNextRotationTime().AsTime().Equal(want) {
		t.Errorf("next_rotation_time after the rotation = %v; want %v", got.GetRotation().GetNextRotationTime().AsTime(), want)
	}

	// The period alone changes by mask; next_rotation_time stays.
	upd, err := sm.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Rotation: &secretmanagerpb.Rotation{RotationPeriod: durationpb.New(2 * time.Hour)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation.rotation_period"}}})
	if err != nil {
		t.Fatal(err)
	}
	if upd.GetRotation().GetRotationPeriod().AsDuration() != 2*time.Hour || !upd.GetRotation().GetNextRotationTime().AsTime().Equal(first.Add(time.Hour)) {
		t.Errorf("updated rotation = %v", upd.GetRotation())
	}

	v1, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("one")}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("two")}})
	if err != nil {
		t.Fatal(err)
	}
	before := advanceSecretsClock(t, h, 0)
	d1, err := sm.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v1.Name})
	if err != nil {
		t.Fatal(err)
	}
	at := d1.GetScheduledDestroyTime().AsTime()
	if d1.GetState() != secretmanagerpb.SecretVersion_DISABLED || at.Before(before.Add(24*time.Hour)) || at.After(before.Add(24*time.Hour+time.Minute)) {
		t.Errorf("DestroySecretVersion with a ttl = %v %v; want DISABLED, scheduled a day on", d1.GetState(), at)
	}
	if _, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v1.Name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("access to a version scheduled for destruction = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := sm.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v2.Name}); err != nil {
		t.Fatal(err)
	}
	e2, err := sm.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v2.Name})
	if err != nil {
		t.Fatal(err)
	}
	if e2.GetState() != secretmanagerpb.SecretVersion_ENABLED || e2.GetScheduledDestroyTime() != nil {
		t.Errorf("EnableSecretVersion on a scheduled version = %v; want ENABLED and the destruction cancelled", e2)
	}

	advanceSecretsClock(t, h, 25*time.Hour)
	g1, err := sm.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v1.Name})
	if err != nil || g1.GetState() != secretmanagerpb.SecretVersion_DESTROYED || g1.GetDestroyTime() == nil {
		t.Errorf("v1 after its scheduled_destroy_time = %v, %v; want DESTROYED", g1, err)
	}
	if acc, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v2.Name}); err != nil || string(acc.GetPayload().GetData()) != "two" {
		t.Errorf("the cancelled v2 = %v, %v; want its payload", acc, err)
	}

	// TOPIC_CONFIGURED, SECRET_CREATE, SECRET_ROTATE (the first rotation;
	// the 25 hours passed in one advance send one more), SECRET_UPDATE,
	// two adds, two DESTROY_SCHEDULED, ENABLE, DESTROY.
	msgs := receiveN(t, ps, sub, 11, 60*time.Second)
	var types []string
	for _, m := range msgs {
		types = append(types, m.Attributes["eventType"])
		if m.Attributes["eventType"] == "SECRET_VERSION_DESTROY_SCHEDULED" && !strings.HasPrefix(m.Attributes["versionId"], name+"/versions/") {
			t.Errorf("DESTROY_SCHEDULED versionId = %q", m.Attributes["versionId"])
		}
		if m.Attributes["eventType"] == "SECRET_ROTATE" && m.Attributes["secretId"] != name {
			t.Errorf("SECRET_ROTATE secretId = %q; want %s", m.Attributes["secretId"], name)
		}
	}
	sort.Strings(types)
	want := []string{"SECRET_CREATE", "SECRET_ROTATE", "SECRET_ROTATE", "SECRET_UPDATE", "SECRET_VERSION_ADD", "SECRET_VERSION_ADD",
		"SECRET_VERSION_DESTROY", "SECRET_VERSION_DESTROY_SCHEDULED", "SECRET_VERSION_DESTROY_SCHEDULED", "SECRET_VERSION_ENABLE", "TOPIC_CONFIGURED"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v; want %v", types, want)
	}
}
