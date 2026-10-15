package secrets

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type sent struct {
	topic string
	data  []byte
	attrs map[string]string
}

// fakePubSub records what is published; a topic not in topics is NOT_FOUND,
// as Pub/Sub answers.
type fakePubSub struct {
	mu     sync.Mutex
	topics map[string]bool
	msgs   []sent
}

func (f *fakePubSub) publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.topics[topic] {
		return status.Errorf(codes.NotFound, "Topic not found")
	}
	f.msgs = append(f.msgs, sent{topic, data, attrs})
	return nil
}

func (f *fakePubSub) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		out = append(out, m.topic+" "+m.attrs["eventType"])
	}
	return out
}

func TestSecretTopicsPublishEvents(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ps := &fakePubSub{topics: map[string]bool{"projects/demo/topics/a": true, "projects/demo/topics/b": true}}
	s.SetPublisher(ps.publish)
	ctx := context.Background()

	sec, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "s",
		Secret: &secretmanagerpb.Secret{Replication: automatic(), Topics: []*secretmanagerpb.Topic{{Name: "projects/demo/topics/a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sec.GetTopics()) != 1 || sec.GetTopics()[0].GetName() != "projects/demo/topics/a" {
		t.Errorf("created topics = %v", sec.GetTopics())
	}
	v, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.Name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{Name: sec.Name, Topics: []*secretmanagerpb.Topic{
			{Name: "projects/demo/topics/a"}, {Name: "projects/demo/topics/b"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"topics"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"projects/demo/topics/a TOPIC_CONFIGURED",
		"projects/demo/topics/a SECRET_CREATE",
		"projects/demo/topics/a SECRET_VERSION_ADD",
		"projects/demo/topics/a SECRET_VERSION_DISABLE",
		"projects/demo/topics/a SECRET_VERSION_ENABLE",
		"projects/demo/topics/a SECRET_VERSION_DESTROY",
		"projects/demo/topics/a TOPIC_CONFIGURED",
		"projects/demo/topics/b TOPIC_CONFIGURED",
		"projects/demo/topics/a SECRET_UPDATE",
		"projects/demo/topics/b SECRET_UPDATE",
		"projects/demo/topics/a SECRET_DELETE",
		"projects/demo/topics/b SECRET_DELETE",
	}
	if got := ps.events(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// TOPIC_CONFIGURED has no data and no other attribute.
	if m := ps.msgs[0]; len(m.data) != 0 || len(m.attrs) != 1 {
		t.Errorf("TOPIC_CONFIGURED = %q %v", m.data, m.attrs)
	}
	add := ps.msgs[2]
	if add.attrs["dataFormat"] != "JSON_API_V1" || add.attrs["secretId"] != sec.Name ||
		add.attrs["versionId"] != v.Name || add.attrs["timestamp"] == "" {
		t.Errorf("SECRET_VERSION_ADD attributes = %v", add.attrs)
	}
	var version map[string]any
	if err := json.Unmarshal(add.data, &version); err != nil || version["name"] != v.Name || version["state"] != "ENABLED" {
		t.Errorf("SECRET_VERSION_ADD data = %s (%v)", add.data, err)
	}
	del := ps.msgs[len(ps.msgs)-1]
	if del.attrs["deleteType"] != "REQUESTED" || del.attrs["versionId"] != "" {
		t.Errorf("SECRET_DELETE attributes = %v", del.attrs)
	}
	var secret map[string]any
	if err := json.Unmarshal(del.data, &secret); err != nil || secret["name"] != sec.Name || secret["topics"] == nil {
		t.Errorf("SECRET_DELETE data = %s (%v)", del.data, err)
	}
}

func TestSecretTopicsRefusals(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	create := func(id string, topics ...string) error {
		sec := &secretmanagerpb.Secret{Replication: automatic()}
		for _, n := range topics {
			sec.Topics = append(sec.Topics, &secretmanagerpb.Topic{Name: n})
		}
		_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: id, Secret: sec})
		return err
	}
	// No Pub/Sub on the instance: nothing could be delivered.
	wantGRPCCode(t, create("none", "projects/demo/topics/a"), codes.Unimplemented)

	ps := &fakePubSub{topics: map[string]bool{"projects/demo/topics/a": true}}
	s.SetPublisher(ps.publish)
	wantGRPCCode(t, create("missing", "projects/demo/topics/gone"), codes.NotFound)
	wantGRPCCode(t, create("bad", "topics/a"), codes.InvalidArgument)
	var eleven []string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, "projects/demo/topics/a"+strings.Repeat("x", i))
	}
	wantGRPCCode(t, create("many", eleven...), codes.InvalidArgument)
	if all, _ := s.ListSecrets("demo"); len(all) != 0 {
		t.Errorf("refused creates stored %d secrets", len(all))
	}
	if len(ps.events()) != 0 {
		t.Errorf("refused creates published %v", ps.events())
	}
	// A secret without topics publishes nothing.
	if err := create("quiet"); err != nil {
		t.Fatal(err)
	}
	if len(ps.events()) != 0 {
		t.Errorf("a secret without topics published %v", ps.events())
	}
	// An update to a topic that does not exist is refused and changes nothing.
	_, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: "projects/demo/secrets/quiet", Topics: []*secretmanagerpb.Topic{{Name: "projects/demo/topics/gone"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"topics"}}})
	wantGRPCCode(t, err, codes.NotFound)
	if sec, _ := s.GetSecret("demo", "quiet"); len(sec.Topics) != 0 {
		t.Errorf("a refused update stored topics %v", sec.Topics)
	}
}

func TestSecretTopicsFilterAndREST(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	ps := &fakePubSub{topics: map[string]bool{"projects/demo/topics/a": true}}
	s.SetPublisher(ps.publish)
	code, body := do(t, srv, "POST", "/v1/projects/demo/secrets?secretId=r",
		`{"replication":{"automatic":{}},"topics":[{"name":"projects/demo/topics/a"}]}`)
	if code != 200 || !strings.Contains(body, `"topics":[{"name":"projects/demo/topics/a"}]`) {
		t.Fatalf("REST create = %d %s", code, body)
	}
	if code, body := do(t, srv, "PATCH", "/v1/projects/demo/secrets/r?updateMask=topics", `{"topics":[]}`); code != 200 || strings.Contains(body, "topics") {
		t.Errorf("REST clear topics = %d %s", code, body)
	}
	if _, err := s.CreateSecretWithTopics("demo", "f", nil, nil, time.Time{}, []string{"projects/demo/topics/a"}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSecretsFiltered("demo", "topics.name:projects/demo/topics/a")
	if err != nil || len(got) != 1 || got[0].Name != "projects/demo/secrets/f" {
		t.Errorf("filter on topics.name = %v %v", got, err)
	}
}
