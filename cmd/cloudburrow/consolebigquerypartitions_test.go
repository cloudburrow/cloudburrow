package main

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

func TestPartitionIDExpr(t *testing.T) {
	day := &bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
		Schema: bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}}}
	if e, ok := partitionIDExpr(day); !ok || !strings.Contains(e, "TIMESTAMP(`d`)") || !strings.Contains(e, "'%Y%m%d'") {
		t.Errorf("day: %q, %v", e, ok)
	}
	month := &bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.MonthPartitioningType, Field: "ts"},
		Schema: bigquery.Schema{{Name: "ts", Type: bigquery.TimestampFieldType}}}
	if e, ok := partitionIDExpr(month); !ok || strings.Contains(e, "TIMESTAMP(`ts`)") || !strings.Contains(e, "MONTH") {
		t.Errorf("month: %q, %v", e, ok)
	}
	rng := &bigquery.TableMetadata{RangePartitioning: &bigquery.RangePartitioning{Field: "n",
		Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}}
	if e, ok := partitionIDExpr(rng); !ok || !strings.Contains(e, "DIV(`n` - 0, 10) * 10") {
		t.Errorf("range: %q, %v", e, ok)
	}
	if _, ok := partitionIDExpr(&bigquery.TableMetadata{TimePartitioning: &bigquery.TimePartitioning{}}); ok {
		t.Error("ingestion-time: want false")
	}
}
