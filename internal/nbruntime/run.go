package nbruntime

// RunNotebook runs a whole notebook on a runtime of its own, as a scheduled
// notebook or a Dataform notebook action runs on Google: a Colab
// Enterprise execution creates a runtime from the runtime template, runs
// every code cell in order and keeps the executed notebook, its outputs
// in it (https://docs.cloud.google.com/colab/docs/schedule-notebook-run,
// https://docs.cloud.google.com/bigquery/docs/orchestrate-notebooks). The
// run stops at the first cell that raises, as an execution does, and the
// notebook returned holds the outputs up to and including that cell.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RunTimeout bounds the wait for a fresh runtime (its image's first build
// included).
const RunTimeout = 30 * time.Minute

// CellError is a cell that raised: the run stopped there.
type CellError struct {
	Cell   int
	Name   string
	Value  string
	Output []byte // the executed notebook up to the failing cell
}

func (e *CellError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("cell %d raised %s", e.Cell, e.Name)
	}
	return fmt.Sprintf("cell %d raised %s: %s", e.Cell, e.Name, e.Value)
}

// RunNotebook runs the code cells of an nbformat 4 notebook on a new
// runtime named key in project, deletes the runtime, and returns the
// executed notebook. A cell that raises ends the run with a *CellError.
func (m *Manager) RunNotebook(ctx context.Context, key, project string, notebook []byte) ([]byte, error) {
	var nb map[string]any
	if err := json.Unmarshal(notebook, &nb); err != nil {
		return nil, fmt.Errorf("the notebook is not nbformat JSON: %w", err)
	}
	cells, _ := nb["cells"].([]any)
	m.Connect(key, project)
	defer func() { _ = m.Disconnect(key) }()
	deadline := time.Now().Add(RunTimeout)
	for {
		st := m.Status(key)
		if st.State == StateConnected {
			break
		}
		if st.State == StateFailed {
			return nil, fmt.Errorf("the notebook runtime did not start: %s", st.Message)
		}
		if st.State == StateDisconnected {
			return nil, errors.New("the notebook runtime was deleted before it started")
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the notebook runtime did not start in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	n := 0
	for _, c := range cells {
		cell, _ := c.(map[string]any)
		if cell == nil || cell["cell_type"] != "code" {
			continue
		}
		n++
		code := sourceOf(cell["source"])
		res, err := m.Execute(ctx, key, code)
		if err != nil {
			return nil, err
		}
		outs := make([]any, 0, len(res.Outputs))
		for _, o := range res.Outputs {
			var v any
			if json.Unmarshal(o, &v) == nil {
				outs = append(outs, v)
			}
		}
		cell["outputs"] = outs
		cell["execution_count"] = res.ExecutionCount
		if res.Status != "ok" {
			out, _ := json.Marshal(nb)
			ce := &CellError{Cell: n, Name: "Error", Output: out}
			for _, o := range outs {
				if om, _ := o.(map[string]any); om != nil && om["output_type"] == "error" {
					ce.Name, _ = om["ename"].(string)
					ce.Value, _ = om["evalue"].(string)
				}
			}
			return out, ce
		}
	}
	return json.Marshal(nb)
}

// sourceOf is a cell's source: a string, or nbformat's list of lines.
func sourceOf(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		var b strings.Builder
		for _, line := range s {
			if l, ok := line.(string); ok {
				b.WriteString(l)
			}
		}
		return b.String()
	}
	return ""
}
