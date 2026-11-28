//go:build compat

package compat

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
)

// providedPyIndex is the instance's Python repository the YAML template's
// launcher and workers read as their package index (beamrun's
// ProvidedPythonIndex), where a user seeds what a pipeline installs.
const providedPyIndex = "projects/dataflow-templates/locations/us/repositories/extra-python-packages"

// pyWheelDeps is a minimal pure-Python wheel of the module pkg (dist
// name dist), whose files are files (path: content) and whose METADATA
// requires requires (Requires-Dist).
func pyWheelDeps(t *testing.T, dist, version string, files map[string]string, requires ...string) (string, []byte) {
	t.Helper()
	norm := strings.ReplaceAll(dist, "-", "_")
	info := norm + "-" + version + ".dist-info"
	meta := "Metadata-Version: 2.1\nName: " + dist + "\nVersion: " + version + "\n"
	for _, r := range requires {
		meta += "Requires-Dist: " + r + "\n"
	}
	all := map[string]string{info + "/METADATA": meta,
		info + "/WHEEL": "Wheel-Version: 1.0\nGenerator: cloudburrow-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"}
	for k, v := range files {
		all[k] = v
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var record strings.Builder
	for _, name := range sortedKeys(all) {
		w, _ := zw.Create(name)
		_, _ = io.WriteString(w, all[name])
		s := sha256.Sum256([]byte(all[name]))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", name, base64.RawURLEncoding.EncodeToString(s[:]), len(all[name]))
	}
	record.WriteString(info + "/RECORD,,\n")
	w, _ := zw.Create(info + "/RECORD")
	_, _ = io.WriteString(w, record.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return norm + "-" + version + "-py3-none-any.whl", buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedProvidedIndex uploads wheels (file: content, of dist name and
// version) to the YAML template's Python index with twine's protocol, as
// a user seeds it (`twine upload --repository-url
// http://HOST:PORT/us-python.pkg.dev/dataflow-templates/extra-python-packages/`),
// making the repository first unless it is there, as `gcloud artifacts
// repositories create extra-python-packages --project=dataflow-templates
// --location=us --repository-format=python` would.
func seedProvidedIndex(t *testing.T, h *Harness, wheels ...[4]string) {
	t.Helper()
	ar, actx := arClient(t, h)
	op, err := ar.CreateRepository(actx, &arpb.CreateRepositoryRequest{Parent: "projects/dataflow-templates/locations/us",
		RepositoryId: "extra-python-packages", Repository: &arpb.Repository{Format: arpb.Repository_PYTHON}})
	if err == nil {
		if _, err := op.Wait(actx); err != nil {
			t.Fatalf("make %s: %v", providedPyIndex, err)
		}
	} else if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("make %s: %v", providedPyIndex, err)
	}
	base := "http://" + h.Endpoint(EnvRun) + "/us-python.pkg.dev/dataflow-templates/extra-python-packages"
	for _, w := range wheels {
		if code, b := twineUpload(t, base, w[0], w[1], w[2], []byte(w[3])); code != http.StatusOK && code != http.StatusConflict {
			t.Fatalf("twine upload %s = %d %s", w[2], code, b)
		}
	}
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestGcloudDataflowYAMLRunUserPackages (#2256): a Beam YAML pipeline run
// with `gcloud dataflow yaml run` (Google's Yaml_Template) whose Python
// mapping needs packages beyond the template's defaults: an extra package
// in Cloud Storage, given by
// --additional-pipeline-options=extra_packages=gs://... (Dataflow's
// pipeline options), which the SDK downloads on the launcher and stages,
// and which requires a package (cbcolour) that the workers' pip installs
// from the package index. On Google pip reads PyPI; the job's network
// reaches only the instance, so it reads the instance's Python index,
// seeded here with twine's protocol as a user seeds it. The job is DONE
// and each line is made by both packages. The same pipeline with an
// extra package whose requirement the index does not hold FAILS.
func TestGcloudDataflowYAMLRunUserPackages(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvRun)
	project := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-yamlpkg-%s", project, suffix)
	sc := storageClient(t, h)
	dataflowBucket(t, ctx, sc, project, bucket)
	gcRun := gcloudDataflow(t, h, project)

	// A version unique to the run, so that the index holds it from this
	// run only.
	ver := "1.0." + suffix
	colour, colourWheel := pyWheelDeps(t, "cbcolour", ver, map[string]string{"cbcolour/__init__.py": "def paint(w):\n    return 'green ' + w\n"})
	seedProvidedIndex(t, h, [4]string{"cbcolour", ver, colour, string(colourWheel)})
	put := bucketWriter(t, ctx, sc, bucket)
	extra, extraWheel := pyWheelDeps(t, "cbextra", ver, map[string]string{"cbextra/__init__.py": "import cbcolour\n\n\ndef bang(w):\n    return cbcolour.paint(w).upper() + '!'\n"},
		"cbcolour=="+ver)
	put("pkgs/"+extra, string(extraWheel))
	broken, brokenWheel := pyWheelDeps(t, "cbbroken", ver, map[string]string{"cbbroken/__init__.py": "import cbabsent\n"}, "cbabsent==9.9."+suffix)
	put("pkgs/"+broken, string(brokenWheel))

	pipeline := func(module, out string) string {
		return fmt.Sprintf(`pipeline:
  type: chain
  transforms:
    - type: Create
      config:
        elements:
          - {name: kiwi}
          - {name: fig}
    - type: MapToFields
      config:
        language: python
        fields:
          line:
            callable: "lambda row: __import__('%s').bang(row.name)"
    - type: WriteToText
      config:
        path: %s
`, module, out)
	}
	name := "yaml-pkg-" + suffix
	out, err := gcRun("dataflow", "yaml", "run", name, "--region=us-central1",
		"--yaml-pipeline="+pipeline("cbextra", "gs://"+bucket+"/out/pkg"), "--temp-location=gs://"+bucket+"/tmp",
		"--additional-pipeline-options=extra_packages=gs://"+bucket+"/pkgs/"+extra, "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow yaml run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	j := waitYAMLJob(t, ctx, c, project, id)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the YAML template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readOutputs(t, ctx, sc, bucket, "out/pkg")
	for _, want := range []string{"GREEN KIWI!", "GREEN FIG!"} {
		if !strings.Contains(got, want) {
			t.Errorf("the job's output lacks %q:\n%s", want, got)
		}
	}

	// An extra package whose requirement the index does not hold.
	out, err = gcRun("dataflow", "yaml", "run", name+"-missing", "--region=us-central1",
		"--yaml-pipeline="+pipeline("cbbroken", "gs://"+bucket+"/out/missing"), "--temp-location=gs://"+bucket+"/tmp",
		"--additional-pipeline-options=extra_packages=gs://"+bucket+"/pkgs/"+broken, "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow yaml run: %v\n%s", err, out)
	}
	id = strings.TrimSpace(out)
	if j := waitYAMLJob(t, ctx, c, project, id); j.CurrentState != dataflowpb.JobState_JOB_STATE_FAILED {
		t.Fatalf("the job of a requirement the index lacks is %s; want FAILED:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	if got := readOutputs(t, ctx, sc, bucket, "out/missing"); got != "" {
		t.Errorf("the failed job wrote %q", got)
	}
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestGcloudDataflowYAMLRunJavaProvider (#2256): Google's YAML template
// (`gcloud dataflow yaml run`) runs a pipeline whose read is a Java
// provider's transform, Beam YAML's ReadFromBigTable, which the
// launcher expands with Beam's Java expansion service for Google Cloud
// I/O (beam:schematransform:org.apache.beam:bigtable_read:v1), as
// Google's launcher does. Beam downloads that jar from Maven Central;
// the launcher has no route there, so it is staged in Beam's jar cache.
// The job runs the read in the Java SDK's harness against the instance's
// Bigtable and writes one line per cell, through Beam YAML's Python
// MapToFields and WriteToText. A pipeline file in Cloud Storage with a
// Java write, WriteToBigTable, writes the rows back to another table.
func TestGcloudDataflowYAMLRunJavaProvider(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvBigtable)
	project := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-yamljava-%s", project, suffix)
	sc := storageClient(t, h)
	dataflowBucket(t, ctx, sc, project, bucket)
	table := "dfyamljava" + suffix
	_, bt := bigtableTable(t, h, table)
	tbl := bt.Open(table)
	for k, v := range map[string]string{"apple": "red", "kiwi": "green"} {
		m := bigtable.NewMutation()
		m.Set("info", "color", bigtable.Timestamp(0), []byte(v))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	gcRun := gcloudDataflow(t, h, project)
	yaml := fmt.Sprintf(`pipeline:
  type: chain
  transforms:
    - type: ReadFromBigTable
      config:
        project: %s
        instance: %s
        table: %s
    - type: MapToFields
      config:
        language: python
        fields:
          line: "key.decode() + ' ' + family_name + ':' + column_qualifier.decode() + '=' + cells[0].value.decode()"
    - type: WriteToText
      config:
        path: gs://%s/out/rows
`, project, bigtableInstance, table, bucket)
	name := "yaml-java-" + suffix
	out, err := gcRun("dataflow", "yaml", "run", name, "--region=us-central1",
		"--yaml-pipeline="+yaml, "--temp-location=gs://"+bucket+"/tmp", "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow yaml run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	if j := waitYAMLJob(t, ctx, c, project, id); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the YAML template's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readPrefix(t, ctx, sc, bucket, "out/rows")
	if want := "apple info:color=red,kiwi info:color=green"; strings.Join(got, ",") != want {
		t.Errorf("the job wrote %q; want %s", got, want)
	}

	// A Java write, from a pipeline file.
	dst := "dfyamljavaw" + suffix
	bigtableTable(t, h, dst)
	put := bucketWriter(t, ctx, sc, bucket)
	put("pipelines/write.yaml", fmt.Sprintf(`pipeline:
  type: chain
  transforms:
    - type: Create
      config:
        elements:
          - {key: plum, value: purple}
    - type: MapToFields
      config:
        language: python
        fields:
          key: "bytes(key, 'utf-8')"
          type: "'SetCell'"
          family_name: "'info'"
          column_qualifier: "b'color'"
          value: "bytes(value, 'utf-8')"
          timestamp_micros: "1000"
    - type: WriteToBigTable
      config:
        project: %s
        instance: %s
        table: %s
`, project, bigtableInstance, dst))
	out, err = gcRun("dataflow", "yaml", "run", name+"-write", "--region=us-central1",
		"--yaml-pipeline-file=gs://"+bucket+"/pipelines/write.yaml", "--temp-location=gs://"+bucket+"/tmp", "--format=value(job.id)")
	if err != nil {
		t.Fatalf("gcloud dataflow yaml run --yaml-pipeline-file: %v\n%s", err, out)
	}
	id = strings.TrimSpace(out)
	if j := waitYAMLJob(t, ctx, c, project, id); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the YAML template's write job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	row, err := bt.Open(dst).ReadRow(ctx, "plum")
	if err != nil || len(row["info"]) != 1 || string(row["info"][0].Value) != "purple" {
		t.Errorf("the written table's row plum = %v, %v; want info:color=purple", row, err)
	}
}
