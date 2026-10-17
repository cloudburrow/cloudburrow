package resourcemanager

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestRecreatedProjectGetsANewNumber (#1544): a project's first number is
// the derived one; a project made again under a purged ID gets another,
// kept in its record and reported by v1 and v3; a purged number is never
// given out again; the purge hooks still see the purged project's number.
func TestRecreatedProjectGetsANewNumber(t *testing.T) {
	r := newRegistry(t)
	resource.SetProjectNumberResolver(r.NumberOf)
	t.Cleanup(func() { resource.SetProjectNumberResolver(nil) })

	const id = "reused-number"
	p, err := r.Create(Project{ProjectID: id})
	if err != nil {
		t.Fatal(err)
	}
	first := resource.DerivedProjectNumber(id)
	if p.Number != first || resource.ProjectNumber(id) != first {
		t.Fatalf("first number = %d (resolved %d), want the derived %d", p.Number, resource.ProjectNumber(id), first)
	}
	var hookSaw int64
	r.OnPurge(func(pid string) error {
		hookSaw = resource.ProjectNumber(pid)
		return nil
	})
	seen := map[int64]bool{first: true}
	for i := 0; i < 5; i++ {
		if _, err := r.Delete(id); err != nil {
			t.Fatal(err)
		}
		before := resource.ProjectNumber(id)
		if err := r.Purge(id); err != nil {
			t.Fatal(err)
		}
		if hookSaw != before {
			t.Fatalf("purge hook saw number %d, want the purged project's %d", hookSaw, before)
		}
		p, err := r.Create(Project{ProjectID: id})
		if err != nil {
			t.Fatal(err)
		}
		if seen[p.Number] || p.Number < 100000000000 || p.Number > 999999999999 {
			t.Fatalf("re-created project got number %d; numbers so far %v", p.Number, seen)
		}
		seen[p.Number] = true
		if got := resource.ProjectNumber(id); got != p.Number {
			t.Fatalf("resolved number %d, record %d", got, p.Number)
		}
		if got := toV1(p).ProjectNumber; got != p.Number {
			t.Fatalf("v1 projectNumber %d, want %d", got, p.Number)
		}
		if got, want := toProto(p).GetName(), "projects/"+strconv.FormatInt(p.Number, 10); got != want {
			t.Fatalf("v3 name %q, want %q", got, want)
		}
		// The new number names the project; the old one does not.
		if got, err := r.resolveProjectLocked("projects/" + strconv.FormatInt(p.Number, 10)); err != nil || got.ProjectID != id {
			t.Fatalf("resolve by new number: %v %v", got, err)
		}
	}
	over, err := r.NumberOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if over[id] != resource.ProjectNumber(id) || len(over) != 1 {
		t.Fatalf("overrides %v", over)
	}

	// A project whose derived number is retired gets another.
	other := "another-number"
	_ = r.st.Put(retiredNumberPrefix+strconv.FormatInt(resource.DerivedProjectNumber(other), 10), []byte("x"))
	q, err := r.Create(Project{ProjectID: other})
	if err != nil {
		t.Fatal(err)
	}
	if q.Number == resource.DerivedProjectNumber(other) || seen[q.Number] {
		t.Fatalf("project got retired number %d", q.Number)
	}
}

// TestLegacyRecordKeepsDerivedNumber: a record written before #1544 has no
// number and keeps the derived one.
func TestLegacyRecordKeepsDerivedNumber(t *testing.T) {
	r := newRegistry(t)
	b, _ := json.Marshal(Project{Name: "projects/legacy-proj", ProjectID: "legacy-proj", State: StateActive})
	if err := r.st.Put(key("legacy-proj"), b); err != nil {
		t.Fatal(err)
	}
	p, err := r.Get("legacy-proj")
	if err != nil {
		t.Fatal(err)
	}
	want := resource.DerivedProjectNumber("legacy-proj")
	if n, ok := r.NumberOf("legacy-proj"); !ok || n != want || numberOf(p) != want {
		t.Fatalf("legacy number %d %v, want %d", n, ok, want)
	}
	if _, ok := r.NumberOf("never-registered"); ok {
		t.Fatal("an unregistered ID resolved")
	}
	// A lien on it carries the number.
	l, err := r.CreateLien(Lien{Parent: "projects/legacy-proj", Restrictions: []string{"resourcemanager.projects.delete"}, Reason: "r", Origin: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "liens/p" + strconv.FormatInt(want, 10) + "-l"; len(l.Name) < len(want) || l.Name[:len(want)] != want {
		t.Fatalf("lien %q", l.Name)
	}
}

// TestOnNumbersChanged: the hook hears of each create and purge, without
// the lock held, and not of a read.
func TestOnNumbersChanged(t *testing.T) {
	r := newRegistry(t)
	calls := 0
	r.OnNumbersChanged(func() {
		_, _ = r.NumberOverrides()
		_, _ = r.Get("hooked-proj") // the lock is not held
		calls++
	})
	if _, err := r.Create(Project{ProjectID: "hooked-proj"}); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Get("hooked-proj")
	if _, err := r.Delete("hooked-proj"); err != nil {
		t.Fatal(err)
	}
	if err := r.Purge("hooked-proj"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("hook called %d times, want 2 (create, purge)", calls)
	}
}
