//go:build compat

package compat

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/crypto/pkcs12"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jws"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setPolicy adds (add true) or removes member's role on resource (a folder,
// the organization or a project) through the official v3 clients.
func setPolicy(t *testing.T, h *Harness, resource, role, member string, add bool) {
	t.Helper()
	fc, oc, pc := foldersClients(t, h)
	ctx := h.Context()
	var pol *iampb.Policy
	var err error
	get := &iampb.GetIamPolicyRequest{Resource: resource}
	switch {
	case strings.HasPrefix(resource, "folders/"):
		pol, err = fc.GetIamPolicy(ctx, get)
	case strings.HasPrefix(resource, "organizations/"):
		pol, err = oc.GetIamPolicy(ctx, get)
	default:
		pol, err = pc.GetIamPolicy(ctx, get)
	}
	if err != nil {
		t.Fatalf("GetIamPolicy(%s): %v", resource, err)
	}
	h3 := &iam.Policy{InternalProto: pol}
	if add {
		h3.Add(member, iam.RoleName(role))
	} else {
		h3.Remove(member, iam.RoleName(role))
	}
	set := &iampb.SetIamPolicyRequest{Resource: resource, Policy: h3.InternalProto}
	switch {
	case strings.HasPrefix(resource, "folders/"):
		_, err = fc.SetIamPolicy(ctx, set)
	case strings.HasPrefix(resource, "organizations/"):
		_, err = oc.SetIamPolicy(ctx, set)
	default:
		_, err = pc.SetIamPolicy(ctx, set)
	}
	if err != nil {
		t.Fatalf("SetIamPolicy(%s): %v", resource, err)
	}
}

// TestIAMImpersonationInheritsGrants (#1750): IAM Credentials' check reads
// every predefined role from Google's catalog, so a role beside Token
// Creator that holds iam.serviceAccounts.getAccessToken counts, and Google's
// resource hierarchy: a grant on the account's project's folder or on the
// organization lets the caller impersonate it, and stops when removed. With
// the official Resource Manager v3 and IAM Credentials clients.
func TestIAMImpersonationInheritsGrants(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	fc, _, pc := foldersClients(t, h)
	ctx := h.Context()
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "sa1750")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	project := v1ID(h, "sai-")
	pop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() {
		if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	me, ts := instanceAccount(t)
	ic := iamCredentialsAs(t, h, ts)
	target := newServiceAccount(t, h, project, "inherit")
	name := "projects/-/serviceAccounts/" + target.GetEmail()
	scope := []string{"https://www.googleapis.com/auth/cloud-platform"}
	mint := func() error {
		_, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{Name: name, Scope: scope})
		return err
	}
	if err := mint(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("without a grant = %v, want PERMISSION_DENIED", err)
	}
	for _, c := range []struct{ resource, role string }{
		{folder.GetName(), "roles/iam.serviceAccountTokenCreator"},
		// A service agent role, which holds getAccessToken, from the catalog.
		{localOrg, "roles/cloudbuild.serviceAgent"},
		{"projects/" + project, "roles/cloudbuild.serviceAgent"},
	} {
		setPolicy(t, h, c.resource, c.role, "serviceAccount:"+me, true)
		if err := mint(); err != nil {
			t.Errorf("%s on %s: GenerateAccessToken = %v", c.role, c.resource, err)
		}
		setPolicy(t, h, c.resource, c.role, "serviceAccount:"+me, false)
		if err := mint(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s removed from %s = %v, want PERMISSION_DENIED", c.role, c.resource, err)
		}
	}
	// A predefined role that holds no IAM Credentials permission grants none.
	setPolicy(t, h, folder.GetName(), "roles/iam.serviceAccountViewer", "serviceAccount:"+me, true)
	if err := mint(); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Service Account Viewer = %v, want PERMISSION_DENIED", err)
	}
}

// TestIAMDeletedServiceAccountBindings (#1750): a deleted account's
// bindings read deleted:serviceAccount:EMAIL?uid=UNIQUE_ID, in its
// project's policy and in another account's, and grant nothing; undelete
// restores them as serviceAccount:EMAIL; an account created afresh under
// the email does not inherit the old one's bindings. With the official
// Resource Manager v3 and IAM admin clients.
func TestIAMDeletedServiceAccountBindings(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	adm := iamAdmin(t, h)
	_, _, pc := foldersClients(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "sad-")
	other := newServiceAccount(t, h, project, "other")
	victim := newServiceAccount(t, h, project, "victim")
	live := "serviceAccount:" + victim.GetEmail()
	deleted := "deleted:serviceAccount:" + victim.GetEmail() + "?uid=" + victim.GetUniqueId()
	setPolicy(t, h, "projects/"+project, "roles/viewer", live, true)
	grantOn(t, h, other.GetName(), "roles/iam.serviceAccountTokenCreator", live)

	members := func() (string, string) {
		t.Helper()
		pp, err := pc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
		if err != nil {
			t.Fatal(err)
		}
		sp, err := adm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: other.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join((&iam.Policy{InternalProto: pp}).Members("roles/viewer"), ","),
			strings.Join(sp.Members("roles/iam.serviceAccountTokenCreator"), ",")
	}
	if p, s := members(); p != live || s != live {
		t.Fatalf("before delete: %q, %q", p, s)
	}
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: victim.GetName()}); err != nil {
		t.Fatal(err)
	}
	if p, s := members(); p != deleted || s != deleted {
		t.Errorf("after delete: %q, %q, want %q", p, s, deleted)
	}
	if _, err := adm.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + victim.GetUniqueId()}); err != nil {
		t.Fatal(err)
	}
	if p, s := members(); p != live || s != live {
		t.Errorf("after undelete: %q, %q, want %q", p, s, live)
	}
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: victim.GetName()}); err != nil {
		t.Fatal(err)
	}
	id := strings.Split(victim.GetEmail(), "@")[0]
	again, err := adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: id})
	if err != nil {
		t.Fatalf("CreateServiceAccount again: %v", err)
	}
	t.Cleanup(func() {
		_ = adm.DeleteServiceAccount(context.Background(), &adminpb.DeleteServiceAccountRequest{Name: again.GetName()})
	})
	if p, s := members(); p != deleted || s != deleted {
		t.Errorf("after the email is reused: %q, %q, want %q", p, s, deleted)
	}
}

// TestIAMServiceAccountCertURLs (#1750): the account's public certificate
// URLs, served on the metadata port as www.googleapis.com serves them:
// service_accounts/v1/metadata/x509/EMAIL (key ID to PEM certificate) and
// .../jwk/EMAIL (a JWK set) hold the system-managed key signJwt signs with,
// so golang.org/x/oauth2/jws verifies a signJwt token against them; a
// created key's file names robot/v1/metadata/x509/EMAIL as its
// client_x509_cert_url, which holds that key; a deleted key leaves, and an
// unknown account is 404.
func TestIAMServiceAccountCertURLs(t *testing.T) {
	h := New(t)
	meta := h.Endpoint(EnvMetadata)
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "sac-")
	me, ts := instanceAccount(t)
	ic := iamCredentialsAs(t, h, ts)
	target := newServiceAccount(t, h, project, "certs")
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+me)

	fetch := func(url string, out any) int {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil && resp.StatusCode == 200 {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}
	pubOf := func(certPEM string) *rsa.PublicKey {
		b, _ := pem.Decode([]byte(certPEM))
		if b == nil {
			t.Fatal("not a PEM certificate")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c.PublicKey.(*rsa.PublicKey)
	}

	sj, err := ic.SignJwt(ctx, &credentialspb.SignJwtRequest{Name: "projects/-/serviceAccounts/" + target.GetEmail(),
		Payload: fmt.Sprintf(`{"iss":%q,"aud":"https://svc.example","exp":%d}`, target.GetEmail(), time.Now().Add(time.Hour).Unix())})
	if err != nil {
		t.Fatalf("SignJwt: %v", err)
	}
	var x509s map[string]string
	if code := fetch(meta+"/service_accounts/v1/metadata/x509/"+target.GetEmail(), &x509s); code != 200 || x509s[sj.GetKeyId()] == "" {
		t.Fatalf("x509 = %d, keys %d, want the signing key", code, len(x509s))
	}
	if err := jws.Verify(sj.GetSignedJwt(), pubOf(x509s[sj.GetKeyId()])); err != nil {
		t.Errorf("jws.Verify against the x509 URL's certificate: %v", err)
	}
	var jwks struct {
		Keys []struct{ Kid, Kty, Alg, N, E string }
	}
	if code := fetch(meta+"/service_accounts/v1/metadata/jwk/"+target.GetEmail(), &jwks); code != 200 {
		t.Fatalf("jwk = %d", code)
	}
	found := false
	for _, k := range jwks.Keys {
		if k.Kid != sj.GetKeyId() {
			continue
		}
		found = true
		n, _ := base64.RawURLEncoding.DecodeString(k.N)
		e, _ := base64.RawURLEncoding.DecodeString(k.E)
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if k.Kty != "RSA" || k.Alg != "RS256" || jws.Verify(sj.GetSignedJwt(), pub) != nil {
			t.Errorf("the JWK %s does not verify the signJwt token", k.Kid)
		}
	}
	if !found {
		t.Errorf("jwk has no key %s", sj.GetKeyId())
	}

	key, err := adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: target.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		ID      string `json:"private_key_id"`
		CertURL string `json:"client_x509_cert_url"`
	}
	if err := json.Unmarshal(key.GetPrivateKeyData(), &file); err != nil {
		t.Fatal("the key file is not JSON")
	}
	if !strings.HasPrefix(file.CertURL, meta+"/robot/v1/metadata/x509/") {
		t.Errorf("client_x509_cert_url is not this instance's robot x509 URL")
	}
	var robot map[string]string
	if code := fetch(file.CertURL, &robot); code != 200 || robot[file.ID] == "" || robot[sj.GetKeyId()] == "" {
		t.Errorf("client_x509_cert_url = %d, %d keys, want the new key and the system key", code, len(robot))
	}
	if err := adm.DeleteServiceAccountKey(ctx, &adminpb.DeleteServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatal(err)
	}
	robot = nil
	if fetch(file.CertURL, &robot); robot[file.ID] != "" {
		t.Error("a deleted key is still served")
	}
	if code := fetch(meta+"/service_accounts/v1/metadata/x509/nobody@"+project+".iam.gserviceaccount.com", nil); code != 404 {
		t.Errorf("an unknown account = %d, want 404", code)
	}
}

// TestIAMServiceAccountKeyFormats (#1750): CreateServiceAccountKey with
// TYPE_PKCS12_FILE returns a PKCS#12 file whose password is "notasecret",
// as Google's reference says: golang.org/x/crypto/pkcs12 decodes it to the
// private key and the certificate GetServiceAccountKey returns; with
// KEY_ALG_RSA_1024 the key is a 1024-bit one, listed so, whose JSON file
// golang.org/x/oauth2/google signs in with. The key material stays in
// memory and is never printed.
func TestIAMServiceAccountKeyFormats(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "sak-")
	sa := newServiceAccount(t, h, project, "formats")

	p12, err := adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName(),
		PrivateKeyType: adminpb.ServiceAccountPrivateKeyType_TYPE_PKCS12_FILE})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey(PKCS12): %v", err)
	}
	if p12.GetPrivateKeyType() != adminpb.ServiceAccountPrivateKeyType_TYPE_PKCS12_FILE || p12.GetKeyAlgorithm() != adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_2048 {
		t.Errorf("PKCS12 key: type %v, algorithm %v", p12.GetPrivateKeyType(), p12.GetKeyAlgorithm())
	}
	priv, cert, err := pkcs12.Decode(p12.GetPrivateKeyData(), "notasecret")
	if err != nil {
		t.Fatalf("pkcs12.Decode with notasecret failed")
	}
	if _, _, err := pkcs12.Decode(p12.GetPrivateKeyData(), "wrong"); err == nil {
		t.Error("pkcs12.Decode took a wrong password")
	}
	got, err := adm.GetServiceAccountKey(ctx, &adminpb.GetServiceAccountKeyRequest{Name: p12.GetName(),
		PublicKeyType: adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(got.GetPublicKeyData())
	rk, ok := priv.(*rsa.PrivateKey)
	if b == nil || !ok || string(b.Bytes) != string(cert.Raw) || rk.PublicKey.N.Cmp(cert.PublicKey.(*rsa.PublicKey).N) != 0 {
		t.Error("the PKCS#12 file's key and certificate are not the key GetServiceAccountKey returns")
	}

	k1024, err := adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName(),
		KeyAlgorithm: adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_1024})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey(RSA 1024): %v", err)
	}
	if k1024.GetKeyAlgorithm() != adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_1024 {
		t.Errorf("RSA 1024 key algorithm = %v", k1024.GetKeyAlgorithm())
	}
	listed, err := adm.ListServiceAccountKeys(ctx, &adminpb.ListServiceAccountKeysRequest{Name: sa.GetName(),
		KeyTypes: []adminpb.ListServiceAccountKeysRequest_KeyType{adminpb.ListServiceAccountKeysRequest_USER_MANAGED}})
	if err != nil {
		t.Fatal(err)
	}
	algs := map[string]adminpb.ServiceAccountKeyAlgorithm{}
	for _, k := range listed.GetKeys() {
		algs[k.GetName()] = k.GetKeyAlgorithm()
	}
	if algs[k1024.GetName()] != adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_1024 || algs[p12.GetName()] != adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_2048 {
		t.Errorf("listed algorithms = %v", algs)
	}
	creds, err := google.CredentialsFromJSON(ctx, k1024.GetPrivateKeyData(), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal("the RSA 1024 key file does not parse")
	}
	if tok, err := creds.TokenSource.Token(); err != nil || tok.AccessToken == "" {
		t.Errorf("signing in with the RSA 1024 key file: %v", err)
	}
}
