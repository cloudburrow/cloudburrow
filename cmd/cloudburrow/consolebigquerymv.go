package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A materialized view's page (#1390): its definition with the table's
// details, and a Refresh action, CALL BQ.REFRESH_MATERIALIZED_VIEW, as
// Google's console gives it.

// actRefreshMV is the materialized view's Refresh action.
const actRefreshMV = "refreshmv"

// materializedViewProps are the properties of md's definition, if it is a
// materialized view.
func materializedViewProps(md *bigquery.TableMetadata) []console.Property {
	mv := md.MaterializedView
	if md.Type != bigquery.MaterializedView || mv == nil {
		return nil
	}
	refresh := "Off"
	if mv.EnableRefresh {
		refresh = "On, every " + mv.RefreshInterval.String()
	}
	last := "-"
	if !mv.LastRefreshTime.IsZero() {
		last = mv.LastRefreshTime.UTC().Format(time.RFC3339)
	}
	props := []console.Property{
		{Label: "Materialized view query", Value: mv.Query},
		{Label: "Automatic refresh", Value: refresh},
		{Label: "Last refreshed", Value: last},
	}
	if md.MaxStaleness != nil {
		props = append(props, console.Property{Label: "Max staleness", Value: md.MaxStaleness.String()})
	}
	return props
}

// refreshMaterializedView runs CALL BQ.REFRESH_MATERIALIZED_VIEW of
// dataset.table.
func (p bigqueryProvider) refreshMaterializedView(ctx context.Context, dataset, table string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	name := strings.ReplaceAll(dataset+"."+table, "'", "")
	job, err := c.Query(fmt.Sprintf("CALL BQ.REFRESH_MATERIALIZED_VIEW('%s')", name)).Run(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	return bigqueryRefusal(status.Err())
}
