//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"google.golang.org/api/iterator"
)

// flexImage is a Python Flex Template's image as Google's guide builds
// one, pushed to the instance's Artifact Registry: the pinned Beam Python
// SDK image (read from the local Docker with `docker save`, so nothing is
// downloaded) with testdata/dataflowpy/flex/main.py and
// FLEX_TEMPLATE_PYTHON_PY_FILE naming it. It answers the image's
// Artifact Registry path and the bucket the spec is written to.
func flexImage(t *testing.T, h *Harness, ctx context.Context) (image, bucket string) {
	t.Helper()
	h.Endpoint(EnvStorage)
	_, port, _ := splitHostPort(h.Endpoint(EnvRun))
	project := h.Project()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repo := fmt.Sprintf("flex-%d", time.Now().UnixNano()%1e6)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	op, err := ar.CreateRepository(ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: parent, RepositoryId: repo,
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &artifactregistrypb.DeleteRepositoryRequest{Name: parent + "/repositories/" + repo}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	saved := filepath.Join(t.TempDir(), "sdk.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", saved, beamPythonImage).CombinedOutput(); err != nil {
		t.Fatalf("docker save %s: %v\n%s", beamPythonImage, err, out)
	}
	base, err := tarball.ImageFromPath(saved, nil)
	if err != nil {
		t.Fatalf("read the saved SDK image: %v", err)
	}
	main, err := os.ReadFile(filepath.Join(moduleRoot(t), "testdata", "dataflowpy", "flex", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hd := range []*tar.Header{{Name: "template/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "template/main.py", Mode: 0o644, Size: int64(len(main)), Typeflag: tar.TypeReg}} {
		if err := tw.WriteHeader(hd); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tw.Write(main); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.Config.Env = append(cf.Config.Env, "FLEX_TEMPLATE_PYTHON_PY_FILE=/template/main.py")
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+repo+"/words:1", registryOptions(h)...); err != nil {
		t.Fatalf("push the Flex Template image: %v", err)
	}
	bucket = fmt.Sprintf("%s-dataflow-flex-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, storageClient(t, h), project, bucket)
	return "us-central1-docker.pkg.dev/" + project + "/" + repo + "/words:1", bucket
}

// flexSpec writes the template's spec with `gcloud dataflow flex-template
// build`, which with --image only writes it to Cloud Storage.
func flexSpec(t *testing.T, gc func(args ...string) (string, error), image, bucket string) string {
	t.Helper()
	spec := "gs://" + bucket + "/templates/words.json"
	if out, err := gc("dataflow", "flex-template", "build", spec, "--image="+image, "--sdk-language=PYTHON",
		"--metadata-file="+filepath.Join(moduleRoot(t), "testdata", "dataflowpy", "flex", "metadata.json")); err != nil {
		t.Fatalf("gcloud dataflow flex-template build: %v\n%s", err, out)
	}
	return spec
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestPythonDataflowFlexTemplate (#2215): a Python Flex Template as
// Google's guide makes one. Its image (the Beam SDK's with the program and
// FLEX_TEMPLATE_PYTHON_PY_FILE) is in the instance's Artifact Registry;
// `gcloud dataflow flex-template build` writes its spec to Cloud Storage
// and `flex-template run` launches it: the launcher runs the program,
// whose DataflowRunner submits the job, the launch's one job, which counts
// the words into Cloud Storage. A launch without a required parameter,
// and an image the registry does not hold, are refused or fail with the
// reason.
func TestPythonDataflowFlexTemplate(t *testing.T) {
	h, _, c := dataflowEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	image, bucket := flexImage(t, h, ctx)
	project := h.Project()
	gcRun := gcloudDataflow(t, h, project)
	spec := flexSpec(t, gcRun, image, bucket)
	sc := storageClient(t, h)
	w := sc.Bucket(bucket).Object("in.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("kiwi apple\nkiwi fig\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := gcRun(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	name := fmt.Sprintf("flex-words-%d", time.Now().UnixNano()%1e6)
	out := gc("dataflow", "flex-template", "run", name, "--region=us-central1", "--template-file-gcs-location="+spec,
		"--parameters=input=gs://"+bucket+"/in.txt,output=gs://"+bucket+"/out/counts", "--temp-location=gs://"+bucket+"/tmp",
		"--max-workers=2", "--format=value(job.id)")
	id := strings.TrimSpace(out)
	if id == "" {
		t.Fatalf("flex-template run answered no job ID:\n%s", out)
	}
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE || j.Name != name {
		t.Fatalf("the Flex Template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	if msgs := jobMessages(t, ctx, c, project, id); !strings.Contains(msgs, "Flex Template launcher") {
		t.Errorf("the job's messages do not show its launch:\n%s", msgs)
	}
	var got strings.Builder
	it := sc.Bucket(bucket).Objects(ctx, nil)
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(a.Name, "out/counts") {
			r, err := sc.Bucket(bucket).Object(a.Name).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r)
			r.Close()
			got.Write(b)
		}
	}
	for _, want := range []string{"kiwi: 2", "apple: 1", "fig: 1"} {
		if !strings.Contains(got.String(), want) {
			t.Errorf("the counts lack %q:\n%s", want, got.String())
		}
	}

	if out, err := gcRun("dataflow", "flex-template", "run", name+"-x", "--region=us-central1", "--template-file-gcs-location=gs://"+bucket+"/templates/none.json"); err == nil {
		t.Errorf("a launch from a spec that does not exist was accepted:\n%s", out)
	}
	// An image the registry does not hold: the launch's job fails with why.
	gc("dataflow", "flex-template", "build", "gs://"+bucket+"/templates/missing.json", "--image="+strings.TrimSuffix(image, ":1")+":nope", "--sdk-language=PYTHON")
	r, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
		LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: name + "-missing",
			Template: &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://" + bucket + "/templates/missing.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := waitDataflowJob(t, ctx, c, project, r.GetJob().GetId(), dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_DONE)
	if msgs := jobMessages(t, ctx, c, project, f.Id); f.CurrentState != dataflowpb.JobState_JOB_STATE_FAILED || !strings.Contains(msgs, "cannot be read from Artifact Registry") {
		t.Errorf("a launch of a missing image is %s:\n%s", f.CurrentState, msgs)
	}
}

// TestTerraformDataflowFlexTemplateJob (#2215): Terraform's
// google_dataflow_flex_template_job through `cloudburrow terraform`
// launches the Flex Template's streaming mode, which upper-cases a
// subscription's messages into a topic; a plan after apply is clean, and
// destroy drains the job (on_delete = "drain").
func TestTerraformDataflowFlexTemplateJob(t *testing.T) {
	h, _, c := dataflowEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	h.Endpoint(EnvPubSub)
	image, bucket := flexImage(t, h, ctx)
	project := h.Project()
	spec := flexSpec(t, gcloudDataflow(t, h, project), image, bucket)
	ps := pubsubClient(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	in, out := fmt.Sprintf("projects/%s/topics/flex-in-%s", project, suffix), fmt.Sprintf("projects/%s/topics/flex-out-%s", project, suffix)
	inSub, outSub := fmt.Sprintf("projects/%s/subscriptions/flex-in-%s", project, suffix), fmt.Sprintf("projects/%s/subscriptions/flex-out-%s", project, suffix)
	for _, tp := range [][2]string{{in, inSub}, {out, outSub}} {
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp[0]}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: tp[1], Topic: tp[0]}); err != nil {
			t.Fatal(err)
		}
	}
	m := newTFModule(t)
	name := "tf-flex-" + suffix
	m.write(fmt.Sprintf(`
resource "google_dataflow_flex_template_job" "j" {
  project                 = %q
  region                  = "us-central1"
  name                    = %q
  container_spec_gcs_path = %q
  temp_location           = "gs://%s/tmp"
  max_workers             = 2
  on_delete               = "drain"
  parameters = {
    subscription = %q
    topic        = %q
  }
}
output "id" {
  value = google_dataflow_flex_template_job.j.job_id
}
`, project, name, spec, bucket, inSub, out))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	id := strings.TrimSpace(m.stdout("output", "-raw", "id"))
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("the job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: in, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("kiwi")}}}); err != nil {
		t.Fatal(err)
	}
	var got string
	for deadline := time.Now().Add(3 * time.Minute); got == "" && time.Now().Before(deadline); {
		r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: outSub, MaxMessages: 10})
		if err == nil {
			for _, rm := range r.ReceivedMessages {
				got += string(rm.Message.Data)
			}
		}
		if got == "" {
			time.Sleep(2 * time.Second)
		}
	}
	if got != "KIWI" {
		t.Errorf("the output topic received %q; want KIWI\n%s", got, jobMessages(t, ctx, c, project, id))
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_CANCELLED, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Errorf("after destroy the job is %s; want DRAINED", j.CurrentState)
	}
}
