package datamigration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// fakeBackend answers the queries a verification and a run send with a
// configurable source and records what was done to the target.
type fakeBackend struct {
	mu        sync.Mutex
	walLevel  string
	pglogical bool
	gtid      string
	migrated  map[string]bool
	paused    map[string]bool
	promoted  map[string]bool
	deleted   map[string]bool
	setvals   []string
	created   []string
	reach     bool
	// existing are the existing destinations' versions, by ID.
	existing map[string]string
	// demotedIDs, resets, releases and migrations record the calls.
	demotedIDs []string
	resets     map[string][]string
	releases   []string
	migrations map[string]Migration
	sources    map[string]Source
	// targetDBs are the existing MySQL destinations' user databases.
	targetDBs map[string][]string
}

func newFake() *fakeBackend {
	return &fakeBackend{walLevel: "logical", pglogical: true, gtid: "ON", reach: true,
		migrated: map[string]bool{}, paused: map[string]bool{}, promoted: map[string]bool{}, deleted: map[string]bool{},
		existing: map[string]string{}, resets: map[string][]string{}, migrations: map[string]Migration{}, sources: map[string]Source{},
		targetDBs: map[string][]string{}}
}

func (f *fakeBackend) CreateCloudSQL(_ context.Context, t Target, _ *dmspb.CloudSqlSettings, _ Source) (CloudSQLAddresses, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, t.ID)
	return CloudSQLAddresses{PublicIP: "10.0.0.5"}, nil
}

func (f *fakeBackend) CreateAlloyDB(_ context.Context, t Target, _ *dmspb.AlloyDbSettings) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, t.ID)
	return "POSTGRES_16", nil
}

func (f *fakeBackend) Delete(_ context.Context, t Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[t.ID] = true
	return nil
}

func (f *fakeBackend) QuerySource(_ context.Context, _ Target, src Source, db, q string) ([][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reach {
		return nil, &ConnError{Msg: "could not connect to server: Connection refused"}
	}
	if src.Password != "pw" {
		return nil, &ConnError{Auth: true, Msg: `password authentication failed for user "` + src.User + `"`}
	}
	switch {
	case strings.Contains(q, "server_version_num"):
		pre := "pglogical"
		if !f.pglogical {
			pre = ""
		}
		return [][]string{{"160004", f.walLevel, "10", "10", "10", pre, "f", "t"}}, nil
	case strings.Contains(q, "FROM pg_database"):
		return [][]string{{"app"}, {"postgres"}}, nil
	case strings.Contains(q, "pg_extension"):
		ext := "1"
		if !f.pglogical {
			ext = "0"
		}
		return [][]string{{ext, "t", "0", "0", "", "f"}}, nil
	case strings.Contains(q, "pg_sequences"):
		if db == "app" {
			return [][]string{{"public.orders_id_seq", "42"}}, nil
		}
		return nil, nil
	case strings.Contains(q, "@@GLOBAL.version"):
		return [][]string{{"8.4.3", f.gtid, "ROW", "1", "1"}}, nil
	case q == "SHOW GRANTS":
		return [][]string{{"GRANT ALL PRIVILEGES ON *.* TO `migrator`@`%`"}}, nil
	case strings.Contains(q, "engine <> 'InnoDB'"):
		return nil, nil
	case strings.Contains(q, "information_schema.schemata"):
		return [][]string{{"shop"}}, nil
	}
	return nil, nil
}

func (f *fakeBackend) QueryTarget(_ context.Context, t Target, db, q string) ([][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(q, "pg_subscription_rel"):
		if f.migrated[t.ID] {
			return [][]string{{"0", "1"}}, nil
		}
		return [][]string{{"0", "0"}}, nil
	case strings.Contains(q, "setval"):
		f.setvals = append(f.setvals, db+":"+q)
		return [][]string{{"42"}}, nil
	case strings.Contains(q, "FROM pg_database"):
		return [][]string{{"postgres"}}, nil
	case strings.Contains(q, "information_schema.schemata"):
		var out [][]string
		for _, d := range f.targetDBs[t.ID] {
			out = append(out, []string{d})
		}
		return out, nil
	}
	return [][]string{{"0"}}, nil
}

func (f *fakeBackend) Migrate(_ context.Context, t Target, src Source, m Migration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrated[t.ID] = true
	f.migrations[t.ID] = m
	f.sources[t.ID] = src
	return nil
}

func (f *fakeBackend) Existing(_ context.Context, t Target) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.existing[t.ID]
	if !ok {
		return "", &ConnError{Msg: "no such instance " + t.ID}
	}
	return v, nil
}

func (f *fakeBackend) Demote(_ context.Context, t Target, _ Source) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.demotedIDs = append(f.demotedIDs, t.ID)
	return nil
}

func (f *fakeBackend) Release(_ context.Context, src Source) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, src.Name)
	return nil
}

func (f *fakeBackend) Pause(_ context.Context, t Target, pause bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused[t.ID] = pause
	return nil
}

func (f *fakeBackend) Promote(_ context.Context, t Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted[t.ID] = true
	return nil
}

func (f *fakeBackend) Reset(_ context.Context, t Target, _ Source, dbs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.migrated, t.ID)
	f.resets[t.ID] = dbs
	return nil
}

const parent = "projects/p1/locations/us-central1"

func testServer(t *testing.T, state string, b Backend) *Server {
	t.Helper()
	s, err := NewServer(state, Options{Backend: b, CloudSQL: true, AlloyDB: true, Poll: 10 * time.Millisecond,
		CloudSQLVersions: func() []string { return []string{"POSTGRES_14", "POSTGRES_16", "POSTGRES_17", "MYSQL_8_4"} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// wait answers a function that waits for an operation and answers it.
func wait(t *testing.T, s *Server) func(op *longrunningpb.Operation, err error) *longrunningpb.Operation {
	return func(op *longrunningpb.Operation, err error) *longrunningpb.Operation {
		t.Helper()
		return waitOp(t, s, op, err)
	}
}

func waitOp(t *testing.T, s *Server, op *longrunningpb.Operation, err error) *longrunningpb.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	o := &operations{s: s}
	got, err := o.WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: op.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetDone() {
		t.Fatalf("operation %s not done", op.GetName())
	}
	return got
}

func mustOK(t *testing.T, op *longrunningpb.Operation) {
	t.Helper()
	if e := op.GetError(); e != nil {
		t.Fatalf("operation %s failed: %v", op.GetName(), e)
	}
}

func jobState(t *testing.T, s *Server, name string) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase) {
	t.Helper()
	j, err := s.GetMigrationJob(context.Background(), &dmspb.GetMigrationJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return j.State, j.Phase
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pgSource(id, password string) *dmspb.CreateConnectionProfileRequest {
	return &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: &dmspb.ConnectionProfile{
		ConnectionProfile: &dmspb.ConnectionProfile_Postgresql{Postgresql: &dmspb.PostgreSqlConnectionProfile{
			Host: "pgsrc", Port: 5432, Username: "migrator", Password: password}}}}
}

func cloudSQLDest(id, source, version string) *dmspb.CreateConnectionProfileRequest {
	return &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: &dmspb.ConnectionProfile{
		ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{
			SourceId: parent + "/connectionProfiles/" + source, Tier: "db-custom-1-3840", RootPassword: "root-pw",
			DatabaseVersion: map[string]dmspb.CloudSqlSettings_SqlDatabaseVersion{
				"POSTGRES_14": dmspb.CloudSqlSettings_POSTGRES_14}[version]}}}}}
}

func TestProfilesAndContinuousJob(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, filepath.Join(t.TempDir(), "dms.json"), f)

	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, pgSource("src", "pw"))))
	got, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/src"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dmspb.ConnectionProfile_READY || got.GetPostgresql().GetPassword() != "" || !got.GetPostgresql().GetPasswordSet() {
		t.Fatalf("source profile: %v", got)
	}

	// A Cloud SQL destination names its version by name (gcloud's
	// --database-version-name).
	dst := cloudSQLDest("dst", "src", "")
	setStr(dst.ConnectionProfile.GetCloudsql().GetSettings(), "database_version_name", "POSTGRES_16")
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, dst)))
	got, _ = s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/dst"})
	if got.State != dmspb.ConnectionProfile_READY || got.GetCloudsql().GetPublicIp() != "10.0.0.5" || got.GetCloudsql().GetSettings().GetRootPasswordSet() != true {
		t.Fatalf("destination profile: %v", got)
	}

	// The source is still referenced by the destination.
	if _, err := s.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/src"}); apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("delete a referenced source: %v", err)
	}

	job := parent + "/migrationJobs/job1"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "job1",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/src",
			Destination: parent + "/connectionProfiles/dst"}})))
	if st, _ := jobState(t, s, job); st != dmspb.MigrationJob_NOT_STARTED {
		t.Fatalf("new job is %v", st)
	}

	// Verification fails with Google's codes while the source is wrong.
	f.mu.Lock()
	f.walLevel = "replica"
	f.mu.Unlock()
	op := wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job}))
	if op.GetError().GetCode() != int32(codes.FailedPrecondition) || len(op.GetError().GetDetails()) != 1 {
		t.Fatalf("verify with wal_level replica: %v", op.GetError())
	}
	var ve dmspb.MigrationJobVerificationError
	if err := op.GetError().GetDetails()[0].UnmarshalTo(&ve); err != nil || ve.ErrorCode != dmspb.MigrationJobVerificationError_INVALID_WAL_LEVEL {
		t.Fatalf("verification detail: %v %v", &ve, err)
	}
	// Starting it verifies too, and leaves it NOT_STARTED.
	op = wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job}))
	if op.GetError() == nil {
		t.Fatal("start with a failing verification succeeded")
	}
	if st, _ := jobState(t, s, job); st != dmspb.MigrationJob_NOT_STARTED {
		t.Fatalf("after a failed start the job is %v", st)
	}
	f.mu.Lock()
	f.walLevel = "logical"
	f.mu.Unlock()
	mustOK(t, wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})))

	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})))
	eventually(t, "the CDC phase", func() bool {
		st, ph := jobState(t, s, job)
		return st == dmspb.MigrationJob_RUNNING && ph == dmspb.MigrationJob_CDC
	})

	// Resume is for a stopped job; stop pauses replication.
	if _, err := s.ResumeMigrationJob(ctx, &dmspb.ResumeMigrationJobRequest{Name: job}); apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("resume a running job: %v", err)
	}
	mustOK(t, wait(t, s)(s.StopMigrationJob(ctx, &dmspb.StopMigrationJobRequest{Name: job})))
	if st, ph := jobState(t, s, job); st != dmspb.MigrationJob_STOPPED || ph != dmspb.MigrationJob_CDC || !f.paused["dst"] {
		t.Fatalf("stopped job: %v %v paused=%v", st, ph, f.paused)
	}
	mustOK(t, wait(t, s)(s.ResumeMigrationJob(ctx, &dmspb.ResumeMigrationJobRequest{Name: job})))
	if st, _ := jobState(t, s, job); st != dmspb.MigrationJob_RUNNING || f.paused["dst"] {
		t.Fatalf("resumed job: %v", st)
	}

	// The state survives a restart of the server.
	s2 := testServer(t, s.state, f)
	if st, ph := jobState(t, s2, job); st != dmspb.MigrationJob_RUNNING || ph != dmspb.MigrationJob_CDC {
		t.Fatalf("reloaded job: %v %v", st, ph)
	}

	mustOK(t, wait(t, s)(s.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job})))
	j, _ := s.GetMigrationJob(ctx, &dmspb.GetMigrationJobRequest{Name: job})
	if j.State != dmspb.MigrationJob_COMPLETED || !f.promoted["dst"] || j.GetEndTime() == nil || j.GetDuration() == nil {
		t.Fatalf("promoted job: %v promoted=%v", j, f.promoted)
	}
	if len(f.setvals) != 1 || !strings.Contains(f.setvals[0], "'public.orders_id_seq', '42'") {
		t.Fatalf("sequences: %v", f.setvals)
	}

	// Deleting the job deletes its Cloud SQL destination's profile, and
	// with force the instance.
	mustOK(t, wait(t, s)(s.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job, Force: true})))
	if _, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/dst"}); apierror.From(err).Code != codes.NotFound {
		t.Fatalf("destination profile after the job's delete: %v", err)
	}
	if !f.deleted["dst"] {
		t.Fatal("force delete kept the replica")
	}
}

func TestVerificationErrors(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.pglogical = false
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, pgSource("src", "pw"))))
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, cloudSQLDest("dst", "src", "POSTGRES_14"))))
	job := parent + "/migrationJobs/j"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "j",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/src",
			Destination: parent + "/connectionProfiles/dst"}})))
	codesOf := func(op *longrunningpb.Operation) map[dmspb.MigrationJobVerificationError_ErrorCode]bool {
		out := map[dmspb.MigrationJobVerificationError_ErrorCode]bool{}
		for _, d := range op.GetError().GetDetails() {
			var v dmspb.MigrationJobVerificationError
			if d.UnmarshalTo(&v) == nil {
				out[v.ErrorCode] = true
			}
		}
		return out
	}
	got := codesOf(wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})))
	for _, c := range []dmspb.MigrationJobVerificationError_ErrorCode{errPreload, errNoPglogical, errVersion} {
		if !got[c] {
			t.Errorf("verify without pglogical to POSTGRES_14 from 16: missing %v in %v", c, got)
		}
	}

	// Native logical replication needs no pglogical.
	d2 := cloudSQLDest("dst2", "src", "")
	setStr(d2.ConnectionProfile.GetCloudsql().GetSettings(), "database_version_name", "POSTGRES_16")
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, d2)))
	native := &dmspb.MigrationJob{}
	if err := overlay.Unmarshal([]byte(`{"type":"CONTINUOUS","source":"`+parent+`/connectionProfiles/src","destination":"`+
		parent+`/connectionProfiles/dst2","postgresHomogeneousConfig":{"isNativeLogical":true}}`), native); err != nil {
		t.Fatal(err)
	}
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "native", MigrationJob: native})))
	mustOK(t, wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: parent + "/migrationJobs/native"})))

	// A wrong password is AUTHENTICATION_FAILURE, an unreachable host
	// CONNECTION_FAILURE.
	_, err := s.UpdateConnectionProfile(ctx, &dmspb.UpdateConnectionProfileRequest{
		ConnectionProfile: &dmspb.ConnectionProfile{Name: parent + "/connectionProfiles/src", ConnectionProfile: &dmspb.ConnectionProfile_Postgresql{
			Postgresql: &dmspb.PostgreSqlConnectionProfile{Password: "wrong"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"postgresql.password"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := codesOf(wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job}))); !got[errAuth] {
		t.Errorf("wrong password: %v", got)
	}
	f.mu.Lock()
	f.reach = false
	f.mu.Unlock()
	if got := codesOf(wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job}))); !got[errConnection] {
		t.Errorf("unreachable source: %v", got)
	}
}

func TestOneTimeMySQLToCloudSQL(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "my",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{
			Host: "mysrc", Port: 3306, Username: "migrator", Password: "pw"}}}})))
	// MYSQL_8_0 is not a version this instance serves.
	_, err := s.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "mydst",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{
			Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/my", Tier: "db-n1-standard-1",
				DatabaseVersion: dmspb.CloudSqlSettings_MYSQL_8_0}}}}})
	if apierror.From(err).Code != codes.Unimplemented {
		t.Fatalf("a destination of an unserved version: %v", err)
	}
	d := &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "mydst",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{
			Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/my", Tier: "db-n1-standard-1"}}}}}
	setStr(d.ConnectionProfile.GetCloudsql().GetSettings(), "database_version_name", "MYSQL_8_4")
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, d)))
	job := parent + "/migrationJobs/once"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "once",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_ONE_TIME, Source: parent + "/connectionProfiles/my",
			Destination: parent + "/connectionProfiles/mydst"}})))
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})))
	eventually(t, "COMPLETED", func() bool { st, _ := jobState(t, s, job); return st == dmspb.MigrationJob_COMPLETED })
	if !f.migrated["mydst"] || !f.promoted["mydst"] {
		t.Fatalf("one-time job: migrated=%v promoted=%v", f.migrated, f.promoted)
	}
	if _, err := s.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job}); apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("promote a completed one-time job: %v", err)
	}
}

func TestUnimplemented(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, "", newFake())
	if _, err := s.ListConversionWorkspaces(ctx, &dmspb.ListConversionWorkspacesRequest{Parent: parent}); apierror.From(err).Code != codes.Unimplemented {
		t.Fatalf("conversion workspaces: %v", err)
	}
	_, err := s.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "ora",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Oracle{Oracle: &dmspb.OracleConnectionProfile{
			Host: "o", Port: 1521, Username: "u", Password: "p", DatabaseService: "ORCL"}}}})
	if apierror.From(err).Code != codes.Unimplemented {
		t.Fatalf("an Oracle profile: %v", err)
	}
}

func TestREST(t *testing.T) {
	s := testServer(t, "", newFake())
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	if c, m := post("/v1/"+parent+"/connectionProfiles?connectionProfileId=src",
		`{"postgresql":{"host":"pgsrc","port":5432,"username":"migrator","password":"pw","ssl":{"type":"NONE"}}}`); c != 200 {
		t.Fatalf("create a profile with ssl NONE: %d %v", c, m)
	}
	if c, m := post("/v1/"+parent+"/connectionProfiles?connectionProfileId=dst",
		`{"cloudsql":{"settings":{"sourceId":"`+parent+`/connectionProfiles/src","tier":"db-custom-1-3840","databaseVersionName":"POSTGRES_17"}}}`); c != 200 {
		t.Fatalf("create a Cloud SQL destination: %d %v", c, m)
	}
	eventually(t, "the destination", func() bool {
		p, _ := s.GetConnectionProfile(context.Background(), &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/dst"})
		return p.GetState() == dmspb.ConnectionProfile_READY
	})
	if c, m := post("/v1/"+parent+"/migrationJobs?migrationJobId=j",
		`{"type":"CONTINUOUS","source":"`+parent+`/connectionProfiles/src","destination":"`+parent+`/connectionProfiles/dst",`+
			`"postgresHomogeneousConfig":{"isNativeLogical":true},"staticIpConnectivity":{}}`); c != 200 {
		t.Fatalf("create a job: %d %v", c, m)
	}
	resp, err := http.Get(srv.URL + "/v1/" + parent + "/migrationJobs/j")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"isNativeLogical":true`) || !strings.Contains(string(b), `"state":"NOT_STARTED"`) {
		t.Fatalf("GET the job: %s", b)
	}
	resp, err = http.Get(srv.URL + "/v1/" + parent + "/connectionProfiles/src")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"type":"NONE"`) || strings.Contains(string(b), `"password":`) {
		t.Fatalf("GET the source: %s", b)
	}
	// gcloud's bodies carry Google's v1 request fields: resume's
	// skipValidation (refused here only for the job's state), restart's
	// restartFailedObjects, and promote's failback, which is not run.
	if c, m := post("/v1/"+parent+"/migrationJobs/j:resume", `{"skipValidation":true}`); c != 400 || !strings.Contains(fmt.Sprint(m), "FAILED_PRECONDITION") {
		t.Errorf("resume a job not started, with skipValidation: %d %v", c, m)
	}
	if c, m := post("/v1/"+parent+"/migrationJobs/j:restart", `{"restartFailedObjects":true,"skipValidation":true}`); c != 400 || !strings.Contains(fmt.Sprint(m), "FAILED_PRECONDITION") {
		t.Errorf("restart a job not started: %d %v", c, m)
	}
	if c, m := post("/v1/"+parent+"/migrationJobs/j:promote", `{"failback":true}`); c != 501 {
		t.Errorf("promote with failback: %d %v", c, m)
	}
	// demoteDestination is served (#2257), for an existing destination.
	if c, m := post("/v1/"+parent+"/migrationJobs/j:demoteDestination", `{}`); c != 400 || !strings.Contains(fmt.Sprint(m), "only an existing destination") {
		t.Errorf("demote a created destination: %d %v", c, m)
	}
	if c, m := post("/v1/"+parent+"/conversionWorkspaces?conversionWorkspaceId=c", `{}`); c != 501 {
		t.Errorf("POST a conversion workspace: %d %v", c, m)
	}
	for _, p := range []string{"/v1/projects/p1/locations", "/v1/" + parent + "/operations", "/v1/" + parent + "/privateConnections"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
}
