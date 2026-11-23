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
// REST API lets root use every table of the default database, and Hive
// jobs submitted with the official client make two tables; the policy,
// updated through the API to one table, makes a Hive job reading the
// other fail with Ranger's "Permission denied", while one reading the
// first succeeds; and the denial is in the Admin server's access audit,
// which it reads from Solr's ranger_audits.
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
	got := dataprocClusterUp(t, ctx, h, cc, project, region, good)
	base := got.GetConfig().GetEndpointConfig().GetHttpPorts()["Ranger"]
	if base == "" {
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

	// A policy: root may use every table of the default database.
	policy := map[string]any{"service": "hive-dataproc", "name": "cb-root-default", "isEnabled": true,
		"resources": map[string]any{"database": map[string]any{"values": []string{"default"}},
			"table": map[string]any{"values": []string{"*"}}, "column": map[string]any{"values": []string{"*"}}},
		"policyItems": []any{map[string]any{"users": []string{"root"}, "accesses": []any{map[string]any{"type": "all", "isAllowed": true}}}}}
	code, body := rest(http.MethodPost, "/service/public/v2/api/policy", password, policy)
	if code != http.StatusOK {
		t.Fatalf("create policy = %d %.500s", code, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == 0 {
		t.Fatalf("create policy: %v %.300s", err, body)
	}
	if code, body := rest(http.MethodGet, "/service/public/v2/api/service/hive-dataproc/policy/cb-root-default", password, nil); code != http.StatusOK || !strings.Contains(body, `"cb-root-default"`) {
		t.Fatalf("get policy = %d %.300s", code, body)
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
	// may start before it has the new policy.
	var made *dataprocpb.Job
	for i := 0; i < 6; i++ {
		made = hive("CREATE TABLE IF NOT EXISTS cb_open (n INT)", "CREATE TABLE IF NOT EXISTS cb_secret (s STRING)",
			"INSERT INTO cb_open VALUES (7)", "INSERT INTO cb_secret VALUES ('hidden')")
		if made.GetStatus().GetState() == dataprocpb.JobStatus_DONE {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if made.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("Hive job allowed by the policy: %v\n%s", made.GetStatus(), output(made))
	}

	// The policy narrowed to cb_open: reading cb_secret is denied.
	policy["resources"].(map[string]any)["table"] = map[string]any{"values": []string{"cb_open"}}
	if code, body := rest(http.MethodPut, fmt.Sprintf("/service/public/v2/api/policy/%d", created.ID), password, policy); code != http.StatusOK {
		t.Fatalf("update policy = %d %.500s", code, body)
	}
	var denied *dataprocpb.Job
	for i := 0; i < 6; i++ {
		denied = hive("SELECT s FROM cb_secret")
		if denied.GetStatus().GetState() == dataprocpb.JobStatus_ERROR {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if out := output(denied); denied.GetStatus().GetState() != dataprocpb.JobStatus_ERROR ||
		!strings.Contains(out, "Permission denied: user [root] does not have [SELECT] privilege") {
		t.Fatalf("Hive job the policy denies: %v\n%s", denied.GetStatus(), out)
	}
	if ok := hive("SELECT n FROM cb_open"); ok.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(output(ok), "7") {
		t.Errorf("Hive job the policy allows: %v\n%s", ok.GetStatus(), output(ok))
	}

	// The denial, in the Admin server's access audit (from Solr).
	deadline := time.Now().Add(3 * time.Minute)
	for {
		code, body := rest(http.MethodGet, "/service/assets/accessAudit?pageSize=100&sortBy=eventTime&sortType=desc&repoName=hive-dataproc", password, nil)
		if code == http.StatusOK && strings.Contains(body, "default/cb_secret") && strings.Contains(body, `"accessResult":0`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("access audit = %d %.800s", code, body)
		}
		time.Sleep(10 * time.Second)
	}
}
