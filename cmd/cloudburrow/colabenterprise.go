package main

// Colab Enterprise runtimes (#2105) on the instance's notebook runtime:
// vertexai's NotebookService runs each NotebookRuntime as an
// internal/nbruntime runtime, and Dataform's notebook actions and
// BigQuery Studio's scheduled notebooks run on a runtime template's
// settings.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/nbruntime"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// colabHost is vertexai.NotebookHost over the notebook runtime manager.
type colabHost struct{ m *nbruntime.Manager }

func nbOptions(o vertexai.NotebookRuntimeOptions) nbruntime.Options {
	return nbruntime.Options{IdleTimeout: o.IdleTimeout, IdleDisabled: o.IdleDisabled, Env: o.Env, Token: o.Token}
}

func (h colabHost) Start(key, project string, o vertexai.NotebookRuntimeOptions) {
	h.m.ConnectWith(key, project, nbOptions(o))
}

func (h colabHost) State(key string) (string, string) {
	st := h.m.Status(key)
	return st.State, st.Message
}

func (h colabHost) Exec(ctx context.Context, key, code string) error {
	r, err := h.m.Execute(ctx, key, code)
	if err != nil {
		return err
	}
	if r.Status != "ok" {
		msg := "the code raised an error"
		for _, o := range r.Outputs {
			if s := string(o); strings.Contains(s, `"output_type": "error"`) || strings.Contains(s, `"output_type":"error"`) {
				msg = s
			}
		}
		return errors.New(msg)
	}
	return nil
}

func (h colabHost) Stop(key string) error { return h.m.Disconnect(key) }

// Outputs runs code and returns its outputs (vertexai.NotebookOutputs),
// for a Workbench instance's console page (#2173).
func (h colabHost) Outputs(ctx context.Context, key, code string) ([]json.RawMessage, error) {
	r, err := h.m.Execute(ctx, key, code)
	if err != nil {
		return nil, err
	}
	return r.Outputs, nil
}

func (h colabHost) Version() string { return nbruntime.Tag() }

// colabTemplates resolves a runtime template's options on srv, the local
// Vertex AI endpoint; nil when that endpoint is not served.
func colabTemplates(srv *vertexai.Server) func(string) (nbruntime.Options, error) {
	if srv == nil {
		return nil
	}
	return func(name string) (nbruntime.Options, error) {
		o, err := srv.NotebookRuntimeTemplateOptions(name)
		if err != nil {
			return nbruntime.Options{}, err
		}
		return nbOptions(o), nil
	}
}

// colabExecutor is vertexai.NotebookExecutor (#2105): Colab Enterprise's
// notebook executions read their notebooks from the instance's Cloud
// Storage (with the official client) or its Dataform repositories, run on
// the notebook runtime, and write the executed notebook back to Cloud
// Storage.
type colabExecutor struct {
	m        *nbruntime.Manager
	storage  func() *netfwd.Forwarder
	dataform func() *dataform.Server
}

func (x colabExecutor) client(ctx context.Context) (*storage.Client, error) {
	var f *netfwd.Forwarder
	if x.storage != nil {
		f = x.storage()
	}
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("Cloud Storage is not served on this instance (start with --services including storage)")
	}
	return storage.NewClient(ctx, option.WithEndpoint("http://"+f.HostAddr()+"/storage/v1/"), runAsAuth(ctx),
		storage.WithJSONReads(), storage.WithDisabledClientMetrics())
}

// runAsAuth is a run's credentials (#2173): the bearer token of the
// identity it executes as, else none (the instance's own).
func runAsAuth(ctx context.Context) option.ClientOption {
	if tok := vertexai.RunAsToken(ctx); tok != "" {
		return option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"}))
	}
	return option.WithoutAuthentication()
}

func (x colabExecutor) ReadObject(ctx context.Context, bucket, object string, generation int64) ([]byte, error) {
	c, err := x.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	o := c.Bucket(bucket).Object(object)
	if generation > 0 {
		o = o.Generation(generation)
	}
	r, err := o.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gs://%s/%s: %w", bucket, object, err)
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.LimitReader(r, 64<<20))
}

func (x colabExecutor) WriteObject(ctx context.Context, bucket, object, contentType string, data []byte) error {
	c, err := x.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	w := c.Bucket(bucket).Object(object).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (x colabExecutor) ReadRepositoryFile(ctx context.Context, repository, commit, file string) ([]byte, error) {
	var api *dataform.Server
	if x.dataform != nil {
		api = x.dataform()
	}
	if api == nil {
		return nil, errors.New("Dataform is not served on this instance (start with --services including dataform)")
	}
	resp, err := api.ReadRepositoryFile(ctx, &dataformpb.ReadRepositoryFileRequest{Name: repository, CommitSha: commit, Path: file})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repository, err)
	}
	return resp.GetContents(), nil
}

func (x colabExecutor) Run(ctx context.Context, key, project string, notebook []byte, o vertexai.NotebookRuntimeOptions) ([]byte, error) {
	out, err := x.m.RunNotebookWith(ctx, key, project, notebook, nbOptions(o))
	var ce *nbruntime.CellError
	if errors.As(err, &ce) && len(out) == 0 {
		out = ce.Output
	}
	return out, err
}

// colabPipelines is vertexai.PipelineRunner (#2173): a Vertex AI pipeline
// task's container runs on this machine's Docker with no network, its
// artifacts read from and written to the instance's Cloud Storage.
type colabPipelines struct {
	colabExecutor
	instance string
	docker   string
}

func (p colabPipelines) bin() string {
	if p.docker != "" {
		return p.docker
	}
	return "docker"
}

func (p colabPipelines) RunContainer(ctx context.Context, c vertexai.PipelineContainer) error {
	if out, err := exec.CommandContext(ctx, p.bin(), "image", "inspect", "--format", "{{.Id}}", c.Image).CombinedOutput(); err != nil {
		return fmt.Errorf("the image %s is not on this machine: CloudBurrow runs a pipeline task's image only when it is already here (docker pull or build it first; nothing is pulled from a Google registry): %s",
			c.Image, strings.TrimSpace(string(out)))
	}
	name := "cb-pl-" + strings.Trim(pipelineContainerSafe.ReplaceAllString(strings.ToLower(c.Key), "-"), "-")
	if len(name) > 120 {
		name = name[:120]
	}
	args := []string{"run", "--rm", "--name", name, "--network", "none", "--label", "cloudburrow.instance=" + p.instance}
	for _, k := range sortedKeys(c.Labels) {
		args = append(args, "--label", k+"="+c.Labels[k])
	}
	for _, k := range sortedKeys(c.Mounts) {
		args = append(args, "-v", c.Mounts[k]+":"+k)
	}
	for _, k := range sortedKeys(c.Env) {
		args = append(args, "-e", k+"="+c.Env[k])
	}
	rest := c.Args
	if len(c.Command) > 0 {
		args = append(args, "--entrypoint", c.Command[0])
		rest = append(append([]string{}, c.Command[1:]...), c.Args...)
	}
	args = append(append(args, c.Image), rest...)
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Cancel = func() error {
		_ = exec.Command(p.bin(), "rm", "-f", name).Run()
		return cmd.Process.Kill()
	}
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("the container exited: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func (p colabPipelines) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []string
	it := c.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, a.Name)
	}
}

var pipelineContainerSafe = regexp.MustCompile(`[^a-z0-9_.-]+`)

// tailBuffer keeps the last 8 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8<<10 {
		t.b = t.b[len(t.b)-8<<10:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
