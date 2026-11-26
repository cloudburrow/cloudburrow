package beamrun

// Google's YAML template (#2255): Yaml_Template, which `gcloud dataflow
// yaml run` launches (gs://dataflow-templates-REGION/latest/flex/
// Yaml_Template). It is DataflowTemplates' python module's
// python/src/main/python/yaml-template/main.py, run by a Python launcher
// image (the templates-maven-plugin's Dockerfile-template-yaml: Google's
// Python launcher base, the template's files in /template, its
// requirements installed with --require-hashes and downloaded to
// /tmp/dataflow-requirements-cache, FLEX_TEMPLATE_PYTHON_PY_FILE=main.py).
//
// When the caller gives no requirements_file, main.py writes one listing
// its DEFAULT_DEPENDENCIES (boto3, botocore, azure-storage-blob,
// azure-core, azure-identity and Google's job_builder_util_transforms
// tarball at https://storage.googleapis.com/dataflow-templates/
// extra-python-packages/...), which the SDK's stager downloads with pip
// for the workers, and which the workers install, with their
// dependencies. A job's network routes only the instance, so here:
//   - the launcher image is built locally (BuildYAMLLauncher), on the
//     Beam Python SDK container the job's workers run (Python 3.12, as
//     the local runner has no other), with the pinned packages
//     yamlPackages lists (hash-checked, as Google's Dockerfile installs
//     them) and the template's own files from the module's build;
//   - job_builder_util_transforms is built from its source in the
//     release (python/src/main/python/job-builder-util-transforms, with
//     poetry-core, as Google's upload-python-package workflow builds it)
//     rather than read from Google's bucket, and staged at the same path
//     of the instance's own gs://dataflow-templates
//     (JobBuilderObject), which storage.googleapis.com resolves to on the
//     job's network (googleHosts);
//   - the packages are seeded into an Artifact Registry Python repository
//     of the instance's (ProvidedPythonIndex), which the launcher's and
//     the workers' pip read as their index, through the job's relay.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
)

// PyPackage is one pinned Python distribution file: a wheel from PyPI,
// checked against its SHA-256, and its licence (its PyPI metadata's
// licence expression or classifier).
type PyPackage struct {
	Name, Version, File, URL, SHA256, License string
}

// yamlPackages are the distributions Yaml_Template's default
// dependencies need beyond what Beam's Python SDK container has: the
// five main.py lists and their own dependencies, at the versions (and
// wheel hashes) DataflowTemplates' yaml-template/requirements.txt pins
// for the release (TemplatesRelease), and poetry-core, the build backend
// job_builder_util_transforms' sdist is built with (pip builds it on the
// launcher and on each worker). Every one is a pure-Python wheel
// (py3-none-any), so one file serves every platform. None is GPL, LGPL or
// AGPL licensed (TestYAMLPackagesLicences).
var yamlPackages = []PyPackage{
	{"boto3", "1.44.3", "boto3-1.44.3-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/9a/b5/2807516d854b60f430917590fcfe470bc60c5383cd6b27dc57aa6dc6afea/boto3-1.44.3-py3-none-any.whl",
		"8c9c4844c7ce184494b102983696d6cdb09f5307ec7f238ca4ad5849fb14e26b", "Apache-2.0"},
	{"botocore", "1.44.3", "botocore-1.44.3-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/20/65/e39cc0e0d16cf9efe0afab0edef48b89491b548e2c7d7ddc23bbb2c6422c/botocore-1.44.3-py3-none-any.whl",
		"793eac2a7ca167e9376b136cf000e1675e9ffe8d7ec609160cc1ee00b93ace28", "Apache-2.0"},
	{"jmespath", "1.1.0", "jmespath-1.1.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/14/2f/967ba146e6d58cf6a652da73885f52fc68001525b4197effc174321d70b4/jmespath-1.1.0-py3-none-any.whl",
		"a5663118de4908c91729bea0acadca56526eb2698e83de10cd116ae0f4e97c64", "MIT"},
	{"s3transfer", "0.19.3", "s3transfer-0.19.3-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/e2/a4/88f9e10498759121060b8050a1554753dfc04df39bbbcc7aad9ecc8d9440/s3transfer-0.19.3-py3-none-any.whl",
		"24eb7535804306355e31c952a90022bf5b2f9d760f9a11062c7493a217c08553", "Apache-2.0"},
	{"azure-storage-blob", "12.31.1", "azure_storage_blob-12.31.1-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/1a/93/b9f0dc477827887aef64012946bdba65e9aad67084ad38044661a72fe47d/azure_storage_blob-12.31.1-py3-none-any.whl",
		"9e012d5f4ab658b7cb8a4581329d7a7aa2bf5f39b4d63baeefaae7f9e90fe6f0", "MIT"},
	{"azure-core", "1.41.0", "azure_core-1.41.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/5b/db/325c6d7312d2200251c52323878281045aaffcb5586612296484e4280eaa/azure_core-1.41.0-py3-none-any.whl",
		"522b4011e8180b1a3dcd2024396a4e7fe9ac37fb8597db47163d230b5efe892d", "MIT"},
	{"azure-identity", "1.27.0", "azure_identity-1.27.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/29/1e/6a2fe486576f41884a120ef7b2c3774f334f9143402fb73650c67db94ddd/azure_identity-1.27.0-py3-none-any.whl",
		"92fd2a93c0d2f777dac6c04e5fd7d5734f6643582b983544ef66b4575844e46d", "MIT"},
	{"isodate", "0.7.2", "isodate-0.7.2-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/15/aa/0aca39a37d3c7eb941ba736ede56d689e7be91cab5d9ca846bde3999eba6/isodate-0.7.2-py3-none-any.whl",
		"28009937d8031054830160fce6d409ed342816b543597cece116d966c6d99e15", "BSD-3-Clause"},
	{"msal", "1.39.0", "msal-1.39.0-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/a4/32/178457385925ce301c7b3232bd85665d4df57da39568b62dcfc74031873f/msal-1.39.0-py3-none-any.whl",
		"2d2577886906cd7293850dffa2da29119966c213bfc6ec0cecf8bf7621e1ca77", "MIT"},
	{"pyjwt", "2.15.1", "pyjwt-2.15.1-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/50/ca/44de4e75f8aadc457f0634be3b542815078ded46dca30efb960edeecad6e/pyjwt-2.15.1-py3-none-any.whl",
		"42d59d631f7768a1028a64c7ff581a9bf7519804daf91fc5b6c56e30eec5e193", "MIT"},
	{"msal-extensions", "1.3.1", "msal_extensions-1.3.1-py3-none-any.whl",
		"https://files.pythonhosted.org/packages/5e/75/bd9b7bb966668920f06b200e84454c8f3566b102183bc55c5473d96cb2b9/msal_extensions-1.3.1-py3-none-any.whl",
		"96d3de4d034504e969ac5e85bae8106c8373b5c6568e4c8fa7af2eca9dbe6bca", "MIT"},
	poetryCore,
}

// poetryCore is job_builder_util_transforms' build backend (its
// pyproject's build-system).
var poetryCore = PyPackage{"poetry-core", "2.5.0", "poetry_core-2.5.0-py3-none-any.whl",
	"https://files.pythonhosted.org/packages/b7/25/7ed0a2ea4fe12f7fd0394387b5debbcd6fdbd06e5ac9daab96ba11471da8/poetry_core-2.5.0-py3-none-any.whl",
	"98ee465b66f7e14bb9fbe68fb5e00d0cff184f036993ec94610ae4b5612f3add", "MIT"}

// YAMLPackages are the pinned packages the instance's Python index is
// seeded with for Yaml_Template.
func YAMLPackages() []PyPackage { return append([]PyPackage(nil), yamlPackages...) }

const (
	// YAMLTemplateName is Google's YAML template's name.
	YAMLTemplateName = "Yaml_Template"
	// JobBuilderObject is where main.py's DEFAULT_DEPENDENCIES read
	// job_builder_util_transforms (https://storage.googleapis.com/ +
	// gs://dataflow-templates/ + this), in the release
	// (python/default_base_yaml_requirements.txt names the same file).
	JobBuilderObject = "extra-python-packages/2026-08-20/job_builder_util_transforms-0.2.2.tar.gz"
	// ProvidedPythonProject, ProvidedPythonLocation and
	// ProvidedPythonRepository name the Artifact Registry Python
	// repository of the instance's that Google-provided templates'
	// packages are seeded into, under Google's templates project, as
	// Google's own images are (gcr.io/dataflow-templates): a user's
	// projects do not list it.
	ProvidedPythonProject    = "dataflow-templates"
	ProvidedPythonLocation   = "us"
	ProvidedPythonRepository = "extra-python-packages"
)

// ProvidedPythonIndex is that repository's index URL, as pip is given it
// (PIP_INDEX_URL) on a launcher and on the workers.
var ProvidedPythonIndex = "https://" + ProvidedPythonLocation + "-python.pkg.dev/" + ProvidedPythonProject + "/" + ProvidedPythonRepository + "/simple/"

// YAMLLauncher is Yaml_Template's local build: its launcher image, the
// packages its index is seeded with, and the job_builder_util_transforms
// sdist staged at JobBuilderObject.
type YAMLLauncher struct {
	// Image is the launcher image's local tag.
	Image string
	// Packages are the seeded files' paths, by PyPackage (yamlPackages'
	// order).
	Packages []string
	// Tarball is the sdist's path.
	Tarball string
}

// yamlDir is the YAML launcher's build directory, beside the python
// module's.
func (p *Provided) yamlDir() string {
	return filepath.Join(p.Cache, "dataflow-templates", TemplatesRelease+"-yaml-launcher")
}

// BuildYAMLLauncher builds Yaml_Template's launcher (YAMLLauncher),
// unless it is built: the python module (Template), the pinned packages
// (fetched once from PyPI, each checked against its SHA-256),
// job_builder_util_transforms from its source, and the launcher image.
func (p *Provided) BuildYAMLLauncher(ctx context.Context) (YAMLLauncher, error) {
	p.yamlMu.Lock()
	defer p.yamlMu.Unlock()
	if p.yamlBuilt != nil {
		if _, err := p.D.docker(ctx, "image", "inspect", p.yamlBuilt.Image); err == nil {
			return *p.yamlBuilt, nil
		}
	}
	t, ok, err := p.Template(ctx, YAMLTemplateName)
	if err != nil {
		return YAMLLauncher{}, err
	}
	if !ok {
		return YAMLLauncher{}, fmt.Errorf("the DataflowTemplates %s build has no %s", TemplatesRelease, YAMLTemplateName)
	}
	dir := p.yamlDir()
	pkgDir := filepath.Join(dir, "packages")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return YAMLLauncher{}, err
	}
	var out YAMLLauncher
	for _, pk := range yamlPackages {
		path := filepath.Join(pkgDir, pk.File)
		if err := fetchPinned(ctx, pk, path); err != nil {
			return YAMLLauncher{}, err
		}
		out.Packages = append(out.Packages, path)
	}
	classes := filepath.Join(p.Dir(t.Module), "classes")
	sdist, wheel, err := p.buildJobBuilder(ctx, classes, dir)
	if err != nil {
		return YAMLLauncher{}, err
	}
	out.Tarball = sdist
	img, err := p.buildYAMLImage(ctx, classes, dir, out.Packages, wheel)
	if err != nil {
		return YAMLLauncher{}, err
	}
	out.Image = img
	p.yamlBuilt = &out
	return out, nil
}

// fetchPinned writes pk's file to path, from its URL, unless a file of
// its SHA-256 is there; a download of another hash is refused.
func fetchPinned(ctx context.Context, pk PyPackage, path string) error {
	if b, err := os.ReadFile(path); err == nil {
		s := sha256.Sum256(b)
		if hex.EncodeToString(s[:]) == pk.SHA256 {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pk.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s %s: %w", pk.Name, pk.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s %s: %s", pk.Name, pk.Version, resp.Status)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".dl-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s %s: %w", pk.Name, pk.Version, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != pk.SHA256 {
		return fmt.Errorf("%s from %s has SHA-256 %s, not the pinned %s", pk.File, pk.URL, got, pk.SHA256)
	}
	return os.Rename(f.Name(), path)
}

// buildJobBuilder builds job_builder_util_transforms' sdist and wheel
// from its source in the module's build (classes/job-builder-util-
// transforms) into dir/job-builder, with poetry-core (its build backend,
// as `poetry build` runs it), in the Python SDK container, offline. Its
// version is its pyproject's; the sdist is staged under main.py's file
// name, as Google's upload-python-package workflow copies its
// dist/job_builder_util_transforms-*.tar.gz to its versioned name.
func (p *Provided) buildJobBuilder(ctx context.Context, classes, dir string) (sdist, wheel string, err error) {
	out := filepath.Join(dir, "job-builder")
	if s, w := globOne(out, "*.tar.gz"), globOne(out, "*.whl"); s != "" && w != "" {
		return s, w, nil
	}
	src := filepath.Join(classes, "job-builder-util-transforms")
	if _, err := os.Stat(filepath.Join(src, "pyproject.toml")); err != nil {
		return "", "", fmt.Errorf("the DataflowTemplates %s python build has no job-builder-util-transforms: %v", TemplatesRelease, err)
	}
	tmp, err := os.MkdirTemp(dir, "job-builder-")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.Chmod(tmp, 0o777); err != nil {
		return "", "", err
	}
	// The release's own date (SOURCE_DATE_EPOCH, 2026-11-18) for the
	// archives' file times.
	script := `set -e; python -m pip install -q --no-index --no-deps --disable-pip-version-check --root-user-action=ignore --target /tmp/pc /pkg/` + poetryCore.File +
		`; cp -r /src /tmp/src; cd /tmp/src; SOURCE_DATE_EPOCH=1794960000 PYTHONPATH=/tmp/pc python -c ` +
		`'from poetry.core.masonry import api; api.build_sdist("/out"); api.build_wheel("/out")'; chmod -R a+rw /out`
	args := []string{"run", "--rm", "--network", "none", "--label", InstanceLabel + "=" + p.D.Instance,
		"-v", src + ":/src:ro", "-v", filepath.Join(dir, "packages") + ":/pkg:ro", "-v", tmp + ":/out",
		"--entrypoint", "sh", PythonImages[LauncherImage], "-c", script}
	p.D.logf("dataflow: building job_builder_util_transforms (DataflowTemplates %s) from its source", TemplatesRelease)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("build job_builder_util_transforms: %v: %s", err, lastLines(string(b), 5))
	}
	if globOne(tmp, "*.tar.gz") == "" || globOne(tmp, "*.whl") == "" {
		return "", "", errors.New("build job_builder_util_transforms: poetry-core wrote no sdist and wheel")
	}
	_ = os.RemoveAll(out)
	if err := os.Rename(tmp, out); err != nil {
		return "", "", err
	}
	return globOne(out, "*.tar.gz"), globOne(out, "*.whl"), nil
}

// globOne is dir's one file matching pattern, or "".
func globOne(dir, pattern string) string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	if len(m) != 1 {
		return ""
	}
	return m[0]
}

// yamlDockerfile is the launcher image's Dockerfile, after Google's
// Dockerfile-template-yaml: the template's files in /template, the
// packages installed with --require-hashes and downloaded to the
// requirements cache Beam's stager reads first
// (/tmp/dataflow-requirements-cache), and main.py the program. Its base
// is the Python SDK container the workers run, which has Apache Beam
// 2.78.0 and its gcp, yaml and dataframe extras' packages; the build has
// no network.
const yamlDockerfile = `FROM %s
COPY template/ /template/
COPY packages/ /tmp/cb-packages/
RUN python -m pip install --no-index --no-deps --require-hashes --disable-pip-version-check --no-cache-dir --root-user-action=ignore -r /tmp/cb-packages/requirements.txt \
 && mkdir -p /tmp/dataflow-requirements-cache && cp /tmp/cb-packages/*.whl /tmp/dataflow-requirements-cache/ \
 && rm -f /tmp/dataflow-requirements-cache/job_builder_util_transforms-*.whl && rm -rf /tmp/cb-packages
ENV FLEX_TEMPLATE_PYTHON_PY_FILE=main.py
WORKDIR /template
`

// buildYAMLImage builds the launcher image (yamlDockerfile) from the
// module's yaml-template files and the packages, and answers its tag,
// which names its inputs' hash.
func (p *Provided) buildYAMLImage(ctx context.Context, classes, dir string, packages []string, jobBuilderWheel string) (string, error) {
	tmp, err := os.MkdirTemp(dir, "image-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	h := sha256.New()
	write := func(rel string, b []byte) error {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %d\n", rel, len(b))
		h.Write(b)
		return os.WriteFile(p, b, 0o644)
	}
	if err := write("Dockerfile", fmt.Appendf(nil, yamlDockerfile, PythonImages[LauncherImage])); err != nil {
		return "", err
	}
	for _, f := range []string{"main.py", "ml_dependencies.py", "requirements.txt"} {
		b, err := os.ReadFile(filepath.Join(classes, "yaml-template", f))
		if err != nil {
			return "", fmt.Errorf("the DataflowTemplates %s python build's yaml-template: %v", TemplatesRelease, err)
		}
		if err := write("template/"+f, b); err != nil {
			return "", err
		}
	}
	// The launcher installs what its program imports: the default
	// dependencies and job_builder_util_transforms (as Google's image
	// does, from default_base_yaml_requirements.txt), not poetry-core.
	var req strings.Builder
	files := append(append([]string(nil), packages...), jobBuilderWheel)
	sort.Strings(files)
	for _, f := range files {
		if filepath.Base(f) == poetryCore.File {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		s := sha256.Sum256(b)
		fmt.Fprintf(&req, "/tmp/cb-packages/%s --hash=sha256:%s\n", filepath.Base(f), hex.EncodeToString(s[:]))
		if err := write("packages/"+filepath.Base(f), b); err != nil {
			return "", err
		}
	}
	if err := write("packages/requirements.txt", []byte(req.String())); err != nil {
		return "", err
	}
	tag := "cloudburrow.local/dataflow-templates/yaml-template:" + hex.EncodeToString(h.Sum(nil))[:24]
	if _, err := p.D.docker(ctx, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	args := []string{"build", "--network", "none", "-q", "-t", tag}
	if p.D.Instance != "" {
		args = append(args, "--label", dockergc.InstanceLabel+"="+p.D.Instance)
	}
	args = append(args, tmp)
	p.D.logf("dataflow: building the launcher image of %s (DataflowTemplates %s)", YAMLTemplateName, TemplatesRelease)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the %s launcher image: %v: %s", YAMLTemplateName, err, lastLines(string(b), 5))
	}
	return tag, nil
}
