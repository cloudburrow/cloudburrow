//go:build upstream

package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// fakeGCSImage is pinned by tag here; #31 replaces tags with digests in the
// real lock set. A mutable tag is acceptable for a probe, never for a release.
const fakeGCSImage = "fsouza/fake-gcs-server:1.56.1"

// gcsBackend is a running Cloud Storage candidate.
type gcsBackend struct {
	endpoint  string // e.g. http://127.0.0.1:4443
	container string
}

// startFakeGCS runs fake-gcs-server in Docker on an OS-assigned port.
func startFakeGCS(t *testing.T) *gcsBackend {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	port := freePort(t)
	name := fmt.Sprintf("cb-probe-gcs-%d", port)

	// -scheme http keeps the probe off TLS; -public-host makes the server
	// rewrite download URLs to an address the client can actually reach.
	host := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("docker", "run", "--rm", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:4443", port),
		fakeGCSImage,
		"-scheme", "http", "-port", "4443",
		"-public-host", host,
		"-backend", "memory",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cannot start %s: %v (%s)", fakeGCSImage, err, out)
	}
	b := &gcsBackend{endpoint: "http://" + host, container: name}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(b.endpoint + "/storage/v1/b")
		if err == nil {
			resp.Body.Close()
			return b
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("fake-gcs-server never became ready at %s", b.endpoint)
	return nil
}

// client returns the official storage client pointed at the candidate.
func (b *gcsBackend) client(t *testing.T) *storage.Client {
	t.Helper()
	// STORAGE_EMULATOR_HOST is what the official Go client honours. Setting it
	// also disables authentication, so nothing reaches Google.
	t.Setenv("STORAGE_EMULATOR_HOST", b.endpoint)
	c, err := storage.NewClient(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(b.endpoint+"/storage/v1/"))
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (b *gcsBackend) bucket(t *testing.T, c *storage.Client, name string) *storage.BucketHandle {
	t.Helper()
	bh := c.Bucket(name)
	if err := bh.Create(context.Background(), "probe-project", nil); err != nil {
		t.Fatalf("Create bucket %s: %v", name, err)
	}
	return bh
}

func writeObject(t *testing.T, bh *storage.BucketHandle, name string, data []byte) *storage.ObjectAttrs {
	t.Helper()
	ctx := context.Background()
	w := bh.Object(name).NewWriter(ctx)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return w.Attrs()
}

// TestGCSBucketAndObjectBasics covers the control plane and simple data plane.
func TestGCSBucketAndObjectBasics(t *testing.T) {
	b := startFakeGCS(t)
	c := b.client(t)
	ctx := context.Background()

	bh := b.bucket(t, c, "probe-basics")

	if _, err := bh.Attrs(ctx); err != nil {
		t.Fatalf("Bucket.Attrs: %v", err)
	}

	writeObject(t, bh, "a.txt", []byte("alpha"))
	writeObject(t, bh, "dir/b.txt", []byte("bravo"))
	writeObject(t, bh, "dir/c.txt", []byte("charlie"))

	r, err := bh.Object("a.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "alpha" {
		t.Errorf("download = %q, want alpha", got)
	}

	// Prefix + delimiter listing is the part emulators most often get wrong.
	it := bh.Objects(ctx, &storage.Query{Prefix: "dir/", Delimiter: "/"})
	var names []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Objects.Next: %v", err)
		}
		names = append(names, attrs.Name)
	}
	if len(names) != 2 {
		t.Errorf("prefix listing returned %v, want 2 objects", names)
	}
	t.Logf("bucket create/get, upload, download, prefix listing: OK (%v)", names)

	if err := bh.Object("a.txt").Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := bh.Object("a.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("Attrs after delete = %v, want ErrObjectNotExist", err)
	}
}

// TestGCSGenerationPreconditions is the correctness case that matters for
// concurrent writers. CloudBurrow cannot adopt a backend that ignores it.
func TestGCSGenerationPreconditions(t *testing.T) {
	b := startFakeGCS(t)
	c := b.client(t)
	ctx := context.Background()
	bh := b.bucket(t, c, "probe-preconditions")

	attrs := writeObject(t, bh, "obj", []byte("v1"))
	if attrs == nil || attrs.Generation == 0 {
		t.Fatalf("no generation returned on write: %+v", attrs)
	}
	gen := attrs.Generation
	t.Logf("first write generation = %d", gen)

	// DoesNotExist must fail now that the object exists.
	w := bh.Object("obj").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	_, _ = w.Write([]byte("v2"))
	err := w.Close()
	if err == nil {
		t.Error("write with DoesNotExist succeeded on an existing object; precondition ignored")
	} else {
		var ae *googleapi.Error
		code := 0
		if errors.As(err, &ae) {
			code = ae.Code
		}
		t.Logf("DoesNotExist on existing object -> HTTP %d (%v)", code, err)
	}

	// A stale generation must be rejected.
	w = bh.Object("obj").If(storage.Conditions{GenerationMatch: gen + 999}).NewWriter(ctx)
	_, _ = w.Write([]byte("v3"))
	if err := w.Close(); err == nil {
		t.Error("write with a stale GenerationMatch succeeded; precondition ignored")
	} else {
		t.Logf("stale GenerationMatch -> rejected as expected (%v)", err)
	}

	// The correct generation must be accepted and must bump the generation.
	w = bh.Object("obj").If(storage.Conditions{GenerationMatch: gen}).NewWriter(ctx)
	_, _ = w.Write([]byte("v4"))
	if err := w.Close(); err != nil {
		t.Fatalf("write with matching generation failed: %v", err)
	}
	after, err := bh.Object("obj").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation == gen {
		t.Errorf("generation did not change after overwrite (still %d)", gen)
	}
	t.Logf("generation after matched-precondition overwrite = %d", after.Generation)
}

// TestGCSResumableUploadAndRangedRead exercises the large-object path the SDK
// uses by default, plus range requests.
func TestGCSResumableUploadAndRangedRead(t *testing.T) {
	b := startFakeGCS(t)
	c := b.client(t)
	ctx := context.Background()
	bh := b.bucket(t, c, "probe-resumable")

	// Larger than the client's resumable threshold, forcing chunked upload.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1024*1024) // 16 MiB
	obj := bh.Object("large.bin")
	w := obj.NewWriter(ctx)
	w.ChunkSize = 256 * 1024
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close (resumable upload): %v", err)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", attrs.Size, len(payload))
	}
	t.Logf("resumable upload of %d bytes: OK (crc32c=%d md5len=%d)", attrs.Size, attrs.CRC32C, len(attrs.MD5))

	// Ranged read.
	r, err := obj.NewRangeReader(ctx, 16, 32)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if len(got) != 32 {
		t.Errorf("ranged read returned %d bytes, want 32", len(got))
	}
	if !bytes.Equal(got, payload[16:48]) {
		t.Errorf("ranged read content mismatch")
	}
	t.Log("ranged read: OK")
}

// TestGCSComposeAndCopy checks two operations that are commonly missing.
func TestGCSComposeAndCopy(t *testing.T) {
	b := startFakeGCS(t)
	c := b.client(t)
	ctx := context.Background()
	bh := b.bucket(t, c, "probe-compose")

	writeObject(t, bh, "p1", []byte("hello "))
	writeObject(t, bh, "p2", []byte("world"))

	t.Run("compose", func(t *testing.T) {
		dst := bh.Object("joined")
		_, err := dst.ComposerFrom(bh.Object("p1"), bh.Object("p2")).Run(ctx)
		if err != nil {
			t.Errorf("Compose unsupported or failed: %v", err)
			return
		}
		r, err := dst.NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != "hello world" {
			t.Errorf("composed = %q, want %q", got, "hello world")
		}
		t.Log("compose: OK")
	})

	t.Run("copy", func(t *testing.T) {
		_, err := bh.Object("p1-copy").CopierFrom(bh.Object("p1")).Run(ctx)
		if err != nil {
			t.Errorf("Copy unsupported or failed: %v", err)
			return
		}
		t.Log("copy: OK")
	})
}

// TestGCSDurability records whether a filesystem-backed instance survives a
// restart, which decides whether CloudBurrow can rely on it for durable mode.
func TestGCSDurability(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	port := freePort(t)
	host := fmt.Sprintf("127.0.0.1:%d", port)
	name := fmt.Sprintf("cb-probe-gcs-durable-%d", port)
	vol := name + "-vol"

	_ = exec.Command("docker", "volume", "create", vol).Run()
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", vol).Run()
	})

	start := func() {
		cmd := exec.Command("docker", "run", "--rm", "-d", "--name", name,
			"-p", fmt.Sprintf("127.0.0.1:%d:4443", port),
			"-v", vol+":/storage",
			fakeGCSImage,
			"-scheme", "http", "-port", "4443", "-public-host", host,
			"-backend", "filesystem", "-filesystem-root", "/storage")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("cannot start durable backend: %v (%s)", err, out)
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get("http://" + host + "/storage/v1/b")
			if err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("durable backend never became ready")
	}

	start()
	b := &gcsBackend{endpoint: "http://" + host}
	c := b.client(t)
	bh := b.bucket(t, c, "durable-bucket")
	writeObject(t, bh, "keep.txt", []byte("persisted"))

	// Restart against the same volume.
	_ = exec.Command("docker", "rm", "-f", name).Run()
	start()

	c2 := b.client(t)
	r, err := c2.Bucket("durable-bucket").Object("keep.txt").NewReader(context.Background())
	if err != nil {
		t.Logf("RESULT: object did NOT survive restart: %v", err)
		return
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) == "persisted" {
		t.Log("RESULT: object SURVIVED restart with a filesystem backend and volume")
	} else {
		t.Logf("RESULT: object survived but content changed: %q", got)
	}
}

// TestGCSSignedURLNotVerified records whether the candidate validates
// signatures, which determines what CloudBurrow may claim in its matrix.
func TestGCSSignedURLBehavior(t *testing.T) {
	b := startFakeGCS(t)
	c := b.client(t)
	bh := b.bucket(t, c, "probe-signed")
	writeObject(t, bh, "secret.txt", []byte("data"))

	// A deliberately bogus signature: if this succeeds, signatures are not
	// verified, and no signing guarantee can be claimed.
	url := fmt.Sprintf("%s/probe-signed/secret.txt?X-Goog-Signature=deadbeef", b.endpoint)
	resp, err := http.Get(url)
	if err != nil {
		t.Skipf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("RESULT: GET with a bogus X-Goog-Signature -> HTTP %d (%d bytes, starts %q)",
		resp.StatusCode, len(body), strings.TrimSpace(string(body[:min(24, len(body))])))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = net.JoinHostPort
