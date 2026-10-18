//go:build compat

package compat

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The Bigtable RPCs TestBigtableTableAndRows does not reach (#1198).
//
// The methods the emulator (bttest) has no implementation of panic it,
// which restarts the pod and loses every table. The front before the
// emulator (internal/bigtablefront, #1206) serves backups itself since
// #1499 (TestBigtableBackups), and schema bundles
// (TestBigtableSchemaBundles), and UndeleteTable since #1511
// (TestBigtableUndeleteTable), and the change stream reads since #1523
// (TestBigtableChangeStreams), from a change log it keeps, and the session
// data plane since #1523 (TestBigtableSessions).

const bigtableInstance = "cb-instance"

// bigtableTable creates a table with an "info" family through the admin
// client and returns both clients; the table is deleted at cleanup.
func bigtableTable(t *testing.T, h *Harness, table string) (*bigtable.AdminClient, *bigtable.Client) {
	t.Helper()
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), table) })
	if err := admin.CreateColumnFamily(ctx, table, "info"); err != nil {
		t.Fatalf("CreateColumnFamily: %v", err)
	}
	c, err := bigtable.NewClient(ctx, h.Project(), bigtableInstance)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return admin, c
}

func bigtableRowKeys(t *testing.T, h *Harness, tbl *bigtable.Table) []string {
	t.Helper()
	var keys []string
	if err := tbl.ReadRows(h.Context(), bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	return keys
}

// TestBigtableBulkAndConditionalWrites writes rows with ApplyBulk
// (MutateRows), applies a conditional mutation (CheckAndMutateRow) whose
// branch depends on the row, increments a counter (ReadModifyWriteRow), and
// samples the row keys (SampleRowKeys).
//
// covers: google.bigtable.v2.Bigtable/MutateRows, google.bigtable.v2.Bigtable/CheckAndMutateRow, google.bigtable.v2.Bigtable/ReadModifyWriteRow, google.bigtable.v2.Bigtable/SampleRowKeys
func TestBigtableBulkAndConditionalWrites(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "writes")
	ctx := h.Context()
	tbl := c.Open("writes")

	var keys []string
	var muts []*bigtable.Mutation
	for i := 0; i < 3; i++ {
		m := bigtable.NewMutation()
		m.Set("info", "n", bigtable.Now(), []byte(fmt.Sprint(i)))
		keys, muts = append(keys, fmt.Sprintf("row-%d", i)), append(muts, m)
	}
	errs, err := tbl.ApplyBulk(ctx, keys, muts)
	if err != nil || errs != nil {
		t.Fatalf("ApplyBulk = %v, %v", errs, err)
	}
	if got := bigtableRowKeys(t, h, tbl); fmt.Sprint(got) != fmt.Sprint(keys) {
		t.Errorf("rows after ApplyBulk = %v, want %v", got, keys)
	}

	// row-0 has info:n, so the true branch runs; row-9 does not exist.
	for _, tc := range []struct {
		row         string
		wantMatched bool
		wantValue   string
	}{{"row-0", true, "yes"}, {"row-9", false, "no"}} {
		yes, no := bigtable.NewMutation(), bigtable.NewMutation()
		yes.Set("info", "matched", bigtable.Now(), []byte("yes"))
		no.Set("info", "matched", bigtable.Now(), []byte("no"))
		var matched bool
		cond := bigtable.NewCondMutation(bigtable.ColumnFilter("n"), yes, no)
		if err := tbl.Apply(ctx, tc.row, cond, bigtable.GetCondMutationResult(&matched)); err != nil {
			t.Fatalf("conditional Apply(%s): %v", tc.row, err)
		}
		if matched != tc.wantMatched {
			t.Errorf("%s matched = %v, want %v", tc.row, matched, tc.wantMatched)
		}
		r, err := tbl.ReadRow(ctx, tc.row, bigtable.RowFilter(bigtable.ColumnFilter("matched")))
		if err != nil {
			t.Fatalf("ReadRow(%s): %v", tc.row, err)
		}
		if items := r["info"]; len(items) != 1 || string(items[0].Value) != tc.wantValue {
			t.Errorf("%s info:matched = %v, want %q", tc.row, items, tc.wantValue)
		}
	}

	for _, step := range []struct{ delta, want int64 }{{5, 5}, {7, 12}} {
		want := step.want
		rmw := bigtable.NewReadModifyWrite()
		rmw.Increment("info", "count", step.delta)
		r, err := tbl.ApplyReadModifyWrite(ctx, "counter", rmw)
		if err != nil {
			t.Fatalf("ApplyReadModifyWrite: %v", err)
		}
		items := r["info"]
		if len(items) != 1 || len(items[0].Value) != 8 || int64(binary.BigEndian.Uint64(items[0].Value)) != want {
			t.Errorf("counter = %v, want %d", items, want)
		}
	}

	samples, err := tbl.SampleRowKeys(ctx)
	if err != nil {
		t.Fatalf("SampleRowKeys: %v", err)
	}
	if !sort.StringsAreSorted(samples) {
		t.Errorf("SampleRowKeys = %q, want sorted keys", samples)
	}
}

// TestBigtableTableMaintenance drops rows by prefix and then all rows
// (DropRowRange), waits for replication (GenerateConsistencyToken, then
// CheckConsistency), turns deletion protection on and off (UpdateTable),
// and deletes the table (DeleteTable), checking each effect.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/DropRowRange, google.bigtable.admin.v2.BigtableTableAdmin/GenerateConsistencyToken, google.bigtable.admin.v2.BigtableTableAdmin/CheckConsistency, google.bigtable.admin.v2.BigtableTableAdmin/UpdateTable, google.bigtable.admin.v2.BigtableTableAdmin/DeleteTable
func TestBigtableTableMaintenance(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "upkeep")
	ctx := h.Context()
	tbl := c.Open("upkeep")

	for _, k := range []string{"a-1", "a-2", "b-1"} {
		m := bigtable.NewMutation()
		m.Set("info", "v", bigtable.Now(), []byte(k))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatalf("Apply(%s): %v", k, err)
		}
	}
	if err := admin.DropRowRange(ctx, "upkeep", "a-"); err != nil {
		t.Fatalf("DropRowRange: %v", err)
	}
	if got := bigtableRowKeys(t, h, tbl); fmt.Sprint(got) != "[b-1]" {
		t.Errorf("rows after dropping a- = %v, want [b-1]", got)
	}
	if err := admin.DropAllRows(ctx, "upkeep"); err != nil {
		t.Fatalf("DropAllRows: %v", err)
	}
	if got := bigtableRowKeys(t, h, tbl); len(got) != 0 {
		t.Errorf("rows after DropAllRows = %v, want none", got)
	}

	if err := admin.WaitForReplication(ctx, "upkeep"); err != nil {
		t.Fatalf("WaitForReplication: %v", err)
	}

	for _, p := range []bigtable.DeletionProtection{bigtable.Protected, bigtable.Unprotected} {
		if err := admin.UpdateTableWithDeletionProtection(ctx, "upkeep", p); err != nil {
			t.Fatalf("UpdateTableWithDeletionProtection(%v): %v", p, err)
		}
		info, err := admin.TableInfo(ctx, "upkeep")
		if err != nil {
			t.Fatalf("TableInfo: %v", err)
		}
		if info.DeletionProtection != p {
			t.Errorf("DeletionProtection = %v, want %v", info.DeletionProtection, p)
		}
	}

	if err := admin.DeleteTable(ctx, "upkeep"); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	tables, err := admin.Tables(ctx)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	for _, name := range tables {
		if name == "upkeep" {
			t.Errorf("Tables after DeleteTable = %v, still lists upkeep", tables)
		}
	}
}

// TestBigtablePingAndWarm warms the client's connection.
//
// covers: google.bigtable.v2.Bigtable/PingAndWarm
func TestBigtablePingAndWarm(t *testing.T) {
	h := New(t)
	_, c := bigtableTable(t, h, "warm")
	if err := c.PingAndWarm(h.Context()); err != nil {
		t.Fatalf("PingAndWarm: %v", err)
	}
}

// TestBigtableSessions (#1523): the session data plane, through the
// official Go client. Measured first: the emulator answers
// GetClientConfiguration, OpenTable, OpenAuthorizedView and
// OpenMaterializedView UNIMPLEMENTED "the emulator does not currently
// support <Method>". The client (cloud.google.com/go/bigtable v1.58.0)
// builds a session client when it dials the endpoint itself, not through
// BIGTABLE_EMULATOR_HOST, polls GetClientConfiguration, and sends the share
// of its ReadRow and Apply calls session_load names through sessions. The
// front answers session_load 1 and serves the sessions: the client's writes
// and reads of a table and of an authorized view go through OpenTable and
// OpenAuthorizedView, with no classic ReadRows or MutateRow, and read and
// write what those do (an authorized view's subset applies:
// PERMISSION_DENIED outside it). By hand through the generated stub: the
// configuration, the handshake and session parameters, a vRPC each way, a
// read on a write session PERMISSION_DENIED, and a materialized view's
// session, whose ReadRow is UNIMPLEMENTED as ReadRows of one is (the
// client then reads it the classic way).
//
// covers: google.bigtable.v2.Bigtable/GetClientConfiguration, google.bigtable.v2.Bigtable/OpenTable, google.bigtable.v2.Bigtable/OpenAuthorizedView
// covers: google.bigtable.v2.Bigtable/OpenMaterializedView (unimplemented)
func TestBigtableSessions(t *testing.T) {
	h := New(t)
	admin, classic := bigtableTable(t, h, "sessions")
	ctx := h.Context()
	endpoint := h.Endpoint(EnvBigtable)
	inst := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)
	table := inst + "/tables/sessions"

	// A classic write is logged naming the table, so the log can show
	// that the session client's are not.
	before := bigtableLastCall(t, endpoint)
	cm := bigtable.NewMutation()
	cm.Set("info", "q", bigtable.Timestamp(500), []byte("classic"))
	if err := classic.Open("sessions").Apply(ctx, "c1", cm); err != nil {
		t.Fatalf("Apply through BIGTABLE_EMULATOR_HOST: %v", err)
	}
	if n := bigtableCalls(t, endpoint, before, "/google.bigtable.v2.Bigtable/MutateRow", table); n != 1 {
		t.Fatalf("a classic MutateRow is logged %d times naming %s, want once", n, table)
	}

	sv := &bigtable.SubsetViewConf{}
	sv.AddRowPrefix([]byte("v#"))
	sv.AddFamilySubsetQualifierPrefix("info", []byte(""))
	if err := admin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: "sessions", AuthorizedViewID: "v", AuthorizedView: sv}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteAuthorizedView(h.Context(), "sessions", "v") })

	// The client dials the endpoint itself, as against Google.
	t.Setenv("BIGTABLE_EMULATOR_HOST", "")
	c, err := bigtable.NewClientWithConfig(ctx, h.Project(), bigtableInstance,
		bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}, DisableDirectAccess: true},
		option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = c.Close()
		}
	}()
	tbl := c.Open("sessions")
	start := bigtableLastCall(t, endpoint)
	// Until the client's first poll lands, its writes are classic.
	for i := 0; ; i++ {
		seq := bigtableLastCall(t, endpoint)
		m := bigtable.NewMutation()
		m.Set("info", "q", bigtable.Timestamp(1000), []byte("v1"))
		if err := tbl.Apply(ctx, "r1", m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if bigtableCalls(t, endpoint, seq, "/google.bigtable.v2.Bigtable/MutateRow", table) == 0 {
			break
		}
		if i == 100 {
			t.Fatal("the client never sent a write through a session")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if bigtableCalls(t, endpoint, start, "/google.bigtable.v2.Bigtable/GetClientConfiguration", "") == 0 {
		t.Error("the client's GetClientConfiguration is not logged")
	}
	seq := bigtableLastCall(t, endpoint)
	r, err := tbl.ReadRow(ctx, "r1")
	if err != nil || len(r["info"]) != 1 || string(r["info"][0].Value) != "v1" || r["info"][0].Timestamp != 1000 {
		t.Errorf("ReadRow = %v, %v", r, err)
	}
	if r, err := tbl.ReadRow(ctx, "missing"); err != nil || r != nil {
		t.Errorf("ReadRow of a missing row = %v, %v, want none", r, err)
	}
	m := bigtable.NewMutation()
	m.Set("info", "q2", bigtable.Timestamp(2000), []byte("v2"))
	if err := tbl.Apply(ctx, "r1", m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r, err := tbl.ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.ColumnFilter("q2"))); err != nil || len(r["info"]) != 1 || string(r["info"][0].Value) != "v2" {
		t.Errorf("ReadRow with a filter = %v, %v", r, err)
	}
	del := bigtable.NewMutation()
	del.DeleteRow()
	if err := tbl.Apply(ctx, "r1", del); err != nil {
		t.Fatalf("Apply DeleteRow: %v", err)
	}
	if r, err := tbl.ReadRow(ctx, "r1"); err != nil || r != nil {
		t.Errorf("ReadRow after DeleteRow = %v, %v, want none", r, err)
	}
	for _, method := range []string{"ReadRows", "MutateRow"} {
		if n := bigtableCalls(t, endpoint, seq, "/google.bigtable.v2.Bigtable/"+method, table); n != 0 {
			t.Errorf("%d classic %s calls once sessions are on, want none", n, method)
		}
	}

	view := c.OpenAuthorizedView("sessions", "v")
	vm := bigtable.NewMutation()
	vm.Set("info", "q", bigtable.Timestamp(3000), []byte("in"))
	if err := view.Apply(ctx, "v#1", vm); err != nil {
		t.Errorf("Apply inside the view: %v", err)
	}
	if r, err := view.ReadRow(ctx, "v#1"); err != nil || len(r["info"]) != 1 || string(r["info"][0].Value) != "in" {
		t.Errorf("ReadRow through the view = %v, %v", r, err)
	}
	if err := view.Apply(ctx, "x#1", vm); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Apply outside the view = %v, want PermissionDenied", err)
	}
	// Read back through the classic path, the session's write is there.
	var back []string
	if err := tbl.ReadRows(ctx, bigtable.RowList{"v#1", "x#1"}, func(r bigtable.Row) bool {
		back = append(back, r.Key())
		return true
	}); err != nil || strings.Join(back, ",") != "v#1" {
		t.Errorf("ReadRows = %v, %v, want v#1 alone", back, err)
	}

	closed = true
	_ = c.Close()
	for _, method := range []string{"OpenTable", "OpenAuthorizedView"} {
		if bigtableCalls(t, endpoint, start, "/google.bigtable.v2.Bigtable/"+method, "") == 0 {
			t.Errorf("no %s session is logged", method)
		}
	}

	bigtableSessionsByHand(t, h, inst, table)
}

// bigtableSessionsByHand drives the protocol through the generated stub.
func bigtableSessionsByHand(t *testing.T, h *Harness, inst, table string) {
	t.Helper()
	ctx := h.Context()
	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d := bigtablepb.NewBigtableClient(conn)

	cfg, err := d.GetClientConfiguration(ctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: inst})
	if err != nil || cfg.GetSessionConfiguration().GetSessionLoad() != 1 || cfg.GetPollingConfiguration().GetPollingInterval().AsDuration() <= 0 {
		t.Fatalf("GetClientConfiguration = %v, %v, want session_load 1 and a polling interval", cfg, err)
	}
	if _, err := d.GetClientConfiguration(ctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: "cb-instance"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetClientConfiguration of a malformed name = %v, want InvalidArgument", err)
	}

	type sessionStream interface {
		Send(*bigtablepb.SessionRequest) error
		Recv() (*bigtablepb.SessionResponse, error)
	}
	open := func(s sessionStream, open proto.Message) {
		t.Helper()
		payload, _ := proto.Marshal(open)
		if err := s.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_OpenSession{OpenSession: &bigtablepb.OpenSessionRequest{ProtocolVersion: 1, Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		if r, err := s.Recv(); err != nil || r.GetOpenSession() == nil {
			t.Fatalf("the open = %v, %v", r, err)
		}
		if r, err := s.Recv(); err != nil || r.GetSessionParameters().GetKeepAlive().AsDuration() <= 0 {
			t.Fatalf("the session parameters = %v, %v", r, err)
		}
	}
	vrpc := func(s sessionStream, id int64, req proto.Message) *bigtablepb.SessionResponse {
		t.Helper()
		payload, _ := proto.Marshal(req)
		if err := s.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_VirtualRpc{VirtualRpc: &bigtablepb.VirtualRpcRequest{RpcId: id, Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		for {
			r, err := s.Recv()
			if err != nil {
				t.Fatalf("vRPC %d: %v", id, err)
			}
			if r.GetHeartbeat() == nil {
				return r
			}
		}
	}

	w, err := d.OpenTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open(w, &bigtablepb.OpenTableRequest{TableName: table, Permission: bigtablepb.OpenTableRequest_PERMISSION_WRITE})
	set := &bigtablepb.Mutation{Mutation: &bigtablepb.Mutation_SetCell_{SetCell: &bigtablepb.Mutation_SetCell{
		FamilyName: "info", ColumnQualifier: []byte("q"), TimestampMicros: 5000, Value: []byte("by-hand")}}}
	if r := vrpc(w, 1, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_MutateRow{MutateRow: &bigtablepb.SessionMutateRowRequest{
		Key: []byte("hand"), Mutations: []*bigtablepb.Mutation{set}}}}); r.GetVirtualRpc().GetRpcId() != 1 {
		t.Errorf("MutateRow vRPC = %v", r)
	}
	if r := vrpc(w, 2, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("hand")}}}); r.GetError().GetStatus().GetCode() != int32(codes.PermissionDenied) || r.GetError().GetRpcId() != 2 {
		t.Errorf("ReadRow on a write session = %v, want PERMISSION_DENIED", r)
	}
	_ = w.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_CloseSession{CloseSession: &bigtablepb.CloseSessionRequest{}}})
	if _, err := w.Recv(); err != io.EOF {
		t.Errorf("after CloseSession = %v, want the stream ended", err)
	}

	rd, err := d.OpenTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open(rd, &bigtablepb.OpenTableRequest{TableName: table, Permission: bigtablepb.OpenTableRequest_PERMISSION_READ})
	var tr bigtablepb.TableResponse
	r := vrpc(rd, 1, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("hand")}}})
	if err := proto.Unmarshal(r.GetVirtualRpc().GetPayload(), &tr); err != nil {
		t.Fatal(err)
	}
	if row := tr.GetReadRow().GetRow(); string(row.GetKey()) != "hand" || len(row.GetFamilies()) != 1 ||
		string(row.GetFamilies()[0].GetColumns()[0].GetCells()[0].GetValue()) != "by-hand" {
		t.Errorf("ReadRow vRPC = %v", r)
	}

	mv, err := d.OpenMaterializedView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open(mv, &bigtablepb.OpenMaterializedViewRequest{MaterializedViewName: inst + "/materializedViews/mv", Permission: bigtablepb.OpenMaterializedViewRequest_PERMISSION_READ})
	r = vrpc(mv, 1, &bigtablepb.MaterializedViewRequest{Payload: &bigtablepb.MaterializedViewRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("k")}}})
	if r.GetError().GetStatus().GetCode() != int32(codes.Unimplemented) || !strings.Contains(r.GetError().GetStatus().GetMessage(), "materialized view") {
		t.Errorf("ReadRow of a materialized view = %v, want UNIMPLEMENTED naming why", r)
	}
	t.Logf("a materialized view's ReadRow: %v", r.GetError().GetStatus())

	bad, err := d.OpenTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_VirtualRpc{VirtualRpc: &bigtablepb.VirtualRpcRequest{}}})
	if _, err := bad.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a session that does not open = %v, want InvalidArgument", err)
	}
}

// bigtableCall is one call of the Bigtable front's call log.
type bigtableCall struct {
	Seq      uint64 `json:"seq"`
	Method   string `json:"method"`
	Resource string `json:"resource"`
}

// bigtableCallsAfter reads the front's call log after seq.
func bigtableCallsAfter(t *testing.T, endpoint string, seq uint64) []bigtableCall {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/_cloudburrow/calls?after=%d", endpoint, seq))
	if err != nil {
		t.Fatalf("the call log: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Calls []bigtableCall `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the call log: %v", err)
	}
	return body.Calls
}

// bigtableLastCall is the number of the front's last logged call.
func bigtableLastCall(t *testing.T, endpoint string) uint64 {
	t.Helper()
	calls := bigtableCallsAfter(t, endpoint, 0)
	if len(calls) == 0 {
		return 0
	}
	return calls[len(calls)-1].Seq
}

// bigtableCalls counts the logged calls of method after seq naming
// resource ("" for any).
func bigtableCalls(t *testing.T, endpoint string, seq uint64, method, resource string) int {
	t.Helper()
	n := 0
	for _, c := range bigtableCallsAfter(t, endpoint, seq) {
		if c.Method == method && (resource == "" || c.Resource == resource) {
			n++
		}
	}
	return n
}

// bigtableRestarts returns a function that reads the restart counts of the
// Bigtable pod's containers, or one that always answers "unknown" without
// the instance's kubeconfig and kubectl.
func bigtableRestarts(t *testing.T) func() string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if _, err := exec.LookPath("kubectl"); kubeconfig == "" || err != nil {
		t.Logf("no %s or kubectl: the pod's restart count is not read", envKubeconfig)
		return func() string { return "unknown" }
	}
	return func() string {
		t.Helper()
		out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A",
			"-l", "app=bigtable,cloudburrow.dev/owned=true",
			"-o", "jsonpath={.items[*].metadata.name} {.items[*].status.containerStatuses[*].restartCount}").Output()
		if err != nil {
			t.Fatalf("kubectl get the Bigtable pod: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
}
