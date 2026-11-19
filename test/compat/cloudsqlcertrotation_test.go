//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLServerCertRotationDuringMaintenance (#1524), in the sql
// shard, through the official Go client (google.golang.org/api/sqladmin/v1)
// and the Go connector: a CUSTOMER_MANAGED_CAS_CA instance with
// serverCertificateRotationMode AUTOMATIC_ROTATION_DURING_MAINTENANCE,
// whose CA pool's issuing CA is valid for 100 days, so its server
// certificate expires within 180 days, as Google's authorize-ssl page
// sets the bound. instances.get reads the mode back; a maintenance event
// (CloudBurrow's control endpoint), run IMMEDIATE, rotates the server
// certificate: listServerCertificates' activeVersion is a new one, the old
// one still listed; connect.get's serverCaCert still holds the chain; and
// the connector is presented the new certificate.
// covers: sql.instances.ListServerCertificates, sql.connect.get, sql.projects.instances.rescheduleMaintenance
func TestCloudSQLServerCertRotationDuringMaintenance(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	shardEndpoint(h, envSQLPrivateCA, EnvPrivateCA)
	cas := privateCAClients(t, h)["grpc"]
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql,privateca")
	}
	ctx := h.Context()
	project := h.Project()
	n := time.Now().UnixNano() % 1e6
	_, pool := sqlCAHierarchyFor(t, ctx, cas, project, fmt.Sprintf("rotca-%d", n), 100*24*time.Hour)
	name := fmt.Sprintf("rotm-%d", n)
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-central1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: &sqladmin.IpConfiguration{ServerCaMode: "CUSTOMER_MANAGED_CAS_CA",
			ServerCaPool: pool, ServerCertificateRotationMode: "AUTOMATIC_ROTATION_DURING_MAINTENANCE"}}})
	if ic := in.Settings.IpConfiguration; ic == nil || ic.ServerCertificateRotationMode != "AUTOMATIC_ROTATION_DURING_MAINTENANCE" {
		t.Fatalf("ipConfiguration = %+v", in.Settings.IpConfiguration)
	}
	before, err := svc.Instances.ListServerCertificates(project, name).Do()
	if err != nil || len(before.ServerCerts) != 1 {
		t.Fatalf("listServerCertificates: %+v, %v", before, err)
	}
	if exp, _ := time.Parse(time.RFC3339Nano, before.ServerCerts[0].ExpirationTime); time.Until(exp) > 180*24*time.Hour {
		t.Fatalf("the server certificate expires %s: want within 180 days, under a 100-day issuing CA", before.ServerCerts[0].ExpirationTime)
	}
	if fp := fingerprintOf(connectorPeerChain(t, h, in.ConnectionName)[0]); fp != before.ActiveVersion {
		t.Fatalf("the connector is presented %s, the active version is %s", fp, before.ActiveVersion)
	}

	scheduleSQLMaintenance(t, h, project, name, time.Time{})
	if _, err := svc.Projects.Instances.RescheduleMaintenance(project, name, &sqladmin.SqlInstancesRescheduleMaintenanceRequestBody{
		Reschedule: &sqladmin.Reschedule{RescheduleType: "IMMEDIATE"}}).Do(); err != nil {
		t.Fatalf("IMMEDIATE: %v", err)
	}
	var after *sqladmin.InstancesListServerCertificatesResponse
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(time.Second) {
		got, err := svc.Instances.Get(project, name).Do()
		if err == nil && got.State == "RUNNABLE" && got.ScheduledMaintenance == nil {
			if after, err = svc.Instances.ListServerCertificates(project, name).Do(); err == nil && after.ActiveVersion != before.ActiveVersion {
				break
			}
		}
		if time.Now().After(end) {
			t.Fatalf("after maintenance: %+v, %+v, %v", got, after, err)
		}
	}
	listed := false
	for _, c := range after.ServerCerts {
		listed = listed || c.Sha1Fingerprint == before.ActiveVersion
	}
	if !listed || len(after.ServerCerts) != 2 {
		t.Errorf("after the rotation, listServerCertificates = %d versions (the old one listed: %v); want the new one and the previous", len(after.ServerCerts), listed)
	}
	cs, err := svc.Connect.Get(project, name).Do()
	if err != nil || cs.ServerCaCert == nil || !strings.Contains(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE") {
		t.Fatalf("connect.get after the rotation: %+v, %v", cs, err)
	}
	eventually(t, "the connector is presented the rotated certificate", func() error {
		if fp := fingerprintOf(connectorPeerChain(t, h, in.ConnectionName)[0]); fp != after.ActiveVersion {
			return fmt.Errorf("presented %s, active %s", fp, after.ActiveVersion)
		}
		return nil
	})
}
