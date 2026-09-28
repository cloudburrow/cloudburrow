package storageserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// earlierVolume lays out dir as the storage-data volume of a cluster
// brought up before #519 and upgraded (#780): the earlier server's buckets,
// each a directory beside its .bucketMetadata file, next to this server's
// meta and objects, which the earlier server also took for buckets.
func earlierVolume(t *testing.T, dir string, buckets ...string) {
	t.Helper()
	for _, b := range append(buckets, "meta", "objects") {
		if err := os.MkdirAll(filepath.Join(dir, b), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, b+".bucketMetadata"), []byte(`{"name":"`+b+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A persistent server over the earlier server's buckets refuses to start,
// naming the buckets, why it cannot use them and what to do (#780), rather
// than starting over them as if the volume were empty.
func TestTheServerRefusesTheEarlierServersBuckets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	earlierVolume(t, dir, "uploads", "avatars")
	err := Run(context.Background(), []string{"--data-dir", dir, "--mode", "persistent"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrEarlierLayout) {
		t.Fatalf("Run over an earlier volume = %v, want ErrEarlierLayout", err)
	}
	// Exactly the two buckets: not the server's own meta and objects.
	for _, want := range []string{dir, "2 bucket(s)", "(#519): avatars, uploads (", ".bucketMetadata", "#519",
		"cannot read those buckets", "`cloudburrow delete`", "docs/install.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "meta", "log.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused server wrote its log anyway: %v", err)
	}
}

// Only the earlier server's own buckets count: metadata files it wrote for
// this server's meta and objects hold nothing of the user's, and an
// ephemeral server starts empty whatever the directory holds.
func TestTheEarlierLayoutCheckIgnoresWhatIsNotABucket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	earlierVolume(t, dir)
	if err := CheckEarlierLayout(dir); err != nil {
		t.Errorf("only meta and objects metadata: %v", err)
	}
	if err := CheckEarlierLayout(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("a data directory not yet made: %v", err)
	}
	earlierVolume(t, dir, "uploads")
	if err := PrepareDir(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritable(dir); err != nil {
		t.Errorf("ephemeral over an earlier volume: %v", err)
	}
}

// Twelve buckets are named ten at a time, so a large volume's refusal stays
// readable.
func TestTheRefusalListsAtMostTenBuckets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var names []string
	for _, c := range "abcdefghijkl" {
		names = append(names, "b-"+string(c))
	}
	earlierVolume(t, dir, names...)
	err := CheckEarlierLayout(dir)
	if err == nil || !strings.Contains(err.Error(), "12 bucket(s)") || !strings.Contains(err.Error(), "b-j and 2 more") {
		t.Errorf("CheckEarlierLayout = %v", err)
	}
}

// A metadata log another user owns, the root-owned meta/log.jsonl the
// earlier server left on an upgraded cluster (#780), is reported by path,
// owner and mode before anything opens it, instead of "permission denied"
// from the first open and a restart loop.
func TestAnUnwritableDataDirNamesTheFileAndItsOwner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no Unix owners")
	}
	if os.Geteuid() == 0 {
		t.Skip("root may write any file; run as another user")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "meta", "log.jsonl")
	if err := os.WriteFile(log, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), []string{"--data-dir", dir, "--mode", "persistent"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrDataDirNotWritable) {
		t.Fatalf("Run over a read-only log = %v, want ErrDataDirNotWritable", err)
	}
	for _, want := range []string{log, "is owned by uid ", "with mode -r--------", "running as uid ",
		"storage-internal", "`cloudburrow delete`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q:\n%v", want, err)
		}
	}

	// A directory of the blob store is checked too.
	if err := os.Chmod(log, 0o600); err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(dir, "objects", "blobs", "ab")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blobs, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blobs, 0o700) })
	if err := CheckWritable(dir); !errors.Is(err, ErrDataDirNotWritable) || !strings.Contains(err.Error(), blobs) {
		t.Errorf("CheckWritable with a read-only blob shard = %v", err)
	}

	// A writable directory, and one not yet made under a writable parent,
	// pass.
	if err := os.Chmod(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritable(dir); err != nil {
		t.Errorf("CheckWritable on a writable store = %v", err)
	}
	if err := CheckWritable(filepath.Join(t.TempDir(), "new", "data")); err != nil {
		t.Errorf("CheckWritable on a directory still to be made = %v", err)
	}
}
