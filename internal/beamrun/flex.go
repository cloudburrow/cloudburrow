package beamrun

// Flex Templates (#2215). Google's launcher runs the template's image, the
// image a `gcloud dataflow flex-template build` names, and the program in
// it submits the job: for Python, the file FLEX_TEMPLATE_PYTHON_PY_FILE
// names is run with the launch's parameters as its arguments and the
// DataflowRunner's options (docs.cloud.google.com/dataflow/docs/guides/
// templates/configuring-flex-templates, re-read 2026-11-24: "Flex
// Templates ... environment variables": FLEX_TEMPLATE_PYTHON_PY_FILE,
// FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE, FLEX_TEMPLATE_PYTHON_SETUP_FILE,
// FLEX_TEMPLATE_PYTHON_EXTRA_PACKAGES and FLEX_TEMPLATE_JAVA_MAIN_CLASS).
// The image is read from the instance's own Artifact Registry, never from
// Google's, loaded into the local Docker, and run on a job network whose
// one route is the instance's APIs, as a job's pipeline is.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
)

// Flex is one Flex Template launch.
type Flex struct {
	Project, Location, JobName string
	// Image is the template's container image, an Artifact Registry path
	// ({location}-docker.pkg.dev/{project}/{repository}/{image}).
	Image string
	// Args are the template's parameters, as --NAME=VALUE.
	Args []string
	// TempLocation, StagingLocation, MaxWorkers, Update and
	// TransformNameMapping (JSON) are the launch's environment, given to
	// the program as its SDK's runner options when set.
	TempLocation, StagingLocation string
	MaxWorkers                    int32
	Update                        bool
	TransformNameMapping          string
}

// flexTimeout bounds a launcher's run: its program submits the job and,
// for a template that waits, until it ends.
const flexTimeout = 30 * time.Minute

// pythonOptions are f's runner options in the Python SDK's spelling.
func (f Flex) pythonOptions() []string {
	o := []string{"--job_name=" + f.JobName, "--project=" + f.Project, "--region=" + f.Location}
	if f.TempLocation != "" {
		o = append(o, "--temp_location="+f.TempLocation)
	}
	if f.StagingLocation != "" {
		o = append(o, "--staging_location="+f.StagingLocation)
	}
	if f.MaxWorkers > 0 {
		o = append(o, fmt.Sprintf("--max_num_workers=%d", f.MaxWorkers))
	}
	if f.Update {
		o = append(o, "--update")
	}
	if f.TransformNameMapping != "" {
		o = append(o, "--transform_name_mapping="+f.TransformNameMapping)
	}
	return o
}

// javaOptions are f's runner options in the Java SDK's spelling.
func (f Flex) javaOptions() []string {
	o := []string{"--jobName=" + f.JobName, "--project=" + f.Project, "--region=" + f.Location}
	if f.TempLocation != "" {
		o = append(o, "--tempLocation="+f.TempLocation, "--gcpTempLocation="+f.TempLocation)
	}
	if f.StagingLocation != "" {
		o = append(o, "--stagingLocation="+f.StagingLocation)
	}
	if f.MaxWorkers > 0 {
		o = append(o, fmt.Sprintf("--maxNumWorkers=%d", f.MaxWorkers))
	}
	if f.Update {
		o = append(o, "--update")
	}
	if f.TransformNameMapping != "" {
		o = append(o, "--transformNameMapping="+f.TransformNameMapping)
	}
	return o
}

// pythonPackageOptions are the Python program's options for the
// packages the image's FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE,
// _SETUP_FILE and _EXTRA_PACKAGES name, which the SDK stages for the
// job's workers (--requirements_file, --setup_file, --extra_package).
func pythonPackageOptions(env map[string]string) []string {
	var o []string
	if f := env["FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE"]; f != "" {
		o = append(o, "--requirements_file="+f)
	}
	if f := env["FLEX_TEMPLATE_PYTHON_SETUP_FILE"]; f != "" {
		o = append(o, "--setup_file="+f)
	}
	for _, p := range strings.Split(env["FLEX_TEMPLATE_PYTHON_EXTRA_PACKAGES"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			o = append(o, "--extra_package="+p)
		}
	}
	return o
}

// pkgDevIndex matches an Artifact Registry Python repository's index URL,
// https://{location}-python.pkg.dev/{project}/{repository}/simple/.
var pkgDevIndex = regexp.MustCompile(`^https?://([a-z0-9-]+-python\.pkg\.dev)(/[^\s]*)$`)

// pipIndexEnv is the launcher's pip configuration: each index URL the
// image's PIP_INDEX_URL or PIP_EXTRA_INDEX_URL names that is an Artifact
// Registry Python repository, read from the instance's own registry
// through the relay (registryPort, the repository's host first in the
// path, as the registry answers a client that reaches it by address).
// Any other index is left as it is, and has no route: the launcher, like
// a Dataflow launcher VM without internet access, reaches only the
// instance. ok is false when the image names no index at all.
func pipIndexEnv(env map[string]string) (out []string, ok bool) {
	for _, k := range []string{"PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL"} {
		v := env[k]
		if v == "" {
			continue
		}
		ok = true
		var urls []string
		for _, u := range strings.Fields(v) {
			if m := pkgDevIndex.FindStringSubmatch(u); m != nil {
				u = fmt.Sprintf("http://%s:%d/%s%s", relayName, registryPort, m[1], m[2])
			}
			urls = append(urls, u)
		}
		out = append(out, "-e", k+"="+strings.Join(urls, " "))
	}
	if ok {
		out = append(out, "-e", "PIP_TRUSTED_HOST="+relayName)
	}
	return out, ok
}

// LaunchFlex runs f's launcher: its program submits the job through the
// instance's Dataflow API. An error says why it did not.
//
// A Python template's program is the file FLEX_TEMPLATE_PYTHON_PY_FILE
// names. The packages FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE lists are
// installed in the launcher first, and the requirements, setup file and
// extra packages are given to the program as its SDK's options, which
// stage them for the job's workers, as Google's Python launcher does. The
// launcher has no route to PyPI, so, as for a launcher without internet
// access (Google's guide to Flex Templates in a VPC Service Controls
// perimeter), the packages come from an Artifact Registry Python
// repository of the instance's, named by the image's PIP_INDEX_URL or
// PIP_EXTRA_INDEX_URL (https://{location}-python.pkg.dev/{project}/{repository}/simple/).
//
// A Java template's program is the class FLEX_TEMPLATE_JAVA_MAIN_CLASS
// names, on the image's FLEX_TEMPLATE_JAVA_CLASSPATH, run with its
// DataflowRunner's options for the instance and the instance's credentials
// (CbCredentialFactory, compiled against the template's own classpath).
func (d *Docker) LaunchFlex(ctx context.Context, f Flex) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not on PATH: Flex Template launchers run in Docker")
	}
	live := map[string]string{}
	if d.Live != nil {
		live = maps.Clone(d.Live())
	}
	if live["storage"] == "" || live["dataflow"] == "" {
		return errors.New("a Flex Template's program stages its job in Cloud Storage, which this instance does not serve")
	}
	if d.Registry != nil {
		live["registry"] = d.Registry()
	}
	ctx, cancel := context.WithTimeout(ctx, flexTimeout)
	defer cancel()
	local, env, err := d.registryImage(ctx, f.Image)
	if err != nil {
		return err
	}
	main, py := env["FLEX_TEMPLATE_JAVA_MAIN_CLASS"], env["FLEX_TEMPLATE_PYTHON_PY_FILE"]
	if main == "" && py == "" {
		return fmt.Errorf("the template's image %s sets neither FLEX_TEMPLATE_PYTHON_PY_FILE nor FLEX_TEMPLATE_JAVA_MAIN_CLASS, so it has no program to launch", f.Image)
	}

	dir, err := os.MkdirTemp(d.Tmp, "cb-dataflow-flex-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeAssets(dir); err != nil {
		return err
	}
	suffix := nameSafe.ReplaceAllString(strings.ToLower(fmt.Sprintf("%s-%d", f.JobName, time.Now().UnixNano()%1e9)), "-")
	if len(suffix) > 30 {
		suffix = suffix[len(suffix)-30:]
	}
	suffix = strings.Trim(suffix, "-")
	network, relay, launch := "cb-df-net-flex-"+suffix, "cb-df-relay-flex-"+suffix, "cb-df-flex-"+suffix
	label := InstanceLabel + "=" + d.Instance
	if main != "" {
		if err := d.flexCredentials(ctx, local, env, dir, label, "cb-df-flexcp-"+suffix); err != nil {
			return err
		}
	}
	relayIP, stop, err := d.jobNetwork(ctx, network, relay, dir, PythonImages["3.12"], label, live)
	defer stop()
	if err != nil {
		return err
	}
	cenv := d.clientEnv(Job{Project: f.Project}, dir, relayIP)
	// The image's own PYTHONPATH stays on the path, after the CLI's.
	for i := range cenv {
		if cenv[i] == "PYTHONPATH=/cb/py" && env["PYTHONPATH"] != "" {
			cenv[i] += ":" + env["PYTHONPATH"]
		}
	}
	args := append([]string{"run", "--rm", "--name", launch, "--label", label, "--network", network, "-v", dir + ":/cb:ro"}, cenv...)
	program := main
	if main != "" {
		cp := env["FLEX_TEMPLATE_JAVA_CLASSPATH"]
		if cp == "" {
			cp = "/template/*"
		}
		args = append(args, "--entrypoint", "java", local, "-cp", "/cb/cbcred:"+cp, main)
		args = append(args, f.Args...)
		args = append(args, f.javaOptions()...)
		args = append(args, "--runner=DataflowRunner", "--apiRootUrl=http://"+relayName+"/",
			fmt.Sprintf("--gcsEndpoint=http://%s:%d/storage/v1/", relayName, storagePort),
			"--credentialFactoryClass=CbCredentialFactory")
	} else {
		program = py
		pip, _ := pipIndexEnv(env)
		args = append(args, pip...)
		run := []string{"python", py}
		run = append(run, f.Args...)
		run = append(run, f.pythonOptions()...)
		run = append(run, pythonPackageOptions(env)...)
		run = append(run, "--runner=DataflowRunner", "--dataflow_endpoint=http://"+relayName)
		// The requirements are installed in the launcher before its
		// program runs, so that the program can import them.
		script := `set -e; if [ -n "$FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE" ]; then ` +
			`python -m pip install --disable-pip-version-check --no-cache-dir -q -r "$FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE"; fi; exec "$@"`
		args = append(args, "--entrypoint", "sh", local, "-c", script, "launch")
		args = append(args, run...)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, "rm", "-f", "-v", launch)
	}()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("the template's program %s exited: %v: %s", program, err, lastLines(string(out), 3))
	}
	return nil
}

// flexCredentials compiles CbCredentialFactory into dir/cbcred against a
// Java template's own classpath (Beam's CredentialFactory and Google's
// auth library are on it, as on any Java pipeline's): the classpath's
// directories are copied out of the image local, and javac runs in the
// Java SDK harness's JDK, for Java 11.
func (d *Docker) flexCredentials(ctx context.Context, local string, env map[string]string, dir, label, name string) error {
	cp := env["FLEX_TEMPLATE_JAVA_CLASSPATH"]
	if cp == "" {
		cp = "/template/*"
	}
	if out, err := d.docker(ctx, "create", "--name", name, "--label", label, "--entrypoint", "true", local); err != nil {
		return fmt.Errorf("read the template's classpath: %s", out)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, "rm", "-f", "-v", name)
	}()
	var jcp []string
	for i, e := range strings.Split(cp, ":") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		src, glob := strings.TrimSuffix(e, "/*"), strings.HasSuffix(e, "/*")
		dst := filepath.Join(dir, "cp", fmt.Sprint(i))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if out, err := d.docker(ctx, "cp", "-L", name+":"+src, dst); err != nil {
			return fmt.Errorf("copy the template's classpath entry %s: %s", e, out)
		}
		in := "/cb/cp/" + fmt.Sprint(i)
		if glob {
			in += "/*"
		}
		jcp = append(jcp, in)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cbcred"), 0o777); err != nil {
		return err
	}
	args := []string{"run", "--rm", "--network", "none", "--label", label, "-v", dir + ":/cb", "--entrypoint", "javac",
		JavaImages[providedJava], "-nowarn", "--release", "11", "-d", "/cb/cbcred", "-cp", strings.Join(jcp, ":"),
		"/cb/java/CbCredentialFactory.java"}
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("compile the launcher's credentials against the template's classpath: %v: %s", err, lastLines(string(out), 3))
	}
	return os.RemoveAll(filepath.Join(dir, "cp"))
}

// lastLines are text's last n non-empty lines, joined.
func lastLines(text string, n int) string {
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

// registryImage reads uri from the instance's Artifact Registry and loads
// it into Docker, labelled for the instance; it answers the local
// reference and the image's environment.
func (d *Docker) registryImage(ctx context.Context, uri string) (string, map[string]string, error) {
	host, rest, ok := strings.Cut(uri, "/")
	loc, isAR := strings.CutSuffix(host, "-docker.pkg.dev")
	if !ok || !isAR || loc == "" || strings.Count(rest, "/") < 2 {
		return "", nil, fmt.Errorf("the template's image %s is not an Artifact Registry image ({location}-docker.pkg.dev/{project}/{repository}/{image}): "+
			"CloudBurrow reads a Flex Template's image from the instance's Artifact Registry only", uri)
	}
	addr := ""
	if d.Registry != nil {
		addr = d.Registry()
	}
	if addr == "" {
		return "", nil, errors.New("a Flex Template's image is read from the instance's Artifact Registry, which this instance does not serve (enable Cloud Run)")
	}
	// The registry answers {location}-docker.pkg.localhost on its port,
	// as a push to it names it; the dial goes to its address.
	_, port, _ := net.SplitHostPort(addr)
	ref, err := name.ParseReference(loc+"-docker.pkg.localhost:"+port+"/"+rest, name.Insecure)
	if err != nil {
		return "", nil, fmt.Errorf("the template's image %s: %v", uri, err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(tr),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH})}
	if d.RegistryToken != nil {
		if tok := d.RegistryToken(); tok != "" {
			opts = append(opts, remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok}))
		}
	}
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return "", nil, fmt.Errorf("the template's image %s cannot be read from Artifact Registry: %v", uri, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", nil, fmt.Errorf("the template's image %s: %v", uri, err)
	}
	env := map[string]string{}
	for _, kv := range cf.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	local := "cloudburrow.local/dataflow-flex:" + digest.Hex[:32]
	if _, err := d.docker(ctx, "image", "inspect", local); err != nil {
		tag, err := name.NewTag(local)
		if err != nil {
			return "", nil, err
		}
		if err := d.load(ctx, tag, img); err != nil {
			return "", nil, fmt.Errorf("load the template's image %s into Docker: %v", uri, err)
		}
		d.logf("dataflow: loaded Flex Template image %s as %s", uri, local)
	}
	return local, env, nil
}

// load writes img to `docker load` as tag, labelled for the instance so
// its stop or delete removes it.
func (d *Docker) load(ctx context.Context, tag name.Tag, img v1.Image) error {
	if d.Instance != "" {
		cf, err := img.ConfigFile()
		if err != nil {
			return err
		}
		c := cf.Config
		c.Labels = maps.Clone(c.Labels)
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		c.Labels[dockergc.InstanceLabel] = d.Instance
		if img, err = mutate.Config(img, c); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "docker", "load")
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := tarball.Write(tag, img, pw)
	_ = pw.CloseWithError(werr)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return werr
}
