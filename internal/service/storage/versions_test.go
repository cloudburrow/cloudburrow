package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

func versionedBucket(t *testing.T, name string) (*gcs.BucketHandle, string) {
	t.Helper()
	_, bh, h := sdkBucket(t, name)
	if _, err := bh.Update(context.Background(), gcs.BucketAttrsToUpdate{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	return bh, h.URL
}

func listVersionsOf(t *testing.T, bh *gcs.BucketHandle, versions bool) []*gcs.ObjectAttrs {
	t.Helper()
	var out []*gcs.ObjectAttrs
	it := bh.Objects(context.Background(), &gcs.Query{Versions: versions})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list (versions=%v): %v", versions, err)
		}
		out = append(out, a)
	}
}

// An overwrite on a versioned bucket keeps the old version as noncurrent,
// with its generation and a timeDeleted, readable by generation (#498).
func TestStorageVersioningKeepsNoncurrentInProcess(t *testing.T) {
	bh, _ := versionedBucket(t, "versioned")
	o := bh.Object("v.txt")
	first := write(t, o, []byte("one"), nil)
	second := write(t, o, []byte("two"), nil)
	if got := listVersionsOf(t, bh, true); len(got) != 2 || got[0].Generation != first.Generation ||
		got[1].Generation != second.Generation || got[0].Deleted.IsZero() || !got[1].Deleted.IsZero() {
		t.Fatalf("versions = %v", got)
	}
	if got := listVersionsOf(t, bh, false); len(got) != 1 || got[0].Generation != second.Generation {
		t.Errorf("plain list = %v", got)
	}
	if b := read(t, o.Generation(first.Generation), 0, -1); string(b) != "one" {
		t.Errorf("noncurrent read = %q", b)
	}
	if b := read(t, o, 0, -1); string(b) != "two" {
		t.Errorf("live read = %q", b)
	}
	a, err := o.Generation(first.Generation).Attrs(context.Background())
	if err != nil || a.Deleted.IsZero() {
		t.Errorf("noncurrent attrs = %+v, %v", a, err)
	}
}

// A delete without a generation keeps the live version as noncurrent; the
// object then does not exist, and ifGenerationMatch=0 may create it.
func TestStorageVersioningDeleteKeepsNoncurrentInProcess(t *testing.T) {
	bh, _ := versionedBucket(t, "versioned-delete")
	ctx := context.Background()
	o := bh.Object("d.txt")
	first := write(t, o, []byte("one"), nil)
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("live after delete = %v", err)
	}
	if got := listVersionsOf(t, bh, true); len(got) != 1 || got[0].Generation != first.Generation || got[0].Deleted.IsZero() {
		t.Fatalf("versions after delete = %v", got)
	}
	if got := listVersionsOf(t, bh, false); len(got) != 0 {
		t.Errorf("plain list after delete = %v", got)
	}
	// Only a noncurrent version exists, so the object does not (the 0
	// precondition, request-preconditions docs).
	w := o.If(gcs.Conditions{DoesNotExist: true}).NewWriter(ctx)
	_, _ = w.Write([]byte("again"))
	if err := w.Close(); err != nil {
		t.Errorf("ifGenerationMatch=0 with only a noncurrent version: %v", err)
	}
}

// A delete that names a generation removes that version permanently, live
// or noncurrent.
func TestStorageVersioningDeleteByGenerationIsPermanent(t *testing.T) {
	bh, _ := versionedBucket(t, "versioned-gen")
	ctx := context.Background()
	o := bh.Object("g.txt")
	first := write(t, o, []byte("one"), nil)
	second := write(t, o, []byte("two"), nil)
	if err := o.Generation(first.Generation).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.Generation(second.Generation).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if got := listVersionsOf(t, bh, true); len(got) != 0 {
		t.Errorf("versions after deleting both by generation = %v", got)
	}
	if err := o.Generation(first.Generation).Delete(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("a second delete of the generation = %v", err)
	}
}

// Disabling versioning stops new noncurrent versions and keeps those that
// exist.
func TestStorageVersioningDisableKeepsExistingInProcess(t *testing.T) {
	bh, _ := versionedBucket(t, "versioned-off")
	ctx := context.Background()
	o := bh.Object("x.txt")
	first := write(t, o, []byte("one"), nil)
	write(t, o, []byte("two"), nil)
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{VersioningEnabled: false}); err != nil {
		t.Fatal(err)
	}
	third := write(t, o, []byte("three"), nil)
	got := listVersionsOf(t, bh, true)
	if len(got) != 2 || got[0].Generation != first.Generation || got[1].Generation != third.Generation {
		t.Errorf("versions after disabling = %v; want the kept noncurrent one and the live one", got)
	}
}

// Metadata of a noncurrent version can be patched by generation, and it
// can be the source of a copy.
func TestStorageVersioningNoncurrentPatchAndCopy(t *testing.T) {
	bh, _ := versionedBucket(t, "versioned-patch")
	ctx := context.Background()
	o := bh.Object("p.txt")
	first := write(t, o, []byte("one"), nil)
	write(t, o, []byte("two"), nil)
	a, err := o.Generation(first.Generation).Update(ctx, gcs.ObjectAttrsToUpdate{Metadata: map[string]string{"k": "v"}})
	if err != nil || a.Metadata["k"] != "v" || a.Metageneration != 2 || a.Deleted.IsZero() {
		t.Fatalf("patch of a noncurrent version = %+v, %v", a, err)
	}
	if _, err := bh.Object("restored.txt").CopierFrom(o.Generation(first.Generation)).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b := read(t, bh.Object("restored.txt"), 0, -1); string(b) != "one" {
		t.Errorf("copy of the noncurrent version = %q", b)
	}
	r, err := bh.Object("restored.txt").Attrs(ctx)
	if err != nil || !r.Deleted.IsZero() {
		t.Errorf("the copy is live: %+v, %v", r, err)
	}
}

// A listing with versions pages within a name's versions, and a bucket with
// only noncurrent versions is not empty.
func TestStorageVersioningPagesAndNonEmptyDelete(t *testing.T) {
	bh, base := versionedBucket(t, "versioned-page")
	o := bh.Object("n.txt")
	var gens []int64
	for i := 0; i < 3; i++ {
		gens = append(gens, write(t, o, []byte(fmt.Sprint(i)), nil).Generation)
	}
	write(t, bh.Object("m.txt"), []byte("m"), nil)
	var got []string
	tok := ""
	for page := 0; page < 10; page++ {
		code, body := raw(t, "GET", base+"/storage/v1/b/versioned-page/o?versions=true&maxResults=1&pageToken="+tok, "")
		if code != http.StatusOK {
			t.Fatalf("page %d = %d %s", page, code, body)
		}
		var resp struct {
			Items []struct {
				Name       string `json:"name"`
				Generation string `json:"generation"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		for _, it := range resp.Items {
			got = append(got, it.Name+"#"+it.Generation)
		}
		if tok = resp.NextPageToken; tok == "" {
			break
		}
	}
	want := []string{"m.txt#" + fmt.Sprint(listVersionsOf(t, bh, false)[0].Generation)}
	for _, g := range gens {
		want = append(want, fmt.Sprintf("n.txt#%d", g))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paged versions = %v, want %v", got, want)
	}

	ctx := context.Background()
	for _, a := range listVersionsOf(t, bh, false) {
		if err := bh.Object(a.Name).Delete(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var ge *googleapi.Error
	if err := bh.Delete(ctx); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Errorf("deleting a bucket with only noncurrent versions = %v; want 409", err)
	}
}
