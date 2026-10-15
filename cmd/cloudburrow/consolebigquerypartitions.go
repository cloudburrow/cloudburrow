package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// partitionsSection lists a column-partitioned table's partitions, each
// with its rows, and opens each as its own page, the table's page by the
// partition's decorated name (`t$20240101`), whose Preview reads that
// partition's rows only with tabledata.list (#1273). The partition IDs are
// BigQuery's: YYYYMMDD, YYYYMMDDHH, YYYYMM or YYYY by the time unit, an
// integer range's start, __NULL__ and __UNPARTITIONED__
// (https://cloud.google.com/bigquery/docs/managing-partitioned-table-data).
// They are read with one query of the table. An ingestion-time
// partitioned table's partitions are not listed: the emulator keeps no
// row's ingestion time.
func (p bigqueryProvider) partitionsSection(ctx context.Context, c *bigquery.Client, datasetID, tableID string,
	md *bigquery.TableMetadata) (console.Section, bool) {
	sec := console.Section{ID: "partitions", Label: "Partitions"}
	out := console.Listing{Columns: []string{"Rows"}, NameColumn: "Partition", Noun: "partitions"}
	id, ok := partitionIDExpr(md)
	switch {
	case md.TimePartitioning == nil && md.RangePartitioning == nil:
		return sec, false
	case !ok:
		out.Unavailable = "The partitions of an ingestion-time partitioned table are not listed: the emulator behind CloudBurrow keeps no row's ingestion time."
		sec.Listing = out
		return sec, true
	}
	q := c.Query(fmt.Sprintf("SELECT %s AS id, COUNT(*) AS n FROM `%s.%s.%s` GROUP BY id ORDER BY id",
		id, p.project, datasetID, tableID))
	it, err := q.Read(ctx)
	if err != nil {
		out.Unavailable = "reading partitions: " + bigqueryRefusal(err).Error()
		sec.Listing = out
		return sec, true
	}
	for {
		var row struct {
			ID string `bigquery:"id"`
			N  int64  `bigquery:"n"`
		}
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			out.Unavailable = "reading partitions: " + bigqueryRefusal(err).Error()
			break
		}
		out.Items = append(out.Items, console.Resource{Name: row.ID, Fields: map[string]string{"Rows": fmt.Sprint(row.N)},
			Opens: []string{datasetID, tableID + "$" + row.ID}})
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "partitions")
			break
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "This table has no rows, so no partitions."
	}
	sec.Listing = out
	return sec, true
}

// partitionIDExpr is the GoogleSQL expression of a row's partition ID in
// a column-partitioned table, or false for an ingestion-time one.
func partitionIDExpr(md *bigquery.TableMetadata) (string, bool) {
	if rp := md.RangePartitioning; rp != nil && rp.Range != nil {
		col := "`" + rp.Field + "`"
		r := rp.Range
		return fmt.Sprintf("CASE WHEN %[1]s IS NULL THEN '__NULL__' WHEN %[1]s < %[2]d OR %[1]s >= %[3]d THEN '__UNPARTITIONED__' "+
			"ELSE CAST(%[2]d + DIV(%[1]s - %[2]d, %[4]d) * %[4]d AS STRING) END", col, r.Start, r.End, r.Interval), true
	}
	tp := md.TimePartitioning
	if tp == nil || tp.Field == "" {
		return "", false
	}
	col := "`" + tp.Field + "`"
	expr := col
	for _, f := range md.Schema {
		if strings.EqualFold(f.Name, tp.Field) && (f.Type == bigquery.DateFieldType || f.Type == bigquery.DateTimeFieldType) {
			expr = "TIMESTAMP(" + col + ")"
		}
	}
	format, unit := "%Y%m%d", "DAY"
	switch tp.Type {
	case bigquery.HourPartitioningType:
		format, unit = "%Y%m%d%H", "HOUR"
	case bigquery.MonthPartitioningType:
		format, unit = "%Y%m", "MONTH"
	case bigquery.YearPartitioningType:
		format, unit = "%Y", "YEAR"
	}
	return fmt.Sprintf("CASE WHEN %[1]s IS NULL THEN '__NULL__' WHEN %[2]s < TIMESTAMP '1960-01-01' OR %[2]s >= TIMESTAMP '2160-01-01' "+
		"THEN '__UNPARTITIONED__' ELSE FORMAT_TIMESTAMP('%[3]s', TIMESTAMP_TRUNC(%[2]s, %[4]s), 'UTC') END", col, expr, format, unit), true
}
