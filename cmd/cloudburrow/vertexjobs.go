package main

// What Vertex AI's pipelines and custom jobs need of the instance beyond
// Docker (#2210): their task images, pipeline templates and BigQuery.
//
//   - A task's (or a custom job replica's) image is used when it is on this
//     machine's Docker; else one in the instance's Artifact Registry
//     ({location}-docker.pkg.dev/{project}/{repository}/{image}) is read
//     from it and loaded into Docker, as the cluster's nodes pull it
//     (clusterhost.go's registry mirror); one in any other Google registry
//     (gcr.io, *.pkg.dev not on this instance) is refused, never pulled;
//     and any other image is pulled from its public registry, as the
//     cluster's nodes pull one.
//   - A pipeline template (template_uri) is read from the instance's
//     Artifact Registry for https://{location}-kfp.pkg.dev/..., the KFP
//     protocol artifactregistrygo.go serves, and through internal/egress
//     for any other https:// URL, so a googleapis.com URL is the instance's
//     own API and no other Google host is dialled.
//   - BigQuery is the instance's BigQuery REST API, as the run's identity.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

var (
	arDockerRE = regexp.MustCompile(`^([a-z0-9-]+)-docker\.pkg\.dev/`)
	arKFPRE    = regexp.MustCompile(`^([a-z0-9-]+)-kfp\.pkg\.dev$`)
)

func (p colabPipelines) addr(service string) string {
	if p.live == nil {
		return ""
	}
	return p.live()[service]
}

// imageHere reports whether ref is on this machine's Docker.
func (p colabPipelines) imageHere(ctx context.Context, ref string) bool {
	return exec.CommandContext(ctx, p.bin(), "image", "inspect", "--format", "{{.Id}}", ref).Run() == nil
}

// ensureImage makes ref runnable on Docker: here already, loaded from the
// instance's Artifact Registry, or pulled from a public registry; it
// returns the reference to run it by.
func (p colabPipelines) ensureImage(ctx context.Context, ref string) (string, error) {
	if p.imageHere(ctx, ref) {
		return ref, nil
	}
	host, _, _ := strings.Cut(ref, "/")
	if m := arDockerRE.FindStringSubmatch(ref); m != nil {
		local := ref
		if base, digest, ok := strings.Cut(ref, "@"); ok {
			// A digest reference is loaded under a tag of its digest.
			base, _, _ = strings.Cut(base, ":")
			local = base + ":cb-" + strings.TrimPrefix(digest, "sha256:")[:min(24, len(strings.TrimPrefix(digest, "sha256:")))]
		}
		if p.imageHere(ctx, local) {
			return local, nil
		}
		return local, p.loadFromRegistry(ctx, ref, local, m[1])
	}
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") && host != "localhost" {
		host = "docker.io"
	}
	if egress.GoogleHost(host) {
		return "", fmt.Errorf("the image %s is in Google's registry %s, which this instance never pulls from: push it to the instance's Artifact Registry "+
			"({location}-docker.pkg.dev/{project}/{repository}/{image}) or docker pull it yourself", ref, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, p.bin(), "pull", ref).CombinedOutput(); err != nil {
		return "", fmt.Errorf("the image %s cannot be pulled from %s: %s", ref, host, strings.TrimSpace(string(out)))
	}
	return ref, nil
}

// loadFromRegistry reads ref from the instance's Artifact Registry (the
// Run port, under {location}-docker.pkg.localhost) and loads it into
// Docker under its own name.
func (p colabPipelines) loadFromRegistry(ctx context.Context, ref, local, location string) error {
	addr := p.addr("run")
	if addr == "" {
		return fmt.Errorf("the image %s is in Artifact Registry, which this instance does not serve (start with --services including run)", ref)
	}
	_, port, _ := net.SplitHostPort(addr)
	rest := strings.TrimPrefix(ref, location+"-docker.pkg.dev/")
	src, err := name.ParseReference(location+"-docker.pkg.localhost:"+port+"/"+rest, name.Insecure)
	if err != nil {
		return fmt.Errorf("the image %s: %v", ref, err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	img, err := remote.Image(src, remote.WithContext(ctx), remote.WithTransport(tr),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return fmt.Errorf("the image %s cannot be read from the instance's Artifact Registry: %v", ref, err)
	}
	tag, err := name.NewTag(local, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("the image %s: %v", ref, err)
	}
	cmd := exec.CommandContext(ctx, p.bin(), "load")
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := tarball.Write(tag, img, pw)
	_ = pw.CloseWithError(werr)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("load %s into Docker: %v: %s", ref, err, strings.TrimSpace(out.String()))
	}
	return werr
}

// FetchTemplate is vertexai.TemplateFetcher: a pipeline template's YAML.
func (p colabPipelines) FetchTemplate(ctx context.Context, uri string) ([]byte, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("template_uri %q is not an https:// URL", uri)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var req *http.Request
	client := egress.Client(&http.Client{})
	if arKFPRE.MatchString(strings.ToLower(u.Host)) {
		addr := p.addr("run")
		if addr == "" {
			return nil, fmt.Errorf("template_uri %s is in Artifact Registry, which this instance does not serve (start with --services including run)", uri)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/"+strings.ToLower(u.Host)+u.EscapedPath(), nil)
		client = &http.Client{}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	}
	if err != nil {
		return nil, err
	}
	if tok := vertexai.RunAsToken(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("template_uri %s cannot be read: %v", uri, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("template_uri %s answered %s: %s", uri, resp.Status, strings.TrimSpace(string(b)))
	}
	if len(b) > 10<<20 {
		return nil, fmt.Errorf("template_uri %s is larger than 10 MiB", uri)
	}
	return b, nil
}

// BigQueryREST is vertexai.PipelineBigQuery: a call to the instance's
// BigQuery REST API as the run's identity.
func (p colabPipelines) BigQueryREST(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	addr := p.addr("bigquery")
	if addr == "" {
		return 0, nil, fmt.Errorf("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vertexai.RunAsToken(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, b, err
}
