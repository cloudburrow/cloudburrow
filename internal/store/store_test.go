package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// both runs a test against every implementation, so the two modes cannot drift.
func both(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		s := NewMemory()
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
	t.Run("durable", func(t *testing.T) {
		t.Parallel()
		s, err := OpenDurable(t.TempDir())
		if err != nil {
			t.Fatalf("OpenDurable: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
}

func TestGetPutDelete(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		if _, err := s.Get("absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(absent) = %v, want ErrNotFound", err)
		}
		if err := s.Put("a/b", []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Get("a/b")
		if err != nil || string(got) != "v" {
			t.Fatalf("Get = %q, %v", got, err)
		}
		if err := s.Delete("a/b"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get("a/b"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after delete = %v, want ErrNotFound", err)
		}
	})
}

// Returned values must be copies, or a caller could mutate stored state.
func TestGetReturnsACopy(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		if err := s.Put("k", []byte("original")); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get("k")
		got[0] = 'X'
		again, _ := s.Get("k")
		if string(again) != "original" {
			t.Errorf("stored value was mutated through a returned slice: %q", again)
		}
	})
}

// Keys reach the filesystem in durable mode, so traversal must be impossible.
func TestUnsafeKeysAreRefused(t *testing.T) {
	t.Parallel()
	bad := []string{"", "/abs", "../escape", "a/../../b", "a%2e%2e/b", "a%2fb", `a\b`, "a\x00b"}
	both(t, func(t *testing.T, s Store) {
		for _, k := range bad {
			if err := s.Put(k, []byte("x")); !errors.Is(err, ErrUnsafeKey) {
				t.Errorf("Put(%q) = %v, want ErrUnsafeKey", k, err)
			}
			if _, err := s.Get(k); !errors.Is(err, ErrUnsafeKey) {
				t.Errorf("Get(%q) = %v, want ErrUnsafeKey", k, err)
			}
		}
	})
}

// A bad key mid-batch must not leave the store half-updated.
func TestCommitIsAllOrNothing(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		if err := s.Put("keep", []byte("before")); err != nil {
			t.Fatal(err)
		}
		err := s.Commit([]Op{
			{Kind: OpPut, Key: "keep", Value: []byte("after")},
			{Kind: OpPut, Key: "../escape", Value: []byte("x")},
			{Kind: OpPut, Key: "new", Value: []byte("x")},
		})
		if !errors.Is(err, ErrUnsafeKey) {
			t.Fatalf("Commit = %v, want ErrUnsafeKey", err)
		}
		got, _ := s.Get("keep")
		if string(got) != "before" {
			t.Errorf("a rejected batch still applied a write: keep = %q", got)
		}
		if _, err := s.Get("new"); !errors.Is(err, ErrNotFound) {
			t.Error("a rejected batch created a key")
		}
	})
}

func TestCommitAppliesEveryWrite(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		if err := s.Put("gone", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit([]Op{
			{Kind: OpPut, Key: "a", Value: []byte("1")},
			{Kind: OpPut, Key: "b", Value: []byte("2")},
			{Kind: OpDelete, Key: "gone"},
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		for k, want := range map[string]string{"a": "1", "b": "2"} {
			got, err := s.Get(k)
			if err != nil || string(got) != want {
				t.Errorf("Get(%s) = %q, %v", k, got, err)
			}
		}
		if _, err := s.Get("gone"); !errors.Is(err, ErrNotFound) {
			t.Error("delete in batch did not apply")
		}
	})
}

func TestListIsSortedAndPrefixed(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		for _, k := range []string{"p/c", "p/a", "p/b", "other/x"} {
			if err := s.Put(k, []byte("v")); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.List("p/")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"p/a", "p/b", "p/c"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("List = %v, want %v", got, want)
		}
	})
}

func TestConcurrentWritesAreSafe(t *testing.T) {
	t.Parallel()
	both(t, func(t *testing.T, s Store) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_ = s.Put(fmt.Sprintf("k%02d", i), []byte("v"))
			}(i)
		}
		wg.Wait()
		keys, err := s.List("k")
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 20 {
			t.Errorf("List = %d keys, want 20", len(keys))
		}
	})
}

// Durable-only behaviour below.

func TestDurableSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1, err := OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Put("persisted", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenDurable(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.Get("persisted")
	if err != nil || string(got) != "value" {
		t.Errorf("after restart Get = %q, %v; want the stored value", got, err)
	}
}

// Two instances sharing a data directory would interleave writes and corrupt
// both copies, so the second must refuse to start.
func TestDurableRefusesSharedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1, err := OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	_, err = OpenDurable(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second OpenDurable = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "owner.lock") {
		t.Errorf("error should name the lock file so it can be cleared: %v", err)
	}
}

func TestDurableReleasesDirectoryOnClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1, err := OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenDurable(dir)
	if err != nil {
		t.Fatalf("directory was not released on Close: %v", err)
	}
	_ = s2.Close()
}

// Memory mode must leave nothing durable behind.
func TestMemoryLeavesNothingOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewMemory()
	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("memory store wrote %d entries to disk", len(entries))
	}
}

// A failed flush must leave the in-memory state untouched, so memory and disk
// cannot disagree.
func TestDurableCommitFailureLeavesStateUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Put("k", []byte("before")); err != nil {
		t.Fatal(err)
	}
	// Make the directory unwritable so the temp-file write fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("cannot make directory read-only: %v", err)
	}
	defer os.Chmod(dir, 0o755)

	if err := s.Put("k", []byte("after")); err == nil {
		t.Skip("write unexpectedly succeeded; filesystem does not enforce the mode")
	}
	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "before" {
		t.Errorf("state changed despite a failed commit: %q", got)
	}
}

func TestDurableFilesAreInsideTheDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := OpenDurable(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put("a/b/c", []byte("v")); err != nil {
		t.Fatal(err)
	}
	// No key may create a path outside the data directory.
	parent := filepath.Dir(dir)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() != filepath.Base(dir) && strings.Contains(e.Name(), "metadata") {
			t.Errorf("store wrote outside its directory: %s", e.Name())
		}
	}
}

// A lock left behind by a process that no longer exists is reclaimed.
//
// O_EXCL on a plain file cannot tell a live owner from a machine that lost
// power. Without this, any unclean exit made the directory permanently
// unopenable until somebody deleted the file by hand.
func TestDurableReclaimsALockWhoseOwnerIsGone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A PID that cannot be running: the kernel refuses 0 as a process id, and
	// a very high one is not allocated on any platform this builds for.
	lock := filepath.Join(dir, "owner.lock")
	if err := os.WriteFile(lock, []byte("4194303\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenDurable(dir)
	if err != nil {
		t.Fatalf("a lock owned by a dead process was not reclaimed: %v", err)
	}
	defer s.Close()

	b, err := os.ReadFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock still names the old owner: %q", b)
	}
}

// A lock this code cannot read is not evidence that nothing owns the
// directory, so it refuses rather than guessing.
func TestDurableRefusesAnUnreadableLock(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"", "not a pid", "-1", "0"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "owner.lock"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDurable(dir); !errors.Is(err, ErrLocked) {
			t.Errorf("a lock containing %q was reclaimed; got %v, want ErrLocked", content, err)
		}
	}
}

// A live owner is still a live owner, whatever else changed.
func TestDurableRespectsALockWhoseOwnerIsRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// This test's own process is, by construction, running.
	if err := os.WriteFile(filepath.Join(dir, "owner.lock"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDurable(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("a lock owned by a running process was reclaimed: %v", err)
	}
}
