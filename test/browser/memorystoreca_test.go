//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	cluster "cloud.google.com/go/redis/cluster/apiv1"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMemorystoreClusterServerCATabsThroughThePage (#1591), in the
// emulators shard. The Memorystore Redis Cluster page's Create cluster and
// the Memorystore for Valkey page's Create instance, each with Enable
// in-transit encryption and IAM authentication checked, make a cluster and
// an instance the official clients read with SERVER_AUTHENTICATION and IAM
// auth. Each one's page shows both modes and a Server CA tab holding the
// certificate GetClusterCertificateAuthority or GetCertificateAuthority
// returns, as Google's console offers the CA on the resource's page.
func TestMemorystoreClusterServerCATabsThroughThePage(t *testing.T) {
	needService(t, "memorystore-clusters")
	needService(t, "memorystore-valkey")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: every change is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	cc, err := cluster.NewCloudRedisClusterClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	vc, err := memorystore.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	sfx := time.Now().UnixNano() % 1e7
	parent := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	cid, vid := fmt.Sprintf("br-clca-%d", sfx), fmt.Sprintf("br-vkca-%d", sfx)
	cname, vname := parent+"/clusters/"+cid, parent+"/instances/"+vid
	t.Cleanup(func() {
		if op, err := cc.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	q := "?project=" + project
	create := func(screen, label string, fields map[string]string) {
		t.Helper()
		p.navigate("/" + screen + q)
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === %q)`, label))
		p.clickText("#view button", label)
		p.run(chromedp.WaitVisible(`form.create-page #f-id`, chromedp.ByQuery))
		for f, v := range fields {
			p.setField("form.create-page #f-"+f, v)
		}
		p.run(chromedp.Click(`form.create-page #f-transitEncryption`, chromedp.ByQuery),
			chromedp.Click(`form.create-page #f-iamAuth`, chromedp.ByQuery),
			chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("form.create-page") === null`, fields["id"]))
	}
	serverCA := func(screen, id string) (page, tab string) {
		t.Helper()
		p.navigate("/" + screen + q)
		p.clickText("#view tbody a", id)
		p.waitFor(`document.querySelector("#tab-server-ca") !== null`)
		p.eval(`document.querySelector("#view").textContent`, &page)
		p.clickText("#tab-server-ca", "Server CA")
		p.eval(`[...document.querySelectorAll("#view [role=tabpanel]")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &tab)
		return page, tab
	}

	// The Redis cluster.
	create("memorystore-clusters", "Create cluster", map[string]string{"id": cid, "shardCount": "1", "replicaCount": "0",
		"nodeType": "REDIS_SHARED_CORE_NANO", "network": network})
	got, err := cc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: cname})
	if err != nil || got.GetTransitEncryptionMode() != clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION ||
		got.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH {
		t.Fatalf("after Create cluster, GetCluster = %v, %v; want in-transit encryption and IAM auth", got, err)
	}
	cca, err := cc.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: cname + "/certificateAuthority"})
	if err != nil || len(cca.GetManagedServerCa().GetCaCerts()) == 0 || len(cca.GetManagedServerCa().GetCaCerts()[0].GetCertificates()) == 0 {
		t.Fatalf("GetClusterCertificateAuthority = %v, %v", cca, err)
	}
	page, tab := serverCA("memorystore-clusters", cid)
	for _, want := range []string{"TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION", "AUTH_MODE_IAM_AUTH"} {
		if !strings.Contains(page, want) {
			t.Errorf("the cluster's page does not show %q", want)
		}
	}
	if cert := firstPEMLine(cca.GetManagedServerCa().GetCaCerts()[0].GetCertificates()[0]); !strings.Contains(tab, cert) {
		t.Errorf("the cluster's Server CA tab reads %q; want its CA's %q", tab, cert)
	}

	// The Valkey instance.
	create("memorystore-valkey", "Create instance", map[string]string{"id": vid, "mode": "CLUSTER_DISABLED", "shardCount": "1",
		"replicaCount": "0", "nodeType": "SHARED_CORE_NANO", "network": network})
	inst, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: vname})
	if err != nil || inst.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION ||
		inst.GetAuthorizationMode() != memorystorepb.Instance_IAM_AUTH {
		t.Fatalf("after Create instance, GetInstance = %v, %v; want in-transit encryption and IAM auth", inst, err)
	}
	vca, err := vc.GetCertificateAuthority(ctx, &memorystorepb.GetCertificateAuthorityRequest{Name: vname + "/certificateAuthority"})
	if err != nil || len(vca.GetManagedServerCa().GetCaCerts()) == 0 || len(vca.GetManagedServerCa().GetCaCerts()[0].GetCertificates()) == 0 {
		t.Fatalf("GetCertificateAuthority = %v, %v", vca, err)
	}
	page, tab = serverCA("memorystore-valkey", vid)
	for _, want := range []string{"SERVER_AUTHENTICATION", "IAM_AUTH"} {
		if !strings.Contains(page, want) {
			t.Errorf("the instance's page does not show %q", want)
		}
	}
	if cert := firstPEMLine(vca.GetManagedServerCa().GetCaCerts()[0].GetCertificates()[0]); !strings.Contains(tab, cert) {
		t.Errorf("the instance's Server CA tab reads %q; want its CA's %q", tab, cert)
	}
}

// firstPEMLine is a PEM certificate's first line of base64, which no
// other certificate shares.
func firstPEMLine(pem string) string {
	for _, l := range strings.Split(pem, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "-----") {
			return l
		}
	}
	return pem
}
