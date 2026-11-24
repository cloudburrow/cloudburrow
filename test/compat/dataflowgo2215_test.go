//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
)

// buildBeamGo builds testdata/dataflowgo (Apache Beam 2.78.0's Go SDK,
// from the Go module proxy) for Linux on Docker's architecture, statically,
// into a directory it answers, as prog: the pipeline's submitter and, as
// the Go SDK's Dataflow runner stages it (--worker_binary), its worker.
func buildBeamGo(t *testing.T, ctx context.Context) string {
	t.Helper()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	work, src := t.TempDir(), t.TempDir()
	// Its go.sum is made here, from the module proxy and the checksum
	// database (the Beam SDK's module graph names modules the repository
	// keeps no sums of).
	if err := os.CopyFS(src, os.DirFS(filepath.Join(moduleRoot(t), "testdata", "dataflowgo"))); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=mod", "-trimpath", "-o", filepath.Join(work, "prog"), ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build testdata/dataflowgo: %v\n%s", err, out)
	}
	return work
}

// runBeamGo runs testdata/dataflowgo's prog with args on network
// (internal/beamrun's relay to the instance), with the SDK's Dataflow
// runner's options for the instance: its Dataflow endpoint
// (--dataflow_endpoint), Cloud Storage through STORAGE_EMULATOR_HOST, and
// the instance's metadata server for the runner's default credentials
// (GCE_METADATA_HOST, relayed on port 9006), as no other is reachable.
func runBeamGo(t *testing.T, ctx context.Context, network, work, project, bucket, name string, args ...string) string {
	t.Helper()
	run := []string{"run", "--rm", "--network", network, "-v", work + ":/w:ro",
		"-e", "STORAGE_EMULATOR_HOST=http://relay.cloudburrow.localhost:9000",
		"-e", "GCE_METADATA_HOST=relay.cloudburrow.localhost:9006",
		"--entrypoint", "/w/prog", beamPythonImage,
		"--runner=dataflow", "--project=" + project, "--region=us-central1",
		"--staging_location=gs://" + bucket + "/staging", "--temp_location=gs://" + bucket + "/tmp",
		"--dataflow_endpoint=http://relay.cloudburrow.localhost/", "--worker_binary=/w/prog",
		"--job_name=" + name, "--max_num_workers=2", "--execute_async"}
	out, err := exec.CommandContext(ctx, "docker", append(run, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("dataflowgo %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// goJobID is the job ID the Go SDK's Dataflow runner printed.
func goJobID(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "Submitted job: "); i >= 0 {
			// The SDK logs through slog: msg="Submitted job: ID".
			if f := strings.Fields(l[i+len("Submitted job: "):]); len(f) > 0 {
				return strings.Trim(f[0], `"`)
			}
		}
	}
	t.Fatalf("the Go SDK printed no job ID:\n%s", out)
	return ""
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob, google.dataflow.v1beta3.JobsV1Beta3/GetJob, google.dataflow.v1beta3.MetricsV1Beta3/GetJobMetrics
//
// TestGoDataflowWordCount (#2215): a word count written with Apache Beam's
// Go SDK (textio, stats.Count), submitted with its Dataflow runner against
// the instance as against Google (docs: the Go quickstart): the SDK stages
// its worker binary and pipeline in the instance's Cloud Storage and
// creates the job; the job runs on the local Flink runner in the Go SDK
// harness, reading and writing Cloud Storage. The counts are right, the
// job is DONE with the Go SDK's version, and its metrics are its Go
// workers' (element counts and its counter of lines).
func TestGoDataflowWordCount(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	mdAddr := h.Endpoint(EnvMetadata)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-go-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	w := sc.Bucket(bucket).Object("in/words.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "the quick brown fox\njumps over the lazy dog\nthe dog sleeps\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	work := buildBeamGo(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr, 9006: mdAddr})
	name := fmt.Sprintf("go-wordcount-%d", time.Now().UnixNano()%1e6)
	id := goJobID(t, runBeamGo(t, ctx, network, work, project, bucket, name, "--pipeline=wordcount",
		"--input=gs://"+bucket+"/in/words.txt", "--output=gs://"+bucket+"/out/counts.txt"))
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	r, err := sc.Bucket(bucket).Object("out/counts.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			got[k] = v
		}
	}
	if got["the"] != "3" || got["dog"] != "2" || got["fox"] != "1" || len(got) != 9 {
		t.Errorf("word counts = %v; want the: 3, dog: 2, fox: 1 among 9 words", got)
	}
	j, err = c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
	if err != nil {
		t.Fatal(err)
	}
	if j.Type != dataflowpb.JobType_JOB_TYPE_BATCH || j.Name != name || j.GetJobMetadata().GetSdkVersion().GetVersion() != "2.78.0" ||
		!strings.Contains(j.GetJobMetadata().GetSdkVersion().GetVersionDisplayName(), "Go") {
		t.Errorf("job = %s %s sdk %v", j.Type, j.Name, j.GetJobMetadata().GetSdkVersion())
	}
	m, err := c.metrics.GetJobMetrics(ctx, &dataflowpb.GetJobMetricsRequest{ProjectId: project, Location: "us-central1", JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	counted, lines := false, -1.0
	for _, u := range m.Metrics {
		switch {
		case u.GetName().GetName() == "ElementCount" && u.GetName().GetOrigin() == "dataflow/v1b3" && u.GetScalar().GetNumberValue() > 0:
			counted = true
		case u.GetName().GetName() == "lines" && u.GetName().GetOrigin() == "user":
			lines = u.GetScalar().GetNumberValue()
		}
	}
	if !counted || lines != 3 {
		t.Errorf("GetJobMetrics = %v; want ElementCount and the user counter lines = 3", m.Metrics)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob, google.dataflow.v1beta3.JobsV1Beta3/UpdateJob
//
// TestGoDataflowPubsub (#2215): a streaming Go pipeline reads a
// subscription with the Go SDK's pubsubio.Read and publishes each
// message, upper-cased, with pubsubio.Write (transforms Dataflow's
// service runs itself), submitted with the Go SDK's Dataflow runner. The
// output topic gets the messages; drained, the job is DRAINED.
func TestGoDataflowPubsub(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	pubsubAddr := h.Endpoint(EnvPubSub)
	dfAddr := h.Endpoint(EnvDataflow)
	mdAddr := h.Endpoint(EnvMetadata)
	ps := pubsubClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-gps-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	work := buildBeamGo(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9001: pubsubAddr, 80: dfAddr, 9006: mdAddr})
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	in := fmt.Sprintf("projects/%s/topics/gps-in-%s", project, suffix)
	inSub := fmt.Sprintf("projects/%s/subscriptions/gps-in-%s", project, suffix)
	out := fmt.Sprintf("projects/%s/topics/gps-out-%s", project, suffix)
	outSub := fmt.Sprintf("projects/%s/subscriptions/gps-out-%s", project, suffix)
	for _, tp := range []string{in, out} {
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp}); err != nil {
			t.Fatal(err)
		}
	}
	for s, tp := range map[string]string{inSub: in, outSub: out} {
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: s, Topic: tp}); err != nil {
			t.Fatal(err)
		}
	}
	name := "go-pubsub-" + suffix
	id := goJobID(t, runBeamGo(t, ctx, network, work, project, bucket, name, "--pipeline=pubsub", "--pubsub_project="+project,
		"--subscription=gps-in-"+suffix, "--topic=gps-out-"+suffix))
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
		t.Errorf("job %s is %s; want JOB_TYPE_STREAMING", id, j.Type)
	}
	for _, w := range []string{"apple", "banana"} {
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: in, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(w)}}}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for deadline := time.Now().Add(8 * time.Minute); len(got) < 2; {
		resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: outSub, MaxMessages: 10, ReturnImmediately: true})
		if err != nil {
			t.Fatal(err)
		}
		var acks []string
		for _, rm := range resp.ReceivedMessages {
			got = append(got, string(rm.Message.Data))
			acks = append(acks, rm.AckId)
		}
		if len(acks) > 0 {
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: outSub, AckIds: acks})
		}
		if len(got) < 2 && time.Now().After(deadline) {
			t.Fatalf("the job published %v:\n%s", got, jobMessages(t, ctx, c, project, id))
		}
		time.Sleep(2 * time.Second)
	}
	sort.Strings(got)
	if s := strings.Join(got, ","); s != "APPLE,BANANA" {
		t.Errorf("the job published %s; want APPLE,BANANA", s)
	}
	if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	j = waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Fatalf("job %s ended %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob
//
// TestGoDataflowBigQuery (#2215): Go pipelines write a BigQuery table and
// read it back with the Go SDK's cross-language bigqueryio (xlang/
// bigqueryio, which Beam's Java expansion service for Google Cloud I/O
// expands into Java's BigQueryIO: the Go SDK's native bigqueryio reaches
// only Google's endpoint), submitted with the Go SDK's Dataflow runner.
// The jobs run BigQueryIO in the Java SDK's harness against the
// instance's BigQuery and the rest in Go's: the table has the rows, and
// the read's output is them.
func TestGoDataflowBigQuery(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	bqAddr := h.Endpoint(EnvBigQuery)
	dfAddr := h.Endpoint(EnvDataflow)
	mdAddr := h.Endpoint(EnvMetadata)
	bq, project := bigqueryClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	bucket := fmt.Sprintf("%s-dataflow-gbq-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	ds := fmt.Sprintf("gobq_%d", time.Now().UnixNano()%1e6)
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(context.Background()) })
	work := buildBeamGo(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9002: bqAddr, 80: dfAddr, 9006: mdAddr})
	netns, expansion := beamExpansionService(t, ctx, network)
	table := project + ":" + ds + ".scores"

	name := fmt.Sprintf("go-bq-write-%d", time.Now().UnixNano()%1e6)
	id := goJobID(t, runBeamGo(t, ctx, netns, work, project, bucket, name, "--pipeline=bigquery",
		"--table="+table, "--expansion_addr="+expansion))
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("write job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	var rows []string
	it := bq.Query("SELECT name, score FROM `" + project + "." + ds + ".scores` ORDER BY name").Read
	ri, err := it(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for {
		var r []bigquery.Value
		if err := ri.Next(&r); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, fmt.Sprintf("%v=%v", r[0], r[1]))
	}
	if s := strings.Join(rows, ","); s != "ada=10,grace=20,linus=30" {
		t.Errorf("the table has %s; want ada=10,grace=20,linus=30", s)
	}

	name = fmt.Sprintf("go-bq-read-%d", time.Now().UnixNano()%1e6)
	id = goJobID(t, runBeamGo(t, ctx, netns, work, project, bucket, name, "--pipeline=bigquery-read",
		"--table="+table, "--expansion_addr="+expansion, "--output=gs://"+bucket+"/out/scores.txt"))
	j = waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("read job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	r, err := sc.Bucket(bucket).Object("out/scores.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	sort.Strings(got)
	if s := strings.Join(got, ","); s != "ada=10,grace=20,linus=30" {
		t.Errorf("the read wrote %s; want ada=10,grace=20,linus=30", s)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob
//
// TestGoDataflowBigQueryRead (#2215): a Go pipeline reads a BigQuery
// table the BigQuery client made, with the Go SDK's cross-language
// bigqueryio.Read (Java's BigQueryIO, expanded by Beam's Java expansion
// service for Google Cloud I/O), submitted with the Go SDK's Dataflow
// runner; the read's output is the table's rows.
func TestGoDataflowBigQueryRead(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	bqAddr := h.Endpoint(EnvBigQuery)
	dfAddr := h.Endpoint(EnvDataflow)
	mdAddr := h.Endpoint(EnvMetadata)
	bq, project := bigqueryClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	bucket := fmt.Sprintf("%s-dataflow-gbr-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	ds := fmt.Sprintf("gobqr_%d", time.Now().UnixNano()%1e6)
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(context.Background()) })
	q := bq.Query("CREATE TABLE `" + project + "." + ds + ".scores` AS SELECT * FROM UNNEST([STRUCT('ada' AS name, 10 AS score), ('grace', 20), ('linus', 30)])")
	if job, err := q.Run(ctx); err != nil {
		t.Fatal(err)
	} else if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("create the table: %v %v", err, st.Err())
	}
	work := buildBeamGo(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9002: bqAddr, 80: dfAddr, 9006: mdAddr})
	netns, expansion := beamExpansionService(t, ctx, network)
	name := fmt.Sprintf("go-bq-read-%d", time.Now().UnixNano()%1e6)
	id := goJobID(t, runBeamGo(t, ctx, netns, work, project, bucket, name, "--pipeline=bigquery-read",
		"--table="+project+":"+ds+".scores", "--expansion_addr="+expansion, "--output=gs://"+bucket+"/out/scores.txt"))
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("read job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	r, err := sc.Bucket(bucket).Object("out/scores.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	sort.Strings(got)
	if s := strings.Join(got, ","); s != "ada=10,grace=20,linus=30" {
		t.Errorf("the read wrote %s; want ada=10,grace=20,linus=30", s)
	}
}
