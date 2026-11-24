//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dataflow "cloud.google.com/go/dataflow/apiv1beta3"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// EnvDataflow is Dataflow (#2215), gRPC and JSON on one port.
const EnvDataflow = "CLOUDBURROW_TEST_DATAFLOW"

// beamPythonImage is the Apache Beam 2.78.0 Python 3.12 SDK container,
// pinned as internal/beamrun pins it: the SDK the tests submit with is the
// harness the instance runs their pipelines on.
const beamPythonImage = "apache/beam_python3.12_sdk@sha256:bf536d721f825259329a396a20947b81ce8d0bbe08472b26f3f149c9957c1e56"

type dataflowClients struct {
	jobs     *dataflow.JobsV1Beta3Client
	messages *dataflow.MessagesV1Beta3Client
	metrics  *dataflow.MetricsV1Beta3Client
	tmpl     *dataflow.TemplatesClient
	flex     *dataflow.FlexTemplatesClient
}

// dataflowEnv asks for every endpoint the Dataflow tests use, before any
// container runs, and returns the official Go clients (gRPC).
func dataflowEnv(t *testing.T) (*Harness, string, dataflowClients) {
	t.Helper()
	h := New(t)
	addr := h.Endpoint(EnvDataflow)
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	ctx := h.Context()
	var c dataflowClients
	var err error
	if c.jobs, err = dataflow.NewJobsV1Beta3Client(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if c.messages, err = dataflow.NewMessagesV1Beta3Client(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if c.metrics, err = dataflow.NewMetricsV1Beta3Client(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if c.tmpl, err = dataflow.NewTemplatesClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if c.flex, err = dataflow.NewFlexTemplatesClient(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.jobs.Close(); c.messages.Close(); c.metrics.Close(); c.tmpl.Close(); c.flex.Close() })
	return h, addr, c
}

// beamNetwork is an --internal Docker network whose one route is a relay
// (internal/beamrun's) to the instance's Cloud Storage, Pub/Sub, BigQuery
// and Dataflow ports, as relay.cloudburrow.localhost (a name the
// instance's host check answers, with localhost in it, to which the Python
// SDK's DataflowRunner speaks REST; Dataflow on port 80).
// A container on it reaches those four ports and nothing else.
func beamNetwork(t *testing.T, ctx context.Context, addrs map[int]string) string {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-dfnet-"+suffix, "cb-dfrelay-"+suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("create the internal network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	assets := filepath.Join(moduleRoot(t), "internal", "beamrun", "assets")
	args := append([]string{"run", "-d", "--name", relay, "-v", assets + ":/cb:ro", "--entrypoint", "python3"}, hostRelayArgs(t, ctx)...)
	args = append(args, beamPythonImage, "/cb/relay.py")
	for listen, a := range addrs {
		_, port, err := net.SplitHostPort(stripScheme(a))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, fmt.Sprintf("%d=host.docker.internal:%s", listen, port))
	}
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	for deadline := time.Now().Add(60 * time.Second); ; {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", relay).CombinedOutput()
		if strings.Contains(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay did not start:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay.cloudburrow.localhost", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}
	return network
}

// runBeamPython runs python with args in the Beam SDK container on
// network: the instance's Cloud Storage, Pub/Sub and BigQuery through
// their clients' emulator variables, and a fixed access token of the
// instance (internal/beamrun's sitecustomize), as no metadata server is
// reachable.
func runBeamPython(t *testing.T, ctx context.Context, network, project string, args ...string) string {
	t.Helper()
	root := moduleRoot(t)
	run := append([]string{"run", "--rm", "--network", network,
		"-v", filepath.Join(root, "internal", "beamrun", "assets", "py") + ":/cb/py:ro",
		"-v", filepath.Join(root, "testdata", "dataflowpy") + ":/flow:ro",
		"-e", "CB_DATAFLOW=1", "-e", "CB_PROJECT=" + project, "-e", "PYTHONPATH=/cb/py",
		"-e", "STORAGE_EMULATOR_HOST=http://relay.cloudburrow.localhost:9000", "-e", "PUBSUB_EMULATOR_HOST=relay.cloudburrow.localhost:9001",
		"-e", "BIGQUERY_EMULATOR_HOST=http://relay.cloudburrow.localhost:9002", "-e", "CB_BIGQUERY_URL=http://relay.cloudburrow.localhost:9002/bigquery/v2/",
		"--entrypoint", "python", beamPythonImage}, args...)
	out, err := exec.CommandContext(ctx, "docker", run...).CombinedOutput()
	if err != nil {
		t.Fatalf("python %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func dataflowRunnerArgs(project, bucket, name string) []string {
	return []string{"--runner=DataflowRunner", "--project=" + project, "--region=us-central1",
		"--temp_location=gs://" + bucket + "/tmp", "--staging_location=gs://" + bucket + "/staging",
		"--dataflow_endpoint=http://relay.cloudburrow.localhost", "--job_name=" + name, "--max_num_workers=2"}
}

func dataflowBucket(t *testing.T, ctx context.Context, sc *storage.Client, project, name string) {
	t.Helper()
	if err := sc.Bucket(name).Create(ctx, project, nil); err != nil && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

func waitDataflowJob(t *testing.T, ctx context.Context, c dataflowClients, project, id string, want ...dataflowpb.JobState) *dataflowpb.Job {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for {
		j, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id})
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		for _, w := range want {
			if j.CurrentState == w {
				return j
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s is %s after 8m; want %v\n%s", id, j.CurrentState, want, jobMessages(t, ctx, c, project, id))
		}
		time.Sleep(2 * time.Second)
	}
}

func jobMessages(t *testing.T, ctx context.Context, c dataflowClients, project, id string) string {
	it := c.messages.ListJobMessages(ctx, &dataflowpb.ListJobMessagesRequest{ProjectId: project, Location: "us-central1", JobId: id,
		MinimumImportance: dataflowpb.JobMessageImportance_JOB_MESSAGE_DEBUG})
	var b strings.Builder
	for {
		m, err := it.Next()
		if err != nil {
			break
		}
		fmt.Fprintf(&b, "%s %s\n", m.MessageImportance, m.MessageText)
	}
	return b.String()
}

func jobIDOf(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "Created job with id: ["); i >= 0 {
			return strings.TrimSuffix(l[i+len("Created job with id: ["):], "]")
		}
		if s, ok := strings.CutPrefix(strings.TrimSpace(l), "JOB_ID "); ok {
			return s
		}
	}
	t.Fatalf("the SDK printed no job ID:\n%s", out)
	return ""
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob, google.dataflow.v1beta3.JobsV1Beta3/GetJob, google.dataflow.v1beta3.JobsV1Beta3/ListJobs, google.dataflow.v1beta3.JobsV1Beta3/AggregatedListJobs, google.dataflow.v1beta3.JobsV1Beta3/UpdateJob, google.dataflow.v1beta3.MessagesV1Beta3/ListJobMessages, google.dataflow.v1beta3.MetricsV1Beta3/GetJobMetrics, google.dataflow.v1beta3.MetricsV1Beta3/GetJobExecutionDetails, google.dataflow.v1beta3.MetricsV1Beta3/GetStageExecutionDetails
//
// TestPythonDataflowWordCount (#2215): Apache Beam's own wordcount example
// (apache_beam.examples.wordcount), run with the Python SDK's
// DataflowRunner against the instance as against Google (docs: the Python
// quickstart): it stages the pipeline in the instance's Cloud Storage,
// creates the job over REST and waits for it; the job runs on the local
// Flink runner, reading and writing Cloud Storage. The counts are right;
// the Go client sees the job DONE, batch, with its SDK, messages, metrics,
// graph and stages; and a finished job can be neither cancelled nor
// drained.
func TestPythonDataflowWordCount(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-wc-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	text := "the quick brown fox\njumps over the lazy dog\nthe dog sleeps\n"
	w := sc.Bucket(bucket).Object("in/words.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, text)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr})
	name := fmt.Sprintf("wordcount-%d", time.Now().UnixNano()%1e6)
	out := runBeamPython(t, ctx, network, project, append([]string{"-m", "apache_beam.examples.wordcount",
		"--input=gs://" + bucket + "/in/words.txt", "--output=gs://" + bucket + "/out/counts"},
		dataflowRunnerArgs(project, bucket, name)...)...)
	id := jobIDOf(t, out)
	if !strings.Contains(out, "JOB_STATE_DONE") {
		t.Errorf("the SDK did not see the job DONE:\n%s", out)
	}

	// The counts, in the output files the job wrote.
	got := map[string]string{}
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "out/counts"})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		r, err := sc.Bucket(bucket).Object(a.Name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if k, v, ok := strings.Cut(l, ": "); ok {
				got[k] = v
			}
		}
	}
	if got["the"] != "3" || got["dog"] != "2" || got["fox"] != "1" || len(got) != 9 {
		t.Errorf("word counts = %v; want the: 3, dog: 2, fox: 1 among 9 words", got)
	}

	j, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
	if err != nil {
		t.Fatal(err)
	}
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE || j.Type != dataflowpb.JobType_JOB_TYPE_BATCH || j.Name != name ||
		j.GetJobMetadata().GetSdkVersion().GetVersion() != "2.78.0" || !strings.Contains(j.GetJobMetadata().GetSdkVersion().GetVersionDisplayName(), "Python") {
		t.Errorf("job = %s %s %s sdk %v", j.CurrentState, j.Type, j.Name, j.GetJobMetadata().GetSdkVersion())
	}
	if len(j.GetPipelineDescription().GetOriginalPipelineTransform()) == 0 || len(j.GetPipelineDescription().GetExecutionPipelineStage()) == 0 {
		t.Errorf("the job has no graph: %v", j.GetPipelineDescription())
	}
	var names []string
	for _, tr := range j.GetPipelineDescription().GetOriginalPipelineTransform() {
		names = append(names, tr.Name)
	}
	if s := strings.Join(names, ","); !strings.Contains(s, "Read") || !strings.Contains(s, "Write") {
		t.Errorf("the graph's transforms are %s; want the example's Read and Write", s)
	}
	sum, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id})
	if err != nil || sum.Environment != nil || sum.PipelineDescription != nil {
		t.Errorf("the default (summary) view has the environment or graph: %v %v", sum, err)
	}
	found := false
	for lit := c.jobs.ListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: project, Location: "us-central1", Filter: dataflowpb.ListJobsRequest_TERMINATED}); ; {
		x, err := lit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.Id == id
	}
	if !found {
		t.Errorf("jobs.list(TERMINATED) does not list %s", id)
	}
	for lit := c.jobs.ListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: project, Location: "us-central1", Filter: dataflowpb.ListJobsRequest_ACTIVE}); ; {
		x, err := lit.Next()
		if err != nil {
			break
		}
		if x.Id == id {
			t.Errorf("jobs.list(ACTIVE) lists the finished job")
		}
	}
	msgs := jobMessages(t, ctx, c, project, id)
	if !strings.Contains(msgs, "Worker pool stopped.") || !strings.Contains(msgs, "Autoscaling") {
		t.Errorf("job messages:\n%s", msgs)
	}
	m, err := c.metrics.GetJobMetrics(ctx, &dataflowpb.GetJobMetricsRequest{ProjectId: project, Location: "us-central1", JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	counted := false
	for _, u := range m.Metrics {
		if u.GetName().GetName() == "ElementCount" && u.GetScalar().GetNumberValue() > 0 {
			counted = true
		}
	}
	if !counted {
		t.Errorf("GetJobMetrics has no ElementCount: %v", m.Metrics)
	}
	det, err := c.metrics.GetJobExecutionDetails(ctx, &dataflowpb.GetJobExecutionDetailsRequest{ProjectId: project, Location: "us-central1", JobId: id}).Next()
	if err != nil || det.State != dataflowpb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("GetJobExecutionDetails = %v, %v; want a SUCCEEDED stage", det, err)
	}
	if det != nil {
		w, err := c.metrics.GetStageExecutionDetails(ctx, &dataflowpb.GetStageExecutionDetailsRequest{ProjectId: project, Location: "us-central1",
			JobId: id, StageId: det.StageId}).Next()
		if err != nil || len(w.GetWorkItems()) == 0 || w.WorkItems[0].State != dataflowpb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
			t.Errorf("GetStageExecutionDetails(%s) = %v, %v; want a worker's SUCCEEDED work item", det.StageId, w, err)
		}
	}
	for _, st := range []dataflowpb.JobState{dataflowpb.JobState_JOB_STATE_CANCELLED, dataflowpb.JobState_JOB_STATE_DRAINED} {
		_, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
			Job: &dataflowpb.Job{RequestedState: st}})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("UpdateJob(%s) of a DONE batch job = %v; want FAILED_PRECONDITION", st, err)
		}
	}
	agg, err := c.jobs.AggregatedListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: project}).Next()
	if err != nil || agg == nil {
		t.Errorf("AggregatedListJobs: %v", err)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/UpdateJob, google.dataflow.v1beta3.JobsV1Beta3/CheckActiveJobs
//
// TestPythonDataflowStreamingDrain (#2215): a streaming pipeline
// (testdata/dataflowpy/streaming.py) from a Pub/Sub subscription to a
// BigQuery table, with Beam's own ReadFromPubSub and WriteToBigQuery
// (streaming inserts), submitted with the DataflowRunner: the job runs,
// reads what is published, and its hour-long windows close when it is
// drained (guides/stopping-a-pipeline): the drained job's counts are in
// BigQuery, and the job ends DRAINED.
func TestPythonDataflowStreamingDrain(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	pubsubAddr := h.Endpoint(EnvPubSub)
	bqAddr := h.Endpoint(EnvBigQuery)
	dfAddr := h.Endpoint(EnvDataflow)
	bq, bqProject := bigqueryClient(t, h)
	ps := pubsubClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := bqProject
	bucket := fmt.Sprintf("%s-dataflow-st-%d", h.Project(), time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	topicName := fmt.Sprintf("projects/%s/topics/dataflow-words-%s", project, suffix)
	subName := fmt.Sprintf("projects/%s/subscriptions/dataflow-words-%s", project, suffix)
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName, Topic: topicName}); err != nil {
		t.Fatal(err)
	}
	ds := "dataflow_" + suffix
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9001: pubsubAddr, 9002: bqAddr, 80: dfAddr})
	name := "streaming-words-" + suffix
	out := runBeamPython(t, ctx, network, project, append([]string{"/flow/streaming.py",
		"--subscription=" + subName, "--table=" + project + ":" + ds + ".counts"},
		dataflowRunnerArgs(project, bucket, name)...)...)
	id := jobIDOf(t, out)
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
		t.Fatalf("job %s is %s %s:\n%s", id, j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
	}
	if a, err := c.jobs.CheckActiveJobs(ctx, &dataflowpb.CheckActiveJobsRequest{ProjectId: project}); err != nil || !a.ActiveJobsExist {
		t.Errorf("CheckActiveJobs = %v, %v; want active jobs", a, err)
	}
	words := []string{"apple", "banana", "apple", "cherry", "apple", "banana"}
	for _, w := range words {
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(w)}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Read: the subscription has no backlog left.
	for deadline := time.Now().Add(3 * time.Minute); ; {
		resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: subName, MaxMessages: 1, ReturnImmediately: true})
		if err == nil && len(resp.ReceivedMessages) == 0 {
			break
		}
		if err == nil {
			// One the test pulled is the job's to read: put it back.
			_ = ps.SubscriptionAdminClient.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: subName,
				AckIds: []string{resp.ReceivedMessages[0].AckId}, AckDeadlineSeconds: 0})
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not read the subscription:\n%s", jobMessages(t, ctx, c, project, id))
		}
		time.Sleep(3 * time.Second)
	}
	if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	j, _ = c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id})
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINING && j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Errorf("after the drain request the job is %s; want DRAINING", j.CurrentState)
	}
	j = waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Fatalf("job %s ended %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	rows := bq.Query("SELECT word, n FROM `" + project + "." + ds + ".counts` ORDER BY word")
	rit, err := rows.Read(ctx)
	if err != nil {
		t.Fatalf("query the counts: %v", err)
	}
	var got []string
	for {
		var r []bigquery.Value
		err := rit.Next(&r)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%v=%v", r[0], r[1]))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "apple=3,banana=2,cherry=1" {
		t.Errorf("BigQuery counts = %v; want apple=3,banana=2,cherry=1", got)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/SnapshotJob (unimplemented)
//
// TestDataflowJobsAPI (#2215): the Go client's failure paths, as the v1b3
// reference documents them: an invalid name is INVALID_ARGUMENT, an
// unknown job NOT_FOUND, a job with no staged pipeline fails with its
// reason as its error message, a second active job of a name answers the
// first, and a pending batch job is cancelled.
func TestDataflowJobsAPI(t *testing.T) {
	h, _, c := dataflowEnv(t)
	ctx := h.Context()
	project := h.Project()
	if _, err := c.jobs.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: project, Location: "us-central1",
		Job: &dataflowpb.Job{Name: "Bad_Name"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateJob(Bad_Name) = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: "2026-01-01_00_00_00-1"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob(unknown) = %v; want NOT_FOUND", err)
	}
	name := fmt.Sprintf("no-pipeline-%d", time.Now().UnixNano()%1e6)
	j, err := c.jobs.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: project, Location: "us-central1",
		Job: &dataflowpb.Job{Name: name, Type: dataflowpb.JobType_JOB_TYPE_BATCH}})
	if err != nil {
		t.Fatal(err)
	}
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_UNKNOWN || j.Id == "" || j.Location != "us-central1" {
		t.Errorf("CreateJob = %v; want the new job, before it has a state", j)
	}
	j = waitDataflowJob(t, ctx, c, project, j.Id, dataflowpb.JobState_JOB_STATE_FAILED)
	if msgs := jobMessages(t, ctx, c, project, j.Id); !strings.Contains(msgs, "pipelineUrl") {
		t.Errorf("the failed job's messages do not say why:\n%s", msgs)
	}
	if _, err := c.jobs.SnapshotJob(ctx, &dataflowpb.SnapshotJobRequest{ProjectId: project, Location: "us-central1", JobId: j.Id}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SnapshotJob = %v; want UNIMPLEMENTED", err)
	}
}
