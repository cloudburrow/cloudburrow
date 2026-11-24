//go:build compat

package compat

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/storage"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"google.golang.org/api/iterator"
)

// pushFlexImage pushes a Flex Template image to a Docker repository of
// the instance's Artifact Registry: base (read from the local Docker with
// `docker save`, so nothing is downloaded) with files (path in the image:
// content) and env added, as `gcloud dataflow flex-template build`'s
// Dockerfile would make it. It answers the image's Artifact Registry path.
func pushFlexImage(t *testing.T, h *Harness, ctx context.Context, base string, files map[string][]byte, env []string) string {
	t.Helper()
	_, port, _ := splitHostPort(h.Endpoint(EnvRun))
	project := h.Project()
	ar, actx := arClient(t, h)
	repo := fmt.Sprintf("flex-%d", time.Now().UnixNano()%1e6)
	formatRepo(t, actx, ar, runParent(h), repo, &arpb.Repository{Format: arpb.Repository_DOCKER})
	saved := filepath.Join(t.TempDir(), "base.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", saved, base).CombinedOutput(); err != nil {
		t.Fatalf("docker save %s: %v\n%s", base, err, out)
	}
	img, err := tarball.ImageFromPath(saved, nil)
	if err != nil {
		t.Fatalf("read the saved base image: %v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	dirs := map[string]bool{}
	for _, n := range names {
		for d := filepath.Dir(n); d != "." && d != "/" && !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	dl := make([]string, 0, len(dirs))
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	for _, d := range dl {
		if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(d, "/") + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(n, "/"), Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.AppendLayers(img, layer); err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.Config.Env = append(cf.Config.Env, env...)
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+repo+"/template:1", registryOptions(h)...); err != nil {
		t.Fatalf("push the Flex Template image: %v", err)
	}
	return "us-central1-docker.pkg.dev/" + project + "/" + repo + "/template:1"
}

// pyWheelOf is a minimal pure-Python wheel of the module pkg, whose
// __init__.py is init.
func pyWheelOf(t *testing.T, pkg, version, init string) (string, []byte) {
	t.Helper()
	dist := pkg + "-" + version + ".dist-info"
	files := []struct{ name, body string }{
		{pkg + "/__init__.py", init},
		{dist + "/METADATA", "Metadata-Version: 2.1\nName: " + pkg + "\nVersion: " + version + "\n"},
		{dist + "/WHEEL", "Wheel-Version: 1.0\nGenerator: cloudburrow-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var record strings.Builder
	for _, f := range files {
		w, _ := zw.Create(f.name)
		_, _ = io.WriteString(w, f.body)
		s := sha256.Sum256([]byte(f.body))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", f.name, base64.RawURLEncoding.EncodeToString(s[:]), len(f.body))
	}
	record.WriteString(dist + "/RECORD,,\n")
	w, _ := zw.Create(dist + "/RECORD")
	_, _ = io.WriteString(w, record.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return pkg + "-" + version + "-py3-none-any.whl", buf.Bytes()
}

// readOutputs is the content of bucket's objects under prefix, joined.
func readOutputs(t *testing.T, ctx context.Context, sc *storage.Client, bucket, prefix string) string {
	t.Helper()
	var got strings.Builder
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
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
		got.Write(b)
	}
	return got.String()
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestPythonDataflowFlexTemplatePackages (#2215): a Python Flex Template
// whose image names FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE, _SETUP_FILE and
// _EXTRA_PACKAGES (docs: Configure Flex Templates, environment variables).
// The launcher has no route to PyPI, so, as for a launcher without
// internet access, its image's PIP_INDEX_URL names an Artifact Registry
// Python repository of the instance's, which holds the requirement
// (cbgreet, uploaded with twine's protocol). The launcher installs it
// before the program runs (the program imports it at the top), and the
// job's workers install the requirement, the setup file's package and the
// extra wheel from what the SDK staged: each line comes out tagged by all
// three. The same template without an index fails, with pip's reason in
// the job's messages.
func TestPythonDataflowFlexTemplatePackages(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvRun)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	project := h.Project()
	ar, actx := arClient(t, h)
	pyRepo := fmt.Sprintf("flexpy-%d", time.Now().UnixNano()%1e6)
	formatRepo(t, actx, ar, runParent(h), pyRepo, &arpb.Repository{Format: arpb.Repository_PYTHON})
	file, wheel := pyWheelOf(t, "cbgreet", "0.1.0", "def greet(w):\n    return 'hi ' + w\n")
	if code, b := twineUpload(t, pkgBase(h, "python", project, pyRepo), "cbgreet", "0.1.0", file, wheel); code != http.StatusOK {
		t.Fatalf("twine upload = %d %s", code, b)
	}
	extraName, extra := pyWheelOf(t, "cbextra", "0.1.0", "def shout(w):\n    return w.upper() + '!'\n")

	src := filepath.Join(moduleRoot(t), "testdata", "dataflowpy", "flexpkg")
	files := map[string][]byte{"/template/extra/" + extraName: extra}
	for _, n := range []string{"main.py", "requirements.txt", "setup.py", "cbsetup/__init__.py"} {
		b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(n)))
		if err != nil {
			t.Fatal(err)
		}
		files["/template/"+n] = b
	}
	env := []string{"FLEX_TEMPLATE_PYTHON_PY_FILE=/template/main.py", "FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE=/template/requirements.txt",
		"FLEX_TEMPLATE_PYTHON_SETUP_FILE=/template/setup.py", "FLEX_TEMPLATE_PYTHON_EXTRA_PACKAGES=/template/extra/" + extraName}
	image := pushFlexImage(t, h, ctx, beamPythonImage, files,
		append(env, "PIP_INDEX_URL=https://us-central1-python.pkg.dev/"+project+"/"+pyRepo+"/simple/"))
	noIndex := pushFlexImage(t, h, ctx, beamPythonImage, files, env)

	bucket := fmt.Sprintf("%s-dataflow-flexpkg-%d", project, time.Now().UnixNano()%1e6)
	sc := storageClient(t, h)
	dataflowBucket(t, ctx, sc, project, bucket)
	w := sc.Bucket(bucket).Object("in.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "kiwi\nfig\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gcRun := gcloudDataflow(t, h, project)
	meta := filepath.Join(src, "metadata.json")
	for spec, img := range map[string]string{"pkg.json": image, "noindex.json": noIndex} {
		if out, err := gcRun("dataflow", "flex-template", "build", "gs://"+bucket+"/templates/"+spec, "--image="+img,
			"--sdk-language=PYTHON", "--metadata-file="+meta); err != nil {
			t.Fatalf("gcloud dataflow flex-template build: %v\n%s", err, out)
		}
	}
	name := fmt.Sprintf("flex-pkg-%d", time.Now().UnixNano()%1e6)
	out, err := gcRun("dataflow", "flex-template", "run", name, "--region=us-central1", "--template-file-gcs-location=gs://"+bucket+"/templates/pkg.json",
		"--parameters=input=gs://"+bucket+"/in.txt,output=gs://"+bucket+"/out/tagged", "--temp-location=gs://"+bucket+"/tmp",
		"--max-workers=2", "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow flex-template run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the Flex Template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readOutputs(t, ctx, sc, bucket, "out/tagged")
	for _, want := range []string{"hi kiwi KIWI! [cbsetup]", "hi fig FIG! [cbsetup]"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}

	// Without an index the requirement cannot be installed: no route.
	r, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
		LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: name + "-noindex",
			Parameters:  map[string]string{"input": "gs://" + bucket + "/in.txt", "output": "gs://" + bucket + "/out/none"},
			Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"},
			Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://" + bucket + "/templates/noindex.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := waitDataflowJob(t, ctx, c, project, r.GetJob().GetId(), dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_DONE)
	if msgs := jobMessages(t, ctx, c, project, f.Id); f.CurrentState != dataflowpb.JobState_JOB_STATE_FAILED || !strings.Contains(msgs, "cbgreet") {
		t.Errorf("a launch whose requirement has no index is %s:\n%s", f.CurrentState, msgs)
	}
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestJavaDataflowFlexTemplate (#2215): a Java Flex Template as Google's
// guide makes one: an image with the pipeline's jars and
// FLEX_TEMPLATE_JAVA_MAIN_CLASS and FLEX_TEMPLATE_JAVA_CLASSPATH naming
// them (here the Beam Java 17 SDK image, which has a JDK, with
// testdata/dataflowjava built into it), its spec written by `gcloud
// dataflow flex-template build --sdk-language=JAVA` and launched by
// `flex-template run`. The launcher runs the main class, whose
// DataflowRunner submits the launch's one job, which counts the words
// into Cloud Storage on the Java harness.
func TestJavaDataflowFlexTemplate(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvRun)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	project := h.Project()
	work := buildBeamJava(t, ctx)
	files := map[string][]byte{}
	for _, sub := range []string{"classes", "deps"} {
		root := filepath.Join(work, sub)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			b, err := os.ReadFile(p)
			files["/template/"+sub+"/"+filepath.ToSlash(rel)] = b
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	image := pushFlexImage(t, h, ctx, beamJava17Image, files, []string{
		"FLEX_TEMPLATE_JAVA_MAIN_CLASS=dev.cloudburrow.dataflow.WordCount",
		"FLEX_TEMPLATE_JAVA_CLASSPATH=/template/classes:/template/deps/*"})
	bucket := fmt.Sprintf("%s-dataflow-jflex-%d", project, time.Now().UnixNano()%1e6)
	sc := storageClient(t, h)
	dataflowBucket(t, ctx, sc, project, bucket)
	w := sc.Bucket(bucket).Object("in.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "kiwi apple\nkiwi fig\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gcRun := gcloudDataflow(t, h, project)
	spec := "gs://" + bucket + "/templates/java.json"
	if out, err := gcRun("dataflow", "flex-template", "build", spec, "--image="+image, "--sdk-language=JAVA",
		"--metadata-file="+filepath.Join(moduleRoot(t), "testdata", "dataflowjava", "flex-metadata.json")); err != nil {
		t.Fatalf("gcloud dataflow flex-template build: %v\n%s", err, out)
	}
	name := fmt.Sprintf("jflex-words-%d", time.Now().UnixNano()%1e6)
	out, err := gcRun("dataflow", "flex-template", "run", name, "--region=us-central1", "--template-file-gcs-location="+spec,
		"--parameters=input=gs://"+bucket+"/in.txt,output=gs://"+bucket+"/out/counts", "--temp-location=gs://"+bucket+"/tmp",
		"--max-workers=2", "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow flex-template run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE || j.Name != name {
		t.Fatalf("the Java Flex Template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	full, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
	if err != nil || !strings.Contains(full.GetJobMetadata().GetSdkVersion().GetVersionDisplayName(), "Java") {
		t.Errorf("the job's SDK is %v (%v); want the Java SDK", full.GetJobMetadata().GetSdkVersion(), err)
	}
	got := readOutputs(t, ctx, sc, bucket, "out/counts")
	for _, want := range []string{"kiwi: 2", "apple: 1", "fig: 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the counts lack %q:\n%s", want, got)
		}
	}
}
