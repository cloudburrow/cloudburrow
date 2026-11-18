package resourcemanager

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Service accounts through the official gRPC client (#1747): creation and
// its rules, names by email and unique ID, patch, disable, delete and the
// 30-day undelete.
func TestServiceAccountsThroughTheOfficialClient(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	if _, err := reg.Create(Project{ProjectID: "sa-project"}); err != nil {
		t.Fatal(err)
	}
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-project", AccountId: "builder",
		ServiceAccount: &adminpb.ServiceAccount{DisplayName: "Builder", Description: "builds"}})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	const email = "builder@sa-project.iam.gserviceaccount.com"
	if sa.GetEmail() != email || sa.GetName() != "projects/sa-project/serviceAccounts/"+email || sa.GetProjectId() != "sa-project" ||
		!regexp.MustCompile(`^1\d{20}$`).MatchString(sa.GetUniqueId()) || sa.GetOauth2ClientId() != sa.GetUniqueId() ||
		sa.GetDisplayName() != "Builder" || sa.GetDescription() != "builds" || len(sa.GetEtag()) == 0 || sa.GetDisabled() {
		t.Errorf("created = %v", sa)
	}
	for _, name := range []string{sa.GetName(), "projects/-/serviceAccounts/" + email, "projects/-/serviceAccounts/" + sa.GetUniqueId(),
		"projects/" + projectNumberString("sa-project") + "/serviceAccounts/" + email} {
		if got, err := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: name}); err != nil || got.GetUniqueId() != sa.GetUniqueId() {
			t.Errorf("GetServiceAccount(%s) = %v, %v", name, got, err)
		}
	}
	for _, tc := range []struct {
		id   string
		sa   *adminpb.ServiceAccount
		want codes.Code
	}{
		{"builder", nil, codes.AlreadyExists},
		{"short", nil, codes.InvalidArgument},
		{strings.Repeat("a", 31), nil, codes.InvalidArgument},
		{"Upper-case", nil, codes.InvalidArgument},
		{"1digitfirst", nil, codes.InvalidArgument},
		{"dash-at-end-", nil, codes.InvalidArgument},
		{"under_score", nil, codes.InvalidArgument},
		{"long-name", &adminpb.ServiceAccount{DisplayName: strings.Repeat("x", 101)}, codes.InvalidArgument},
		{"long-desc", &adminpb.ServiceAccount{Description: strings.Repeat("x", 257)}, codes.InvalidArgument},
	} {
		_, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-project", AccountId: tc.id, ServiceAccount: tc.sa})
		if status.Code(err) != tc.want {
			t.Errorf("CreateServiceAccount(%q) = %v, want %v", tc.id, err, tc.want)
		}
	}
	if _, err := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: "projects/-/serviceAccounts/nobody@sa-project.iam.gserviceaccount.com"}); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown account = %v, want NOT_FOUND", err)
	}
	if _, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/no-such-project", AccountId: "builder"}); status.Code(err) != codes.NotFound {
		t.Errorf("in an unknown project = %v, want NOT_FOUND", err)
	}

	// Patch, with and without the etag.
	got, err := c.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(),
		Description: "ships", Etag: sa.GetEtag()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || got.GetDescription() != "ships" || got.GetDisplayName() != "Builder" {
		t.Fatalf("PatchServiceAccount = %v, %v", got, err)
	}
	if _, err := c.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(),
		Description: "x", Etag: sa.GetEtag()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v, want ABORTED", err)
	}
	if _, err := c.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"email"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("patching email = %v, want INVALID_ARGUMENT", err)
	}

	// Disable and enable.
	if err := c.DisableServiceAccount(ctx, &adminpb.DisableServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if st, ok := reg.ServiceAccountByName(email); !ok || !st.Disabled {
		t.Errorf("after Disable: %v, %v", st, ok)
	}
	if got, _ := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: sa.GetName()}); !got.GetDisabled() {
		t.Errorf("Get after Disable = %v", got)
	}
	if err := c.EnableServiceAccount(ctx, &adminpb.EnableServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}

	// List, paged.
	for _, id := range []string{"aaa-first", "zzz-last"} {
		if _, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-project", AccountId: id}); err != nil {
			t.Fatal(err)
		}
	}
	it := c.ListServiceAccounts(ctx, &adminpb.ListServiceAccountsRequest{Name: "projects/sa-project", PageSize: 2})
	var emails []string
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		emails = append(emails, strings.Split(a.GetEmail(), "@")[0])
	}
	if strings.Join(emails, ",") != "aaa-first,builder,zzz-last" {
		t.Errorf("listed %v", emails)
	}

	// Delete, undelete by unique ID only, and the 30 days.
	if err := c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: sa.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("Get of a deleted account = %v, want NOT_FOUND", err)
	}
	if _, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: sa.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Undelete by email = %v, want INVALID_ARGUMENT", err)
	}
	now = now.Add(29 * 24 * time.Hour)
	u, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + sa.GetUniqueId()})
	if err != nil || u.GetRestoredAccount().GetEmail() != email || u.GetRestoredAccount().GetDescription() != "ships" {
		t.Fatalf("Undelete = %v, %v", u, err)
	}
	if err := c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * 24 * time.Hour)
	if _, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + sa.GetUniqueId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Undelete after 30 days = %v, want NOT_FOUND", err)
	}

	// A new account under a deleted one's email is a new identity, and the
	// deleted one can no longer be undeleted.
	old, _ := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-project", AccountId: "reused-name"})
	_ = c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: old.GetName()})
	again, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-project", AccountId: "reused-name"})
	if err != nil || again.GetUniqueId() == old.GetUniqueId() {
		t.Fatalf("recreate = %v, %v", again, err)
	}
	if _, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + old.GetUniqueId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Undelete of a replaced account = %v, want NOT_FOUND", err)
	}
}

// A project's 101st account.
//
// unverified: google.iam.admin.v1.IAM/CreateServiceAccount RESOURCE_EXHAUSTED: a project's 101st account
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/iam/quotas
func TestServiceAccountLimit(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "sa-limit"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxServiceAccounts; i++ {
		if _, err := reg.CreateServiceAccount(&adminpb.CreateServiceAccountRequest{Name: "projects/sa-limit", AccountId: "account-" + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-limit", AccountId: "one-too-many"}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("account %d = %v, want RESOURCE_EXHAUSTED", maxServiceAccounts+1, err)
	}
}

// Keys (#1747): a created key's file, never kept; upload; list by type;
// get with the public key; the 10-key limit; disable and delete; and
// ServiceAccountPublicKey, which the token endpoint accepts a key with.
func TestServiceAccountKeysThroughTheOfficialClient(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	reg.SetServiceAccountTokenURI("http://127.0.0.1:1/token")
	if _, err := reg.Create(Project{ProjectID: "key-project"}); err != nil {
		t.Fatal(err)
	}
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/key-project", AccountId: "signer"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey: %v", err)
	}
	id := key.GetName()[strings.LastIndex(key.GetName(), "/")+1:]
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(id) || key.GetPrivateKeyType() != adminpb.ServiceAccountPrivateKeyType_TYPE_GOOGLE_CREDENTIALS_FILE ||
		key.GetKeyAlgorithm() != adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_2048 || key.GetKeyOrigin() != adminpb.ServiceAccountKeyOrigin_GOOGLE_PROVIDED ||
		key.GetKeyType() != adminpb.ListServiceAccountKeysRequest_USER_MANAGED || key.GetValidBeforeTime().AsTime().Year() != 9999 {
		t.Errorf("created key: name %s, type %v, alg %v, origin %v, keyType %v", key.GetName(), key.GetPrivateKeyType(), key.GetKeyAlgorithm(), key.GetKeyOrigin(), key.GetKeyType())
	}
	var file map[string]string
	if err := json.Unmarshal(key.GetPrivateKeyData(), &file); err != nil {
		t.Fatalf("the key file is not JSON: %v", err)
	}
	if file["type"] != "service_account" || file["client_email"] != sa.GetEmail() || file["private_key_id"] != id ||
		file["client_id"] != sa.GetUniqueId() || file["token_uri"] != "http://127.0.0.1:1/token" || !strings.HasPrefix(file["private_key"], "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("key file fields: type %q, email %q, id ok %v, token_uri %q", file["type"], file["client_email"], file["private_key_id"] == id, file["token_uri"])
	}
	// The private key is not kept: a Get never returns it, and the public
	// key it returns is the file's.
	g, err := c.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: key.GetName(), PublicKeyType: adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE})
	if err != nil || len(g.GetPrivateKeyData()) != 0 {
		t.Fatalf("GetServiceAccountKey = %v", err)
	}
	b, _ := pem.Decode(g.GetPublicKeyData())
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := pem.Decode([]byte(file["private_key"]))
	priv, err := x509.ParsePKCS8PrivateKey(pb.Bytes)
	if err != nil || !priv.(*rsa.PrivateKey).PublicKey.Equal(cert.PublicKey) {
		t.Errorf("the certificate is not the file's key's: %v", err)
	}
	if pub, who, known, ok := reg.ServiceAccountKeyState(id, time.Now()); !known || !ok || who != sa.GetEmail() || !pub.Equal(cert.PublicKey) {
		t.Errorf("ServiceAccountPublicKey = %v, %v", who, ok)
	}

	// Upload a certificate.
	own, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "uploaded"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &own.PublicKey, own)
	up, err := c.UploadServiceAccountKey(ctx, &adminpb.UploadServiceAccountKeyRequest{Name: sa.GetName(), PublicKeyData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})})
	if err != nil || up.GetKeyOrigin() != adminpb.ServiceAccountKeyOrigin_USER_PROVIDED {
		t.Fatalf("UploadServiceAccountKey = %v, %v", up, err)
	}
	if _, err := c.UploadServiceAccountKey(ctx, &adminpb.UploadServiceAccountKeyRequest{Name: sa.GetName(), PublicKeyData: []byte("not a certificate")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("uploading garbage = %v, want INVALID_ARGUMENT", err)
	}

	count := func(types ...adminpb.ListServiceAccountKeysRequest_KeyType) int {
		l, err := c.ListServiceAccountKeys(ctx, &adminpb.ListServiceAccountKeysRequest{Name: sa.GetName(), KeyTypes: types})
		if err != nil {
			t.Fatal(err)
		}
		return len(l.GetKeys())
	}
	if u, s, all := count(adminpb.ListServiceAccountKeysRequest_USER_MANAGED), count(adminpb.ListServiceAccountKeysRequest_SYSTEM_MANAGED), count(); u != 2 || s != 1 || all != 3 {
		t.Errorf("keys: %d user, %d system, %d all; want 2, 1, 3", u, s, all)
	}

	// Disable refuses the key at the token endpoint; delete removes it.
	if err := c.DisableServiceAccountKey(ctx, &adminpb.DisableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, _, known, ok := reg.ServiceAccountKeyState(id, time.Now()); !known || ok {
		t.Error("a disabled key still signs in")
	}
	if err := c.EnableServiceAccountKey(ctx, &adminpb.EnableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	_ = c.DisableServiceAccount(ctx, &adminpb.DisableServiceAccountRequest{Name: sa.GetName()})
	if _, _, known, ok := reg.ServiceAccountKeyState(id, time.Now()); !known || ok {
		t.Error("a disabled account's key still signs in")
	}
	_ = c.EnableServiceAccount(ctx, &adminpb.EnableServiceAccountRequest{Name: sa.GetName()})
	if err := c.DeleteServiceAccountKey(ctx, &adminpb.DeleteServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: key.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted key = %v, want NOT_FOUND", err)
	}

	// Ten user-managed keys at most.
	for count(adminpb.ListServiceAccountKeysRequest_USER_MANAGED) < maxUserKeys {
		if _, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an 11th key = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName(),
		PrivateKeyType: adminpb.ServiceAccountPrivateKeyType(99)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unknown private key type = %v, want INVALID_ARGUMENT", err)
	}
	// The system-managed key signs, and cannot be deleted.
	signer, kid, err := reg.ServiceAccountSigner(sa.GetEmail())
	if err != nil || signer == nil || kid == "" {
		t.Fatalf("ServiceAccountSigner = %v", err)
	}
	if err := c.DeleteServiceAccountKey(ctx, &adminpb.DeleteServiceAccountKeyRequest{Name: sa.GetName() + "/keys/" + kid}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting the system key = %v, want FAILED_PRECONDITION", err)
	}
}

// A service account's own policy (#1747): round-trips with its etag, is
// refused for an unknown account, and answers TestIamPermissions with the
// account's permissions alone.
func TestServiceAccountIAMPolicy(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "sa-policy"}); err != nil {
		t.Fatal(err)
	}
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/sa-policy", AccountId: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	want := &iam.Policy{InternalProto: &iampb.Policy{}}
	want.Add("user:dev@example.com", "roles/iam.serviceAccountTokenCreator")
	p, err := c.SetIamPolicy(ctx, &admin.SetIamPolicyRequest{Resource: "projects/-/serviceAccounts/" + sa.GetEmail(), Policy: want})
	if err != nil || len(p.InternalProto.GetBindings()) != 1 || len(p.InternalProto.GetEtag()) == 0 {
		t.Fatalf("SetIamPolicy = %v, %v", p, err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sa.GetName()})
	if err != nil || strings.Join(got.Members("roles/iam.serviceAccountTokenCreator"), ",") != "user:dev@example.com" {
		t.Errorf("GetIamPolicy = %v, %v", got, err)
	}
	if stored, _ := reg.IAMPolicy(sa.GetName()); stored == nil || len(stored.Bindings) != 1 {
		t.Errorf("the registry's policy = %v", stored)
	}
	tp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: sa.GetName(),
		Permissions: []string{"iam.serviceAccounts.getAccessToken", "storage.buckets.get", "iam.serviceAccountKeys.create"}})
	if err != nil || strings.Join(tp.GetPermissions(), ",") != "iam.serviceAccounts.getAccessToken,iam.serviceAccountKeys.create" {
		t.Errorf("TestIamPermissions = %v, %v", tp, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/-/serviceAccounts/nobody@sa-policy.iam.gserviceaccount.com"}); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown account's policy = %v, want NOT_FOUND", err)
	}
	// A project custom role is grantable on its project's accounts.
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/sa-policy", RoleId: "impersonator",
		Role: &adminpb.Role{IncludedPermissions: []string{"iam.serviceAccounts.getAccessToken"}}}); err != nil {
		t.Fatal(err)
	}
	custom := &iam.Policy{InternalProto: &iampb.Policy{}}
	custom.Add("user:dev@example.com", "projects/sa-policy/roles/impersonator")
	if _, err := c.SetIamPolicy(ctx, &admin.SetIamPolicyRequest{Resource: sa.GetName(), Policy: custom}); err != nil {
		t.Errorf("binding the project's custom role = %v", err)
	}
}

// The same over REST, as gcloud and Terraform call it.
func TestServiceAccountsOverREST(t *testing.T) {
	reg, srv, _ := startIAMAdmin(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "sa-rest"}); err != nil {
		t.Fatal(err)
	}
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	sa, err := svc.Projects.ServiceAccounts.Create("projects/sa-rest", &iamv1.CreateServiceAccountRequest{AccountId: "rest-account",
		ServiceAccount: &iamv1.ServiceAccount{DisplayName: "REST"}}).Do()
	if err != nil || sa.Email != "rest-account@sa-rest.iam.gserviceaccount.com" {
		t.Fatalf("Create = %+v, %v", sa, err)
	}
	if _, err := svc.Projects.ServiceAccounts.Create("projects/sa-rest", &iamv1.CreateServiceAccountRequest{AccountId: "rest-account"}).Do(); code(err) != 409 {
		t.Errorf("a duplicate = %v, want 409", err)
	}
	p, err := svc.Projects.ServiceAccounts.Patch(sa.Name, &iamv1.PatchServiceAccountRequest{ServiceAccount: &iamv1.ServiceAccount{Description: "d"},
		UpdateMask: "description"}).Do()
	if err != nil || p.Description != "d" || p.DisplayName != "REST" {
		t.Errorf("Patch = %+v, %v", p, err)
	}
	// Terraform names the mask's paths in snake_case, which Google takes.
	if p, err := svc.Projects.ServiceAccounts.Patch(sa.Name, &iamv1.PatchServiceAccountRequest{ServiceAccount: &iamv1.ServiceAccount{DisplayName: "snake"},
		UpdateMask: "display_name"}).Do(); err != nil || p.DisplayName != "snake" {
		t.Errorf("Patch with a snake_case mask = %+v, %v", p, err)
	}
	k, err := svc.Projects.ServiceAccounts.Keys.Create(sa.Name, &iamv1.CreateServiceAccountKeyRequest{}).Do()
	if err != nil {
		t.Fatalf("Keys.Create = %v", err)
	}
	file, err := base64.StdEncoding.DecodeString(k.PrivateKeyData)
	if err != nil || !json.Valid(file) {
		t.Errorf("privateKeyData is not base64 JSON: %v", err)
	}
	g, err := svc.Projects.ServiceAccounts.Keys.Get(k.Name).PublicKeyType("TYPE_X509_PEM_FILE").Do()
	if err != nil || g.PublicKeyData == "" || g.PrivateKeyData != "" {
		t.Errorf("Keys.Get = %v", err)
	}
	pol, err := svc.Projects.ServiceAccounts.SetIamPolicy(sa.Name, &iamv1.SetIamPolicyRequest{Policy: &iamv1.Policy{
		Bindings: []*iamv1.Binding{{Role: "roles/iam.serviceAccountUser", Members: []string{"user:dev@example.com"}}}}}).Do()
	if err != nil || len(pol.Bindings) != 1 {
		t.Errorf("SetIamPolicy = %+v, %v", pol, err)
	}
	if _, err := svc.Projects.ServiceAccounts.Disable(sa.Name, &iamv1.DisableServiceAccountRequest{}).Do(); err != nil {
		t.Errorf("Disable = %v", err)
	}
	if _, err := svc.Projects.ServiceAccounts.Delete(sa.Name).Do(); err != nil {
		t.Errorf("Delete = %v", err)
	}
	u, err := svc.Projects.ServiceAccounts.Undelete("projects/-/serviceAccounts/"+sa.UniqueId, &iamv1.UndeleteServiceAccountRequest{}).Do()
	if err != nil || u.RestoredAccount.Email != sa.Email {
		t.Errorf("Undelete = %+v, %v", u, err)
	}
	if _, err := svc.Projects.ServiceAccounts.Get("projects/-/serviceAccounts/nobody@sa-rest.iam.gserviceaccount.com").Do(); code(err) != 404 {
		t.Errorf("an unknown account = %v, want 404", err)
	}
}
