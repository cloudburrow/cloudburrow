//go:build compat

package compat

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// instanceAccount is the ADC fixture's account, which these tests call as.
func instanceAccount(t *testing.T) (email string, ts oauth2.TokenSource) {
	t.Helper()
	path := credentialsFixture(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		ClientEmail string `json:"client_email"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(b, &f); err != nil || !strings.HasPrefix(f.TokenURI, "http://127.0.0.1") && !strings.HasPrefix(f.TokenURI, "http://localhost") {
		t.Fatalf("the fixture's token_uri is not local")
	}
	creds, err := google.CredentialsFromJSON(context.Background(), b, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	return f.ClientEmail, creds.TokenSource
}

// newServiceAccount creates an account in project through the official
// admin client, deleted when the test ends.
func newServiceAccount(t *testing.T, h *Harness, project, prefix string) *adminpb.ServiceAccount {
	t.Helper()
	c := iamAdmin(t, h)
	id := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e8)
	sa, err := c.CreateServiceAccount(h.Context(), &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: id})
	if err != nil {
		t.Fatalf("CreateServiceAccount %s: %v", id, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteServiceAccount(context.Background(), &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()})
	})
	return sa
}

// grantOn adds member to role in account's own policy.
func grantOn(t *testing.T, h *Harness, account, role, member string) {
	t.Helper()
	c := iamAdmin(t, h)
	pol, err := c.GetIamPolicy(h.Context(), &iampb.GetIamPolicyRequest{Resource: account})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	pol.Add(member, iam.RoleName(role))
	if _, err := c.SetIamPolicy(h.Context(), &admin.SetIamPolicyRequest{Resource: account, Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
}

// iamCredentialsAs is the official IAM Credentials REST client at the
// metadata server, calling with ts's tokens.
func iamCredentialsAs(t *testing.T, h *Harness, ts oauth2.TokenSource) *credentials.IamCredentialsClient {
	t.Helper()
	meta := h.Endpoint(EnvMetadata)
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	opts := []option.ClientOption{option.WithEndpoint(meta)}
	if ts != nil {
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: ts}}))
	} else {
		opts = append(opts, option.WithoutAuthentication())
	}
	c, err := credentials.NewIamCredentialsRESTClient(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.iam.admin.v1.IAM/CreateServiceAccount, google.iam.admin.v1.IAM/GetServiceAccount, google.iam.admin.v1.IAM/ListServiceAccounts, google.iam.admin.v1.IAM/UpdateServiceAccount, google.iam.admin.v1.IAM/PatchServiceAccount, google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount, google.iam.admin.v1.IAM/EnableServiceAccount, google.iam.admin.v1.IAM/DisableServiceAccount
//
// TestIAMServiceAccounts (#1747), with the official
// cloud.google.com/go/iam/admin/apiv1 client over gRPC and
// google.golang.org/api/iam/v1 over REST: an account is created (its
// email, 21-digit unique ID and OAuth2 client ID), read by email, by unique
// ID and with "-" for the project, listed, patched (a stale etag ABORTED),
// updated, disabled and enabled, deleted (then NOT_FOUND) and undeleted by
// its unique ID. Google's refusals: an ID outside 6 to 30 lowercase
// letters, digits and dashes, a display name over 100 bytes, a description
// over 256, a taken ID (ALREADY_EXISTS), and undelete by email.
func TestIAMServiceAccounts(t *testing.T) {
	h := New(t)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "iams-")
	ctx := h.Context()
	sa, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "compat-account",
		ServiceAccount: &adminpb.ServiceAccount{DisplayName: "Compat", Description: "made by the compat suite"}})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	email := "compat-account@" + project + ".iam.gserviceaccount.com"
	if sa.GetEmail() != email || sa.GetName() != "projects/"+project+"/serviceAccounts/"+email || !regexp.MustCompile(`^1\d{20}$`).MatchString(sa.GetUniqueId()) ||
		sa.GetOauth2ClientId() != sa.GetUniqueId() || sa.GetProjectId() != project || sa.GetDisabled() {
		t.Errorf("CreateServiceAccount = %v", sa)
	}
	for _, name := range []string{sa.GetName(), "projects/-/serviceAccounts/" + email, "projects/-/serviceAccounts/" + sa.GetUniqueId()} {
		if got, err := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: name}); err != nil || got.GetUniqueId() != sa.GetUniqueId() {
			t.Errorf("GetServiceAccount(%s) = %v, %v", name, got, err)
		}
	}
	for _, tc := range []struct {
		id string
		sa *adminpb.ServiceAccount
		c  codes.Code
	}{
		{"compat-account", nil, codes.AlreadyExists},
		{"short", nil, codes.InvalidArgument},
		{strings.Repeat("a", 31), nil, codes.InvalidArgument},
		{"Capital-letters", nil, codes.InvalidArgument},
		{"ends-in-dash-", nil, codes.InvalidArgument},
		{"long-display", &adminpb.ServiceAccount{DisplayName: strings.Repeat("x", 101)}, codes.InvalidArgument},
		{"long-descr", &adminpb.ServiceAccount{Description: strings.Repeat("x", 257)}, codes.InvalidArgument},
	} {
		if _, err := c.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: tc.id, ServiceAccount: tc.sa}); status.Code(err) != tc.c {
			t.Errorf("CreateServiceAccount(%q) = %v, want %v", tc.id, err, tc.c)
		}
	}
	p, err := c.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(), Description: "patched", Etag: sa.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || p.GetDescription() != "patched" || p.GetDisplayName() != "Compat" {
		t.Fatalf("PatchServiceAccount = %v, %v", p, err)
	}
	if _, err := c.PatchServiceAccount(ctx, &adminpb.PatchServiceAccountRequest{ServiceAccount: &adminpb.ServiceAccount{Name: sa.GetName(), Description: "x", Etag: sa.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v, want ABORTED", err)
	}
	if u, err := c.UpdateServiceAccount(ctx, &adminpb.ServiceAccount{Name: sa.GetName(), DisplayName: "Updated", Description: "patched"}); err != nil || u.GetDisplayName() != "Updated" {
		t.Errorf("UpdateServiceAccount = %v, %v", u, err)
	}
	if err := c.DisableServiceAccount(ctx, &adminpb.DisableServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: sa.GetName()}); !got.GetDisabled() {
		t.Error("not disabled")
	}
	if err := c.EnableServiceAccount(ctx, &adminpb.EnableServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	it := c.ListServiceAccounts(ctx, &adminpb.ListServiceAccountsRequest{Name: "projects/" + project})
	n := 0
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
		if a.GetEmail() != email {
			t.Errorf("listed %s", a.GetEmail())
		}
	}
	if n != 1 {
		t.Errorf("listed %d accounts, want 1", n)
	}
	if err := c.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: sa.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted account = %v, want NOT_FOUND", err)
	}
	if _, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: sa.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("undelete by email = %v, want INVALID_ARGUMENT", err)
	}
	u, err := c.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + sa.GetUniqueId()})
	if err != nil || u.GetRestoredAccount().GetEmail() != email {
		t.Fatalf("UndeleteServiceAccount = %v, %v", u, err)
	}

	// The same over REST.
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(h.Endpoint(EnvResourceManager), "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Projects.ServiceAccounts.Create("projects/"+project, &iamv1.CreateServiceAccountRequest{AccountId: "rest-account"}).Context(ctx).Do()
	if err != nil || r.Email != "rest-account@"+project+".iam.gserviceaccount.com" {
		t.Fatalf("REST create = %+v, %v", r, err)
	}
	if l, err := svc.Projects.ServiceAccounts.List("projects/" + project).Context(ctx).Do(); err != nil || len(l.Accounts) != 2 {
		t.Errorf("REST list = %+v, %v", l, err)
	}
	if _, err := svc.Projects.ServiceAccounts.Delete(r.Name).Context(ctx).Do(); err != nil {
		t.Errorf("REST delete = %v", err)
	}
	if _, err := svc.Projects.ServiceAccounts.Get(r.Name).Context(ctx).Do(); !isHTTPStatus(err, 404) {
		t.Errorf("REST get after delete = %v, want 404", err)
	}
}

// covers: google.iam.admin.v1.IAM/CreateServiceAccountKey, google.iam.admin.v1.IAM/UploadServiceAccountKey, google.iam.admin.v1.IAM/ListServiceAccountKeys, google.iam.admin.v1.IAM/GetServiceAccountKey, google.iam.admin.v1.IAM/DeleteServiceAccountKey, google.iam.admin.v1.IAM/DisableServiceAccountKey, google.iam.admin.v1.IAM/EnableServiceAccountKey
//
// TestIAMServiceAccountKeys (#1747): a key created through the official
// client is a JSON key file whose token_uri is this instance's, so
// golang.org/x/oauth2/google signs in with it locally and the token is the
// account's; disabled, the key is refused (invalid_grant); Get returns its
// X.509 certificate, matching the file's key; List filters by key type
// and shows the system-managed key; a certificate uploads; at most 10
// user-managed keys; delete removes it. The key material is never
// printed.
func TestIAMServiceAccountKeys(t *testing.T) {
	h := New(t)
	c := iamAdmin(t, h)
	h.Endpoint(EnvMetadata)
	project := iamProject(t, h, "iamk-")
	ctx := h.Context()
	sa := newServiceAccount(t, h, project, "keys")
	key, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey: %v", err)
	}
	if key.GetKeyType() != adminpb.ListServiceAccountKeysRequest_USER_MANAGED || key.GetKeyOrigin() != adminpb.ServiceAccountKeyOrigin_GOOGLE_PROVIDED ||
		key.GetPrivateKeyType() != adminpb.ServiceAccountPrivateKeyType_TYPE_GOOGLE_CREDENTIALS_FILE || key.GetValidBeforeTime().AsTime().Year() != 9999 {
		t.Errorf("created key: type %v origin %v file %v", key.GetKeyType(), key.GetKeyOrigin(), key.GetPrivateKeyType())
	}
	var file struct {
		TokenURI    string `json:"token_uri"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(key.GetPrivateKeyData(), &file); err != nil || file.ClientEmail != sa.GetEmail() ||
		!(strings.HasPrefix(file.TokenURI, "http://127.0.0.1") || strings.HasPrefix(file.TokenURI, "http://localhost")) {
		t.Fatalf("the key file: email %q, token_uri %q, %v", file.ClientEmail, file.TokenURI, err)
	}
	// Sign in with the key file, locally.
	signIn := func() error {
		creds, err := google.CredentialsFromJSON(ctx, key.GetPrivateKeyData(), "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return err
		}
		tok, err := creds.TokenSource.Token()
		if err == nil && !strings.HasPrefix(tok.AccessToken, "cbl_") {
			return fmt.Errorf("the token is not a local one")
		}
		return err
	}
	if err := signIn(); err != nil {
		t.Fatalf("sign in with the key file: %v", err)
	}
	if err := c.DisableServiceAccountKey(ctx, &adminpb.DisableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if err := signIn(); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("sign in with a disabled key = %v, want invalid_grant", err)
	}
	if err := c.EnableServiceAccountKey(ctx, &adminpb.EnableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if err := signIn(); err != nil {
		t.Errorf("sign in after enable: %v", err)
	}
	g, err := c.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: key.GetName(), PublicKeyType: adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE})
	if err != nil || len(g.GetPrivateKeyData()) != 0 {
		t.Fatalf("GetServiceAccountKey: %v", err)
	}
	cb, _ := pem.Decode(g.GetPublicKeyData())
	pb, _ := pem.Decode([]byte(file.PrivateKey))
	if cb == nil || pb == nil {
		t.Fatal("no PEM in the certificate or the key file")
	}
	cert, err1 := x509.ParseCertificate(cb.Bytes)
	priv, err2 := x509.ParsePKCS8PrivateKey(pb.Bytes)
	if err1 != nil || err2 != nil || !priv.(*rsa.PrivateKey).PublicKey.Equal(cert.PublicKey) {
		t.Error("the certificate is not the key file's key")
	}
	// Upload: the certificate of a key made here.
	up, err := c.UploadServiceAccountKey(ctx, &adminpb.UploadServiceAccountKeyRequest{Name: sa.GetName(), PublicKeyData: uploadCert(t)})
	if err != nil || up.GetKeyOrigin() != adminpb.ServiceAccountKeyOrigin_USER_PROVIDED {
		t.Fatalf("UploadServiceAccountKey = %v", err)
	}
	keys := func(kt ...adminpb.ListServiceAccountKeysRequest_KeyType) int {
		l, err := c.ListServiceAccountKeys(ctx, &adminpb.ListServiceAccountKeysRequest{Name: sa.GetName(), KeyTypes: kt})
		if err != nil {
			t.Fatal(err)
		}
		return len(l.GetKeys())
	}
	if u, s := keys(adminpb.ListServiceAccountKeysRequest_USER_MANAGED), keys(adminpb.ListServiceAccountKeysRequest_SYSTEM_MANAGED); u != 2 || s != 1 {
		t.Errorf("%d user-managed and %d system-managed keys, want 2 and 1", u, s)
	}
	for keys(adminpb.ListServiceAccountKeysRequest_USER_MANAGED) < 10 {
		if _, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an 11th key = %v, want FAILED_PRECONDITION", err)
	}
	if err := c.DeleteServiceAccountKey(ctx, &adminpb.DeleteServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: key.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted key = %v, want NOT_FOUND", err)
	}
	if err := signIn(); err == nil {
		t.Error("a deleted key still signs in")
	}
}

func uploadCert(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// covers: google.iam.admin.v1.IAM/GetIamPolicy, google.iam.admin.v1.IAM/SetIamPolicy, google.iam.admin.v1.IAM/TestIamPermissions, google.iam.credentials.v1.IAMCredentials/GenerateAccessToken, google.iam.credentials.v1.IAMCredentials/GenerateIdToken, google.iam.credentials.v1.IAMCredentials/SignBlob, google.iam.credentials.v1.IAMCredentials/SignJwt
//
// TestIAMCredentialsImpersonation (#1747), with the official
// cloud.google.com/go/iam/credentials/apiv1 client, calling as the ADC
// fixture's account: without roles/iam.serviceAccountTokenCreator on the
// target every method is PERMISSION_DENIED (and so for an account that does
// not exist); granted on the account's own policy (GetIamPolicy,
// SetIamPolicy, TestIamPermissions through the admin client), an access
// token is minted for the target, an ID token names its unique ID and
// email, signBlob and signJwt verify against the certificate of the key ID
// they return; the target may not mint for itself (FAILED_PRECONDITION),
// a call with no token is UNAUTHENTICATED, a disabled target is
// FAILED_PRECONDITION, and a lifetime over 3,600s is INVALID_ARGUMENT.
func TestIAMCredentialsImpersonation(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata)
	admin := iamAdmin(t, h)
	project := iamProject(t, h, "iami-")
	ctx := h.Context()
	me, ts := instanceAccount(t)
	ic := iamCredentialsAs(t, h, ts)
	target := newServiceAccount(t, h, project, "target")
	name := "projects/-/serviceAccounts/" + target.GetEmail()
	scope := []string{"https://www.googleapis.com/auth/cloud-platform"}

	if _, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("without the role = %v, want PERMISSION_DENIED", err)
	}
	if _, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: "projects/-/serviceAccounts/nobody@" + project + ".iam.gserviceaccount.com", Scope: scope}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an unknown account = %v, want PERMISSION_DENIED", err)
	}
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+me)
	if pol, err := admin.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: target.GetName()}); err != nil ||
		!strings.Contains(strings.Join(pol.Members("roles/iam.serviceAccountTokenCreator"), ","), me) {
		t.Errorf("GetIamPolicy after the grant: %v", err)
	}
	if tp, err := admin.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: target.GetName(),
		Permissions: []string{"iam.serviceAccounts.getAccessToken", "storage.objects.get"}}); err != nil || strings.Join(tp.GetPermissions(), ",") != "iam.serviceAccounts.getAccessToken" {
		t.Errorf("TestIamPermissions = %v, %v", tp, err)
	}

	at, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope})
	if err != nil || !strings.HasPrefix(at.GetAccessToken(), "cbl_") || at.GetExpireTime() == nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	if _, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope, Lifetime: durationpb.New(3601 * time.Second)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a lifetime of 3601s = %v, want INVALID_ARGUMENT", err)
	}
	// The target's own token may not mint another for it.
	self := iamCredentialsAs(t, h, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: at.GetAccessToken()}))
	if _, err := self.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope}); !isRESTStatus(err, 400, "FAILED_PRECONDITION") {
		t.Errorf("self-impersonation = %v, want FAILED_PRECONDITION", err)
	}
	anon := iamCredentialsAs(t, h, nil)
	if _, err := anon.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no token = %v, want UNAUTHENTICATED", err)
	}

	idt, err := ic.GenerateIdToken(ctx, &credentialspb.GenerateIdTokenRequest{Name: name, Audience: "https://svc.example", IncludeEmail: true})
	if err != nil {
		t.Fatalf("GenerateIdToken: %v", err)
	}
	var claims map[string]any
	if parts := strings.Split(idt.GetToken(), "."); len(parts) == 3 {
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(b, &claims)
	}
	if claims["sub"] != target.GetUniqueId() || claims["email"] != target.GetEmail() || claims["aud"] != "https://svc.example" {
		t.Errorf("ID token claims: sub %v, email %v, aud %v", claims["sub"], claims["email"], claims["aud"])
	}

	certOf := func(keyID string) *rsa.PublicKey {
		g, err := admin.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: target.GetName() + "/keys/" + keyID,
			PublicKeyType: adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE})
		if err != nil {
			t.Fatalf("GetServiceAccountKey(%s): %v", keyID, err)
		}
		b, _ := pem.Decode(g.GetPublicKeyData())
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c.PublicKey.(*rsa.PublicKey)
	}
	sb, err := ic.SignBlob(ctx, &credentialspb.SignBlobRequest{Name: name, Payload: []byte("compat blob")})
	if err != nil {
		t.Fatalf("SignBlob: %v", err)
	}
	sum := sha256.Sum256([]byte("compat blob"))
	if rsa.VerifyPKCS1v15(certOf(sb.GetKeyId()), crypto.SHA256, sum[:], sb.GetSignedBlob()) != nil {
		t.Error("the signed blob does not verify against its key's certificate")
	}
	sj, err := ic.SignJwt(ctx, &credentialspb.SignJwtRequest{Name: name, Payload: `{"iss":"` + target.GetEmail() + `","aud":"https://svc.example"}`})
	if err != nil {
		t.Fatalf("SignJwt: %v", err)
	}
	parts := strings.Split(sj.GetSignedJwt(), ".")
	if len(parts) != 3 {
		t.Fatal("signJwt returned no JWT")
	}
	jsum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	jsig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if rsa.VerifyPKCS1v15(certOf(sj.GetKeyId()), crypto.SHA256, jsum[:], jsig) != nil {
		t.Error("the signed JWT does not verify against its key's certificate")
	}

	if err := admin.DisableServiceAccount(ctx, &adminpb.DisableServiceAccountRequest{Name: target.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope}); !isRESTStatus(err, 400, "FAILED_PRECONDITION") {
		t.Errorf("a disabled account = %v, want FAILED_PRECONDITION", err)
	}
}

// TestGcloudIAMServiceAccounts (#1747): gcloud iam service-accounts create,
// describe, list, update, disable, enable, keys create / list / delete,
// add-iam-policy-binding, get-iam-policy, delete and undelete, through
// gcloud-setup's iam override alone. The key file gcloud writes is in the
// test's temporary directory and never printed.
func TestGcloudIAMServiceAccounts(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	project := iamProject(t, h, "iamgs-")
	g := newGcloudSession(t, h)
	email := "gcloud-account@" + project + ".iam.gserviceaccount.com"
	g.must("iam", "service-accounts", "create", "gcloud-account", "--project", project, "--display-name", "gcloud account", "--description", "made by gcloud")
	if out := g.must("iam", "service-accounts", "describe", email, "--project", project, "--format=value(displayName,uniqueId)"); !strings.Contains(out, "gcloud account") {
		t.Errorf("describe:\n%s", out)
	}
	if out := g.must("iam", "service-accounts", "list", "--project", project, "--format=value(email)"); !strings.Contains(out, email) {
		t.Errorf("list:\n%s", out)
	}
	g.must("iam", "service-accounts", "update", email, "--project", project, "--display-name", "renamed")
	g.must("iam", "service-accounts", "disable", email, "--project", project)
	if out := g.must("iam", "service-accounts", "describe", email, "--project", project, "--format=value(disabled)"); !strings.Contains(out, "True") {
		t.Errorf("describe after disable:\n%s", out)
	}
	g.must("iam", "service-accounts", "enable", email, "--project", project)
	keyFile := t.TempDir() + "/key.json"
	g.must("iam", "service-accounts", "keys", "create", keyFile, "--iam-account", email, "--project", project)
	if b, err := os.ReadFile(keyFile); err != nil || !json.Valid(b) {
		t.Errorf("the key file gcloud wrote is not JSON: %v", err)
	}
	out := g.must("iam", "service-accounts", "keys", "list", "--iam-account", email, "--project", project, "--managed-by", "user", "--format=value(name)")
	ids := strings.Fields(out)
	if len(ids) != 1 {
		t.Fatalf("keys list --managed-by user:\n%s", out)
	}
	g.must("iam", "service-accounts", "add-iam-policy-binding", email, "--project", project, "--member", "user:gcloud@example.com",
		"--role", "roles/iam.serviceAccountUser")
	if out := g.must("iam", "service-accounts", "get-iam-policy", email, "--project", project, "--format=json"); !strings.Contains(out, "user:gcloud@example.com") {
		t.Errorf("get-iam-policy:\n%s", out)
	}
	g.must("iam", "service-accounts", "keys", "delete", ids[0], "--iam-account", email, "--project", project, "--quiet")
	uid := strings.TrimSpace(g.must("iam", "service-accounts", "describe", email, "--project", project, "--format=value(uniqueId)"))
	g.must("iam", "service-accounts", "delete", email, "--project", project, "--quiet")
	if _, err := g.run(nil, "iam", "service-accounts", "describe", email, "--project", project); err == nil {
		t.Error("describe after delete succeeded")
	}
	g.must("iam", "service-accounts", "undelete", uid, "--project", project)
	if _, err := g.run(nil, "iam", "service-accounts", "create", "x", "--project", project); err == nil {
		t.Error("an account ID of one letter was accepted")
	}
}

// TestTerraformIAMServiceAccount (#1747): google_service_account,
// google_service_account_key, google_service_account_iam_member and
// google_service_account_iam_binding apply through cloudburrow terraform,
// read back through the official client, plan clean, change in place (the
// display name) and destroy (the account deleted). The key's private_key
// stays in the state in the test's temporary directory and is never
// printed.
func TestTerraformIAMServiceAccount(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "iamts-")
	m := newTFModule(t)
	module := func(display string) string {
		return fmt.Sprintf(`
resource "google_service_account" "sa" {
  project      = %q
  account_id   = "tf-account"
  display_name = %q
  description  = "made by terraform"
}

resource "google_service_account_key" "k" {
  service_account_id = google_service_account.sa.name
}

resource "google_service_account_iam_member" "m" {
  service_account_id = google_service_account.sa.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "user:tf@example.com"
}

resource "google_service_account_iam_binding" "b" {
  service_account_id = google_service_account.sa.name
  role               = "roles/iam.serviceAccountUser"
  members            = ["user:tf-binding@example.com"]
}
`, project, display)
	}
	m.write(module("tf account"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + project + "/serviceAccounts/tf-account@" + project + ".iam.gserviceaccount.com"
	sa, err := c.GetServiceAccount(h.Context(), &adminpb.GetServiceAccountRequest{Name: name})
	if err != nil || sa.GetDisplayName() != "tf account" || sa.GetDescription() != "made by terraform" {
		t.Fatalf("after apply GetServiceAccount = %v, %v", sa, err)
	}
	if l, err := c.ListServiceAccountKeys(h.Context(), &adminpb.ListServiceAccountKeysRequest{Name: name,
		KeyTypes: []adminpb.ListServiceAccountKeysRequest_KeyType{adminpb.ListServiceAccountKeysRequest_USER_MANAGED}}); err != nil || len(l.GetKeys()) != 1 {
		t.Errorf("user-managed keys after apply = %v, %v", l, err)
	}
	pol, err := c.GetIamPolicy(h.Context(), &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(pol.Members("roles/iam.serviceAccountTokenCreator")) != 1 || len(pol.Members("roles/iam.serviceAccountUser")) != 1 {
		t.Errorf("the account's policy after apply: %v", err)
	}
	m.planClean()
	m.write(module("tf account 2"))
	m.apply()
	if sa, err := c.GetServiceAccount(h.Context(), &adminpb.GetServiceAccountRequest{Name: name}); err != nil || sa.GetDisplayName() != "tf account 2" || sa.GetUniqueId() == "" {
		t.Errorf("after the change = %v, %v", sa, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetServiceAccount(h.Context(), &adminpb.GetServiceAccountRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy = %v, want NOT_FOUND", err)
	}
}

func isHTTPStatus(err error, code int) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == code
}

// isRESTStatus is whether a REST client's error is the HTTP code with the
// JSON error's status: the Go REST clients take their code from the HTTP
// status, so a 400 FAILED_PRECONDITION reads as INVALID_ARGUMENT.
func isRESTStatus(err error, code int, st string) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == code && strings.Contains(e.Body, `"`+st+`"`)
}
