package main

// Undelete dataset on the BigQuery list screen (#1552): datasets.undelete,
// which restores a dataset deleted within its time travel window with its
// tables, views, routines and models (#1460). BigQuery's API lists no
// deleted dataset, so the form takes the ID the dataset had, as
// `bq undelete` does; UNDROP SCHEMA in the query editor's Read-write mode is
// the same restore in SQL.

import (
	"context"
	"errors"
	"strings"

	bq "google.golang.org/api/bigquery/v2"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const actUndeleteDataset = "undeletedataset"

// ListActions implements console.ListActor: Undelete dataset, on the one
// project the emulator serves.
func (p bigqueryProvider) ListActions(_ context.Context, project string) []console.Action {
	if p.writable(project) != nil {
		return nil
	}
	return []console.Action{{ID: actUndeleteDataset, Label: "Undelete dataset", Fields: []console.Field{
		{Name: "dataset", Label: "Dataset ID", Type: "text", Required: true, Pattern: `[A-Za-z0-9_]{1,1024}`,
			Help: "The ID of a dataset deleted within its time travel window; it comes back with its tables, views, routines and models. BigQuery lists no deleted dataset, so type the ID it had."},
	}}}
}

// undeleteDataset is datasets.undelete of the most recently deleted
// dataset with that ID.
func (p bigqueryProvider) undeleteDataset(ctx context.Context, values map[string]string) error {
	id := strings.TrimSpace(values["dataset"])
	if id == "" {
		return errors.New("name the dataset to undelete")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := p.service(ctx)
	if err != nil {
		return err
	}
	_, err = svc.Datasets.Undelete(p.project, id, &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	return bigqueryRefusal(err)
}

var _ console.ListActor = bigqueryProvider{}
