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
	// Args are the pipeline's arguments: the launch's parameters and
	// the runner's options.
	Args []string
}

// flexTimeout bounds a launcher's run: its program submits the job and,
// for a template that waits, until it ends.
const flexTimeout = 30 * time.Minute

// LaunchFlex runs f's launcher: its program submits the job through the
// instance's Dataflow API. An error says why it did not.
func (d *Docker) LaunchFlex(ctx context.Context, f Flex) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not on PATH: Flex Template launchers run in Docker")
	}
	live := map[string]string{}
	if d.Live != nil {
		live = d.Live()
	}
	if live["storage"] == "" || live["dataflow"] == "" {
		return errors.New("a Flex Template's program stages its job in Cloud Storage, which this instance does not serve")
	}
	ctx, cancel := context.WithTimeout(ctx, flexTimeout)
	defer cancel()
	local, env, err := d.registryImage(ctx, f.Image)
	if err != nil {
		return err
	}
	if env["FLEX_TEMPLATE_JAVA_MAIN_CLASS"] != "" {
		return fmt.Errorf("the template's image %s is a Java Flex Template (FLEX_TEMPLATE_JAVA_MAIN_CLASS): CloudBurrow's local runner serves the Python SDK's pipelines (Apache Beam %s)", f.Image, BeamVersion)
	}
	py := env["FLEX_TEMPLATE_PYTHON_PY_FILE"]
	if py == "" {
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
	args = append(args, "--entrypoint", "python", local, py)
	args = append(args, f.Args...)
	args = append(args, "--runner=DataflowRunner", "--dataflow_endpoint=http://"+relayName)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, "rm", "-f", "-v", launch)
	}()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("the template's program %s exited: %v: %s", py, err, lastLines(string(out), 3))
	}
	return nil
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
