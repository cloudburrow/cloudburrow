//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// storageAs are the official client's three transports as a minted token's
// account: JSON (metadata and, with WithJSONReads, media), XML (the
// default reads) and gRPC.
type storageAs struct {
	json, xml, grpc *storage.Client
}

func newStorageAs(t *testing.T, h *Harness, token string) storageAs {
	t.Helper()
	endpoint := h.Endpoint(EnvStorage)
	ctx := h.Context()
	hc := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}}
	t.Cleanup(hc.CloseIdleConnections)
	var out storageAs
	var err error
	if out.json, err = storage.NewClient(ctx, option.WithEndpoint(endpoint+"/storage/v1/"), option.WithHTTPClient(hc), storage.WithJSONReads()); err != nil {
		t.Fatal(err)
	}
	if out.xml, err = storage.NewClient(ctx, option.WithEndpoint(endpoint+"/storage/v1/"), option.WithHTTPClient(hc)); err != nil {
		t.Fatal(err)
	}
	if out.grpc, err = storage.NewGRPCClient(ctx, option.WithEndpoint(stripScheme(endpoint)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(bearerCreds(token)))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.json.Close(); _ = out.xml.Close(); _ = out.grpc.Close() })
	return out
}

// read reads bucket/object through c, the payload or the error.
func storageRead(t *testing.T, h *Harness, c *storage.Client, bucket, object string) (string, error) {
	t.Helper()
	r, err := c.Bucket(bucket).Object(object).NewReader(h.Context())
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	return string(b), err
}

// wantStorageDenied checks err is Google's 403 for the transport: the
// JSON API's message, the XML API's AccessDenied with the message in
// Details, gRPC's PERMISSION_DENIED with the message.
func wantStorageDenied(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != msg {
			t.Fatalf("%s = %v; want PERMISSION_DENIED %q", what, err, msg)
		}
		return
	}
	if ge.Code != http.StatusForbidden {
		t.Fatalf("%s = %v; want 403", what, err)
	}
	jsonBody := strings.Contains(ge.Body, `"message":"`+msg+`"`) && strings.Contains(ge.Body, `"reason":"forbidden"`)
	xmlBody := strings.Contains(ge.Body, "<Code>AccessDenied</Code>") && strings.Contains(ge.Body, "<Details>"+strings.ReplaceAll(msg, "'", "&apos;")+"</Details>")
	if ge.Message != msg && !jsonBody && !xmlBody {
		t.Fatalf("%s = %v (body %q); want %q", what, err, ge.Body, msg)
	}
}

// covers: storage.objects.get, storage.objects.list, storage.objects.insert, storage.buckets.getIamPolicy, storage.buckets.setIamPolicy, storage.buckets.testIamPermissions, storage.objectAccessControls.insert
// covers: google.storage.v2.Storage/ReadObject
//
// TestIAMEnforcementStorage (#1879), with the official Go client on its
// JSON, XML and gRPC transports and a token minted for a service account
// of the test's own: without a role, reading an object is Google's 403
// "<account> does not have storage.objects.get access to the Google Cloud
// Storage object. Permission 'storage.objects.get' denied on resource (or
// it may not exist)." on all three; roles/storage.objectViewer on the
// bucket's policy lets each read, and testIamPermissions answers only
// storage.objects.get of what is asked; the revoke refuses again; the role
// on the project is inherited; allUsers on the bucket lets the account read
// (allUsers is everyone); an object ACL grants on a bucket without uniform
// bucket-level access and not with it; a write stays denied to a viewer; a
// token the instance did not mint is 401; the instance's own account reads.
func TestIAMEnforcementStorage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iest-")
	owner := storageClient(t, h)
	bucket := project + "-enforced"
	bh := owner.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("Create bucket as the instance's account: %v", err)
	}
	t.Cleanup(func() {
		_ = bh.Object("a.txt").Delete(ctx)
		_ = bh.Delete(ctx)
	})
	w := bh.Object("a.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatalf("write as the instance's account: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iest")
	as := newStorageAs(t, h, p.token)
	msg := p.email + " does not have storage.objects.get access to the Google Cloud Storage object. Permission 'storage.objects.get' denied on resource (or it may not exist)."
	readAll := func(what string) {
		t.Helper()
		for name, c := range map[string]*storage.Client{"JSON": as.json, "XML": as.xml, "gRPC": as.grpc} {
			waitGranted(t, what+" over "+name, func() error {
				got, err := storageRead(t, h, c, bucket, "a.txt")
				if err == nil && got != "hello" {
					t.Fatalf("%s over %s read %q", what, name, got)
				}
				return err
			})
		}
	}
	deniedAll := func(what string) {
		t.Helper()
		for name, c := range map[string]*storage.Client{"JSON": as.json, "XML": as.xml, "gRPC": as.grpc} {
			_, err := storageRead(t, h, c, bucket, "a.txt")
			wantStorageDenied(t, what+" over "+name, err, msg)
		}
		_, err := as.json.Bucket(bucket).Object("a.txt").Attrs(ctx)
		wantStorageDenied(t, what+": objects.get metadata", err, msg)
	}
	deniedAll("read without a role")

	setBucketRole := func(member string, add bool) {
		t.Helper()
		pol, err := bh.IAM().Policy(ctx)
		if err != nil {
			t.Fatalf("getIamPolicy: %v", err)
		}
		if add {
			pol.Add(member, "roles/storage.objectViewer")
		} else {
			pol.Remove(member, "roles/storage.objectViewer")
		}
		if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
			t.Fatalf("setIamPolicy: %v", err)
		}
	}
	setBucketRole("serviceAccount:"+p.email, true)
	readAll("read with objectViewer on the bucket")
	held, err := as.json.Bucket(bucket).IAM().TestPermissions(ctx, []string{"storage.objects.get", "storage.objects.delete"})
	if err != nil || len(held) != 1 || held[0] != "storage.objects.get" {
		t.Errorf("testIamPermissions as objectViewer = %v, %v; want only storage.objects.get", held, err)
	}
	ow := as.json.Bucket(bucket).Object("b.txt").NewWriter(ctx)
	_, _ = ow.Write([]byte("x"))
	err = ow.Close()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusForbidden || !strings.Contains(ge.Message, "does not have storage.objects.create access") {
		t.Errorf("write as objectViewer = %v; want 403 storage.objects.create", err)
	}
	setBucketRole("serviceAccount:"+p.email, false)
	deniedAll("read after the revoke")

	p.grant("roles/storage.objectViewer")
	readAll("read with objectViewer on the project")
	p.revoke("roles/storage.objectViewer")
	deniedAll("read after the project's revoke")

	setBucketRole(string(iam.AllUsers), true)
	readAll("read with allUsers on the bucket")
	setBucketRole(string(iam.AllUsers), false)
	deniedAll("read after allUsers is gone")

	// ACLs grant only without uniform bucket-level access.
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: false}}); err != nil {
		t.Fatalf("disable uniform bucket-level access: %v", err)
	}
	if err := bh.Object("a.txt").ACL().Set(ctx, storage.ACLEntity("user-"+p.email), storage.RoleReader); err != nil {
		t.Fatalf("object ACL: %v", err)
	}
	waitGranted(t, "read with an object ACL", func() error {
		_, err := storageRead(t, h, as.xml, bucket, "a.txt")
		return err
	})
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("enable uniform bucket-level access: %v", err)
	}
	deniedAll("read with an object ACL under uniform bucket-level access")

	forged := newStorageAs(t, h, "cbl_not-minted-here")
	if _, err := forged.json.Bucket(bucket).Object("a.txt").Attrs(ctx); !errors.As(err, &ge) || ge.Code != http.StatusUnauthorized {
		t.Errorf("objects.get with a token the instance did not mint = %v; want 401", err)
	}
	if got, err := storageRead(t, h, owner, bucket, "a.txt"); err != nil || got != "hello" {
		t.Errorf("read as the instance's account = %q, %v", got, err)
	}
}
