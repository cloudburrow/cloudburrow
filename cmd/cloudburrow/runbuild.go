package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// storageSourceFetcher reads SubmitBuild's source archive from this
// instance's Cloud Storage through its JSON API, as any client would.
type storageSourceFetcher struct {
	cfg    config.Config
	client *http.Client
	// addr is the storage tunnel's bound host address, read at each fetch:
	// with an OS-assigned port (--port-storage 0) the configured port is 0
	// and only the tunnel knows the real one.
	addr func() string
}

func (f storageSourceFetcher) base() string {
	if f.addr != nil {
		if a := f.addr(); a != "" {
			return hostURL(a)
		}
	}
	host := f.cfg.BindAddress
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(f.cfg.Endpoints.Storage))
}

func (f storageSourceFetcher) Fetch(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error) {
	u := f.base() + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(object) + "?alt=media"
	if generation != 0 {
		u += "&generation=" + strconv.FormatInt(generation, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, apierror.InvalidArgument("storage_source: %v", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, apierror.FailedPrecondition("read gs://%s/%s from this instance's Cloud Storage: %v", bucket, object, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, apierror.NotFound("storage_source gs://%s/%s not found", bucket, object)
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, apierror.FailedPrecondition("read gs://%s/%s: HTTP %d %s", bucket, object, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// Put writes a Cloud Build log to this instance's Cloud Storage (#1347).
// With create, a missing bucket is made first in project. The build's
// default logs bucket, {number}.cloudbuild-logs.googleusercontent.com,
// which Google owns, is written through the server's Cloud Build path
// (#1430): no user may create that name or write to it.
func (f storageSourceFetcher) Put(ctx context.Context, project, bucket, object string, data []byte, create bool) error {
	put := func() (int, string, error) {
		u := f.base() + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(object)
		if storage.IsCloudBuildLogsBucket(bucket) {
			u = f.base() + storage.CloudBuildLogsPath + "?" +
				url.Values{"project": {project}, "bucket": {bucket}, "name": {object}}.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		resp, err := f.client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, strings.TrimSpace(string(b)), nil
	}
	code, msg, err := put()
	if err == nil && code == http.StatusNotFound && create {
		body := fmt.Sprintf(`{"name":%q}`, bucket)
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, f.base()+"/storage/v1/b?project="+url.QueryEscape(project), strings.NewReader(body))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, rerr := f.client.Do(req); rerr == nil {
			resp.Body.Close()
		}
		code, msg, err = put()
	}
	if err != nil {
		return fmt.Errorf("write gs://%s/%s: %v", bucket, object, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("write gs://%s/%s: HTTP %d %s", bucket, object, code, msg)
	}
	return nil
}

// localImageBuilder builds SubmitBuild's source with the host's Docker, or
// pack for buildpacks, and loads the image into the instance's kind cluster.
type localImageBuilder struct {
	cfg    config.Config
	loader *images.Loader
}

func newLocalImageBuilder(cfg config.Config) *localImageBuilder {
	return &localImageBuilder{cfg: cfg, loader: &images.Loader{ClusterName: cfg.ClusterName(), Runner: images.ExecRunner{}}}
}

func (b *localImageBuilder) Build(ctx context.Context, spec runadapter.BuildSpec) error {
	dir, err := os.MkdirTemp("", "cloudburrow-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := unpackSource(spec.Archive, dir); err != nil {
		return fmt.Errorf("unpack the source archive: %w", err)
	}
	switch spec.Kind {
	case runadapter.BuildDocker:
		if err := b.docker(ctx, spec, dir); err != nil {
			return err
		}
	case runadapter.BuildBuildpacks:
		if err := b.pack(ctx, spec, dir); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown build kind %v", spec.Kind)
	}
	return b.loader.Load(ctx, spec.LocalImage, b.cfg.KubeconfigPath())
}

// docker builds the Dockerfile for the cluster nodes' architecture, so the
// image runs there whatever the host is.
func (b *localImageBuilder) docker(ctx context.Context, spec runadapter.BuildSpec, dir string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed")
	}
	dockerfile, context := filepath.Join(dir, spec.Dockerfile), filepath.Join(dir, spec.Context)
	if _, err := os.Stat(dockerfile); err != nil {
		if spec.Dockerfile == "Dockerfile" {
			return fmt.Errorf("the source has no Dockerfile at its root")
		}
		return fmt.Errorf("the source has no %s", spec.Dockerfile)
	}
	arch, err := b.loader.NodeArchitecture(ctx, b.cfg.KubeconfigPath())
	if err != nil {
		return fmt.Errorf("read the cluster's node architecture: %w", err)
	}
	args := []string{"build", "--platform", "linux/" + arch, "-t", spec.LocalImage, "-f", dockerfile}
	for _, a := range spec.BuildArgs {
		args = append(args, "--build-arg", a)
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", append(args, context)...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if spec.Log != nil {
		cmd.Stdout = io.MultiWriter(&out, spec.Log)
		cmd.Stderr = cmd.Stdout
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %v\n%s", err, tailLines(out.String(), 25))
	}
	return nil
}

// buildRegistryImage is the registry pack publishes to (registry 2.8.3,
// pinned by digest, as the Functions tests use): pack cannot export this
// builder's amd64 image to the Docker daemon on every host
// (buildpacks.Request.Registry).
const buildRegistryImage = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

// pack builds with Google's buildpacks into a throwaway registry on a
// network of its own, then pulls the image back and tags it local.
func (b *localImageBuilder) pack(ctx context.Context, spec runadapter.BuildSpec, dir string) error {
	if ok, note := buildpacks.SupportedHere(); !ok {
		return fmt.Errorf("%s", note)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
	network, name := "cb-build-"+b.cfg.Name+"-"+suffix, "cb-build-registry-"+b.cfg.Name+"-"+suffix
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", network).CombinedOutput(); err != nil {
		return fmt.Errorf("create the build network: %v\n%s", err, out)
	}
	defer exec.Command("docker", "network", "rm", network).Run()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--network", network,
		"-p", fmt.Sprintf("127.0.0.1:%d:5000", port), buildRegistryImage).CombinedOutput(); err != nil {
		return fmt.Errorf("start the build registry: %v\n%s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", "-v", name).Run()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v2/", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the build registry never answered")
		}
	}
	repo := "build-" + suffix + ":latest"
	err = (&buildpacks.Builder{}).Build(ctx, buildpacks.Request{
		Source: filepath.Join(dir, spec.Context), Image: name + ":5000/" + repo, Registry: name + ":5000", Network: network,
		Insecure: true, FunctionTarget: spec.FunctionTarget, Env: spec.Env,
	})
	if err != nil {
		return err
	}
	pulled := fmt.Sprintf("127.0.0.1:%d/%s", port, repo)
	if out, err := exec.CommandContext(ctx, "docker", "pull", "--platform", buildpacks.BuilderPlatform, pulled).CombinedOutput(); err != nil {
		return fmt.Errorf("pull the built image: %v\n%s", err, out)
	}
	defer exec.Command("docker", "image", "rm", pulled).Run()
	if out, err := exec.CommandContext(ctx, "docker", "tag", pulled, spec.LocalImage).CombinedOutput(); err != nil {
		return fmt.Errorf("tag the built image: %v\n%s", err, out)
	}
	return nil
}

// maxSourceBytes bounds what a source archive may unpack to.
const maxSourceBytes = 2 << 30

// unpackSource unpacks a source archive into dir: a zip, which gcloud 586's
// `run deploy --source` uploads (#1347), or a gzipped tarball, which
// SubmitBuild's other callers and Cloud Build's own builds use.
func unpackSource(r io.Reader, dir string) error {
	br := bufio.NewReader(r)
	if magic, _ := br.Peek(4); !bytes.Equal(magic, []byte("PK\x03\x04")) {
		return untarGz(br, dir)
	}
	// A zip's directory is at its end, so it is read whole, bounded.
	data, err := io.ReadAll(io.LimitReader(br, maxSourceBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxSourceBytes {
		return fmt.Errorf("the source archive is more than %d bytes", int64(maxSourceBytes))
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("the source is not a zip archive: %w", err)
	}
	var total int64
	for _, f := range zr.File {
		clean := filepath.Clean(f.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("entry %q escapes the source directory", f.Name)
		}
		target := filepath.Join(dir, clean)
		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case f.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if f.Mode()&0o111 != 0 {
				mode = 0o755
			}
			src, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				src.Close()
				return err
			}
			n, err := io.Copy(out, io.LimitReader(src, maxSourceBytes-total+1))
			out.Close()
			src.Close()
			if err != nil {
				return err
			}
			if total += n; total > maxSourceBytes {
				return fmt.Errorf("the source unpacks to more than %d bytes", int64(maxSourceBytes))
			}
		}
		// Links and devices are skipped, as in a tarball.
	}
	return nil
}

// untarGz unpacks a gzipped tarball into dir, refusing any entry that would
// land outside it.
func untarGz(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the source is not a gzipped tarball: %w", err)
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(h.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("entry %q escapes the source directory", h.Name)
		}
		target := filepath.Join(dir, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxSourceBytes-total+1))
			f.Close()
			if err != nil {
				return err
			}
			if total += n; total > maxSourceBytes {
				return fmt.Errorf("the source unpacks to more than %d bytes", int64(maxSourceBytes))
			}
		default:
			// Links and devices are skipped: a link could point outside dir.
		}
	}
}

func tailLines(s string, n int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "\n")
}

// hostURL is http:// and a tunnel's host address, with a wildcard bind
// address dialled on loopback.
func hostURL(hostPort string) string {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "http://" + hostPort
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Save reads a locally built image back from Docker, for the push of it
// into this instance's Artifact Registry (#1383): `docker save` to a file
// in the instance's directory, read as an image. cleanup removes the file.
func (b *localImageBuilder) Save(ctx context.Context, local string) (v1.Image, func(), error) {
	f, err := os.CreateTemp(b.cfg.InstanceDir(), "push-*.tar")
	if err != nil {
		return nil, nil, err
	}
	f.Close()
	cleanup := func() { _ = os.Remove(f.Name()) }
	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", f.Name(), local).CombinedOutput(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("docker save %s: %v\n%s", local, err, tailLines(string(out), 10))
	}
	tag, err := name.NewTag(local)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	img, err := tarball.ImageFromPath(f.Name(), &tag)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read the saved image %s: %w", local, err)
	}
	return img, cleanup, nil
}
