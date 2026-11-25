//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
)

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob
//
// TestJavaDataflowBigtableRead (#2215): a Java pipeline reads a Bigtable
// table with Beam's own BigtableIO.read (testdata/dataflowjava's
// BigtableRead), submitted with the DataflowRunner: the Java SDK harness
// reaches the instance's Bigtable through BIGTABLE_EMULATOR_HOST, which
// Google's Bigtable client honours, and the job writes each row it read.
func TestJavaDataflowBigtableRead(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	btAddr := h.Endpoint(EnvBigtable)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-bt-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	table := "dfread" + suffix
	_, bt := bigtableTable(t, h, table)
	tbl := bt.Open(table)
	for k, v := range map[string]string{"apple": "red", "kiwi": "green", "plum": "purple"} {
		m := bigtable.NewMutation()
		m.Set("info", "color", bigtable.Timestamp(0), []byte(v))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	work := buildBeamJava(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr, 9004: btAddr})
	// The submitting program's BigtableIO checks the table, through the
	// emulator variable as Google's client reads it; gRPC resolves a
	// .localhost name to loopback, so it is given the relay's address.
	ip, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{(index .NetworkSettings.Networks \""+network+"\").IPAddress}}",
		strings.Replace(network, "cb-dfnet-", "cb-dfrelay-", 1)).Output()
	if err != nil {
		t.Fatal(err)
	}
	name := "java-bigtable-" + suffix
	out := runBeamJavaEnv(t, ctx, network, work, []string{"BIGTABLE_EMULATOR_HOST=" + strings.TrimSpace(string(ip)) + ":9004"}, "dev.cloudburrow.dataflow.BigtableRead", append([]string{
		"--bigtableProject=" + project, "--bigtableInstance=" + bigtableInstance, "--bigtableTable=" + table,
		"--output=gs://" + bucket + "/out/rows.txt"}, javaDataflowArgs(project, bucket, name)...)...)
	id := javaJobID(t, out)
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	got := readPrefix(t, ctx, sc, bucket, "out/rows")
	want := "apple info:color=red,kiwi info:color=green,plum info:color=purple"
	if strings.Join(got, ",") != want {
		t.Errorf("the job wrote %q; want %s", got, want)
	}
}
