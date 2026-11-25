//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
	"github.com/cloudburrow/cloudburrow/third_party/zetasketch"
)

// TestBigtableAggregateFamiliesMinMaxAndHLL (#1893): the aggregate column
// families Google documents (https://docs.cloud.google.com/bigtable/docs/aggregates)
// through the official Go client: Min and Max keep the lower and higher
// value of a cell, Sum the total, HLL++ a ZetaSketch sketch whose estimate
// counts the distinct inputs, each merged at the cell's timestamp, through
// MutateRow, MutateRows and CheckAndMutateRow, with MergeToCell taking a
// state and a deleted cell starting again.
func TestBigtableAggregateFamiliesMinMaxAndHLL(t *testing.T) {
	h := New(t)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := localclient.Bigtable(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	table := fmt.Sprintf("aggs-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = ac.DeleteTable(context.Background(), table) })
	checkAggregateFamilies(ctx, t, ac, c, table)
}

// checkAggregateFamilies is the test; internal/bigtablefront's
// TestAggregateFamiliesMinMaxAndHLL runs the same against bttest in process.
func checkAggregateFamilies(ctx context.Context, t *testing.T, ac *bigtable.AdminClient, c *bigtable.Client, table string) {
	t.Helper()
	agg := func(a bigtable.Aggregator) bigtable.Family {
		return bigtable.Family{ValueType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: a}}
	}
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: table, ColumnFamilies: map[string]bigtable.Family{
		"mn": agg(bigtable.MinAggregator{}), "mx": agg(bigtable.MaxAggregator{}),
		"sm": agg(bigtable.SumAggregator{}), "hl": agg(bigtable.HllppUniqueCountAggregator{}),
	}}); err != nil {
		t.Fatalf("CreateTableFromConf: %v", err)
	}
	tbl := c.Open(table)
	ts := bigtable.Time(time.UnixMilli(1_700_000_000_000))
	later := bigtable.Time(time.UnixMilli(1_700_000_060_000))
	for _, v := range []int64{5, -3, 9, 2} {
		m := bigtable.NewMutation()
		m.AddIntToCell("mn", "q", ts, v)
		m.AddIntToCell("mx", "q", ts, v)
		m.AddIntToCell("sm", "q", ts, v)
		if err := tbl.Apply(ctx, "r1", m); err != nil {
			t.Fatalf("Apply AddToCell %d: %v", v, err)
		}
	}
	// Two in one mutation list, and one in a bulk write.
	m := bigtable.NewMutation()
	m.AddIntToCell("mn", "q", ts, 4)
	m.AddIntToCell("mn", "q", ts, -7)
	if err := tbl.Apply(ctx, "r1", m); err != nil {
		t.Fatalf("Apply two AddToCell: %v", err)
	}
	bulk := bigtable.NewMutation()
	bulk.AddIntToCell("mx", "q", ts, 12)
	if errs, err := tbl.ApplyBulk(ctx, []string{"r1"}, []*bigtable.Mutation{bulk}); err != nil || errs != nil {
		t.Fatalf("ApplyBulk AddToCell: %v %v", err, errs)
	}
	// MergeToCell takes the state: 8 big-endian bytes for an Int64.
	merge := bigtable.NewMutation()
	merge.MergeBytesToCell("mx", "q", ts, binary.BigEndian.AppendUint64(nil, 30))
	merge.MergeBytesToCell("mn", "q", ts, binary.BigEndian.AppendUint64(nil, 0))
	if err := tbl.Apply(ctx, "r1", merge); err != nil {
		t.Fatalf("Apply MergeToCell: %v", err)
	}
	// Another timestamp is another cell.
	other := bigtable.NewMutation()
	other.AddIntToCell("mn", "q", later, 100)
	if err := tbl.Apply(ctx, "r1", other); err != nil {
		t.Fatalf("Apply AddToCell at another timestamp: %v", err)
	}

	row, err := tbl.ReadRow(ctx, "r1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	got := map[string][]string{}
	for fam, items := range row {
		for _, it := range items {
			if fam == "hl" {
				continue
			}
			if len(it.Value) != 8 {
				t.Fatalf("%s cell at %d holds %d bytes, want an 8-byte Int64", it.Column, it.Timestamp, len(it.Value))
			}
			got[fam] = append(got[fam], fmt.Sprintf("%d@%d", int64(binary.BigEndian.Uint64(it.Value)), it.Timestamp))
		}
	}
	want := map[string][]string{
		"mn": {fmt.Sprintf("100@%d", later), fmt.Sprintf("-7@%d", ts)},
		"mx": {fmt.Sprintf("30@%d", ts)},
		"sm": {fmt.Sprintf("13@%d", ts)},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("aggregate cells = %v, want %v", got, want)
	}

	// HLL++: 50 distinct values, each added twice.
	for i := range 100 {
		h := bigtable.NewMutation()
		h.AddIntToCell("hl", "q", ts, int64(i%50))
		if err := tbl.Apply(ctx, "r2", h); err != nil {
			t.Fatalf("Apply AddToCell to the HLL++ family: %v", err)
		}
	}
	estimate := func() int64 {
		t.Helper()
		row, err := tbl.ReadRow(ctx, "r2", bigtable.RowFilter(bigtable.FamilyFilter("hl")))
		if err != nil {
			t.Fatalf("ReadRow hl: %v", err)
		}
		if len(row["hl"]) != 1 {
			t.Fatalf("the HLL++ family holds %d cells, want 1", len(row["hl"]))
		}
		h := zetasketch.NewHLL(nil)
		if err := h.UnmarshalBinary(row["hl"][0].Value); err != nil {
			t.Fatalf("the HLL++ cell is not a ZetaSketch sketch: %v", err)
		}
		return h.Result()
	}
	if n := estimate(); n != 50 {
		t.Errorf("HLL++ estimate after 50 distinct values = %d, want 50", n)
	}
	// MergeToCell merges a sketch of 25 more distinct values.
	more := zetasketch.NewHLL(nil)
	for i := range 25 {
		more.Add(zetasketch.Uint64Value(uint64(1000 + i)))
	}
	sketch, err := more.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	hm := bigtable.NewMutation()
	hm.MergeBytesToCell("hl", "q", ts, sketch)
	if err := tbl.Apply(ctx, "r2", hm); err != nil {
		t.Fatalf("Apply MergeToCell of a sketch: %v", err)
	}
	if n := estimate(); n != 75 {
		t.Errorf("HLL++ estimate after the merge = %d, want 75", n)
	}
	bad := bigtable.NewMutation()
	bad.MergeBytesToCell("hl", "q", ts, []byte("not a sketch"))
	if err := tbl.Apply(ctx, "r2", bad); err == nil {
		t.Error("MergeToCell of bytes that are not a sketch succeeded; want INVALID_ARGUMENT")
	}

	// A conditional write merges too, and a deleted cell starts again.
	cond := bigtable.NewMutation()
	cond.AddIntToCell("mx", "q", ts, 31)
	if err := tbl.Apply(ctx, "r1", bigtable.NewCondMutation(bigtable.FamilyFilter("mx"), cond, nil)); err != nil {
		t.Fatalf("Apply CheckAndMutateRow: %v", err)
	}
	row, err = tbl.ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.FamilyFilter("mx")))
	if err != nil {
		t.Fatalf("ReadRow mx: %v", err)
	}
	if len(row["mx"]) != 1 || int64(binary.BigEndian.Uint64(row["mx"][0].Value)) != 31 {
		t.Errorf("the max cell after a conditional AddToCell 31 = %v, want 31", row["mx"])
	}
	reset := bigtable.NewMutation()
	reset.DeleteCellsInFamily("mx")
	reset.AddIntToCell("mx", "q", ts, 1)
	if err := tbl.Apply(ctx, "r1", reset); err != nil {
		t.Fatalf("Apply delete then AddToCell: %v", err)
	}
	row, err = tbl.ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.FamilyFilter("mx")))
	if err != nil {
		t.Fatalf("ReadRow mx: %v", err)
	}
	if len(row["mx"]) != 1 || int64(binary.BigEndian.Uint64(row["mx"][0].Value)) != 1 {
		t.Errorf("the max cell after a delete and AddToCell 1 = %v, want 1", row["mx"])
	}
}
