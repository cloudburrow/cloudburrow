package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// A repository with immutable tags keeps every tag where it was pushed: a
// push of the same tag to another image, a retag, a tag's delete and the
// delete of a tagged version or image are refused, over the registry API
// (DENIED) and in the store; a new tag and a re-push of the same image
// are not.
func TestImmutableTags(t *testing.T) {
	st, host := newTestRegistry(t)
	on := true
	st.Immutable = func(r Repo) bool { return on && r.Repository == "r" }
	repo := Repo{"us-central1", "p", "r"}
	a, _ := random.Image(256, 1)
	b, _ := random.Image(256, 1)
	ref := host + "/p/r/app:v1"
	if err := crane.Push(a, ref); err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(a, ref); err != nil {
		t.Fatalf("re-push of the same image under its tag: %v", err)
	}
	if err := crane.Push(b, ref); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("push of another image under an immutable tag = %v; want DENIED", err)
	}
	if err := crane.Push(b, host+"/p/r/app:v2"); err != nil {
		t.Fatalf("push under a new tag: %v", err)
	}
	da, _ := a.Digest()
	db, _ := b.Digest()
	if err := crane.Tag(ref, "v3"); err != nil {
		t.Fatalf("a new tag: %v", err)
	}
	check := func(what string, err error) {
		t.Helper()
		var ti *TagImmutableError
		if !errors.Is(err, ErrTagImmutable) || !errors.As(err, &ti) {
			t.Errorf("%s = %v; want ErrTagImmutable", what, err)
		}
	}
	check("SetTag moving v1", st.SetTag(repo, "app", "v1", db.String()))
	check("DeleteTag v1", st.DeleteTag(repo, "app", "v1"))
	check("DeleteManifest of a tagged version", st.DeleteManifest(repo, "app", da.String()))
	check("DeleteImage of a tagged image", st.DeleteImage(repo, "app"))
	if err := crane.Delete(host + "/p/r/app@" + da.String()); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Errorf("registry delete of a tagged manifest = %v; want DENIED", err)
	}
	if err := st.SetTag(repo, "app", "v1", da.String()); err != nil {
		t.Errorf("SetTag to the digest it names: %v", err)
	}
	// Turned off, the same changes are made.
	on = false
	if err := st.SetTag(repo, "app", "v1", db.String()); err != nil {
		t.Fatalf("SetTag with immutable tags off: %v", err)
	}
	if err := st.DeleteImage(repo, "app"); err != nil {
		t.Fatalf("DeleteImage with immutable tags off: %v", err)
	}
}
