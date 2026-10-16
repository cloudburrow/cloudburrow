package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// Cloud Build's arbitrary steps (#1382), run as Google's build worker runs
// them: each step a container on the host's Docker, with the Docker socket
// mounted at /var/run/docker.sock, on the user-defined network cloudbuild,
// with the build's /workspace volume as its working directory, a
// /builder/home volume every step shares (HOME), /builder/outputs
// (BUILDER_OUTPUT), and the named volumes its volumes field lists. A step
// can itself run docker, and reach containers it starts on --network
// cloudbuild, as on Google.

// cloudbuildNetwork is the network every step joins. It is shared by the
// builds this process runs and made on the first; the last removes it, if
// this process made it.
const cloudbuildNetwork = "cloudbuild"

var stepNetwork struct {
	mu      sync.Mutex
	users   int
	created bool
}

func (b *localImageBuilder) dockerOut(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func acquireStepNetwork(ctx context.Context, b *localImageBuilder) error {
	stepNetwork.mu.Lock()
	defer stepNetwork.mu.Unlock()
	if stepNetwork.users == 0 {
		if _, err := b.dockerOut(ctx, "network", "inspect", cloudbuildNetwork); err != nil {
			if _, err := b.dockerOut(ctx, "network", "create", "--label", "cloudburrow.instance="+b.cfg.Name, cloudbuildNetwork); err != nil {
				return err
			}
			stepNetwork.created = true
		}
	}
	stepNetwork.users++
	return nil
}

func releaseStepNetwork(b *localImageBuilder) {
	stepNetwork.mu.Lock()
	defer stepNetwork.mu.Unlock()
	if stepNetwork.users--; stepNetwork.users == 0 && stepNetwork.created {
		// Fails harmlessly while a container a step started is still on it.
		if _, err := b.dockerOut(context.Background(), "network", "rm", cloudbuildNetwork); err == nil {
			stepNetwork.created = false
		}
	}
}

// hostWorkspace is one build's volumes on the host's Docker.
type hostWorkspace struct {
	b       *localImageBuilder
	prefix  string // container and volume names
	holder  string // a created, never started, container mounting /workspace
	mu      sync.Mutex
	volumes []string
}

// OpenWorkspace implements runadapter.StepRunner.
func (b *localImageBuilder) OpenWorkspace(ctx context.Context, buildID string, archive io.Reader) (runadapter.BuildWorkspace, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker is not installed")
	}
	short := strings.ReplaceAll(buildID, "-", "")
	if len(short) > 12 {
		short = short[:12]
	}
	w := &hostWorkspace{b: b, prefix: "cloudburrow-build-" + b.cfg.Name + "-" + short}
	if err := acquireStepNetwork(ctx, b); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			w.Close()
		}
	}()
	for _, v := range []string{"workspace", "home", "outputs"} {
		if err := w.volume(ctx, v); err != nil {
			return nil, err
		}
	}
	// The holder mounts /workspace so docker cp can fill and read it. It
	// is the cluster's node image, which is on the host whenever Cloud Run
	// is, and is never started.
	w.holder = w.prefix + "-workspace"
	if _, err := b.dockerOut(ctx, "create", "--name", w.holder, "--label", "cloudburrow.instance="+b.cfg.Name,
		"-v", w.prefix+"-workspace:/workspace", b.cfg.Cluster.NodeImage); err != nil {
		return nil, fmt.Errorf("create the /workspace holder: %w", err)
	}
	if archive != nil {
		dir, err := os.MkdirTemp("", "cloudburrow-build-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		if err := unpackSource(archive, dir); err != nil {
			return nil, fmt.Errorf("unpack the source archive: %w", err)
		}
		if _, err := b.dockerOut(ctx, "cp", dir+"/.", w.holder+":/workspace"); err != nil {
			return nil, fmt.Errorf("copy the source into /workspace: %w", err)
		}
	}
	ok = true
	return w, nil
}

// volume makes the build's volume name, labelled as this instance's.
func (w *hostWorkspace) volume(ctx context.Context, name string) error {
	full := w.prefix + "-" + name
	w.mu.Lock()
	for _, v := range w.volumes {
		if v == full {
			w.mu.Unlock()
			return nil
		}
	}
	w.volumes = append(w.volumes, full)
	w.mu.Unlock()
	_, err := w.b.dockerOut(ctx, "volume", "create", "--label", "cloudburrow.instance="+w.b.cfg.Name, full)
	return err
}

// RunStep implements runadapter.BuildWorkspace.
func (w *hostWorkspace) RunStep(ctx context.Context, spec runadapter.StepSpec) error {
	name := w.prefix + "-step-" + strconv.Itoa(spec.Index)
	if _, err := w.b.dockerOut(ctx, "image", "inspect", "--format", "{{.Id}}", spec.Image); err != nil {
		fmt.Fprintf(spec.Log, "Pulling image: %s\n", spec.Image)
		pull := exec.CommandContext(ctx, "docker", "pull", spec.Image)
		pull.Stdout, pull.Stderr = spec.Log, spec.Log
		if err := pull.Run(); err != nil {
			return fmt.Errorf("failed to pull image %s: %v", spec.Image, err)
		}
	}
	args := []string{"create", "--name", name, "--label", "cloudburrow.instance=" + w.b.cfg.Name,
		"--network", cloudbuildNetwork,
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", w.prefix + "-workspace:/workspace",
		"-v", w.prefix + "-home:/builder/home",
		"-v", w.prefix + "-outputs:/builder/outputs",
		"-w", path.Join("/workspace", spec.Dir),
		"-e", "HOME=/builder/home", "-e", "BUILDER_OUTPUT=/builder/outputs"}
	for _, v := range spec.Volumes {
		if err := w.volume(ctx, "vol-"+v.Name); err != nil {
			return err
		}
		args = append(args, "-v", w.prefix+"-vol-"+v.Name+":"+v.Path)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	if spec.Entrypoint != "" {
		args = append(args, "--entrypoint", spec.Entrypoint)
	}
	args = append(append(args, spec.Image), spec.Args...)
	if _, err := w.b.dockerOut(ctx, args...); err != nil {
		return err
	}
	defer exec.Command("docker", "rm", "-f", "-v", name).Run()
	// docker start -a ends with the container's exit; a cancelled build or
	// timed-out step kills the container, which the CLI alone would not.
	start := exec.Command("docker", "start", "-a", name)
	start.Stdout, start.Stderr = spec.Log, spec.Log
	if err := start.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- start.Wait() }()
	var err error
	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = exec.Command("docker", "kill", name).Run()
		<-waited
		return ctx.Err()
	}
	code, ierr := w.b.dockerOut(context.Background(), "inspect", "--format", "{{.State.ExitCode}}", name)
	if ierr == nil {
		if n, cerr := strconv.Atoi(code); cerr == nil {
			if n != 0 {
				return &runadapter.StepExitError{Code: n}
			}
			return nil
		}
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &runadapter.StepExitError{Code: exit.ExitCode()}
		}
	}
	return err
}

// Archive implements runadapter.BuildWorkspace: /workspace as a gzipped
// tarball rooted at /workspace.
func (w *hostWorkspace) Archive(ctx context.Context) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "docker", "cp", w.holder+":/workspace", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		err := retarWorkspace(out, pw)
		_, _ = io.Copy(io.Discard, out)
		if werr := cmd.Wait(); err == nil && werr != nil {
			err = fmt.Errorf("docker cp /workspace: %v: %s", werr, strings.TrimSpace(stderr.String()))
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// retarWorkspace rewrites docker cp's tar of /workspace, whose entries are
// workspace/..., as a gzipped tarball of its contents.
func retarWorkspace(in io.Reader, out io.Writer) error {
	tr := tar.NewReader(in)
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(h.Name, "workspace/")
		if !ok || rel == "" {
			continue
		}
		h.Name = rel
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// Close implements runadapter.BuildWorkspace.
func (w *hostWorkspace) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if w.holder != "" {
		_, _ = w.b.dockerOut(ctx, "rm", "-f", "-v", w.holder)
	}
	w.mu.Lock()
	vols := append([]string(nil), w.volumes...)
	w.mu.Unlock()
	for _, v := range vols {
		_, _ = w.b.dockerOut(ctx, "volume", "rm", "-f", v)
	}
	releaseStepNetwork(w.b)
}
