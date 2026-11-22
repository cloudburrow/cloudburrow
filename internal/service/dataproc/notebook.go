package dataproc

// PySpark notebook batches (#2162): a pyspark_notebook_batch runs an
// .ipynb from Cloud Storage as the batch's PySpark driver. Its code cells
// run in order in one Python process on the batch's runtime, with
// params injected as papermill does (Google's PySparkNotebookBatch.params,
// "The parameters to pass to the notebook", read 2026-11-22:
// https://docs.cloud.google.com/dataproc-serverless/docs/reference/rest/v1/projects.locations.batches#PySparkNotebookBatch):
// a cell of NAME = "VALUE" assignments right after the cell tagged
// "parameters", or first when no cell is. Markdown and raw cells are
// skipped. A cell's IPython magics and shell escapes (% and ! lines) have
// no IPython to run them on the Spark runtime's Python, so a notebook with
// one fails, naming the cell.

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

// notebookScript is the Python a notebook's code cells run as, with
// params injected.
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
	var b strings.Builder
	b.WriteString("# A PySpark notebook batch's code cells (CloudBurrow).\n")
	tagged := false
	for _, c := range n.Cells {
		for _, t := range c.Metadata.Tags {
			tagged = tagged || t == "parameters"
		}
	}
	if !tagged {
		b.WriteString(inject.String())
	}
	cell := 0
	for _, c := range n.Cells {
		if c.CellType != "code" {
			continue
		}
		cell++
		src := source(c.Source)
		for _, line := range strings.Split(src, "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "%") || strings.HasPrefix(t, "!") {
				return "", fmt.Errorf("code cell %d has %q: IPython magics and shell commands do not run in a PySpark notebook batch", cell, t)
			}
		}
		fmt.Fprintf(&b, "\n# In[%d]:\n%s\n", cell, src)
		for _, t := range c.Metadata.Tags {
			if t == "parameters" {
				b.WriteString(inject.String())
			}
		}
	}
	return b.String(), nil
}
