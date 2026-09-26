package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// seedTwice seeds a document, then the same document again, then again with
// ifNotExists. It is the acceptance shape every seeder shares: the first
// succeeds, the repeat is ALREADY_EXISTS, and ifNotExists makes it a no-op.
func seedTwice(t *testing.T, seed func(json.RawMessage) error, doc string) {
	t.Helper()
	if err := seed(json.RawMessage(doc)); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if err := seed(json.RawMessage(doc)); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("an identical second seed returned %v, want ALREADY_EXISTS", err)
	}
	withSkip := strings.Replace(doc, "{", `{"ifNotExists": true, `, 1)
	if err := seed(json.RawMessage(withSkip)); err != nil {
		t.Fatalf("a repeat with ifNotExists failed: %v", err)
	}
}

// ---------------------------------------------------------------- storage

type storedObject struct {
	contentType string
	metadata    map[string]string
	data        string
}

// fakeUploads accepts bucket creation and multipart uploads, and enforces the
// two conflicts the seeder relies on: 409 for an existing bucket, 412 for an
// existing object under ifGenerationMatch=0.
type fakeUploads struct {
	mu      sync.Mutex
	project string
	buckets map[string]map[string]storedObject
	uploads int
}

func (f *fakeUploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/storage/v1/b":
		f.project = r.URL.Query().Get("project")
		var b struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := f.buckets[b.Name]; ok {
			http.Error(w, "exists", http.StatusConflict)
			return
		}
		f.buckets[b.Name] = map[string]storedObject{}
		_, _ = io.WriteString(w, "{}")
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"):
		bucket := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload/storage/v1/b/"), "/o")
		if r.URL.Query().Get("uploadType") != "multipart" || r.URL.Query().Get("ifGenerationMatch") != "0" {
			http.Error(w, "unconditional upload", http.StatusBadRequest)
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		metaPart, _ := mr.NextPart()
		var meta struct {
			Name        string
			ContentType string
			Metadata    map[string]string
		}
		_ = json.NewDecoder(metaPart).Decode(&meta)
		dataPart, _ := mr.NextPart()
		data, _ := io.ReadAll(dataPart)
		if _, ok := f.buckets[bucket][meta.Name]; ok {
			http.Error(w, "precondition", http.StatusPreconditionFailed)
			return
		}
		f.buckets[bucket][meta.Name] = storedObject{meta.ContentType, meta.Metadata, string(data)}
		f.uploads++
		_, _ = io.WriteString(w, "{}")
	default:
		http.Error(w, r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func TestStorageSeedCreatesBucketsAndObjectsOnce(t *testing.T) {
	fake := &fakeUploads{buckets: map[string]map[string]storedObject{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s := &storageSeeder{project: "dev-project", addr: func() string { return srv.Listener.Addr().String() }}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}

	seedTwice(t, seed, `{"buckets": [{"name": "assets", "objects": [
		{"name": "hello.txt", "content": "hello", "contentType": "text/plain", "metadata": {"owner": "seed"}},
		{"name": "dir/bin", "contentBase64": "AAEC"},
		{"name": "empty"}
	]}]}`)

	got := fake.buckets["assets"]
	if o := got["hello.txt"]; o.data != "hello" || o.contentType != "text/plain" || o.metadata["owner"] != "seed" {
		t.Errorf("hello.txt stored as %+v", o)
	}
	if o := got["dir/bin"]; o.data != "\x00\x01\x02" || o.contentType != "application/octet-stream" {
		t.Errorf("dir/bin stored as %+v, want the decoded bytes and the default type", o)
	}
	if _, ok := got["empty"]; !ok {
		t.Error("an object with no content was not created")
	}
	if fake.uploads != 3 {
		t.Errorf("%d uploads, want 3: a repeat must not rewrite anything", fake.uploads)
	}
	if fake.project != "dev-project" {
		t.Errorf("bucket created in project %q", fake.project)
	}
}

// ----------------------------------------------------------------- pubsub

func TestPubSubSeedPassesSubscriptionConfigThrough(t *testing.T) {
	fake := pstest.NewServer()
	defer func() { _ = fake.Close() }()
	p := &pubsubSeeder{tunnel: forwarderAt(t, fake.Addr)}
	ctx := context.Background()
	seed := func(doc json.RawMessage) error {
		if err := p.Validate(doc); err != nil {
			return err
		}
		return p.Seed(ctx, doc)
	}

	seedTwice(t, seed, `{"topics": [
		{"name": "projects/seed-proj/topics/orders", "labels": {"team": "a"}},
		{"name": "projects/seed-proj/topics/orders-dlq"}
	], "subscriptions": [{
		"name": "projects/seed-proj/subscriptions/orders-sub",
		"topic": "projects/seed-proj/topics/orders",
		"ackDeadlineSeconds": 42,
		"filter": "attributes.kind = \"paid\"",
		"pushConfig": {"pushEndpoint": "http://worker.default.svc.cluster.local/push"},
		"deadLetterPolicy": {"deadLetterTopic": "projects/seed-proj/topics/orders-dlq", "maxDeliveryAttempts": 7},
		"retryPolicy": {"minimumBackoff": "5s", "maximumBackoff": "60s"}
	}]}`)

	c, err := pubsubAdmin(ctx, p.tunnel, "seed-proj")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sub, err := c.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: "projects/seed-proj/subscriptions/orders-sub"})
	if err != nil {
		t.Fatal(err)
	}
	if sub.AckDeadlineSeconds != 42 || sub.Filter != `attributes.kind = "paid"` ||
		sub.GetPushConfig().GetPushEndpoint() != "http://worker.default.svc.cluster.local/push" ||
		sub.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 ||
		sub.GetRetryPolicy().GetMaximumBackoff().AsDuration().Seconds() != 60 {
		t.Errorf("subscription seeded as %v", sub)
	}
	topic, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/seed-proj/topics/orders"})
	if err != nil || topic.Labels["team"] != "a" {
		t.Errorf("topic seeded as %v (%v)", topic, err)
	}
}

// A field the emulator is not known to honour is refused by name, never
// dropped: a seed that accepted a schema and ignored it would report a
// validation the application never gets.
func TestPubSubSeedRefusesWhatTheEmulatorDoesNotHonour(t *testing.T) {
	p := &pubsubSeeder{}
	for doc, want := range map[string]string{
		`{"topics": [{"name": "projects/seed-proj/topics/t-one", "schemaSettings": {"schema": "projects/seed-proj/schemas/s"}}]}`:                                "schemaSettings",
		`{"topics": [{"name": "projects/seed-proj/topics/t-one", "kmsKeyName": "k"}]}`:                                                                           "kmsKeyName",
		`{"subscriptions": [{"name": "projects/seed-proj/subscriptions/s-one", "topic": "projects/seed-proj/topics/t-one", "bigqueryConfig": {}}]}`:              "bigqueryConfig",
		`{"subscriptions": [{"name": "projects/seed-proj/subscriptions/s-one", "topic": "projects/seed-proj/topics/t-one", "enableExactlyOnceDelivery": true}]}`: "enableExactlyOnceDelivery",
		`{"topics": [{"name": "projects/seed-proj/topics/t-one", "colour": "red"}]}`:                                                                             "colour",
		`{"topics": [{"name": "orders"}]}`: "topics[0].name",
		`{"subscriptions": [{"name": "projects/seed-proj/subscriptions/s-one", "topic": "projects/seed-proj/topics/t-one", "ackDeadlineSeconds": 5}]}`:                        "ackDeadlineSeconds",
		`{"subscriptions": [{"name": "projects/seed-proj/subscriptions/s-one", "topic": "projects/seed-proj/topics/t-one", "retryPolicy": {"minimumBackoff": "5 seconds"}}]}`: "minimumBackoff",
	} {
		err := p.Validate(json.RawMessage(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
}

// ---------------------------------------------------------- secretmanager

func TestSecretSeedAddsVersionsOnce(t *testing.T) {
	st := secrets.NewStore(store.NewMemory())
	s := &secretsSeeder{svc: &secretsService{store: st}}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}

	seedTwice(t, seed, `{"secrets": [{"name": "projects/seed-proj/secrets/api-key", "labels": {"env": "dev"},
		"versions": [{"data": "v1"}, {"dataBase64": "djI="}]}]}`)

	latest, err := st.AccessVersion("seed-proj", "api-key", "latest")
	if err != nil || string(latest.Payload) != "v2" {
		t.Errorf("latest = %q (%v), want the last version listed", latest.Payload, err)
	}
	versions, _ := st.ListVersions("seed-proj", "api-key")
	if len(versions) != 2 {
		t.Errorf("%d versions after a repeated seed, want 2: ifNotExists must not duplicate them", len(versions))
	}
	sec, _ := st.GetSecret("seed-proj", "api-key")
	if sec.Labels["env"] != "dev" {
		t.Errorf("labels = %v", sec.Labels)
	}
}

func TestSecretSeedValidation(t *testing.T) {
	s := &secretsSeeder{}
	for doc, want := range map[string]string{
		`{"secrets": [{"name": "api-key"}]}`:                                                                 "secrets[0].name",
		`{"secrets": [{"name": "projects/p/secrets/k", "versions": [{}]}]}`:                                  "versions[0]",
		`{"secrets": [{"name": "projects/p/secrets/k", "versions": [{"data": "a", "dataBase64": "YQ=="}]}]}`: "exclusive",
		`{"secrets": [{"name": "projects/p/secrets/k", "replication": "user-managed"}]}`:                     "replication",
	} {
		err := s.Validate(json.RawMessage(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
}

// ------------------------------------------------------------------ tasks

func TestTasksSeedIsStrictAndRepeatable(t *testing.T) {
	st := tasks.NewStore(store.NewMemory())
	s := &tasksSeeder{svc: &tasksService{store: st}}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}
	seedTwice(t, seed, `{"queues": ["projects/seed-proj/locations/us-central1/queues/work"]}`)

	for doc, want := range map[string]string{
		`{"queues": ["work"]}`: "queues[0]",
		`{"queue": []}`:        "queue",
	} {
		if err := s.Validate(json.RawMessage(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
}

// TestTheSeedSchemaMatchesTheDecoder keeps docs/seed.schema.json honest. The
// decoder refuses unknown fields, so a field the schema lists and the decoder
// lacks is refused in practice, and one the decoder takes that the schema
// omits is undocumented. Fields refused by name are the only permitted
// difference: the decoder knows them so it can name them in its refusal.
func TestTheSeedSchemaMatchesTheDecoder(t *testing.T) {
	raw, err := os.ReadFile("../../docs/seed.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	defs := schema["$defs"].(map[string]any)
	props := func(node any, path ...string) map[string]any {
		for _, p := range path {
			node = node.(map[string]any)[p]
		}
		return node.(map[string]any)["properties"].(map[string]any)
	}
	refused := map[string]map[string]bool{
		"bucket":       {"labels": true, "location": true, "storageClass": true},
		"topic":        {"schemaSettings": true, "kmsKeyName": true},
		"subscription": {"bigqueryConfig": true, "cloudStorageConfig": true, "enableExactlyOnceDelivery": true},
	}

	for _, c := range []struct {
		name   string
		schema map[string]any
		typ    reflect.Type
	}{
		{"tasks", props(defs["tasks"]), reflect.TypeOf(tasksSeedSpec{})},
		{"storage", props(defs["storage"]), reflect.TypeOf(storageSeed{})},
		{"bucket", props(defs["storage"], "properties", "buckets", "items"), reflect.TypeOf(bucketSeed{})},
		{"object", props(defs["storage"], "properties", "buckets", "items", "properties", "objects", "items"), reflect.TypeOf(objectSeed{})},
		{"pubsub", props(defs["pubsub"]), reflect.TypeOf(pubsubSeed{})},
		{"topic", props(defs["pubsub"], "properties", "topics", "items"), reflect.TypeOf(topicSeed{})},
		{"subscription", props(defs["pubsub"], "properties", "subscriptions", "items"), reflect.TypeOf(subscriptionSeed{})},
		{"secretmanager", props(defs["secretmanager"]), reflect.TypeOf(secretsSeed{})},
		{"secret", props(defs["secretmanager"], "properties", "secrets", "items"), reflect.TypeOf(secretSeed{})},
		{"version", props(defs["secretmanager"], "properties", "secrets", "items", "properties", "versions", "items"), reflect.TypeOf(versionSeed{})},
	} {
		inGo := map[string]bool{}
		for i := 0; i < c.typ.NumField(); i++ {
			name := strings.Split(c.typ.Field(i).Tag.Get("json"), ",")[0]
			if !refused[c.name][name] {
				inGo[name] = true
			}
		}
		for name := range c.schema {
			if !inGo[name] {
				t.Errorf("%s: the schema lists %q, which the decoder refuses", c.name, name)
			}
			delete(inGo, name)
		}
		for name := range inGo {
			t.Errorf("%s: the decoder accepts %q, which the schema does not document", c.name, name)
		}
	}
}

// Against the builtin server (#503), a seed may set labels, location and
// storageClass, and the bucket reads back with them.
func TestStorageSeedAcceptsLabels(t *testing.T) {
	gcs, err := storage.NewServer(storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gcs)
	defer srv.Close()
	s := &storageSeeder{project: "dev-project", addr: func() string { return srv.Listener.Addr().String() }}
	doc := json.RawMessage(`{"buckets": [{"name": "labelled", "labels": {"env": "dev"}, "location": "EU", "storageClass": "NEARLINE"}]}`)
	if err := s.Validate(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.Seed(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/storage/v1/b/labelled?prettyPrint=false")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b struct {
		Labels       map[string]string `json:"labels"`
		Location     string            `json:"location"`
		StorageClass string            `json:"storageClass"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Labels["env"] != "dev" || b.Location != "EU" || b.StorageClass != "NEARLINE" {
		t.Errorf("seeded bucket = %+v", b)
	}
}

// The seed tests against the builtin server (#510): buckets and objects are
// created once, a repeat is ALREADY_EXISTS unless ifNotExists, and a seeded
// upload is an ordinary write, so it emits its notification.
func TestStorageSeedCreatesBucketsAndObjectsOnceOnBuiltin(t *testing.T) {
	pub := &recordingPublisher{}
	gcs, err := storage.NewServer(storage.Options{Publisher: pub})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gcs)
	defer srv.Close()
	s := &storageSeeder{project: "dev-project", addr: func() string { return srv.Listener.Addr().String() }}
	seed := func(doc json.RawMessage) error {
		if err := s.Validate(doc); err != nil {
			return err
		}
		return s.Seed(context.Background(), doc)
	}
	// A notification configuration on the bucket the seed will create
	// cannot exist yet, so seed the bucket first, configure, then seed its
	// objects.
	if err := seed(json.RawMessage(`{"buckets": [{"name": "assets", "labels": {"env": "dev"}}]}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/storage/v1/b/assets/notificationConfigs", "application/json", strings.NewReader(`{"topic":"projects/dev-project/topics/t"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("notification config: %v %v", resp, err)
	}
	resp.Body.Close()
	seedTwice(t, seed, `{"buckets": [{"name": "more", "objects": [
		{"name": "hello.txt", "content": "hello", "contentType": "text/plain", "metadata": {"owner": "seed"}},
		{"name": "dir/bin", "contentBase64": "AAEC"}
	]}]}`)
	get := func(path string) string {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if got := get("/download/storage/v1/b/more/o/hello.txt?alt=media"); got != "hello" {
		t.Errorf("hello.txt = %q", got)
	}
	if meta := get("/storage/v1/b/more/o/hello.txt?prettyPrint=false"); !strings.Contains(meta, `"owner":"seed"`) || !strings.Contains(meta, `"contentType":"text/plain"`) {
		t.Errorf("hello.txt metadata = %s", meta)
	}
	if got := get("/download/storage/v1/b/more/o/dir%2Fbin?alt=media"); got != "\x00\x01\x02" {
		t.Errorf("dir/bin = %q", got)
	}
	if err := s.Seed(context.Background(), json.RawMessage(`{"buckets": [{"name": "assets", "objects": [{"name": "note.txt", "content": "n"}]}], "ifNotExists": true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := gcs.DispatchNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pub.events(); len(got) != 1 || got[0] != "OBJECT_FINALIZE note.txt" {
		t.Errorf("notifications from the seed = %v; want the seeded object's OBJECT_FINALIZE", got)
	}
}

// Validation names the field at fault; labels, location and storageClass
// are kept (#503), so they are accepted.
func TestStorageSeedValidation(t *testing.T) {
	s := &storageSeeder{addr: func() string { return "" }}
	for doc, want := range map[string]string{
		`{"buckets": [{"name": "Bad_Name"}]}`: "buckets[0].name",
		`{"buckets": [{"name": "ok-bucket", "objects": [{"name": "x", "content": "a", "contentBase64": "YQ=="}]}]}`: "exclusive",
		`{"buckets": [{"name": "ok-bucket", "objects": [{"name": "x", "contentBase64": "%%%"}]}]}`:                  "contentBase64",
		`{"buckets": [{"name": "ok-bucket", "objects": [{"content": "a"}]}]}`:                                       "objects[0].name is required",
		`{"buckets": [{"name": "ok-bucket"}, {"name": "ok-bucket"}]}`:                                               "appears twice",
		`{"buckets": [{"name": "ok-bucket", "versioning": true}]}`:                                                  "versioning",
	} {
		if err := s.Validate(json.RawMessage(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
	if err := s.Validate(json.RawMessage(`{"buckets": [{"name": "ok-bucket", "labels": {"a": "b"}, "location": "EU", "storageClass": "COLDLINE"}]}`)); err != nil {
		t.Errorf("labels, location and storageClass: %v", err)
	}
}

type recordingPublisher struct {
	mu  sync.Mutex
	got []string
}

func (p *recordingPublisher) Publish(_ context.Context, _ string, _ []byte, attrs map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, attrs["eventType"]+" "+attrs["objectId"])
	return nil
}

func (p *recordingPublisher) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}
