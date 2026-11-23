//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const rmAdmin = "org.apache.hadoop.yarn.client.cli.RMAdminCLI"

// rmHAState is one line of `yarn rmadmin -getAllServiceState`:
// HOST:PORT and the ResourceManager's state.
var rmHAState = regexp.MustCompile(`(?m)^(\S+):8033\s+(active|standby)\s*$`)

// dataprocRMHAPySpark runs one Spark action, waits for the HDFS file
// argv[1], and runs another: a YARN application that lives across a
// ResourceManager failover.
const dataprocRMHAPySpark = `import sys, time
from pyspark.sql import SparkSession
spark = SparkSession.builder.appName("rmha").getOrCreate()
sc = spark.sparkContext
print("APP", sc.applicationId, flush=True)
print("BEFORE", sc.parallelize(range(100), 4).map(lambda x: x * 2).sum(), flush=True)
path = sc._jvm.org.apache.hadoop.fs.Path(sys.argv[1])
fs = path.getFileSystem(sc._jsc.hadoopConfiguration())
deadline = time.time() + 1500
while not fs.exists(path) and time.time() < deadline:
    time.sleep(3)
print("AFTER", sc.parallelize(range(100), 4).map(lambda x: x * 3).sum(), flush=True)
spark.stop()
`

// TestDataprocYARNHighAvailability (#2189): a High Availability cluster
// (masterConfig.numInstances 3) runs a YARN ResourceManager on every
// master, as Google's High Availability page documents ("all masters run
// ResourceManager"), with ZooKeeper-based automatic failover and
// recovery, and, with RANGER, Ranger's YARN plugin in each. Through the
// official Go clients: yarn rmadmin (a Hadoop job) reports one active and
// two standby ResourceManagers on -m-0, -m-1 and -m-2; Ranger's Admin
// server lists the yarn-dataproc plugin of each master; a PySpark job's
// YARN application keeps running when the active ResourceManager's master
// stops, as a VM stops: another ResourceManager becomes active, recovers
// the application (one attempt: its ApplicationMaster kept running), the
// NodeManagers register with it, and the job ends DONE; the Component
// Gateway's YARN ResourceManager link follows the active one. Once the
// master is back, its ResourceManager is a standby, and a yarn-dataproc
// deny policy is enforced by the active ResourceManager on another
// master.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/GetCluster, google.cloud.dataproc.v1.JobController/SubmitJob, google.cloud.dataproc.v1.JobController/GetJob
func TestDataprocYARNHighAvailability(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	kmsAddr := h.Endpoint(EnvKMS)
	if kmsAddr == "" {
		t.Skip("Cloud KMS is not served: RANGER's admin password is KMS-encrypted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := dataprocSuffix(h, 26)
	d := newDataprocHDFSClients(t, ctx, h, addr, project, suffix)
	bh := bucket(t, h, d.sc)

	// RANGER's admin password, encrypted with a Cloud KMS key.
	kc, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(kmsAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	parent := "projects/" + project + "/locations/global"
	ring := "rmha-" + suffix
	if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring}); err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: "ranger",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	const password = "Ranger2189x"
	enc, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte(password + "\n")})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pw := bh.Object("rmha/admin-password.encrypted").NewWriter(ctx)
	if _, err := pw.Write(enc.GetCiphertext()); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}

	name := "rmha-" + suffix
	up := dataprocClusterUp(t, ctx, h, d.cc, project, dataprocHDFSRegion, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true},
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
				OptionalComponents: []dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER},
				Properties: map[string]string{"dataproc:ranger.kms.key.uri": key.GetName(),
					"dataproc:ranger.admin.password.uri": "gs://" + bh.BucketName() + "/rmha/admin-password.encrypted"}},
			MasterConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 3},
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2}}})
	uuid := up.GetClusterUuid()
	got, err := d.cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: dataprocHDFSRegion, ClusterName: name})
	if err != nil {
		t.Fatal(err)
	}
	ports := got.GetConfig().GetEndpointConfig().GetHttpPorts()
	yarnUI, rangerUI := ports["YARN ResourceManager"], ports["Ranger"]
	if !strings.HasPrefix(yarnUI, "http") || !strings.Contains(yarnUI, "/component-gateway/") || !strings.HasPrefix(rangerUI, "http") {
		t.Fatalf("httpPorts: %v", ports)
	}
	masters := []string{name + "-m-0", name + "-m-1", name + "-m-2"}

	get := func(base, path, user string, v any) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
		req.Header.Set("Accept", "application/json")
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if v != nil && resp.StatusCode == http.StatusOK {
			_ = json.Unmarshal(b, v)
		}
		return resp.StatusCode, string(b)
	}
	// rmStates are the ResourceManagers' states by master, from yarn
	// rmadmin run as a Hadoop job.
	rmStates := func() (map[string]string, string) {
		out := d.hadoop(h, name, dataprocpb.JobStatus_DONE, rmAdmin, "-getAllServiceState")
		st := map[string]string{}
		for _, m := range rmHAState.FindAllStringSubmatch(out, -1) {
			st[m[1]] = m[2]
		}
		return st, out
	}
	activeOf := func(st map[string]string) (string, int) {
		active, standby := "", 0
		for host, s := range st {
			if s == "active" {
				active = host
			} else {
				standby++
			}
		}
		return active, standby
	}

	st, out := rmStates()
	active, standby := activeOf(st)
	if len(st) != 3 || active == "" || standby != 2 {
		t.Fatalf("the ResourceManagers' states: %s", out)
	}
	for _, m := range masters {
		if _, ok := st[m]; !ok {
			t.Fatalf("no ResourceManager on %s: %s", m, out)
		}
	}

	// Ranger's YARN plugin in every ResourceManager: each master's
	// yarn-dataproc plugin has its policies from the Admin server.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var infos struct {
			List []struct {
				Service string `json:"serviceName"`
				Host    string `json:"hostName"`
				App     string `json:"appType"`
			} `json:"pluginInfoList"`
		}
		code, body := get(rangerUI, "/service/plugins/plugins/info", "admin", &infos)
		hosts := map[string]bool{}
		for _, p := range infos.List {
			if p.Service == "yarn-dataproc" && p.App == "yarn" {
				hosts[p.Host] = true
			}
		}
		if hosts[masters[0]] && hosts[masters[1]] && hosts[masters[2]] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Ranger's YARN plugins = %d %.1500s", code, body)
		}
		time.Sleep(10 * time.Second)
	}

	// A PySpark job, its first action run, waits for /tmp/rmha-go in HDFS.
	main := dataprocPut(t, ctx, bh, "rmha/survive.py", dataprocRMHAPySpark)
	jobID := "rmha-spark-" + suffix
	if _, err := d.jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: dataprocHDFSRegion, Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: name}, Reference: &dataprocpb.JobReference{JobId: jobID},
		TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: main, Args: []string{"/tmp/rmha-go"}}}}}); err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	type yarnApp struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
		Final string `json:"finalStatus"`
	}
	var app yarnApp
	deadline = time.Now().Add(10 * time.Minute)
	for app.ID == "" {
		var apps struct {
			Apps struct {
				App []yarnApp `json:"app"`
			} `json:"apps"`
		}
		code, body := get(yarnUI, "/ws/v1/cluster/apps?states=RUNNING", "", &apps)
		for _, a := range apps.Apps.App {
			if a.Name == "rmha" {
				app = a
			}
		}
		if app.ID == "" && time.Now().After(deadline) {
			t.Fatalf("the PySpark job's YARN application = %d %.800s", code, body)
		}
		time.Sleep(5 * time.Second)
	}

	// The active ResourceManager's master stops, as its VM would.
	node := dataprocNodeContainers(t, ctx, uuid, active)
	dockerRun(t, ctx, append([]string{"stop", "-t", "2"}, node...)...)
	stopped := true
	t.Cleanup(func() {
		if stopped {
			_ = exec.CommandContext(h.Context(), "docker", append([]string{"start"}, node...)...).Run()
		}
	})
	deadline = time.Now().Add(5 * time.Minute)
	var next string
	for {
		st, out = rmStates()
		next, _ = activeOf(st)
		if next != "" && next != active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ResourceManagers once %s stopped: %s", active, out)
		}
		time.Sleep(5 * time.Second)
	}
	// The gateway's link follows the new active ResourceManager, which
	// recovered the application and has both NodeManagers.
	deadline = time.Now().Add(5 * time.Minute)
	for {
		var info struct {
			Cluster struct {
				HAState string `json:"haState"`
				Store   string `json:"rmStateStoreName"`
			} `json:"clusterInfo"`
		}
		var metrics struct {
			Metrics struct {
				Active int `json:"activeNodes"`
			} `json:"clusterMetrics"`
		}
		var got struct {
			App yarnApp `json:"app"`
		}
		c1, b1 := get(yarnUI, "/ws/v1/cluster/info", "", &info)
		c2, b2 := get(yarnUI, "/ws/v1/cluster/metrics", "", &metrics)
		c3, b3 := get(yarnUI, "/ws/v1/cluster/apps/"+app.ID, "", &got)
		if info.Cluster.HAState == "ACTIVE" && strings.HasSuffix(info.Cluster.Store, "ZKRMStateStore") &&
			metrics.Metrics.Active == 2 && got.App.State == "RUNNING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new active ResourceManager through the gateway: %d %.300s; %d %.300s; %d %.500s", c1, b1, c2, b2, c3, b3)
		}
		time.Sleep(5 * time.Second)
	}

	// The job goes on: its second action runs on the new active
	// ResourceManager's containers.
	d.hadoop(h, name, dataprocpb.JobStatus_DONE, fsShell, "-touchz", "/tmp/rmha-go")
	done, err := dataprocWaitJob(ctx, d.jc, project, dataprocHDFSRegion, jobID)
	if err != nil {
		t.Fatalf("job %s: %v", jobID, err)
	}
	driver := dataprocGSText(t, ctx, d.sc, done.GetDriverOutputResourceUri())
	if done.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(driver, "APP "+app.ID) ||
		!strings.Contains(driver, "BEFORE 9900") || !strings.Contains(driver, "AFTER 14850") {
		t.Fatalf("the PySpark job across the failover: %v\n%s", done.GetStatus(), driver)
	}
	var attempts struct {
		Attempts struct {
			Attempt []struct {
				ID int `json:"id"`
			} `json:"appAttempt"`
		} `json:"appAttempts"`
	}
	var final struct {
		App yarnApp `json:"app"`
	}
	if code, body := get(yarnUI, "/ws/v1/cluster/apps/"+app.ID, "", &final); final.App.State != "FINISHED" || final.App.Final != "SUCCEEDED" {
		t.Errorf("the application after the job = %d %.500s", code, body)
	}
	if code, body := get(yarnUI, "/ws/v1/cluster/apps/"+app.ID+"/appattempts", "", &attempts); len(attempts.Attempts.Attempt) != 1 {
		t.Errorf("the application's attempts (its ApplicationMaster kept running) = %d %.500s", code, body)
	}

	// The master back: its ResourceManager rejoins as a standby.
	dockerRun(t, ctx, append([]string{"start"}, node...)...)
	stopped = false
	deadline = time.Now().Add(6 * time.Minute)
	for {
		st, out = rmStates()
		a, s := activeOf(st)
		if len(st) == 3 && a == next && s == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ResourceManagers once %s is back: %s", active, out)
		}
		time.Sleep(10 * time.Second)
	}

	// The active ResourceManager, on another master than before, enforces
	// a yarn-dataproc deny policy with its Ranger plugin.
	rest := func(method, path string, body any) (int, string) {
		bs, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(rangerUI, "/")+path, bytes.NewReader(bs))
		req.SetBasicAuth("admin", password)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		code, body := rest(http.MethodPost, "/service/public/v2/api/policy", map[string]any{"service": "yarn-dataproc",
			"name": "cb-rmha-deny", "isEnabled": true,
			"resources": map[string]any{"queue": map[string]any{"values": []string{"root.default"}}},
			"denyPolicyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{
				map[string]any{"type": "submit-app", "isAllowed": true}, map[string]any{"type": "admin-queue", "isAllowed": true}}}}})
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create the YARN deny policy = %d %.500s", code, body)
		}
		time.Sleep(10 * time.Second)
	}
	in := dataprocPut(t, ctx, bh, "rmha/in/words.txt", dataprocWords)
	var refusal string
	for i := 0; i < 8; i++ {
		d.n++
		j := dataprocRunJob(t, ctx, d.jc, project, dataprocHDFSRegion, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: name},
			Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("rmha-wc%d-%s", d.n, suffix)},
			TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
				Driver: &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/hadoop-mapreduce/hadoop-mapreduce-examples.jar"},
				Args:   []string{"wordcount", strings.TrimSuffix(in, "words.txt"), fmt.Sprintf("gs://%s/rmha/wc%d", bh.BucketName(), i)}}}})
		refusal = dataprocGSText(t, ctx, d.sc, j.GetDriverOutputResourceUri())
		if j.GetStatus().GetState() == dataprocpb.JobStatus_ERROR && strings.Contains(refusal, "does not have permission to submit") {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if !strings.Contains(refusal, "User root does not have permission to submit") || !strings.Contains(refusal, "to queue default") {
		t.Fatalf("a job the yarn-dataproc policy denies, on the ResourceManager of %s:\n%s", next, refusal)
	}
}
