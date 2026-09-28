//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The console's Permissions tab (#793): on a queue, a secret, a key ring, a
// key, a bucket and a managed folder (#847), the binding Grant access writes is what the official
// client's GetIamPolicy reads, and a change made with an etag that a change
// through the official client has made stale is refused with the message
// the official client's own SetIamPolicy gets for that etag.

// policyHandle is the official client's IAM for one resource.
type policyHandle struct {
	get func(ctx context.Context) (*iampb.Policy, error)
	set func(ctx context.Context, p *iampb.Policy) error
}

func handleOf(h *iam.Handle) policyHandle {
	return policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			p, err := h.Policy(ctx)
			if err != nil {
				return nil, err
			}
			return p.InternalProto, nil
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			return h.SetPolicy(ctx, &iam.Policy{InternalProto: p})
		},
	}
}

// consolePermissionsTab is the Permissions tab of a console page.
type consolePermissionsTab struct {
	Note, Link, Etag string
	Bindings         []struct {
		Role    string
		Members []string
	}
}

func readConsolePermissions(t *testing.T, addr, service, project string, path []string) consolePermissionsTab {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	var page struct {
		Unavailable string
		Sections    []struct {
			ID, Kind, Unavailable string
			Permissions           *consolePermissionsTab
		}
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	for _, s := range page.Sections {
		if s.ID == "permissions" && s.Kind == "permissions" {
			if s.Unavailable != "" || s.Permissions == nil {
				t.Fatalf("%v: the Permissions tab cannot be read: %q", path, s.Unavailable)
			}
			return *s.Permissions
		}
	}
	t.Fatalf("%v has no Permissions tab (unavailable %q): %s", path, page.Unavailable, body)
	return consolePermissionsTab{}
}

func consoleGrant(t *testing.T, addr, service, project string, path []string, etag, role string, members ...string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Path": path, "Etag": etag,
		"Grant": map[string]any{"Role": role, "Members": members}})
	code, body := consoleDo(t, addr, http.MethodPost, "/api/permissions/"+service+"?project="+url.QueryEscape(project), string(b))
	var e struct{ Error string }
	_ = json.Unmarshal([]byte(body), &e)
	return code, e.Error
}

func holders(p *iampb.Policy, role string) []string {
	for _, b := range p.GetBindings() {
		if b.GetRole() == role {
			return b.GetMembers()
		}
	}
	return nil
}

// consolePermissionsRoundTrip is the acceptance for one resource.
func consolePermissionsRoundTrip(t *testing.T, h *Harness, addr, service string, path []string, c policyHandle, role string) {
	t.Helper()
	ctx := h.Context()
	project := h.Project()
	ada := "user:ada@example.com"

	tab := readConsolePermissions(t, addr, service, project, path)
	if !strings.Contains(tab.Note, "does not enforce") || !strings.Contains(tab.Note, "ADR-0006") || tab.Link == "" {
		t.Errorf("the Permissions tab's note is %q, linking %q; want it to say nothing is enforced", tab.Note, tab.Link)
	}
	if code, msg := consoleGrant(t, addr, service, project, path, tab.Etag, role, ada); code != http.StatusOK {
		t.Fatalf("Grant access = %d: %s", code, msg)
	}
	got, err := c.get(ctx)
	if err != nil {
		t.Fatalf("the official client's GetIamPolicy: %v", err)
	}
	if !slices.Equal(holders(got, role), []string{ada}) {
		t.Fatalf("after Grant access the official client reads %v; want %s holding %s", got.GetBindings(), ada, role)
	}
	page := readConsolePermissions(t, addr, service, project, path)
	if len(page.Bindings) != 1 || !slices.Equal(page.Bindings[0].Members, []string{ada}) {
		t.Errorf("the tab reads %+v after Grant access", page.Bindings)
	}

	// A change through the official client makes the page's etag stale.
	stale := got.GetEtag()
	got.Bindings = append(got.Bindings, &iampb.Binding{Role: "roles/viewer", Members: []string{"group:other@example.com"}})
	if err := c.set(ctx, got); err != nil {
		t.Fatalf("the official client's SetIamPolicy: %v", err)
	}
	got.Etag = stale
	sdkErr := c.set(ctx, got)
	if sdkErr == nil {
		t.Fatal("the official client's SetIamPolicy with a stale etag was accepted")
	}
	st, _ := status.FromError(sdkErr)
	want := st.Code().String() + ": " + st.Message()
	code, msg := consoleGrant(t, addr, service, project, path, page.Etag, role, "user:late@example.com")
	if code != http.StatusBadRequest || msg != want {
		t.Errorf("Grant access with the stale etag = %d %q; want 400 with the official client's own %q", code, msg, want)
	}
	after, err := c.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(holders(after, role), "user:late@example.com") || len(holders(after, "roles/viewer")) != 1 {
		t.Errorf("a refused grant changed the policy: %v", after.GetBindings())
	}
}

// TestConsolePermissionsOnAQueue, in the shards that serve Cloud Tasks.
func TestConsolePermissionsOnAQueue(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	name := queue(t, h, c, "console-perms-q")
	consolePermissionsRoundTrip(t, h, consoleAddr(t, h), "tasks", []string{name}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: p})
			return err
		},
	}, "roles/cloudtasks.enqueuer")
}

// TestConsolePermissionsOnASecret, in the shards that serve Secret Manager.
func TestConsolePermissionsOnASecret(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	s, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "console-perms",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: s.GetName()})
	})
	consolePermissionsRoundTrip(t, h, consoleAddr(t, h), "secrets", []string{s.GetName()}, handleOf(c.IAM(s.GetName())),
		"roles/secretmanager.secretAccessor")
}

// TestConsolePermissionsOnAKeyRingAndKey, in the served shard, whose instance
// serves Cloud KMS: a ring's policy and its key's, each its own.
func TestConsolePermissionsOnAKeyRingAndKey(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Rings and keys cannot be deleted; the project is the test's own.
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "console-perms"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	t.Run("ring", func(t *testing.T) {
		consolePermissionsRoundTrip(t, h, addr, "kms", []string{ring.GetName()}, handleOf(c.ResourceIAM(ring.GetName())),
			"roles/cloudkms.admin")
	})
	t.Run("key", func(t *testing.T) {
		consolePermissionsRoundTrip(t, h, addr, "kms", []string{ring.GetName(), "k"}, handleOf(c.ResourceIAM(key.GetName())),
			"roles/cloudkms.cryptoKeyEncrypterDecrypter")
	})
}

// TestConsolePermissionsOnABucket, in the storage shard: through the JSON
// API's storage.buckets.getIamPolicy and setIamPolicy, whose stale etag is
// refused 412.
func TestConsolePermissionsOnABucket(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := c.Bucket(h.Project() + "-console-perms")
	if err := bh.Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })
	consolePermissionsRoundTrip(t, h, consoleAddr(t, h), "storage", []string{bh.BucketName()}, handleOf(bh.IAM()),
		"roles/storage.objectViewer")
}

// managedFolderHandle is a managed folder's IAM through Google's generated JSON
// API client, managedFolders.getIamPolicy and setIamPolicy: the official
// client has no managed folder call.
func managedFolderHandle(s *storagev1.Service, bucket, folder string) policyHandle {
	return policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			p, err := s.ManagedFolders.GetIamPolicy(bucket, folder).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			out := &iampb.Policy{Version: int32(p.Version), Etag: []byte(p.Etag)}
			for _, b := range p.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out, nil
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			in := &storagev1.Policy{Version: int64(p.GetVersion()), Etag: string(p.GetEtag())}
			for _, b := range p.GetBindings() {
				in.Bindings = append(in.Bindings, &storagev1.PolicyBindings{Role: b.GetRole(), Members: b.GetMembers()})
			}
			_, err := s.ManagedFolders.SetIamPolicy(bucket, folder, in).Context(ctx).Do()
			return err
		},
	}
}

// TestConsolePermissionsOnAManagedFolder, in the storage shard (#847): the
// page of a folder that is a managed folder has a Permissions tab, through the
// JSON API's storage.managedFolders.getIamPolicy and setIamPolicy, whose stale
// etag is refused 412; the bucket's own policy is left alone, and a plain
// folder's page has no tab.
func TestConsolePermissionsOnAManagedFolder(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := managedFolderBucket(t, h, c, s)
	b := bh.BucketName()
	if _, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "team/reports/"}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	addr := consoleAddr(t, h)
	consolePermissionsRoundTrip(t, h, addr, "storage", []string{b, "team", "reports"},
		managedFolderHandle(s, b, "team/reports/"), "roles/storage.objectViewer")

	if bp, err := bh.IAM().Policy(ctx); err != nil || len(bp.Roles()) != 0 {
		t.Errorf("the bucket's policy took the managed folder's bindings: %v, %v", bp.Roles(), err)
	}
	q := url.Values{"project": {h.Project()}, "name": {b, "team"}}
	if code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+q.Encode(), ""); code != http.StatusOK || strings.Contains(body, `"id":"permissions"`) {
		t.Errorf("a plain folder's page = %d, and has a Permissions tab: %s", code, body)
	}
	if code, msg := consoleGrant(t, addr, "storage", h.Project(), []string{b, "team"}, "CAE=", "roles/viewer", "user:ada@example.com"); code != http.StatusBadRequest {
		t.Errorf("a grant on a plain folder = %d %q; want it refused", code, msg)
	}
}
