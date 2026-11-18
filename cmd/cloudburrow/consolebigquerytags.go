package main

import (
	"context"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A dataset's and a table's Tags (#1780): the Resource Manager tag values
// bound to //bigquery.googleapis.com/projects/P/datasets/D[/tables/T],
// with Bind tag and Unbind tag, as Google's console tags a dataset ("Edit
// details", Tags) and a table (https://docs.cloud.google.com/bigquery/docs/tags,
// read 2026-11-18). BigQuery's per-project IAM reads them in tag
// conditions (internal/bigqueryfront/iamcheck.go).

// bigqueryTagTarget is the full resource name of the dataset or table at
// path (dataset, or dataset and table), "" for any other path.
func (p bigqueryProvider) bigqueryTagTarget(path []string) string {
	switch {
	case len(path) == 1:
		return bigqueryfront.BigQueryTagPrefix + "projects/" + p.project + "/datasets/" + path[0]
	case len(path) == 2 && !isConnectionPath(path):
		return bigqueryfront.BigQueryTagPrefix + "projects/" + p.project + "/datasets/" + path[0] + "/tables/" + path[1]
	}
	return ""
}

// withTags adds the Tags section to a dataset's or table's page.
func (p bigqueryProvider) withTags(d console.Detail, path []string) console.Detail {
	target := p.bigqueryTagTarget(path)
	if p.registry == nil || target == "" || d.Unavailable != "" || d.Prompt != "" {
		return d
	}
	reg := p.registry
	sec := tagSection(reg, target)
	sec.Listing.Note = "Bind tag attaches a value here; a value of the same key bound nearer replaces an inherited one. " +
		"BigQuery's IAM conditions on tags read them for a caller with a minted token."
	d.Sections = append(d.Sections, sec)
	return d
}

// tagActionsAt are Bind tag and Unbind tag on a dataset's or table's page.
func (p bigqueryProvider) tagActionsAt(path []string) []console.Action {
	target := p.bigqueryTagTarget(path)
	if p.registry == nil || target == "" {
		return nil
	}
	reg := p.registry
	return tagActions(reg, target)
}

// tagActAt performs Bind tag or Unbind tag; handled is false otherwise.
func (p bigqueryProvider) tagActAt(_ context.Context, path []string, action string, values map[string]string) (bool, error) {
	if action != tagBind && action != tagUnbind {
		return false, nil
	}
	target := p.bigqueryTagTarget(path)
	if p.registry == nil || target == "" {
		return false, nil
	}
	reg := p.registry
	return tagAct(reg, target, action, values)
}
