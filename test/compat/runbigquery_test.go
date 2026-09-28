//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
)

// TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront (#874): a Cloud
// Run service deployed with no env of its own calls BigQuery, through the
// official Go client, at the CLOUDBURROW_BIGQUERY_ENDPOINT the adapter
// injected, and gets BigQuery's refusals: 409 for a dataset that exists,
// 400 for a dataset ID with a hyphen and for two columns whose names differ
// only in case, and an "invalid" insertErrors entry for a row missing its
// REQUIRED value. The emulator answers the first and third with a 500 and
// accepts the second and fourth (#861); the pod gets the front's answers
// because the address it is given is the host tunnel's front, published at
// cloudburrow-host, not the emulator's own Service.
func TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	endpoint := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set: the emulator serves one project, and the revision must use it", EnvBigQueryProject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)

	// The host's client, to clean up what the revision made.
	bq, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Close() })
	dataset := "incluster_" + strings.ReplaceAll(h.Project(), "-", "_")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = bq.Dataset(dataset).DeleteWithContents(cctx)
	})

	id := "compat-bqprobe"
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

	// The Knative Service carries the front's address, not the emulator's.
	manifest, err := kubectlGet(t, "ksvc", id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, "name: CLOUDBURROW_BIGQUERY_ENDPOINT") {
		t.Fatalf("the Knative Service lacks CLOUDBURROW_BIGQUERY_ENDPOINT:\n%s", manifest)
	}

	q := url.Values{"project": {project}, "dataset": {dataset}}
	code, body := httpGet(t, ingress(t), hostOf(t, svc.GetUri()), "/bigquery?"+q.Encode())
	t.Logf("%s", strings.TrimSpace(body))
	if code != http.StatusOK || !strings.Contains(body, "BIGQUERY PROBE: OK") {
		t.Fatalf("the revision could not use BigQuery: %d %s", code, body)
	}
	if !strings.Contains(body, "endpoint=http://cloudburrow-host.") {
		t.Errorf("the revision was given BigQuery at another address than the front's: %s", body)
	}
	for _, want := range []string{"create=ok", "duplicate=409", "invalid_id=400", "duplicate_column=400", "missing_required=invalid"} {
		if !strings.Contains(body, want+" ") && !strings.HasSuffix(strings.TrimSpace(body), want) {
			t.Errorf("the revision's BigQuery did not answer %s: %s", want, body)
		}
	}
	// What was refused is not there.
	if _, err := bq.Dataset(dataset + "-bad").Metadata(ctx); err == nil {
		t.Errorf("the refused dataset %s-bad was created", dataset)
	}
	if _, err := bq.Dataset(dataset).Table("twice").Metadata(ctx); err == nil {
		t.Error("the table with a column named twice was created")
	}
	if n := countRows(t, h, bq.Dataset(dataset).Table("rows")); n != 0 {
		t.Errorf("the row missing its REQUIRED value was stored: %d rows", n)
	}
}
