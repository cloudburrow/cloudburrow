//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
)

// beamGCPExpansionJar is Apache Beam 2.78.0's Java expansion service for
// Google Cloud I/O, the jar the Python SDK's ReadFromBigtable starts
// (BeamJarExpansionService) when no expansion service is given.
const beamGCPExpansionJar = "org.apache.beam:beam-sdks-java-io-google-cloud-platform-expansion-service:2.78.0"

// beamExpansionService fetches beamGCPExpansionJar from Maven Central and
// runs it on network, in the Beam Java 17 SDK image, as the Python SDK
// runs it beside the submitting program; it answers the network the
// submitting program runs on (the service's container's: the Python SDK
// dials an expansion service with local credentials, which take a
// loopback address only) and the service's address there.
func beamExpansionService(t *testing.T, ctx context.Context, network string) (string, string) {
	t.Helper()
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "m2"), 0o777); err != nil {
		t.Fatal(err)
	}
	script := "mvn -q -B -Dmaven.repo.local=/w/m2 dependency:copy -Dartifact=" + beamGCPExpansionJar +
		" -DoutputDirectory=/w/jar && rm -rf /w/m2 && chmod -R a+rwX /w"
	if out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", work+":/w", "-w", "/w", "--entrypoint", "sh",
		beamJavaBuildImage, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("fetch the expansion service: %v\n%s", err, out)
	}
	name := "cb-dfexp-" + fmt.Sprint(time.Now().UnixNano()%1e9)
	jar := "/w/jar/beam-sdks-java-io-google-cloud-platform-expansion-service-2.78.0.jar"
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--network", network, "-v", work+":/w:ro",
		"--entrypoint", "java", beamJava17Image, "-jar", jar, "8097").CombinedOutput(); err != nil {
		t.Fatalf("start the expansion service: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	for deadline := time.Now().Add(2 * time.Minute); ; {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		// It prints its transforms as it starts serving.
		if strings.Contains(string(logs), "Registered transforms:") {
			time.Sleep(3 * time.Second)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the expansion service did not start:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
	return "container:" + name, "localhost:8097"
}

// beamJava17Image is Beam 2.78.0's Java 17 SDK container
// (internal/beamrun's).
const beamJava17Image = "apache/beam_java17_sdk@sha256:8acfe1acfd173b80474c12833397ca1d19b432a3aa5d0d160c5f8fd4abf95126"

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob
//
// TestPythonDataflowBigtableRead (#2215): a Python pipeline reads a
// Bigtable table with the Python SDK's ReadFromBigtable, the
// cross-language transform Beam's Java expansion service for Google Cloud
// I/O expands (docs: Dataflow's multi-language pipelines), submitted with
// the DataflowRunner. The job runs the read in the Java SDK's harness,
// against the instance's Bigtable, and the rest in Python's; it writes
// each row it read.
func TestPythonDataflowBigtableRead(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	btAddr := h.Endpoint(EnvBigtable)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-xbt-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	table := "dfxread" + suffix
	_, bt := bigtableTable(t, h, table)
	tbl := bt.Open(table)
	for k, v := range map[string]string{"apple": "red", "kiwi": "green", "plum": "purple"} {
		m := bigtable.NewMutation()
		m.Set("info", "color", bigtable.Timestamp(0), []byte(v))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr, 9004: btAddr})
	submit, expansion := beamExpansionService(t, ctx, network)
	name := "py-bigtable-xlang-" + suffix
	args := append([]string{"/flow/bigtable_xlang.py", "--bigtable_project=" + project, "--bigtable_instance=" + bigtableInstance,
		"--bigtable_table=" + table, "--expansion_service=" + expansion, "--output=gs://" + bucket + "/out/rows.txt"},
		dataflowRunnerArgs(project, bucket, name)...)
	id := jobIDOf(t, runBeamPython(t, ctx, submit, project, args...))
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readPrefix(t, ctx, sc, bucket, "out/rows")
	want := "apple info:color=red,kiwi info:color=green,plum info:color=purple"
	if strings.Join(got, ",") != want {
		t.Errorf("the job wrote %q; want %s", got, want)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob
//
// TestPythonDataflowBeamYAMLJavaProvider (#2215): a Beam YAML pipeline
// (docs: Dataflow's Beam YAML) whose read is a Java provider's transform:
// ReadFromBigTable, Beam YAML's standard Bigtable read, which Beam's Java
// expansion service for Google Cloud I/O expands
// (beam:schematransform:org.apache.beam:bigtable_read:v1). It is run with
// apache_beam.yaml.main and the DataflowRunner, the expansion service
// given by --beam_services as the YAML provider's gradle target, as Beam
// documents for a service already running; the rest of the pipeline is
// Beam YAML's Python MapToFields and WriteToText. The job runs the read in
// the Java SDK's harness, against the instance's Bigtable, and writes one
// line per cell.
func TestPythonDataflowBeamYAMLJavaProvider(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	btAddr := h.Endpoint(EnvBigtable)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-yaml-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	table := "dfyaml" + suffix
	_, bt := bigtableTable(t, h, table)
	tbl := bt.Open(table)
	for k, v := range map[string]string{"apple": "red", "kiwi": "green"} {
		m := bigtable.NewMutation()
		m.Set("info", "color", bigtable.Timestamp(0), []byte(v))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr, 9004: btAddr})
	submit, expansion := beamExpansionService(t, ctx, network)
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
	name := "py-yaml-java-" + suffix
	args := append([]string{"-m", "apache_beam.yaml.main", "--yaml_pipeline=" + yaml,
		`--beam_services={"sdks:java:io:google-cloud-platform:expansion-service:shadowJar": "` + expansion + `"}`},
		dataflowRunnerArgs(project, bucket, name)...)
	id := jobIDOf(t, runBeamPython(t, ctx, submit, project, args...))
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readPrefix(t, ctx, sc, bucket, "out/rows")
	if want := "apple info:color=red,kiwi info:color=green"; strings.Join(got, ",") != want {
		t.Errorf("the job wrote %q; want %s", got, want)
	}
}
