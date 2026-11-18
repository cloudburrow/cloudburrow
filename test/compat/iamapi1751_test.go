//go:build compat

package compat

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2/jws"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// accountCerts reads the account's public certificates (key ID to PEM)
// from the metadata port's x509 URL, as a verifier does.
func accountCerts(t *testing.T, h *Harness, email string) map[string]*rsa.PublicKey {
	t.Helper()
	meta := h.Endpoint(EnvMetadata)
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	resp, err := http.Get(meta + "/service_accounts/v1/metadata/x509/" + email)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var certs map[string]string
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&certs) != nil {
		t.Fatalf("x509 URL = %d", resp.StatusCode)
	}
	out := map[string]*rsa.PublicKey{}
	for kid, c := range certs {
		b, _ := pem.Decode([]byte(c))
		if b == nil {
			t.Fatal("not a PEM certificate")
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out[kid] = cert.PublicKey.(*rsa.PublicKey)
	}
	return out
}

// covers: google.iam.admin.v1.IAM/SignBlob, google.iam.admin.v1.IAM/SignJwt
//
// TestIAMLegacySignBlobAndSignJwt (#1751): the IAM API's deprecated
// projects.serviceAccounts.signBlob and signJwt, through the official
// admin client. signBlob's signature is RSASSA-PKCS1-v1_5 over SHA-256 by
// the system-managed key keyId names, which the account's x509 URL serves;
// signJwt's token verifies with golang.org/x/oauth2/jws against the same
// certificates. A payload without exp gets one 1 hour ahead; an exp in the
// past or more than 12 hours ahead is INVALID_ARGUMENT, as Google's
// reference says; a disabled account is FAILED_PRECONDITION and a deleted
// one NOT_FOUND.
func TestIAMLegacySignBlobAndSignJwt(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "lsg-")
	sa := newServiceAccount(t, h, project, "legacy")

	blob := []byte("cloudburrow legacy signBlob")
	sb, err := adm.SignBlob(ctx, &adminpb.SignBlobRequest{Name: sa.GetName(), BytesToSign: blob})
	if err != nil {
		t.Fatalf("SignBlob: %v", err)
	}
	certs := accountCerts(t, h, sa.GetEmail())
	pub := certs[sb.GetKeyId()]
	if pub == nil {
		t.Fatalf("SignBlob's key %q is not among the account's certificates", sb.GetKeyId())
	}
	sum := sha256.Sum256(blob)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sb.GetSignature()); err != nil {
		t.Errorf("SignBlob's signature does not verify: %v", err)
	}
	// By unique ID too, as the reference allows.
	if _, err := adm.SignBlob(ctx, &adminpb.SignBlobRequest{Name: "projects/" + project + "/serviceAccounts/" + sa.GetUniqueId(), BytesToSign: blob}); err != nil {
		t.Errorf("SignBlob by unique ID: %v", err)
	}

	sj, err := adm.SignJwt(ctx, &adminpb.SignJwtRequest{Name: sa.GetName(), Payload: fmt.Sprintf(`{"iss":%q,"aud":"https://svc.example"}`, sa.GetEmail())})
	if err != nil {
		t.Fatalf("SignJwt: %v", err)
	}
	pub = certs[sj.GetKeyId()]
	if pub == nil {
		t.Fatalf("SignJwt's key %q is not among the account's certificates", sj.GetKeyId())
	}
	if err := jws.Verify(sj.GetSignedJwt(), pub); err != nil {
		t.Errorf("jws.Verify: %v", err)
	}
	cs, err := jws.Decode(sj.GetSignedJwt())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(time.Unix(cs.Exp, 0)); d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("exp added %v ahead, want 1h", d)
	}
	if cs.Iss != sa.GetEmail() || cs.Aud != "https://svc.example" {
		t.Errorf("claims iss %q aud %q, not the payload's", cs.Iss, cs.Aud)
	}
	head, _ := base64.RawURLEncoding.DecodeString(strings.Split(sj.GetSignedJwt(), ".")[0])
	var hdr struct{ Alg, Typ, Kid string }
	if json.Unmarshal(head, &hdr) != nil || hdr.Alg != "RS256" || hdr.Kid != sj.GetKeyId() {
		t.Errorf("header %s, want RS256 with kid %s", head, sj.GetKeyId())
	}

	for name, exp := range map[string]int64{
		"past":     time.Now().Add(-time.Minute).Unix(),
		"over 12h": time.Now().Add(13 * time.Hour).Unix(),
	} {
		_, err := adm.SignJwt(ctx, &adminpb.SignJwtRequest{Name: sa.GetName(), Payload: fmt.Sprintf(`{"exp":%d}`, exp)})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("SignJwt exp %s = %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if _, err := adm.SignJwt(ctx, &adminpb.SignJwtRequest{Name: sa.GetName(), Payload: fmt.Sprintf(`{"exp":%d}`, time.Now().Add(11*time.Hour).Unix())}); err != nil {
		t.Errorf("SignJwt exp 11h ahead: %v", err)
	}
	if _, err := adm.SignJwt(ctx, &adminpb.SignJwtRequest{Name: sa.GetName(), Payload: "not json"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SignJwt with a non-JSON payload = %v, want INVALID_ARGUMENT", err)
	}

	if err := adm.DisableServiceAccount(ctx, &adminpb.DisableServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SignBlob(ctx, &adminpb.SignBlobRequest{Name: sa.GetName(), BytesToSign: blob}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SignBlob on a disabled account = %v, want FAILED_PRECONDITION", err)
	}
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SignJwt(ctx, &adminpb.SignJwtRequest{Name: sa.GetName(), Payload: "{}"}); status.Code(err) != codes.NotFound {
		t.Errorf("SignJwt on a deleted account = %v, want NOT_FOUND", err)
	}
}

// covers: google.iam.admin.v1.IAM/QueryAuditableServices
//
// TestIAMQueryAuditableServices (#1751): a project's, a folder's and the
// organization's auditable services are the services Google's audit
// logging page lists, sorted; a project that does not exist is refused.
func TestIAMQueryAuditableServices(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "qas-")
	for _, full := range []string{
		"//cloudresourcemanager.googleapis.com/projects/" + project,
		"//cloudresourcemanager.googleapis.com/" + localOrgRoles,
	} {
		resp, err := adm.QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{FullResourceName: full})
		if err != nil {
			t.Fatalf("QueryAuditableServices %s: %v", full, err)
		}
		var names []string
		for _, s := range resp.GetServices() {
			names = append(names, s.GetName())
		}
		for _, want := range []string{"iam.googleapis.com", "storage.googleapis.com", "secretmanager.googleapis.com", "cloudkms.googleapis.com", "pubsub.googleapis.com"} {
			if !slices.Contains(names, want) {
				t.Errorf("%s: %s missing from %d services", full, want, len(names))
			}
		}
		if !slices.IsSorted(names) {
			t.Errorf("%s: services are not sorted", full)
		}
	}
	_, err := adm.QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{FullResourceName: "//cloudresourcemanager.googleapis.com/projects/no-such-project-1751"})
	if c := status.Code(err); c != codes.NotFound && c != codes.PermissionDenied {
		t.Errorf("QueryAuditableServices on a missing project = %v, want NOT_FOUND or PERMISSION_DENIED", err)
	}
	if _, err := adm.QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("QueryAuditableServices without a name = %v, want INVALID_ARGUMENT", err)
	}
}

// TestIAMDeletedServiceAccountInOtherServicesPolicies (#1751): a deleted
// account's bindings read deleted:serviceAccount:EMAIL?uid=ID in the
// policies other services keep on their own resources (a secret's, a key
// ring's, a queue's), as in the hierarchy's (#1750), and serviceAccount:EMAIL
// again after undelete, through each service's official client.
func TestIAMDeletedServiceAccountInOtherServicesPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvKMS)
	h.Endpoint(EnvTasks)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "dso-")
	victim := newServiceAccount(t, h, project, "other-svc")
	live := "serviceAccount:" + victim.GetEmail()
	deleted := "deleted:serviceAccount:" + victim.GetEmail() + "?uid=" + victim.GetUniqueId()
	const role = "roles/viewer"

	sm := secretsClient(t, h)
	secret, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/" + h.Project(), SecretId: fmt.Sprintf("dso-%d", time.Now().UnixNano()%1e8),
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()})
	})
	kc := kmsClients(t, h)["grpc"]
	ring := kmsRing(t, h, kc, fmt.Sprintf("dso-%d", time.Now().UnixNano()%1e8))
	tc := tasksClient(t, h)
	queue, err := tc.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: location(h), Queue: &cloudtaskspb.Queue{Name: fmt.Sprintf("%s/queues/dso-%d", location(h), time.Now().UnixNano()%1e8)},
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() {
		_ = tc.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: queue.GetName()})
	})

	type policyAPI struct {
		get func(*iampb.GetIamPolicyRequest) (*iampb.Policy, error)
		set func(*iampb.SetIamPolicyRequest) (*iampb.Policy, error)
	}
	resources := map[string]policyAPI{
		secret.GetName(): {
			get: func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return sm.GetIamPolicy(ctx, r) },
			set: func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return sm.SetIamPolicy(ctx, r) },
		},
		ring: {
			get: func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return kc.GetIamPolicy(ctx, r) },
			set: func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return kc.SetIamPolicy(ctx, r) },
		},
		queue.GetName(): {
			get: func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return tc.GetIamPolicy(ctx, r) },
			set: func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return tc.SetIamPolicy(ctx, r) },
		},
	}
	for name, api := range resources {
		p, err := api.get(&iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			t.Fatalf("GetIamPolicy %s: %v", name, err)
		}
		pol := &iam.Policy{InternalProto: p}
		pol.Add(live, role)
		pol.Add("user:keep@example.com", role)
		if _, err := api.set(&iampb.SetIamPolicyRequest{Resource: name, Policy: pol.InternalProto}); err != nil {
			t.Fatalf("SetIamPolicy %s: %v", name, err)
		}
	}
	check := func(when, want string) {
		t.Helper()
		for name, api := range resources {
			p, err := api.get(&iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy %s: %v", name, err)
			}
			members := (&iam.Policy{InternalProto: p}).Members(role)
			if !slices.Contains(members, want) || !slices.Contains(members, "user:keep@example.com") || len(members) != 2 {
				t.Errorf("%s, %s: members %q, want %q and user:keep@example.com", when, name, members, want)
			}
		}
	}
	check("before delete", live)
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: victim.GetName()}); err != nil {
		t.Fatal(err)
	}
	check("after delete", deleted)
	if _, err := adm.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + victim.GetUniqueId()}); err != nil {
		t.Fatal(err)
	}
	check("after undelete", live)
	// A policy set while the account is deleted, carrying the deleted
	// identifier back as a read-modify-write does, is restored too.
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: victim.GetName()}); err != nil {
		t.Fatal(err)
	}
	for name, api := range resources {
		p, err := api.get(&iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := api.set(&iampb.SetIamPolicyRequest{Resource: name, Policy: p}); err != nil {
			t.Fatalf("SetIamPolicy %s with the deleted member: %v", name, err)
		}
	}
	if _, err := adm.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + victim.GetUniqueId()}); err != nil {
		t.Fatal(err)
	}
	check("after the second undelete", live)
}
