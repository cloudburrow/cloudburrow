//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Bigtable change streams (#1523), which the Bigtable front serves from a
// change log it keeps of the writes it decodes (internal/bigtablefront,
// changestreams.go). The veneer does not read change streams, so the reads
// go through the module's generated data stub.

// bigtableStreamStub is the generated data client on the Bigtable endpoint.
func bigtableStreamStub(t *testing.T, h *Harness) bigtablepb.BigtableClient {
	t.Helper()
	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bigtablepb.NewBigtableClient(conn)
}

// bigtableStreamTable creates table with a change stream of retention and
// families f and g; it is deleted at cleanup.
func bigtableStreamTable(t *testing.T, h *Harness, table string, retention time.Duration) (*bigtable.AdminClient, *bigtable.Client) {
	t.Helper()
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: table, ChangeStreamRetention: retention,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy(), "g": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatalf("CreateTable with a change stream: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })
	c, err := bigtable.NewClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return admin, c
}

// readBigtableStream reads a change stream until it ends, returning its
// DataChange records and the other messages.
func readBigtableStream(ctx context.Context, d bigtablepb.BigtableClient, req *bigtablepb.ReadChangeStreamRequest) ([]*bigtablepb.ReadChangeStreamResponse_DataChange, []*bigtablepb.ReadChangeStreamResponse, error) {
	s, err := d.ReadChangeStream(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var dcs []*bigtablepb.ReadChangeStreamResponse_DataChange
	var other []*bigtablepb.ReadChangeStreamResponse
	for {
		r, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return dcs, other, nil
		}
		if err != nil {
			return dcs, other, err
		}
		if dc := r.GetDataChange(); dc != nil {
			dcs = append(dcs, dc)
		} else {
			other = append(other, r)
		}
	}
}

// bigtableChange is one DataChange in a short form.
func bigtableChange(dc *bigtablepb.ReadChangeStreamResponse_DataChange) string {
	var parts []string
	for _, ch := range dc.GetChunks() {
		switch m := ch.GetMutation().GetMutation().(type) {
		case *bigtablepb.Mutation_SetCell_:
			parts = append(parts, fmt.Sprintf("set %s:%s=%q", m.SetCell.GetFamilyName(), m.SetCell.GetColumnQualifier(), m.SetCell.GetValue()))
		case *bigtablepb.Mutation_DeleteFromColumn_:
			parts = append(parts, fmt.Sprintf("delcol %s:%s", m.DeleteFromColumn.GetFamilyName(), m.DeleteFromColumn.GetColumnQualifier()))
		case *bigtablepb.Mutation_DeleteFromFamily_:
			parts = append(parts, "delfam "+m.DeleteFromFamily.GetFamilyName())
		default:
			parts = append(parts, fmt.Sprintf("%T", m))
		}
	}
	return string(dc.GetRowKey()) + ": " + strings.Join(parts, ", ")
}

func bigtableTokens(p *bigtablepb.StreamPartition, token string) *bigtablepb.ReadChangeStreamRequest_ContinuationTokens {
	return &bigtablepb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &bigtablepb.StreamContinuationTokens{
		Tokens: []*bigtablepb.StreamContinuationToken{{Partition: p, Token: token}}}}
}

// TestBigtableChangeStreams (#1523): a table created with a change stream
// through the official admin client reports its retention; writes, deletes,
// a ReadModifyWrite and a conditional write through the official data
// client are read back from its change stream, on the partition
// GenerateInitialChangeStreamPartitions returns, with commit timestamps,
// tokens and watermarks, as Google's change stream records them; a read
// resumes from a continuation token, a caught-up stream sends heartbeats and
// then a live write, another partition is closed with the table's, and
// UpdateTable changes the retention and disables the stream, whose reads
// are then refused. The Bigtable pod's containers do not restart.
//
// covers: google.bigtable.v2.Bigtable/GenerateInitialChangeStreamPartitions, google.bigtable.v2.Bigtable/ReadChangeStream
func TestBigtableChangeStreams(t *testing.T) {
	h := New(t)
	admin, c := bigtableStreamTable(t, h, "streamed", 36*time.Hour)
	d := bigtableStreamStub(t, h)
	restarts := bigtableRestarts(t)
	before := restarts()
	ctx := h.Context()
	name := fmt.Sprintf("projects/%s/instances/%s/tables/streamed", h.Project(), bigtableInstance)
	if ti, err := admin.TableInfo(ctx, "streamed"); err != nil || ti.ChangeStreamRetention != 36*time.Hour {
		t.Fatalf("TableInfo = %+v, %v; want a retention of 36h", ti, err)
	}
	tbl := c.Open("streamed")
	// The front stamps commits on its own clock, which on Docker Desktop
	// trails the host's by up to a second: a start taken here just before
	// the first write could be after its commit, and the read from it left
	// that write out. The table is new, so nothing older can come in.
	start := time.Now().Add(-5 * time.Second)
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.ServerTime, []byte("v1"))
	if err := tbl.Apply(ctx, "r1", m); err != nil {
		t.Fatal(err)
	}
	m1, m2 := bigtable.NewMutation(), bigtable.NewMutation()
	m1.Set("f", "q", bigtable.Timestamp(5000), []byte("a"))
	m2.Set("g", "x", bigtable.Timestamp(5000), []byte("b"))
	if errs, err := tbl.ApplyBulk(ctx, []string{"r2", "r3"}, []*bigtable.Mutation{m1, m2}); err != nil || errs != nil {
		t.Fatalf("ApplyBulk: %v %v", err, errs)
	}
	del := bigtable.NewMutation()
	del.DeleteCellsInColumn("f", "q")
	if err := tbl.Apply(ctx, "r2", del); err != nil {
		t.Fatal(err)
	}
	row := bigtable.NewMutation()
	row.DeleteRow()
	if err := tbl.Apply(ctx, "r3", row); err != nil {
		t.Fatal(err)
	}
	rmw := bigtable.NewReadModifyWrite()
	rmw.AppendValue("f", "q", []byte("+"))
	if _, err := tbl.ApplyReadModifyWrite(ctx, "r1", rmw); err != nil {
		t.Fatal(err)
	}
	yes, no := bigtable.NewMutation(), bigtable.NewMutation()
	yes.Set("f", "c", bigtable.Timestamp(7000), []byte("matched"))
	no.Set("f", "c", bigtable.Timestamp(7000), []byte("not matched"))
	if err := tbl.Apply(ctx, "r1", bigtable.NewCondMutation(bigtable.FamilyFilter("f"), yes, no)); err != nil {
		t.Fatal(err)
	}

	ps, err := d.GenerateInitialChangeStreamPartitions(ctx, &bigtablepb.GenerateInitialChangeStreamPartitionsRequest{TableName: name})
	if err != nil {
		t.Fatal(err)
	}
	var parts []*bigtablepb.StreamPartition
	for {
		p, err := ps.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("GenerateInitialChangeStreamPartitions: %v", err)
		}
		parts = append(parts, p.GetPartition())
	}
	if len(parts) != 1 {
		t.Fatalf("partitions = %v, want one", parts)
	}
	end := time.Now()
	dcs, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)}, EndTime: timestamppb.New(end)})
	if err != nil {
		t.Fatalf("ReadChangeStream: %v", err)
	}
	var got []string
	var last time.Time
	for _, dc := range dcs {
		got = append(got, bigtableChange(dc))
		at := dc.GetCommitTimestamp().AsTime()
		if !at.After(last) || at.Before(start.Truncate(time.Microsecond)) || !dc.GetDone() || dc.GetToken() == "" || dc.GetEstimatedLowWatermark() == nil ||
			dc.GetType() != bigtablepb.ReadChangeStreamResponse_DataChange_USER {
			t.Errorf("record %s: commit %v after %v, done %v, token %q, watermark %v, type %v", bigtableChange(dc), at, last, dc.GetDone(), dc.GetToken(), dc.GetEstimatedLowWatermark(), dc.GetType())
		}
		last = at
	}
	want := []string{`r1: set f:q="v1"`, `r2: set f:q="a"`, `r3: set g:x="b"`, `r2: delcol f:q`, `r3: delfam f, delfam g`, `r1: set f:q="v1+"`, `r1: set f:c="matched"`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the change stream holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	resumed, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: bigtableTokens(parts[0], dcs[2].GetToken()), EndTime: timestamppb.New(end)})
	if err != nil || len(resumed) != len(want)-3 || bigtableChange(resumed[0]) != want[3] {
		t.Errorf("resumed from the third record's token: %d records, %v; want %d from %s", len(resumed), err, len(want)-3, want[3])
	}

	hctx, hcancel := context.WithTimeout(ctx, 30*time.Second)
	defer hcancel()
	s, err := d.ReadChangeStream(hctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: bigtableTokens(parts[0], dcs[len(dcs)-1].GetToken()), HeartbeatDuration: durationpb.New(200 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	hb, err := s.Recv()
	if err != nil || hb.GetHeartbeat() == nil || hb.GetHeartbeat().GetContinuationToken().GetToken() != dcs[len(dcs)-1].GetToken() {
		t.Fatalf("a caught-up stream's first message = %v, %v; want a heartbeat with the last token", hb, err)
	}
	live := bigtable.NewMutation()
	live.Set("f", "q", bigtable.Timestamp(9000), []byte("live"))
	if err := tbl.Apply(ctx, "r9", live); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := s.Recv()
		if err != nil {
			t.Fatalf("waiting for the live write: %v", err)
		}
		if dc := msg.GetDataChange(); dc != nil {
			if got := bigtableChange(dc); got != `r9: set f:q="live"` {
				t.Errorf("the live record = %s", got)
			}
			break
		}
	}
	hcancel()

	_, other, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name,
		Partition: &bigtablepb.StreamPartition{RowRange: &bigtablepb.RowRange{EndKey: &bigtablepb.RowRange_EndKeyOpen{EndKeyOpen: []byte("m")}}}})
	if err != nil || len(other) != 1 || other[0].GetCloseStream() == nil || len(other[0].GetCloseStream().GetNewPartitions()) != 1 {
		t.Errorf("a read of another partition = %v, %v; want a CloseStream with the table's partition", other, err)
	}

	if err := admin.UpdateTableWithChangeStream(ctx, "streamed", 72*time.Hour); err != nil {
		t.Fatalf("UpdateTableWithChangeStream: %v", err)
	}
	if ti, err := admin.TableInfo(ctx, "streamed"); err != nil || ti.ChangeStreamRetention != 72*time.Hour {
		t.Errorf("TableInfo after the update = %+v, %v", ti, err)
	}
	if err := admin.UpdateTableWithChangeStream(ctx, "streamed", 8*24*time.Hour); err == nil {
		t.Errorf("a retention of 8 days was accepted")
	}
	if err := admin.UpdateTableDisableChangeStream(ctx, "streamed"); err != nil {
		t.Fatalf("UpdateTableDisableChangeStream: %v", err)
	}
	if _, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0]}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a read after disabling: %v, want FailedPrecondition", err)
	}
	if _, err := admin.TableInfo(ctx, "streamed"); err != nil {
		t.Errorf("the table after the reads: %v", err)
	}
	if after := restarts(); after != before {
		t.Errorf("the Bigtable pod's restarts went from %s to %s", before, after)
	}
}

// TestBigtableChangeLogSurvivesAFrontRestart (#1523): the change log the
// Bigtable front keeps on its pod's emptyDir, as it keeps the rest of its
// state (#1514), is read back after a restart of the front's container
// alone, the emulator still running, and resumed from a token taken
// before it.
//
// covers: google.bigtable.v2.Bigtable/ReadChangeStream
func TestBigtableChangeLogSurvivesAFrontRestart(t *testing.T) {
	h := New(t)
	admin, c := bigtableStreamTable(t, h, "streamkept", 24*time.Hour)
	d := bigtableStreamStub(t, h)
	ctx := h.Context()
	name := fmt.Sprintf("projects/%s/instances/%s/tables/streamkept", h.Project(), bigtableInstance)
	start := time.Now()
	for i := 0; i < 3; i++ {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte(fmt.Sprint(i)))
		if err := c.Open("streamkept").Apply(ctx, fmt.Sprintf("k%d", i), m); err != nil {
			t.Fatal(err)
		}
	}
	full := &bigtablepb.StreamPartition{RowRange: &bigtablepb.RowRange{}}
	first, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: full,
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)}, EndTime: timestamppb.Now()})
	if err != nil || len(first) != 3 {
		t.Fatalf("before the restart: %d records, %v", len(first), err)
	}

	restartFront(t, "bigtable", "bigtable")

	var ti *bigtable.TableInfo
	if err := untilServed(t, func() (err error) { ti, err = admin.TableInfo(ctx, "streamkept"); return err }); err != nil || ti.ChangeStreamRetention != 24*time.Hour {
		t.Fatalf("TableInfo after the restart = %+v, %v", ti, err)
	}
	after, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: full,
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)}, EndTime: timestamppb.Now()})
	if err != nil || len(after) != 3 {
		t.Fatalf("after the restart: %d records, %v; want 3", len(after), err)
	}
	for i := range after {
		if bigtableChange(after[i]) != bigtableChange(first[i]) || after[i].GetToken() != first[i].GetToken() ||
			!after[i].GetCommitTimestamp().AsTime().Equal(first[i].GetCommitTimestamp().AsTime()) {
			t.Errorf("record %d after the restart = %s at %v, before %s at %v", i, bigtableChange(after[i]), after[i].GetCommitTimestamp(), bigtableChange(first[i]), first[i].GetCommitTimestamp())
		}
	}
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.Timestamp(1000), []byte("later"))
	if err := c.Open("streamkept").Apply(ctx, "later", m); err != nil {
		t.Fatal(err)
	}
	resumed, _, err := readBigtableStream(ctx, d, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: full,
		StartFrom: bigtableTokens(full, first[2].GetToken()), EndTime: timestamppb.Now()})
	if err != nil || len(resumed) != 1 || string(resumed[0].GetRowKey()) != "later" {
		t.Errorf("resumed after the restart from a token taken before it: %v, %v", resumed, err)
	}
}

// TestConsoleBigtableChangeStream (#1523): a table page's Edit change
// stream enables the table's change stream, read back through the official
// admin client, is prefilled with its retention, lists a write made through
// the official data client in the page's Change stream section, and
// disables the stream.
func TestConsoleBigtableChangeStream(t *testing.T) {
	h := New(t)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	admin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const table = "console-streamed"
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })
	prefilled := func() string {
		for _, a := range bigtableConsolePage(t, addr, project, table).Actions {
			if a.ID == "changestream" {
				for _, f := range a.Fields {
					if f.Name == "retention" {
						return f.Default
					}
				}
			}
		}
		return "absent"
	}
	if got := prefilled(); got != "0" {
		t.Errorf("Edit change stream is prefilled with %s, want 0", got)
	}
	if code, out := bigtableConsoleAct(t, addr, project, []string{table}, "changestream", map[string]string{"retention": "2"}); code != http.StatusOK {
		t.Fatalf("Edit change stream 2 = %d: %s", code, out)
	}
	if ti, err := admin.TableInfo(ctx, table); err != nil || ti.ChangeStreamRetention != 48*time.Hour {
		t.Errorf("TableInfo after Edit change stream = %+v, %v", ti, err)
	}
	if got := prefilled(); got != "2" {
		t.Errorf("Edit change stream is prefilled with %s, want 2", got)
	}
	c, err := bigtable.NewClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := admin.CreateColumnFamily(ctx, table, "cf"); err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("cf", "q", bigtable.Timestamp(1000), []byte("streamed"))
	if err := c.Open(table).Apply(ctx, "row-1", m); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, sec := range bigtableConsolePage(t, addr, project, table).Sections {
		if sec.ID == "changestream" {
			for _, it := range sec.Listing.Items {
				listed = append(listed, it.Name)
			}
		}
	}
	if len(listed) != 1 || listed[0] != "row-1" {
		t.Errorf("the Change stream section lists %v, want row-1", listed)
	}
	if code, out := bigtableConsoleAct(t, addr, project, []string{table}, "changestream", map[string]string{"retention": "0"}); code != http.StatusOK {
		t.Fatalf("Edit change stream 0 = %d: %s", code, out)
	}
	if ti, err := admin.TableInfo(ctx, table); err != nil || ti.ChangeStreamRetention != nil {
		t.Errorf("TableInfo after disabling = %+v, %v", ti, err)
	}
}

// TestTerraformBigtableTableChangeStream (#1523): a google_bigtable_table
// with change_stream_retention applies through `cloudburrow terraform`
// with the official hashicorp/google provider, egress blocked, reads back
// through the official admin client, plans clean, takes a new retention in
// place, plans clean again and destroys.
func TestTerraformBigtableTableChangeStream(t *testing.T) {
	h := New(t)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx := h.Context()
	const instance, table = "tf-stream-instance", "tf-streamed"
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	flags := cliArgs()
	dir := t.TempDir()
	write := func(retention string) {
		t.Helper()
		module := googleProviderRequirement + fmt.Sprintf(`resource "google_bigtable_table" "t" {
  project                 = %q
  instance_name           = %q
  name                    = %q
  deletion_protection     = "UNPROTECTED"
  change_stream_retention = %q
  column_family { family = "cf" }
}
`, h.Project(), instance, table, retention)
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tf := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if args[0] != "init" {
			cmd.Env = noGoogleEgress()
		}
		// The provider's Bigtable address comes from the wrapper.
		cmd.Env = append(cmd.Env, "BIGTABLE_EMULATOR_HOST=")
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	run := func(args ...string) {
		t.Helper()
		out, err := tf(args...)
		if err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, lastLines(out, 30))
		}
		t.Logf("terraform %s:\n%s", args[0], lastLines(out, 6))
	}
	cleanPlan := func(when string) {
		t.Helper()
		if out, err := tf("plan", "-detailed-exitcode", "-input=false", "-no-color"); err != nil {
			t.Errorf("plan %s is not clean (%v):\n%s", when, err, lastLines(out, 40))
		}
	}
	check := func(when string, want time.Duration) {
		t.Helper()
		ti, err := admin.TableInfo(ctx, table)
		if err != nil || ti.ChangeStreamRetention != want {
			t.Errorf("the table %s = %+v, %v; want a retention of %s", when, ti, err, want)
		}
	}

	write("24h0m0s")
	useGoogleProviderLock(t, dir)
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })
	check("after apply", 24*time.Hour)
	cleanPlan("right after apply")

	write("72h0m0s")
	if out, err := tf("plan", "-input=false", "-no-color"); err != nil || !strings.Contains(out, "0 to add, 1 to change, 0 to destroy") {
		t.Errorf("the new retention is not planned in place (%v):\n%s", err, lastLines(out, 30))
	}
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after the update", 72*time.Hour)
	cleanPlan("after the update")

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := admin.TableInfo(ctx, table); status.Code(err) != codes.NotFound {
		t.Errorf("the table after destroy: %v, want NotFound", err)
	}
}
