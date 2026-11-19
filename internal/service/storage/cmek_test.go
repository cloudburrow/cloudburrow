package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// cmekFixture is a storage server whose Cloud KMS is an in-process one,
// with blobs on disk so a test can look at what is at rest.
type cmekFixture struct {
	client  *gcs.Client
	kms     kmspb.KeyManagementServiceClient
	blobs   string
	kmsAddr string
}

func newCMEKFixture(t *testing.T, check ...*iamcheck.Client) *cmekFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	kms.NewServer(store.NewMemory()).Register(g)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	keys, closer, err := DialKMS(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	dir := t.TempDir()
	blobs, err := OpenFileBlobStore(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{KMS: keys, Blobs: blobs}
	if len(check) > 0 {
		o.IAMCheck = check[0]
	}
	s, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	c, err := gcs.NewClient(context.Background(), option.WithEndpoint(h.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &cmekFixture{client: c, kms: kmspb.NewKeyManagementServiceClient(conn), blobs: dir, kmsAddr: ln.Addr().String()}
}

func (f *cmekFixture) key(t *testing.T, ring, name string) string {
	t.Helper()
	ctx := context.Background()
	parent := "projects/proj-cmek/locations/us"
	if _, err := f.kms.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring}); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		t.Fatal(err)
	}
	k, err := f.kms.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: name,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return k.GetName()
}

func (f *cmekFixture) setState(t *testing.T, version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := f.kms.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

func cmekPut(t *testing.T, o *gcs.ObjectHandle, key string, data []byte) (*gcs.ObjectAttrs, error) {
	t.Helper()
	w := o.NewWriter(context.Background())
	w.KMSKeyName = key
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return w.Attrs(), nil
}

func get(o *gcs.ObjectHandle) ([]byte, error) {
	r, err := o.NewReader(context.Background())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func code(err error) int {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// atRest reports whether any stored blob holds needle.
func atRest(t *testing.T, dir string, needle []byte) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	return found
}

// An object written with a key, or into a bucket with a default key, is
// sealed at rest, reports its key version, reads back whole and by range,
// and stops reading while that version is disabled; a rotated key's new
// writes use the new primary while older objects keep theirs (#1938).
func TestCMEKObjectLifecycle(t *testing.T) {
	f := newCMEKFixture(t)
	ctx := context.Background()
	def := f.key(t, "ring", "default")
	other := f.key(t, "ring", "other")
	b := f.client.Bucket("cmek-bucket")
	if err := b.Create(ctx, "p", &gcs.BucketAttrs{Location: "US", Encryption: &gcs.BucketEncryption{DefaultKMSKeyName: def}}); err != nil {
		t.Fatal(err)
	}
	attrs, err := b.Attrs(ctx)
	if err != nil || attrs.Encryption == nil || attrs.Encryption.DefaultKMSKeyName != def {
		t.Fatalf("bucket attrs = %+v, %v; want default key %s", attrs, err, def)
	}
	secret := bytes.Repeat([]byte("plaintext-marker-0123456789/"), 10000) // 280 KB: several segments
	a, err := cmekPut(t, b.Object("by-default"), "", secret)
	if err != nil {
		t.Fatal(err)
	}
	if want := def + "/cryptoKeyVersions/1"; a.KMSKeyName != want {
		t.Errorf("default-key object kmsKeyName = %q; want %q", a.KMSKeyName, want)
	}
	if atRest(t, f.blobs, []byte("plaintext-marker")) {
		t.Fatal("the object's plaintext is at rest; want only ciphertext")
	}
	got, err := get(b.Object("by-default"))
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("read = %d bytes, %v; want the %d bytes written", len(got), err, len(secret))
	}
	r, err := b.Object("by-default").NewRangeReader(ctx, 65530, 20)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(part, secret[65530:65550]) {
		t.Errorf("range read across a segment boundary = %q; want %q", part, secret[65530:65550])
	}
	// A request's key wins over the bucket's default.
	a, err = cmekPut(t, b.Object("explicit"), other, []byte("other key"))
	if err != nil || a.KMSKeyName != other+"/cryptoKeyVersions/1" {
		t.Fatalf("explicit-key write = %+v, %v; want kmsKeyName %s/cryptoKeyVersions/1", a, err, other)
	}
	// Rotation: new writes use the new primary, old objects keep version 1.
	v2, err := f.kms.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: def, CryptoKeyVersion: &kmspb.CryptoKeyVersion{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.kms.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: def, CryptoKeyVersionId: "2"}); err != nil {
		t.Fatal(err)
	}
	a, err = cmekPut(t, b.Object("after-rotation"), "", []byte("new primary"))
	if err != nil || a.KMSKeyName != v2.GetName() {
		t.Fatalf("write after rotation = %+v, %v; want kmsKeyName %s", a, err, v2.GetName())
	}
	old, err := b.Object("by-default").Attrs(ctx)
	if err != nil || old.KMSKeyName != def+"/cryptoKeyVersions/1" {
		t.Fatalf("old object after rotation = %+v, %v; want it still on version 1", old, err)
	}
	// Disabling version 1 blocks reading, copying and rewriting its
	// objects, not their metadata, nor the objects of version 2.
	f.setState(t, def+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_DISABLED)
	if _, err := get(b.Object("by-default")); code(err) != 400 {
		t.Errorf("read with version 1 disabled = %v; want 400", err)
	}
	if _, err := b.Object("copy").CopierFrom(b.Object("by-default")).Run(ctx); code(err) != 400 {
		t.Errorf("copy with version 1 disabled = %v; want 400", err)
	}
	if _, err := b.Object("by-default").Attrs(ctx); err != nil {
		t.Errorf("metadata with version 1 disabled = %v; want it served", err)
	}
	if got, err := get(b.Object("after-rotation")); err != nil || string(got) != "new primary" {
		t.Errorf("version 2 object = %q, %v; want it readable", got, err)
	}
	f.setState(t, def+"/cryptoKeyVersions/1", kmspb.CryptoKeyVersion_ENABLED)
	if got, err := get(b.Object("by-default")); err != nil || !bytes.Equal(got, secret) {
		t.Errorf("read after re-enabling = %d bytes, %v; want the object", len(got), err)
	}
	// A key whose primary is disabled cannot encrypt.
	f.setState(t, v2.GetName(), kmspb.CryptoKeyVersion_DISABLED)
	if _, err := cmekPut(t, b.Object("refused"), "", []byte("x")); code(err) != 400 {
		t.Errorf("write with the default key's primary disabled = %v; want 400", err)
	}
	f.setState(t, v2.GetName(), kmspb.CryptoKeyVersion_ENABLED)
	// A missing key is refused.
	if _, err := cmekPut(t, b.Object("missing"), "projects/proj-cmek/locations/us/keyRings/ring/cryptoKeys/nope", []byte("x")); code(err) != 400 {
		t.Errorf("write with a missing key = %v; want 400", err)
	}
	// Copy and rewrite re-encrypt with destinationKmsKeyName.
	c := b.Object("copied").CopierFrom(b.Object("by-default"))
	c.DestinationKMSKeyName = other
	ca, err := c.Run(ctx)
	if err != nil || ca.KMSKeyName != other+"/cryptoKeyVersions/1" {
		t.Fatalf("rewrite to another key = %+v, %v; want kmsKeyName %s/cryptoKeyVersions/1", ca, err, other)
	}
	if got, err := get(b.Object("copied")); err != nil || !bytes.Equal(got, secret) {
		t.Errorf("rewritten object = %d bytes, %v; want the source's bytes", len(got), err)
	}
	// The Go client sends no key with a compose: the bucket's default
	// key's primary encrypts the result, whatever its sources' keys.
	cp, err := b.Object("composed").ComposerFrom(b.Object("explicit"), b.Object("after-rotation")).Run(ctx)
	if err != nil || cp.KMSKeyName != v2.GetName() {
		t.Fatalf("compose = %+v, %v; want kmsKeyName %s", cp, err, v2.GetName())
	}
	if got, err := get(b.Object("composed")); err != nil || string(got) != "other keynew primary" {
		t.Errorf("composed object = %q, %v", got, err)
	}
	// Listing leaves out a CMEK object's checksums.
	it := b.Objects(ctx, &gcs.Query{Prefix: "explicit"})
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if o.CRC32C != 0 || len(o.MD5) != 0 || o.KMSKeyName == "" {
			t.Errorf("listed CMEK object = crc32c %d md5 %x kms %q; want no checksums and its key", o.CRC32C, o.MD5, o.KMSKeyName)
		}
	}
	// Metadata updates cannot change the key.
	if _, err := b.Object("explicit").Update(ctx, gcs.ObjectAttrsToUpdate{Metadata: map[string]string{"a": "b"}}); err != nil {
		t.Errorf("metadata update of a CMEK object = %v", err)
	}
}

// A key ring outside the bucket's location is refused, as a default key
// and on a write.
func TestCMEKKeyLocation(t *testing.T) {
	f := newCMEKFixture(t)
	ctx := context.Background()
	key := f.key(t, "ring", "k")
	err := f.client.Bucket("eu-bucket").Create(ctx, "p", &gcs.BucketAttrs{Location: "EU", Encryption: &gcs.BucketEncryption{DefaultKMSKeyName: key}})
	if code(err) != 400 {
		t.Errorf("EU bucket with a us key = %v; want 400", err)
	}
	b := f.client.Bucket("eu-plain")
	if err := b.Create(ctx, "p", &gcs.BucketAttrs{Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekPut(t, b.Object("o"), key, []byte("x")); code(err) != 400 {
		t.Errorf("write to an EU bucket with a us key = %v; want 400", err)
	}
}

// The bucket's encryption enforcement config refuses new objects of a
// restricted type, keeps at least one type allowed, and stamps
// effectiveTime (#1938).
func TestCMEKEnforcementConfig(t *testing.T) {
	f := newCMEKFixture(t)
	ctx := context.Background()
	key := f.key(t, "ring", "k")
	b := f.client.Bucket("enforced")
	if err := b.Create(ctx, "p", &gcs.BucketAttrs{Location: "US",
		GoogleManagedEncryptionEnforcementConfig: &gcs.EncryptionEnforcementConfig{RestrictionMode: gcs.FullyRestricted}}); err != nil {
		t.Fatal(err)
	}
	attrs, err := b.Attrs(ctx)
	if err != nil || attrs.GoogleManagedEncryptionEnforcementConfig == nil ||
		attrs.GoogleManagedEncryptionEnforcementConfig.RestrictionMode != gcs.FullyRestricted ||
		attrs.GoogleManagedEncryptionEnforcementConfig.EffectiveTime.IsZero() {
		t.Fatalf("enforcement config = %+v, %v; want FullyRestricted with an effectiveTime", attrs.GoogleManagedEncryptionEnforcementConfig, err)
	}
	if _, err := cmekPut(t, b.Object("gmek"), "", []byte("x")); code(err) != 400 {
		t.Errorf("Google-managed write to a GMEK-restricted bucket = %v; want 400", err)
	}
	if _, err := cmekPut(t, b.Object("cmek"), key, []byte("x")); err != nil {
		t.Errorf("CMEK write to a GMEK-restricted bucket = %v; want it allowed", err)
	}
	// Restricting CMEK too keeps the GMEK config (omitted: retained).
	if _, err := b.Update(ctx, gcs.BucketAttrsToUpdate{
		CustomerManagedEncryptionEnforcementConfig: &gcs.EncryptionEnforcementConfig{RestrictionMode: gcs.FullyRestricted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekPut(t, b.Object("cmek2"), key, []byte("x")); code(err) != 400 {
		t.Errorf("CMEK write after restricting CMEK = %v; want 400", err)
	}
	if _, err := cmekPut(t, b.Object("gmek2"), "", []byte("x")); code(err) != 400 {
		t.Errorf("GMEK write after the CMEK update = %v; want 400 (GMEK restriction retained)", err)
	}
	csek := bytes.Repeat([]byte{7}, 32)
	if _, err := cmekPut(t, b.Object("csek").Key(csek), "", []byte("x")); err != nil {
		t.Errorf("CSEK write = %v; want allowed", err)
	}
	// All three restricted is refused.
	if _, err := b.Update(ctx, gcs.BucketAttrsToUpdate{
		CustomerSuppliedEncryptionEnforcementConfig: &gcs.EncryptionEnforcementConfig{RestrictionMode: gcs.FullyRestricted}}); code(err) != 400 {
		t.Errorf("restricting every encryption type = %v; want 400", err)
	}
}

// Without a Cloud KMS a key is refused rather than ignored.
func TestCMEKWithoutKMS(t *testing.T) {
	h := newTestServer(t)
	c, err := gcs.NewClient(context.Background(), option.WithEndpoint(h.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := c.Bucket("plain")
	if err := b.Create(context.Background(), "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekPut(t, b.Object("o"), "projects/proj-cmek/locations/us/keyRings/r/cryptoKeys/k", []byte("x")); code(err) != 400 {
		t.Errorf("write with a key and no Cloud KMS = %v; want 400", err)
	}
}

// The sealed format round-trips every size around the segment boundary
// and refuses a truncated blob.
func TestSealedBlobRoundTrip(t *testing.T) {
	dek := bytes.Repeat([]byte{1}, 32)
	for _, n := range []int{0, 1, sealSegment - 1, sealSegment, sealSegment + 1, 3 * sealSegment} {
		plain := bytes.Repeat([]byte{'a'}, n)
		sr, err := newSealReader(bytes.NewReader(plain), dek)
		if err != nil {
			t.Fatal(err)
		}
		ct, _ := io.ReadAll(sr)
		r, err := newSealedReader(nopSeekCloser{bytes.NewReader(ct)}, dek, int64(n))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("size %d: read %d bytes, %v", n, len(got), err)
		}
		if n > sealSegment {
			cut := ct[:sealHeader+sealSegment+sealTag]
			r, _ := newSealedReader(nopSeekCloser{bytes.NewReader(cut)}, dek, int64(sealSegment))
			if _, err := io.ReadAll(r); err == nil {
				t.Errorf("size %d truncated to one segment opened; want refused", n)
			}
		}
	}
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// With IAM enforcement on, the bucket project's Cloud Storage service agent
// must hold roles/cloudkms.cryptoKeyEncrypterDecrypter on the key: the
// storage server asks the host's evaluator with the key's own policies.
func TestCMEKServiceAgentPermission(t *testing.T) {
	var agents []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in iamcheck.Request
		_ = json.NewDecoder(r.Body).Decode(&in)
		d := iamcheck.Decision{Held: []string{}}
		if in.Signer == "" {
			d.Owner, d.Held = true, in.Permissions
		} else {
			agents = append(agents, in.Signer)
			for _, p := range in.Own {
				for _, b := range p.Policy.Bindings {
					for _, m := range b.Members {
						if b.Role == "roles/cloudkms.cryptoKeyEncrypterDecrypter" && m == "serviceAccount:"+in.Signer {
							d.Held = in.Permissions
						}
					}
				}
			}
		}
		_ = json.NewEncoder(w).Encode(d)
	}))
	defer host.Close()
	f := newCMEKFixture(t, &iamcheck.Client{Base: host.URL})
	ctx := context.Background()
	key := f.key(t, "ring", "k")
	b := f.client.Bucket("agent-bucket")
	if err := b.Create(ctx, "proj-cmek", &gcs.BucketAttrs{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	_, err := cmekPut(t, b.Object("o"), key, []byte("x"))
	if code(err) != 403 || !strings.Contains(err.Error(), "service account") {
		t.Fatalf("write without the agent's grant = %v; want 403 naming the service account", err)
	}
	agent := serviceAgent("proj-cmek")
	if len(agents) == 0 || agents[0] != agent {
		t.Errorf("checked accounts = %v; want %s", agents, agent)
	}
	conn, err := grpc.NewClient(strings.TrimPrefix(f.kmsAddr, "http://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := iampb.NewIAMPolicyClient(conn).SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: key, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyEncrypterDecrypter", Members: []string{"serviceAccount:" + agent}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cmekPut(t, b.Object("o"), key, []byte("x")); err != nil {
		t.Fatalf("write with the agent's grant = %v", err)
	}
	if got, err := get(b.Object("o")); err != nil || string(got) != "x" {
		t.Errorf("read with the agent's grant = %q, %v", got, err)
	}
	if _, err := iampb.NewIAMPolicyClient(conn).SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: key, Policy: &iampb.Policy{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := get(b.Object("o")); code(err) != 403 {
		t.Errorf("read after the grant is removed = %v; want 403", err)
	}
}
