package beamrun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Google-provided templates (#2215): Google's classic templates
// (templates/provided-templates), built from Google's open-source
// DataflowTemplates repository (Apache-2.0) at a pinned release, with
// Maven, and staged in the instance's own Cloud Storage the way Google's
// templates-maven-plugin stages them (TemplatesStageMojo): each
// template's main class run with the DataflowRunner and
// --templateLocation, and its generated metadata beside it as
// TEMPLATE_metadata. Nothing is read from Google's Cloud Storage or
// registries: the sources come from the repository's GitHub archive, the
// libraries from Maven Central.
const (
	// TemplatesRelease is the DataflowTemplates release built: Google's
	// own release tag, whose Beam version is BeamVersion.
	TemplatesRelease = "2026-11-18-00_RC00"
	templatesCommit  = "d5b2d345d5d56bb9b70f0222bcced9a0855f6b1a"
	templatesSHA256  = "95d044ad9270506443c159acc61b8578f5bd0bfb3b8be423ac7fd430d4d52d04"
	templatesURL     = "https://github.com/GoogleCloudPlatform/DataflowTemplates/archive/" + templatesCommit + ".tar.gz"
	// MavenImage builds the templates: Maven 3.9.9 on Eclipse Temurin 17,
	// the JDK DataflowTemplates' pom targets.
	MavenImage = "maven:3.9.9-eclipse-temurin-17@sha256:f58d59b6273e785ac0a4477f6e9b5ba1d7731c75b906c0f7b34076f1851318cc"
	// providedJava is the Java SDK harness the templates run in.
	providedJava = "17"
	// buildTimeout bounds the Maven build: the first one resolves the v1
	// module's libraries from Maven Central.
	buildTimeout = 45 * time.Minute
)

// relaySpannerHost is a Java SpannerIO's spannerHost on a job's network:
// the relay's Spanner port, over TLS (jobNetwork).
var relaySpannerHost = fmt.Sprintf("https://%s:%d", relayName, spannerPort)

// StageVersion names the launch options Stage fixes in a staged template;
// it changes whenever they do, so templates staged before are staged again
// (2: --pubsubRootUrl; 3: --spannerHost; 4: spannerHost over TLS).
const StageVersion = "4"

// ProvidedNames are the Google-provided classic templates CloudBurrow
// builds, by name (their @Template name in DataflowTemplates' v1 module).
var ProvidedNames = []string{"Bulk_Compress_GCS_Files", "Bulk_Decompress_GCS_Files", "Cloud_BigQuery_to_GCS_TensorFlow_Records",
	"Cloud_PubSub_to_GCS_Text", "Cloud_Spanner_to_GCS_Avro", "GCS_Avro_to_Cloud_Spanner", "GCS_Text_to_BigQuery", "GCS_Text_to_Cloud_PubSub",
	"GCS_Text_to_Cloud_Spanner", "Jdbc_to_BigQuery", "PubSub_Subscription_to_BigQuery", "PubSub_to_BigQuery", "Spanner_to_GCS_Text", "Word_Count"}

// ProvidedFlexNames are the Google-provided Flex Templates CloudBurrow
// builds (their @Template name), each with the DataflowTemplates module
// it is in. Google ships each as a launcher image (built with Jib, from
// gcr.io/dataflow-templates-base's Java launcher) whose spec is
// gs://dataflow-templates-REGION/VERSION/flex/NAME; here the module's
// build is the template's classpath, and the launcher runs its main class
// in the Java SDK image (LaunchProvidedFlex).
var ProvidedFlexNames = map[string]string{
	"BigQuery_to_Parquet":            "v2/googlecloud-to-googlecloud",
	"Cloud_PubSub_to_GCS_Text_Flex":  "v2/googlecloud-to-googlecloud",
	"Cloud_Spanner_to_BigQuery_Flex": "v2/googlecloud-to-googlecloud",
	"File_Format_Conversion":         "v2/googlecloud-to-googlecloud",
	"Jdbc_to_BigQuery_Flex":          "v2/jdbc-and-googlecloud",
	"Jdbc_to_PubSub":                 "v2/jdbc-and-googlecloud",
	"Pubsub_to_Jdbc":                 "v2/jdbc-and-googlecloud",
	"GCS_Text_to_BigQuery_Flex":      "v2/googlecloud-to-googlecloud",
	"PubSub_to_BigQuery_Flex":        "v2/googlecloud-to-googlecloud",
}

// providedModule is the DataflowTemplates module name is built in: v1 for
// the classic templates, a v2 module for a Flex Template.
func providedModule(name string) string {
	if m, ok := ProvidedFlexNames[name]; ok {
		return m
	}
	return "v1"
}

// moduleNames are the templates built in module, sorted.
func moduleNames(module string) []string {
	if module == "v1" {
		return ProvidedNames
	}
	var out []string
	for n, m := range ProvidedFlexNames {
		if m == module {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ProvidedFlexImage is the launcher image a provided Flex Template's spec
// names, which is never pulled: it stands for the template's local build.
// Google's templates share one image per container (flexContainerName,
// gcr.io/dataflow-templates/CONTAINER), and its launcher picks the
// template by the command spec in it; here the template's own name
// follows the container's, so the image names the template:
// gcr.io/dataflow-templates/CONTAINER/TEMPLATE-NAME:VERSION.
func ProvidedFlexImage(t ProvidedTemplate) string {
	return "gcr.io/dataflow-templates/" + t.Container + "/" + templateDash(t.Name) + ":" + strings.ToLower(TemplatesRelease)
}

// ProvidedFlexTemplate answers whether image is a provided Flex
// Template's (ProvidedFlexImage), and which, by name.
func ProvidedFlexTemplate(image string) (name string, ok bool) {
	rest, ok := strings.CutPrefix(image, "gcr.io/dataflow-templates/")
	if !ok {
		return "", false
	}
	path, version, ok := strings.Cut(rest, ":")
	if !ok || version != strings.ToLower(TemplatesRelease) {
		return "", false
	}
	_, dash, ok := strings.Cut(path, "/")
	if !ok {
		return "", false
	}
	for n := range ProvidedFlexNames {
		if templateDash(n) == dash {
			return n, true
		}
	}
	return "", false
}

// templateDash is a template's name as Google's spec files spell it:
// lowercase, with dashes.
func templateDash(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

// ProvidedTemplate is one built template: its generated metadata (the
// TEMPLATE_metadata file, as Google's plugin writes it) and what its
// staging runs.
type ProvidedTemplate struct {
	Name string
	// Module is the DataflowTemplates module it is built in; Flex whether
	// it is a Flex Template (a v2 module's), and Container its launcher
	// image's name (the @Template's flexContainerName).
	Module    string
	Flex      bool
	Container string
	// Metadata is the template's metadata file's content.
	Metadata []byte
	// MainClass is the pipeline's main class; RuntimeParameters the
	// options the staging run is given (a template's variant, such as
	// useSubscription); Streaming whether it is a streaming pipeline.
	MainClass         string
	RuntimeParameters map[string]string
	Streaming         bool
}

// Provided builds and stages the Google-provided templates. Cache is the
// directory builds are kept in, across instances.
type Provided struct {
	D     *Docker
	Cache string

	// mu guards modules; each module's own lock is held while it builds,
	// so one module's build does not hold up another's templates.
	mu      sync.Mutex
	modules map[string]*providedModuleBuild
}

// providedModuleBuild is one module's build, once read.
type providedModuleBuild struct {
	mu    sync.Mutex
	built map[string]ProvidedTemplate
}

// Template answers name's build, building the templates first when they
// are not in the cache. A name not built is ok false.
func (p *Provided) Template(ctx context.Context, name string) (ProvidedTemplate, bool, error) {
	module := providedModule(name)
	p.mu.Lock()
	if p.modules == nil {
		p.modules = map[string]*providedModuleBuild{}
	}
	m := p.modules[module]
	if m == nil {
		m = &providedModuleBuild{}
		p.modules[module] = m
	}
	p.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.built == nil {
		dir, err := p.build(ctx, module)
		if err != nil {
			return ProvidedTemplate{}, false, err
		}
		built, err := readProvided(dir, module)
		if err != nil {
			return ProvidedTemplate{}, false, err
		}
		m.built = built
	}
	t, ok := m.built[name]
	return t, ok, nil
}

// FlexTemplate answers the provided Flex Template name and its module's
// build directory, building it first when it is not in the cache.
func (p *Provided) FlexTemplate(ctx context.Context, name string) (ProvidedTemplate, string, bool, error) {
	if _, ok := ProvidedFlexNames[name]; !ok {
		return ProvidedTemplate{}, "", false, nil
	}
	t, ok, err := p.Template(ctx, name)
	if err != nil || !ok {
		return ProvidedTemplate{}, "", false, err
	}
	return t, p.Dir(t.Module), true, nil
}

// Dir is module's build's directory: classes/, deps/ and metadata/.
func (p *Provided) Dir(module string) string {
	if module == "v1" {
		return filepath.Join(p.Cache, "dataflow-templates", TemplatesRelease)
	}
	return filepath.Join(p.Cache, "dataflow-templates", TemplatesRelease+"-"+strings.ReplaceAll(module, "/", "-"))
}

// build builds DataflowTemplates' module (v1, or a v2 module) into Dir,
// unless a build is there: the release's sources (checked against
// templatesSHA256), Maven's install of the module, Google's
// templates-maven-plugin and the modules they need, its runtime
// libraries, and the templates' metadata (the templates-maven-plugin's
// spec goal).
func (p *Provided) build(ctx context.Context, module string) (string, error) {
	dir := p.Dir(module)
	if _, err := os.Stat(filepath.Join(dir, "ok")); err == nil {
		return dir, nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "", errors.New("docker is not on PATH: Google-provided templates are built with Maven in Docker")
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(filepath.Dir(dir), "build-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(work) }()
	src := filepath.Join(work, "src")
	if err := fetchSources(ctx, src); err != nil {
		return "", err
	}
	m2 := filepath.Join(filepath.Dir(dir), "m2")
	if err := os.MkdirAll(m2, 0o777); err != nil {
		return "", err
	}
	out := filepath.Join(work, "out")
	if err := os.MkdirAll(out, 0o777); err != nil {
		return "", err
	}
	skip := " -DskipTests -Dmaven.test.skip=true -Djib.skip=true -Dspotless.skip=true -Dspotless.check.skip=true" +
		" -Dcheckstyle.skip=true -Djacoco.skip=true -Dmdep.analyze.skip=true"
	script := "set -e; cd /src" +
		"; mvn -B -q -Dmaven.repo.local=/m2 -pl " + module + ",plugins/templates-maven-plugin -am" + skip + " install" +
		"; mvn -B -q -Dmaven.repo.local=/m2 -pl " + module + skip + " dependency:copy-dependencies -DincludeScope=runtime -DoutputDirectory=/out/deps" +
		"; mvn -B -q -Dmaven.repo.local=/m2 -pl " + module + skip + " com.google.cloud.teleport:templates-maven-plugin:1.0-SNAPSHOT:spec" +
		"; mkdir -p /out/classes /out/metadata; cp -r " + module + "/target/classes/. /out/classes/" +
		"; find " + module + "/target -maxdepth 2 -name '*-generated-metadata.json' -exec cp {} /out/metadata/ \\;" +
		"; chmod -R a+rwX /out /m2"
	name := "cb-df-templates-build-" + fmt.Sprint(time.Now().UnixNano()%1e9)
	args := []string{"run", "--rm", "--name", name, "--label", InstanceLabel + "=" + p.D.Instance,
		"-v", src + ":/src", "-v", m2 + ":/m2", "-v", out + ":/out", "--entrypoint", "sh", MavenImage, "-c", script}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = p.D.docker(c, "rm", "-f", "-v", name)
	}()
	p.D.logf("dataflow: building Google-provided templates (DataflowTemplates %s %s) with Maven", TemplatesRelease, module)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build DataflowTemplates %s %s: %v: %s", TemplatesRelease, module, err, lastLines(string(b), 5))
	}
	if err := os.WriteFile(filepath.Join(out, "ok"), []byte(templatesCommit+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(out, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// fetchSources writes the release's sources into dir, from the
// repository's GitHub archive, checked against templatesSHA256.
func fetchSources(ctx context.Context, dir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, templatesURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download DataflowTemplates %s: %w", TemplatesRelease, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download DataflowTemplates %s: %s", TemplatesRelease, resp.Status)
	}
	f, err := os.CreateTemp(filepath.Dir(dir), "src-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != templatesSHA256 {
		return fmt.Errorf("DataflowTemplates %s's archive has SHA-256 %s, not %s", TemplatesRelease, got, templatesSHA256)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return untar(f, dir)
}

// untar writes a gzipped tar's files into dir, without its one top
// directory (GitHub's REPO-COMMIT/).
func untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, _ := strings.Cut(hd.Name, "/")
		if rel == "" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(p, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("the archive's %s is outside it", hd.Name)
		}
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hd.Mode&0o755|0o644))
			if err != nil {
				return err
			}
			_, err = io.Copy(w, tr)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

// readProvided reads module's built templates' metadata, keeping the
// names it builds (moduleNames). A Flex Template's metadata names its
// launcher image's container (flexContainerName, in its spec file
// NAME-spec-generated-metadata.json's image, or else its metadata's).
func readProvided(dir, module string) (map[string]ProvidedTemplate, error) {
	files, err := filepath.Glob(filepath.Join(dir, "metadata", "*-generated-metadata.json"))
	if err != nil {
		return nil, err
	}
	names := moduleNames(module)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string]ProvidedTemplate{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var m struct {
			Name              string            `json:"name"`
			InternalName      string            `json:"internalName"`
			MainClass         string            `json:"mainClass"`
			RuntimeParameters map[string]string `json:"runtimeParameters"`
			Streaming         bool              `json:"streaming"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if !want[m.InternalName] {
			continue
		}
		t := ProvidedTemplate{Name: m.InternalName, Module: module, Flex: module != "v1", Metadata: b, MainClass: m.MainClass,
			RuntimeParameters: m.RuntimeParameters, Streaming: m.Streaming}
		if t.Flex {
			t.Container = flexContainer(f, m.InternalName)
		}
		out[m.InternalName] = t
	}
	var missing []string
	for _, n := range names {
		if _, ok := out[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("the DataflowTemplates %s %s build has no metadata for %s", TemplatesRelease, module, strings.Join(missing, ", "))
	}
	return out, nil
}

// flexContainer is a Flex Template's launcher image's name: the last path
// element of the image its spec file (beside metadata, NAME-spec-generated-
// metadata.json) names, without its tag, or else its name as templateDash
// spells it.
func flexContainer(metadata, name string) string {
	spec := strings.TrimSuffix(metadata, "-generated-metadata.json") + "-spec-generated-metadata.json"
	if b, err := os.ReadFile(spec); err == nil {
		var s struct {
			Image string `json:"image"`
		}
		if json.Unmarshal(b, &s) == nil && s.Image != "" {
			img := s.Image[strings.LastIndex(s.Image, "/")+1:]
			if i := strings.Index(img, ":"); i >= 0 {
				img = img[:i]
			}
			if img != "" {
				return img
			}
		}
	}
	return templateDash(name)
}

// Stage stages t at templatePath (gs://BUCKET/latest/NAME), its libraries
// under stagingPath, as project: the template's main class, run with the
// DataflowRunner on a network whose one route is the instance, writes the
// template file there through the instance's Dataflow and Cloud Storage.
// The metadata file is the caller's to write.
func (p *Provided) Stage(ctx context.Context, t ProvidedTemplate, project, templatePath, stagingPath string) error {
	d := p.D
	live := map[string]string{}
	if d.Live != nil {
		live = d.Live()
	}
	if live["storage"] == "" || live["dataflow"] == "" {
		return errors.New("a template is staged in Cloud Storage, which this instance does not serve")
	}
	tmp, err := os.MkdirTemp(d.Tmp, "cb-dataflow-stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := writeAssets(tmp); err != nil {
		return err
	}
	suffix := nameSafe.ReplaceAllString(strings.ToLower(fmt.Sprintf("%s-%d", t.Name, time.Now().UnixNano()%1e9)), "-")
	if len(suffix) > 30 {
		suffix = suffix[len(suffix)-30:]
	}
	suffix = strings.Trim(suffix, "-")
	network, relay, run := "cb-df-net-tpl-"+suffix, "cb-df-relay-tpl-"+suffix, "cb-df-tpl-"+suffix
	label := InstanceLabel + "=" + d.Instance
	relayIP, stop, err := d.jobNetwork(ctx, network, relay, tmp, PythonImages[LauncherImage], label, live)
	defer stop()
	if err != nil {
		return err
	}
	labels := fmt.Sprintf(`{"goog-dataflow-provided-template-name":"%s","goog-dataflow-provided-template-version":"%s","goog-dataflow-provided-template-type":"legacy"}`,
		strings.ToLower(t.Name), strings.ToLower(TemplatesRelease))
	opts := []string{"--runner=DataflowRunner", "--project=" + project, "--region=us-central1",
		"--stagingLocation=" + stagingPath, "--templateLocation=" + templatePath,
		"--gcpTempLocation=" + stagingPath + "temp/", "--tempLocation=" + stagingPath + "temp/",
		"--apiRootUrl=http://" + relayName + "/",
		fmt.Sprintf("--gcsEndpoint=http://%s:%d/storage/v1/", relayName, storagePort),
		// A batch PubsubIO write publishes through Pub/Sub's REST API
		// (PubsubJsonClient), which the instance's Pub/Sub serves too.
		fmt.Sprintf("--pubsubRootUrl=http://%s:%d", relayName, pubsubPort),
		"--credentialFactoryClass=CbCredentialFactory", "--labels=" + labels}
	if t.Streaming {
		opts = append(opts, "--autoscalingAlgorithm=THROUGHPUT_BASED", "--maxNumWorkers=5")
	}
	// A Spanner template's SpannerIO is given its spannerHost parameter
	// (default https://batch-spanner.googleapis.com), and so ignores
	// SPANNER_EMULATOR_HOST: the template is staged with the instance's
	// Spanner, through the relay's TLS on its Spanner port, as its value.
	if bytes.Contains(t.Metadata, []byte(`"spannerHost"`)) {
		opts = append(opts, "--spannerHost="+relaySpannerHost)
	}
	keys := make([]string, 0, len(t.RuntimeParameters))
	for k := range t.RuntimeParameters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		opts = append(opts, "--"+k+"="+t.RuntimeParameters[k])
	}
	script := "set -e; mkdir -p /tmp/cbcred; CP='/t/classes:/t/deps/*'" +
		"; javac -nowarn -d /tmp/cbcred -cp \"$CP\" /cb/java/CbCredentialFactory.java" +
		"; exec java -cp \"/tmp/cbcred:$CP\" \"$@\""
	args := append([]string{"run", "--rm", "--name", run, "--label", label, "--network", network,
		"-v", tmp + ":/cb:ro", "-v", p.Dir("v1") + ":/t:ro"}, d.clientEnv(Job{Project: project}, tmp, relayIP)...)
	args = append(args, "--entrypoint", "sh", JavaImages[providedJava], "-c", script, "stage", t.MainClass)
	args = append(args, opts...)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, "rm", "-f", "-v", run)
	}()
	d.logf("dataflow: staging the Google-provided template %s at %s", t.Name, templatePath)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("stage %s: %v: %s", t.Name, err, lastLines(string(b), 5))
	}
	return nil
}
