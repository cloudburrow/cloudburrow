package resource

import (
	"errors"
	"strings"
	"testing"
)

// Resource IDs are untrusted input that reaches the filesystem and the
// Kubernetes API. Anything that could address a parent path must be refused.
func TestValidIDRejectsTraversal(t *testing.T) {
	t.Parallel()
	bad := []string{
		"", ".", "..",
		"../etc/passwd",
		"a/b",
		`a\b`,
		"%2e%2e",
		"%2E%2E",
		"foo%2Fbar",
		"foo%2fbar",
		"foo%5Cbar",
		"a\x00b",
		"/leading",
		"trailing/",
		"-leading-hyphen-is-not-alnum",
	}
	for _, id := range bad {
		t.Run(strings.ReplaceAll(id, "\x00", "NUL"), func(t *testing.T) {
			t.Parallel()
			if ValidID(id) {
				t.Errorf("ValidID(%q) = true; this could escape its namespace", id)
			}
		})
	}
}

func TestValidIDAcceptsReasonableNames(t *testing.T) {
	t.Parallel()
	good := []string{"a", "my-queue", "my_queue", "Queue1", "a.b", "x~y", "a+b", "task-00001"}
	for _, id := range good {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
}

func TestParseProject(t *testing.T) {
	t.Parallel()
	if got, err := ParseProject("projects/my-project"); err != nil || got != "my-project" {
		t.Errorf("ParseProject = %q, %v", got, err)
	}
	for _, bad := range []string{"", "projects", "projects/", "project/x", "projects/x/extra", "projects/AB", "projects/-bad"} {
		if _, err := ParseProject(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseProject(%q) = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	n, err := Parse("projects/my-project/locations/us-central1/queues/my-queue")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.Project != "my-project" || n.Location != "us-central1" || n.Collection != "queues" || n.ID != "my-queue" {
		t.Errorf("Parse = %+v", n)
	}
	if got := n.String(); got != "projects/my-project/locations/us-central1/queues/my-queue" {
		t.Errorf("round trip = %q", got)
	}
}

func TestParseNested(t *testing.T) {
	t.Parallel()
	n, err := Parse("projects/my-project/locations/us-central1/queues/q1/tasks/t1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.ParentCollection != "queues" || n.ParentID != "q1" || n.Collection != "tasks" || n.ID != "t1" {
		t.Errorf("Parse nested = %+v", n)
	}
	if got := n.String(); got != "projects/my-project/locations/us-central1/queues/q1/tasks/t1" {
		t.Errorf("round trip = %q", got)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"queues/q",
		"projects/p/queues/q",
		"projects/my-project/locations/us-central1/queues",
		"projects/my-project/locations/us-central1/queues/q/extra",
		"projects/my-project/locations/US_CENTRAL/queues/q",
		"projects/my-project/locations/us-central1/queues/..",
		"projects/my-project/locations/us-central1/queues/q/tasks/..",
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(name); !errors.Is(err, ErrMalformed) {
				t.Errorf("Parse(%q) = %v, want ErrMalformed", name, err)
			}
		})
	}
}

// Project isolation must be structural, not conventional: the same ID in two
// projects has to produce different keys by construction.
func TestProjectIsolationIsStructural(t *testing.T) {
	t.Parallel()
	a, err := Parse("projects/project-aaa/locations/us-central1/queues/shared")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("projects/project-bbb/locations/us-central1/queues/shared")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key() == b.Key() {
		t.Fatalf("identical keys across projects: %q", a.Key())
	}
	// Location must isolate too.
	c, err := Parse("projects/project-aaa/locations/europe-west1/queues/shared")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key() == c.Key() {
		t.Fatalf("identical keys across locations: %q", a.Key())
	}
}

func TestParseLogName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string][2]string{
		"projects/demo-project/logs/app":                                            {"demo-project", "app"},
		"/projects/demo-project/logs/app":                                           {"demo-project", "app"},
		"projects/demo-project/logs/cloudresourcemanager.googleapis.com%2Factivity": {"demo-project", "cloudresourcemanager.googleapis.com/activity"},
	} {
		p, id, err := ParseLogName(name)
		if err != nil || p != want[0] || id != want[1] {
			t.Errorf("ParseLogName(%q) = %q, %q, %v; want %q, %q", name, p, id, err, want[0], want[1])
		}
	}
	for _, name := range []string{
		"",
		"projects/demo-project/logs/",
		"projects/demo-project/logs/a/b",
		"projects/demo-project/logs/a%20b",
		"projects/demo-project/logs/a%zz",
		"projects/demo-project/logs/" + strings.Repeat("a", 512),
		"projects/Bad_Project/logs/app",
		"organizations/123/logs/app",
		"projects/demo-project/sinks/app",
	} {
		if _, _, err := ParseLogName(name); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseLogName(%q) = %v, want ErrMalformed", name, err)
		}
	}
}
