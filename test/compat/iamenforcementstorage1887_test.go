//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	credentialspb "cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
)

// batchGet posts one batch of a GET of each path through the batch
// endpoint, with token in the outer request only ("" for none), and
// returns each part's status.
func batchGet(t *testing.T, h *Harness, token string, paths ...string) []int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i, p := range paths {
		pw, err := mw.CreatePart(map[string][]string{"Content-Type": {"application/http"}, "Content-ID": {fmt.Sprintf("<%d>", i)}})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(pw, "GET %s HTTP/1.1\r\n\r\n", p)
	}
	_ = mw.Close()
	req, err := http.NewRequestWithContext(h.Context(), http.MethodPost, h.Endpoint(EnvStorage)+"/batch/storage/v1", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := &http.Client{}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	defer resp.Body.Close()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("batch answer %d %s: %v", resp.StatusCode, resp.Header.Get("Content-Type"), err)
	}
	var codes []int
	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("batch answer: %v", err)
		}
		raw, _ := io.ReadAll(part)
		var code int
		if _, err := fmt.Sscanf(string(raw), "HTTP/1.1 %d", &code); err != nil {
			t.Fatalf("batch part %q: %v", raw, err)
		}
		codes = append(codes, code)
	}
	return codes
}

// covers: storage.buckets.getStorageLayout, storage.projects.serviceAccount.get
//
// TestIAMEnforcementStorageSignersAndBatch (#1887): a V4 signed URL
// (signed through the official IAM Credentials client's SignBlob for a
// service account of the test's own, by the official storage client's
// SignedURL) is checked as its signer: 403 AccessDenied naming the account
// until roles/storage.objectViewer on the bucket lets it read; the batch
// endpoint checks each nested request as the outer request's
// Authorization, as Google documents, while a nested call of the
// instance's own account reads; buckets.getStorageLayout (storage.objects.list)
// and projects.serviceAccount.get (resourcemanager.projects.get), through
// the official google.golang.org/api/storage/v1 client, are denied without
// a role and allowed with Viewer on the project.
func TestIAMEnforcementStorageSignersAndBatch(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvStorage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iesb-")
	owner := storageClient(t, h)
	bucket := project + "-signed"
	bh := owner.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("Create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = bh.Object("a.txt").Delete(ctx)
		_ = bh.Delete(ctx)
	})
	putObject(t, ctx, bh.Object("a.txt"), "hello")
	p := newIAMPrincipal(t, h, project, "iesb")
	_, ts := instanceAccount(t)
	ic := iamCredentialsAs(t, h, ts)

	// A signed URL, as its signer.
	host := strings.TrimPrefix(endpoint, "http://")
	u, err := storage.SignedURL(bucket, "a.txt", &storage.SignedURLOptions{GoogleAccessID: p.email, Method: "GET",
		Expires: time.Now().Add(10 * time.Minute), Scheme: storage.SigningSchemeV4, Hostname: host, Insecure: true,
		SignBytes: func(b []byte) ([]byte, error) {
			resp, err := ic.SignBlob(ctx, &credentialspb.SignBlobRequest{Name: "projects/-/serviceAccounts/" + p.email, Payload: b})
			if err != nil {
				return nil, err
			}
			return resp.GetSignedBlob(), nil
		}})
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	u = strings.Replace(u, "https://", "http://", 1)
	msg := p.email + " does not have storage.objects.get access to the Google Cloud Storage object. Permission &apos;storage.objects.get&apos; denied on resource (or it may not exist)."
	if resp, body := xmlCall(t, h, "GET", u, "", nil); resp.StatusCode != http.StatusForbidden || xmlErrorCode(body) != "AccessDenied" ||
		!strings.Contains(body, "<Details>"+msg+"</Details>") {
		t.Fatalf("a signed URL of an account without the role = %d %s", resp.StatusCode, body)
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("serviceAccount:"+p.email, "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatal(err)
	}
	waitGranted(t, "a signed URL of an objectViewer", func() error {
		if resp, body := xmlCall(t, h, "GET", u, "", nil); resp.StatusCode != http.StatusOK || body != "hello" {
			return fmt.Errorf("%d %s", resp.StatusCode, body)
		}
		return nil
	})
	if pol, err = bh.IAM().Policy(ctx); err != nil {
		t.Fatal(err)
	}
	pol.Remove("serviceAccount:"+p.email, "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatal(err)
	}

	// The batch endpoint: each nested call as the outer Authorization.
	objPath := "/storage/v1/b/" + bucket + "/o/a.txt"
	if got := batchGet(t, h, p.token, objPath, "/storage/v1/b/"+bucket); len(got) != 2 || got[0] != http.StatusForbidden || got[1] != http.StatusForbidden {
		t.Errorf("a batch as an account without a role = %v; want [403 403]", got)
	}
	if got := batchGet(t, h, "", objPath); len(got) != 1 || got[0] != http.StatusOK {
		t.Errorf("a batch as the instance's account = %v; want [200]", got)
	}

	// The methods round 2 left unmapped.
	hc := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}
	t.Cleanup(hc.CloseIdleConnections)
	sv, err := storagev1.NewService(ctx, option.WithEndpoint(endpoint+"/storage/v1/"), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	layout := func() error { _, err := sv.Buckets.GetStorageLayout(bucket).Context(ctx).Do(); return err }
	account := func() error { _, err := sv.Projects.ServiceAccount.Get(project).Context(ctx).Do(); return err }
	for what, call := range map[string]func() error{"getStorageLayout": layout, "serviceAccount.get": account} {
		var ge *googleapi.Error
		if err := call(); !errors.As(err, &ge) || ge.Code != http.StatusForbidden {
			t.Errorf("%s without a role = %v; want 403", what, err)
		}
	}
	p.grant("roles/viewer")
	defer p.revoke("roles/viewer")
	waitGranted(t, "getStorageLayout as a project viewer", layout)
	waitGranted(t, "serviceAccount.get as a project viewer", account)
}
