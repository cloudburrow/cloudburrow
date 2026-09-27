//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/logadmin"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunRevisionUsesInjectedMetadataADCAndServedEndpoints (#681): a
// Cloud Run service deployed with no env of its own uses the rest of what
// the adapter injects.
//
//   - google.FindDefaultCredentials, with no credentials file anywhere,
//     falls through to the metadata server at the injected
//     GCE_METADATA_HOST, as it does on real Cloud Run. The token is a local
//     one, and an official Storage client sends it on a bucket listing.
//   - KMS Encrypt, Scheduler ListJobs and Logging WriteLogEntries are called
//     at the injected CLOUDBURROW_KMS_ENDPOINT, CLOUDBURROW_SCHEDULER_ENDPOINT
//     and CLOUDBURROW_LOGGING_ENDPOINT.
//
// The host then reads the effect back from the same servers: it decrypts
// the revision's ciphertext, sees the job it created in the revision's
// listing, and lists the entry the revision wrote.
func TestCloudRunRevisionUsesInjectedMetadataADCAndServedEndpoints(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	kmsAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)
	schedAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SCHEDULER", EnvScheduler)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)

	plain := func(addr string) []option.ClientOption {
		return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	}

	// What the revision reads: an object, a key and a job.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bh := bucket(t, h, sc)
	w := bh.Object("from-the-host.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write an object: %v", err)
	}

	kc, err := kms.NewKeyManagementClient(ctx, plain(kmsAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "adcprobe-ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}

	schc, err := scheduler.NewCloudSchedulerClient(ctx, plain(schedAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = schc.Close() })
	parent := "projects/" + h.Project() + "/locations/us-central1"
	// Once a year, to an address nothing listens on: it is only listed.
	job, err := schc.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/adcprobe", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:1/never", HttpMethod: schedulerpb.HttpMethod_POST}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = schc.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })

	// No Env at all.
	id := "compat-adcprobe"
	name := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{Image: envProbeImage}}}},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}

	// The injected variables are in the Knative Service, at in-cluster
	// addresses, and no credential is.
	manifest, err := kubectlGet(t, "ksvc", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: GCE_METADATA_HOST", "name: CLOUDBURROW_KMS_ENDPOINT",
		"name: CLOUDBURROW_SCHEDULER_ENDPOINT", "name: CLOUDBURROW_LOGGING_ENDPOINT", "cloudburrow-host."} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the Knative Service lacks %q:\n%s", want, manifest)
		}
	}
	for _, bad := range []string{"127.0.0.1", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if strings.Contains(manifest, bad) {
			t.Errorf("the Knative Service names %s:\n%s", bad, manifest)
		}
	}

	const logID = "adcprobe"
	base, host := ingress(t), hostOf(t, svc.GetUri())
	q := url.Values{"bucket": {bh.BucketName()}, "key": {key.GetName()}, "log": {logID}}
	code, body := httpGet(t, base, host, "/adc?"+q.Encode())
	if code != http.StatusOK || !strings.Contains(body, "ADC PROBE: OK") {
		t.Fatalf("the revision did not get credentials or reach the served services: %d %s", code, body)
	}
	t.Logf("%s", strings.TrimSpace(body))
	field := func(k string) string {
		m := regexp.MustCompile(`(?:^| )` + k + `=(\S*)`).FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return m[1]
	}

	// (a) The official auth library took a local token from the injected
	// metadata server and sent it on the Storage call.
	if field("token") != "local" || field("bearer_requests") == "" || field("bearer_requests") == "0" {
		t.Errorf("the ADC token was not a local one sent on a request: %s", body)
	}
	if field("adc_project") == "" {
		t.Errorf("ADC carried no project from the metadata server: %s", body)
	}
	if field("objects") != "from-the-host.txt" {
		t.Errorf("the ADC-authenticated listing saw %q, want the host's object", field("objects"))
	}
	for k, want := range map[string]string{"metadata": "cloudburrow-host.", "kms": "cloudburrow-host.",
		"scheduler": "cloudburrow-host.", "logging": "cloudburrow-host."} {
		if !strings.HasPrefix(field(k), want) {
			t.Errorf("the revision used %s=%q; want the injected %s… address", k, field(k), want)
		}
	}

	// (b) The host reads back what the revision did.
	ct, err := base64.StdEncoding.DecodeString(field("ciphertext"))
	if err != nil || len(ct) == 0 {
		t.Fatalf("the revision returned no ciphertext (%v): %s", err, body)
	}
	dec, err := kc.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: ct})
	if err != nil || string(dec.GetPlaintext()) != "from-the-revision" {
		t.Errorf("the host decrypted the revision's ciphertext to %q (%v), want from-the-revision", dec.GetPlaintext(), err)
	}
	if !strings.Contains(","+field("jobs")+",", ","+job.GetName()+",") {
		t.Errorf("the revision's ListJobs = %q, want it to include %s", field("jobs"), job.GetName())
	}

	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), plain(logAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	logName := "projects/" + h.Project() + "/logs/" + logID
	if field("logged") != logName {
		t.Errorf("the revision wrote to %q, want %s", field("logged"), logName)
	}
	it := ac.Entries(ctx, logadmin.Filter(`logName = "`+logName+`"`))
	var payloads []string
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if s, ok := e.Payload.(string); ok {
			payloads = append(payloads, s)
		}
	}
	if len(payloads) != 1 || payloads[0] != "from-the-revision" {
		t.Errorf("the host lists %q in %s, want the revision's one entry", payloads, logName)
	}
}
