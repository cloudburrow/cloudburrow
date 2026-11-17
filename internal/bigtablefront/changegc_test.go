package bigtablefront

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestChangeStreamRecordsGarbageCollection (#1523): the cells the
// emulator's garbage collection removes from a table with a change stream
// are recorded as GARBAGE_COLLECTION records with no source cluster, a
// DeleteFromColumn of each collected cell; a family with no rule records
// none.
func TestChangeStreamRecordsGarbageCollection(t *testing.T) {
	old := gcPollInterval
	gcPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { gcPollInterval = old })
	addr := serve(t)
	ctx, ac, c, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "gc", ChangeStreamRetention: 24 * time.Hour,
		Families: map[string]bigtable.GCPolicy{
			"v": bigtable.MaxVersionsPolicy(1),
			"a": bigtable.MaxAgePolicy(time.Hour),
			"n": bigtable.NoGcPolicy(),
		}}); err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("gc")
	start := time.Now()
	m := bigtable.NewMutation()
	m.Set("v", "q", bigtable.Timestamp(1000), []byte("old"))
	m.Set("v", "q", bigtable.Timestamp(2000), []byte("new"))
	m.Set("n", "q", bigtable.Timestamp(1000), []byte("kept"))
	m.Set("n", "q", bigtable.Timestamp(2000), []byte("kept too"))
	m.Set("a", "q", bigtable.Timestamp(3000), []byte("aged"))
	m.Set("a", "q", bigtable.Now(), []byte("fresh"))
	if err := tbl.Apply(ctx, "r", m); err != nil {
		t.Fatal(err)
	}
	name := "projects/p/instances/i/tables/gc"
	var gc []*bigtablepb.ReadChangeStreamResponse_DataChange
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		rs, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: wholeTable(),
			StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)},
			EndTime:   timestamppb.Now()})
		if err != nil {
			t.Fatal(err)
		}
		gc = nil
		for _, dc := range dataChanges(rs) {
			if dc.GetType() == bigtablepb.ReadChangeStreamResponse_DataChange_GARBAGE_COLLECTION {
				gc = append(gc, dc)
			}
		}
		var n int
		for _, dc := range gc {
			n += len(dc.GetChunks())
		}
		if n >= 2 {
			break
		}
	}
	var got []string
	for _, dc := range gc {
		if dc.GetSourceClusterId() != "" {
			t.Errorf("GC record source cluster %q, want none", dc.GetSourceClusterId())
		}
		for _, ch := range dc.GetChunks() {
			dfc := ch.GetMutation().GetDeleteFromColumn()
			if dfc == nil {
				t.Fatalf("GC record mutation %v, want a DeleteFromColumn", ch.GetMutation())
			}
			got = append(got, string(dc.GetRowKey())+" "+dfc.GetFamilyName()+":"+string(dfc.GetColumnQualifier())+" "+
				timeRange(dfc.GetTimeRange()))
		}
	}
	want := "r a:q 3000-4000\nr v:q 1000-2000"
	if strings.Join(got, "\n") != want {
		t.Fatalf("GC records:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
	r, err := tbl.ReadRow(ctx, "r")
	if err != nil || len(r["v"]) != 1 || len(r["n"]) != 2 || len(r["a"]) != 1 {
		t.Fatalf("row after GC = %v, %v", r, err)
	}
}

func timeRange(r *bigtablepb.TimestampRange) string {
	return strconv.FormatInt(r.GetStartTimestampMicros(), 10) + "-" + strconv.FormatInt(r.GetEndTimestampMicros(), 10)
}

// TestChangeStreamRecordsDropRowRange (#1523): DropRowRange on a table
// with a change stream records each row it deleted, as Google documents,
// a USER record of a DeleteFromFamily of each family where the row had
// data; the rows outside the range are not recorded.
func TestChangeStreamRecordsDropRowRange(t *testing.T) {
	addr := serve(t)
	ctx, ac, c, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "drop", ChangeStreamRetention: 24 * time.Hour,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy(), "g": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("drop")
	for row, fams := range map[string][]string{"p1": {"f", "g"}, "p2": {"g"}, "q1": {"f"}} {
		m := bigtable.NewMutation()
		for _, fam := range fams {
			m.Set(fam, "c", bigtable.Timestamp(1000), []byte("x"))
		}
		if err := tbl.Apply(ctx, row, m); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := ac.DropRowRange(ctx, "drop", "p"); err != nil {
		t.Fatal(err)
	}
	rs, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: "projects/p/instances/i/tables/drop", Partition: wholeTable(),
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)}, EndTime: timestamppb.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, dc := range dataChanges(rs) {
		if dc.GetType() != bigtablepb.ReadChangeStreamResponse_DataChange_USER {
			t.Errorf("record %s type %v, want USER", describeChange(dc), dc.GetType())
		}
		got = append(got, describeChange(dc))
	}
	if want := "p1: delfam f, delfam g\np2: delfam g"; strings.Join(got, "\n") != want {
		t.Fatalf("DropRowRange records:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
	if err := ac.DropAllRows(ctx, "drop"); err != nil {
		t.Fatal(err)
	}
	if r, err := tbl.ReadRow(ctx, "q1"); err != nil || len(r) != 0 {
		t.Fatalf("q1 after DropAllRows = %v, %v", r, err)
	}
}
