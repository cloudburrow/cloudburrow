//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// valkeyAs is a client of inst's primary endpoint signed in as user.
func valkeyAs(t *testing.T, inst *memorystorepb.Instance, user, pass string) *redis.Client {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY),
		Username: user, Password: pass, MaxRetries: -1})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// valkeyAuthEventually waits until user's sign-in on inst succeeds (ok) or
// fails (!ok).
func valkeyAuthEventually(t *testing.T, ctx context.Context, inst *memorystorepb.Instance, user, pass string, ok bool) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		rc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY),
			Username: user, Password: pass, MaxRetries: -1})
		err = rc.Ping(ctx).Err()
		_ = rc.Close()
		if (err == nil) == ok {
			return
		}
	}
	t.Fatalf("sign-in as %s = %v; want success %v", user, err, ok)
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateAclPolicy, google.cloud.memorystore.v1.Memorystore/GetAclPolicy, google.cloud.memorystore.v1.Memorystore/ListAclPolicies, google.cloud.memorystore.v1.Memorystore/UpdateAclPolicy, google.cloud.memorystore.v1.Memorystore/DeleteAclPolicy, google.cloud.memorystore.v1.Memorystore/GetAclPolicyRevision, google.cloud.memorystore.v1.Memorystore/ListAclPolicyRevisions
//
// TestMemorystoreValkeyACLPolicies (#1578): the official Memorystore client
// makes an ACL policy of one rule, gets and lists it, and creates an
// instance attached to it; the rule's user signs in to the instance and
// reads, and is refused a write its rule does not allow. An update adds a
// user, revision 2, which signs in; both revisions are listed. A
// policy that is attached cannot be deleted; detached (acl_policy cleared
// through UpdateInstance), its users are gone from the instance and it is
// deleted.
func TestMemorystoreValkeyACLPolicies(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	parent := "projects/" + project + "/locations/us-central1"
	pid := fmt.Sprintf("acl-%d", sfx)
	name := parent + "/aclPolicies/" + pid
	reader := &memorystorepb.AclRule{Username: "reader", Rule: "on >readerpass ~app:* +@read +ping"}
	p, err := c.CreateAclPolicy(ctx, &memorystorepb.CreateAclPolicyRequest{Parent: parent, AclPolicyId: pid,
		AclPolicy: &memorystorepb.AclPolicy{Rules: []*memorystorepb.AclRule{reader}}})
	if err != nil {
		t.Fatalf("CreateAclPolicy: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteAclPolicy(context.Background(), &memorystorepb.DeleteAclPolicyRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if p.GetName() != name || p.GetState() != memorystorepb.AclPolicy_ACTIVE || len(p.GetRules()) != 1 || p.GetEtag() == "" {
		t.Errorf("CreateAclPolicy = %v", p)
	}
	if _, err := c.CreateAclPolicy(ctx, &memorystorepb.CreateAclPolicyRequest{Parent: parent, AclPolicyId: pid,
		AclPolicy: &memorystorepb.AclPolicy{Rules: []*memorystorepb.AclRule{reader}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateAclPolicy again = %v; want ALREADY_EXISTS", err)
	}
	found := false
	for it := c.ListAclPolicies(ctx, &memorystorepb.ListAclPoliciesRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.GetName() == name
	}
	if !found {
		t.Errorf("ListAclPolicies has no %s", name)
	}

	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("acl-i-%d", sfx), func(in *memorystorepb.Instance) {
		in.AclPolicy = proto.String(name)
	})
	if inst.GetAclPolicy() != name || inst.GetAclPolicyInfo().GetAppliedAclPolicy() != name ||
		inst.GetAclPolicyInfo().GetAppliedAclPolicyRevisionNumber() != 1 {
		t.Errorf("the instance's acl_policy %q, acl_policy_info %v", inst.GetAclPolicy(), inst.GetAclPolicyInfo())
	}
	if got, err := c.GetAclPolicy(ctx, &memorystorepb.GetAclPolicyRequest{Name: name}); err != nil ||
		len(got.GetInstanceAclPolicyAttachments()) != 1 || got.GetInstanceAclPolicyAttachments()[0].GetInstance() != inst.GetName() {
		t.Errorf("GetAclPolicy = %v, %v; want it attached to %s", got, err, inst.GetName())
	}
	valkeyAuthEventually(t, ctx, inst, "reader", "readerpass", true)
	rc := valkeyAs(t, inst, "reader", "readerpass")
	if err := rc.Get(ctx, "app:k").Err(); err != nil && err != redis.Nil {
		t.Errorf("GET as reader: %v", err)
	}
	if err := rc.Set(ctx, "app:k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Errorf("SET as reader = %v; want NOPERM", err)
	}

	writer := &memorystorepb.AclRule{Username: "writer", Rule: "on >writerpass ~app:* +@all"}
	uop, err := c.UpdateAclPolicy(ctx, &memorystorepb.UpdateAclPolicyRequest{
		AclPolicy:  &memorystorepb.AclPolicy{Name: name, Rules: []*memorystorepb.AclRule{reader, writer}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rules"}}})
	if err != nil {
		t.Fatalf("UpdateAclPolicy: %v", err)
	}
	up, err := uop.Wait(ctx)
	if err != nil || len(up.GetRules()) != 2 {
		t.Fatalf("UpdateAclPolicy operation = %v, %v", up, err)
	}
	wc := valkeyAs(t, inst, "writer", "writerpass")
	if err := wc.Set(ctx, "app:k", "v", 0).Err(); err != nil {
		t.Errorf("SET as writer after the update: %v", err)
	}
	rev, err := c.GetAclPolicyRevision(ctx, &memorystorepb.GetAclPolicyRevisionRequest{Name: name + "/revisions/2"})
	if err != nil || rev.GetRevisionNumber() != 2 || len(rev.GetSnapshot().GetRules()) != 2 ||
		len(rev.GetAttachedInstances()) != 1 || rev.GetAttachedInstances()[0] != inst.GetName() {
		t.Errorf("GetAclPolicyRevision(2) = %v, %v", rev, err)
	}
	var nums []int64
	for it := c.ListAclPolicyRevisions(ctx, &memorystorepb.ListAclPolicyRevisionsRequest{Parent: name}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		nums = append(nums, x.GetRevisionNumber())
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	if fmt.Sprint(nums) != "[1 2]" {
		t.Errorf("ListAclPolicyRevisions numbers = %v; want revisions 1 and 2", nums)
	}
	if _, err := c.GetAclPolicyRevision(ctx, &memorystorepb.GetAclPolicyRevisionRequest{Name: name + "/revisions/3"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAclPolicyRevision(3) = %v; want NOT_FOUND", err)
	}

	if _, err := c.DeleteAclPolicy(ctx, &memorystorepb.DeleteAclPolicyRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteAclPolicy of an attached policy = %v; want FAILED_PRECONDITION", err)
	}
	iop, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{
		Instance:   &memorystorepb.Instance{Name: inst.GetName(), AclPolicy: proto.String("")},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"acl_policy"}}})
	if err != nil {
		t.Fatalf("UpdateInstance acl_policy cleared: %v", err)
	}
	got, err := waitValkeyOp(t, h, iop)
	if err != nil || got.GetAclPolicy() != "" {
		t.Fatalf("UpdateInstance acl_policy cleared = %v, %v", got.GetAclPolicy(), err)
	}
	valkeyAuthEventually(t, ctx, inst, "writer", "writerpass", false)
	dop, err := c.DeleteAclPolicy(ctx, &memorystorepb.DeleteAclPolicyRequest{Name: name})
	if err == nil {
		err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeleteAclPolicy of a detached policy: %v", err)
	}
	if _, err := c.GetAclPolicy(ctx, &memorystorepb.GetAclPolicyRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAclPolicy after delete = %v; want NOT_FOUND", err)
	}
}
