//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// storageCMEKKeys makes a key ring in the us location with the named keys, and
// grants the project's Cloud Storage service agent
// roles/cloudkms.cryptoKeyEncrypterDecrypter on each, as
// using-customer-managed-keys has it done (`gcloud storage service-agent
// --authorize-cmek`).
func storageCMEKKeys(t *testing.T, h *Harness, c *storage.Client, ring string, names ...string) (*kms.KeyManagementClient, []string) {
	t.Helper()
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]
	r, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us", KeyRingId: ring})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	agent, err := c.ServiceAccount(ctx, h.Project())
	if err != nil {
		t.Fatalf("ServiceAccount: %v", err)
	}
	var keys []string
	for _, n := range names {
		key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: n,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatalf("CreateCryptoKey %s: %v", n, err)
		}
		ih := k.ResourceIAM(key.GetName())
		p, err := ih.Policy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.Add("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
		if err := ih.SetPolicy(ctx, p); err != nil {
			t.Fatalf("grant the service agent on %s: %v", n, err)
		}
		keys = append(keys, key.GetName())
	}
	return k, keys
}

func cmekWrite(h *Harness, o *storage.ObjectHandle, key, body string) (*storage.ObjectAttrs, error) {
	w := o.NewWriter(h.Context())
	w.KMSKeyName = key
	if _, err := io.WriteString(w, body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return w.Attrs(), nil
}

func cmekRead(h *Harness, o *storage.ObjectHandle) (string, error) {
	r, err := o.NewReader(h.Context())
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	return string(b), err
}

// refusedCode is an error's HTTP status, or its gRPC code's.
func refusedCode(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return fmt.Sprint(ge.Code)
	}
	if st, ok := status.FromError(err); ok && err != nil {
		return st.Code().String()
	}
	return fmt.Sprint(err)
}

// TestStorageCMEK (#1938): customer-managed encryption keys from the
// instance's Cloud KMS, with the official Go client on JSON and gRPC: a
// bucket's default key and a write's own key (Writer.KMSKeyName), the
// object's kmsKeyName with its version, rotation (new writes use the new
// primary, old objects keep theirs), a disabled version refusing reads and
// rewrites but not metadata, a disabled primary refusing writes, a rewrite
// re-encrypting with Copier.DestinationKMSKeyName, and a compose under the
// default key. Measured first: kmsKeyName was 501.
//
// covers: storage.objects.insert, storage.objects.get, storage.objects.rewrite, storage.objects.compose
// covers: storage.buckets.insert, storage.buckets.get
// covers: google.storage.v2.Storage/WriteObject, google.storage.v2.Storage/ReadObject, google.storage.v2.Storage/GetObject
func TestStorageCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	// Cloud KMS as the storage shard's CMEK_KMS, so the KMS suite stays in
	// served.
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	j := storageClient(t, h)
	g := storageGRPCClient(t, h, false)
	ctx := h.Context()
	k, keys := storageCMEKKeys(t, h, j, "storage-cmek", "default", "other")
	def, other := keys[0], keys[1]
	name := h.Project() + "-cmek"
	jb := j.Bucket(name)
	if err := jb.Create(ctx, h.Project(), &storage.BucketAttrs{Location: "US", Encryption: &storage.BucketEncryption{DefaultKMSKeyName: def}}); err != nil {
		t.Fatalf("create a bucket with a default key: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, jb) })
	gb := g.Bucket(name)
	if a, err := gb.Attrs(ctx); err != nil || a.Encryption == nil || a.Encryption.DefaultKMSKeyName != def {
		t.Fatalf("gRPC bucket attrs = %+v, %v; want default key %s", a, err, def)
	}
	v1 := def + "/cryptoKeyVersions/1"
	for tr, b := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		a, err := cmekWrite(h, b.Object("default-"+tr), "", "by default over "+tr)
		if err != nil || a.KMSKeyName != v1 {
			t.Fatalf("%s write under the default key = %+v, %v; want kmsKeyName %s", tr, a, err, v1)
		}
		a, err = cmekWrite(h, b.Object("explicit-"+tr), other, "explicit over "+tr)
		if err != nil || a.KMSKeyName != other+"/cryptoKeyVersions/1" {
			t.Fatalf("%s write with its own key = %+v, %v", tr, a, err)
		}
	}
	for tr, b := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		for _, src := range []string{"JSON", "gRPC"} {
			if got, err := cmekRead(h, b.Object("default-"+src)); err != nil || got != "by default over "+src {
				t.Errorf("%s read of the %s write = %q, %v", tr, src, got, err)
			}
		}
		if a, err := b.Object("explicit-JSON").Attrs(ctx); err != nil || a.KMSKeyName != other+"/cryptoKeyVersions/1" {
			t.Errorf("%s attrs = %+v, %v", tr, a, err)
		}
	}
	// Rotation.
	if _, err := k.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: def, CryptoKeyVersion: &kmspb.CryptoKeyVersion{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: def, CryptoKeyVersionId: "2"}); err != nil {
		t.Fatal(err)
	}
	v2 := def + "/cryptoKeyVersions/2"
	if a, err := cmekWrite(h, jb.Object("rotated"), "", "after rotation"); err != nil || a.KMSKeyName != v2 {
		t.Fatalf("write after rotation = %+v, %v; want kmsKeyName %s", a, err, v2)
	}
	if a, err := jb.Object("default-JSON").Attrs(ctx); err != nil || a.KMSKeyName != v1 {
		t.Errorf("an old object after rotation = %+v, %v; want %s still", a, err, v1)
	}
	// A disabled version: no reads or rewrites of its objects, on either
	// transport; metadata and the other version's objects still served.
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_DISABLED)
	for tr, b := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		if _, err := cmekRead(h, b.Object("default-JSON")); err == nil {
			t.Errorf("%s read with its version disabled succeeded", tr)
		} else if c := refusedCode(err); c != "400" && c != codes.InvalidArgument.String() && c != codes.FailedPrecondition.String() {
			t.Errorf("%s read with its version disabled = %v; want 400", tr, err)
		}
		if _, err := b.Object("default-JSON").Attrs(ctx); err != nil {
			t.Errorf("%s metadata with the version disabled = %v; want served", tr, err)
		}
		if got, err := cmekRead(h, b.Object("rotated")); err != nil || got != "after rotation" {
			t.Errorf("%s read of a version 2 object = %q, %v", tr, got, err)
		}
	}
	if _, err := jb.Object("copy").CopierFrom(jb.Object("default-JSON")).Run(ctx); err == nil {
		t.Error("rewrite with the source's version disabled succeeded")
	}
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_ENABLED)
	// A disabled primary cannot encrypt.
	setVersionState(t, h, k, v2, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := cmekWrite(h, jb.Object("refused"), "", "x"); err == nil {
		t.Error("write with the default key's primary disabled succeeded")
	}
	setVersionState(t, h, k, v2, kmspb.CryptoKeyVersion_ENABLED)
	// A rewrite to another key, over both transports.
	for tr, b := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		c := b.Object("rekeyed-"+tr).CopierFrom(b.Object("default-JSON"))
		c.DestinationKMSKeyName = other
		a, err := c.Run(ctx)
		if err != nil || a.KMSKeyName != other+"/cryptoKeyVersions/1" {
			t.Fatalf("%s rewrite with DestinationKMSKeyName = %+v, %v", tr, a, err)
		}
		if got, err := cmekRead(h, b.Object("rekeyed-"+tr)); err != nil || got != "by default over JSON" {
			t.Errorf("%s rewritten object = %q, %v", tr, got, err)
		}
	}
	// A compose takes the bucket's default key's primary.
	a, err := jb.Object("composed").ComposerFrom(jb.Object("explicit-JSON"), jb.Object("rotated")).Run(ctx)
	if err != nil || a.KMSKeyName != v2 {
		t.Fatalf("compose = %+v, %v; want kmsKeyName %s", a, err, v2)
	}
	if got, err := cmekRead(h, jb.Object("composed")); err != nil || got != "explicit over JSONafter rotation" {
		t.Errorf("composed = %q, %v", got, err)
	}
}

// TestStorageEncryptionEnforcement (#1938): a bucket's
// googleManagedEncryptionEnforcementConfig,
// customerManagedEncryptionEnforcementConfig and
// customerSuppliedEncryptionEnforcementConfig, with the official Go
// client: FullyRestricted refuses new objects of that type, effectiveTime
// is stamped, an omitted config is kept on update, and at least one type
// must stay allowed (enforce-encryption-types).
//
// covers: storage.buckets.insert, storage.buckets.patch, storage.objects.insert
func TestStorageEncryptionEnforcement(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	j := storageClient(t, h)
	ctx := h.Context()
	_, keys := storageCMEKKeys(t, h, j, "storage-enforce", "k")
	name := h.Project() + "-enforce"
	b := j.Bucket(name)
	if err := b.Create(ctx, h.Project(), &storage.BucketAttrs{Location: "US",
		GoogleManagedEncryptionEnforcementConfig: &storage.EncryptionEnforcementConfig{RestrictionMode: storage.FullyRestricted}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, b) })
	a, err := b.Attrs(ctx)
	if err != nil || a.GoogleManagedEncryptionEnforcementConfig == nil || a.GoogleManagedEncryptionEnforcementConfig.RestrictionMode != storage.FullyRestricted ||
		a.GoogleManagedEncryptionEnforcementConfig.EffectiveTime.IsZero() {
		t.Fatalf("enforcement config = %+v, %v", a.GoogleManagedEncryptionEnforcementConfig, err)
	}
	if _, err := cmekWrite(h, b.Object("gmek"), "", "x"); err == nil {
		t.Error("a Google-managed write to a bucket restricting it succeeded")
	}
	if _, err := cmekWrite(h, b.Object("cmek"), keys[0], "x"); err != nil {
		t.Errorf("a CMEK write = %v; want allowed", err)
	}
	if _, err := b.Update(ctx, storage.BucketAttrsToUpdate{
		CustomerManagedEncryptionEnforcementConfig: &storage.EncryptionEnforcementConfig{RestrictionMode: storage.FullyRestricted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekWrite(h, b.Object("cmek2"), keys[0], "x"); err == nil {
		t.Error("a CMEK write after restricting CMEK succeeded")
	}
	if _, err := cmekWrite(h, b.Object("gmek2"), "", "x"); err == nil {
		t.Error("a Google-managed write after the update succeeded: the omitted config was not kept")
	}
	w := b.Object("csek").Key(bytes.Repeat([]byte{3}, 32)).NewWriter(ctx)
	_, _ = io.WriteString(w, "x")
	if err := w.Close(); err != nil {
		t.Errorf("a CSEK write = %v; want allowed", err)
	}
	_, err = b.Update(ctx, storage.BucketAttrsToUpdate{
		CustomerSuppliedEncryptionEnforcementConfig: &storage.EncryptionEnforcementConfig{RestrictionMode: storage.FullyRestricted}})
	if refusedCode(err) != "400" {
		t.Errorf("restricting every type = %v; want 400", err)
	}
}

// TestGcloudStorageCMEK (#1938): `gcloud storage buckets create
// --default-encryption-key`, `gcloud storage cp --encryption-key`, `gcloud
// storage objects describe` naming the key version, `gcloud storage
// buckets update --clear-default-encryption-key`, through the harness.
func TestGcloudStorageCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	j := storageClient(t, h)
	_, keys := storageCMEKKeys(t, h, j, "gcloud-cmek", "default", "other")
	g := newGcloudSession(t, h)
	name := h.Project() + "-gcmek"
	g.must("storage", "buckets", "create", "gs://"+name, "--location=US", "--default-encryption-key="+keys[0])
	t.Cleanup(func() { _, _ = g.run(nil, "storage", "rm", "-r", "gs://"+name) })
	if out := g.must("storage", "buckets", "describe", "gs://"+name, "--format=value(default_kms_key)"); strings.TrimSpace(out) != keys[0] {
		t.Errorf("default_kms_key = %q; want %s", out, keys[0])
	}
	src := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(src, []byte("gcloud cmek"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("storage", "cp", src, "gs://"+name+"/default.txt")
	g.must("storage", "cp", src, "gs://"+name+"/other.txt", "--encryption-key="+keys[1])
	for obj, key := range map[string]string{"default.txt": keys[0], "other.txt": keys[1]} {
		out := g.must("storage", "objects", "describe", "gs://"+name+"/"+obj, "--format=value(kms_key)")
		if strings.TrimSpace(out) != key+"/cryptoKeyVersions/1" {
			t.Errorf("%s kms_key = %q; want %s/cryptoKeyVersions/1", obj, out, key)
		}
		if out := g.must("storage", "cat", "gs://"+name+"/"+obj); out != "gcloud cmek" {
			t.Errorf("cat %s = %q", obj, out)
		}
	}
	g.must("storage", "buckets", "update", "gs://"+name, "--clear-default-encryption-key")
	a, err := j.Bucket(name).Attrs(h.Context())
	if err != nil || (a.Encryption != nil && a.Encryption.DefaultKMSKeyName != "") {
		t.Errorf("after --clear-default-encryption-key: %+v, %v", a.Encryption, err)
	}
}

// TestTerraformStorageBucketCMEK (#1938): google_storage_bucket with
// encryption { default_kms_key_name } applies, plans clean and destroys,
// and an object written into it is encrypted with that key.
func TestTerraformStorageBucketCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	j := storageClient(t, h)
	_, keys := storageCMEKKeys(t, h, j, "tf-cmek", "k")
	m := newTFModule(t)
	name := m.project + "-tfcmek-" + fmt.Sprint(time.Now().UnixNano()%1e6)
	m.write(fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project       = %q
  name          = %q
  location      = "US"
  force_destroy = true
  encryption {
    default_kms_key_name = %q
  }
}
`, m.project, name, keys[0]))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	a, err := j.Bucket(name).Attrs(h.Context())
	if err != nil || a.Encryption == nil || a.Encryption.DefaultKMSKeyName != keys[0] {
		t.Fatalf("after apply: %+v, %v", a, err)
	}
	if oa, err := cmekWrite(h, j.Bucket(name).Object("o"), "", "x"); err != nil || oa.KMSKeyName != keys[0]+"/cryptoKeyVersions/1" {
		t.Errorf("object in the bucket = %+v, %v", oa, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = j.Bucket(name).Attrs(h.Context())
	if !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("after destroy: %v", err)
	}
}

// TestIAMEnforcementStorageCMEK (#1938): with --iam-enforcement=on, the
// Cloud Storage service agent must hold
// roles/cloudkms.cryptoKeyEncrypterDecrypter on the key: without it a
// write is refused 403, with it the write and read go through, and once it
// is removed the read is refused.
func TestIAMEnforcementStorageCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	requireIAMEnforcement(t)
	ctx := h.Context()
	j := storageClient(t, h)
	k := kmsClients(t, h)["grpc"]
	r, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us", KeyRingId: "iam-cmek"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	b := j.Bucket(h.Project() + "-iamcmek")
	if err := b.Create(ctx, h.Project(), &storage.BucketAttrs{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, b) })
	if _, err := cmekWrite(h, b.Object("o"), key.GetName(), "x"); refusedCode(err) != fmt.Sprint(http.StatusForbidden) {
		t.Fatalf("write without the service agent's grant = %v; want 403", err)
	}
	agent, err := j.ServiceAccount(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	ih := k.ResourceIAM(key.GetName())
	p, err := ih.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekWrite(h, b.Object("o"), key.GetName(), "granted"); err != nil {
		t.Fatalf("write with the grant = %v", err)
	}
	if got, err := cmekRead(h, b.Object("o")); err != nil || got != "granted" {
		t.Errorf("read with the grant = %q, %v", got, err)
	}
	p.Remove("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekRead(h, b.Object("o")); refusedCode(err) != fmt.Sprint(http.StatusForbidden) {
		t.Errorf("read after the grant is removed = %v; want 403", err)
	}
}
