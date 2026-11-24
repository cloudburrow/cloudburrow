package alloydb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	connectorspb "cloud.google.com/go/alloydb/connectors/apiv1alpha/connectorspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakePG is a PostgreSQL wire-protocol server that answers every
// statement, records them, and returns rows for the two queries the
// service reads: pg_database and SELECT 1.
type fakePG struct {
	ln      net.Listener
	mu      sync.Mutex
	queries []string
}

func newFakePG(t *testing.T) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePG{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakePG) serve(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			f.mu.Lock()
			f.queries = append(f.queries, m.String)
			f.mu.Unlock()
			switch {
			case strings.Contains(m.String, "pg_database"):
				text := func(n string) pgproto3.FieldDescription {
					return pgproto3.FieldDescription{Name: []byte(n), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1}
				}
				be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{text("datname"), text("charset"), text("datcollate"), text("datctype"),
					{Name: []byte("datistemplate"), DataTypeOID: 16, DataTypeSize: 1, TypeModifier: -1}}})
				be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("postgres"), []byte("UTF8"), []byte("C.UTF-8"), []byte("C.UTF-8"), []byte("f")}})
				be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("template1"), []byte("UTF8"), []byte("C.UTF-8"), []byte("C.UTF-8"), []byte("t")}})
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
			case strings.HasPrefix(strings.ToUpper(strings.TrimSpace(m.String)), "SELECT"):
				be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("?column?"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}})
				be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			default:
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if be.Flush() != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		}
	}
}

func (f *fakePG) saw(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

// fakeEngine runs no server: every started server answers on the fake
// PostgreSQL, and dumps are marker files.
type fakeEngine struct {
	pg      *fakePG
	pool    *fakePG
	mu      sync.Mutex
	running map[string]ServerSpec
	calls   []string
}

func (e *fakeEngine) record(s string) {
	e.mu.Lock()
	e.calls = append(e.calls, s)
	e.mu.Unlock()
}

func (e *fakeEngine) called(prefix string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (e *fakeEngine) Start(_ context.Context, spec ServerSpec) error {
	e.mu.Lock()
	e.running[spec.Server] = spec
	e.mu.Unlock()
	e.record("start " + spec.Server)
	return nil
}
func (e *fakeEngine) Stop(_ context.Context, server string) error {
	e.mu.Lock()
	delete(e.running, server)
	e.mu.Unlock()
	e.record("stop " + server)
	return nil
}
func (e *fakeEngine) Restart(_ context.Context, server string) error {
	e.record("restart " + server)
	return nil
}
func (e *fakeEngine) Remove(_ context.Context, server string) error {
	e.mu.Lock()
	delete(e.running, server)
	e.mu.Unlock()
	e.record("remove " + server)
	return nil
}
func (e *fakeEngine) Addr(server string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.running[server]; !ok {
		return ""
	}
	return e.pg.ln.Addr().String()
}
func (e *fakeEngine) Dump(_ context.Context, server, dir string) error {
	e.record("dump " + server)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "dump.sql"), []byte("-- "+server), 0o644)
}
func (e *fakeEngine) Load(_ context.Context, server, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "dump.sql")); err != nil {
		return err
	}
	e.record("load " + server)
	return nil
}
func (e *fakeEngine) Promote(_ context.Context, server string) error {
	e.record("promote " + server)
	return nil
}
func (e *fakeEngine) PoolerAddr(server string, connectors bool) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if spec, ok := e.running[server]; !ok || spec.Pooler == nil {
		return ""
	}
	if connectors {
		return e.pool.ln.Addr().String()
	}
	return e.pool.ln.Addr().String()
}
func (e *fakeEngine) PITRBase(_ context.Context, server string) error {
	e.record("base " + server)
	return nil
}
func (e *fakeEngine) RecoverTo(_ context.Context, server string, at time.Time, dir string) error {
	e.record("recover " + server + " " + at.UTC().Format(time.RFC3339))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "dump.sql"), []byte("-- recovered"), 0o644)
}

type fixture struct {
	s   *Server
	eng *fakeEngine
	pg  *fakePG
	dir string
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	pg := newFakePG(t)
	eng := &fakeEngine{pg: pg, pool: newFakePG(t), running: map[string]ServerSpec{}}
	dir := t.TempDir()
	opts.Engine = eng
	opts.BackupsDir = filepath.Join(dir, "backups")
	if opts.ConnectorHost == "" {
		opts.ConnectorHost = "127.0.0.1"
	}
	s, err := NewServer(filepath.Join(dir, "alloydb.json"), filepath.Join(dir, "ca.json"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Wait)
	return &fixture{s: s, eng: eng, pg: pg, dir: dir}
}

const (
	loc     = "projects/p1/locations/us-central1"
	cluster = loc + "/clusters/c1"
	network = "projects/p1/global/networks/default"
)

func wait(t *testing.T, s *Server, op *longrunningpb.Operation, err error) *longrunningpb.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	got, err := s.Operation(op.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetDone() {
		t.Fatalf("operation %s is not done", got.GetName())
	}
	if e := got.GetError(); e != nil {
		t.Fatalf("operation %s failed: %s", got.GetName(), e.GetMessage())
	}
	return got
}

func code(err error) codes.Code { return status.Code(err) }

func (f *fixture) cluster(t *testing.T, pitr bool) {
	t.Helper()
	c := &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser: &alloydbpb.UserPassword{User: "postgres", Password: "pw"}}
	if !pitr {
		c.ContinuousBackupConfig = &alloydbpb.ContinuousBackupConfig{Enabled: proto.Bool(false)}
	}
	op, err := f.s.CreateCluster(context.Background(), &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "c1", Cluster: c})
	wait(t, f.s, op, err)
}

func (f *fixture) primary(t *testing.T) {
	t.Helper()
	op, err := f.s.CreateInstance(context.Background(), &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "primary",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	wait(t, f.s, op, err)
}

func TestClusterLifecycle(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	// A cluster needs a network or PSC.
	if _, err := f.s.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "c1", Cluster: &alloydbpb.Cluster{}}); code(err) != codes.InvalidArgument {
		t.Fatalf("no network: %v", err)
	}
	if _, err := f.s.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "Bad_ID",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	f.cluster(t, true)
	c, err := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster})
	if err != nil {
		t.Fatal(err)
	}
	if c.GetState() != alloydbpb.Cluster_READY || c.GetDatabaseVersion() == alloydbpb.DatabaseVersion_DATABASE_VERSION_UNSPECIFIED ||
		!c.GetContinuousBackupConfig().GetEnabled() || c.GetUid() == "" {
		t.Fatalf("cluster = %v", c)
	}
	if _, err := f.s.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "c1",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}}); code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate: %v", err)
	}
	list, err := f.s.ListClusters(ctx, &alloydbpb.ListClustersRequest{Parent: "projects/p1/locations/-"})
	if err != nil || len(list.GetClusters()) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	op, err := f.s.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: cluster, DisplayName: "Orders"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	wait(t, f.s, op, err)
	if c, _ := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster}); c.GetDisplayName() != "Orders" {
		t.Fatalf("display name = %q", c.GetDisplayName())
	}
	f.primary(t)
	// A cluster with instances is deleted only with force.
	if _, err := f.s.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: cluster}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete without force: %v", err)
	}
	op, err = f.s.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: cluster, Force: true})
	wait(t, f.s, op, err)
	if _, err := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster}); code(err) != codes.NotFound {
		t.Fatalf("after delete: %v", err)
	}
	if !f.eng.called("remove ") {
		t.Fatal("the primary's server was not removed")
	}
}

func TestInstancesAndReadPools(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.cluster(t, false)
	pool := &alloydbpb.Instance{InstanceType: alloydbpb.Instance_READ_POOL, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
		ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 2}}
	if _, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "pool", Instance: pool}); code(err) != codes.FailedPrecondition {
		t.Fatalf("read pool before primary: %v", err)
	}
	if _, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "p",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 3}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("3 vCPUs: %v", err)
	}
	if _, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "p",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
			DatabaseFlags: map[string]string{"no_such_flag": "1"}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("unknown flag: %v", err)
	}
	f.primary(t)
	if !f.pg.saw(SuperuserRole) {
		t.Fatal("the primary was not given the alloydbsuperuser role")
	}
	if _, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "second",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}}); code(err) != codes.AlreadyExists && code(err) != codes.FailedPrecondition {
		t.Fatalf("second primary: %v", err)
	}
	op, err := f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "pool", Instance: pool})
	wait(t, f.s, op, err)
	in, err := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: cluster + "/instances/pool"})
	if err != nil || in.GetState() != alloydbpb.Instance_READY || in.GetReadPoolConfig().GetNodeCount() != 2 {
		t.Fatalf("pool = %v, %v", in, err)
	}
	f.eng.mu.Lock()
	spec := f.eng.running[ServerOf(in.GetName())]
	f.eng.mu.Unlock()
	if spec.ReplicaOf != ServerOf(cluster+"/instances/primary") {
		t.Fatalf("read pool replicates %q", spec.ReplicaOf)
	}
	// The primary is not deleted while a read pool depends on it.
	if _, err := f.s.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: cluster + "/instances/primary"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete primary with pool: %v", err)
	}
	op, err = f.s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: in.GetName(),
		ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 3}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"read_pool_config.node_count"}}})
	wait(t, f.s, op, err)
	if in, _ := f.s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: in.GetName()}); in.GetReadPoolConfig().GetNodeCount() != 3 {
		t.Fatalf("node count = %d", in.GetReadPoolConfig().GetNodeCount())
	}
	op, err = f.s.RestartInstance(ctx, &alloydbpb.RestartInstanceRequest{Name: cluster + "/instances/primary"})
	wait(t, f.s, op, err)
	if !f.eng.called("restart ") {
		t.Fatal("restart did not restart the server")
	}
	op, err = f.s.FailoverInstance(ctx, &alloydbpb.FailoverInstanceRequest{Name: cluster + "/instances/primary"})
	wait(t, f.s, op, err)
	list, err := f.s.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: loc + "/clusters/-"})
	if err != nil || len(list.GetInstances()) != 2 {
		t.Fatalf("instances = %v, %v", list, err)
	}
	info, err := f.s.GetConnectionInfo(ctx, &alloydbpb.GetConnectionInfoRequest{Parent: cluster + "/instances/primary"})
	if err != nil || info.GetIpAddress() != "127.0.0.1" || info.GetInstanceUid() == "" {
		t.Fatalf("connection info = %v, %v", info, err)
	}
	flags, err := f.s.ListSupportedDatabaseFlags(ctx, &alloydbpb.ListSupportedDatabaseFlagsRequest{Parent: loc})
	if err != nil || len(flags.GetSupportedDatabaseFlags()) == 0 {
		t.Fatalf("flags = %v, %v", flags, err)
	}
}

func TestUsersAndDatabases(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.cluster(t, false)
	if _, err := f.s.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: cluster, UserId: "app",
		User: &alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_BUILT_IN, Password: "x"}}); code(err) != codes.FailedPrecondition {
		t.Fatalf("user without primary: %v", err)
	}
	f.primary(t)
	u, err := f.s.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: cluster, UserId: "app",
		User: &alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_BUILT_IN, Password: "secret", DatabaseRoles: []string{SuperuserRole}}})
	if err != nil {
		t.Fatal(err)
	}
	if u.GetPassword() != "" {
		t.Fatal("the password is returned")
	}
	if !f.pg.saw(`CREATE ROLE "app"`) {
		t.Fatalf("no CREATE ROLE: %v", f.pg.queries)
	}
	if _, err := f.s.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: cluster, UserId: "sa@p1.iam.gserviceaccount.com",
		User: &alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_IAM_USER}}); err != nil {
		t.Fatal(err)
	}
	if !f.pg.saw(`"sa@p1.iam"`) {
		t.Fatal("the IAM user's role is not the email without .gserviceaccount.com")
	}
	list, err := f.s.ListUsers(ctx, &alloydbpb.ListUsersRequest{Parent: cluster})
	if err != nil || len(list.GetUsers()) != 3 {
		t.Fatalf("users = %v, %v", list, err)
	}
	if _, err := f.s.DeleteUser(ctx, &alloydbpb.DeleteUserRequest{Name: cluster + "/users/app"}); err != nil {
		t.Fatal(err)
	}
	if !f.pg.saw(`DROP ROLE IF EXISTS "app"`) {
		t.Fatal("no DROP ROLE")
	}
	dbs, err := f.s.ListDatabases(ctx, &alloydbpb.ListDatabasesRequest{Parent: cluster})
	if err != nil || len(dbs.GetDatabases()) == 0 || dbs.GetDatabases()[0].GetCharset() != "UTF8" {
		t.Fatalf("databases = %v, %v", dbs, err)
	}
	res, err := f.s.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: cluster + "/instances/primary", Database: "postgres", User: "postgres",
		SqlStatement: "SELECT 1", UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "pw"}})
	if err != nil || len(res.GetSqlResults()) != 1 || len(res.GetSqlResults()[0].GetRows()) != 1 {
		t.Fatalf("ExecuteSql = %v, %v", res, err)
	}
}

func TestBackupRestoreAndPITR(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	f := newFixture(t, Options{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }})
	ctx := context.Background()
	f.cluster(t, true)
	f.primary(t)
	if !f.eng.called("base ") {
		t.Fatal("continuous backup took no base")
	}
	op, err := f.s.CreateBackup(ctx, &alloydbpb.CreateBackupRequest{Parent: loc, BackupId: "b1", Backup: &alloydbpb.Backup{ClusterName: cluster}})
	wait(t, f.s, op, err)
	b, err := f.s.GetBackup(ctx, &alloydbpb.GetBackupRequest{Name: loc + "/backups/b1"})
	if err != nil || b.GetState() != alloydbpb.Backup_READY || b.GetType() != alloydbpb.Backup_ON_DEMAND || b.GetSizeBytes() == 0 {
		t.Fatalf("backup = %v, %v", b, err)
	}
	op, err = f.s.RestoreCluster(ctx, &alloydbpb.RestoreClusterRequest{Parent: loc, ClusterId: "restored",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}},
		Source:  &alloydbpb.RestoreClusterRequest_BackupSource{BackupSource: &alloydbpb.BackupSource{BackupName: b.GetName()}}})
	wait(t, f.s, op, err)
	op, err = f.s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: loc + "/clusters/restored", InstanceId: "p",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	wait(t, f.s, op, err)
	if !f.eng.called("load " + ServerOf(loc+"/clusters/restored/instances/p")) {
		t.Fatal("the restored cluster's primary was not loaded from the backup")
	}

	clock.Lock()
	now = now.Add(time.Minute)
	clock.Unlock()
	c, _ := f.s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: cluster})
	earliest := c.GetContinuousBackupInfo().GetEarliestRestorableTime()
	if earliest == nil {
		t.Fatal("no earliest restorable time")
	}
	// Before the earliest restorable time is refused.
	if _, err := f.s.RestoreCluster(ctx, &alloydbpb.RestoreClusterRequest{Parent: loc, ClusterId: "early",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}},
		Source: &alloydbpb.RestoreClusterRequest_ContinuousBackupSource{ContinuousBackupSource: &alloydbpb.ContinuousBackupSource{Cluster: cluster,
			PointInTime: timestamppb.New(earliest.AsTime().Add(-time.Hour))}}}); code(err) != codes.InvalidArgument && code(err) != codes.FailedPrecondition {
		t.Fatalf("before the window: %v", err)
	}
	op, err = f.s.RestoreCluster(ctx, &alloydbpb.RestoreClusterRequest{Parent: loc, ClusterId: "pitr",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}},
		Source:  &alloydbpb.RestoreClusterRequest_ContinuousBackupSource{ContinuousBackupSource: &alloydbpb.ContinuousBackupSource{Cluster: cluster, PointInTime: earliest}}})
	wait(t, f.s, op, err)
	if !f.eng.called("recover " + ServerOf(cluster+"/instances/primary")) {
		t.Fatal("the point-in-time restore did not recover the source")
	}

	// Automated backups follow the weekly schedule.
	op, err = f.s.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: cluster,
		AutomatedBackupPolicy: &alloydbpb.AutomatedBackupPolicy{Enabled: proto.Bool(true),
			Schedule: &alloydbpb.AutomatedBackupPolicy_WeeklySchedule_{WeeklySchedule: &alloydbpb.AutomatedBackupPolicy_WeeklySchedule{
				StartTimes: []*timeofday.TimeOfDay{{Hours: 13}}, DaysOfWeek: []dayofweek.DayOfWeek{dayofweek.DayOfWeek_TUESDAY}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"automated_backup_policy"}}})
	wait(t, f.s, op, err)
	clock.Lock()
	now = now.Add(25 * time.Hour)
	clock.Unlock()
	f.s.RunAutomatedBackups(now)
	f.s.Wait()
	list, err := f.s.ListBackups(ctx, &alloydbpb.ListBackupsRequest{Parent: loc})
	if err != nil {
		t.Fatal(err)
	}
	auto := 0
	for _, b := range list.GetBackups() {
		if b.GetType() == alloydbpb.Backup_AUTOMATED {
			auto++
		}
	}
	if auto == 0 {
		t.Fatalf("no automated backup: %v", list)
	}
	op, err = f.s.DeleteBackup(ctx, &alloydbpb.DeleteBackupRequest{Name: loc + "/backups/b1"})
	wait(t, f.s, op, err)
}

func TestPersistence(t *testing.T) {
	f := newFixture(t, Options{})
	f.cluster(t, false)
	s2, err := NewServer(filepath.Join(f.dir, "alloydb.json"), filepath.Join(f.dir, "ca.json"), Options{Engine: f.eng})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetCluster(context.Background(), &alloydbpb.GetClusterRequest{Name: cluster}); err != nil {
		t.Fatalf("after reload: %v", err)
	}
	if !bytes.Equal(s2.ca.cert.Raw, f.s.ca.cert.Raw) {
		t.Fatal("the certificate authority changed across a reload")
	}
}

// TestRESTVersions drives v1, v1beta and v1alpha over JSON, as the
// connectors (v1alpha) and gcloud beta/alpha do.
func TestRESTVersions(t *testing.T) {
	f := newFixture(t, Options{})
	srv := httptest.NewServer(NewRESTHandler(f.s))
	defer srv.Close()
	f.cluster(t, false)
	f.primary(t)
	for _, v := range Versions {
		resp, err := http.Get(srv.URL + "/" + v + "/" + cluster)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if resp.StatusCode != 200 || got["name"] != cluster {
			t.Fatalf("%s GET cluster = %d %v", v, resp.StatusCode, got)
		}
		resp, err = http.Get(srv.URL + "/" + v + "/" + cluster + "/instances/primary/connectionInfo")
		if err != nil {
			t.Fatal(err)
		}
		got = nil
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if resp.StatusCode != 200 || got["ipAddress"] != "127.0.0.1" {
			t.Fatalf("%s connectionInfo = %d %v", v, resp.StatusCode, got)
		}
	}
	// An operation started through v1beta reports v1beta's types.
	body := `{"displayName":"x"}`
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1beta/"+cluster+"?updateMask=displayName", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var op map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&op)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("v1beta PATCH = %d %v", resp.StatusCode, op)
	}
	f.s.Wait()
	resp, err = http.Get(srv.URL + "/v1beta/" + op["name"].(string))
	if err != nil {
		t.Fatal(err)
	}
	op = nil
	_ = json.NewDecoder(resp.Body).Decode(&op)
	resp.Body.Close()
	r, _ := op["response"].(map[string]any)
	if op["done"] != true || r["@type"] != "type.googleapis.com/google.cloud.alloydb.v1beta.Cluster" {
		t.Fatalf("v1beta operation = %v", op)
	}
	// An export with no database is refused (INVALID_ARGUMENT, 400).
	resp, err = http.Post(srv.URL+"/v1/"+cluster+":export", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("export = %d", resp.StatusCode)
	}
}

func clientCert(t *testing.T, s *Server, mdx bool) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	resp, err := s.GenerateClientCertificate(context.Background(), &alloydbpb.GenerateClientCertificateRequest{Parent: cluster,
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), UseMetadataExchange: mdx})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetPemCertificateChain()) != 2 || resp.GetCaCert() == "" {
		t.Fatalf("chain = %v", resp)
	}
	var cert tls.Certificate
	for _, p := range resp.GetPemCertificateChain() {
		b, _ := pem.Decode([]byte(p))
		cert.Certificate = append(cert.Certificate, b.Bytes)
	}
	cert.PrivateKey = key
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(resp.GetCaCert()))
	return cert, pool
}

// dialConnector opens a connection through the connector as the
// connectors do: TLS 1.3 with the client certificate, then the metadata
// exchange when mdx.
func dialConnector(t *testing.T, addr, serverName string, cert tls.Certificate, pool *x509.CertPool, mdx *connectorspb.MetadataExchangeRequest) (net.Conn, *connectorspb.MetadataExchangeResponse) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: serverName})
	if err != nil {
		t.Fatal(err)
	}
	if mdx == nil {
		return conn, nil
	}
	b, _ := proto.Marshal(mdx)
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	if _, err := conn.Write(append(out, b...)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		t.Fatal(err)
	}
	rb := make([]byte, binary.BigEndian.Uint32(n[:]))
	if _, err := io.ReadFull(r, rb); err != nil {
		t.Fatal(err)
	}
	var resp connectorspb.MetadataExchangeResponse
	if err := proto.Unmarshal(rb, &resp); err != nil {
		t.Fatal(err)
	}
	return conn, &resp
}

func query(t *testing.T, conn net.Conn) {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close(ctx)
	var one int
	if err := pc.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 = %d, %v", one, err)
	}
}

func TestConnector(t *testing.T) {
	tokens := map[string]string{"tok": "sa@p1.iam.gserviceaccount.com"}
	f := newFixture(t, Options{IAMToken: func(tok string) (string, bool) { e, ok := tokens[tok]; return e, ok }})
	f.cluster(t, false)
	f.primary(t)
	conn, err := f.s.ServeConnector("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The v1 connectors' path: metadata exchange, built-in auth, to the
	// primary.
	cert, pool := clientCert(t, f.s, true)
	c, resp := dialConnector(t, conn.Addr(), "127.0.0.1", cert, pool, &connectorspb.MetadataExchangeRequest{UserAgent: "test",
		AuthType: connectorspb.MetadataExchangeRequest_DB_NATIVE})
	if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_OK {
		t.Fatalf("mdx = %v", resp)
	}
	query(t, c)

	// Without metadata exchange (the Auth Proxy's older protocol).
	cert2, _ := clientCert(t, f.s, false)
	c2, _ := dialConnector(t, conn.Addr(), "127.0.0.1", cert2, pool, nil)
	query(t, c2)

	// AUTO_IAM: an unknown token, and a known one with no IAM user.
	_, resp = dialConnector(t, conn.Addr(), "127.0.0.1", cert, pool, &connectorspb.MetadataExchangeRequest{
		AuthType: connectorspb.MetadataExchangeRequest_AUTO_IAM, Oauth2Token: "nope"})
	if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_ERROR {
		t.Fatalf("bad token = %v", resp)
	}
	_, resp = dialConnector(t, conn.Addr(), "127.0.0.1", cert, pool, &connectorspb.MetadataExchangeRequest{
		AuthType: connectorspb.MetadataExchangeRequest_AUTO_IAM, Oauth2Token: "tok"})
	if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_ERROR {
		t.Fatalf("no IAM user = %v", resp)
	}
	if _, err := f.s.CreateUser(context.Background(), &alloydbpb.CreateUserRequest{Parent: cluster, UserId: "sa@p1.iam.gserviceaccount.com",
		User: &alloydbpb.User{UserType: alloydbpb.User_ALLOYDB_IAM_USER}}); err != nil {
		t.Fatal(err)
	}
	c3, resp := dialConnector(t, conn.Addr(), "127.0.0.1", cert, pool, &connectorspb.MetadataExchangeRequest{
		AuthType: connectorspb.MetadataExchangeRequest_AUTO_IAM, Oauth2Token: "tok"})
	if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_OK {
		t.Fatalf("IAM user = %v", resp)
	}
	c3.Close()

	// A read pool is reached by its PSC DNS name.
	op, err := f.s.CreateInstance(context.Background(), &alloydbpb.CreateInstanceRequest{Parent: cluster, InstanceId: "pool",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_READ_POOL, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
			ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 1}}})
	wait(t, f.s, op, err)
	c4, resp := dialConnector(t, conn.Addr(), PSCDNSName(cluster+"/instances/pool"), cert, pool, &connectorspb.MetadataExchangeRequest{
		AuthType: connectorspb.MetadataExchangeRequest_DB_NATIVE})
	if resp.GetResponseCode() != connectorspb.MetadataExchangeResponse_OK {
		t.Fatalf("read pool = %v", resp)
	}
	query(t, c4)

	// A certificate from another authority is refused.
	other, _ := NewServer("", "", Options{})
	op, _ = other.CreateCluster(context.Background(), &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "c1",
		Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}})
	other.Wait()
	_ = op
	foreign, _ := clientCert(t, other, true)
	tc, err := tls.Dial("tcp", conn.Addr(), &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{foreign}, RootCAs: pool, ServerName: "127.0.0.1"})
	if err == nil {
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = tc.Read(make([]byte, 1))
		tc.Close()
	}
	if err == nil {
		t.Fatal("a foreign client certificate was accepted")
	}
}
