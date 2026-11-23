//go:build compat

package compat

import (
	"context"
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
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster
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

	got := dataprocClusterUp(t, ctx, h, cc, project, region, cluster("rg-"+suffix,
		[]dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER}, props))
	base := got.GetConfig().GetEndpointConfig().GetHttpPorts()["Ranger"]
	if base == "" {
		t.Fatalf("httpPorts: %v", got.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	call := func(user, pw string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/service/public/v2/api/servicedef/name/hdfs", nil)
		req.SetBasicAuth(user, pw)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(b)
	}
	if code, body := call("admin", password); code != http.StatusOK || !strings.Contains(body, `"name":"hdfs"`) {
		t.Errorf("Ranger's hdfs service definition as admin = %d %.300s", code, body)
	}
	if code, _ := call("admin", "Wrong2162x"); code != http.StatusUnauthorized {
		t.Errorf("Ranger with a wrong password = %d; want 401", code)
	}
}
