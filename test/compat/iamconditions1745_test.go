//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// IAM Conditions on other services' own policies (#1745), as Google's
// "Services that allow conditions" lists them
// (docs.cloud.google.com/iam/docs/resource-types-with-conditional-roles)
// and its "Policy versions" page documents the version rules.

// conditionalPolicy checks a resource of a service that allows conditions,
// through its official client: a conditional binding in a version 1 policy
// is INVALID_ARGUMENT; one in a version 3 policy is stored and read back
// with its condition at version 3; a version 1 read renders the role
// ROLE_withcond_HASH without it, and a version 1 set of that rendering
// keeps it; iam.googleapis.com/modifiedGrantsByRole, which Google accepts
// on projects, folders and organizations only, is INVALID_ARGUMENT.
func conditionalPolicy(t *testing.T, ctx context.Context, what string, c gaxIAMClient, resource, role, expression string) {
	t.Helper()
	cond := &expr.Expr{Title: "scoped", Description: "#1745", Expression: expression}
	binding := func(c *expr.Expr) []*iampb.Binding {
		return []*iampb.Binding{{Role: role, Members: []string{"user:ada@example.com"}, Condition: c}}
	}
	set := func(p *iampb.Policy) (*iampb.Policy, error) {
		return c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: p})
	}
	if _, err := set(&iampb.Policy{Version: 1, Bindings: binding(cond)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("%s: a conditional binding in a version 1 policy = %v; want INVALID_ARGUMENT", what, err)
	}
	mgr := &expr.Expr{Title: "t", Expression: `api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(['roles/viewer'])`}
	if _, err := set(&iampb.Policy{Version: 3, Bindings: binding(mgr)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("%s: modifiedGrantsByRole = %v; want INVALID_ARGUMENT", what, err)
	}
	got, err := set(&iampb.Policy{Version: 3, Bindings: binding(cond)})
	if err != nil || got.GetVersion() != 3 || got.GetBindings()[0].GetCondition().GetExpression() != expression {
		t.Fatalf("%s: SetIamPolicy version 3 = %v, %v", what, got, err)
	}
	v3, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || v3.GetVersion() != 3 || len(v3.GetBindings()) != 1 {
		t.Fatalf("%s: GetIamPolicy version 3 = %v, %v", what, v3, err)
	}
	if g := v3.GetBindings()[0].GetCondition(); g.GetTitle() != cond.Title || g.GetDescription() != cond.Description || g.GetExpression() != expression {
		t.Errorf("%s: the condition read back = %v", what, g)
	}
	v1, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil || v1.GetVersion() != 1 || len(v1.GetBindings()) != 1 ||
		!strings.HasPrefix(v1.GetBindings()[0].GetRole(), role+"_withcond_") || v1.GetBindings()[0].GetCondition() != nil {
		t.Fatalf("%s: GetIamPolicy version 1 = %v, %v; want ROLE_withcond_HASH without the condition", what, v1, err)
	}
	if _, err := set(v1); err != nil {
		t.Fatalf("%s: a version 1 write back = %v", what, err)
	}
	again, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || len(again.GetBindings()) != 1 || again.GetBindings()[0].GetCondition().GetExpression() != expression {
		t.Errorf("%s: after a version 1 write back = %v, %v; want the condition kept", what, again, err)
	}
}

// TestIAMConditionsOnServicePolicies (#1745): Secret Manager's secrets and
// Cloud KMS's key rings and keys, through their official clients, take
// conditional role bindings.
// covers: google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy
func TestIAMConditionsOnServicePolicies(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "cond-1745",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name})
	})
	conditionalPolicy(t, ctx, "secret", sc, sec.Name, "roles/secretmanager.secretAccessor",
		`resource.type == "secretmanager.googleapis.com/SecretVersion" && resource.name.endsWith("/versions/1")`)

	g := kmsClients(t, h)["grpc"]
	ring, err := g.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "cond-1745"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := g.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	conditionalPolicy(t, ctx, "key ring", g, ring.GetName(), "roles/cloudkms.cryptoKeyDecrypter",
		`resource.name.startsWith("`+ring.GetName()+`/cryptoKeys/prod-") && request.time < timestamp("2099-01-01T00:00:00Z")`)
	conditionalPolicy(t, ctx, "crypto key", g, key.GetName(), "roles/cloudkms.cryptoKeyEncrypterDecrypter",
		`resource.matchTag("`+h.Project()+`/env", "prod")`)
}

// TestSpannerIAMConditions (#1745): Spanner instances and databases take
// conditional role bindings, through the official admin clients.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/SetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/SetIamPolicy
func TestSpannerIAMConditions(t *testing.T) {
	f := newSpannerAdmin(t, "cb-cond", "conddb") // skips without CLOUDBURROW_TEST_SPANNER
	conditionalPolicy(t, f.ctx, "instance", f.inst, f.instance, "roles/spanner.viewer", `resource.type == "spanner.googleapis.com/Database"`)
	conditionalPolicy(t, f.ctx, "database", f.db, f.database, "roles/spanner.databaseReader", `request.time.getHours("Europe/Berlin") < 18`)
}

// TestIAMConditionAttributes (#1745): every attribute Google's attribute
// reference documents for an allow policy's role binding compiles in a
// project's policy (resource tags, request.path and .host,
// request.auth.access_levels, destination.ip and .port, api.getAttribute
// with hasOnly and with a map default (#1748), the compute.* functions),
// and principal.type and
// principal.subject, which the reference documents for Principal Access
// Boundary policies only, are INVALID_ARGUMENT.
// covers: google.cloud.resourcemanager.v3.Projects/SetIamPolicy
func TestIAMConditionAttributes(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	id := iamProject(t, h, "iamattr-")
	c, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	set := func(e string) error {
		_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + id, Policy: &iampb.Policy{Version: 3,
			Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:ada@example.com"}, Condition: &expr.Expr{Title: "t", Expression: e}}}}})
		return err
	}
	for _, e := range []string{
		`resource.matchTag("123456789012/env", "prod")`,
		`resource.matchTagId("tagKeys/123", "tagValues/456") || resource.hasTagKey("123456789012/env") || resource.hasTagKeyId("tagKeys/123")`,
		`request.path.startsWith("/admin") && request.host.endsWith(".example.com")`,
		`"accessPolicies/1/accessLevels/corp" in request.auth.access_levels`,
		`destination.ip == "10.0.0.5" && destination.port < 3001`,
		`api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(['roles/pubsub.editor', 'roles/pubsub.publisher'])`,
		`!compute.isForwardingRuleCreationOperation() || (compute.isForwardingRuleCreationOperation() && compute.matchLoadBalancingSchemes(['INTERNAL', 'INTERNAL_MANAGED', 'INTERNAL_SELF_MANAGED']))`,
		// #1748: the map default, as Google's Memory Bank page writes it.
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', {}) == {'userId': 'userA'}`,
		`'user_id' in api.getAttribute('aiplatform.googleapis.com/memoryScope', {}) && api.getAttribute('aiplatform.googleapis.com/memoryScope', {})['user_id'] == 'userA'`,
		`api.getAttribute('aiplatform.googleapis.com/memoryScope', {})['userId'] in ['userA', 'userB']`,
	} {
		if err := set(e); err != nil {
			t.Errorf("%s: SetIamPolicy = %v", e, err)
		}
	}
	for _, e := range []string{
		`principal.type == "iam.googleapis.com/ServiceAccount"`,
		`principal.subject.endsWith("@example.com")`,
		`request.unknown == "x"`,
	} {
		if err := set(e); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: SetIamPolicy = %v; want INVALID_ARGUMENT", e, err)
		}
	}
}

// consoleConditionalGrant is Grant access on a Permissions tab with a
// condition, and what the console answered.
func consoleConditionalGrant(t *testing.T, addr, service, project string, path []string, etag, role, member, title, expression string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Path": path, "Etag": etag,
		"Grant": map[string]any{"Role": role, "Members": []string{member},
			"Condition": map[string]any{"title": title, "expression": expression}}})
	code, body := consoleDo(t, addr, http.MethodPost, "/api/permissions/"+service+"?project="+url.QueryEscape(project), string(b))
	var e struct{ Error string }
	_ = json.Unmarshal([]byte(body), &e)
	return code, e.Error
}

// TestConsoleIAMConditionsOnServicePolicies (#1745): Grant access with a
// condition from a secret's Permissions tab and from a bucket's (with
// uniform bucket-level access) stores the conditional binding, as the
// official clients read it back at version 3; on a bucket without uniform
// bucket-level access the service's refusal is shown.
// covers: google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, storage.buckets.setIamPolicy
func TestConsoleIAMConditionsOnServicePolicies(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := consoleAddr(t, h)
	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "console-cond-1745",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: sec.GetName()})
	})
	const ada = "user:ada@example.com"
	e := `request.time < timestamp("2099-01-01T00:00:00Z")`
	tab := readConsolePermissions(t, addr, "secrets", h.Project(), []string{sec.GetName()})
	if code, msg := consoleConditionalGrant(t, addr, "secrets", h.Project(), []string{sec.GetName()}, tab.Etag, "roles/secretmanager.secretAccessor", ada, "until 2099", e); code != http.StatusOK {
		t.Fatalf("Grant access with a condition = %d: %s", code, msg)
	}
	got, err := sc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sec.GetName(), Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetCondition().GetTitle() != "until 2099" || got.GetBindings()[0].GetCondition().GetExpression() != e {
		t.Errorf("the secret's policy after Grant access = %v, %v", got, err)
	}

	c := storageClient(t, h)
	bh := c.Bucket(h.Project() + "-console-cond")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })
	be := `resource.name.startsWith("projects/_/buckets/` + bh.BucketName() + `/objects/public/")`
	tab = readConsolePermissions(t, addr, "storage", h.Project(), []string{bh.BucketName()})
	if code, msg := consoleConditionalGrant(t, addr, "storage", h.Project(), []string{bh.BucketName()}, tab.Etag, "roles/storage.objectViewer", ada, "public", be); code != http.StatusBadRequest || !strings.Contains(msg, "uniformBucketLevelAccess") {
		t.Errorf("Grant access with a condition without uniform access = %d %q; want the service's 400", code, msg)
	}
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	tab = readConsolePermissions(t, addr, "storage", h.Project(), []string{bh.BucketName()})
	if code, msg := consoleConditionalGrant(t, addr, "storage", h.Project(), []string{bh.BucketName()}, tab.Etag, "roles/storage.objectViewer", ada, "public", be); code != http.StatusOK {
		t.Fatalf("Grant access with a condition = %d: %s", code, msg)
	}
	p3, err := bh.IAM().V3().Policy(ctx)
	if err != nil || len(p3.Bindings) != 1 || p3.Bindings[0].GetCondition().GetExpression() != be {
		t.Errorf("the bucket's policy after Grant access = %v, %v", p3, err)
	}
}
