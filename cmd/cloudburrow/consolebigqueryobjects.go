package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// An object table's page (#1783), as Google's console gives one: the
// fixed schema, the table's connection, its Cloud Storage URIs, its
// metadata cache mode and max staleness, a Preview of its rows (read by a
// query, since tabledata.list does not read an external table), and
// Refresh metadata cache, CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE, for a
// MANUAL cache. A dataset's Create object table makes one through
// tables.insert of an objectMetadata externalDataConfiguration.

const (
	actRefreshObjectCache = "refreshobjectcache"
	actCreateObjectTable  = "createobjecttable"
)

// isObjectTable reports whether md is an object table's.
func isObjectTable(md *bigquery.TableMetadata) bool {
	return md != nil && md.Type == bigquery.ExternalTable && md.ExternalDataConfig != nil && md.ExternalDataConfig.ObjectMetadata != ""
}

// objectTableProps are the properties of md's object table definition,
// if it is one.
func objectTableProps(md *bigquery.TableMetadata) []console.Property {
	if !isObjectTable(md) {
		return nil
	}
	e := md.ExternalDataConfig
	mode := "-"
	if e.MetadataCacheMode != "" {
		mode = string(e.MetadataCacheMode)
	}
	staleness := "0 (metadata caching off)"
	if md.MaxStaleness != nil {
		staleness = md.MaxStaleness.String()
	}
	return []console.Property{
		{Label: "Object metadata", Value: string(e.ObjectMetadata)},
		{Label: "Connection ID", Value: orDash(e.ConnectionID)},
		{Label: "Source URIs", Value: strings.Join(e.SourceURIs, ", ")},
		{Label: "Metadata cache mode", Value: mode},
		{Label: "Max staleness", Value: staleness},
	}
}

// objectPreviewSection is an object table's first rows, by a query.
func (p bigqueryProvider) objectPreviewSection(ctx context.Context, c *bigquery.Client, dataset, table string, schema bigquery.Schema) console.Section {
	sec := console.Section{ID: "preview", Label: "Preview"}
	q := c.Query(fmt.Sprintf("SELECT * FROM `%s.%s` ORDER BY uri LIMIT %d", dataset, table, detailLimit))
	it, err := q.Read(ctx)
	if err != nil {
		sec.Unavailable = "reading rows: " + bigqueryRefusal(err).Error()
		return sec
	}
	listing, err := readRows(it, schema)
	if err != nil {
		sec.Unavailable = "reading rows: " + err.Error()
		return sec
	}
	sec.Listing = listing
	return sec
}

// objectTableActions are an object table's page's actions: it is
// read-only, so no Insert rows, Edit schema, Load or Copy.
func objectTableActions(md *bigquery.TableMetadata) []console.Action {
	var actions []console.Action
	if md.ExternalDataConfig.MetadataCacheMode == bigquery.Manual {
		actions = append(actions, console.Action{ID: actRefreshObjectCache, Label: "Refresh metadata cache"})
	}
	return append(actions,
		console.Action{ID: "edittable", Label: "Edit table", Fields: bigqueryEditFields(md)},
		console.Action{ID: "deletetable", Label: "Delete table", Destructive: true, Leaves: true, ConfirmStyle: console.DeleteConfirmWord})
}

// createObjectTableFields are Create object table's form.
func createObjectTableFields() []console.Field {
	return []console.Field{
		{Name: "tableId", Label: "Table", Type: "text", Required: true, Pattern: bigqueryTableIDPattern},
		{Name: "connection", Label: "Connection ID", Type: "text", Required: true,
			Help: "PROJECT_ID.LOCATION.CONNECTION_ID of a Cloud resource connection in the dataset's location; its service account needs Storage Object Viewer on the bucket."},
		{Name: "uris", Label: "Cloud Storage URIs", Type: "textarea", Required: true,
			Help: "One per line: gs://bucket/[folder/]object, with at most one * wildcard."},
		{Name: "cacheMode", Label: "Metadata cache mode", Type: "select", Options: []string{"", "AUTOMATIC", "MANUAL"},
			Help: "Empty for no metadata cache."},
		{Name: "stalenessMinutes", Label: "Max staleness (minutes)", Type: "number",
			Help: "30 to 10080 with a cache mode; empty or 0 for no cache."},
	}
}

// createObjectTable makes an object table in dataset.
func (p bigqueryProvider) createObjectTable(ctx context.Context, dataset string, values map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var uris []string
	for _, u := range strings.Split(values["uris"], "\n") {
		if u = strings.TrimSpace(u); u != "" {
			uris = append(uris, u)
		}
	}
	md := &bigquery.TableMetadata{ExternalDataConfig: &bigquery.ExternalDataConfig{ObjectMetadata: bigquery.ObjectMetadataSimple,
		SourceURIs: uris, ConnectionID: strings.TrimSpace(values["connection"])}}
	switch values["cacheMode"] {
	case "AUTOMATIC":
		md.ExternalDataConfig.MetadataCacheMode = bigquery.Automatic
	case "MANUAL":
		md.ExternalDataConfig.MetadataCacheMode = bigquery.Manual
	}
	if s := strings.TrimSpace(values["stalenessMinutes"]); s != "" && s != "0" {
		var m int64
		if _, err := fmt.Sscan(s, &m); err != nil || m < 0 {
			return errors.New("max staleness is a whole number of minutes")
		}
		d := time.Duration(m) * time.Minute
		md.MaxStaleness = &bigquery.IntervalValue{Days: int32(d / (24 * time.Hour)), Hours: int32(d % (24 * time.Hour) / time.Hour),
			Minutes: int32(d % time.Hour / time.Minute)}
	}
	return bigqueryRefusal(c.Dataset(dataset).Table(strings.TrimSpace(values["tableId"])).Create(ctx, md))
}

// refreshObjectCache runs CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE of
// dataset.table.
func (p bigqueryProvider) refreshObjectCache(ctx context.Context, dataset, table string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	name := strings.ReplaceAll(dataset+"."+table, "'", "")
	job, err := c.Query(fmt.Sprintf("CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE('%s')", name)).Run(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	return bigqueryRefusal(status.Err())
}
