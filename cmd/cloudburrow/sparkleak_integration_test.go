//go:build integration

package main

// #2162: a cancelled batch's Spark containers (its relay among them) must
// not outlive the instance. Every container and network CloudBurrow runs
// for an instance's workloads carries io.cloudburrow.instance=<name>, and
// stopping the instance removes them.
//
// It needs Docker, kind and the pinned apache/spark image, and pulls the
// image when it is missing:
//
//	go test -tags integration -run TestCancelledBatchLeavesNoContainers ./cmd/cloudburrow/

import (
	"context"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// labelled are the instance's labelled containers (all states) and
// networks.
func labelled(t *testing.T, instance string) (containers, networks []string) {
	t.Helper()
	f := "label=" + sparkproc.InstanceLabel + "=" + instance
	out, err := exec.Command("docker", "ps", "-a", "--filter", f, "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	containers = strings.Fields(string(out))
	out, err = exec.Command("docker", "network", "ls", "--filter", f, "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("docker network ls: %v", err)
	}
	return containers, strings.Fields(string(out))
}

func TestCancelledBatchLeavesNoContainers(t *testing.T) {
	name, stateDir := uniqueInstance(t)
	base := 10000 + int(time.Now().UnixNano()%50)*200
	args := []string{"--name", name, "--state-dir", stateDir, "--port-control", "0",
		"--port-base", strconv.Itoa(base), "--services", "bigquery,storage,logging,dataproc"}
	cfg, err := config.Load(config.Options{Args: args, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, _, stop := runUpAsync(t, args...)
	stopped := false
	defer func() {
		if !stopped {
			_ = stop()
		}
		_, _ = sparkproc.RemoveInstance(context.Background(), name)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Endpoints.Dataproc))
	bc, err := dataproc.NewBatchControllerClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	parent := "projects/" + cfg.DefaultProject() + "/locations/us-central1"
	op, err := bc.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "leak-" + name, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_SparkBatch{SparkBatch: &dataprocpb.SparkBatch{
			Driver:      &dataprocpb.SparkBatch_MainClass{MainClass: "org.apache.spark.examples.SparkPi"},
			JarFileUris: []string{"file:///usr/lib/spark/examples/jars/spark-examples.jar"}, Args: []string{"100000"}}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2"}}})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	// Running: the batch's containers, its relay among them, are up.
	deadline := time.Now().Add(15 * time.Minute)
	for {
		b, err := bc.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: parent + "/batches/leak-" + name})
		if err != nil {
			t.Fatalf("GetBatch: %v", err)
		}
		cs, _ := labelled(t, name)
		relay := false
		for _, c := range cs {
			relay = relay || strings.Contains(c, "-relay")
		}
		if b.GetState() == dataprocpb.Batch_RUNNING && relay {
			break
		}
		if b.GetState() != dataprocpb.Batch_PENDING && b.GetState() != dataprocpb.Batch_RUNNING {
			t.Fatalf("the batch ended %s before it was cancelled: %s", b.GetState(), b.GetStateMessage())
		}
		if time.Now().After(deadline) {
			t.Fatalf("the batch never ran with a labelled relay; containers %v", cs)
		}
		time.Sleep(2 * time.Second)
	}
	if err := bc.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		b, err := bc.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: parent + "/batches/leak-" + name})
		if err == nil && b.GetState() == dataprocpb.Batch_CANCELLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the batch was never CANCELLED: %v %v", b.GetState(), err)
		}
	}
	stopped = true
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if cs, ns := labelled(t, name); len(cs) > 0 || len(ns) > 0 {
		t.Errorf("after stop, instance %s still has containers %v and networks %v", name, cs, ns)
	}
}
