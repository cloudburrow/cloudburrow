//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2/google"
)

// TestStorageSignedURLIAMServiceAccount (#1787): on Google, a URL an IAM
// service account signs with its own keys verifies against the account's
// public certificates (www.googleapis.com/service_accounts/v1/metadata/
// x509/EMAIL): its system-managed key, which IAM Credentials signBlob signs
// with, and its user-managed keys, which a downloaded key file holds
// (docs.cloud.google.com/storage/docs/access-control/signing-urls-with-helpers,
// "use the IAM signBlob method"). Here the official storage client's
// SignedURL signs V2 and V4 URLs with SignBytes through the official IAM
// Credentials client's SignBlob, and again with a key file's private key
// from the official IAM admin client's CreateServiceAccountKey; plain HTTP
// reads the object through each. A disabled key's, a deleted key's and a
// deleted account's URLs are 403, as their certificates are no longer
// published.
// covers: storage.objects.insert, google.iam.admin.v1.IAM/CreateServiceAccountKey, google.iam.admin.v1.IAM/DisableServiceAccountKey, google.iam.admin.v1.IAM/EnableServiceAccountKey, google.iam.admin.v1.IAM/DeleteServiceAccountKey, google.iam.admin.v1.IAM/DeleteServiceAccount
func TestStorageSignedURLIAMServiceAccount(t *testing.T) {
	h := New(t)
	storageEndpoint := h.Endpoint(EnvStorage)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	c := storageClient(t, h)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	putObject(t, ctx, bh.Object("dir/secret.txt"), "classified")

	project := iamProject(t, h, "ssu-")
	me, ts := instanceAccount(t)
	ic := iamCredentialsAs(t, h, ts)
	target := newServiceAccount(t, h, project, "signer")
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+me)
	host := strings.TrimPrefix(storageEndpoint, "http://")

	signBytes := func(b []byte) ([]byte, error) {
		resp, err := ic.SignBlob(ctx, &credentialspb.SignBlobRequest{Name: "projects/-/serviceAccounts/" + target.GetEmail(), Payload: b})
		if err != nil {
			return nil, err
		}
		return resp.GetSignedBlob(), nil
	}
	sign := func(opts storage.SignedURLOptions, sc storage.SigningScheme) string {
		t.Helper()
		opts.GoogleAccessID, opts.Method, opts.Expires = target.GetEmail(), "GET", time.Now().Add(10*time.Minute)
		opts.Scheme, opts.Hostname, opts.Insecure = sc, host, true
		u, err := storage.SignedURL(bh.BucketName(), "dir/secret.txt", &opts)
		if err != nil {
			t.Fatalf("SignedURL: %v", err)
		}
		return strings.Replace(u, "https://", "http://", 1)
	}
	get := func(u string) (int, string) {
		t.Helper()
		resp, body := xmlCall(t, h, "GET", u, "", nil)
		return resp.StatusCode, body
	}
	schemes := []storage.SigningScheme{storage.SigningSchemeV2, storage.SigningSchemeV4}

	// signBlob: the system-managed key.
	var blobURLs []string
	for _, sc := range schemes {
		u := sign(storage.SignedURLOptions{SignBytes: signBytes}, sc)
		blobURLs = append(blobURLs, u)
		if code, body := get(u); code != http.StatusOK || body != "classified" {
			t.Errorf("scheme %d, signed by signBlob: GET = %d %s", sc, code, body)
		}
	}
	tampered := strings.Replace(blobURLs[1], "X-Goog-Signature=", "X-Goog-Signature=00", 1)
	if code, body := get(tampered); code != http.StatusForbidden || xmlErrorCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("a tampered signBlob URL = %d %s; want 403 SignatureDoesNotMatch", code, body)
	}

	// A user-managed key, from its downloaded key file.
	key, err := adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: target.GetName()})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey: %v", err)
	}
	jwt, err := google.JWTConfigFromJSON(key.GetPrivateKeyData())
	if err != nil || jwt.Email != target.GetEmail() {
		t.Fatalf("the key file does not parse as the account's (%v)", err)
	}
	fileURLs := map[storage.SigningScheme]string{}
	for _, sc := range schemes {
		u := sign(storage.SignedURLOptions{PrivateKey: jwt.PrivateKey}, sc)
		fileURLs[sc] = u
		if code, body := get(u); code != http.StatusOK || body != "classified" {
			t.Errorf("scheme %d, signed by the key file: GET = %d %s", sc, code, body)
		}
	}

	// A disabled key verifies nothing; enabled again, it does.
	if err := adm.DisableServiceAccountKey(ctx, &adminpb.DisableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatalf("DisableServiceAccountKey: %v", err)
	}
	for sc, u := range fileURLs {
		if code, body := get(u); code != http.StatusForbidden {
			t.Errorf("scheme %d, a disabled key's URL = %d %s; want 403", sc, code, body)
		}
	}
	if code, body := get(sign(storage.SignedURLOptions{PrivateKey: jwt.PrivateKey}, storage.SigningSchemeV4)); code != http.StatusForbidden {
		t.Errorf("a URL signed after the key was disabled = %d %s; want 403", code, body)
	}
	if code, body := get(blobURLs[1]); code != http.StatusOK || body != "classified" {
		t.Errorf("the system key's URL while a user key is disabled = %d %s", code, body)
	}
	if err := adm.EnableServiceAccountKey(ctx, &adminpb.EnableServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatalf("EnableServiceAccountKey: %v", err)
	}
	if code, body := get(fileURLs[storage.SigningSchemeV4]); code != http.StatusOK {
		t.Errorf("the key's URL once enabled again = %d %s", code, body)
	}

	// A deleted key verifies nothing.
	if err := adm.DeleteServiceAccountKey(ctx, &adminpb.DeleteServiceAccountKeyRequest{Name: key.GetName()}); err != nil {
		t.Fatalf("DeleteServiceAccountKey: %v", err)
	}
	for sc, u := range fileURLs {
		if code, body := get(u); code != http.StatusForbidden {
			t.Errorf("scheme %d, a deleted key's URL = %d %s; want 403", sc, code, body)
		}
	}

	// A deleted account's URLs, signed while it lived, verify nothing.
	if err := adm.DeleteServiceAccount(context.Background(), &adminpb.DeleteServiceAccountRequest{Name: target.GetName()}); err != nil {
		t.Fatalf("DeleteServiceAccount: %v", err)
	}
	for i, u := range blobURLs {
		if code, body := get(u); code != http.StatusForbidden {
			t.Errorf("URL %d of a deleted account = %d %s; want 403", i, code, body)
		}
	}
}
