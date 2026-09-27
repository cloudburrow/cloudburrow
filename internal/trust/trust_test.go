package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashFollowsPathsContentsAndTheExecutableBit(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.sh")
	b := filepath.Join(dir, "b.sh")
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "echo a", 0o755)
	write(b, "echo b", 0o755)
	hash := func(files ...string) string {
		t.Helper()
		h, err := Hash(files)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(a, b)
	if hash(b, a) != base {
		t.Error("the hash depends on the order the files were listed in")
	}
	if hash(a) == base {
		t.Error("removing a file did not change the hash")
	}
	write(b, "echo B", 0o755)
	if hash(a, b) == base {
		t.Error("editing a file did not change the hash")
	}
	write(b, "echo b", 0o644)
	if hash(a, b) == base {
		t.Error("the executable bit is not part of the hash")
	}
	write(b, "echo b", 0o755)
	if hash(a, b) != base {
		t.Error("the same files hash differently")
	}
	if _, err := Hash([]string{filepath.Join(dir, "gone")}); err == nil {
		t.Error("a missing file hashed")
	}
}

func TestRecordAndLookup(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if _, ok, err := Lookup(state, "/repo"); ok || err != nil {
		t.Fatalf("an empty store has a record: %v %v", ok, err)
	}
	if err := Record(state, "/repo", "abc", []string{"/repo/z", "/repo/a"}); err != nil {
		t.Fatal(err)
	}
	if err := Record(state, "/other", "def", nil); err != nil {
		t.Fatal(err)
	}
	e, ok, err := Lookup(state, "/repo")
	if err != nil || !ok || e.SHA256 != "abc" || len(e.Files) != 2 || e.Files[0] != "/repo/a" {
		t.Errorf("Lookup = %+v %v %v", e, ok, err)
	}
	if e, ok, _ := Lookup(state, "/other"); !ok || e.SHA256 != "def" {
		t.Errorf("a second record replaced the first: %+v", e)
	}
	info, err := os.Stat(filepath.Join(state, FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the store is %v (%v), want 0600", info.Mode().Perm(), err)
	}
}
