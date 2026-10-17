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

// TestMemorystoreTokenAuthBackupsAndACLThroughThePage (#1591), in the
// emulators shard. A Redis cluster and a Valkey instance are created on
// their pages with Token authentication checked. On the cluster's page,
// Add token auth user and Add auth token make the user and token
// ListTokenAuthUsers and ListAuthTokens read, Back up now the backup
// ListBackups lists in its backup collection, and Create ACL policy then
// Set ACL policy the policy GetAclPolicy reads and GetCluster names. On the
// instance's page, Add token auth user and Back up now do the same through
// the Memorystore API.
func TestMemorystoreTokenAuthBackupsAndACLThroughThePage(t *testing.T) {
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
	cid, vid, pid := fmt.Sprintf("br-cltok-%d", sfx), fmt.Sprintf("br-vktok-%d", sfx), fmt.Sprintf("br-acl-%d", sfx)
	cname, vname, pname := parent+"/clusters/"+cid, parent+"/instances/"+vid, parent+"/aclPolicies/"+pid
	t.Cleanup(func() {
		bg := context.Background()
		if op, err := cc.DeleteCluster(bg, &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := cc.DeleteAclPolicy(bg, &clusterpb.DeleteAclPolicyRequest{Name: pname}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := vc.DeleteInstance(bg, &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = op.Wait(bg)
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
		p.run(chromedp.Click(`form.create-page #f-tokenAuth`, chromedp.ByQuery),
			chromedp.Click(`form.create-page button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("form.create-page") === null`, fields["id"]))
	}
	page := func(screen, id string) {
		t.Helper()
		p.navigate("/" + screen + q)
		p.clickText("#view tbody a", id)
		p.waitFor(`document.querySelector("#view .page-actions") !== null`)
	}

	// The Redis cluster.
	create("memorystore-clusters", "Create cluster", map[string]string{"id": cid, "shardCount": "1", "replicaCount": "0",
		"nodeType": "REDIS_SHARED_CORE_NANO", "network": network})
	page("memorystore-clusters", cid)
	p.pageAction("Add token auth user", map[string]string{"user": "app"}, "")
	p.pageAction("Add auth token", map[string]string{"user": "app"}, "")
	user := cname + "/tokenAuthUsers/app"
	if u, err := cc.ListTokenAuthUsers(ctx, &clusterpb.ListTokenAuthUsersRequest{Parent: cname}).Next(); err != nil || u.GetName() != user {
		t.Errorf("after Add token auth user, ListTokenAuthUsers = %v, %v", u, err)
	}
	if tok, err := cc.ListAuthTokens(ctx, &clusterpb.ListAuthTokensRequest{Parent: user}).Next(); err != nil || tok.GetToken() == "" {
		t.Errorf("after Add auth token, ListAuthTokens = %v, %v", tok, err)
	}
	p.pageAction("Back up now", map[string]string{"backup": "first"}, "")
	cl, err := cc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if b, err := cc.ListBackups(ctx, &clusterpb.ListBackupsRequest{Parent: cl.GetBackupCollection()}).Next(); err != nil ||
		b.GetName() != cl.GetBackupCollection()+"/backups/first" {
		t.Errorf("after Back up now, ListBackups = %v, %v", b, err)
	}
	p.pageAction("Create ACL policy", map[string]string{"policyId": pid, "rules": "reader=on >readerpass ~app:* +@read"}, "")
	if pol, err := cc.GetAclPolicy(ctx, &clusterpb.GetAclPolicyRequest{Name: pname}); err != nil || len(pol.GetRules()) != 1 ||
		pol.GetRules()[0].GetUsername() != "reader" {
		t.Errorf("after Create ACL policy, GetAclPolicy = %v, %v", pol, err)
	}
	p.pageAction("Set ACL policy", map[string]string{"policy": pname}, "")
	if cl, err := cc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: cname}); err != nil || cl.GetAclPolicy() != pname {
		t.Errorf("after Set ACL policy, GetCluster's acl_policy = %q, %v; want %s", cl.GetAclPolicy(), err, pname)
	}

	// The Valkey instance.
	create("memorystore-valkey", "Create instance", map[string]string{"id": vid, "mode": "CLUSTER_DISABLED", "shardCount": "1",
		"replicaCount": "0", "nodeType": "SHARED_CORE_NANO", "network": network})
	page("memorystore-valkey", vid)
	p.pageAction("Add token auth user", map[string]string{"user": "app"}, "")
	if u, err := vc.ListTokenAuthUsers(ctx, &memorystorepb.ListTokenAuthUsersRequest{Parent: vname}).Next(); err != nil || u.GetName() != vname+"/tokenAuthUsers/app" {
		t.Errorf("after Add token auth user, ListTokenAuthUsers = %v, %v", u, err)
	}
	p.pageAction("Back up now", map[string]string{"backup": "first"}, "")
	inst, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: vname})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if b, err := vc.ListBackups(ctx, &memorystorepb.ListBackupsRequest{Parent: inst.GetBackupCollection()}).Next(); err != nil ||
		b.GetName() != inst.GetBackupCollection()+"/backups/first" {
		t.Errorf("after Back up now, ListBackups = %v, %v", b, err)
	}
}
