//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
)

// TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront (#874, #902): a
// Cloud Run service deployed with no env of its own calls BigQuery, through
// the official Go client, at the CLOUDBURROW_BIGQUERY_ENDPOINT the adapter
// injected, and gets BigQuery's refusals: 409 for a dataset that exists,
// 400 for a dataset ID with a hyphen and for two columns whose names differ
// only in case, and an "invalid" insertErrors entry for a row missing its
// REQUIRED value. The emulator answers the first and third with a 500 and
// accepts the second and fourth (#861); the pod gets the front's answers
// because the address it is given, the emulator's own Service, is served
// by the front in the emulator's pod.
func TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront(t *testing.T) {
	body := bigqueryFromRevision(t, "compat-bqprobe", "incluster_", nil)
	if !strings.Contains(body, "endpoint=http://bigquery."+testNamespace()+".svc.cluster.local:9050 ") {
		t.Errorf("the revision was given BigQuery at another address than its Service's: %s", body)
	}
}

// TestCloudRunRevisionDialingTheBigQueryServiceGetsTheFront (#881, #902): a
// revision that dials the emulator's own Service,
// bigquery.<namespace>.svc.cluster.local:9050, itself gets the same
// refusals: that port is the front's, in the emulator's pod, and the
// Service selects that pod. Its Storage Read port, 9060, still answers.
func TestCloudRunRevisionDialingTheBigQueryServiceGetsTheFront(t *testing.T) {
	ns := testNamespace()
	svc := "bigquery." + ns + ".svc.cluster.local"
	body := bigqueryFromRevision(t, "compat-bqsvc", "viasvc_", url.Values{
		"endpoint": {"http://" + svc + ":9050"}, "storage": {svc + ":9060"}})
	if !strings.Contains(body, "endpoint=http://"+svc+":9050 ") {
		t.Errorf("the probe did not use the Service's address: %s", body)
	}
	if !strings.Contains(body, "storage_read=reachable") {
		t.Errorf("the Service's Storage Read port does not answer: %s", body)
	}
	bigQueryServiceSelectsTheFrontPod(t, ns)
}

// TestAPodDialingTheBigQueryServiceGetsTheFrontWithoutCloudRun (#902): on
// an instance without Cloud Run, which publishes nothing to pods, a one-off
// pod that dials bigquery.<namespace>.svc.cluster.local:9050 with the
// official Go client gets the front's refusals, and none of what they
// refused is stored: the front serves that port in the emulator's pod, so
// no route through the host is needed.
func TestAPodDialingTheBigQueryServiceGetsTheFrontWithoutCloudRun(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set: the emulator serves one project, and the pod must use it", EnvBigQueryProject)
	}
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)
	ns := testNamespace()
	// Nothing is published to pods on this instance: no cloudburrow-host.
	if _, err := kubectlGetIn(t, ns, "service", "cloudburrow-host"); err == nil {
		t.Skip("this instance publishes host services to pods (Cloud Run is enabled); the Cloud Run tests cover it")
	}

	bq, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Close() })
	dataset := "podsvc_" + strings.ReplaceAll(h.Project(), "-", "_")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = bq.Dataset(dataset).DeleteWithContents(cctx)
	})

	svc := "bigquery." + ns + ".svc.cluster.local"
	q := url.Values{"project": {project}, "dataset": {dataset},
		"endpoint": {"http://" + svc + ":9050"}, "storage": {svc + ":9060"}}
	name := "compat-bqpod-" + strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(name) > 60 {
		name = name[:60]
	}
	body := runPod(t, ctx, kubeconfig, strings.TrimRight(name, "-"), envProbeImage, nil, "bigquery", q.Encode())
	t.Logf("%s", body)
	checkBigQueryProbe(t, h, bq, dataset, body)
	if !strings.Contains(body, "endpoint=http://"+svc+":9050 ") || !strings.Contains(body, "storage_read=reachable") {
		t.Errorf("the pod did not use the Service's ports: %s", body)
	}
	bigQueryServiceSelectsTheFrontPod(t, ns)
}

// bigQueryServiceSelectsTheFrontPod checks that the bigquery Service
// selects the emulator's pod, whose front container serves the REST port.
func bigQueryServiceSelectsTheFrontPod(t *testing.T, ns string) {
	t.Helper()
	manifest, err := kubectlGetIn(t, ns, "service", "bigquery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, "selector:\n    app: bigquery\n") {
		t.Errorf("the bigquery Service does not select the emulator's pod:\n%s", manifest)
	}
	deploy, err := kubectlGetIn(t, ns, "deployment", "bigquery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deploy, "- bigquery-front\n") || !strings.Contains(deploy, "name: front\n") {
		t.Errorf("the bigquery pod runs no front:\n%s", deploy)
	}
}

// testNamespace is the instance's namespace, as the harness is told it.
func testNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE")); ns != "" {
		return ns
	}
	return "cloudburrow"
}

// bigqueryFromRevision deploys the env probe as a Cloud Run service with no
// env of its own, calls its /bigquery with extra, checks that every request
// BigQuery refuses was refused and that none of them is stored, and returns
// what the probe reported.
func bigqueryFromRevision(t *testing.T, id, datasetPrefix string, extra url.Values) string {
	t.Helper()
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
	dataset := datasetPrefix + strings.ReplaceAll(h.Project(), "-", "_")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = bq.Dataset(dataset).DeleteWithContents(cctx)
	})

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
	for k, v := range extra {
		q[k] = v
	}
	code, body := httpGet(t, ingress(t), hostOf(t, svc.GetUri()), "/bigquery?"+q.Encode())
	t.Logf("%s", strings.TrimSpace(body))
	if code != http.StatusOK {
		t.Fatalf("the revision could not use BigQuery: %d %s", code, body)
	}
	checkBigQueryProbe(t, h, bq, dataset, body)
	return body
}

// checkBigQueryProbe checks what the env probe reported: every request
// BigQuery refuses was refused, and none of them is stored.
func checkBigQueryProbe(t *testing.T, h *Harness, bq *bigquery.Client, dataset, body string) {
	t.Helper()
	ctx := h.Context()
	if !strings.Contains(body, "BIGQUERY PROBE: OK") {
		t.Fatalf("the probe could not use BigQuery: %s", body)
	}
	for _, want := range []string{"create=ok", "duplicate=409", "invalid_id=400", "duplicate_column=400", "missing_required=invalid"} {
		if !strings.Contains(body, want+" ") && !strings.HasSuffix(strings.TrimSpace(body), want) {
			t.Errorf("the probe's BigQuery did not answer %s: %s", want, body)
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

// kubectlGetIn is kubectlGet in another namespace.
func kubectlGetIn(t *testing.T, namespace, kind, name string) (string, error) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	raw, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", namespace, "get", kind, name, "-o", "yaml").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl get %s %s: %w\n%s", kind, name, err, raw)
	}
	return string(raw), nil
}
