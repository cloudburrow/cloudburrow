package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestToolBoxPinsMatchCI: the compat tool box (test/compat/toolbox_test.go,
// #2174) runs its own Terraform, OpenTofu and hashicorp/google provider, so
// its image must carry the versions ci.yml installs and the test modules
// require; otherwise a bump in one place would leave the tests running
// another version than the one CI and compatibility.md name.
func TestToolBoxPinsMatchCI(t *testing.T) {
	root := repoRoot(t)
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dockerfile := read("test/compat/testdata/toolbox/Dockerfile")
	ci := read(".github/workflows/ci.yml")
	realtools := read("test/compat/realtools_test.go")
	find := func(what, src, pattern string) string {
		t.Helper()
		m := regexp.MustCompile(pattern).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no %s (%s)", what, pattern)
		}
		return m[1]
	}
	for _, c := range []struct{ name, box, want string }{
		{"Terraform", find("ARG TERRAFORM_VERSION", dockerfile, `(?m)^ARG TERRAFORM_VERSION=(\S+)$`),
			find("ci.yml's terraform_version", ci, `terraform_version: (\S+)`)},
		{"OpenTofu", find("ARG TOFU_VERSION", dockerfile, `(?m)^ARG TOFU_VERSION=(\S+)$`),
			find("ci.yml's OpenTofu download", ci, `opentofu/releases/download/v([0-9.]+)/`)},
		{"hashicorp/google", find("ARG GOOGLE_PROVIDER_VERSION", dockerfile, `(?m)^ARG GOOGLE_PROVIDER_VERSION=(\S+)$`),
			find("googleProviderVersion", realtools, `const googleProviderVersion = "([^"]+)"`)},
	} {
		if c.box != c.want {
			t.Errorf("the tool box's %s is %s, but %s is pinned elsewhere: change testdata/toolbox/Dockerfile (and its checksums) with it", c.name, c.box, c.want)
		}
	}
}
