package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree makes directories under a temp root, each holding one .go file when
// its name has no trailing slash, and none (a container) when it has one.
func tree(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		p := filepath.Join(root, strings.TrimSuffix(d, "/"))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(d, "/") {
			if err := os.WriteFile(filepath.Join(p, "x.go"), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func doc(rows ...string) string {
	return "# Architecture\n\n## 3. Module boundaries\n\n| Path | Purpose |\n|---|---|\n" + strings.Join(rows, "\n") + "\n\n## 4. Next\n\n| `internal/ignored/` | outside the section |\n"
}

func TestModuleMapAcceptsATreeItDescribes(t *testing.T) {
	root := tree(t, "internal/config", "internal/service/", "internal/service/kms", "internal/transport/", "internal/transport/rest")
	got, err := checkModuleMap(doc(
		"| `internal/config/` | Configuration. |",
		"| `internal/service/kms/` | Cloud KMS. |",
		"| `internal/transport/rest/` | REST plumbing. |",
		"| `internal/k8s/` | **Planned** (#599): the one runner. |",
	), root)
	if err != nil || len(got) != 0 {
		t.Fatalf("checkModuleMap = %v, %v; want no problems", got, err)
	}
}

func TestModuleMapNamesEachDrift(t *testing.T) {
	root := tree(t, "internal/config", "internal/archtest", "internal/service/", "internal/service/kms", "internal/service/new")
	got, err := checkModuleMap(doc(
		"| `internal/config/` | Configuration. |",
		"| `internal/gone/` | Removed long ago. |",
		"| `internal/archtest/` | **Planned** (#672): tests only. |",
		"| `internal/service/kms/` | Cloud KMS. |",
	), root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"lists internal/gone, which does not exist",
		"internal/archtest is marked Planned but exists",
		"does not list internal/service/new/",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "internal/service/\n") || strings.Contains(joined, "does not list internal/service/\n") {
		t.Errorf("a container directory was required to be listed:\n%s", joined)
	}
	if len(got) != 3 {
		t.Errorf("got %d problems, want 3:\n%s", len(got), joined)
	}
}

func TestModuleMapWithNoTableIsAProblem(t *testing.T) {
	got, err := checkModuleMap("# Architecture\n\nNo table here.\n", t.TempDir())
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "no module map table") {
		t.Fatalf("checkModuleMap = %v, %v", got, err)
	}
}

func TestConsoleSubjectVerifiedAndNotSupported(t *testing.T) {
	compat := "## Storage\n\n| Pagination | **Not supported** | elsewhere |\n\n" +
		"## Console\n\n| Claim | Status | Evidence |\n|---|---|---|\n" +
		"| Pagination | **Verified** | `TestPaging` |\n" +
		"| **Pagination** | **Not supported** | stale |\n" +
		"| Traffic splitting | **Not supported** | |\n" +
		"| Deploy a service | **Verified** | `TestDeploy` |\n" +
		"| Visual parity | **Partial — unverifiable today** | |\n" +
		"\n## Next\n\n| Deploy a service | **Not supported** | outside the section |\n"
	got := checkConsole(compat)
	if len(got) != 1 || !strings.Contains(got[0], `"pagination"`) {
		t.Fatalf("checkConsole = %v; want only pagination flagged", got)
	}
}
