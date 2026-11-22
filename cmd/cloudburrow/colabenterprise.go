package main

// Colab Enterprise runtimes (#2105) on the instance's notebook runtime:
// vertexai's NotebookService runs each NotebookRuntime as an
// internal/nbruntime runtime, and Dataform's notebook actions and
// BigQuery Studio's scheduled notebooks run on a runtime template's
// settings.

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/nbruntime"
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
