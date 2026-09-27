package components_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
)

type entry struct {
	Image        string            `json:"image"`
	Version      string            `json:"version"`
	Digest       *string           `json:"digest"`
	Verification string            `json:"verification"`
	Source       string            `json:"source"`
	Manifests    map[string]string `json:"manifests"`
	Checksums    map[string]string `json:"checksums"`
}

func inventory(t *testing.T) (map[string]map[string]entry, []any) {
	t.Helper()
	raw, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
		Unresolved []any                                 `json:"unresolved"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]entry{}
	for g, items := range d.Components {
		out[g] = map[string]entry{}
		for k, v := range items {
			var e entry
			if json.Unmarshal(v, &e) == nil {
				out[g][k] = e
			}
		}
	}
	return out, d.Unresolved
}

func pinned(e entry) string {
	if e.Digest == nil {
		return e.Image + ":" + e.Version
	}
	return e.Image + "@" + *e.Digest
}

// The binary ships the pins dependencies.json records (#597): a Go constant
// that disagreed with the inventory would make the release's "tested
// against" list false without anything failing.
func TestPinsMatchTheInventory(t *testing.T) {
	inv, unresolved := inventory(t)
	node := inv["cluster"]["kubernetesNodeImage"]
	checks := map[string][2]string{
		"PubSubImage":      {components.PubSubImage, pinned(inv["emulatorBackends"]["googleCloudCliEmulatorsImage"])},
		"SpannerImage":     {components.SpannerImage, pinned(inv["emulatorBackends"]["spannerEmulator"])},
		"KnativeVersion":   {components.KnativeVersion, inv["serving"]["knativeServing"].Version},
		"net-kourier":      {components.KnativeVersion, inv["serving"]["knativeNetKourier"].Version},
		"DefaultNodeImage": {config.DefaultNodeImage, node.Image + ":" + node.Version + "@" + *node.Digest},
		"KindVersion":      {doctor.KindVersion, inv["cluster"]["kind"].Version},
		"BuilderImage":     {buildpacks.BuilderImage, pinned(inv["build"]["buildpacksBuilder"])},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q; dependencies.json has %q", name, c[0], c[1])
		}
	}
	want := map[string]string{}
	for _, comp := range []string{"knativeServing", "knativeNetKourier"} {
		for f, sum := range inv["serving"][comp].Manifests {
			want[f] = sum
		}
	}
	for _, m := range components.KnativeManifests() {
		if want[m.Name] != "sha256:"+m.SHA256 {
			t.Errorf("KnativeManifests %s = sha256:%s; dependencies.json has %q", m.Name, m.SHA256, want[m.Name])
		}
		delete(want, m.Name)
	}
	for f := range want {
		t.Errorf("dependencies.json pins %s, which KnativeManifests does not apply", f)
	}
	if !strings.Contains(config.DefaultNodeImage, "@sha256:") {
		t.Errorf("DefaultNodeImage %q is not pinned by digest", config.DefaultNodeImage)
	}
	if len(unresolved) != 0 {
		t.Errorf("dependencies.json still lists unresolved pins: %v", unresolved)
	}
}

// Every pulled image recorded as digest-verified has a digest, and every
// release YAML recorded as checksum-verified has its per-file hashes.
func TestInventoryVerificationsHaveTheirPins(t *testing.T) {
	inv, _ := inventory(t)
	for g, items := range inv {
		for k, e := range items {
			switch {
			case e.Verification == "image-digest" && e.Image != "" && (e.Digest == nil || !strings.HasPrefix(*e.Digest, "sha256:")):
				t.Errorf("%s.%s is image-digest verified with no digest", g, k)
			case e.Verification == "release-yaml-checksum" && len(e.Manifests) == 0:
				t.Errorf("%s.%s is release-yaml-checksum verified with no manifest hashes", g, k)
			}
			for f, sum := range e.Checksums {
				if !strings.HasPrefix(sum, "sha256:") || len(sum) != len("sha256:")+64 {
					t.Errorf("%s.%s checksum for %s = %q, want sha256:<64 hex>", g, k, f, sum)
				}
			}
		}
	}
}

// dockerfilePins is what the inventory allows a Dockerfile to consume: the
// image@digest of every digest-pinned entry, and every recorded release
// checksum as bare hex.
func dockerfilePins(inv map[string]map[string]entry) (images, sums map[string]bool) {
	images, sums = map[string]bool{}, map[string]bool{}
	for _, items := range inv {
		for _, e := range items {
			if e.Image != "" && e.Digest != nil {
				images[e.Image+"@"+*e.Digest] = true
			}
			for _, sum := range e.Checksums {
				sums[strings.TrimPrefix(sum, "sha256:")] = true
			}
		}
	}
	return images, sums
}

var (
	// curl or wget run as a command, not named as a package to install.
	downloads = regexp.MustCompile(`(^RUN|&&|;|\|\||\||\$\(|\()\s*(curl|wget)\s`)
	hexSum    = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
)

// dockerfileProblems reports every base image a Dockerfile takes by tag, or
// by a digest the inventory does not record, and every curl or wget whose
// RUN step does not check a checksum the inventory records.
func dockerfileProblems(content string, images, sums map[string]bool) []string {
	var problems []string
	stages := map[string]bool{"scratch": true}
	joined := strings.ReplaceAll(content, "\\\n", " ")
	for _, line := range strings.Split(joined, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "FROM":
			args := f[1:]
			for len(args) > 0 && strings.HasPrefix(args[0], "--") {
				args = args[1:]
			}
			if len(args) == 0 {
				problems = append(problems, "FROM with no image: "+line)
				continue
			}
			img := args[0]
			switch {
			case stages[strings.ToLower(img)]:
			case !strings.Contains(img, "@sha256:"):
				problems = append(problems, "FROM "+img+" is not pinned by digest")
			case !images[img]:
				problems = append(problems, "FROM "+img+" is not a digest dependencies.json records")
			}
			if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
				stages[strings.ToLower(args[2])] = true
			}
		case "RUN":
			if !downloads.MatchString(line) {
				continue
			}
			if !strings.Contains(line, "sha256sum -c") {
				problems = append(problems, "RUN downloads with no sha256sum -c check: "+strings.TrimSpace(line))
				continue
			}
			found := hexSum.FindAllString(line, -1)
			if len(found) == 0 {
				problems = append(problems, "RUN downloads with no pinned checksum: "+strings.TrimSpace(line))
			}
			for _, sum := range found {
				if !sums[sum] {
					problems = append(problems, "RUN checks "+sum+", which dependencies.json does not record")
				}
			}
		}
	}
	return problems
}

// Every Dockerfile in the repository builds from bases pinned by a digest
// the inventory records and checks every download against a recorded
// checksum (#687). The released litert-lm image is the one that matters
// most: a mutable base or an unchecked download would make its provenance
// attestation describe inputs nobody reviewed.
func TestDockerfilesPinTheInventory(t *testing.T) {
	inv, _ := inventory(t)
	images, sums := dockerfilePins(inv)
	root := filepath.Join("..", "..")
	var seen []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "Dockerfile" && !strings.HasSuffix(d.Name(), ".Dockerfile") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		seen = append(seen, filepath.ToSlash(rel))
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range dockerfileProblems(string(b), images, sums) {
			t.Errorf("%s: %s", filepath.ToSlash(rel), p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deploy/litert-lm/Dockerfile", "testdata/worker/Dockerfile"} {
		found := false
		for _, s := range seen {
			found = found || s == want
		}
		if !found {
			t.Errorf("%s was not checked; found %v", want, seen)
		}
	}

	// The litert-lm build fetches exactly the bazelisk the inventory records,
	// with a checksum for each architecture the release publishes.
	b, err := os.ReadFile(filepath.Join(root, "deploy", "litert-lm", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	df := string(b)
	bz := inv["localai"]["bazelisk"]
	if !strings.Contains(df, "ARG BAZELISK_VERSION="+bz.Version+"\n") {
		t.Errorf("deploy/litert-lm/Dockerfile does not fetch bazelisk %s, the version dependencies.json records", bz.Version)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		sum := strings.TrimPrefix(bz.Checksums["bazelisk-linux-"+arch], "sha256:")
		if sum == "" || !strings.Contains(df, arch+") sum="+sum+" ;;") {
			t.Errorf("deploy/litert-lm/Dockerfile does not check bazelisk-linux-%s against dependencies.json's sha256:%s", arch, sum)
		}
	}
	base := inv["localai"]["litertLMBase"]
	if base.Digest == nil || strings.Count(df, "FROM "+base.Image+"@"+*base.Digest) != 2 {
		t.Errorf("deploy/litert-lm/Dockerfile's two stages do not both build FROM localai.litertLMBase")
	}
}

// The check itself rejects what #687 found: a base taken by tag, a digest
// the inventory does not know, and a download with no checksum.
func TestDockerfileProblemsCatchesUnpinnedInputs(t *testing.T) {
	good := strings.Repeat("a", 64)
	images := map[string]bool{"debian:trixie-slim@sha256:" + good: true}
	sums := map[string]bool{good: true}
	cases := map[string]int{
		"FROM debian:trixie-slim AS build\nFROM build\n":                                                              1,
		"FROM debian:trixie-slim@sha256:" + strings.Repeat("b", 64) + "\n":                                            1,
		"FROM debian:trixie-slim@sha256:" + good + " AS build\nFROM build\nFROM scratch\n":                            0,
		"FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:" + good + "\n":                                     0,
		"RUN curl -fsSL -o /b https://example.invalid/b \\\n    && chmod +x /b\n":                                     1,
		"RUN apt-get install -y curl wget && rm -rf /var/lib/apt/lists/*\n":                                           0,
		"RUN true \\\n    && wget -O /b https://example.invalid/b\n":                                                  1,
		"RUN curl -o /b https://example.invalid/b && echo \"" + good + "  /b\" | sha256sum -c -\n":                    0,
		"RUN wget -O /b https://example.invalid/b && echo \"" + strings.Repeat("c", 64) + "  /b\" | sha256sum -c -\n": 1,
	}
	for in, want := range cases {
		if got := dockerfileProblems(in, images, sums); len(got) != want {
			t.Errorf("dockerfileProblems(%q) = %v, want %d problem(s)", in, got, want)
		}
	}
}

// The real tools the compat job runs are the ones dependencies.json records
// (#694): the gcloud archive CI downloads and the checksum it checks, and the
// hashicorp/google version the Terraform modules require and the committed
// lock file locks.
func TestRealToolPinsMatchTheInventory(t *testing.T) {
	inv, _ := inventory(t)
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	gcloud := inv["realTools"]["googleCloudCli"]
	if gcloud.Digest == nil || !strings.HasPrefix(*gcloud.Digest, "sha256:") || !strings.Contains(gcloud.Source, "-"+gcloud.Version+"-") {
		t.Fatalf("realTools.googleCloudCli = %+v; want a versioned source and a sha256 digest", gcloud)
	}
	ci := read("../../.github/workflows/ci.yml")
	if want := "curl -fsSLo \"$RUNNER_TEMP/gcloud.tar.gz\" " + gcloud.Source + "\n"; !strings.Contains(ci, want) {
		t.Errorf("ci.yml does not download %s", gcloud.Source)
	}
	if want := "echo \"" + strings.TrimPrefix(*gcloud.Digest, "sha256:") + "  $RUNNER_TEMP/gcloud.tar.gz\" | sha256sum -c -"; !strings.Contains(ci, want) {
		t.Errorf("ci.yml does not check the gcloud archive against %s", *gcloud.Digest)
	}
	provider := inv["realTools"]["hashicorpGoogleProvider"].Version
	if provider == "" {
		t.Fatal("realTools.hashicorpGoogleProvider has no version")
	}
	if want := "const googleProviderVersion = \"" + provider + "\""; !strings.Contains(read("../../test/compat/realtools_test.go"), want) {
		t.Errorf("test/compat/realtools_test.go does not have %s", want)
	}
	lock := read("../../test/compat/testdata/terraform/.terraform.lock.hcl")
	if n := strings.Count(lock, "  version     = \""+provider+"\"\n"); n != 2 {
		t.Errorf("the lock file locks %s for %d registries; want 2", provider, n)
	}
}

// The release pipeline's own tools are the ones dependencies.json records
// (#695): release.yml builds with exactly toolchain.goRelease, and every
// workflow that installs govulncheck installs toolchain.govulncheck.
func TestWorkflowToolPinsMatchTheInventory(t *testing.T) {
	inv, _ := inventory(t)
	goRelease := inv["toolchain"]["goRelease"].Version
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(goRelease) {
		t.Fatalf("toolchain.goRelease version %q is not an exact Go patch release", goRelease)
	}
	vuln := inv["toolchain"]["govulncheck"].Version
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(vuln) {
		t.Fatalf("toolchain.govulncheck version %q is not an exact module version", vuln)
	}

	files, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	goVersion := regexp.MustCompile(`(?m)^\s*go-version(?:-file)?:\s*(.*?)\s*$`)
	vulnInstall := regexp.MustCompile(`golang\.org/x/vuln/cmd/govulncheck@(\S+)`)
	installs := map[string]int{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body, name := string(raw), filepath.Base(f)
		for _, m := range vulnInstall.FindAllStringSubmatch(body, -1) {
			installs[name]++
			if m[1] != vuln {
				t.Errorf("%s installs govulncheck@%s; dependencies.json toolchain.govulncheck is %s", name, m[1], vuln)
			}
		}
		if name != "release.yml" {
			continue
		}
		setups := goVersion.FindAllStringSubmatch(body, -1)
		if len(setups) == 0 {
			t.Error("release.yml sets up no Go version")
		}
		for _, m := range setups {
			if got := strings.Trim(m[1], `"'`); !strings.HasPrefix(strings.TrimSpace(m[0]), "go-version:") || got != goRelease {
				t.Errorf("release.yml has %q; want go-version: %q (dependencies.json toolchain.goRelease)", strings.TrimSpace(m[0]), goRelease)
			}
		}
	}
	for _, name := range []string{"ci.yml", "release.yml"} {
		if installs[name] == 0 {
			t.Errorf("%s no longer installs govulncheck; update this test and dependencies.json toolchain.govulncheck.usedBy", name)
		}
	}
}

// No workflow installs anything at @latest (#695): an unpinned tool's
// verdict can change without a commit, and nothing records what ran.
func TestNoWorkflowInstallsLatest(t *testing.T) {
	files, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "@latest") {
				t.Errorf("%s:%d uses @latest: %s", filepath.Base(f), i+1, strings.TrimSpace(line))
			}
		}
	}
}
