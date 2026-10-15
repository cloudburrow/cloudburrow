package main

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

func TestPartitionIDExpr(t *testing.T) {
	day := &bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
		Schema: bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}}}
	if e := partitionIDExpr(day); !strings.Contains(e, "TIMESTAMP(`d`)") || !strings.Contains(e, "'%Y%m%d'") {
		t.Errorf("day: %q", e)
	}
	month := &bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.MonthPartitioningType, Field: "ts"},
		Schema: bigquery.Schema{{Name: "ts", Type: bigquery.TimestampFieldType}}}
	if e := partitionIDExpr(month); strings.Contains(e, "TIMESTAMP(`ts`)") || !strings.Contains(e, "MONTH") {
		t.Errorf("month: %q", e)
	}
	rng := &bigquery.TableMetadata{RangePartitioning: &bigquery.RangePartitioning{Field: "n",
		Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}}
	if e := partitionIDExpr(rng); !strings.Contains(e, "DIV(`n` - 0, 10) * 10") {
		t.Errorf("range: %q", e)
	}
	hour := &bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.HourPartitioningType}}
	if e := partitionIDExpr(hour); !strings.Contains(e, "FORMAT_TIMESTAMP('%Y%m%d%H', _PARTITIONTIME, 'UTC')") {
		t.Errorf("ingestion-time: %q", e) // #1277
	}
}
