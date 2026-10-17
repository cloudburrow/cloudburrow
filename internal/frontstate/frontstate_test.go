package frontstate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreWritesEachCopyOnceAndRemovesTheGone (#1514): the records read
// back as written; a copy is written when first saved and not again while
// it is the same copy, a new copy under the same key replaces it, a copy
// no longer held is removed, and Clear leaves nothing.
func TestStoreWritesEachCopyOnceAndRemovesTheGone(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct{ N int }
	encodes := 0
	copyOf := func(data *string) Copy {
		return Copy{Data: data, Encode: func() ([]byte, error) { encodes++; return Pack([]byte(*data), []byte("x")), nil }}
	}
	a, b := "a", "b"
	if err := s.Save(rec{1}, map[string]Copy{"k1": copyOf(&a), "k2": copyOf(&b)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(rec{2}, map[string]Copy{"k1": copyOf(&a), "k2": copyOf(&b)}); err != nil {
		t.Fatal(err)
	}
	if encodes != 2 {
		t.Errorf("%d encodes for two copies saved twice, want 2", encodes)
	}
	a2 := "a2"
	if err := s.Save(rec{3}, map[string]Copy{"k1": copyOf(&a2)}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ReadCopy("k1")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := Unpack(raw)
	if err != nil || len(parts) != 2 || string(parts[0]) != "a2" {
		t.Errorf("the copy k1 = %q, %v; want the new one", parts, err)
	}
	if _, err := s.ReadCopy("k2"); !os.IsNotExist(err) {
		t.Errorf("the copy k2, no longer held: %v, want it removed", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got rec
	if ok, err := s2.Load(&got); !ok || err != nil || got.N != 3 {
		t.Errorf("Load = %v, %v, %+v; want the last records", ok, err, got)
	}
	if err := s2.Clear(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s2.Load(&got); ok {
		t.Error("records left after Clear")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, copiesDir)); len(left) != 0 {
		t.Errorf("%d copies left after Clear", len(left))
	}
	if _, err := Unpack([]byte{0xff}); err == nil {
		t.Error("Unpack of a bad copy succeeded")
	}
}
