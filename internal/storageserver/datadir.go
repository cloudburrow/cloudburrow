package storageserver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrEarlierLayout means the data directory holds buckets in the layout of
// the Cloud Storage server CloudBurrow ran before its builtin one (#519),
// which this server cannot read (#780).
var ErrEarlierLayout = errors.New("the storage data directory holds buckets this server cannot read")

// ErrDataDirNotWritable means a file or directory the server must write
// in its data directory belongs to someone else (#780).
var ErrDataDirNotWritable = errors.New("the storage data directory is not writable")

// earlierBucketSuffix marks a bucket of the server CloudBurrow ran before
// #519: each bucket is a top-level directory with its metadata in a file
// beside it, <bucket> and <bucket>.bucketMetadata.
const earlierBucketSuffix = ".bucketMetadata"

// storeDirs are the directories this server keeps its state in.
var storeDirs = []string{"meta", "objects"}

// CheckEarlierLayout refuses a data directory that holds the earlier
// server's buckets (#780). A cluster brought up before #519 kept them on
// the same volume this server now uses; this server keeps its state in
// meta/ and objects/ and would start over them as if they were not there,
// while they stay on the volume, unreadable. The refusal names them and
// says what to do.
//
// A metadata file the earlier server wrote for this server's own meta or
// objects directory, which it took for buckets when both ran on one volume,
// holds nothing of the user's and is not a reason to refuse.
func CheckEarlierLayout(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var buckets []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), earlierBucketSuffix)
		if !ok || name == "" || !e.Type().IsRegular() || isStoreDir(name) {
			continue
		}
		buckets = append(buckets, name)
	}
	if len(buckets) == 0 {
		return nil
	}
	sort.Strings(buckets)
	return fmt.Errorf("%w: %s holds %d bucket(s) written by the Cloud Storage server CloudBurrow ran before "+
		"its builtin server (#519): %s (each a directory beside a %s file). This server keeps its state in "+
		"%s and %s and cannot read those buckets, so it will not start over them. To start with empty "+
		"storage, run `cloudburrow delete` and then `cloudburrow up`, which deletes the volume and those "+
		"buckets with it; to keep the objects, copy them out first with the CloudBurrow release that wrote "+
		"them. See docs/install.md, Upgrading",
		ErrEarlierLayout, dir, len(buckets), listNames(buckets, 10), earlierBucketSuffix,
		filepath.Join(dir, "meta"), filepath.Join(dir, "objects"))
}

// existingAncestor is dir, or the nearest directory above it that exists:
// where a directory the server creates has to be made.
func existingAncestor(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func isStoreDir(name string) bool {
	for _, d := range storeDirs {
		if name == d {
			return true
		}
	}
	return false
}

// listNames joins names, showing at most max of them.
func listNames(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:max], ", "), len(names)-max)
}

// CheckWritable fails, naming the path and its owner, when something the
// server must write in dir is not writable by it (#780): each directory of
// its store, the metadata files it appends to or replaces, and dir itself
// when a store directory has still to be created there (or the
// nearest directory above it that exists). Without it the
// server stopped on the first open that failed, "open /data/meta/log.jsonl:
// permission denied", and a Deployment restarted it forever.
//
// Blob files are not checked: they are written once and never reopened
// for writing, and the directories they are renamed into and removed from
// are.
func CheckWritable(dir string) error {
	var paths []string
	for _, sub := range storeDirs {
		p := filepath.Join(dir, sub)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			paths = append(paths, existingAncestor(dir))
			continue
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || (sub == "meta" && d.Type().IsRegular()) {
				paths = append(paths, path)
			}
			return nil
		})
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			return fmt.Errorf("%w: %s %s, and this server, running as %s, cannot read it: %w",
				ErrDataDirNotWritable, pe.Path, describeOwner(pe.Path), currentUser(), err)
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDataDirNotWritable, err)
		}
	}
	for _, p := range paths {
		if err := writable(p); err != nil {
			return fmt.Errorf("%w: %s %s, so this server, running as %s, cannot write it. Another process or user "+
				"changed it; on a cluster first brought up before the builtin storage "+
				"server (#519) that is the storage-internal Deployment an earlier CloudBurrow ran, which "+
				"`cloudburrow up` now removes. Make %s writable by %s again, or run `cloudburrow delete` and "+
				"then `cloudburrow up` to start with empty storage. See docs/install.md, Upgrading",
				ErrDataDirNotWritable, p, describeOwner(p), currentUser(), dir, currentUser())
		}
	}
	return nil
}
