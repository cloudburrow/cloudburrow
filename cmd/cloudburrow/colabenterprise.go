package main

// Colab Enterprise runtimes (#2105) on the instance's notebook runtime:
// vertexai's NotebookService runs each NotebookRuntime as an
// internal/nbruntime runtime, and Dataform's notebook actions and
// BigQuery Studio's scheduled notebooks run on a runtime template's
// settings.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/nbruntime"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// colabHost is vertexai.NotebookHost over the notebook runtime manager.
type colabHost struct{ m *nbruntime.Manager }

func nbOptions(o vertexai.NotebookRuntimeOptions) nbruntime.Options {
	return nbruntime.Options{IdleTimeout: o.IdleTimeout, IdleDisabled: o.IdleDisabled, Env: o.Env}
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
	return storage.NewClient(ctx, option.WithEndpoint("http://"+f.HostAddr()+"/storage/v1/"), option.WithoutAuthentication(),
		storage.WithJSONReads(), storage.WithDisabledClientMetrics())
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
