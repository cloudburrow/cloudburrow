package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The macOS signing job (#605) can only really run on a tagged release with
// the maintainer's Apple secrets. What can be checked here is its place in
// the job graph, that it skips cleanly without the secrets, and that it
// cleans up after itself.

// signingSecrets are the repository secrets the maintainer adds, by the
// names docs/install.md gives.
var signingSecrets = []string{
	"MACOS_SIGNING_CERT_P12_BASE64",
	"MACOS_SIGNING_CERT_PASSWORD",
	"MACOS_SIGNING_IDENTITY",
	"APPLE_NOTARY_KEY_P8_BASE64",
	"APPLE_NOTARY_KEY_ID",
	"APPLE_NOTARY_ISSUER_ID",
}

// releaseJobs splits release.yml into its top-level jobs, by name, with
// comment lines dropped so that only what runs is checked.
func releaseJobs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "\njobs:\n")
	if !ok {
		t.Fatal("release.yml has no jobs")
	}
	jobs := map[string]string{}
	header := regexp.MustCompile(`^  ([a-z0-9-]+):$`)
	name := ""
	for _, line := range strings.Split(body, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			jobs[name] += line + "\n"
		}
	}
	return jobs
}

func job(t *testing.T, jobs map[string]string, name string) string {
	t.Helper()
	j, ok := jobs[name]
	if !ok {
		t.Fatalf("release.yml has no %s job", name)
	}
	return j
}

// needs returns a job's needs list.
func needs(t *testing.T, j string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^    needs: (.+)$`).FindStringSubmatch(j)
	if m == nil {
		return nil
	}
	return strings.Split(strings.Trim(m[1], "[]"), ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestReleaseSignsDarwinBeforeChecksumsAndAttestation: build, then
// sign-macos, then publish, which alone computes checksums.txt, attests and
// renders the formula; so all three cover the signed archives.
func TestReleaseSignsDarwinBeforeChecksumsAndAttestation(t *testing.T) {
	jobs := releaseJobs(t)
	sign := job(t, jobs, "sign-macos")
	publish := job(t, jobs, "publish")

	if !contains(needs(t, sign), "build") {
		t.Errorf("sign-macos needs %v, want build", needs(t, sign))
	}
	if !contains(needs(t, publish), "sign-macos") {
		t.Errorf("publish needs %v, want sign-macos", needs(t, publish))
	}
	if !strings.Contains(sign, "runs-on: macos-latest") {
		t.Error("sign-macos does not run on macos-latest")
	}
	for _, want := range []string{
		"codesign --force --options runtime --timestamp",
		"xcrun notarytool submit",
		"--wait",
		"--key-id",
		"--issuer",
		"security create-keychain",
		"security delete-keychain",
		"overwrite: true",
		"name: cloudburrow-darwin-arm64",
		"name: cloudburrow-darwin-amd64",
	} {
		if !strings.Contains(sign, want) {
			t.Errorf("sign-macos does not contain %q", want)
		}
	}
	for _, s := range signingSecrets {
		if !strings.Contains(sign, "secrets."+s+" }}") {
			t.Errorf("sign-macos does not read secrets.%s", s)
		}
	}
	// The cleanup step runs whatever happened before it.
	if !regexp.MustCompile(`name: Delete the temporary keychain[^\n]*\n\s+if: always\(\)`).MatchString(sign) {
		t.Error("the keychain cleanup step is not if: always()")
	}

	// Checksums, attestation and formula are publish's alone: none may be
	// computed before signing.
	for name, j := range jobs {
		for _, marker := range []string{"checksums.txt", "attest-build-provenance", "render-formula.sh"} {
			if name == "publish" || !strings.Contains(j, marker) {
				continue
			}
			if marker == "attest-build-provenance" && name == "runtime-image-publish" {
				continue // the container image, not the archives
			}
			t.Errorf("job %s uses %s; only publish may, after signing", name, marker)
		}
	}
	for _, want := range []string{"checksums.txt", "attest-build-provenance", "render-formula.sh"} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish does not contain %q", want)
		}
	}
}

// TestReleaseActionsArePinnedBySHA: every action, including the signing
// job's, is pinned to a full commit SHA.
func TestReleaseActionsArePinnedBySHA(t *testing.T) {
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\S+$`)
	for name, j := range releaseJobs(t) {
		for _, line := range strings.Split(j, "\n") {
			_, ref, ok := strings.Cut(line, "uses: ")
			if ok && !pinned.MatchString(ref) {
				t.Errorf("job %s: %q is not pinned by commit SHA", name, strings.TrimSpace(line))
			}
		}
	}
}

// secretsCheck extracts the sign-macos step that decides whether to sign.
func secretsCheck(t *testing.T) string {
	t.Helper()
	sign := job(t, releaseJobs(t), "sign-macos")
	_, step, ok := strings.Cut(sign, "        id: secrets\n")
	if !ok {
		t.Fatal("sign-macos has no step with id: secrets")
	}
	_, script, ok := strings.Cut(step, "        run: |\n")
	if !ok {
		t.Fatal("the secrets step has no run block")
	}
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		b.WriteString(strings.TrimPrefix(line, "          ") + "\n")
	}
	return b.String()
}

// runSecretsCheck runs the step as Actions would on macOS (bash -e), with
// the given secrets set, returning its output, GITHUB_OUTPUT and error.
func runSecretsCheck(t *testing.T, set map[string]string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	out := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command("bash", "-e", "-c", secretsCheck(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + out}
	for k, v := range set {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, err := cmd.CombinedOutput()
	o, _ := os.ReadFile(out)
	return string(stdout), string(o), err
}

// TestSigningSkipsWithoutSecrets: a fork, or this repository before the
// maintainer adds the secrets, releases unsigned with a notice.
func TestSigningSkipsWithoutSecrets(t *testing.T) {
	stdout, output, err := runSecretsCheck(t, map[string]string{"MACOS_SIGNING_IDENTITY": ""})
	if err != nil {
		t.Fatalf("the check failed without secrets: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "::notice::") {
		t.Errorf("no ::notice:: when skipping:\n%s", stdout)
	}
	if output != "present=false\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want present=false", output)
	}

	// Every later step is gated on the check, except the cleanup.
	sign := job(t, releaseJobs(t), "sign-macos")
	steps := strings.Split(sign, "\n      - ")[1:]
	for _, s := range steps {
		if strings.Contains(s, "id: secrets") || strings.Contains(s, "if: always()") {
			continue
		}
		if !strings.Contains(s, "if: steps.secrets.outputs.present == 'true'") {
			t.Errorf("a sign-macos step runs without the secrets:\n%s", s)
		}
	}
}

// TestSigningWithAllSecrets: all six set means sign.
func TestSigningWithAllSecrets(t *testing.T) {
	set := map[string]string{}
	for _, s := range signingSecrets {
		set[s] = "x"
	}
	stdout, output, err := runSecretsCheck(t, set)
	if err != nil {
		t.Fatalf("the check failed with every secret: %v\n%s", err, stdout)
	}
	if output != "present=true\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want present=true", output)
	}
}

// TestSigningRefusesPartialSecrets: some but not all is a misconfiguration
// that fails the release rather than quietly shipping unsigned binaries.
func TestSigningRefusesPartialSecrets(t *testing.T) {
	stdout, output, err := runSecretsCheck(t, map[string]string{
		"MACOS_SIGNING_CERT_P12_BASE64": "x",
		"MACOS_SIGNING_IDENTITY":        "x",
	})
	if err == nil {
		t.Fatalf("a partial set of secrets was accepted:\n%s", stdout)
	}
	if !strings.Contains(stdout, "::error::") || !strings.Contains(stdout, "APPLE_NOTARY_KEY_ID") {
		t.Errorf("the error does not name what is missing:\n%s", stdout)
	}
	if output != "" {
		t.Errorf("GITHUB_OUTPUT = %q, want nothing", output)
	}
}
