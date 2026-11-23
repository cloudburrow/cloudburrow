package dataproc

// PySpark notebook batches (#2162): a pyspark_notebook_batch runs an
// .ipynb from Cloud Storage as the batch's PySpark driver. Its code cells
// run in order in one Python process on the batch's runtime, with
// params injected as papermill does (Google's PySparkNotebookBatch.params,
// "The parameters to pass to the notebook", read 2026-11-22:
// https://docs.cloud.google.com/dataproc-serverless/docs/reference/rest/v1/projects.locations.batches#PySparkNotebookBatch):
// a cell of NAME = "VALUE" assignments right after the cell tagged
// "parameters", or first when no cell is. Markdown and raw cells are
// skipped. The cells run in IPython (IPython 8.37 and its dependencies,
// pure-Python wheels from PyPI pinned by SHA-256 in dependencies.json,
// mounted into the driver), so their magics and shell escapes run.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// pyIdentRE is a parameter's name: a Python identifier, as it is
// assigned.
var pyIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ipynb struct {
	Cells []struct {
		CellType string          `json:"cell_type"`
		Source   json.RawMessage `json:"source"`
		Metadata struct {
			Tags []string `json:"tags"`
		} `json:"metadata"`
	} `json:"cells"`
}

// source is a cell's source, a string or a list of lines.
func source(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var lines []string
	_ = json.Unmarshal(raw, &lines)
	return strings.Join(lines, "")
}

// notebookScript is the Python a notebook's driver runs: its code cells,
// with params injected, each run in turn by IPython's InteractiveShell as
// a notebook's kernel runs them, so IPython's line and cell magics and
// shell escapes (% and ! lines) run as they do in the notebook (IPython is
// on the driver's PYTHONPATH, sparkproc.Job.IPython). The batch fails at
// the first cell that raises, after its traceback.
func notebookScript(nb []byte, params map[string]string) (string, error) {
	var n ipynb
	if err := json.Unmarshal(nb, &n); err != nil {
		return "", fmt.Errorf("the notebook is not a Jupyter notebook (.ipynb): %v", err)
	}
	var inject strings.Builder
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		inject.WriteString("# Parameters\n")
		for _, k := range keys {
			v, _ := json.Marshal(params[k])
			fmt.Fprintf(&inject, "%s = %s\n", k, v)
		}
	}
	type cell struct {
		Label  string `json:"label"`
		Source string `json:"source"`
	}
	var cells []cell
	tagged := false
	for _, c := range n.Cells {
		for _, t := range c.Metadata.Tags {
			tagged = tagged || t == "parameters"
		}
	}
	if !tagged && inject.Len() > 0 {
		cells = append(cells, cell{"injected parameters", inject.String()})
	}
	count := 0
	for _, c := range n.Cells {
		if c.CellType != "code" {
			continue
		}
		count++
		cells = append(cells, cell{fmt.Sprintf("In[%d]", count), source(c.Source)})
		for _, t := range c.Metadata.Tags {
			if t == "parameters" && inject.Len() > 0 {
				cells = append(cells, cell{"injected parameters", inject.String()})
			}
		}
	}
	body, err := json.Marshal(cells)
	if err != nil {
		return "", err
	}
	quoted, _ := json.Marshal(string(body))
	return fmt.Sprintf(notebookDriver, quoted), nil
}

// notebookDriver runs the cells (%%s: their JSON, as a Python string
// literal) in IPython's InteractiveShell.
const notebookDriver = `# A PySpark notebook batch's code cells (CloudBurrow), run by IPython.
import json as _cb_json, os as _cb_os, sys as _cb_sys
_cb_os.environ.setdefault("IPYTHONDIR", "/tmp/cb-ipython")
from IPython.core.interactiveshell import InteractiveShell as _cb_Shell
_cb_shell = _cb_Shell.instance(colors="NoColor")
for _cb_cell in _cb_json.loads(%s):
    print("# " + _cb_cell["label"] + ":", flush=True)
    _cb_result = _cb_shell.run_cell(_cb_cell["source"], store_history=True)
    _cb_sys.stdout.flush()
    if not _cb_result.success:
        _cb_err = _cb_result.error_in_exec or _cb_result.error_before_exec
        print(type(_cb_err).__name__ + ": " + str(_cb_err) + " (in the notebook's cell " + _cb_cell["label"] + ")", file=_cb_sys.stderr, flush=True)
        _cb_sys.exit(1)
`
