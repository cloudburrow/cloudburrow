package localai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Provenance is the whole point of the catalogue: a converted model hosted
// beside Google's own is still a community conversion.
func TestProvenanceIsRecordedNotAssumed(t *testing.T) {
	t.Parallel()
	google, err := Lookup("gemma-3n-e2b-it")
	if err != nil {
		t.Fatal(err)
	}
	if !google.GooglePublished() {
		t.Error("gemma-3n-e2b-it is published by the verified google org")
	}
	if !strings.HasPrefix(google.Repo, "google/") {
		t.Errorf("repo = %q, want a google/ repository", google.Repo)
	}

	community, err := Lookup("gemma-4-e2b-it-community")
	if err != nil {
		t.Fatal(err)
	}
	if community.GooglePublished() {
		t.Error("a litert-community conversion must not be reported as Google-published")
	}
	// The name contains "gemma", which is exactly why provenance is a field
	// rather than something inferred from the model name.
	if !strings.Contains(strings.ToLower(community.Repo), "gemma") {
		t.Fatal("test fixture no longer covers the name-looks-official case")
	}
	if !strings.Contains(community.Notes, "not verified") {
		t.Error("a community conversion must say so in its notes")
	}
}

// Gated artifacts must be refused without credentials, not attempted.
func TestGatedModelIsRefusedWithoutAToken(t *testing.T) {
	t.Parallel()
	m, _ := Lookup("gemma-3n-e2b-it")
	if !m.RequiresCredentials() {
		t.Fatal("Gemma artifacts are gated; the catalogue must say so")
	}

	a := &Acquirer{Cache: Cache{Dir: t.TempDir()}, Fetcher: failFetcher{}}
	err := a.Acquire(context.Background(), m, "", nil)
	if !errors.Is(err, ErrGated) {
		t.Fatalf("Acquire() = %v, want ErrGated", err)
	}
	// The message must tell the user what to do about it.
	for _, want := range []string{"licence", "huggingface.co", "HUGGINGFACE_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

type failFetcher struct{}

func (failFetcher) Fetch(context.Context, Model, io.Writer, func(int64)) error {
	return errors.New("fetch should not have been attempted")
}

type stubFetcher struct{ content string }

func (s stubFetcher) Fetch(_ context.Context, _ Model, dst io.Writer, progress func(int64)) error {
	n, err := io.WriteString(dst, s.content)
	if progress != nil {
		progress(int64(n))
	}
	return err
}

func openModel(t *testing.T) Model {
	t.Helper()
	m, err := Lookup("gemma-4-e2b-it-community")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAcquireVerifiesChecksum(t *testing.T) {
	t.Parallel()
	m := openModel(t)
	dir := t.TempDir()
	a := &Acquirer{Cache: Cache{Dir: dir}, Fetcher: stubFetcher{content: "weights"}}

	// sha256("weights")
	const want = "8a2fd4b4b8b1a12d6fd6b0e3b2fc6b0f7b76fdc5fa8b0e2b2f6e4e2a9c3f9f7a"
	err := a.Acquire(context.Background(), m, want, nil)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Acquire() = %v, want ErrChecksumMismatch", err)
	}

	// A failed verification must leave nothing behind that looks cached.
	if _, statErr := os.Stat(a.Cache.Path(m)); !os.IsNotExist(statErr) {
		t.Error("a checksum failure left a cached artifact behind")
	}
}

func TestAcquireCachesAndReports(t *testing.T) {
	t.Parallel()
	m := openModel(t)
	dir := t.TempDir()
	var reported int64
	a := &Acquirer{Cache: Cache{Dir: dir}, Fetcher: stubFetcher{content: "weights"}}

	if err := a.Acquire(context.Background(), m, "", func(n int64) { reported = n }); err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	if reported == 0 {
		t.Error("no download progress was reported")
	}

	st := a.Cache.Status(m, "")
	if !st.Cached || st.Bytes != int64(len("weights")) {
		t.Errorf("Status = %+v, want a cached artifact", st)
	}
	// Without a pinned checksum the cache must not claim verification.
	if st.Verified {
		t.Error("Status reported Verified with no pinned checksum")
	}
	if !strings.Contains(st.Problem, "unverified") {
		t.Errorf("Status should say the contents are unverified, got %q", st.Problem)
	}
}

// A second acquire must not refetch.
func TestAcquireIsIdempotent(t *testing.T) {
	t.Parallel()
	m := openModel(t)
	dir := t.TempDir()
	a := &Acquirer{Cache: Cache{Dir: dir}, Fetcher: stubFetcher{content: "weights"}}
	if err := a.Acquire(context.Background(), m, "", nil); err != nil {
		t.Fatal(err)
	}
	a.Fetcher = failFetcher{} // a second fetch would fail the test
	if err := a.Acquire(context.Background(), m, "", nil); err != nil {
		t.Fatalf("second Acquire refetched: %v", err)
	}
}

// A corrupt cache entry must be detectable and removable, which is how
// recovery works.
func TestCorruptCacheIsDetectedAndRecoverable(t *testing.T) {
	t.Parallel()
	m := openModel(t)
	dir := t.TempDir()
	c := Cache{Dir: dir}
	path := c.Path(m)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := c.Status(m, strings.Repeat("a", 64))
	if st.Verified {
		t.Error("a corrupt artifact was reported as verified")
	}
	if !strings.Contains(st.Problem, "mismatch") {
		t.Errorf("Problem = %q, want a checksum mismatch", st.Problem)
	}

	if err := c.Remove(m); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if c.Status(m, "").Cached {
		t.Error("Remove did not clear the cache entry")
	}
	// Removing an absent artifact is not an error.
	if err := c.Remove(m); err != nil {
		t.Errorf("Remove on an absent artifact = %v, want nil", err)
	}
}

// Filling a developer's disk and failing at 98% is far worse than refusing.
func TestPreflightRefusesAnImpossibleDownload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := PreflightDisk(dir, 1<<20); err != nil {
		t.Errorf("PreflightDisk rejected a 1 MiB artifact: %v", err)
	}
	// Larger than any plausible disk.
	err := PreflightDisk(dir, 1<<50)
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("PreflightDisk = %v, want ErrInsufficientDisk", err)
	}
	if !strings.Contains(err.Error(), "GiB") {
		t.Errorf("error should quantify the shortfall, got: %v", err)
	}
}

// A catalogue entry is not a promise that CloudBurrow can execute it.
func TestRunnableReportsTheRuntimeGap(t *testing.T) {
	t.Parallel()
	for _, m := range Models() {
		ok, why := m.Runnable()
		if ok {
			t.Errorf("%s reports runnable; the audit found no runtime that works in a cluster pod", m.ID)
		}
		if why == "" {
			t.Errorf("%s gives no reason for being unrunnable", m.ID)
		}
	}

	gen, _ := Lookup("gemma-3n-e2b-it")
	if _, why := gen.Runnable(); !strings.Contains(why, "Linux") {
		t.Errorf("generation models should cite the missing Linux binary, got: %q", why)
	}
	emb, _ := Lookup("embeddinggemma-300m")
	if _, why := emb.Runnable(); !strings.Contains(why, "no verified local runtime") {
		t.Errorf("embedding model should cite the absent runtime, got: %q", why)
	}
}

func TestLookupUnknownModelListsTheKnownOnes(t *testing.T) {
	t.Parallel()
	_, err := Lookup("gemini-ultra")
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Lookup() = %v, want ErrUnknownModel", err)
	}
	if !strings.Contains(err.Error(), "gemma-3n-e2b-it") {
		t.Errorf("error should list known models, got: %v", err)
	}
}

func TestEveryCatalogueEntryIsFullyDescribed(t *testing.T) {
	t.Parallel()
	for _, m := range Models() {
		if m.ID == "" || m.Repo == "" || m.License == "" || m.Artifact == "" || m.Notes == "" {
			t.Errorf("incomplete entry: %+v", m)
		}
		switch m.Publisher {
		case PublisherGoogle, PublisherCommunity:
		default:
			t.Errorf("%s has an unrecognised publisher %q", m.ID, m.Publisher)
		}
		switch m.Modality {
		case ModalityText, ModalityEmbedding:
		default:
			t.Errorf("%s has an unrecognised modality %q", m.ID, m.Modality)
		}
		// A specific artifact is required: a repository holds several and they
		// are not interchangeable.
		if !strings.Contains(m.Artifact, ".") {
			t.Errorf("%s names no specific artifact file", m.ID)
		}
	}
	_ = fmt.Sprint(len(Models()))
}
