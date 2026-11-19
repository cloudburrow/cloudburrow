//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"golang.org/x/oauth2"
	asset "google.golang.org/api/cloudasset/v1"
	"google.golang.org/api/option"
	pt "google.golang.org/api/policytroubleshooter/v3"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// envIAMEnforcement is "on" when the instance under test was started with
// `up --iam-enforcement=on` (#1879). The enforcement tests need it, and
// every other test needs it off, so they run against an instance of their
// own.
const envIAMEnforcement = "CLOUDBURROW_TEST_IAM_ENFORCEMENT"

func requireIAMEnforcement(t *testing.T) {
	t.Helper()
	if v := os.Getenv(envIAMEnforcement); v == "" || v != "on" {
		t.Skipf("%s is not on: the instance was not started with --iam-enforcement=on", envIAMEnforcement)
	}
}

// bearerCreds sends a CloudBurrow-minted access token over plaintext gRPC
// to a loopback instance.
type bearerCreds string

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearerCreds) RequireTransportSecurity() bool { return false }

// grpcAs are client options for endpoint calling with token ("" sends no
// token: the instance's own account).
func grpcAs(endpoint, token string) []option.ClientOption {
	opts := []option.ClientOption{option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	if token != "" {
		opts = append(opts, option.WithGRPCDialOption(grpc.WithPerRPCCredentials(bearerCreds(token))))
	}
	return opts
}

// wantDenied checks err is PERMISSION_DENIED with msg, and ErrorInfo
// IAM_PERMISSION_DENIED naming permission when withInfo.
func wantDenied(t *testing.T, what string, err error, msg, permission string, withInfo bool) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != codes.PermissionDenied || st.Message() != msg {
		t.Fatalf("%s = %v; want PERMISSION_DENIED %q", what, err, msg)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			info = ei
		}
	}
	switch {
	case withInfo && (info == nil || info.GetReason() != "IAM_PERMISSION_DENIED" || info.GetDomain() != "iam.googleapis.com" ||
		info.GetMetadata()["permission"] != permission):
		t.Fatalf("%s: ErrorInfo = %v; want IAM_PERMISSION_DENIED of %s", what, info, permission)
	case !withInfo && info != nil:
		t.Fatalf("%s: ErrorInfo = %v; want none", what, info)
	}
}

// waitGranted retries call until it succeeds: a grant takes effect on the
// next check, but a retry keeps the test honest about it.
func waitGranted(t *testing.T, what string, call func() error) {
	t.Helper()
	var err error
	for i := 0; i < 10; i++ {
		if err = call(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s after the grant: %v", what, err)
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.cloud.secretmanager.v1.SecretManagerService/TestIamPermissions
//
// TestIAMEnforcementSecretManager (#1879), with the official Secret
// Manager client over gRPC and an access token minted through IAM
// Credentials for a service account of the test's own: without a role,
// AccessSecretVersion is PERMISSION_DENIED with Google's message and
// ErrorInfo; roles/secretmanager.secretAccessor on the project lets the same
// call through, and taking it back refuses it again; the role on the
// secret's own policy grants it too; a condition on request.time grants
// while it holds and not after; roles/secretmanager.viewer reads metadata
// but not the payload, and TestIamPermissions answers only what it holds; a token the instance did not mint is
// UNAUTHENTICATED; the instance's own account is the owner.
func TestIAMEnforcementSecretManager(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvSecrets)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iesm-")
	owner, err := secretmanager.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	secret := "projects/" + project + "/secrets/enforced"
	if _, err := owner.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "enforced",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret as the instance's account: %v", err)
	}
	if _, err := owner.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("s3cret")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iesm")
	as, err := secretmanager.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	version := secret + "/versions/1"
	access := func() error {
		_, err := as.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version})
		return err
	}
	deniedMsg := "Permission 'secretmanager.versions.access' denied for resource '" + version + "' (or it may not exist)."
	wantDenied(t, "AccessSecretVersion without a role", access(), deniedMsg, "secretmanager.versions.access", true)

	p.grant("roles/secretmanager.secretAccessor")
	waitGranted(t, "AccessSecretVersion with secretAccessor on the project", access)
	p.revoke("roles/secretmanager.secretAccessor")
	wantDenied(t, "AccessSecretVersion after the revoke", access(), deniedMsg, "secretmanager.versions.access", true)

	// The secret's own policy.
	undo := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, secret,
		&iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"serviceAccount:" + p.email}})
	waitGranted(t, "AccessSecretVersion with secretAccessor on the secret", access)
	undo()
	wantDenied(t, "AccessSecretVersion after the secret's binding is gone", access(), deniedMsg, "secretmanager.versions.access", true)

	// A condition on request.time, on the project's policy.
	rm, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	undo = bindOn(t, rm.GetIamPolicy, rm.SetIamPolicy, "projects/"+project, &iampb.Binding{Role: "roles/secretmanager.secretAccessor",
		Members: []string{"serviceAccount:" + p.email}, Condition: &expr.Expr{Title: "for an hour",
			Expression: fmt.Sprintf("request.time < timestamp(%q)", until)}})
	waitGranted(t, "AccessSecretVersion under a condition that holds", access)
	undo()
	undo = bindOn(t, rm.GetIamPolicy, rm.SetIamPolicy, "projects/"+project, &iampb.Binding{Role: "roles/secretmanager.secretAccessor",
		Members: []string{"serviceAccount:" + p.email}, Condition: &expr.Expr{Title: "from an hour on",
			Expression: fmt.Sprintf("request.time > timestamp(%q)", until)}})
	wantDenied(t, "AccessSecretVersion under a condition that does not hold", access(), deniedMsg, "secretmanager.versions.access", true)
	undo()

	// A viewer reads metadata, not the payload.
	p.grant("roles/secretmanager.viewer")
	waitGranted(t, "GetSecret as a viewer", func() error {
		_, err := as.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret})
		return err
	})
	if _, err := as.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/" + project}).Next(); err != nil {
		t.Errorf("ListSecrets as a viewer: %v", err)
	}
	wantDenied(t, "AccessSecretVersion as a viewer", access(), deniedMsg, "secretmanager.versions.access", true)
	held, err := as.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: secret,
		Permissions: []string{"secretmanager.secrets.get", "secretmanager.versions.access"}})
	if err != nil || len(held.GetPermissions()) != 1 || held.GetPermissions()[0] != "secretmanager.secrets.get" {
		t.Errorf("TestIamPermissions as a viewer = %v, %v; want only secretmanager.secrets.get", held.GetPermissions(), err)
	}
	p.revoke("roles/secretmanager.viewer")

	forged, err := secretmanager.NewClient(ctx, grpcAs(endpoint, "cbl_not-minted-here")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forged.Close() })
	if _, err := forged.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("AccessSecretVersion with a token the instance did not mint = %v; want UNAUTHENTICATED", err)
	}
	if _, err := owner.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version}); err != nil {
		t.Errorf("AccessSecretVersion as the instance's account: %v", err)
	}
}

// covers: google.cloud.kms.v1.KeyManagementService/Encrypt, google.cloud.kms.v1.KeyManagementService/Decrypt, google.cloud.kms.v1.KeyManagementService/GetCryptoKey
//
// TestIAMEnforcementKMS (#1879), with the official Cloud KMS client over
// gRPC and a minted token: Encrypt without a role is PERMISSION_DENIED with
// Google's "denied on resource" message and ErrorInfo; Cloud KMS CryptoKey
// Encrypter/Decrypter on the key ring's own policy, which its keys
// inherit, lets Encrypt and Decrypt through; GetCryptoKey stays denied, as
// that role does not hold cloudkms.cryptoKeys.get.
func TestIAMEnforcementKMS(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvKMS)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iekms-")
	owner, err := kms.NewKeyManagementClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	ring, err := owner.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "ring"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := owner.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "key",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iekms")
	as, err := kms.NewKeyManagementClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	var ct []byte
	encrypt := func() error {
		resp, err := as.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("hello")})
		ct = resp.GetCiphertext()
		return err
	}
	wantDenied(t, "Encrypt without a role", encrypt(),
		"Permission 'cloudkms.cryptoKeyVersions.useToEncrypt' denied on resource '"+key.GetName()+"' (or it may not exist).",
		"cloudkms.cryptoKeyVersions.useToEncrypt", true)
	undo := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, ring.GetName(),
		&iampb.Binding{Role: "roles/cloudkms.cryptoKeyEncrypterDecrypter", Members: []string{"serviceAccount:" + p.email}})
	defer undo()
	waitGranted(t, "Encrypt with the role on the key ring", encrypt)
	resp, err := as.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: ct})
	if err != nil || string(resp.GetPlaintext()) != "hello" {
		t.Fatalf("Decrypt with the role on the key ring = %v, %v", resp, err)
	}
	_, err = as.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	wantDenied(t, "GetCryptoKey as an encrypter", err,
		"Permission 'cloudkms.cryptoKeys.get' denied on resource '"+key.GetName()+"' (or it may not exist).", "cloudkms.cryptoKeys.get", true)
}

// covers: google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/GetQueue
//
// TestIAMEnforcementTasks (#1879), with the official Cloud Tasks client
// over gRPC and a minted token: CreateTask without a role is
// PERMISSION_DENIED with Cloud Tasks's own wording and no ErrorInfo; Cloud
// Tasks Enqueuer granted to a Cloud Identity group the account is a member
// of, on the project, lets it through; GetQueue stays denied, as the
// Enqueuer holds only cloudtasks.tasks.create.
func TestIAMEnforcementTasks(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvTasks)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ietq-")
	owner, err := cloudtasks.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	q, err := owner.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: "projects/" + project + "/locations/us-central1",
		Queue: &cloudtaskspb.Queue{Name: "projects/" + project + "/locations/us-central1/queues/enforced"}})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: q.GetName()})
	})
	p := newIAMPrincipal(t, h, project, "ietq")
	as, err := cloudtasks.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	create := func() error {
		_, err := as.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: q.GetName(), Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: "http://127.0.0.1:1/never"}}}})
		return err
	}
	wantDenied(t, "CreateTask without a role", create(),
		`The principal (user or service account) lacks IAM permission "cloudtasks.tasks.create" for the resource "`+q.GetName()+`" (or the resource may not exist).`,
		"cloudtasks.tasks.create", false)

	svc := cloudIdentity(t, h, rmEndpoint)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e8)
	group, _ := groupFixture(t, svc, "ie-enq-"+suffix, "ie-unused-"+suffix)
	addMember(t, svc, group.Name, p.email)
	undo := projectRoles(t, h, project, "group:"+group.GroupKey.Id)
	undo("roles/cloudtasks.enqueuer", true)
	defer undo("roles/cloudtasks.enqueuer", false)
	waitGranted(t, "CreateTask with Enqueuer granted to the account's group", create)
	_, err = as.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: q.GetName()})
	wantDenied(t, "GetQueue as an enqueuer", err,
		`The principal (user or service account) lacks IAM permission "cloudtasks.queues.get" for the resource "`+q.GetName()+`" (or the resource may not exist).`,
		"cloudtasks.queues.get", false)
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion
//
// TestIAMEnforcementInherited (#1879): a project in a folder of the
// organization, and Secret Accessor granted on the folder to a minted
// token's account, lets it read a secret of the project; Policy
// Troubleshooter v3 (google.golang.org/api/policytroubleshooter/v3)
// explains the same decision, CAN_ACCESS with the folder's policy granting
// and CANNOT_ACCESS for another permission; Cloud Asset's analyzeIamPolicy
// (google.golang.org/api/cloudasset/v1) finds the folder's binding for the
// account and the permission.
func TestIAMEnforcementInherited(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvSecrets)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	fc, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001",
		DisplayName: folderLabel(h, "ief-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project := v1ID(h, "ief-")
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", project, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project})
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	owner, err := secretmanager.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	secret := "projects/" + project + "/secrets/inherited"
	if _, err := owner.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "inherited",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := owner.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "ief")
	as, err := secretmanager.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	access := func() error {
		_, err := as.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: secret + "/versions/latest"})
		return err
	}
	wantDenied(t, "AccessSecretVersion before the folder grant", access(),
		"Permission 'secretmanager.versions.access' denied for resource '"+secret+"/versions/latest' (or it may not exist).",
		"secretmanager.versions.access", true)
	undo := bindOn(t, fc.GetIamPolicy, fc.SetIamPolicy, folder.GetName(),
		&iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"serviceAccount:" + p.email}})
	defer undo()
	waitGranted(t, "AccessSecretVersion with the role on the folder", access)

	// Policy Troubleshooter explains it.
	base := "http://" + strings.TrimPrefix(rmEndpoint, "http://") + "/"
	pts, err := pt.NewService(ctx, option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	full := "//secretmanager.googleapis.com/" + secret
	ts := func(perm string) *pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyResponse {
		t.Helper()
		resp, err := pts.Iam.Troubleshoot(&pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyRequest{
			AccessTuple: &pt.GoogleCloudPolicytroubleshooterIamV3AccessTuple{Principal: p.email, FullResourceName: full, Permission: perm}}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("iam:troubleshoot %s: %v", perm, err)
		}
		return resp
	}
	resp := ts("secretmanager.versions.access")
	if resp.OverallAccessState != "CAN_ACCESS" || resp.AllowPolicyExplanation.AllowAccessState != "ALLOW_ACCESS_STATE_GRANTED" {
		t.Fatalf("iam:troubleshoot access = %s / %+v", resp.OverallAccessState, resp.AllowPolicyExplanation)
	}
	foundFolder := false
	for _, ep := range resp.AllowPolicyExplanation.ExplainedPolicies {
		if ep.FullResourceName == "//cloudresourcemanager.googleapis.com/"+folder.GetName() {
			foundFolder = ep.AllowAccessState == "ALLOW_ACCESS_STATE_GRANTED"
			for _, be := range ep.BindingExplanations {
				if be.Role == "roles/secretmanager.secretAccessor" && (be.RolePermission != "ROLE_PERMISSION_INCLUDED" ||
					be.Memberships["serviceAccount:"+p.email].Membership != "MEMBERSHIP_MATCHED") {
					t.Errorf("folder binding explanation = %+v", be)
				}
			}
		}
	}
	if !foundFolder {
		t.Errorf("iam:troubleshoot explained no granting policy on %s: %+v", folder.GetName(), resp.AllowPolicyExplanation.ExplainedPolicies)
	}
	if resp := ts("secretmanager.secrets.delete"); resp.OverallAccessState != "CANNOT_ACCESS" {
		t.Errorf("iam:troubleshoot secrets.delete = %s; want CANNOT_ACCESS", resp.OverallAccessState)
	}

	// Policy Analyzer finds the folder's binding.
	as1, err := asset.NewService(ctx, option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	ar, err := as1.V1.AnalyzeIamPolicy("projects/" + project).AnalysisQueryIdentitySelectorIdentity("serviceAccount:" + p.email).
		AnalysisQueryAccessSelectorPermissions("secretmanager.versions.access").Context(ctx).Do()
	if err != nil {
		t.Fatalf("analyzeIamPolicy: %v", err)
	}
	found := false
	for _, r := range ar.MainAnalysis.AnalysisResults {
		if r.AttachedResourceFullName == "//cloudresourcemanager.googleapis.com/"+folder.GetName() && r.IamBinding.Role == "roles/secretmanager.secretAccessor" {
			found = len(r.AccessControlLists) == 1 && r.AccessControlLists[0].Accesses[0].Permission == "secretmanager.versions.access"
		}
	}
	if !found {
		t.Errorf("analyzeIamPolicy found no folder binding: %+v", ar.MainAnalysis.AnalysisResults)
	}
}

// TestGcloudPolicyTroubleshoot (#1879): gcloud policy-troubleshoot iam,
// through gcloud-setup's policytroubleshooter override alone, answers
// GRANTED for a service account holding Secret Manager Secret Accessor on
// a project and NOT_GRANTED for a permission the role does not hold.
func TestGcloudPolicyTroubleshoot(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	g := newGcloudSession(t, h)
	project := iamProject(t, h, "iegc-")
	sa := newServiceAccount(t, h, project, "iegc")
	change := projectRoles(t, h, project, "serviceAccount:"+sa.GetEmail())
	change("roles/secretmanager.secretAccessor", true)
	defer change("roles/secretmanager.secretAccessor", false)
	full := "//cloudresourcemanager.googleapis.com/projects/" + project
	out := g.must("policy-troubleshoot", "iam", full, "--principal-email="+sa.GetEmail(),
		"--permission=secretmanager.versions.access", "--format=value(access)")
	if strings.TrimSpace(out) != "GRANTED" {
		t.Errorf("policy-troubleshoot iam secretmanager.versions.access = %q; want GRANTED", out)
	}
	out = g.must("policy-troubleshoot", "iam", full, "--principal-email="+sa.GetEmail(),
		"--permission=secretmanager.secrets.delete", "--format=value(access)")
	if strings.TrimSpace(out) != "NOT_GRANTED" {
		t.Errorf("policy-troubleshoot iam secretmanager.secrets.delete = %q; want NOT_GRANTED", out)
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.kms.v1.KeyManagementService/Encrypt, google.cloud.tasks.v2.CloudTasks/GetQueue
//
// TestIAMEnforcementJSON (#1879): the same checks over the JSON APIs,
// through the official clients' REST transports with a minted token:
// Secret Manager's AccessSecretVersion, Cloud KMS's Encrypt and Cloud
// Tasks's GetQueue are 403 PERMISSION_DENIED with Google's messages (and
// ErrorInfo IAM_PERMISSION_DENIED for Secret Manager and KMS), and the
// grant lets each through.
func TestIAMEnforcementJSON(t *testing.T) {
	h := New(t)
	smEndpoint := h.Endpoint(EnvSecrets)
	kmsEndpoint := h.Endpoint(EnvKMS)
	tasksEndpoint := h.Endpoint(EnvTasks)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iejs-")
	p := newIAMPrincipal(t, h, project, "iejs")
	restAs := func(endpoint string) []option.ClientOption {
		return []option.ClientOption{option.WithEndpoint("http://" + strings.TrimPrefix(endpoint, "http://")),
			option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}})}
	}
	wantREST := func(what string, err error, msg string, withInfo bool) {
		t.Helper()
		var ae *apierror.APIError
		if !errors.As(err, &ae) || ae.HTTPCode() != http.StatusForbidden || !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s over JSON = %v; want 403 %q", what, err, msg)
		}
		if withInfo && ae.Reason() != "IAM_PERMISSION_DENIED" {
			t.Fatalf("%s over JSON: ErrorInfo reason %q; want IAM_PERMISSION_DENIED", what, ae.Reason())
		}
	}

	// Secret Manager.
	owner, err := secretmanager.NewClient(ctx, grpcAs(smEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	secret := "projects/" + project + "/secrets/json"
	if _, err := owner.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "json",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := owner.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("j")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	sm, err := secretmanager.NewRESTClient(ctx, restAs(smEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	access := func() error {
		_, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: secret + "/versions/1"})
		return err
	}
	wantREST("AccessSecretVersion", access(), "Permission 'secretmanager.versions.access' denied for resource '"+secret+"/versions/1' (or it may not exist).", true)
	p.grant("roles/secretmanager.secretAccessor")
	waitGranted(t, "AccessSecretVersion over JSON", access)

	// Cloud KMS.
	kowner, err := kms.NewKeyManagementClient(ctx, grpcAs(kmsEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kowner.Close() })
	ring, err := kowner.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "json"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kowner.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	kc, err := kms.NewKeyManagementRESTClient(ctx, restAs(kmsEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	encrypt := func() error {
		_, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")})
		return err
	}
	wantREST("Encrypt", encrypt(), "Permission 'cloudkms.cryptoKeyVersions.useToEncrypt' denied on resource '"+key.GetName()+"' (or it may not exist).", true)
	p.grant("roles/cloudkms.cryptoKeyEncrypter")
	waitGranted(t, "Encrypt over JSON", encrypt)

	// Cloud Tasks.
	towner, err := cloudtasks.NewClient(ctx, grpcAs(tasksEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = towner.Close() })
	q, err := towner.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: "projects/" + project + "/locations/us-central1",
		Queue: &cloudtaskspb.Queue{Name: "projects/" + project + "/locations/us-central1/queues/json"}})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() {
		_ = towner.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: q.GetName()})
	})
	tc, err := cloudtasks.NewRESTClient(ctx, restAs(tasksEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	get := func() error {
		_, err := tc.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: q.GetName()})
		return err
	}
	wantREST("GetQueue", get(), `The principal (user or service account) lacks IAM permission "cloudtasks.queues.get" for the resource "`+q.GetName()+`" (or the resource may not exist).`, false)
	p.grant("roles/cloudtasks.viewer")
	waitGranted(t, "GetQueue over JSON", get)
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/TestIamPermissions
//
// TestIAMEnforcementJSONTestIamPermissions (#1879): TestIamPermissions
// over JSON, through the official Secret Manager client's REST transport
// with a minted token, answers only the requested permissions the caller
// holds, as over gRPC: none without a role, secretmanager.secrets.get and
// not secretmanager.versions.access as a viewer; the instance's own account
// holds both.
func TestIAMEnforcementJSONTestIamPermissions(t *testing.T) {
	h := New(t)
	smEndpoint := h.Endpoint(EnvSecrets)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iejt-")
	p := newIAMPrincipal(t, h, project, "iejt")
	restAs := func(token string) []option.ClientOption {
		hc := &http.Client{}
		if token != "" {
			hc.Transport = &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}
		}
		t.Cleanup(hc.CloseIdleConnections)
		return []option.ClientOption{option.WithEndpoint("http://" + strings.TrimPrefix(smEndpoint, "http://")), option.WithHTTPClient(hc)}
	}
	owner, err := secretmanager.NewRESTClient(ctx, restAs("")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	secret := "projects/" + project + "/secrets/tip"
	if _, err := owner.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "tip",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	as, err := secretmanager.NewRESTClient(ctx, restAs(p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	ask := []string{"secretmanager.secrets.get", "secretmanager.versions.access"}
	test := func(c *secretmanager.Client) []string {
		t.Helper()
		resp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: secret, Permissions: ask})
		if err != nil {
			t.Fatalf("TestIamPermissions over JSON: %v", err)
		}
		return resp.GetPermissions()
	}
	if got := test(as); len(got) != 0 {
		t.Errorf("TestIamPermissions over JSON without a role = %v; want none", got)
	}
	p.grant("roles/secretmanager.viewer")
	waitGranted(t, "TestIamPermissions over JSON as a viewer", func() error {
		if got := test(as); len(got) != 1 || got[0] != "secretmanager.secrets.get" {
			return fmt.Errorf("got %v; want only secretmanager.secrets.get", got)
		}
		return nil
	})
	if got := test(owner); len(got) != 2 {
		t.Errorf("TestIamPermissions over JSON as the instance's account = %v; want both", got)
	}
}
