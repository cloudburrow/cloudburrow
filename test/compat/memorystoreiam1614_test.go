//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Memorystore's connect roles (#1614), as Google names them.
const (
	redisConnectRole  = "roles/redis.dbConnectionUser"
	valkeyConnectRole = "roles/memorystore.dbConnectionUser"
)

// iamPrincipal is a service account of its own for one test, with an
// access token CloudBurrow minted for it through the official IAM
// Credentials client (generateAccessToken), and the project roles granted
// it, which its cleanup takes back.
type iamPrincipal struct {
	email, token string
	grant        func(role string)
	revoke       func(role string)
}

// newIAMPrincipal asks for the metadata and Resource Manager endpoints
// before any call, so a shard without them skips. Resource Manager is the
// memorystore shard's MEMORYSTORE_RESOURCEMANAGER, so the Resource Manager
// suite stays in storage.
func newIAMPrincipal(t *testing.T, h *Harness, project, name string, roles ...string) *iamPrincipal {
	t.Helper()
	return newIAMPrincipalIn(t, h, envMemorystoreResourceManager, project, name, roles...)
}

// newIAMPrincipalIn is newIAMPrincipal in a shard whose own name for
// Resource Manager is rmEnv (Cloud SQL's is SQL_RESOURCEMANAGER, #1944).
func newIAMPrincipalIn(t *testing.T, h *Harness, rmEnv, project, name string, roles ...string) *iamPrincipal {
	t.Helper()
	meta := h.Endpoint(EnvMetadata)
	// projectRoles reads the shared variable, so the shard's own name is
	// exported under it for this test.
	shardEndpoint(h, rmEnv, EnvResourceManager)
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	ctx := h.Context()
	// The token is minted through IAM Credentials as the ADC fixture's
	// account, granted the Token Creator role on a service account of
	// the test's own (#1747).
	me, source := instanceAccount(t)
	ic, err := credentials.NewIamCredentialsRESTClient(ctx, option.WithEndpoint(meta),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: source}}))
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	sa := newServiceAccount(t, h, project, name)
	grantOn(t, h, sa.GetName(), "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+me)
	p := &iamPrincipal{email: sa.GetEmail()}
	change := projectRoles(t, h, project, "serviceAccount:"+p.email)
	p.grant = func(role string) { change(role, true) }
	p.revoke = func(role string) { change(role, false) }
	for _, role := range roles {
		p.grant(role)
		t.Cleanup(func() { change(role, false) })
	}
	tok, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{
		Name:  "projects/-/serviceAccounts/" + p.email,
		Scope: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	p.token = tok.GetAccessToken()
	return p
}

// authErr is the error a client gets on its first command at addr,
// authenticating as user (none for "") with pass.
func authErr(ctx context.Context, addr, user, pass string) error {
	c := redis.NewClient(&redis.Options{Addr: addr, Username: user, Password: pass, MaxRetries: -1,
		DialTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second})
	defer c.Close()
	return c.Ping(ctx).Err()
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster
//
// TestMemorystoreIAMAuthChecksTheToken (#1614), with the official
// cloud.google.com/go/memorystore/apiv1, redis/cluster/apiv1,
// iam/credentials/apiv1 and resourcemanager/apiv3 clients and go-redis: a
// Valkey instance and a Redis cluster with IAM auth take, as their AUTH
// password (no username, "default", or the principal's email), an access
// token CloudBurrow minted for a principal granted the product's connect
// role, and refuse with WRONGPASS a made-up token, a token of a principal
// without the role (one granted the other product's role among them), and
// a granted token sent as another user; no AUTH is NOAUTH. Taking the role
// back refuses the principal's new connections, and its open connection
// keeps working, as Google documents. Turning a cluster's IAM auth off
// takes any client again, and on again checks the token.
func TestMemorystoreIAMAuthChecksTheToken(t *testing.T) {
	h := New(t)
	vc, project := valkeyAdminClient(t, h)
	cc, _ := redisClusterAdminClient(t, h)
	valkeyUser := newIAMPrincipal(t, h, project, "vk-iam", valkeyConnectRole)
	redisUser := newIAMPrincipal(t, h, project, "rc-iam", redisConnectRole)
	nobody := newIAMPrincipal(t, h, project, "no-iam")
	parent := "projects/" + project + "/locations/us-central1"
	sfx := time.Now().UnixNano() % 1e8

	vid := fmt.Sprintf("compat-vkiam-%d", sfx)
	vop, err := vc.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: vid, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}},
		AuthorizationMode:  memorystorepb.Instance_IAM_AUTH,
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + vid}); err == nil {
			wctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = waitLRODone(wctx, op)
		}
	})
	cid := fmt.Sprintf("compat-rciam-%d", sfx)
	cname := parent + "/clusters/" + cid
	cop, err := cc.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: cid, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(0), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH,
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cc.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			wctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = waitLRODone(wctx, op)
		}
	})
	inst, err := waitLRO(memorystoreWait(t), vop)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	cl, err := waitLRO(memorystoreWait(t), cop)
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	vaddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	ep := cl.GetDiscoveryEndpoints()[0]
	caddr := net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))
	ctx := h.Context()

	const wrongpass = "WRONGPASS invalid username-password pair or user is disabled"
	refused := func(what, addr, user, pass, want string) {
		t.Helper()
		if err := authErr(ctx, addr, user, pass); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	taken := func(what, addr, user, pass string) {
		t.Helper()
		if err := authErr(ctx, addr, user, pass); err != nil {
			t.Errorf("%s: %v, want it taken", what, err)
		}
	}
	for _, x := range []struct {
		what, addr string
		user       *iamPrincipal
		other      *iamPrincipal
	}{{"Valkey instance", vaddr, valkeyUser, redisUser}, {"Redis cluster", caddr, redisUser, valkeyUser}} {
		taken(x.what+": the granted principal's token", x.addr, "", x.user.token)
		taken(x.what+": the token as user default", x.addr, "default", x.user.token)
		taken(x.what+": the token as the principal's email", x.addr, x.user.email, x.user.token)
		refused(x.what+": a made-up token", x.addr, "", "cbl_madeupmadeupmadeupmadeupmadeupmade", wrongpass)
		refused(x.what+": a principal without a role", x.addr, "", nobody.token, wrongpass)
		refused(x.what+": a principal with the other product's role", x.addr, "", x.other.token, wrongpass)
		refused(x.what+": the token as another principal", x.addr, nobody.email, x.user.token, wrongpass)
		refused(x.what+": no AUTH", x.addr, "", "", "NOAUTH")
	}

	// The role taken back: new connections are refused once the nodes
	// follow the policy; an open one keeps working.
	open := redis.NewClient(&redis.Options{Addr: vaddr, Password: valkeyUser.token, PoolSize: 1, MinIdleConns: 1})
	defer open.Close()
	if err := open.Set(ctx, "iam", "kept", 0).Err(); err != nil {
		t.Fatalf("SET with the granted token: %v", err)
	}
	valkeyUser.revoke(valkeyConnectRole)
	deadline := time.Now().Add(time.Minute)
	for authErr(ctx, vaddr, "", valkeyUser.token) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	refused("Valkey instance: the token after its role is taken back", vaddr, "", valkeyUser.token, wrongpass)
	if v, err := open.Get(ctx, "iam").Result(); err != nil || v != "kept" {
		t.Errorf("the connection opened before the role was taken back: GET = %q, %v", v, err)
	}
	valkeyUser.grant(valkeyConnectRole)
	deadline = time.Now().Add(time.Minute)
	for authErr(ctx, vaddr, "", valkeyUser.token) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	taken("Valkey instance: the token once the role is granted again", vaddr, "", valkeyUser.token)

	// A cluster's IAM auth turned off, then on again.
	update := func(mode clusterpb.AuthorizationMode) {
		t.Helper()
		op, err := cc.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"authorization_mode"}},
			Cluster:    &clusterpb.Cluster{Name: cname, AuthorizationMode: mode},
		})
		if err != nil {
			t.Fatalf("UpdateCluster %s: %v", mode, err)
		}
		got, err := waitLRO(memorystoreWait(t), op)
		if err != nil || got.GetAuthorizationMode() != mode {
			t.Fatalf("UpdateCluster %s: %v, %v", mode, got.GetAuthorizationMode(), err)
		}
	}
	update(clusterpb.AuthorizationMode_AUTH_MODE_DISABLED)
	taken("Redis cluster with IAM auth off: no AUTH", caddr, "", "")
	update(clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH)
	refused("Redis cluster with IAM auth on again: no AUTH", caddr, "", "", "NOAUTH")
	refused("Redis cluster with IAM auth on again: a made-up token", caddr, "", "cbl_madeupmadeupmadeupmadeupmadeupmade", wrongpass)
	taken("Redis cluster with IAM auth on again: the granted token", caddr, "", redisUser.token)
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{caddr}, Password: redisUser.token})
	defer rc.Close()
	if err := rc.Set(ctx, "iam", "cluster", 0).Err(); err != nil {
		t.Errorf("a ClusterClient with the granted token: SET = %v", err)
	}
}

// projectRoles changes member's roles on project through the official
// Resource Manager client: change(role, true) grants role, false takes it
// back, retrying a concurrent change's ABORTED.
func projectRoles(t *testing.T, h *Harness, project, member string) func(role string, add bool) {
	t.Helper()
	ctx := h.Context()
	rm, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	change := func(role string, add bool) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			pol, err := rm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
			if err != nil {
				t.Fatalf("GetIamPolicy: %v", err)
			}
			var bindings []*iampb.Binding
			found := false
			for _, b := range pol.GetBindings() {
				if b.GetRole() == role {
					found = true
					var ms []string
					for _, m := range b.GetMembers() {
						if m != member {
							ms = append(ms, m)
						}
					}
					if add {
						ms = append(ms, member)
					}
					if len(ms) == 0 {
						continue
					}
					b = &iampb.Binding{Role: role, Members: ms}
				}
				bindings = append(bindings, b)
			}
			if !found && add {
				bindings = append(bindings, &iampb.Binding{Role: role, Members: []string{member}})
			}
			pol.Bindings = bindings
			_, err = rm.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project, Policy: pol})
			if err == nil {
				return
			}
			if status.Code(err) != codes.Aborted || attempt > 20 {
				t.Fatalf("SetIamPolicy: %v", err)
			}
		}
	}
	return change
}
