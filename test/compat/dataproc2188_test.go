//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataprocHDFSClients are the official Go client's cluster and job
// controllers, a Cloud Storage bucket, and job runners on cluster name.
type dataprocHDFSClients struct {
	t       *testing.T
	ctx     context.Context
	cc      *dataproc.ClusterControllerClient
	jc      *dataproc.JobControllerClient
	sc      *storage.Client
	project string
	suffix  string
	n       int
}

const dataprocHDFSRegion = "us-central1"

func newDataprocHDFSClients(t *testing.T, ctx context.Context, h *Harness, addr, project, suffix string) *dataprocHDFSClients {
	t.Helper()
	grpcOpts := append(dataprocGRPC(), option.WithEndpoint(addr))
	cc, err := dataproc.NewClusterControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	jc, err := dataproc.NewJobControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	return &dataprocHDFSClients{t: t, ctx: ctx, cc: cc, jc: jc, sc: storageClient(t, h), project: project, suffix: suffix}
}

// hadoop runs a Hadoop job of main class on cluster and returns its
// driver output, failing the test unless it ends in want.
func (d *dataprocHDFSClients) hadoop(h *Harness, cluster string, want dataprocpb.JobStatus_State, class string, args ...string) string {
	d.t.Helper()
	d.n++
	j := &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: cluster},
		Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("hdfs2-%d-%s", d.n, d.suffix)},
		TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
			Driver: &dataprocpb.HadoopJob_MainClass{MainClass: class}, Args: args}}}
	done := dataprocRunJob(d.t, d.ctx, d.jc, d.project, dataprocHDFSRegion, j)
	out := dataprocGSText(d.t, d.ctx, d.sc, done.GetDriverOutputResourceUri())
	if done.GetStatus().GetState() != want {
		d.t.Fatalf("job %s (%s %v): %v; want %v\n%s", j.Reference.JobId, class, args, done.GetStatus(), want, out)
	}
	return out
}

const (
	fsShell = "org.apache.hadoop.fs.FsShell"
	haAdmin = "org.apache.hadoop.hdfs.tools.DFSHAAdmin"
)

// dataprocNodeContainers are the containers of a cluster's node (its host
// name), the node's own first: what stops when the node's VM does.
func dataprocNodeContainers(t *testing.T, ctx context.Context, uuid, host string) []string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Names}}",
		"--filter", "label=cloudburrow.dataproc.cluster="+uuid, "--filter", "label=cloudburrow.dataproc.node="+host).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	var node, rest []string
	for _, n := range strings.Fields(string(out)) {
		if strings.HasSuffix(n, "-m-1") || strings.HasSuffix(n, "-m-2") {
			node = append(node, n)
		} else {
			rest = append(rest, n)
		}
	}
	if len(node) > 1 || len(rest) == 0 || (len(node) == 0 && !strings.HasSuffix(host, "-m-0")) {
		t.Fatalf("node %s's containers: %q", host, out)
	}
	return append(node, rest...)
}

func dockerRun(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// TestDataprocHDFSHighAvailability (#2188): a High Availability cluster
// (masterConfig.numInstances 3) runs HDFS High Availability as Google's
// High Availability page documents it: NameNodes (with their
// ZKFailoverControllers) on -m-0 and -m-1, a JournalNode and a ZooKeeper
// server on each of the three masters, and fs.defaultFS the nameservice
// hdfs://CLUSTER. Through the official Go client's jobs: the NameNodes'
// states (DFSHAAdmin), a graceful failover to nn1, then nn1's node (-m-1)
// stopped as a VM stops: ZooKeeper's election makes nn0 active again, and
// HDFS keeps its data and takes writes; once -m-1 is back, nn1 is the
// standby.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/GetCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocHDFSHighAvailability(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	suffix := dataprocSuffix(h, 26)
	d := newDataprocHDFSClients(t, ctx, h, addr, project, suffix)
	name := "hdfsha-" + suffix
	up := dataprocClusterUp(t, ctx, h, d.cc, project, dataprocHDFSRegion, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"},
			MasterConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 3},
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2}}})
	if got := up.GetConfig().GetMasterConfig().GetInstanceNames(); strings.Join(got, ",") != name+"-m-0,"+name+"-m-1,"+name+"-m-2" {
		t.Fatalf("masterConfig.instanceNames = %v", got)
	}
	uuid := up.GetClusterUuid()

	// The nameservice is fs.defaultFS; a file written through it.
	in := dataprocPut(t, ctx, bucket(t, h, d.sc), "ha/words.txt", dataprocWords)
	d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-mkdir", "-p", "/data")
	d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-cp", in, "hdfs:///data/words.txt")
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-ls", "hdfs://"+name+"/data"); !strings.Contains(out, "hdfs://"+name+"/data/words.txt") {
		t.Fatalf("hdfs://%s/data: %s", name, out)
	}
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-df", "/"); !strings.Contains(out, "hdfs://"+name+" ") {
		t.Errorf("hdfs dfs -df / is not the nameservice's: %s", out)
	}
	states := func() string {
		return d.hadoop(h, name, dataprocpb.JobStatus_DONE, haAdmin, "-getAllServiceState")
	}
	out := states()
	if !strings.Contains(out, name+"-m-0:8020") || !strings.Contains(out, name+"-m-1:8020") ||
		strings.Count(out, "active") != 1 || strings.Count(out, "standby") != 1 {
		t.Fatalf("the NameNodes' states: %s", out)
	}

	// A graceful failover to nn1 through the ZKFCs: refused to a job's
	// user (root), as HDFS lets only its administrators fail over; the
	// hdfs superuser's, on -m-0 (as `sudo -u hdfs hdfs haadmin` there).
	if out := d.hadoop(h, name, dataprocpb.JobStatus_ERROR, haAdmin, "-failover", "nn0", "nn1"); !strings.Contains(out, "Not listed in dfs.cluster.administrators") {
		t.Errorf("root's failover: %s", out)
	}
	failover := func(from, to string) {
		t.Helper()
		nn := ""
		for _, c := range dataprocNodeContainers(t, ctx, uuid, name+"-m-0") {
			if strings.HasSuffix(c, "-m-hdfs") {
				nn = c
			}
		}
		if nn == "" {
			t.Fatalf("no NameNode container on %s-m-0", name)
		}
		dockerRun(t, ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", "-e", "HADOOP_CONF_DIR=/cb-conf", nn,
			"/opt/hadoop/bin/hdfs", "haadmin", "-failover", from, to)
	}
	failover("nn0", "nn1")
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, haAdmin, "-getServiceState", "nn1"); !strings.Contains(out, "active") {
		t.Fatalf("nn1 after the failover: %s", out)
	}

	// -m-1, the active NameNode's node, stops: nn0 takes over.
	node := dataprocNodeContainers(t, ctx, uuid, name+"-m-1")
	dockerRun(t, ctx, append([]string{"stop", "-t", "2"}, node...)...)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, haAdmin, "-getServiceState", "nn0")
		if strings.Contains(out, "active") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nn0 after -m-1 stopped: %s", out)
		}
		time.Sleep(5 * time.Second)
	}
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-cat", "/data/words.txt"); !strings.Contains(out, "cherry apple banana") {
		t.Errorf("/data/words.txt after the failover: %s", out)
	}
	d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-cp", "/data/words.txt", "/data/after-failover.txt")

	// -m-1 back: nn1 rejoins as the standby, and has the edits made
	// while it was down.
	dockerRun(t, ctx, append([]string{"start"}, node...)...)
	deadline = time.Now().Add(4 * time.Minute)
	for {
		out := states()
		if strings.Contains(out, name+"-m-1:8020") && strings.Count(out, "standby") == 1 && strings.Count(out, "active") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the NameNodes once -m-1 is back: %s", out)
		}
		time.Sleep(5 * time.Second)
	}
	failover("nn0", "nn1")
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-cat", "/data/after-failover.txt"); !strings.Contains(out, "cherry apple banana") {
		t.Errorf("nn1, active again, lacks the file written while it was down: %s", out)
	}
}

// TestDataprocHDFSLocalSSDs (#2188): with workerConfig.diskConfig.
// numLocalSsds, each primary worker's DataNode keeps its blocks on its
// local SSDs (/mnt/N/hadoop/dfs/data, each a 375 GB volume) and its
// NodeManager YARN's scratch on them, as Dataproc's local SSDs page
// documents; dataproc:dataproc.localssd.mount.enable=false leaves them
// unused (the boot disk's capacity). More than DiskConfig's 8 local SSDs
// is INVALID_ARGUMENT.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocHDFSLocalSSDs(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	suffix := dataprocSuffix(h, 25)
	d := newDataprocHDFSClients(t, ctx, h, addr, project, suffix)

	_, err := d.cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: dataprocHDFSRegion,
		Cluster: &dataprocpb.Cluster{ProjectId: project, ClusterName: "ssd9-" + suffix, Config: &dataprocpb.ClusterConfig{
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2, DiskConfig: &dataprocpb.DiskConfig{NumLocalSsds: 9}}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateCluster with 9 local SSDs: %v; want InvalidArgument", err)
	}

	name := "hdfsssd-" + suffix
	dataprocClusterUp(t, ctx, h, d.cc, project, dataprocHDFSRegion, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"},
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2,
				DiskConfig: &dataprocpb.DiskConfig{BootDiskSizeGb: 100, NumLocalSsds: 2, LocalSsdInterface: "nvme"}}}})
	// Two workers of two 375 GB SSDs.
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-df", "/"); !strings.Contains(out, fmt.Sprint(4*375*(int64(1)<<30))) {
		t.Errorf("hdfs dfs -df with two local SSDs a worker: %s", out)
	}
	// A MapReduce job (its shuffle on the SSDs) writes HDFS.
	in := dataprocPut(t, ctx, bucket(t, h, d.sc), "ssd/words.txt", dataprocWords)
	d.n++
	wc := &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: name},
		Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("hdfs2-%d-%s", d.n, suffix)},
		TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
			Driver: &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/hadoop-mapreduce/hadoop-mapreduce-examples.jar"},
			Args:   []string{"wordcount", strings.TrimSuffix(in, "words.txt"), "/data/wc"}}}}
	if done := dataprocRunJob(t, ctx, d.jc, project, dataprocHDFSRegion, wc); done.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("word count: %v", done.GetStatus())
	}
	if out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-cat", "/data/wc/part-r-00000"); !strings.Contains(out, "apple\t3") {
		t.Errorf("the word count in HDFS: %s", out)
	}

	unmounted := "hdfsnossd-" + suffix
	dataprocClusterUp(t, ctx, h, d.cc, project, dataprocHDFSRegion, &dataprocpb.Cluster{ProjectId: project, ClusterName: unmounted,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
			Properties: map[string]string{"dataproc:dataproc.localssd.mount.enable": "false"}},
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2,
				DiskConfig: &dataprocpb.DiskConfig{BootDiskSizeGb: 100, NumLocalSsds: 1}}}})
	if out := d.hadoop(h, unmounted, dataprocpb.JobStatus_DONE, fsShell, "-df", "/"); !strings.Contains(out, fmt.Sprint(2*100*(int64(1)<<30))) {
		t.Errorf("hdfs dfs -df with the local SSDs unmounted: %s", out)
	}
}

// The restart probe (#2188): HDFS keeps its data across the instance's
// stop and up, as a Compute Engine persistent disk does. CI runs the
// setup alone just before `stop`, and the probe after `up`.
const (
	envDataprocHDFSSetup  = "CLOUDBURROW_TEST_DATAPROC_HDFS_SETUP"
	envDataprocHDFSExpect = "CLOUDBURROW_TEST_DATAPROC_HDFS_EXPECT"
	dataprocHDFSRestarted = "hdfsrestart"
)

// TestDataprocHDFSRestartSetup leaves a running cluster with a file in
// its HDFS for TestDataprocHDFSAcrossRestart.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocHDFSRestartSetup(t *testing.T) {
	if os.Getenv(envDataprocHDFSSetup) == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envDataprocHDFSSetup)
	}
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	d := newDataprocHDFSClients(t, ctx, h, addr, project, "setup")
	op, err := d.cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: dataprocHDFSRegion,
		Cluster: &dataprocpb.Cluster{ProjectId: project, ClusterName: dataprocHDFSRestarted, Config: &dataprocpb.ClusterConfig{
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"},
			WorkerConfig:   &dataprocpb.InstanceGroupConfig{NumInstances: 2}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	in := dataprocPut(t, ctx, bucket(t, h, d.sc), "restart/words.txt", dataprocWords)
	d.hadoop(h, dataprocHDFSRestarted, dataprocpb.JobStatus_DONE, fsShell, "-mkdir", "-p", "/data")
	d.hadoop(h, dataprocHDFSRestarted, dataprocpb.JobStatus_DONE, fsShell, "-cp", in, "/data/before-restart.txt")
}

// TestDataprocHDFSAcrossRestart: after the instance's stop and up, the
// cluster runs again and its HDFS has the file; then it is deleted.
//
// covers: google.cloud.dataproc.v1.ClusterController/GetCluster, google.cloud.dataproc.v1.ClusterController/DeleteCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocHDFSAcrossRestart(t *testing.T) {
	if os.Getenv(envDataprocHDFSExpect) == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envDataprocHDFSExpect)
	}
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	d := newDataprocHDFSClients(t, ctx, h, addr, project, "probe")
	t.Cleanup(func() {
		if op, err := d.cc.DeleteCluster(h.Context(), &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: dataprocHDFSRegion,
			ClusterName: dataprocHDFSRestarted}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	deadline := time.Now().Add(15 * time.Minute)
	for {
		c, err := d.cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: dataprocHDFSRegion, ClusterName: dataprocHDFSRestarted})
		if err != nil {
			t.Fatalf("GetCluster after the restart: %v", err)
		}
		if st := c.GetStatus().GetState(); st == dataprocpb.ClusterStatus_RUNNING {
			break
		} else if st == dataprocpb.ClusterStatus_ERROR || time.Now().After(deadline) {
			t.Fatalf("the cluster after the restart: %v", c.GetStatus())
		}
		time.Sleep(5 * time.Second)
	}
	if out := d.hadoop(h, dataprocHDFSRestarted, dataprocpb.JobStatus_DONE, fsShell, "-cat", "/data/before-restart.txt"); !strings.Contains(out, "cherry apple banana") {
		t.Fatalf("HDFS after the instance's restart: %s", out)
	}
}
