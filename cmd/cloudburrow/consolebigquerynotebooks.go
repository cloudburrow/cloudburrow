package main

// BigQuery Studio's notebook runtimes (#2094): a notebook's Connect,
// Interrupt execution, Restart session and Disconnect and delete runtime,
// and its cells' runs, through POST /api/studio/bigquery?call=runtime. A
// runtime is internal/nbruntime's container, CloudBurrow's stand-in for
// the Colab Enterprise runtime Google's notebooks connect to
// (https://docs.cloud.google.com/bigquery/docs/create-notebooks).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// colabRuntimeKey is the notebook runtime manager's key of a Colab
// Enterprise runtime, the one vertexai's NotebookService starts it under.
func colabRuntimeKey(name string) string { return "colab/" + name }

// notebookCellTimeout bounds one cell's run. Colab Enterprise sets no
// limit on a cell; the console's request needs one, and a cell that
// outlives it is interrupted.
const notebookCellTimeout = 30 * time.Minute

type studioRuntimeReq struct {
	// Op is connect, status, run, interrupt, restart or disconnect.
	Op string `json:"op"`
	// Key is the notebook's: its saved asset's name, or its tab's id.
	Key  string `json:"key"`
	Code string `json:"code"`
	// Runtime, when set, is a Colab Enterprise runtime of the project
	// (projects/P/locations/L/notebookRuntimes/ID) the notebook is
	// connected to, as Studio's Connect to a runtime does: its cells run on
	// that runtime's kernel, which Colab Enterprise's NotebookService
	// started (colabHost). Its lifecycle stays the runtime's: connect does
	// not start it and disconnect leaves it running.
	Runtime string `json:"runtime"`
}

func (p bigqueryProvider) studioRuntime(ctx context.Context, project string, body json.RawMessage) (any, error) {
	if p.notebooks == nil {
		return nil, errors.New("this instance runs no notebook runtimes")
	}
	var req studioRuntimeReq
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("a runtime request: %w", err)
	}
	if req.Key == "" {
		return nil, errors.New("a runtime request needs the notebook's key")
	}
	if project == "" {
		project = p.project
	}
	// One runtime per project and notebook: a notebook opened in another
	// project is another runtime.
	key := project + "/" + req.Key
	if req.Runtime != "" {
		if !strings.HasPrefix(req.Runtime, "projects/"+project+"/locations/") || !strings.Contains(req.Runtime, "/notebookRuntimes/") {
			return nil, fmt.Errorf("%q is not a Colab Enterprise runtime of project %s", req.Runtime, project)
		}
		key = colabRuntimeKey(req.Runtime)
		switch req.Op {
		case "connect", "status":
			st := p.notebooks.Status(key)
			if st.State == "DISCONNECTED" {
				return nil, fmt.Errorf("the runtime %s is not running: start it first", req.Runtime[strings.LastIndex(req.Runtime, "/")+1:])
			}
			return st, nil
		case "disconnect":
			return map[string]any{"state": "DISCONNECTED"}, nil
		}
	}
	switch req.Op {
	case "connect":
		return p.notebooks.Connect(key, project), nil
	case "status":
		return p.notebooks.Status(key), nil
	case "run":
		ctx, cancel := context.WithTimeout(ctx, notebookCellTimeout)
		defer cancel()
		return p.notebooks.Execute(ctx, key, req.Code)
	case "interrupt":
		return map[string]any{}, p.notebooks.Interrupt(key)
	case "restart":
		return p.notebooks.Restart(key), nil
	case "disconnect":
		return p.notebooks.Status(key), p.notebooks.Disconnect(key)
	}
	return nil, fmt.Errorf("unknown runtime operation %q", req.Op)
}
