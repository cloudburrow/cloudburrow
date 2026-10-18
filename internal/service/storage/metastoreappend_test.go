package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Append adds to a JSON array: a transaction reads its own appends, the log
// replays them over the snapshot, and an empty value Put is kept, not taken
// for a deletion.
func TestMetadataStoreAppend(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLogMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error {
		tx.Append("list", []byte(`1`))
		tx.Append("list", []byte(`{"a":2}`))
		if v, _ := tx.Get("list"); string(v) != `[1,{"a":2}]` {
			t.Errorf("in the transaction, list = %s", v)
		}
		tx.Put("empty", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.compact(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error { tx.Append("list", []byte(`"three"`)); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error { tx.Put("other", []byte("[]")); tx.Append("other", []byte(`4`)); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenLogMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.View(func(tx Tx) error {
		for k, want := range map[string]string{"list": `[1,{"a":2},"three"]`, "other": `[4]`} {
			if v, _ := tx.Get(k); string(v) != want {
				t.Errorf("after reopen %s = %s, want %s", k, v, want)
			}
		}
		if _, ok := tx.Get("empty"); !ok {
			t.Error("an empty value was dropped")
		}
		return nil
	})
	if s.liveSize != func() (n int64) {
		for k, v := range s.data {
			n += int64(len(k) + len(v))
		}
		return n
	}() {
		t.Errorf("liveSize %d is not the live data's size", s.liveSize)
	}
}

// #1609: in the soak, objects overwritten all day under the default 7-day
// soft delete policy each kept thousands of soft-deleted versions in one
// record, which every overwrite read, wrote whole and logged again, so an
// upload's cost and the storage pod's memory grew with the versions kept.
// Each overwrite's log record now holds the version it adds, whatever the
// number kept before it, and versioned buckets' noncurrent versions too.
func TestOverwriteLogsOnlyTheVersionItKeeps(t *testing.T) {
	dir := t.TempDir()
	meta, err := OpenLogMetaStore(filepath.Join(dir, "meta"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	srv, err := NewServer(Options{Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	defer h.Close()
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	c, err := gcs.NewClient(context.Background(), option.WithEndpoint(h.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	for _, bucket := range []string{"soft-kept", "versioned"} {
		bh := c.Bucket(bucket)
		if err := bh.Create(ctx, "demo-project", &gcs.BucketAttrs{VersioningEnabled: bucket == "versioned"}); err != nil {
			t.Fatal(err)
		}
		const n = 300
		var first, last int
		for i := 0; i < n; i++ {
			write(t, bh.Object("ring/0"), []byte(fmt.Sprint("payload ", i)), nil)
			size := lastLogRecord(t, filepath.Join(dir, "meta", "log.jsonl"))
			if i == 2 {
				first = size
			}
			last = size
		}
		// The record of the 300th overwrite is about the size of the 3rd's:
		// before #1609 it held all 299 versions kept, some 300 KB.
		if last > 2*first+512 {
			t.Errorf("%s: the last overwrite logged %d bytes, the third %d: it rewrote the versions kept", bucket, last, first)
		}
		q := &gcs.Query{SoftDeleted: bucket == "soft-kept", Versions: bucket == "versioned"}
		it := bh.Objects(ctx, q)
		var gens []int64
		for {
			a, err := it.Next()
			if err != nil {
				break
			}
			if a.Deleted.IsZero() && bucket == "versioned" {
				continue // the live version
			}
			gens = append(gens, a.Generation)
		}
		if len(gens) != n-1 {
			t.Fatalf("%s: %d versions kept, want %d", bucket, len(gens), n-1)
		}
		for i := 1; i < len(gens); i++ {
			if gens[i] <= gens[i-1] {
				t.Fatalf("%s: versions out of order at %d: %v", bucket, i, gens[i-1:i+1])
			}
		}
	}
}

// lastLogRecord is the size of the log's last record, 0 if the log was just
// compacted and is empty.
func lastLogRecord(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	last := lines[len(lines)-1]
	if len(last) > 0 && !json.Valid(last) {
		t.Fatalf("the log's last record is not JSON: %.100s", last)
	}
	return len(last)
}
