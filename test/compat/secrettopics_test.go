//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSecretManagerTopicNotifications (#1259): through the official
// clients, a secret's `topics` are stored and returned, and each change to
// the secret or its versions is published to the local Pub/Sub with
// Google's documented attributes (docs.cloud.google.com/secret-manager/
// docs/event-notifications): TOPIC_CONFIGURED first, then SECRET_CREATE,
// SECRET_VERSION_ADD, _DISABLE, _ENABLE, _DESTROY, SECRET_UPDATE and
// SECRET_DELETE, each with eventType, dataFormat JSON_API_V1, secretId and
// timestamp, versionId on version events, deleteType on the delete, and
// the resource's JSON as data. A topic that does not exist fails the
// create, and nothing is stored.
//
// unverified: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret NOT_FOUND: a topic in Secret.topics that does not exist (Pub/Sub's code for the failed TOPIC_CONFIGURED publish)
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/secret-manager/docs/event-notifications
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/EnableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DisableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DestroySecretVersion
func TestSecretManagerTopicNotifications(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sm := secretsClient(t, h)
	ps := pubsubClient(t, h)
	topicName := topic(t, h, ps, "secret-events-"+h.Project())
	sub := subscription(t, h, ps, "secret-events-sub-"+h.Project(), topicName)

	missing := "projects/" + h.Project() + "/topics/no-such-topic-1259"
	_, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "topics-missing",
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Topics: []*secretmanagerpb.Topic{{Name: missing}}}})
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), missing) {
		t.Errorf("CreateSecret with a missing topic = %v; want NOT_FOUND naming the topic", err)
	}
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secretsParent(h) + "/secrets/topics-missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("the refused secret was stored: %v", err)
	}

	id := "topics-events"
	name := secretsParent(h) + "/secrets/" + id
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	created, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Topics: []*secretmanagerpb.Topic{{Name: topicName}}}})
	if err != nil {
		t.Fatalf("CreateSecret with topics: %v", err)
	}
	if len(created.GetTopics()) != 1 || created.GetTopics()[0].GetName() != topicName {
		t.Errorf("created topics = %v", created.GetTopics())
	}
	got, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil || len(got.GetTopics()) != 1 || got.GetTopics()[0].GetName() != topicName {
		t.Errorf("GetSecret topics = %v, %v", got.GetTopics(), err)
	}
	v, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("s3cret")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Labels: map[string]string{"env": "dev"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Fatal(err)
	}
	if err := sm.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name}); err != nil {
		t.Fatal(err)
	}

	msgs := receiveN(t, ps, sub, 8, 60*time.Second)
	byType := map[string]map[string]string{}
	data := map[string][]byte{}
	var types []string
	for _, m := range msgs {
		e := m.Attributes["eventType"]
		types = append(types, e)
		byType[e], data[e] = m.Attributes, m.Data
	}
	sort.Strings(types)
	want := []string{"SECRET_CREATE", "SECRET_DELETE", "SECRET_UPDATE", "SECRET_VERSION_ADD", "SECRET_VERSION_DESTROY",
		"SECRET_VERSION_DISABLE", "SECRET_VERSION_ENABLE", "TOPIC_CONFIGURED"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v; want %v", types, want)
	}
	if a := byType["TOPIC_CONFIGURED"]; len(a) != 1 || len(data["TOPIC_CONFIGURED"]) != 0 {
		t.Errorf("TOPIC_CONFIGURED = %v %q; want eventType alone, no data", a, data["TOPIC_CONFIGURED"])
	}
	for _, e := range want[:7] {
		a := byType[e]
		if a["dataFormat"] != "JSON_API_V1" || a["secretId"] != name {
			t.Errorf("%s attributes = %v", e, a)
		}
		if _, err := time.Parse(time.RFC3339Nano, a["timestamp"]); err != nil {
			t.Errorf("%s timestamp %q: %v", e, a["timestamp"], err)
		}
		var body map[string]any
		if err := json.Unmarshal(data[e], &body); err != nil {
			t.Errorf("%s data %q: %v", e, data[e], err)
			continue
		}
		wantName, isVersion := name, strings.HasPrefix(e, "SECRET_VERSION_")
		if isVersion {
			wantName = v.Name
			if a["versionId"] != v.Name {
				t.Errorf("%s versionId = %q; want %s", e, a["versionId"], v.Name)
			}
		} else if a["versionId"] != "" {
			t.Errorf("%s carries versionId %q", e, a["versionId"])
		}
		if body["name"] != wantName {
			t.Errorf("%s data names %v; want %s", e, body["name"], wantName)
		}
	}
	if byType["SECRET_DELETE"]["deleteType"] != "REQUESTED" {
		t.Errorf("SECRET_DELETE deleteType = %q; want REQUESTED", byType["SECRET_DELETE"]["deleteType"])
	}
	var state struct{ State string }
	_ = json.Unmarshal(data["SECRET_VERSION_DISABLE"], &state)
	if state.State != "DISABLED" {
		t.Errorf("SECRET_VERSION_DISABLE data state = %q; want the version after the change", state.State)
	}
}
