package alloydb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// createOmni creates cluster id of version with ctx's headers.
func createOmni(ctx context.Context, f *fixture, id string, version alloydbpb.DatabaseVersion) error {
	op, err := f.s.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		DatabaseVersion: version, NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser: &alloydbpb.UserPassword{User: "postgres", Password: "pw"}}})
	f.s.Wait()
	if err == nil && op.GetError() != nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

func engineCtx(engine string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(strings.ToLower(EngineHeader), engine))
}

// startPrimary creates id's primary and returns the spec the engine ran.
func startPrimary(t *testing.T, f *fixture, id string) ServerSpec {
	t.Helper()
	name := loc + "/clusters/" + id
	op, err := f.s.CreateInstance(context.Background(), &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	wait(t, f.s, op, err)
	f.eng.mu.Lock()
	defer f.eng.mu.Unlock()
	return f.eng.running[serverName(name+"/instances/primary")]
}

// Without acceptance (#2207): a cluster runs PostgreSQL, as before; an
// explicit Omni request is FAILED_PRECONDITION saying how to accept, and
// makes no cluster; bad header values are INVALID_ARGUMENT.
func TestOmniRefusedAndFallbackWithoutAcceptance(t *testing.T) {
	f := newFixture(t, Options{})
	if err := createOmni(context.Background(), f, "plain", alloydbpb.DatabaseVersion_POSTGRES_16); err != nil {
		t.Fatal(err)
	}
	if spec := startPrimary(t, f, "plain"); spec.Omni != nil || spec.Version != "POSTGRES_16" {
		t.Errorf("the fallback: %+v, want PostgreSQL 16", spec)
	}
	if a := f.s.OmniOf(loc + "/clusters/plain"); a != nil {
		t.Errorf("OmniOf the PostgreSQL cluster: %+v", a)
	}
	// AlloyDB's engine flags stay accepted, and stored, on PostgreSQL.
	op, err := f.s.CreateInstance(context.Background(), &alloydbpb.CreateInstanceRequest{Parent: loc + "/clusters/plain", InstanceId: "pool",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_READ_POOL, ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: 1},
			DatabaseFlags: map[string]string{"google_columnar_engine.enabled": "on"}}})
	wait(t, f.s, op, err)

	for _, ctx := range []context.Context{engineCtx("OMNI"), engineCtx("omni")} {
		err := createOmni(ctx, f, "asked", alloydbpb.DatabaseVersion_POSTGRES_16)
		if code(err) != codes.FailedPrecondition {
			t.Fatalf("explicit Omni with no acceptance: %v, want FAILED_PRECONDITION", err)
		}
		for _, want := range []string{"--accept-alloydb-omni-terms", "CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y", OmniTermsURL,
			"never accepts them on your behalf", OmniTermsHeader + ": Y", "alloydb.acceptOmniTerms"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q", err, want)
			}
		}
		if _, err := f.s.GetCluster(context.Background(), &alloydbpb.GetClusterRequest{Name: loc + "/clusters/asked"}); code(err) != codes.NotFound {
			t.Errorf("the refused cluster: %v, want NOT_FOUND", err)
		}
	}
	for name, ctx := range map[string]context.Context{
		"engine": engineCtx("ORACLE"),
		"accept": metadata.NewIncomingContext(context.Background(), metadata.Pairs(strings.ToLower(OmniTermsHeader), "yes")),
		"both": metadata.NewIncomingContext(context.Background(), metadata.Pairs(strings.ToLower(OmniTermsHeader), "Y",
			strings.ToLower(EngineHeader), "POSTGRES")),
		"14": engineCtx("OMNI"),
	} {
		v := alloydbpb.DatabaseVersion_POSTGRES_16
		if name == "14" {
			v = alloydbpb.DatabaseVersion_POSTGRES_14
		}
		if err := createOmni(ctx, f, "bad-"+name, v); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	// POSTGRES is always served.
	if err := createOmni(engineCtx("POSTGRES"), f, "pg", alloydbpb.DatabaseVersion_POSTGRES_17); err != nil {
		t.Errorf("POSTGRES: %v", err)
	}
}

// With `up`'s acceptance: new clusters of 15, 16 and 17 run Omni under it,
// recorded with the cluster and kept across a restart; 14 stays
// PostgreSQL; POSTGRES opts a cluster out; a secondary cluster runs its
// primary's engine; a cluster made before the acceptance stays PostgreSQL.
func TestOmniWithUpAcceptance(t *testing.T) {
	f := newFixture(t, Options{})
	if err := createOmni(context.Background(), f, "before", alloydbpb.DatabaseVersion_POSTGRES_16); err != nil {
		t.Fatal(err)
	}
	up := &OmniAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-24T10:00:00Z", Via: OmniViaFlag}
	f.s.AcceptOmniTerms(up)
	for _, v := range []alloydbpb.DatabaseVersion{alloydbpb.DatabaseVersion_POSTGRES_15, alloydbpb.DatabaseVersion_POSTGRES_16, alloydbpb.DatabaseVersion_POSTGRES_17} {
		id := "omni-" + strings.TrimPrefix(strings.ToLower(v.String()), "postgres_")
		if err := createOmni(context.Background(), f, id, v); err != nil {
			t.Fatal(err)
		}
		if spec := startPrimary(t, f, id); spec.Omni == nil || *spec.Omni != *up || spec.Version != v.String() {
			t.Errorf("%s: %+v, want Omni under %+v", v, spec, up)
		}
	}
	if err := createOmni(context.Background(), f, "fourteen", alloydbpb.DatabaseVersion_POSTGRES_14); err != nil {
		t.Fatal(err)
	}
	if spec := startPrimary(t, f, "fourteen"); spec.Omni != nil {
		t.Errorf("POSTGRES_14 with the terms accepted: %+v, want PostgreSQL", spec)
	}
	if err := createOmni(engineCtx("POSTGRES"), f, "optout", alloydbpb.DatabaseVersion_POSTGRES_16); err != nil {
		t.Fatal(err)
	}
	if spec := startPrimary(t, f, "optout"); spec.Omni != nil {
		t.Errorf("POSTGRES: %+v", spec)
	}
	if spec := startPrimary(t, f, "before"); spec.Omni != nil {
		t.Errorf("a cluster made before the acceptance: %+v", spec)
	}
	// The secondary of an Omni primary runs Omni, under the primary's.
	op, err := f.s.CreateSecondaryCluster(context.Background(), &alloydbpb.CreateSecondaryClusterRequest{Parent: "projects/p1/locations/us-east1",
		ClusterId: "sec", Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
			SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: loc + "/clusters/omni-16"}}})
	wait(t, f.s, op, err)
	if a := f.s.OmniOf("projects/p1/locations/us-east1/clusters/sec"); a == nil || *a != *up {
		t.Errorf("the secondary: %+v", a)
	}
	// The records survive a restart, and `up` without the flag keeps them.
	again, err := NewServer(filepath.Join(f.dir, "alloydb.json"), filepath.Join(f.dir, "ca.json"), Options{Engine: f.eng, ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if a := again.OmniOf(loc + "/clusters/omni-17"); a == nil || *a != *up {
		t.Errorf("after a restart: %+v", a)
	}
	// Deleting the cluster drops its record.
	dop, err := f.s.DeleteCluster(context.Background(), &alloydbpb.DeleteClusterRequest{Name: loc + "/clusters/omni-15", Force: true})
	wait(t, f.s, dop, err)
	if a := f.s.OmniOf(loc + "/clusters/omni-15"); a != nil {
		t.Errorf("a deleted cluster's record: %+v", a)
	}
}

// The per-request acceptance, over the JSON API's header and the console's
// context: the cluster runs Omni, recorded as accepted by the local user
// through the header, now.
func TestOmniAcceptedByRequestHeader(t *testing.T) {
	f := newFixture(t, Options{})
	if err := createOmni(WithOmniTermsAccepted(context.Background()), f, "console", alloydbpb.DatabaseVersion_POSTGRES_17); err != nil {
		t.Fatal(err)
	}
	if spec := startPrimary(t, f, "console"); !spec.Omni.Accepted() || spec.Omni.Via != OmniViaConsole {
		t.Errorf("the console's acceptance: %+v", spec.Omni)
	}
	srv := httptest.NewServer(NewRESTHandler(f.s))
	t.Cleanup(srv.Close)
	post := func(id string, h map[string]string) *http.Response {
		body := `{"databaseVersion":"POSTGRES_16","networkConfig":{"network":"` + network + `"}}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/"+loc+"/clusters?clusterId="+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		f.s.Wait()
		return resp
	}
	if r := post("rest-refused", map[string]string{EngineHeader: "OMNI"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("REST explicit Omni with no acceptance: %d, want 400 (FAILED_PRECONDITION)", r.StatusCode)
	}
	if r := post("rest", map[string]string{OmniTermsHeader: "Y"}); r.StatusCode != http.StatusOK {
		t.Fatalf("REST with %s: Y: %d", OmniTermsHeader, r.StatusCode)
	}
	if a := f.s.OmniOf(loc + "/clusters/rest"); !a.Accepted() || a.Via != OmniViaConsole {
		t.Errorf("REST's acceptance: %+v", a)
	}
	if r := post("rest-plain", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("REST plain: %d", r.StatusCode)
	}
	if a := f.s.OmniOf(loc + "/clusters/rest-plain"); a != nil {
		t.Errorf("REST with no header: %+v, want PostgreSQL", a)
	}
}
