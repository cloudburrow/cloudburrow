package resourcemanager

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	return New(store.NewMemory())
}

func TestCreateAndGet(t *testing.T) {
	r := newRegistry(t)
	p, err := r.Create(Project{ProjectID: "my-project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Name != "projects/my-project" {
		t.Errorf("Name = %q, want projects/my-project", p.Name)
	}
	if p.State != StateActive {
		t.Errorf("State = %q, want ACTIVE", p.State)
	}
	// The display name defaults to the identifier rather than being empty,
	// because a blank name in a picker is worse than a repeated one.
	if p.DisplayName != "my-project" {
		t.Errorf("DisplayName = %q, want the identifier", p.DisplayName)
	}
	if p.CreateTime == "" {
		t.Error("CreateTime is empty")
	}

	got, err := r.Get("my-project")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProjectID != p.ProjectID {
		t.Errorf("round trip lost the identifier: %+v", got)
	}
}

func TestDuplicateIsRefused(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Create(Project{ProjectID: "my-project"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Create(Project{ProjectID: "my-project"})
	if err == nil {
		t.Fatal("a duplicate was accepted")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error does not say why: %v", err)
	}
}

// The identifier rules are Google's. Enforcing them here means a project made
// through any path is one the real API would also have accepted, so a project
// that works locally does not fail the first time it is created for real.
func TestProjectIDRulesMatchGoogles(t *testing.T) {
	valid := []string{"my-project", "abc123", "a-b-c-d-e-f", "project-1", strings.Repeat("a", 30)}
	for _, id := range valid {
		if err := ValidateProjectID(id); err != nil {
			t.Errorf("%q was refused: %v", id, err)
		}
	}

	invalid := map[string]string{
		"short":                 "too short",
		"":                      "empty",
		"1project":              "starts with a digit",
		"-project":              "starts with a hyphen",
		"project-":              "ends with a hyphen",
		"My-Project":            "uppercase",
		"my_project":            "underscore",
		"my project":            "space",
		strings.Repeat("a", 31): "too long",
	}
	for id, why := range invalid {
		if err := ValidateProjectID(id); err == nil {
			t.Errorf("%q was accepted (%s)", id, why)
		}
	}
}

func TestListIsOrderedAndComplete(t *testing.T) {
	r := newRegistry(t)
	for _, id := range []string{"zebra-project", "alpha-project", "middle-project"} {
		if _, err := r.Create(Project{ProjectID: id}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d, want 3", len(got))
	}
	want := []string{"alpha-project", "middle-project", "zebra-project"}
	for i, p := range got {
		if p.ProjectID != want[i] {
			t.Errorf("List()[%d] = %q, want %q — ordering must be stable", i, p.ProjectID, want[i])
		}
	}
}

// A delete keeps the project in DELETE_REQUESTED for 30 days, as Google
// does (#1216): Undelete restores it within the window, its ID cannot be
// taken meanwhile, and after the window it is purged.
func TestDeleteKeepsTheProjectForUndelete(t *testing.T) {
	r := newRegistry(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if _, err := r.Create(Project{ProjectID: "doomed-project"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Undelete("doomed-project"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Undelete of an active project = %v, want FailedPrecondition", err)
	}
	p, err := r.Delete("doomed-project")
	if err != nil || p.State != StateDeleteRequested || p.DeleteTime == "" {
		t.Fatalf("Delete = %+v, %v", p, err)
	}
	if got, err := r.Get("doomed-project"); err != nil || got.State != StateDeleteRequested {
		t.Errorf("Get after delete = %+v, %v; want DELETE_REQUESTED", got, err)
	}
	if active, _ := r.Active(); len(active) != 0 {
		t.Errorf("Active lists a deleted project: %+v", active)
	}
	if _, err := r.Delete("doomed-project"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a second delete = %v, want FailedPrecondition", err)
	}
	if _, err := r.Create(Project{ProjectID: "doomed-project"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing a deleted project's ID = %v, want AlreadyExists", err)
	}
	if _, err := r.Update("doomed-project", "x", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Update of a deleted project = %v, want FailedPrecondition", err)
	}
	now = now.Add(29 * 24 * time.Hour)
	if p, err := r.Undelete("doomed-project"); err != nil || p.State != StateActive || p.DeleteTime != "" {
		t.Fatalf("Undelete within the window = %+v, %v", p, err)
	}
	if _, err := r.Delete("doomed-project"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(RetentionPeriod)
	if _, err := r.Undelete("doomed-project"); status.Code(err) != codes.NotFound {
		t.Errorf("Undelete after the window = %v, want NotFound", err)
	}
	if all, _ := r.List(); len(all) != 0 {
		t.Errorf("a purged project is still listed: %+v", all)
	}
	if _, err := r.Delete("missing-project"); status.Code(err) != codes.NotFound {
		t.Errorf("deleting a missing project = %v, want NotFound", err)
	}
}

// EnsureExists is how the instance's own project is registered at startup. It
// must be safe to call on every start, which means it cannot fail on the
// second one.
func TestEnsureExistsIsRepeatable(t *testing.T) {
	r := newRegistry(t)
	first, err := r.EnsureExists("demo-project")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.EnsureExists("demo-project")
	if err != nil {
		t.Fatalf("the second call failed: %v", err)
	}
	if first.CreateTime != second.CreateTime {
		t.Error("EnsureExists recreated the project instead of returning the existing one")
	}
	list, _ := r.List()
	if len(list) != 1 {
		t.Errorf("registry holds %d projects, want 1", len(list))
	}
}

func TestPersistsAcrossRegistries(t *testing.T) {
	st := store.NewMemory()
	if _, err := New(st).Create(Project{ProjectID: "kept-project"}); err != nil {
		t.Fatal(err)
	}
	// A second registry over the same store must see it: the registry holds no
	// state of its own, which is what lets the console and the API agree.
	if _, err := New(st).Get("kept-project"); err != nil {
		t.Errorf("a project written by one registry is invisible to another: %v", err)
	}
}

// TestEnsureExistsAcceptsAnInstanceNameThatIsNotACreatableID is the regression
// test for a defect that left the registry empty.
//
// An instance named "demo" has been answering for project "demo" since it
// started; resources may already sit under it. Applying the creation rules to
// that name refused it — four characters against a six-character minimum — so
// the registry stayed empty, the picker had nothing to show, and the failure
// was silent. Google validates when a project is created, not when one is
// referred to.
func TestEnsureExistsAcceptsAnInstanceNameThatIsNotACreatableID(t *testing.T) {
	r := newRegistry(t)

	// Too short to create...
	if err := ValidateProjectID("demo"); err == nil {
		t.Fatal("this test assumes \"demo\" is not a creatable ID")
	}
	if _, err := r.Create(Project{ProjectID: "demo"}); err == nil {
		t.Error("Create accepted an identifier that breaks the rules")
	}

	// ...but registrable, because it already exists.
	p, err := r.EnsureExists("demo")
	if err != nil {
		t.Fatalf("EnsureExists refused the instance's own project: %v", err)
	}
	if p.ProjectID != "demo" {
		t.Errorf("ProjectID = %q, want demo", p.ProjectID)
	}
	list, _ := r.List()
	if len(list) != 1 {
		t.Fatalf("the registry holds %d projects, want 1 — the picker would have nothing to show", len(list))
	}
}

// Registration still refuses what cannot be stored, so a bad instance name
// cannot corrupt the store's key space.
func TestEnsureExistsRefusesAnUnstorableName(t *testing.T) {
	r := newRegistry(t)
	for _, bad := range []string{"", "   ", "../escape", "with/slash"} {
		if _, err := r.EnsureExists(bad); err == nil {
			t.Errorf("EnsureExists(%q) was accepted", bad)
		}
	}
}

// TestUpdateChangesOnlyWhatIsASetting.
//
// The identifier is the project's identity, and the state and creation time are
// facts about what happened rather than settings. Google's UpdateProject accepts
// a field mask over displayName and labels and nothing else.
func TestUpdateChangesOnlyWhatIsASetting(t *testing.T) {
	r := newRegistry(t)
	made, err := r.Create(Project{ProjectID: "demo-one", DisplayName: "Demo"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Update("demo-one", "Renamed", map[string]string{"env": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Renamed" || got.Labels["env"] != "dev" {
		t.Fatalf("update = %+v", got)
	}
	if got.ProjectID != made.ProjectID || got.CreateTime != made.CreateTime ||
		got.State != made.State || got.Name != made.Name {
		t.Fatalf("update changed something that is not a setting:\nbefore %+v\nafter  %+v",
			made, got)
	}

	// Clearing the display name falls back to the identifier, because "no
	// display name" is not a state a project has.
	got, err = r.Update("demo-one", "  ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "demo-one" {
		t.Fatalf("cleared display name = %q, want the project ID", got.DisplayName)
	}
	if len(got.Labels) != 0 {
		t.Fatalf("labels = %v, want cleared", got.Labels)
	}

	if _, err := r.Update("no-such-project", "x", nil); err == nil {
		t.Fatal("updating a project that does not exist was accepted")
	}
}
