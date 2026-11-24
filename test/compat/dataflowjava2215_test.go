//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// beamJavaBuildImage builds and runs the Java test pipelines
// (testdata/dataflowjava): Maven on Eclipse Temurin 17, the JDK the
// Apache Beam 2.78.0 Java 17 SDK harness runs.
const beamJavaBuildImage = "maven:3.9.9-eclipse-temurin-17@sha256:" + mavenTemurin17Digest

// mavenTemurin17Digest pins maven:3.9.9-eclipse-temurin-17.
const mavenTemurin17Digest = "f58d59b6273e785ac0a4477f6e9b5ba1d7731c75b906c0f7b34076f1851318cc"

// buildBeamJava resolves testdata/dataflowjava's dependencies (Apache Beam
// 2.78.0's Java SDK, its Dataflow runner and Google Cloud I/O, from Maven
// Central) and compiles it, on the default network, into a directory it
// answers: classes/ and deps/.
func buildBeamJava(t *testing.T, ctx context.Context) string {
	t.Helper()
	work := t.TempDir()
	src := filepath.Join(moduleRoot(t), "testdata", "dataflowjava")
	if err := os.CopyFS(work, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "m2"), 0o777); err != nil {
		t.Fatal(err)
	}
	script := "mvn -q -B -Dmaven.repo.local=/w/m2 dependency:copy-dependencies -DoutputDirectory=/w/deps -DincludeScope=runtime" +
		" && mkdir -p /w/classes && javac -nowarn -d /w/classes -cp '/w/deps/*' $(find /w/src -name '*.java')" +
		" && rm -rf /w/m2 && chmod -R a+rwX /w"
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", work+":/w", "-w", "/w", "--entrypoint", "sh",
		beamJavaBuildImage, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("build testdata/dataflowjava: %v\n%s", err, out)
	}
	return work
}

// runBeamJava runs main of testdata/dataflowjava with args on network
// (internal/beamrun's relay to the instance), with a fixed access token of
// the instance (TokenCredentialFactory), as no metadata server is
// reachable.
func runBeamJava(t *testing.T, ctx context.Context, network, work, main string, args ...string) string {
	t.Helper()
	run := append([]string{"run", "--rm", "--network", network, "-v", work + ":/w:ro", "--entrypoint", "java", beamJavaBuildImage,
		"-cp", "/w/classes:/w/deps/*", main}, args...)
	out, err := exec.CommandContext(ctx, "docker", run...).CombinedOutput()
	if err != nil {
		t.Fatalf("java %s: %v\n%s", main, err, out)
	}
	return string(out)
}

// javaDataflowArgs are the Java SDK's DataflowRunner's options for the
// instance: its Dataflow and Cloud Storage endpoints (apiRootUrl,
// gcsEndpoint) through the relay.
func javaDataflowArgs(project, bucket, name string) []string {
	return []string{"--runner=DataflowRunner", "--project=" + project, "--region=us-central1",
		"--tempLocation=gs://" + bucket + "/tmp", "--gcpTempLocation=gs://" + bucket + "/tmp",
		"--stagingLocation=gs://" + bucket + "/staging", "--jobName=" + name, "--maxNumWorkers=2",
		"--apiRootUrl=http://relay.cloudburrow.localhost/", "--gcsEndpoint=http://relay.cloudburrow.localhost:9000/storage/v1/",
		"--credentialFactoryClass=dev.cloudburrow.dataflow.TokenCredentialFactory"}
}

func javaJobID(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "Submitted job: "); i >= 0 {
			return strings.TrimSpace(l[i+len("Submitted job: "):])
		}
	}
	t.Fatalf("the Java SDK printed no job ID:\n%s", out)
	return ""
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob, google.dataflow.v1beta3.JobsV1Beta3/GetJob
//
// TestJavaDataflowWordCount (#2215): a word count written with Apache
// Beam's Java SDK, submitted with its DataflowRunner against the instance
// as against Google (docs: the Java quickstart): the SDK stages its jars
// and pipeline in the instance's Cloud Storage and creates the job; the
// job runs on the local Flink runner in the Beam Java 17 SDK harness,
// reading and writing Cloud Storage. The counts are right, the job is
// DONE with the Java SDK's version, and its graph has the pipeline's
// transforms.
func TestJavaDataflowWordCount(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-java-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	w := sc.Bucket(bucket).Object("in/words.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "the quick brown fox\njumps over the lazy dog\nthe dog sleeps\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	work := buildBeamJava(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr})
	name := fmt.Sprintf("java-wordcount-%d", time.Now().UnixNano()%1e6)
	out := runBeamJava(t, ctx, network, work, "dev.cloudburrow.dataflow.WordCount", append([]string{
		"--input=gs://" + bucket + "/in/words.txt", "--output=gs://" + bucket + "/out/counts.txt"},
		javaDataflowArgs(project, bucket, name)...)...)
	id := javaJobID(t, out)
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}

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
	if j.Type != dataflowpb.JobType_JOB_TYPE_BATCH || j.Name != name || j.GetJobMetadata().GetSdkVersion().GetVersion() != "2.78.0" ||
		!strings.Contains(j.GetJobMetadata().GetSdkVersion().GetVersionDisplayName(), "Java") {
		t.Errorf("job = %s %s sdk %v", j.Type, j.Name, j.GetJobMetadata().GetSdkVersion())
	}
	var names []string
	for _, tr := range j.GetPipelineDescription().GetOriginalPipelineTransform() {
		names = append(names, tr.Name)
	}
	if s := strings.Join(names, ","); !strings.Contains(s, "Read") || !strings.Contains(s, "Count") || !strings.Contains(s, "Write") {
		t.Errorf("the graph's transforms are %s; want the pipeline's Read, Count and Write", s)
	}
}
