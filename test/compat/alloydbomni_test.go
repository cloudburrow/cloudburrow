//go:build compat

package compat

// The AlloyDB Omni engine, opt-in (#2207). Google's google/alloydbomni
// image is governed by Google's AlloyDB Omni licence terms, which only the
// end user may accept: nobody, these tests and CI included, accepts them
// on anyone's behalf. So TestAlloyDBOmniEngine, which runs Omni, runs only
// when the person running it opted in, by exporting
// CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y both for `cloudburrow up` (which
// then records them as accepting the terms) and for `go test`; otherwise
// it skips, and the refusal and fallback test runs instead
// (docs/alloydb.md#running-the-omni-test-yourself).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	alloydb "cloud.google.com/go/alloydb/apiv1"
	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"github.com/googleapis/gax-go/v2/callctx"
	"google.golang.org/grpc/codes"
)

// EnvAcceptOmniTerms is the user's own opt-in: Y accepts Google's AlloyDB
// Omni terms, as `up` reads it too.
const EnvAcceptOmniTerms = "CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS"

func omniOptedIn() bool { return os.Getenv(EnvAcceptOmniTerms) == "Y" }

// The opt-in's headers, CloudBurrow's own (internal/service/alloydb), in
// lower case: gRPC metadata keys must be, and HTTP's are case-insensitive.
const (
	omniEngineHeader = "x-cloudburrow-alloydb-engine"
	omniTermsHeader  = "x-cloudburrow-accept-alloydb-omni-terms"
)

// alloyDBSQL runs stmt on instance as postgres through ExecuteSql and
// returns the first column of the first row.
func alloyDBSQL(ctx context.Context, t *testing.T, c *alloydb.AlloyDBAdminClient, instance, password, stmt string) string {
	t.Helper()
	res, err := c.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: instance, Database: "postgres", User: "postgres",
		UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: password}, SqlStatement: stmt})
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	r := res.GetSqlResults()
	if len(r) == 0 || len(r[len(r)-1].GetRows()) == 0 {
		return ""
	}
	return r[len(r)-1].GetRows()[0].GetValues()[0].GetValue()
}

// alloyDBOmniCluster creates cluster id of version in loc with ctx's
// headers and its primary with flags, and returns the primary's name.
func alloyDBOmniCluster(ctx context.Context, t *testing.T, c *alloydb.AlloyDBAdminClient, loc, network, id string,
	version alloydbpb.DatabaseVersion, flags map[string]string) string {
	t.Helper()
	name := loc + "/clusters/" + id
	op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		DatabaseVersion: version, NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser: &alloydbpb.UserPassword{User: "postgres", Password: "burrow-pw"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(alloyDBWait(t), &alloydbpb.DeleteClusterRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(alloyDBWait(t))
		}
	})
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}, DatabaseFlags: flags}})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatal(err)
	}
	return primary.GetName()
}

// TestAlloyDBOmniRefusedWithoutAcceptance (#2207): with no acceptance of
// the AlloyDB Omni terms, CreateCluster asking for Omni (the engine
// header), over gRPC and REST, is FAILED_PRECONDITION saying how to
// accept, and makes no cluster; a cluster that does not ask runs
// PostgreSQL, as before: its primary has none of Omni's extensions.
func TestAlloyDBOmniRefusedWithoutAcceptance(t *testing.T) {
	h := New(t)
	clients := alloyDBClients(t, h)
	if omniOptedIn() {
		t.Skip(EnvAcceptOmniTerms + "=Y: this instance was started accepting the AlloyDB Omni terms, so the refusal is not what it answers; " +
			"TestAlloyDBOmniEngine runs instead, and the unit tests check the refusal")
	}
	ctx := alloyDBWait(t)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	suffix := time.Now().UnixNano() % 1e6
	for transport, c := range clients {
		id := fmt.Sprintf("omni-%s-%d", transport, suffix)
		_, err := c.CreateCluster(callctx.SetHeaders(ctx, omniEngineHeader, "OMNI"), &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id,
			Cluster: &alloydbpb.Cluster{DatabaseVersion: alloydbpb.DatabaseVersion_POSTGRES_16,
				NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}})
		if alloyDBCode(err) != codes.FailedPrecondition {
			t.Fatalf("%s: explicit Omni with no acceptance: %v, want FAILED_PRECONDITION", transport, err)
		}
		for _, want := range []string{"--accept-alloydb-omni-terms", EnvAcceptOmniTerms + "=Y", "never accepts them on your behalf",
			"cloud.google.com/terms/alloydb-omni-marketplace-terms"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not say %q", transport, err, want)
			}
		}
		if _, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: loc + "/clusters/" + id}); alloyDBCode(err) != codes.NotFound {
			t.Errorf("%s: the refused cluster: %v, want NOT_FOUND", transport, err)
		}
		// A header value other than Y accepts nothing.
		_, err = c.CreateCluster(callctx.SetHeaders(ctx, omniTermsHeader, "yes"), &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id,
			Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}}})
		if alloyDBCode(err) != codes.InvalidArgument {
			t.Errorf("%s: %s: yes: %v, want INVALID_ARGUMENT", transport, omniTermsHeader, err)
		}
	}

	// The fallback: no header, PostgreSQL, with AlloyDB's flags stored.
	c := clients["grpc"]
	primary := alloyDBOmniCluster(ctx, t, c, loc, network, fmt.Sprintf("omni-pg-%d", suffix), alloydbpb.DatabaseVersion_POSTGRES_16,
		map[string]string{"google_columnar_engine.enabled": "on"})
	if n := alloyDBSQL(ctx, t, c, primary, "burrow-pw", "SELECT count(*)::text FROM pg_available_extensions "+
		"WHERE name IN ('google_columnar_engine', 'google_ml_integration', 'alloydb_scann')"); n != "0" {
		t.Errorf("the PostgreSQL fallback has %s of Omni's extensions", n)
	}
}

// TestAlloyDBOmniEngine (#2207): only for a person who accepted the AlloyDB
// Omni terms (see the file's comment). A POSTGRES_16 cluster's primary runs
// Omni: AlloyDB's extensions are there and are created (the columnar
// engine, google_ml_integration, alloydb_scann), the columnar engine's flag
// takes effect, a table is added to the column store, and the index
// advisor's flag is on; a POSTGRES_17 cluster asked for by the request
// header alone runs Omni 17; a POSTGRES_14 one stays PostgreSQL.
func TestAlloyDBOmniEngine(t *testing.T) {
	h := New(t)
	if !omniOptedIn() {
		t.Skip("runs Google's AlloyDB Omni, whose terms only you can accept: export " + EnvAcceptOmniTerms +
			"=Y for `cloudburrow up` and for this test to run it (docs/alloydb.md#running-the-omni-test-yourself)")
	}
	c := alloyDBClients(t, h)["grpc"]
	// The image's first pull is large, and Omni starts slower than PostgreSQL.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	suffix := time.Now().UnixNano() % 1e6
	primary := alloyDBOmniCluster(ctx, t, c, loc, network, fmt.Sprintf("omni16-%d", suffix), alloydbpb.DatabaseVersion_POSTGRES_16,
		map[string]string{"google_columnar_engine.enabled": "on", "google_columnar_engine.memory_size_in_mb": "256",
			"google_db_advisor.enabled": "on", "google_ml_integration.enable_model_support": "on"})
	q := func(stmt string) string { return alloyDBSQL(ctx, t, c, primary, "burrow-pw", stmt) }
	if v := q("SHOW server_version_num"); !strings.HasPrefix(v, "16") {
		t.Errorf("server_version_num %s, want 16", v)
	}
	for _, ext := range []string{"google_columnar_engine", "google_ml_integration", "alloydb_scann"} {
		if n := q("SELECT count(*)::text FROM pg_available_extensions WHERE name = '" + ext + "'"); n != "1" {
			t.Errorf("Omni lacks the %s extension", ext)
		}
	}
	for _, stmt := range []string{"CREATE EXTENSION IF NOT EXISTS google_columnar_engine", "CREATE EXTENSION IF NOT EXISTS google_ml_integration CASCADE",
		"CREATE EXTENSION IF NOT EXISTS alloydb_scann CASCADE", "CREATE TABLE sales (id int PRIMARY KEY, amount numeric)",
		"INSERT INTO sales SELECT g, g * 1.5 FROM generate_series(1, 1000) g"} {
		q(stmt)
	}
	for flag, want := range map[string]string{"google_columnar_engine.enabled": "on", "google_columnar_engine.memory_size_in_mb": "256",
		"google_db_advisor.enabled": "on", "google_ml_integration.enable_model_support": "on"} {
		if v := q("SHOW " + flag); v != want {
			t.Errorf("SHOW %s = %q, want %q: the flag did not take effect", flag, v, want)
		}
	}
	// The column store takes the table (google_columnar_engine_add, Omni's
	// documented way to add one by hand).
	q("SELECT google_columnar_engine_add('sales')")
	if n := q("SELECT count(*)::text FROM g_columnar_relations WHERE relation_name = 'sales'"); n == "0" || n == "" {
		t.Errorf("sales is not in the column store (g_columnar_relations: %q)", n)
	}

	// Omni 17 by the request header alone.
	p17 := alloyDBOmniCluster(callctx.SetHeaders(ctx, omniEngineHeader, "OMNI"), t, c, loc, network, fmt.Sprintf("omni17-%d", suffix),
		alloydbpb.DatabaseVersion_POSTGRES_17, nil)
	if n := alloyDBSQL(ctx, t, c, p17, "burrow-pw", "SELECT count(*)::text FROM pg_available_extensions WHERE name = 'google_columnar_engine'"); n != "1" {
		t.Errorf("the POSTGRES_17 cluster does not run Omni")
	}
	// Omni publishes no PostgreSQL 14: the cluster runs PostgreSQL 14.
	p14 := alloyDBOmniCluster(ctx, t, c, loc, network, fmt.Sprintf("omni14-%d", suffix), alloydbpb.DatabaseVersion_POSTGRES_14, nil)
	if n := alloyDBSQL(ctx, t, c, p14, "burrow-pw", "SELECT count(*)::text FROM pg_available_extensions WHERE name = 'google_columnar_engine'"); n != "0" {
		t.Errorf("the POSTGRES_14 cluster runs Omni")
	}
}
