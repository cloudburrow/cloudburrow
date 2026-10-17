package registry

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func newTestRegistry(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: st, Lookup: func(loc, project, repo string) (Repo, bool) {
		if project != "p" || repo != "r" {
			return Repo{}, false
		}
		return Repo{"us-central1", project, repo}, true
	}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return st, strings.TrimPrefix(srv.URL, "http://")
}

func TestPushPullTagDelete(t *testing.T) {
	st, host := newTestRegistry(t)
	img, err := random.Image(1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/p/r/nested/app:v1"
	if err := crane.Push(img, ref); err != nil {
		t.Fatalf("push: %v", err)
	}
	want, _ := img.Digest()
	got, err := crane.Digest(ref)
	if err != nil || got != want.String() {
		t.Fatalf("digest = %q, %v; want %s", got, err, want)
	}
	pulled, err := crane.Pull(ref)
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := pulled.Layers()
	if len(layers) != 3 {
		t.Fatalf("pulled %d layers", len(layers))
	}
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
	}
	if err := crane.Tag(ref, "v2"); err != nil {
		t.Fatal(err)
	}
	tags, err := crane.ListTags(host + "/p/r/nested/app")
	if err != nil || strings.Join(tags, ",") != "v1,v2" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	cat, err := crane.Catalog(host)
	if err != nil || strings.Join(cat, ",") != "p/r/nested/app" {
		t.Fatalf("catalog = %v, %v", cat, err)
	}
	ms, err := st.Manifests(Repo{"us-central1", "p", "r"}, "nested/app")
	if err != nil || len(ms) != 1 || strings.Join(ms[0].Tags, ",") != "v1,v2" {
		t.Fatalf("manifests = %+v, %v", ms, err)
	}
	// An index over the image, pushed by digest.
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
	r, _ := name.ParseReference(host + "/p/r/nested/app:multi")
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if _, err := crane.Pull(host + "/p/r/nested/app:multi"); err != nil {
		t.Fatalf("pull index: %v", err)
	}
	if err := crane.Delete(host + "/p/r/nested/app@" + want.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := crane.Digest(ref); err == nil {
		t.Fatal("v1 resolves after its manifest was deleted")
	}
	if err := crane.Push(img, host+"/p/other/app:v1"); err == nil || !strings.Contains(err.Error(), "NAME_UNKNOWN") {
		t.Fatalf("push to a missing repository: %v", err)
	}
}

func TestLocationOf(t *testing.T) {
	for host, want := range map[string]string{
		"us-central1-docker.pkg.dev":             "us-central1",
		"europe-west1-docker.pkg.localhost:9000": "europe-west1",
		"127.0.0.1:9000":                         "",
		"docker.pkg.dev":                         "",
	} {
		if got := LocationOf(host); got != want {
			t.Errorf("LocationOf(%q) = %q, want %q", host, got, want)
		}
	}
	if EscapePackage("a/b") != "a%2Fb" || UnescapePackage("a%2Fb") != "a/b" {
		t.Fatal("package escaping")
	}
}

// A remote repository (#1546) pulls an image, and an index, through from
// its upstream and keeps them: they are its own once pulled, still served
// when the upstream deletes them, and a push to it is refused.
func TestRemotePullThrough(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up, cache := Repo{"us-central1", "p", "r"}, Repo{"us-central1", "p", "cache"}
	h := &Handler{Store: st, Lookup: func(loc, project, repo string) (Repo, bool) {
		switch repo {
		case "r":
			return up, true
		case "cache":
			return cache, true
		}
		return Repo{}, false
	}, Remote: func(r Repo) (Repo, bool) { return up, r == cache }}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(512, 2)
	if err := crane.Push(img, host+"/p/r/app:v1"); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	if d, err := crane.Digest(host + "/p/cache/app:v1"); err != nil || d != want.String() {
		t.Fatalf("the remote repository's v1 = %q, %v; want %s", d, err, want)
	}
	if _, err := crane.Pull(host + "/p/cache/app@" + want.String()); err != nil {
		t.Fatalf("pull through by digest: %v", err)
	}
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
	r, _ := name.ParseReference(host + "/p/r/app:multi")
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatal(err)
	}
	if _, err := crane.Digest(host + "/p/cache/app:multi"); err != nil {
		t.Fatalf("pull through of an index: %v", err)
	}
	if err := st.DeleteImage(up, "app"); err != nil {
		t.Fatal(err)
	}
	if d, err := crane.Digest(host + "/p/cache/app:v1"); err != nil || d != want.String() {
		t.Errorf("the cached v1 after the upstream deleted it = %q, %v", d, err)
	}
	if err := crane.Push(img, host+"/p/cache/app:v2"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a push to the remote repository = %v; want it refused as read-only", err)
	}
}
