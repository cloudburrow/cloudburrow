//go:build compat

package compat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// customName is a custom constraint name of this run: custom constraints
// are the organization's, which every test shares.
func customName(h *Harness, base string) string {
	var digits strings.Builder
	for _, r := range h.Project() {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' {
			digits.WriteRune(r)
		}
	}
	s := digits.String()
	if len(s) > 20 {
		s = s[len(s)-20:]
	}
	return "custom." + base + s
}

// createCustom creates a custom constraint and deletes it at the end.
func createCustom(t *testing.T, h *Harness, c *orgpolicy.Client, short, resourceType, condition string, action orgpolicypb.CustomConstraint_ActionType, desc string, methods ...orgpolicypb.CustomConstraint_MethodType) {
	t.Helper()
	name := localOrg + "/customConstraints/" + short
	if _, err := c.CreateCustomConstraint(h.Context(), &orgpolicypb.CreateCustomConstraintRequest{Parent: localOrg, CustomConstraint: &orgpolicypb.CustomConstraint{
		Name: name, ResourceTypes: []string{resourceType}, MethodTypes: methods, Condition: condition, ActionType: action,
		DisplayName: short, Description: desc}}); err != nil {
		t.Fatalf("CreateCustomConstraint %s: %v", short, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteCustomConstraint(context.Background(), &orgpolicypb.DeleteCustomConstraintRequest{Name: name})
	})
}

var (
	ccCreate = orgpolicypb.CustomConstraint_CREATE
	ccUpdate = orgpolicypb.CustomConstraint_UPDATE
	ccAllow  = orgpolicypb.CustomConstraint_ALLOW
	ccDeny   = orgpolicypb.CustomConstraint_DENY
)

func customMessage(short, desc string) string {
	return `Operation denied by custom org policies: ["customConstraints/` + short + `":"` + desc + `"]`
}

// covers: storage.buckets.insert, storage.buckets.patch, google.cloud.orgpolicy.v2.OrgPolicy/CreateCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/DeleteCustomConstraint
//
// TestOrgPolicyCustomConstraintsStorage (#1891), with the official Cloud
// Storage and orgpolicy/apiv2 clients: a custom constraint on
// storage.googleapis.com/Bucket enforced on a project refuses a bucket
// it does not ALLOW (versioning off), 412 "orgpolicy:projects/_/buckets/B
// violates customConstraints/custom.x." with the description, creates
// one it allows, and refuses an update that would violate it; a DENY
// constraint refuses the storage class it names. A project without the
// policy is unaffected. Deleting the constraint deletes its policy.
func TestOrgPolicyCustomConstraintsStorage(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "occ-")
	versioned := customName(h, "versioned")
	nearline := customName(h, "nonearline")
	createCustom(t, h, c, versioned, "storage.googleapis.com/Bucket", "resource.versioning.enabled == true", ccAllow,
		"Buckets must have versioning enabled.", ccCreate, ccUpdate)
	createCustom(t, h, c, nearline, "storage.googleapis.com/Bucket", "resource.storageClass == 'NEARLINE'", ccDeny,
		"No Nearline buckets.", ccCreate)
	setOrgPolicy(t, h, c, "projects/"+project, versioned, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, nearline, opEnforce())
	expect412 := func(what string, err error, msg string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 412 || ge.Message != msg {
			t.Errorf("%s = %v, want 412 %s", what, err, msg)
		}
	}
	plain := project + "-plain"
	expect412("a bucket without versioning", sc.Bucket(plain).Create(ctx, project, &storage.BucketAttrs{Location: "US"}),
		"orgpolicy:projects/_/buckets/"+plain+" violates customConstraints/"+versioned+". Details: Buckets must have versioning enabled.")
	cold := project + "-cold"
	expect412("a Nearline bucket", sc.Bucket(cold).Create(ctx, project, &storage.BucketAttrs{Location: "US", VersioningEnabled: true, StorageClass: "NEARLINE"}),
		"orgpolicy:projects/_/buckets/"+cold+" violates customConstraints/"+nearline+". Details: No Nearline buckets.")
	b := sc.Bucket(project + "-ok")
	if err := b.Create(ctx, project, &storage.BucketAttrs{Location: "US", VersioningEnabled: true}); err != nil {
		t.Fatalf("a versioned bucket: %v", err)
	}
	t.Cleanup(func() { _ = b.Delete(context.Background()) })
	_, err := b.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: false})
	expect412("turning versioning off", err,
		"orgpolicy:projects/_/buckets/"+project+"-ok violates customConstraints/"+versioned+". Details: Buckets must have versioning enabled.")
	if _, err := b.Update(ctx, storage.BucketAttrsToUpdate{StorageClass: "STANDARD"}); err != nil {
		t.Errorf("an update that keeps versioning: %v", err)
	}

	free := orgPolicyProject(t, h, pc, localOrg, "ocf-")
	fb := sc.Bucket(free + "-plain")
	if err := fb.Create(ctx, free, &storage.BucketAttrs{Location: "US"}); err != nil {
		t.Fatalf("a bucket in a project without the policy: %v", err)
	}
	t.Cleanup(func() { _ = fb.Delete(context.Background()) })

	// "If you delete a custom constraint, any policies that have been
	// created using that constraint are deleted."
	if err := c.DeleteCustomConstraint(ctx, &orgpolicypb.DeleteCustomConstraintRequest{Name: localOrg + "/customConstraints/" + nearline}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: "projects/" + project + "/policies/" + nearline}); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted constraint's policy = %v, want NOT_FOUND", err)
	}
	cold2 := sc.Bucket(project + "-cold2")
	if err := cold2.Create(ctx, project, &storage.BucketAttrs{Location: "US", VersioningEnabled: true, StorageClass: "NEARLINE"}); err != nil {
		t.Errorf("a Nearline bucket after the constraint is deleted: %v", err)
	} else {
		t.Cleanup(func() { _ = cold2.Delete(context.Background()) })
	}
}

// covers: google.iam.admin.v1.IAM/CreateServiceAccount, google.iam.admin.v1.IAM/PatchServiceAccount, google.iam.admin.v1.IAM/CreateServiceAccountKey
//
// TestOrgPolicyCustomConstraintsIAM (#1891), with the official IAM admin
// client: a DENY constraint on iam.googleapis.com/ServiceAccount (the IAM
// page's example, resource.description.contains('INVALID_DESCRIPTION'))
// refuses creating, and patching to, such an account, FAILED_PRECONDITION
// "Operation denied by custom org policy: [...]"; one on
// iam.googleapis.com/ServiceAccountKey refuses a user-managed key.
func TestOrgPolicyCustomConstraintsIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	ep := h.Endpoint(EnvResourceManager)
	c := orgPolicyClients(t, h)["rest"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(ep))
	project := orgPolicyProject(t, h, pc, localOrg, "oci-")
	adm, err := admin.NewIamClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	deny := customName(h, "denysa")
	keys := customName(h, "nokeys")
	createCustom(t, h, c, deny, "iam.googleapis.com/ServiceAccount", "resource.description.contains('INVALID_DESCRIPTION')", ccDeny,
		"Do not allow service account with INVALID_DESCRIPTION to be created.", ccCreate, ccUpdate)
	createCustom(t, h, c, keys, "iam.googleapis.com/ServiceAccountKey", "resource.keyType == 'USER_MANAGED'", ccDeny,
		"No user-managed keys.", ccCreate)
	setOrgPolicy(t, h, c, "projects/"+project, deny, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, keys, opEnforce())
	want := `Operation denied by custom org policy: ["customConstraints/` + deny + `":"Do not allow service account with INVALID_DESCRIPTION to be created."]`
	_, err = adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "ccbad1",
		ServiceAccount: &adminpb.ServiceAccount{Description: "an INVALID_DESCRIPTION here"}})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("CreateServiceAccount violating = %v, want FAILED_PRECONDITION %s", err, want)
	}
	sa, err := adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "ccgood1",
		ServiceAccount: &adminpb.ServiceAccount{Description: "fine"}})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	_, err = adm.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(),
		Description: "now INVALID_DESCRIPTION"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("PatchServiceAccount violating = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := adm.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(),
		DisplayName: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil {
		t.Errorf("PatchServiceAccount of the display name: %v", err)
	}
	_, err = adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || !strings.Contains(st.Message(), keys) {
		t.Errorf("CreateServiceAccountKey under the key constraint = %v, want FAILED_PRECONDITION naming %s", err, keys)
	}
}

// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Subscriber/CreateSubscription
//
// TestOrgPolicyCustomConstraintsPubSub (#1891), with the official Pub/Sub
// client: the Pub/Sub page's example (ALLOW
// has(resource.messageRetentionDuration) on topics, CREATE and UPDATE)
// refuses a topic without a retention, FAILED_PRECONDITION "Operation
// denied by custom org policies: [...]", creates one with it, and
// refuses an update that clears it; a DENY constraint on subscriptions
// (resource.ackDeadlineSeconds < 30) refuses a 10-second deadline.
func TestOrgPolicyCustomConstraintsPubSub(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocp-")
	retention := customName(h, "retention")
	deadline := customName(h, "deadline")
	createCustom(t, h, c, retention, "pubsub.googleapis.com/Topic", "has(resource.messageRetentionDuration)", ccAllow,
		"All topics must set a message retention duration.", ccCreate, ccUpdate)
	createCustom(t, h, c, deadline, "pubsub.googleapis.com/Subscription", "resource.ackDeadlineSeconds < 30", ccDeny,
		"Ack deadlines of at least 30 seconds.", ccCreate)
	setOrgPolicy(t, h, c, "projects/"+project, retention, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, deadline, opEnforce())
	name := "projects/" + project + "/topics/cc-topic"
	_, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != customMessage(retention, "All topics must set a message retention duration.") {
		t.Errorf("a topic without retention = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name, MessageRetentionDuration: durationpb.New(time.Hour)}); err != nil {
		t.Fatalf("a topic with retention: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: name})
	})
	_, err = ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("clearing the retention = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: name, Labels: map[string]string{"a": "b"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Errorf("an update of the labels: %v", err)
	}
	sub := "projects/" + project + "/subscriptions/cc-sub"
	_, err = ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: name, AckDeadlineSeconds: 10})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != customMessage(deadline, "Ack deadlines of at least 30 seconds.") {
		t.Errorf("a 10-second subscription = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: name, AckDeadlineSeconds: 60}); err != nil {
		t.Errorf("a 60-second subscription: %v", err)
	} else {
		t.Cleanup(func() {
			_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
		})
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
//
// TestOrgPolicyCustomConstraintsSecretManager (#1891): the Secret Manager
// page's example (DENY resource.versionDestroyTtl < duration('30h'))
// refuses a secret with a 25-hour version destroy TTL, and one with
// none (the condition cannot be evaluated), FAILED_PRECONDITION
// "Operation denied by custom org policies: [...]"; a 48-hour one is
// created, and lowering it is refused.
func TestOrgPolicyCustomConstraintsSecretManager(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	smc := secretsClient(t, h)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocs-")
	ttl := customName(h, "ttl")
	desc := "New secrets must have version destroy TTL values of at least 30 hours."
	createCustom(t, h, c, ttl, "secretmanager.googleapis.com/Secret", "resource.versionDestroyTtl < duration('30h')", ccDeny, desc, ccCreate, ccUpdate)
	setOrgPolicy(t, h, c, "projects/"+project, ttl, opEnforce())
	auto := &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
	for id, d := range map[string]*durationpb.Duration{"cc-short": durationpb.New(25 * time.Hour), "cc-none": nil} {
		_, err := smc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: id,
			Secret: &secretmanagerpb.Secret{Replication: auto, VersionDestroyTtl: d}})
		if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != customMessage(ttl, desc) {
			t.Errorf("secret %s = %v, want FAILED_PRECONDITION", id, err)
		}
	}
	s, err := smc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "cc-long",
		Secret: &secretmanagerpb.Secret{Replication: auto, VersionDestroyTtl: durationpb.New(48 * time.Hour)}})
	if err != nil {
		t.Fatalf("a 48-hour secret: %v", err)
	}
	s.VersionDestroyTtl = durationpb.New(26 * time.Hour)
	_, err = smc.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_destroy_ttl"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("lowering the TTL = %v, want FAILED_PRECONDITION", err)
	}
}

// covers: google.cloud.orgpolicy.v2.OrgPolicy/CreateCustomConstraint
//
// TestOrgPolicyCustomConstraintRefusals (#1891): a condition that is not
// CEL is INVALID_ARGUMENT "Custom constraint condition [...] is invalid",
// over gRPC and JSON; a 71-character name is refused, a 70-character one
// created.
func TestOrgPolicyCustomConstraintRefusals(t *testing.T) {
	h := New(t)
	for transport, c := range orgPolicyClients(t, h) {
		short := customName(h, "bad"+transport)
		_, err := c.CreateCustomConstraint(h.Context(), &orgpolicypb.CreateCustomConstraintRequest{Parent: localOrg, CustomConstraint: &orgpolicypb.CustomConstraint{
			Name: localOrg + "/customConstraints/" + short, ResourceTypes: []string{"storage.googleapis.com/Bucket"},
			MethodTypes: []orgpolicypb.CustomConstraint_MethodType{ccCreate}, Condition: "resource.name ==", ActionType: ccAllow}})
		if opCode(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Custom constraint condition [resource.name ==] is invalid") {
			t.Errorf("%s: a malformed condition = %v, want INVALID_ARGUMENT", transport, err)
		}
	}
	c := orgPolicyClients(t, h)["grpc"]
	long := strings.Repeat("a", 70)
	tooLong := long + "b"
	if _, err := c.CreateCustomConstraint(h.Context(), &orgpolicypb.CreateCustomConstraintRequest{Parent: localOrg, CustomConstraint: &orgpolicypb.CustomConstraint{
		Name: localOrg + "/customConstraints/custom." + tooLong, ResourceTypes: []string{"storage.googleapis.com/Bucket"},
		MethodTypes: []orgpolicypb.CustomConstraint_MethodType{ccCreate}, Condition: "true", ActionType: ccAllow}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a 71-character name = %v, want INVALID_ARGUMENT", err)
	}
	short := customName(h, "x")
	short = short + strings.Repeat("a", 70-len(strings.TrimPrefix(short, "custom.")))
	createCustom(t, h, c, short, "storage.googleapis.com/Bucket", "true", ccAllow, "", ccCreate)
}

// TestGcloudOrgPoliciesCustomConstraints (#1891): gcloud org-policies
// set-custom-constraint, describe-custom-constraint,
// list-custom-constraints and delete-custom-constraint, with a policy that
// enforces the constraint set by set-policy.
func TestGcloudOrgPoliciesCustomConstraints(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	g := newGcloudSession(t, h)
	project := iamProject(t, h, "ocg-")
	short := customName(h, "gcloud")
	dir := t.TempDir()
	file := filepath.Join(dir, "constraint.yaml")
	body := "name: " + localOrg + "/customConstraints/" + short + "\nresourceTypes:\n- storage.googleapis.com/Bucket\nmethodTypes:\n- CREATE\n" +
		"condition: \"resource.name.matches('^[a-z-]+$')\"\nactionType: ALLOW\ndisplayName: Letters only\ndescription: Newly created buckets must have a name that matches the specified regular expression.\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("org-policies", "set-custom-constraint", file)
	t.Cleanup(func() {
		_, _ = g.run(nil, "org-policies", "delete-custom-constraint", short, "--organization="+strings.TrimPrefix(localOrg, "organizations/"), "--quiet")
	})
	out := g.must("org-policies", "describe-custom-constraint", short, "--organization="+strings.TrimPrefix(localOrg, "organizations/"), "--format=json")
	if !strings.Contains(out, "resource.name.matches") || !strings.Contains(out, "ALLOW") {
		t.Errorf("describe-custom-constraint = %s", out)
	}
	out = g.must("org-policies", "list-custom-constraints", "--organization="+strings.TrimPrefix(localOrg, "organizations/"), "--format=json")
	if !strings.Contains(out, short) {
		t.Errorf("list-custom-constraints = %s", out)
	}
	pfile := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(pfile, []byte("name: projects/"+project+"/policies/"+short+"\nspec:\n  rules:\n  - enforce: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("org-policies", "set-policy", pfile)
	out = g.must("org-policies", "describe", short, "--project="+project, "--effective", "--format=json")
	if !strings.Contains(out, "enforce") {
		t.Errorf("describe --effective = %s", out)
	}
	g.must("org-policies", "delete-custom-constraint", short, "--organization="+strings.TrimPrefix(localOrg, "organizations/"), "--quiet")
	if out, err := g.run(nil, "org-policies", "describe", short, "--project="+project); err == nil {
		t.Errorf("the policy outlived its constraint: %s", out)
	}
}
