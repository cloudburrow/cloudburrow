package sqladmin

import (
	"context"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestRotateDuringMaintenance (#1524): with AUTOMATIC_ROTATION_DURING_MAINTENANCE
// a maintenance run rotates a CAS instance's server certificate once it
// expires within 180 days, keeping the old one listed as the previous
// version; not earlier, and never with NO_AUTOMATIC_ROTATION or on a
// per-instance CA instance.
func TestRotateDuringMaintenance(t *testing.T) {
	srv := NewServer("127.0.0.1:0", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	api := "http://" + srv.Addr() + "/"
	svc, err := sqladmin.NewService(ctx, option.WithEndpoint(api), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	base := api + "v1/projects/proj/instances"
	for _, b := range []string{
		`{"name":"auto","databaseVersion":"POSTGRES_17","settings":{"tier":"t","ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA","serverCertificateRotationMode":"AUTOMATIC_ROTATION_DURING_MAINTENANCE"}}}`,
		`{"name":"manual","databaseVersion":"POSTGRES_17","settings":{"tier":"t","ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA","serverCertificateRotationMode":"NO_AUTOMATIC_ROTATION"}}}`,
		`{"name":"internal","databaseVersion":"POSTGRES_17","settings":{"tier":"t","ipConfiguration":{"serverCertificateRotationMode":"AUTOMATIC_ROTATION_DURING_MAINTENANCE"}}}`,
	} {
		if code, resp := call(t, "POST", base, b); code != 200 {
			t.Fatalf("insert = %d %s", code, resp)
		}
	}
	for _, n := range []string{"auto", "manual", "internal"} {
		getRunnable(t, base+"/"+n)
	}
	spec := func(name string) Spec {
		srv.emu.RLock()
		defer srv.emu.RUnlock()
		return srv.writes.byName[name].spec
	}
	before, err := svc.Instances.ListServerCertificates("proj", "auto").Do()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if rotated, err := srv.rotateDuringMaintenance(spec("auto"), now); err != nil || rotated {
		t.Errorf("a year before expiry: rotated %v, %v; want no rotation", rotated, err)
	}
	later := now.Add(200 * 24 * time.Hour) // 165 days before expiry
	if rotated, err := srv.rotateDuringMaintenance(spec("auto"), later); err != nil || !rotated {
		t.Fatalf("165 days before expiry: rotated %v, %v; want a rotation", rotated, err)
	}
	after, err := svc.Instances.ListServerCertificates("proj", "auto").Do()
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveVersion == before.ActiveVersion || len(after.ServerCerts) != 2 {
		t.Errorf("after the rotation: %+v; want a new active version and the old one listed", after)
	}
	cs, err := svc.Connect.Get("proj", "auto").Do()
	if err != nil || cs.ServerCaCert == nil || cs.ServerCaCert.Cert == "" {
		t.Errorf("connect settings after the rotation: %v %+v", err, cs)
	}
	for _, n := range []string{"manual", "internal"} {
		if rotated, err := srv.rotateDuringMaintenance(spec(n), later); err != nil || rotated {
			t.Errorf("%s: rotated %v, %v; want none", n, rotated, err)
		}
	}
}
