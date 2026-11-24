//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestDataprocRangerComponent (#2162): the RANGER optional component as
// the Ranger component page documents it
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/ranger),
// through the official Go clients: the admin password encrypted with a key
// of the instance's Cloud KMS and uploaded to Cloud Storage, a cluster with
// SOLR and RANGER and the Component Gateway runs the Ranger Admin server
// (built from the pinned Apache Ranger 2.8.0 source release), whose REST
// API at the gateway's Ranger answers the admin user with that password and
// refuses another. RANGER without SOLR, and without the password's
// properties, is INVALID_ARGUMENT.
//
// The policies are enforced: the Admin server has the hive-dataproc,
// hadoop-dataproc and yarn-dataproc services (the names Google's "Use
// Ranger with Kerberos" page shows); a policy made through Ranger's public
// REST API lets root use the cb_ranger database, where Hive jobs submitted
// with the official client make, fill and read two tables; a policy
// denying root SELECT on one of them, made disabled and then enabled
// through the API, makes a Hive job reading it fail with Ranger's
// "Permission denied", while one reading the other succeeds; and the
// denial is in the Admin server's access audit, which it reads from
// Solr's ranger_audits. The YARN plugin denies a queue the same way, and
// (#2186) the NameNode's HDFS plugin a path: a deny policy of
// hadoop-dataproc on an HDFS directory makes a Hadoop job reading a file
// in it fail with Hadoop's "Permission denied", audited under
// hadoop-dataproc.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocRangerComponent(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	kmsAddr := h.Endpoint(EnvKMS)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	cc, err := dataproc.NewClusterControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	kc, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(kmsAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	const region = "us-central1"
	ring := "rg" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	parent := "projects/" + project + "/locations/global"
	if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring}); err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: "ranger",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	const password = "Ranger2162x"
	enc, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte(password + "\n")})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	w := bh.Object("admin-password.encrypted").NewWriter(ctx)
	if _, err := w.Write(enc.GetCiphertext()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	props := map[string]string{"dataproc:dataproc.allow.zero.workers": "true", "dataproc:ranger.kms.key.uri": key.GetName(),
		"dataproc:ranger.admin.password.uri": "gs://" + bh.BucketName() + "/admin-password.encrypted"}
	suffix := dataprocSuffix(h, 30)
	cluster := func(name string, comps []dataprocpb.Component, p map[string]string) *dataprocpb.Cluster {
		return &dataprocpb.Cluster{ProjectId: project, ClusterName: name, Config: &dataprocpb.ClusterConfig{
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true},
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "3.0", Properties: p, OptionalComponents: comps}}}
	}
	for _, bad := range []struct {
		comps []dataprocpb.Component
		props map[string]string
		want  string
	}{
		{[]dataprocpb.Component{dataprocpb.Component_RANGER}, props, "Solr"},
		{[]dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER},
			map[string]string{"dataproc:ranger.kms.key.uri": key.GetName()}, "dataproc:ranger.admin.password.uri"},
	} {
		_, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: cluster("rgbad-"+suffix, bad.comps, bad.props)})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("CreateCluster %v = %v; want INVALID_ARGUMENT naming %s", bad.comps, err, bad.want)
		}
	}

	good := cluster("rg-"+suffix, []dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER}, props)
	dataprocClusterUp(t, ctx, h, cc, project, region, good)
	got, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: good.ClusterName})
	if err != nil {
		t.Fatal(err)
	}
	base := got.GetConfig().GetEndpointConfig().GetHttpPorts()["Ranger"]
	if !strings.HasPrefix(base, "http") || !strings.Contains(base, "/component-gateway/") {
		t.Fatalf("httpPorts: %v", got.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	rest := func(method, path, pw string, body any) (int, string) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
		req.SetBasicAuth("admin", pw)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp.StatusCode, string(b)
	}
	if code, body := rest(http.MethodGet, "/service/public/v2/api/servicedef/name/hdfs", password, nil); code != http.StatusOK || !strings.Contains(body, `"name":"hdfs"`) {
		t.Errorf("Ranger's hdfs service definition as admin = %d %.300s", code, body)
	}
	if code, _ := rest(http.MethodGet, "/service/public/v2/api/servicedef/name/hdfs", "Wrong2162x", nil); code != http.StatusUnauthorized {
		t.Errorf("Ranger with a wrong password = %d; want 401", code)
	}
	for _, svc := range []string{"hadoop-dataproc", "hive-dataproc", "yarn-dataproc"} {
		if code, body := rest(http.MethodGet, "/service/public/v2/api/service/name/"+svc, password, nil); code != http.StatusOK || !strings.Contains(body, `"name":"`+svc+`"`) {
			t.Errorf("Ranger service %s = %d %.300s", svc, code, body)
		}
	}

	// A policy, made through Ranger's public REST API: root may use every
	// table of the cb_ranger database. (Ranger's default hive policies
	// also let a table's owner use it.)
	allow := map[string]any{"service": "hive-dataproc", "name": "cb-root-ranger", "isEnabled": true,
		"resources": map[string]any{"database": map[string]any{"values": []string{"cb_ranger"}},
			"table": map[string]any{"values": []string{"*"}}, "column": map[string]any{"values": []string{"*"}}},
		"policyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{map[string]any{"type": "all", "isAllowed": true}}}}}
	if code, body := rest(http.MethodPost, "/service/public/v2/api/policy", password, allow); code != http.StatusOK {
		t.Fatalf("create policy = %d %.500s", code, body)
	}
	if code, body := rest(http.MethodGet, "/service/public/v2/api/service/hive-dataproc/policy/cb-root-ranger", password, nil); code != http.StatusOK || !strings.Contains(body, `"cb-root-ranger"`) {
		t.Fatalf("get policy = %d %.300s", code, body)
	}
	// A policy denying root SELECT on cb_secret, made disabled.
	deny := map[string]any{"service": "hive-dataproc", "name": "cb-deny-secret", "isEnabled": false,
		"resources": map[string]any{"database": map[string]any{"values": []string{"cb_ranger"}},
			"table": map[string]any{"values": []string{"cb_secret"}}, "column": map[string]any{"values": []string{"*"}}},
		"denyPolicyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{map[string]any{"type": "select", "isAllowed": true}}}}}
	code, body := rest(http.MethodPost, "/service/public/v2/api/policy", password, deny)
	if code != http.StatusOK {
		t.Fatalf("create the deny policy = %d %.500s", code, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == 0 {
		t.Fatalf("create the deny policy: %v %.300s", err, body)
	}

	jc, err := dataproc.NewJobControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	n := 0
	hive := func(queries ...string) *dataprocpb.Job {
		n++
		return dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: good.ClusterName},
			Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("rghive%d-%s", n, suffix)},
			TypeJob:   &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: queries}}}}})
	}
	output := func(j *dataprocpb.Job) string { return dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri()) }
	// The plugin polls the Admin server every 5 seconds; the first jobs
	// may start before it has the new policies.
	var made *dataprocpb.Job
	for i := 0; i < 6; i++ {
		made = hive("CREATE DATABASE IF NOT EXISTS cb_ranger", "CREATE TABLE IF NOT EXISTS cb_ranger.cb_open (n INT)",
			"CREATE TABLE IF NOT EXISTS cb_ranger.cb_secret (s STRING)", "INSERT INTO cb_ranger.cb_open VALUES (7)",
			"INSERT INTO cb_ranger.cb_secret VALUES ('hidden')", "SELECT s FROM cb_ranger.cb_secret")
		if made.GetStatus().GetState() == dataprocpb.JobStatus_DONE {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if out := output(made); made.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(out, "hidden") {
		t.Fatalf("Hive job the policies allow: %v\n%s", made.GetStatus(), out)
	}

	// The deny policy, enabled through the API: reading cb_secret is denied.
	deny["isEnabled"] = true
	if code, body := rest(http.MethodPut, fmt.Sprintf("/service/public/v2/api/policy/%d", created.ID), password, deny); code != http.StatusOK || !strings.Contains(body, `"isEnabled":true`) {
		t.Fatalf("update policy = %d %.500s", code, body)
	}
	var denied *dataprocpb.Job
	for i := 0; i < 6; i++ {
		denied = hive("SELECT s FROM cb_ranger.cb_secret")
		if denied.GetStatus().GetState() == dataprocpb.JobStatus_ERROR {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if out := output(denied); denied.GetStatus().GetState() != dataprocpb.JobStatus_ERROR ||
		!strings.Contains(out, "Permission denied: user [root] does not have [SELECT] privilege") {
		t.Fatalf("Hive job the policy denies: %v\n%s", denied.GetStatus(), out)
	}
	if ok := hive("SELECT n FROM cb_ranger.cb_open"); ok.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(output(ok), "7") {
		t.Errorf("Hive job the policy allows: %v\n%s", ok.GetStatus(), output(ok))
	}

	// The denial, in the Admin server's access audit (from Solr).
	deadline := time.Now().Add(3 * time.Minute)
	for {
		code, body := rest(http.MethodGet, "/service/assets/accessAudit?pageSize=100&sortBy=eventTime&sortType=desc&repoName=hive-dataproc", password, nil)
		if code == http.StatusOK && strings.Contains(body, "cb_ranger/cb_secret") && strings.Contains(body, `"accessResult":0`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("access audit = %d %.800s", code, body)
		}
		time.Sleep(10 * time.Second)
	}

	// YARN: the ResourceManager authorizes with Ranger's YARN plugin. A
	// Hadoop job runs while no policy decides root's access to the
	// default queue (the queue's own ACL lets everyone submit).
	in := dataprocPut(t, ctx, bh, "rg-in/words.txt", dataprocWords)
	inDir := strings.TrimSuffix(in, "words.txt")
	wordcount := func(out string) *dataprocpb.Job {
		n++
		return dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: good.ClusterName},
			Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("rgyarn%d-%s", n, suffix)},
			TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
				Driver: &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/hadoop-mapreduce/hadoop-mapreduce-examples.jar"},
				Args:   []string{"wordcount", inDir, "gs://" + bh.BucketName() + "/" + out}}}})
	}
	if wc := wordcount("rg-out1"); wc.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("Hadoop job before the YARN deny policy: %v\n%s", wc.GetStatus(), output(wc))
	}
	// A policy of yarn-dataproc, made through the API, denying root
	// submit-app and admin-queue on the default queue (YARN lets a queue's
	// administrators submit to it too): the job is refused by YARN.
	yarnDeny := map[string]any{"service": "yarn-dataproc", "name": "cb-deny-default-queue", "isEnabled": true,
		"resources": map[string]any{"queue": map[string]any{"values": []string{"root.default"}}},
		"denyPolicyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{
			map[string]any{"type": "submit-app", "isAllowed": true}, map[string]any{"type": "admin-queue", "isAllowed": true}}}}}
	if code, body := rest(http.MethodPost, "/service/public/v2/api/policy", password, yarnDeny); code != http.StatusOK {
		t.Fatalf("create the YARN deny policy = %d %.500s", code, body)
	}
	var refused *dataprocpb.Job
	for i := 0; i < 6; i++ {
		refused = wordcount(fmt.Sprintf("rg-out-denied%d", i))
		if refused.GetStatus().GetState() == dataprocpb.JobStatus_ERROR {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if out := output(refused); refused.GetStatus().GetState() != dataprocpb.JobStatus_ERROR ||
		!strings.Contains(out, "User root does not have permission to submit") || !strings.Contains(out, "to queue default") {
		t.Fatalf("Hadoop job the YARN policy denies: %v\n%s", refused.GetStatus(), out)
	}
	// The denial, in the access audit of yarn-dataproc.
	deadline = time.Now().Add(3 * time.Minute)
	for {
		code, body := rest(http.MethodGet, "/service/assets/accessAudit?pageSize=100&sortBy=eventTime&sortType=desc&repoName=yarn-dataproc", password, nil)
		if code == http.StatusOK && strings.Contains(body, `"resourcePath":"root.default"`) && strings.Contains(body, `"accessResult":0`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("YARN access audit = %d %.800s", code, body)
		}
		time.Sleep(10 * time.Second)
	}

	// HDFS: the NameNode authorizes with Ranger's HDFS plugin. A Hadoop
	// job (FsShell) puts a file in HDFS and reads it while no policy
	// decides root's access to it (HDFS's own permissions allow it).
	fs := func(files []string, args ...string) *dataprocpb.Job {
		n++
		return dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: good.ClusterName},
			Reference: &dataprocpb.JobReference{JobId: fmt.Sprintf("rghdfs%d-%s", n, suffix)},
			TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
				Driver: &dataprocpb.HadoopJob_MainClass{MainClass: "org.apache.hadoop.fs.FsShell"}, Args: args, FileUris: files}}})
	}
	if put := fs(nil, "-mkdir", "-p", "/tmp/cb-ranger-hdfs/secret"); put.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("hdfs dfs -mkdir: %v\n%s", put.GetStatus(), output(put))
	}
	if put := fs([]string{in}, "-put", "-f", "words.txt", "/tmp/cb-ranger-hdfs/secret/words.txt"); put.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("hdfs dfs -put: %v\n%s", put.GetStatus(), output(put))
	}
	if cat := fs(nil, "-cat", "/tmp/cb-ranger-hdfs/secret/words.txt"); cat.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(output(cat), "cherry apple banana") {
		t.Fatalf("hdfs dfs -cat before the HDFS deny policy: %v\n%s", cat.GetStatus(), output(cat))
	}
	// A policy of hadoop-dataproc, made through the API, denying root
	// read and execute under /tmp/cb-ranger-hdfs/secret.
	hdfsDeny := map[string]any{"service": "hadoop-dataproc", "name": "cb-deny-hdfs-secret", "isEnabled": true,
		"resources": map[string]any{"path": map[string]any{"values": []string{"/tmp/cb-ranger-hdfs/secret"}, "isRecursive": true}},
		"denyPolicyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{
			map[string]any{"type": "read", "isAllowed": true}, map[string]any{"type": "execute", "isAllowed": true}}}}}
	if code, body := rest(http.MethodPost, "/service/public/v2/api/policy", password, hdfsDeny); code != http.StatusOK {
		t.Fatalf("create the HDFS deny policy = %d %.500s", code, body)
	}
	var blocked *dataprocpb.Job
	for i := 0; i < 6; i++ {
		blocked = fs(nil, "-cat", "/tmp/cb-ranger-hdfs/secret/words.txt")
		if blocked.GetStatus().GetState() == dataprocpb.JobStatus_ERROR {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if out := output(blocked); blocked.GetStatus().GetState() != dataprocpb.JobStatus_ERROR || !strings.Contains(out, "Permission denied: user=root") {
		t.Fatalf("hdfs dfs -cat the HDFS policy denies: %v\n%s", blocked.GetStatus(), out)
	}
	if ls := fs(nil, "-ls", "/tmp/cb-ranger-hdfs"); ls.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(output(ls), "/tmp/cb-ranger-hdfs/secret") {
		t.Errorf("hdfs dfs -ls outside the denied path: %v\n%s", ls.GetStatus(), output(ls))
	}
	// The denial, in the access audit of hadoop-dataproc.
	deadline = time.Now().Add(3 * time.Minute)
	for {
		code, body := rest(http.MethodGet, "/service/assets/accessAudit?pageSize=100&sortBy=eventTime&sortType=desc&repoName=hadoop-dataproc", password, nil)
		if code == http.StatusOK && strings.Contains(body, `"resourcePath":"/tmp/cb-ranger-hdfs/secret`) && strings.Contains(body, `"accessResult":0`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HDFS access audit = %d %.800s", code, body)
		}
		time.Sleep(10 * time.Second)
	}
}
