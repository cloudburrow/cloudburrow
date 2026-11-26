package datamigration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func mysqlSource(id string) *dmspb.CreateConnectionProfileRequest {
	return &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: &dmspb.ConnectionProfile{
		ConnectionProfile: &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{
			Host: "mysrc", Port: 3306, Username: "migrator", Password: "pw"}}}}
}

// existingMySQL is gcloud's `connection-profiles create mysql ID
// --cloudsql-instance=INSTANCE --role=DESTINATION`.
func existingMySQL(id, instance string) *dmspb.CreateConnectionProfileRequest {
	p := &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{CloudSqlId: instance}}}
	setStr(p, "role", "DESTINATION")
	return &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: p}
}

func restCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func waitJSONOp(t *testing.T, s *Server, m map[string]any) map[string]any {
	t.Helper()
	name, _ := m["name"].(string)
	if name == "" {
		t.Fatalf("no operation: %v", m)
	}
	h := NewRESTHandler(s)
	for i := 0; i < 500; i++ {
		_, op := restCall(t, h, http.MethodGet, "/v1/"+name, "")
		if op["done"] == true {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s not done", name)
	return nil
}

func TestExistingCloudSQLDestination(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.existing["dest1"] = "MYSQL_8_4"
	f.targetDBs["dest1"] = []string{"leftover"}
	s := testServer(t, "", f)
	h := NewRESTHandler(s)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, mysqlSource("my"))))
	// An instance that does not exist is refused.
	if _, err := s.CreateConnectionProfile(ctx, existingMySQL("nodst", "missing")); err == nil {
		t.Fatal("a profile of a missing instance was created")
	}
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, existingMySQL("dst", "dest1"))))
	p, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/dst"})
	if err != nil || p.GetProvider() != dmspb.DatabaseProvider_CLOUDSQL || p.State != dmspb.ConnectionProfile_READY {
		t.Fatalf("the existing destination's profile: %v %v", p, err)
	}
	job := parent + "/migrationJobs/j"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "j",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/my",
			Destination: parent + "/connectionProfiles/dst"}})))
	// Not demoted, a start fails.
	op := wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job}))
	if op.GetError() == nil || !strings.Contains(op.GetError().GetMessage(), "demote it first") {
		t.Fatalf("a start before the demotion: %v", op.GetError())
	}
	// The destination holds user data: the demotion fails.
	c, m := restCall(t, h, http.MethodPost, "/v1/"+job+":demoteDestination", `{}`)
	if c != 200 {
		t.Fatalf("demoteDestination: %d %v", c, m)
	}
	if done := waitJSONOp(t, s, m); done["error"] == nil {
		t.Fatalf("a destination with user data was demoted: %v", done)
	}
	f.mu.Lock()
	f.targetDBs["dest1"] = nil
	f.mu.Unlock()
	_, m = restCall(t, h, http.MethodPost, "/v1/"+job+":demoteDestination", ``)
	if done := waitJSONOp(t, s, m); done["error"] != nil {
		t.Fatalf("demoteDestination: %v", done)
	}
	if len(f.demotedIDs) != 1 || f.demotedIDs[0] != "dest1" {
		t.Fatalf("demoted: %v", f.demotedIDs)
	}
	if c, _ := restCall(t, h, http.MethodPost, "/v1/"+job+":demoteDestination", `{}`); c != 400 {
		t.Fatalf("a second demotion: %d", c)
	}
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})))
	eventually(t, "CDC", func() bool { _, ph := jobState(t, s, job); return ph == dmspb.MigrationJob_CDC })
	// Deleted with force, the existing instance is promoted, not deleted.
	mustOK(t, wait(t, s)(s.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job, Force: true})))
	if f.deleted["dest1"] || !f.promoted["dest1"] {
		t.Fatalf("deleting the job: deleted=%v promoted=%v", f.deleted, f.promoted)
	}
}

func TestExistingAlloyDBDestination(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.existing["cl1"] = "POSTGRES_16"
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, pgSource("src", "pw"))))
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "dst",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Alloydb{Alloydb: &dmspb.AlloyDbConnectionProfile{ClusterId: "cl1"}}}})))
	job := parent + "/migrationJobs/j"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "j",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/src",
			Destination: parent + "/connectionProfiles/dst"}})))
	h := NewRESTHandler(s)
	_, m := restCall(t, h, http.MethodPost, "/v1/"+job+":demoteDestination", `{}`)
	if done := waitJSONOp(t, s, m); done["error"] != nil {
		t.Fatalf("demoteDestination: %v", done)
	}
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})))
	eventually(t, "migrated", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.migrated["cl1"] })
	// Restarted, its migration is undone and demoted again, not deleted.
	f.mu.Lock()
	f.migrated["cl1"] = true
	f.mu.Unlock()
	eventually(t, "CDC", func() bool { _, ph := jobState(t, s, job); return ph == dmspb.MigrationJob_CDC })
	mustOK(t, wait(t, s)(s.RestartMigrationJob(ctx, &dmspb.RestartMigrationJobRequest{Name: job})))
	f.mu.Lock()
	r := f.resets["cl1"]
	f.mu.Unlock()
	if len(r) != 2 || f.deleted["cl1"] {
		t.Fatalf("restart: resets=%v deleted=%v", f.resets, f.deleted)
	}
}

func TestObjectsAndSelection(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, "", f)
	h := NewRESTHandler(s)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, pgSource("src", "pw"))))
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, cloudSQLDest("dst", "src", "POSTGRES_14"))))
	c, m := restCall(t, h, http.MethodPost, "/v1/"+parent+"/migrationJobs?migrationJobId=j", `{"type":"CONTINUOUS",
		"source":"`+parent+`/connectionProfiles/src","destination":"`+parent+`/connectionProfiles/dst",
		"objectsConfig":{"sourceObjectsConfig":{"objectsSelectionType":"SPECIFIED_OBJECTS",
		  "objectConfigs":[{"objectIdentifier":{"database":"app","type":"DATABASE"}}]}}}`)
	if c != 200 {
		t.Fatalf("create a job selecting app: %d %v", c, m)
	}
	job := parent + "/migrationJobs/j"
	c, m = restCall(t, h, http.MethodGet, "/v1/"+job+":fetchSourceObjects", "")
	if c != 200 {
		t.Fatalf("fetchSourceObjects: %d %v", c, m)
	}
	if done := waitJSONOp(t, s, m); done["error"] != nil {
		t.Fatalf("fetchSourceObjects: %v", done)
	}
	c, m = restCall(t, h, http.MethodGet, "/v1/"+job+"/objects", "")
	objs, _ := m["migrationJobObjects"].([]any)
	if c != 200 || len(objs) != 2 {
		t.Fatalf("objects.list: %d %v", c, m)
	}
	states := map[string]string{}
	for _, o := range objs {
		o := o.(map[string]any)
		states[o["sourceObject"].(map[string]any)["database"].(string)] = o["state"].(string)
	}
	if states["app"] != "NOT_STARTED" || states["postgres"] != "NOT_SELECTED" {
		t.Fatalf("object states: %v", states)
	}
	_, one := restCall(t, h, http.MethodGet, "/v1/"+job+"/objects?pageSize=1", "")
	if one["nextPageToken"] == nil {
		t.Fatalf("a page of one: %v", one)
	}
	c, m = restCall(t, h, http.MethodPost, "/v1/"+job+"/objects:lookup", `{"sourceObjectIdentifier":{"database":"app","type":"DATABASE"}}`)
	if c != 200 || m["state"] != "NOT_STARTED" {
		t.Fatalf("objects.lookup: %d %v", c, m)
	}
	name := m["name"].(string)
	if c, got := restCall(t, h, http.MethodGet, "/v1/"+name, ""); c != 200 || got["name"] != name {
		t.Fatalf("objects.get: %d %v", c, got)
	}
	if c, _ := restCall(t, h, http.MethodPost, "/v1/"+job+"/objects:lookup", `{"sourceObjectIdentifier":{"database":"none"}}`); c != 404 {
		t.Fatalf("lookup of an unknown database: %d", c)
	}
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job, SkipValidation: true})))
	eventually(t, "migrated", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.migrated["dst"] })
	f.mu.Lock()
	got := f.migrations["dst"].Databases
	f.mu.Unlock()
	if len(got) != 1 || got[0] != "app" {
		t.Fatalf("migrated databases: %v", got)
	}
	_, m = restCall(t, h, http.MethodPost, "/v1/"+job+"/objects:lookup", `{"sourceObjectIdentifier":{"database":"app"}}`)
	if m["state"] != "RUNNING" {
		t.Fatalf("a running job's object: %v", m)
	}
	// Tables are selected only through conversion workspaces.
	c, _ = restCall(t, h, http.MethodPost, "/v1/"+parent+"/migrationJobs?migrationJobId=t", `{"type":"CONTINUOUS",
		"source":"`+parent+`/connectionProfiles/src","destination":"`+parent+`/connectionProfiles/dst",
		"objectsConfig":{"sourceObjectsConfig":{"objectsSelectionType":"SPECIFIED_OBJECTS",
		  "objectConfigs":[{"objectIdentifier":{"database":"app","schema":"public","table":"t","type":"TABLE"}}]}}}`)
	if c != 501 {
		t.Fatalf("a table selection: %d", c)
	}
}

func TestDumpFlagsAndPath(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, mysqlSource("my"))))
	d := &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "mydst",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{
			Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/my", Tier: "db-n1-standard-1"}}}}}
	setStr(d.ConnectionProfile.GetCloudsql().GetSettings(), "database_version_name", "MYSQL_8_4")
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, d)))
	mk := func(id string, j *dmspb.MigrationJob) error {
		j.Type, j.Source, j.Destination = dmspb.MigrationJob_ONE_TIME, parent+"/connectionProfiles/my", parent+"/connectionProfiles/mydst"
		_, err := s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: id, MigrationJob: j})
		return err
	}
	flags := func(kv ...string) *dmspb.MigrationJob_DumpFlags {
		out := &dmspb.MigrationJob_DumpFlags{}
		for i := 0; i < len(kv); i += 2 {
			out.DumpFlags = append(out.DumpFlags, &dmspb.MigrationJob_DumpFlag{Name: kv[i], Value: kv[i+1]})
		}
		return out
	}
	for name, j := range map[string]*dmspb.MigrationJob{
		"both":    {DumpPath: "gs://b/d.sql", DumpFlags: flags("add-locks", "")},
		"badpath": {DumpPath: "/tmp/d.sql"},
		"managed": {DumpFlags: flags("databases", "x")},
		"shell":   {DumpFlags: flags("max-allowed-packet", "1;rm")},
	} {
		if err := mk(name, j); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := mk("ok", &dmspb.MigrationJob{DumpFlags: flags("add-locks", "", "max-allowed-packet", "64M")}); err != nil {
		t.Fatal(err)
	}
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: parent + "/migrationJobs/ok"})))
	eventually(t, "migrated", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.migrated["mydst"] })
	f.mu.Lock()
	got := strings.Join(f.migrations["mydst"].DumpFlags, " ")
	f.mu.Unlock()
	if got != "--add-locks --max-allowed-packet=64M" {
		t.Fatalf("dump flags: %q", got)
	}
}

func TestGTIDOffContinuous(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.gtid = "OFF"
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, mysqlSource("my"))))
	d := &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "mydst",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{
			Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/my", Tier: "db-n1-standard-1"}}}}}
	setStr(d.ConnectionProfile.GetCloudsql().GetSettings(), "database_version_name", "MYSQL_8_4")
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, d)))
	job := parent + "/migrationJobs/j"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "j",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/my",
			Destination: parent + "/connectionProfiles/mydst"}})))
	if op := wait(t, s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})); op.GetError() != nil {
		t.Fatalf("a continuous job from a source without GTIDs: %v", op.GetError())
	}
}

func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}

func TestVerifiedSSLAndSSH(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, "", f)
	h := NewRESTHandler(s)
	ca, key := selfSigned(t)
	caJ, _ := json.Marshal(ca)
	keyJ, _ := json.Marshal(key)
	if c, m := restCall(t, h, http.MethodPost, "/v1/"+parent+"/connectionProfiles?connectionProfileId=bad", `{"postgresql":{"host":"h","port":5432,
		"username":"u","password":"pw","ssl":{"type":"SERVER_ONLY","caCertificate":"not a cert"}}}`); c != 400 {
		t.Fatalf("a bad CA: %d %v", c, m)
	}
	c, m := restCall(t, h, http.MethodPost, "/v1/"+parent+"/connectionProfiles?connectionProfileId=src", `{"postgresql":{"host":"pgsrc","port":5432,
		"username":"migrator","password":"pw","ssl":{"type":"SERVER_CLIENT","caCertificate":`+string(caJ)+`,"clientCertificate":`+string(caJ)+`,"clientKey":`+string(keyJ)+`},
		"forwardSshConnectivity":{"hostname":"bastion","username":"tunnel","password":"sshpw"}}}`)
	if c != 200 {
		t.Fatalf("a SERVER_CLIENT profile through SSH: %d %v", c, m)
	}
	c, m = restCall(t, h, http.MethodGet, "/v1/"+parent+"/connectionProfiles/src", "")
	b, _ := json.Marshal(m)
	if c != 200 || strings.Contains(string(b), "sshpw") || strings.Contains(string(b), "PRIVATE KEY") || !strings.Contains(string(b), `"hostname":"bastion"`) ||
		!strings.Contains(string(b), `"port":22`) && !strings.Contains(string(b), `"forwardSshConnectivity"`) {
		t.Fatalf("GET the profile: %s", b)
	}
	s.mu.Lock()
	src := sourceOf(s.profiles[parent+"/connectionProfiles/src"])
	s.mu.Unlock()
	if src.CACert != ca || src.ClientKey != key || src.SSH == nil || src.SSH.Port != 22 || src.SSH.Password != "sshpw" || src.SSH.Host != "bastion" {
		t.Fatalf("the source: %+v %+v", src, src.SSH)
	}
	// An update without the input-only secrets keeps them.
	c, m = restCall(t, h, http.MethodPatch, "/v1/"+parent+"/connectionProfiles/src?updateMask=postgresql.host", `{"postgresql":{"host":"pgsrc2"}}`)
	if c != 200 {
		t.Fatalf("update: %d %v", c, m)
	}
	waitJSONOp(t, s, m)
	s.mu.Lock()
	src = sourceOf(s.profiles[parent+"/connectionProfiles/src"])
	s.mu.Unlock()
	if src.Host != "pgsrc2" || src.CACert != ca || src.SSH == nil || src.SSH.Password != "sshpw" {
		t.Fatalf("after the update: %+v", src)
	}
	mustOK(t, wait(t, s)(s.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/src"})))
	eventually(t, "the tunnel released", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.releases) == 1 })
	_ = io.Discard
}

func TestReverseSSH(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := testServer(t, "", f)
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, pgSource("src", "pw"))))
	mustOK(t, wait(t, s)(s.CreateConnectionProfile(ctx, cloudSQLDest("dst", "src", "POSTGRES_14"))))
	job := parent + "/migrationJobs/j"
	mustOK(t, wait(t, s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "j",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/src",
			Destination: parent + "/connectionProfiles/dst",
			Connectivity: &dmspb.MigrationJob_ReverseSshConnectivity{ReverseSshConnectivity: &dmspb.ReverseSshConnectivity{
				VmIp: "10.0.0.9", VmPort: 5433, Vm: "bastion"}}}})))
	mustOK(t, wait(t, s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job, SkipValidation: true})))
	eventually(t, "migrated", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.migrated["dst"] })
	f.mu.Lock()
	got := f.sources["dst"]
	f.mu.Unlock()
	if got.Host != "10.0.0.9" || got.Port != 5433 {
		t.Fatalf("the source through the bastion: %+v", got)
	}
	if _, err := s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: "k",
		MigrationJob: &dmspb.MigrationJob{Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/src",
			Destination:  parent + "/connectionProfiles/dst",
			Connectivity: &dmspb.MigrationJob_ReverseSshConnectivity{ReverseSshConnectivity: &dmspb.ReverseSshConnectivity{VmPort: 5433}}}}); apierror.From(err).Code != codes.InvalidArgument {
		t.Fatalf("reverse SSH without vmIp: %v", err)
	}
}
