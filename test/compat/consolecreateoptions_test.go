//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// consoleCreate posts a create form's values to the console.
func consoleCreate(t *testing.T, addr, service, project string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(values)
	return consoleDo(t, addr, http.MethodPost, "/api/resources/"+service+"?project="+project, string(body))
}

// TestConsolePubSubCreateOptions (#852): Create topic with a message
// retention and a schema, and Create subscription with every option it
// offers, each read back by the official client as the console set it. The
// options act, not only persist: the schema refuses a message that does not
// conform to it, and the filter delivers only matching messages. A malformed
// filter is refused at once with the emulator's answer rather than retried
// until the request's deadline. Expiration and exactly-once delivery (#873):
// a ttl under a day is refused by the API, exactly-once with a push endpoint
// by the console, and a subscription with both reads them back; that they
// act is TestPubSubSubscriptionExpiresWhenIdle and
// TestPubSubExactlyOnceDelivery.
func TestConsolePubSubCreateOptions(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	sc, err := vkit.NewSchemaClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	const schemaID = "console-create-schema"
	schema := "projects/" + project + "/schemas/" + schemaID
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: schemaID,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO,
			Definition: `{"type":"record","name":"Order","fields":[{"name":"n","type":"int"}]}`}}); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: schema}) })

	topic := "projects/" + project + "/topics/console-create-opts"
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
	})
	if code, body := consoleCreate(t, addr, "pubsub", project, map[string]string{
		"name": "console-create-opts", "defaultSubscription": "false",
		"messageRetention": "36h", "schema": schemaID, "schemaEncoding": "JSON",
	}); code != http.StatusOK {
		t.Fatalf("console create topic = %d: %s", code, body)
	}
	got, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMessageRetentionDuration().AsDuration() != 36*time.Hour || got.GetSchemaSettings().GetSchema() != schema ||
		got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_JSON {
		t.Errorf("the console's topic reads back as retention %v, schema %v", got.GetMessageRetentionDuration(), got.GetSchemaSettings())
	}
	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte(`{"n":"not a number"}`)}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("publishing a message the schema refuses = %v; want INVALID_ARGUMENT", err)
	}
	if code, body := consoleCreate(t, addr, "pubsub", project, map[string]string{
		"name": "console-create-noschema", "defaultSubscription": "false", "schema": "no-such-schema",
	}); code != http.StatusBadRequest || !strings.Contains(body, "NotFound") {
		t.Errorf("a topic naming a missing schema = %d %s; want the emulator's NOT_FOUND", code, body)
	}

	plainTopic := topic + "-plain"
	dead := topic + "-dead"
	for _, n := range []string{plainTopic, dead} {
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: n}); err != nil {
			t.Fatal(err)
		}
		n := n
		t.Cleanup(func() {
			_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: n})
		})
	}
	sub := "projects/" + project + "/subscriptions/console-create-opts-eu"
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	if code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-eu", "ackDeadline": "30", "messageRetention": "2d", "retainAcked": "true",
		"messageOrdering": "true", "filter": `attributes.region = "eu"`, "minBackoff": "5s", "maxBackoff": "1m",
		"deadLetterTopic": dead, "maxDeliveryAttempts": "7",
	}); code != http.StatusOK {
		t.Fatalf("console create subscription = %d: %s", code, body)
	}
	s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetTopic() != plainTopic || s.GetAckDeadlineSeconds() != 30 || s.GetMessageRetentionDuration().AsDuration() != 48*time.Hour ||
		!s.GetRetainAckedMessages() || !s.GetEnableMessageOrdering() || s.GetFilter() != `attributes.region = "eu"` ||
		s.GetRetryPolicy().GetMinimumBackoff().AsDuration() != 5*time.Second ||
		s.GetRetryPolicy().GetMaximumBackoff().AsDuration() != time.Minute ||
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != dead || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 {
		t.Errorf("the console's subscription reads back as %v", s)
	}
	// The filter acts: of three messages, only the matching one is delivered.
	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: plainTopic, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("eu"), Attributes: map[string]string{"region": "eu"}},
		{Data: []byte("us"), Attributes: map[string]string{"region": "us"}},
		{Data: []byte("none")},
	}}); err != nil {
		t.Fatal(err)
	}
	var pulled []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && len(pulled) == 0; {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, _ := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10, ReturnImmediately: true})
		cancel()
		for _, m := range resp.GetReceivedMessages() {
			pulled = append(pulled, string(m.GetMessage().GetData()))
		}
	}
	if strings.Join(pulled, ",") != "eu" {
		t.Errorf("the filtered subscription delivered %v; want only eu", pulled)
	}
	// Out of range: the emulator's own refusal, and no subscription.
	if code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-dl", "deadLetterTopic": dead, "maxDeliveryAttempts": "2",
	}); code != http.StatusBadRequest || !strings.Contains(body, "max_delivery_attempts is too small") {
		t.Errorf("2 delivery attempts = %d %s; want the emulator's refusal", code, body)
	}
	start := time.Now()
	code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-bad", "filter": "nonsense ==="})
	if code != http.StatusBadRequest || !strings.Contains(body, "the emulator refused the filter") {
		t.Errorf("a malformed filter = %d %s; want the console's refusal", code, body)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("a malformed filter took %v to be refused: the create was retried", d)
	}

	// Expiration and exactly-once delivery (#873). A ttl Google refuses is
	// refused by the API, with its reason, and creates nothing.
	if code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-short", "messageRetention": "10m", "expiration": "12h",
	}); code != http.StatusBadRequest || !strings.Contains(body, "at least 1 day") {
		t.Errorf("a 12h expiration = %d %s; want the API's refusal", code, body)
	}
	if code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-push", "exactlyOnce": "true", "pushEndpoint": "http://127.0.0.1:1/push",
	}); code != http.StatusBadRequest || !strings.Contains(body, "pull subscriptions only") {
		t.Errorf("exactly-once with a push endpoint = %d %s; want the console's refusal", code, body)
	}
	for _, n := range []string{"console-create-opts-short", "console-create-opts-push"} {
		if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
			Subscription: "projects/" + project + "/subscriptions/" + n}); status.Code(err) != codes.NotFound {
			t.Errorf("%s was refused, and reads %v", n, err)
		}
	}
	once := "projects/" + project + "/subscriptions/console-create-opts-once"
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: once})
	})
	if code, body := consoleAct(t, addr, "pubsub", project, []string{plainTopic}, "create-subscription", map[string]string{
		"name": "console-create-opts-once", "messageRetention": "1d", "expiration": "1d", "exactlyOnce": "true",
	}); code != http.StatusOK {
		t.Fatalf("console create subscription with expiration and exactly-once = %d: %s", code, body)
	}
	kept, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: once})
	if err != nil || kept.GetExpirationPolicy().GetTtl().AsDuration() != 24*time.Hour || !kept.GetEnableExactlyOnceDelivery() {
		t.Errorf("the console's subscription reads %v, %v; want a 1-day ttl and exactly-once", kept, err)
	}
}

// TestConsoleKMSCreateKeyOptions (#852), in the served shard: Create key on a
// ring's page sets the destroy scheduled duration and the rotation schedule,
// which the official client's GetCryptoKey reads back; a duration below 24
// hours is refused with the message the official client's own CreateCryptoKey
// receives, and creates nothing.
func TestConsoleKMSCreateKeyOptions(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := h.Project()
	// Rings and keys cannot be deleted; the project is the test's own.
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent: "projects/" + project + "/locations/global", KeyRingId: "console-create-opts"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	next := time.Now().UTC().Add(14 * 24 * time.Hour).Truncate(time.Second)
	if code, body := consoleAct(t, addr, "kms", project, []string{ring.GetName()}, "createkey", map[string]string{
		"cryptoKeyId": "k", "destroyScheduledDuration": "3d", "rotationPeriod": "30d",
		"nextRotationTime": next.Format(time.RFC3339)}); code != http.StatusOK {
		t.Fatalf("console create key = %d: %s", code, body)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring.GetName() + "/cryptoKeys/k"})
	if err != nil {
		t.Fatal(err)
	}
	if k.GetDestroyScheduledDuration().AsDuration() != 72*time.Hour || k.GetRotationPeriod().AsDuration() != 30*24*time.Hour ||
		!k.GetNextRotationTime().AsTime().Equal(next) {
		t.Errorf("the console's key reads destroy %v, rotation %v, next %v", k.GetDestroyScheduledDuration(),
			k.GetRotationPeriod(), k.GetNextRotationTime())
	}

	_, sdkErr := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sdk-short",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, DestroyScheduledDuration: durationpb.New(12 * time.Hour)}})
	st, _ := status.FromError(sdkErr)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("the official client's CreateCryptoKey with 12h = %v", sdkErr)
	}
	code, body := consoleAct(t, addr, "kms", project, []string{ring.GetName()}, "createkey", map[string]string{
		"cryptoKeyId": "short", "destroyScheduledDuration": "12h"})
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if code != http.StatusBadRequest || !strings.Contains(refusal.Error, st.Message()) {
		t.Errorf("console create with 12h = %d %s; want the official client's own %q", code, body, st.Message())
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring.GetName() + "/cryptoKeys/short"}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused create left a key: %v", err)
	}
}

// TestConsoleStorageCreateBucketOptions (#852): Create bucket with labels, a
// storage class, uniform access, versioning, a soft delete retention and
// object retention, each read back by the official client; a soft delete
// retention the API refuses is refused with its message and creates nothing.
func TestConsoleStorageCreateBucketOptions(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bucket := h.Project() + "-create-opts"
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(context.Background()) })
	if code, body := consoleCreate(t, addr, "storage", h.Project(), map[string]string{
		"name": bucket, "labels": `{"team":"blue"}`, "storageClass": "COLDLINE", "uniformAccess": "true",
		"versioning": "true", "softDeleteSeconds": "864000", "objectRetention": "true",
	}); code != http.StatusOK {
		t.Fatalf("console create bucket = %d: %s", code, body)
	}
	a, err := sc.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Labels["team"] != "blue" || a.StorageClass != "COLDLINE" || !a.UniformBucketLevelAccess.Enabled || !a.VersioningEnabled ||
		a.SoftDeletePolicy == nil || a.SoftDeletePolicy.RetentionDuration != 10*24*time.Hour || a.ObjectRetentionMode != "Enabled" {
		t.Errorf("the console's bucket reads labels %v, class %s, uniform %v, versioning %v, soft delete %+v, object retention %q",
			a.Labels, a.StorageClass, a.UniformBucketLevelAccess.Enabled, a.VersioningEnabled, a.SoftDeletePolicy, a.ObjectRetentionMode)
	}
	refused := h.Project() + "-create-refused"
	code, body := consoleCreate(t, addr, "storage", h.Project(), map[string]string{"name": refused, "softDeleteSeconds": "5"})
	if code != http.StatusBadRequest || !strings.Contains(body, "softDeletePolicy.retentionDurationSeconds") {
		t.Errorf("a 5-second soft delete = %d %s; want buckets.insert's refusal", code, body)
	}
	if _, err := sc.Bucket(refused).Attrs(ctx); err == nil {
		_ = sc.Bucket(refused).Delete(context.Background())
		t.Error("a refused create left a bucket")
	}
}

// TestConsoleRunJobLabelsSecretsAndOverrides (#852), in the run shard: a job
// created on the console with labels and a variable from Secret Manager is
// read back by the official JobsClient with both, the variable as the
// secretKeyRef that was set; Execute with overrides starts an execution whose
// task count, timeout, arguments and variables are the overrides, which runs
// to success with the secret's payload in the task, and leaves the job as it
// was.
func TestConsoleRunJobLabelsSecretsAndOverrides(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	jc, xc := runJobClients(t, h)
	secrets := secretsClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()

	const secretID = "console-job-opts-secret"
	secretName := secretsParent(h) + "/secrets/" + secretID
	if _, err := secrets.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = secrets.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secretName})
	})
	if _, err := secrets.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("opts-secret-value")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}

	id := "console-job-opts"
	name := runParent(h) + "/jobs/" + id
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: name}) })
	// The check is split so the payload is not a literal in the manifest.
	if code, body := consoleCreate(t, addr, "run-jobs", project, map[string]string{
		"name": id, "image": jobImage, "command": "sh -c", "args": `'echo "no overrides"; exit 3'`,
		"env": `{"MODE":"full"}`, "secretEnv": fmt.Sprintf(`{"TOKEN":%q}`, secretID+":latest"),
		"labels": `{"team":"data"}`, "taskCount": "1", "maxRetries": "0", "timeout": "120",
	}); code != http.StatusOK {
		t.Fatalf("console create job = %d: %s", code, body)
	}
	job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]*runpb.SecretKeySelector{}
	for _, e := range job.GetTemplate().GetTemplate().GetContainers()[0].GetEnv() {
		ref[e.GetName()] = e.GetValueSource().GetSecretKeyRef()
	}
	if job.GetLabels()["team"] != "data" || ref["TOKEN"].GetSecret() != secretID || ref["TOKEN"].GetVersion() != "latest" {
		t.Errorf("the console's job reads labels %v, TOKEN %v", job.GetLabels(), ref["TOKEN"])
	}

	// The job as created would exit 3; the overrides make it check the
	// secret and the overridden variable and succeed, twice.
	if code, body := consoleAct(t, addr, "run-jobs", project, []string{id}, "execute-overrides", map[string]string{
		"args": `'[ "${TOKEN#opts-secret-}" = "value" ] && [ "$MODE" = "delta" ] && echo ok'`,
		"env":  `{"MODE":"delta"}`, "taskCount": "2", "timeout": "90",
	}); code != http.StatusOK {
		t.Fatalf("console execute with overrides = %d: %s", code, body)
	}
	var e *runpb.Execution
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		resp, err := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: name}).Next()
		if err == nil && resp.GetCompletionTime() != nil {
			e = resp
			break
		}
	}
	if e == nil {
		t.Fatal("the execution did not finish within four minutes")
	}
	if e.GetTaskCount() != 2 || e.GetSucceededCount() != 2 || e.GetTemplate().GetTimeout().AsDuration() != 90*time.Second {
		t.Errorf("the execution ran %d tasks (%d succeeded) with timeout %v; want the overrides' 2 and 90s",
			e.GetTaskCount(), e.GetSucceededCount(), e.GetTemplate().GetTimeout())
	}
	after, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if after.GetTemplate().GetTaskCount() != 1 || after.GetGeneration() != job.GetGeneration() {
		t.Errorf("Execute with overrides changed the job: %v", after.GetTemplate())
	}
}

// TestConsoleRunServiceLabelsAndSecrets (#852), in the run shard: a service
// deployed on the console with labels and a variable from Secret Manager is
// read back by the official ServicesClient with both, the variable as the
// secretKeyRef that was set, and its edit form is prefilled with them.
func TestConsoleRunServiceLabelsAndSecrets(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	secrets := secretsClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()

	const secretID = "console-svc-opts-secret"
	secretName := secretsParent(h) + "/secrets/" + secretID
	if _, err := secrets.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = secrets.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secretName})
	})
	if _, err := secrets.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secretName,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("svc-secret")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}

	id := "console-svc-opts"
	name := runParent(h) + "/services/" + id
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = c.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	if code, body := consoleCreate(t, addr, "run", project, map[string]string{
		"name": id, "image": "ghcr.io/knative/helloworld-go:latest", "labels": `{"team":"web"}`,
		"env": `{"TARGET":"opts"}`, "secretEnv": fmt.Sprintf(`{"TOKEN":%q}`, secretID+":1"),
	}); code != http.StatusOK {
		t.Fatalf("console deploy = %d: %s", code, body)
	}
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]*runpb.SecretKeySelector{}
	for _, e := range svc.GetTemplate().GetContainers()[0].GetEnv() {
		ref[e.GetName()] = e.GetValueSource().GetSecretKeyRef()
	}
	if svc.GetLabels()["team"] != "web" || ref["TOKEN"].GetSecret() != secretID || ref["TOKEN"].GetVersion() != "1" {
		t.Errorf("the console's service reads labels %v, TOKEN %v", svc.GetLabels(), ref["TOKEN"])
	}
	_, values, _ := consoleRunEditForm(t, addr, project, id)
	if values["labels"] != `{"team":"web"}` || values["secretEnv"] != fmt.Sprintf(`{"TOKEN":%q}`, secretID+":1") {
		t.Errorf("the edit form is prefilled with labels %q and secretEnv %q", values["labels"], values["secretEnv"])
	}
}
