package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"

	"google.golang.org/api/iterator"
)

// Create table's Destination is Google's: Project, Dataset and Table, then
// Table type (https://docs.cloud.google.com/bigquery/docs/tables, read
// 2026-11-20). The form opened on a dataset names that dataset, and any
// other dataset of the project may be chosen instead; the table is made
// where the Destination says (#1861). The project is the one chosen in the
// toolbar: a table in another project is made from that project's page.
const (
	bqTableProject = "tableProject"
	bqTableDataset = "tableDataset"
)

// createTableFields are bigqueryTableFields with the Destination's Project
// and Dataset before Table: project is the one on screen, dataset the one
// the form was opened on, and datasets the project's, listed for the
// Dataset select (the opened one alone when they cannot be read).
func createTableFields(project, dataset string, datasets []string) []console.Field {
	if !slices.Contains(datasets, dataset) {
		datasets = append([]string{dataset}, datasets...)
	}
	dest := []console.Field{
		{Name: bqTableProject, Label: "Project", Type: "select", Section: "Destination", Options: []string{project},
			Default: project, Required: true,
			Help: "The project chosen in the toolbar. To make a table in another project, choose that project first."},
		{Name: bqTableDataset, Label: "Dataset", Type: "select", Section: "Destination", Options: datasets,
			Default: dataset, Required: true},
	}
	var out []console.Field
	for _, f := range bigqueryTableFields() {
		if f.Name == "tableId" {
			out = append(out, dest...)
		}
		out = append(out, f)
	}
	return out
}

// datasetIDs lists the project's dataset IDs, in the API's order.
func (p bigqueryProvider) datasetIDs(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var ids []string
	it := c.Datasets(ctx)
	for {
		ds, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return ids, nil
		}
		if err != nil {
			return ids, err
		}
		ids = append(ids, ds.DatasetID)
	}
}

// createTableTarget is the dataset path Create table makes its table in:
// the Destination's Dataset when the form sent one, else the dataset it
// was opened on. A Project other than the one on screen is refused.
func createTableTarget(project string, path []string, action string, values map[string]string) ([]string, error) {
	if action != "createtable" || len(path) != 1 {
		return path, nil
	}
	if v := strings.TrimSpace(values[bqTableProject]); v != "" && v != project {
		return nil, fmt.Errorf("Project %s: Create table makes the table in the project chosen in the toolbar, %s; choose %s there first", v, project, v)
	}
	if ds := strings.TrimSpace(values[bqTableDataset]); ds != "" {
		return []string{ds}, nil
	}
	return path, nil
}
