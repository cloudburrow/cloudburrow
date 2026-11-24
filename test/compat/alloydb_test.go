//go:build compat

package compat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	alloydb "cloud.google.com/go/alloydb/apiv1"
	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
	alloydbbeta "cloud.google.com/go/alloydb/apiv1beta"
	betapb "cloud.google.com/go/alloydb/apiv1beta/alloydbpb"
	"cloud.google.com/go/alloydbconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EnvAlloyDB points at the local AlloyDB Admin API (#2191), gRPC and JSON
// on one port; EnvAlloyDBConnector at the connectors' listener when it is
// not at port 5433 (another instance holds it).
const (
	EnvAlloyDB          = "CLOUDBURROW_TEST_ALLOYDB"
	EnvAlloyDBConnector = "CLOUDBURROW_TEST_ALLOYDB_CONNECTOR"
)

// alloyDBClients are the official client over gRPC and over REST, both
// pointed at the local endpoint: AlloyDB has no emulator variable.
func alloyDBClients(t *testing.T, h *Harness) map[string]*alloydb.AlloyDBAdminClient {
	t.Helper()
	addr := h.Endpoint(EnvAlloyDB)
	ctx := h.Context()
	g, err := alloydb.NewAlloyDBAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := alloydb.NewAlloyDBAdminRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*alloydb.AlloyDBAdminClient{"grpc": g, "rest": r}
}

// alloyDBWait is a context for one instance's operation, which starts a
// PostgreSQL server on the cluster.
func alloyDBWait(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// alloyDBDialer is the official Go connector (cloud.google.com/go/alloydbconn)
// pointed at this instance's Admin API, with its built-in telemetry off
// (it exports to Cloud Monitoring). The token is never checked for
// DB_NATIVE; the connector insists on having one. When the connectors'
// listener is not at 5433, the connector's own WithDialFunc redirects its
// dial of the reported address at 5433 to it, as an application beside a
// second instance does.
func alloyDBDialer(t *testing.T, h *Harness) *alloydbconn.Dialer {
	t.Helper()
	opts := []alloydbconn.Option{
		alloydbconn.WithAdminAPIEndpoint("http://" + h.Endpoint(EnvAlloyDB)),
		alloydbconn.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "cloudburrow", Expiry: time.Now().Add(time.Hour)})),
		alloydbconn.WithOptOutOfBuiltInTelemetry(),
		// The opt-out alone still leaves NewDialer opening a Cloud
		// Monitoring gRPC client, which connects to
		// monitoring.googleapis.com; an HTTP client makes that client fail
		// to start (gRPC refuses one), so nothing leaves the machine
		// (TestAlloyDBConnectorsSendRealSQL's go client proves it).
		alloydbconn.WithHTTPClient(&http.Client{Transport: &http.Transport{}}),
	}
	if listener := os.Getenv(EnvAlloyDBConnector); listener != "" {
		opts = append(opts, alloydbconn.WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, port, _ := net.SplitHostPort(addr); port != "5433" {
				return nil, fmt.Errorf("the connector dialed %s, want port 5433", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, listener)
		}))
	}
	d, err := alloydbconn.NewDialer(h.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// alloyDBConn opens a pgx connection to instance through the connector.
func alloyDBConn(ctx context.Context, t *testing.T, d *alloydbconn.Dialer, instance, user, password string, dial ...alloydbconn.DialOption) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("user=%s password=%s dbname=postgres sslmode=disable", user, password))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, instance, dial...) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to %s through the connector: %v", instance, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func alloyDBCode(err error) codes.Code { return pmCode(err) }

// TestAlloyDBClusterLifecycle (#2191): the official client, over gRPC and
// REST, creates a cluster with its initial user, gets, lists across
// locations, updates and deletes it, and is refused what Google refuses:
// a cluster with no network, a bad ID, a duplicate.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/CreateCluster, google.cloud.alloydb.v1.AlloyDBAdmin/GetCluster, google.cloud.alloydb.v1.AlloyDBAdmin/ListClusters, google.cloud.alloydb.v1.AlloyDBAdmin/UpdateCluster, google.cloud.alloydb.v1.AlloyDBAdmin/DeleteCluster, google.cloud.alloydb.v1.AlloyDBAdmin/ListSupportedDatabaseFlags
func TestAlloyDBClusterLifecycle(t *testing.T) {
	h := New(t)
	for transport, c := range alloyDBClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			ctx := alloyDBWait(t)
			loc := "projects/" + h.Project() + "/locations/us-central1"
			id := fmt.Sprintf("cl-%s-%d", transport, time.Now().UnixNano()%1e6)
			if _, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{}}); alloyDBCode(err) != codes.InvalidArgument {
				t.Errorf("a cluster with no network: %v, want INVALID_ARGUMENT", err)
			}
			network := "projects/" + h.Project() + "/global/networks/default"
			if _, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: "Bad_Id",
				Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}}); alloyDBCode(err) != codes.InvalidArgument {
				t.Errorf("a bad cluster ID: %v, want INVALID_ARGUMENT", err)
			}
			op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
				NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}, DatabaseVersion: alloydbpb.DatabaseVersion_POSTGRES_16,
				InitialUser: &alloydbpb.UserPassword{User: "postgres", Password: "burrow-pw"}, Labels: map[string]string{"env": "test"}}})
			if err != nil {
				t.Fatal(err)
			}
			cl, err := waitLRO(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			name := loc + "/clusters/" + id
			if cl.GetName() != name || cl.GetState() != alloydbpb.Cluster_READY || cl.GetDatabaseVersion() != alloydbpb.DatabaseVersion_POSTGRES_16 ||
				!cl.GetContinuousBackupConfig().GetEnabled() || cl.GetClusterType() != alloydbpb.Cluster_PRIMARY {
				t.Errorf("the created cluster: %v", cl)
			}
			if _, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id,
				Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}}); alloyDBCode(err) != codes.AlreadyExists {
				t.Errorf("a duplicate cluster: %v, want ALREADY_EXISTS", err)
			}
			found := false
			it := c.ListClusters(ctx, &alloydbpb.ListClustersRequest{Parent: "projects/" + h.Project() + "/locations/-"})
			for {
				x, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				found = found || x.GetName() == name
			}
			if !found {
				t.Errorf("ListClusters in every location left out %s", name)
			}
			uop, err := c.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: name, DisplayName: "Orders"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
			if err != nil {
				t.Fatal(err)
			}
			if cl, err := waitLRO(ctx, uop); err != nil || cl.GetDisplayName() != "Orders" {
				t.Errorf("UpdateCluster: %v, %v", cl, err)
			}
			fit := c.ListSupportedDatabaseFlags(ctx, &alloydbpb.ListSupportedDatabaseFlagsRequest{Parent: loc})
			if f, err := fit.Next(); err != nil || f.GetFlagName() == "" {
				t.Errorf("ListSupportedDatabaseFlags: %v, %v", f, err)
			}
			dop, err := c.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			if err := dop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); alloyDBCode(err) != codes.NotFound {
				t.Errorf("after DeleteCluster: %v, want NOT_FOUND", err)
			}
		})
	}
}

// TestAlloyDBInstancesConnectorAndBackups (#2191): one cluster, end to
// end. A primary instance starts a PostgreSQL server; the official Go
// connector reaches it by instance URI (GetConnectionInfo and
// GenerateClientCertificate through v1alpha, TLS 1.3 to port 5433 and the
// metadata exchange) and runs real SQL; a user created through the API
// logs in; ListDatabases and ExecuteSql read the server; a read pool
// replicates the primary and is reached through the connector by its PSC
// DNS name; an on-demand backup restores into a new cluster whose primary
// holds the data, and so does a point-in-time restore.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/CreateInstance, google.cloud.alloydb.v1.AlloyDBAdmin/GetInstance, google.cloud.alloydb.v1.AlloyDBAdmin/ListInstances, google.cloud.alloydb.v1.AlloyDBAdmin/UpdateInstance, google.cloud.alloydb.v1.AlloyDBAdmin/DeleteInstance, google.cloud.alloydb.v1.AlloyDBAdmin/RestartInstance, google.cloud.alloydb.v1.AlloyDBAdmin/GetConnectionInfo, google.cloud.alloydb.v1.AlloyDBAdmin/GenerateClientCertificate, google.cloud.alloydb.v1.AlloyDBAdmin/CreateUser, google.cloud.alloydb.v1.AlloyDBAdmin/GetUser, google.cloud.alloydb.v1.AlloyDBAdmin/ListUsers, google.cloud.alloydb.v1.AlloyDBAdmin/UpdateUser, google.cloud.alloydb.v1.AlloyDBAdmin/DeleteUser, google.cloud.alloydb.v1.AlloyDBAdmin/ListDatabases, google.cloud.alloydb.v1.AlloyDBAdmin/ExecuteSql, google.cloud.alloydb.v1.AlloyDBAdmin/CreateBackup, google.cloud.alloydb.v1.AlloyDBAdmin/GetBackup, google.cloud.alloydb.v1.AlloyDBAdmin/ListBackups, google.cloud.alloydb.v1.AlloyDBAdmin/UpdateBackup, google.cloud.alloydb.v1.AlloyDBAdmin/DeleteBackup, google.cloud.alloydb.v1.AlloyDBAdmin/RestoreCluster
func TestAlloyDBInstancesConnectorAndBackups(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"]
	rc := alloyDBClients(t, h)["rest"]
	ctx := alloyDBWait(t)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	suffix := time.Now().UnixNano() % 1e6
	id := fmt.Sprintf("e2e-%d", suffix)
	name := loc + "/clusters/" + id
	created := []string{}
	t.Cleanup(func() {
		for _, n := range created {
			if op, err := c.DeleteCluster(context.Background(), &alloydbpb.DeleteClusterRequest{Name: n, Force: true}); err == nil {
				_ = op.Wait(alloyDBWait(t))
			}
		}
	})
	op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser:   &alloydbpb.UserPassword{User: "postgres", Password: "burrow-pw"}}})
	if err != nil {
		t.Fatal(err)
	}
	created = append(created, name)
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	// A read pool needs a primary.
	if _, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "pool", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_READ_POOL, ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 1},
		MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("a read pool with no primary: %v, want FAILED_PRECONDITION", err)
	}
	iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2},
		DatabaseFlags: map[string]string{"max_connections": "200"}}})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatal(err)
	}
	if primary.GetState() != alloydbpb.Instance_READY || primary.GetIpAddress() == "" || primary.GetInstanceType() != alloydbpb.Instance_PRIMARY {
		t.Errorf("the primary: %v", primary)
	}
	info, err := rc.GetConnectionInfo(ctx, &alloydbpb.GetConnectionInfoRequest{Parent: primary.GetName()})
	if err != nil || info.GetIpAddress() != primary.GetIpAddress() || info.GetInstanceUid() != primary.GetUid() {
		t.Errorf("GetConnectionInfo: %v, %v", info, err)
	}
	// GenerateClientCertificate through the client: a chain ending in the
	// CA, for the key sent.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	cert, err := c.GenerateClientCertificate(ctx, &alloydbpb.GenerateClientCertificateRequest{Parent: name,
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))})
	if err != nil || len(cert.GetPemCertificateChain()) != 2 || cert.GetCaCert() == "" {
		t.Fatalf("GenerateClientCertificate: %v, %v", cert, err)
	}

	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, primary.GetName(), "postgres", "burrow-pw")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'burrow'), (2, 'tunnel')"} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	var setting string
	if err := pg.QueryRow(ctx, "SELECT count(*), current_setting('max_connections') FROM orders").Scan(&n, &setting); err != nil || n != 2 || setting != "200" {
		t.Fatalf("through the connector: %d rows, max_connections %s, %v", n, setting, err)
	}

	// A user created through the API logs in through the connector.
	if _, err := c.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: name, UserId: "app", User: &alloydbpb.User{
		UserType: alloydbpb.User_ALLOYDB_BUILT_IN, Password: "app-pw", DatabaseRoles: []string{"alloydbsuperuser"}}}); err != nil {
		t.Fatal(err)
	}
	// alloydbsuperuser is not PostgreSQL's superuser: app reads postgres's
	// table once granted, as on Google.
	app := alloyDBConn(ctx, t, d, primary.GetName(), "app", "app-pw")
	var who string
	if err := app.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != "app" {
		t.Errorf("as app: current_user %q, %v", who, err)
	}
	if _, err := pg.Exec(ctx, "GRANT SELECT ON orders TO app"); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n); err != nil || n != 2 {
		t.Errorf("as app: %d, %v", n, err)
	}
	if u, err := rc.GetUser(ctx, &alloydbpb.GetUserRequest{Name: name + "/users/app"}); err != nil || u.GetUserType() != alloydbpb.User_ALLOYDB_BUILT_IN {
		t.Errorf("GetUser: %v, %v", u, err)
	}
	if _, err := c.UpdateUser(ctx, &alloydbpb.UpdateUserRequest{User: &alloydbpb.User{Name: name + "/users/app", Password: "app-pw2"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"password"}}}); err != nil {
		t.Errorf("UpdateUser: %v", err)
	}
	users := 0
	uit := c.ListUsers(ctx, &alloydbpb.ListUsersRequest{Parent: name})
	for {
		_, err := uit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		users++
	}
	if users != 2 {
		t.Errorf("ListUsers: %d users, want postgres and app", users)
	}
	dit := c.ListDatabases(ctx, &alloydbpb.ListDatabasesRequest{Parent: name})
	sawPostgres := false
	for {
		db, err := dit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sawPostgres = sawPostgres || strings.HasSuffix(db.GetName(), "/databases/postgres")
	}
	if !sawPostgres {
		t.Error("ListDatabases left out postgres")
	}
	res, err := c.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: primary.GetName(), Database: "postgres", User: "postgres",
		UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "burrow-pw"}, SqlStatement: "SELECT item FROM orders ORDER BY id"})
	if err != nil || len(res.GetSqlResults()) != 1 || len(res.GetSqlResults()[0].GetRows()) != 2 ||
		res.GetSqlResults()[0].GetRows()[0].GetValues()[0].GetValue() != "burrow" {
		t.Errorf("ExecuteSql: %v, %v", res, err)
	}

	// A read pool replicates the primary, and the connector reaches it by
	// its PSC DNS name (WithPSC).
	pop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "pool", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_READ_POOL, ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 2},
		MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := waitLRO(ctx, pop)
	if err != nil {
		t.Fatal(err)
	}
	// In this private-services-access cluster the connectors reach the
	// primary (every instance's IP is this host's); ExecuteSql reaches the
	// pool's own server, a hot standby with the primary's rows.
	var standby string
	deadline := time.Now().Add(time.Minute)
	for {
		res, err = c.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: pool.GetName(), Database: "postgres", User: "postgres",
			UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "burrow-pw"}, SqlStatement: "SELECT pg_is_in_recovery()::text, (SELECT count(*) FROM orders)::text"})
		if err == nil && len(res.GetSqlResults()) == 1 && len(res.GetSqlResults()[0].GetRows()) == 1 {
			v := res.GetSqlResults()[0].GetRows()[0].GetValues()
			standby = v[0].GetValue() + "/" + v[1].GetValue()
		}
		if standby == "true/2" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if standby != "true/2" {
		t.Errorf("the read pool: in recovery/rows %q, %v", standby, err)
	}
	instances := 0
	iit := rc.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: name})
	for {
		_, err := iit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		instances++
	}
	if instances != 2 {
		t.Errorf("ListInstances: %d, want 2", instances)
	}
	// The primary is not deleted under its read pool.
	if _, err := c.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: primary.GetName()}); alloyDBCode(err) != codes.FailedPrecondition {
		t.Errorf("deleting the primary under a read pool: %v, want FAILED_PRECONDITION", err)
	}
	dop, err := c.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: pool.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// An on-demand backup, restored into a new cluster.
	bop, err := c.CreateBackup(ctx, &alloydbpb.CreateBackupRequest{Parent: loc, BackupId: id + "-b", Backup: &alloydbpb.Backup{ClusterName: name}})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := waitLRO(ctx, bop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteBackup(context.Background(), &alloydbpb.DeleteBackupRequest{Name: backup.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if backup.GetState() != alloydbpb.Backup_READY || backup.GetType() != alloydbpb.Backup_ON_DEMAND || backup.GetSizeBytes() == 0 {
		t.Errorf("the backup: %v", backup)
	}
	upb, err := c.UpdateBackup(ctx, &alloydbpb.UpdateBackupRequest{Backup: &alloydbpb.Backup{Name: backup.GetName(), Labels: map[string]string{"k": "v"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := waitLRO(ctx, upb); err != nil || b.GetLabels()["k"] != "v" {
		t.Errorf("UpdateBackup: %v, %v", b, err)
	}
	// Restored into a Private Service Connect cluster: the connectors reach
	// its primary and its read pool by their PSC DNS names (WithPSC).
	restored := alloyDBRestore(ctx, t, c, loc, id+"-r", &alloydbpb.RestoreClusterRequest{
		Cluster: &alloydbpb.Cluster{PscConfig: &alloydbpb.Cluster_PscConfig{PscEnabled: true}},
		Source:  &alloydbpb.RestoreClusterRequest_BackupSource{BackupSource: &alloydbpb.BackupSource{BackupName: backup.GetName()}}})
	created = append(created, restored)
	alloyDBExpectRows(ctx, t, d, restored+"/instances/primary", 2, false, alloydbconn.WithPSC())
	rpop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: restored, InstanceId: "pool", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_READ_POOL, ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 1},
		MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	rpool, err := waitLRO(ctx, rpop)
	if err != nil {
		t.Fatal(err)
	}
	if rpool.GetPscInstanceConfig().GetPscDnsName() == "" || rpool.GetIpAddress() != "" {
		t.Errorf("a PSC cluster's read pool: %v", rpool)
	}
	alloyDBExpectRows(ctx, t, d, rpool.GetName(), 2, true, alloydbconn.WithPSC())

	// A point-in-time restore, after a third row: the time before it
	// holds two rows.
	before := time.Now()
	time.Sleep(2 * time.Second)
	if _, err := pg.Exec(ctx, "INSERT INTO orders VALUES (3, 'late')"); err != nil {
		t.Fatal(err)
	}
	cl, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	if err != nil || cl.GetContinuousBackupInfo().GetEarliestRestorableTime() == nil {
		t.Fatalf("continuous backup info: %v, %v", cl, err)
	}
	pitr := alloyDBRestore(ctx, t, c, loc, id+"-p", &alloydbpb.RestoreClusterRequest{
		Source: &alloydbpb.RestoreClusterRequest_ContinuousBackupSource{ContinuousBackupSource: &alloydbpb.ContinuousBackupSource{
			Cluster: name, PointInTime: timestamppb.New(before)}}})
	created = append(created, pitr)
	alloyDBExpectRows(ctx, t, d, pitr+"/instances/primary", 2, false)

	// A role that holds privileges is not dropped (PostgreSQL's
	// dependency check), so the grant goes first.
	if _, err := pg.Exec(ctx, "REVOKE SELECT ON orders FROM app"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUser(ctx, &alloydbpb.DeleteUserRequest{Name: name + "/users/app"}); err != nil {
		t.Errorf("DeleteUser: %v", err)
	}
	rop, err := c.RestartInstance(ctx, &alloydbpb.RestartInstanceRequest{Name: primary.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, rop); err != nil {
		t.Errorf("RestartInstance: %v", err)
	}
}

// alloyDBRestore restores a cluster from req's source and gives it a
// primary, returning its name.
func alloyDBRestore(ctx context.Context, t *testing.T, c *alloydb.AlloyDBAdminClient, loc, id string, req *alloydbpb.RestoreClusterRequest) string {
	t.Helper()
	req.Parent, req.ClusterId = loc, id
	if req.Cluster == nil {
		req.Cluster = &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: strings.Replace(loc, "/locations/us-central1", "/global/networks/default", 1)}}
	}
	op, err := c.RestoreCluster(ctx, req)
	if err != nil {
		t.Fatalf("RestoreCluster %s: %v", id, err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("RestoreCluster %s: %v", id, err)
	}
	name := loc + "/clusters/" + id
	iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, iop); err != nil {
		t.Fatalf("the restored cluster %s's primary: %v", id, err)
	}
	return name
}

func alloyDBExpectRows(ctx context.Context, t *testing.T, d *alloydbconn.Dialer, instance string, want int, standby bool, dial ...alloydbconn.DialOption) {
	t.Helper()
	conn := alloyDBConn(ctx, t, d, instance, "postgres", "burrow-pw", dial...)
	var n int
	var recovery bool
	deadline := time.Now().Add(time.Minute)
	var err error
	for {
		err = conn.QueryRow(ctx, "SELECT count(*), pg_is_in_recovery() FROM orders").Scan(&n, &recovery)
		if err == nil && n == want || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || n != want || recovery != standby {
		t.Errorf("%s: %d rows, in recovery %v, %v; want %d rows, in recovery %v", instance, n, recovery, err, want, standby)
	}
}

// TestAlloyDBBetaClient (#2191): the v1beta client, over gRPC and REST,
// reads the clusters v1 made, and its long-running operations report
// v1beta's types.
func TestAlloyDBBetaClient(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvAlloyDB)
	ctx := alloyDBWait(t)
	g, err := alloydbbeta.NewAlloyDBAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := alloydbbeta.NewAlloyDBAdminRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	loc := "projects/" + h.Project() + "/locations/us-central1"
	for transport, c := range map[string]*alloydbbeta.AlloyDBAdminClient{"grpc": g, "rest": r} {
		id := fmt.Sprintf("beta-%s-%d", transport, time.Now().UnixNano()%1e6)
		op, err := c.CreateCluster(ctx, &betapb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &betapb.Cluster{
			NetworkConfig: &betapb.Cluster_NetworkConfig{Network: "projects/" + h.Project() + "/global/networks/default"}}})
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		cl, err := waitLRO(ctx, op)
		if err != nil || cl.GetState() != betapb.Cluster_READY {
			t.Fatalf("%s: %v, %v", transport, cl, err)
		}
		got, err := c.GetCluster(ctx, &betapb.GetClusterRequest{Name: cl.GetName()})
		if err != nil || got.GetUid() != cl.GetUid() {
			t.Errorf("%s GetCluster: %v, %v", transport, got, err)
		}
		dop, err := c.DeleteCluster(ctx, &betapb.DeleteClusterRequest{Name: cl.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if err := dop.Wait(ctx); err != nil {
			t.Errorf("%s DeleteCluster: %v", transport, err)
		}
	}
}

// TestAlloyDBInstanceOperations (#2191): BatchCreateInstances makes a
// cluster's primary; UpdateInstance changes its flags, which the server
// then runs with; FailoverInstance swaps a regional primary's nodes;
// InjectFault STOP_VM restarts it; UpgradeCluster takes the cluster from
// PostgreSQL 15 to 16 with its data; and the connector reads the table
// after each.
// covers: google.cloud.alloydb.v1.AlloyDBAdmin/BatchCreateInstances, google.cloud.alloydb.v1.AlloyDBAdmin/UpdateInstance, google.cloud.alloydb.v1.AlloyDBAdmin/FailoverInstance, google.cloud.alloydb.v1.AlloyDBAdmin/InjectFault, google.cloud.alloydb.v1.AlloyDBAdmin/UpgradeCluster
func TestAlloyDBInstanceOperations(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"]
	ctx := alloyDBWait(t)
	loc := "projects/" + h.Project() + "/locations/us-central1"
	id := fmt.Sprintf("ops-%d", time.Now().UnixNano()%1e6)
	name := loc + "/clusters/" + id
	op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		NetworkConfig:   &alloydbpb.Cluster_NetworkConfig{Network: "projects/" + h.Project() + "/global/networks/default"},
		DatabaseVersion: alloydbpb.DatabaseVersion_POSTGRES_15, InitialUser: &alloydbpb.UserPassword{Password: "burrow-pw"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(context.Background(), &alloydbpb.DeleteClusterRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(alloyDBWait(t))
		}
	})
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	bop, err := c.BatchCreateInstances(ctx, &alloydbpb.BatchCreateInstancesRequest{Parent: name, Requests: &alloydbpb.CreateInstanceRequests{
		CreateInstanceRequests: []*alloydbpb.CreateInstanceRequest{{Parent: name, InstanceId: "primary", Instance: &alloydbpb.Instance{
			InstanceType: alloydbpb.Instance_PRIMARY, AvailabilityType: alloydbpb.Instance_REGIONAL,
			MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := waitLRO(ctx, bop); err != nil || len(res.GetInstances()) != 1 {
		t.Fatalf("BatchCreateInstances: %v, %v", res, err)
	}
	primary := name + "/instances/primary"
	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, primary, "postgres", "burrow-pw")
	if _, err := pg.Exec(ctx, "CREATE TABLE orders (id int); INSERT INTO orders VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	_ = pg.Close(ctx)

	uop, err := c.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: primary,
		DatabaseFlags: map[string]string{"work_mem": "8192"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"database_flags"}}})
	if err != nil {
		t.Fatal(err)
	}
	if in, err := waitLRO(ctx, uop); err != nil || in.GetDatabaseFlags()["work_mem"] != "8192" {
		t.Fatalf("UpdateInstance: %v, %v", in, err)
	}
	var workMem string
	if err := alloyDBConn(ctx, t, d, primary, "postgres", "burrow-pw").QueryRow(ctx, "SHOW work_mem").Scan(&workMem); err != nil || workMem != "8MB" {
		t.Errorf("work_mem after UpdateInstance: %q, %v", workMem, err)
	}

	before, err := c.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: primary})
	if err != nil {
		t.Fatal(err)
	}
	fop, err := c.FailoverInstance(ctx, &alloydbpb.FailoverInstanceRequest{Name: primary})
	if err != nil {
		t.Fatal(err)
	}
	after, err := waitLRO(ctx, fop)
	if err != nil {
		t.Fatal(err)
	}
	if before.GetWritableNode().GetZoneId() == after.GetWritableNode().GetZoneId() {
		t.Errorf("FailoverInstance left the writable node in %s", after.GetWritableNode().GetZoneId())
	}
	if _, err := c.InjectFault(ctx, &alloydbpb.InjectFaultRequest{Name: primary, FaultType: alloydbpb.InjectFaultRequest_FAULT_TYPE_UNSPECIFIED}); alloyDBCode(err) != codes.InvalidArgument {
		t.Errorf("InjectFault with no fault type: %v, want INVALID_ARGUMENT", err)
	}
	iop, err := c.InjectFault(ctx, &alloydbpb.InjectFaultRequest{Name: primary, FaultType: alloydbpb.InjectFaultRequest_STOP_VM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iop.Wait(ctx); err != nil {
		t.Errorf("InjectFault: %v", err)
	}

	gop, err := c.UpgradeCluster(ctx, &alloydbpb.UpgradeClusterRequest{Name: name, Version: alloydbpb.DatabaseVersion_POSTGRES_16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, gop); err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	if cl, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); err != nil || cl.GetDatabaseVersion() != alloydbpb.DatabaseVersion_POSTGRES_16 {
		t.Errorf("after UpgradeCluster: %v, %v", cl, err)
	}
	var version string
	var n int
	if err := alloyDBConn(ctx, t, d, primary, "postgres", "burrow-pw").QueryRow(ctx, "SELECT current_setting('server_version_num'), (SELECT count(*) FROM orders)").Scan(&version, &n); err != nil ||
		!strings.HasPrefix(version, "16") || n != 2 {
		t.Errorf("after UpgradeCluster: version %s, %d rows, %v", version, n, err)
	}
}
