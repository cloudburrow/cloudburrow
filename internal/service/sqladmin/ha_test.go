package sqladmin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestHighAvailabilityRecord checks what the API records and answers for a
// REGIONAL instance and its failover (#1408); the servers are fakes.
func TestHighAvailabilityRecord(t *testing.T) {
	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "ha", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("create: %+v", got.Error)
	}
	in, err := svc.Instances.Get("proj", "ha").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.Settings.AvailabilityType != "REGIONAL" || in.GceZone != Region+"-a" || in.SecondaryGceZone != Region+"-b" {
		t.Errorf("REGIONAL instance: %s %s %s", in.Settings.AvailabilityType, in.GceZone, in.SecondaryGceZone)
	}
	if prov.started["ha"].HAPassword == "" {
		t.Error("no standby replication password")
	}
	if _, err := svc.Instances.Failover("proj", "ha", &sqladmin.InstancesFailoverRequest{
		FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion + 1}}).Do(); err == nil ||
		!strings.Contains(err.Error(), "412") {
		t.Errorf("failover with a stale settingsVersion: %v, want 412", err)
	}
	op, err = svc.Instances.Failover("proj", "ha", &sqladmin.InstancesFailoverRequest{
		FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "FAILOVER" {
		t.Fatalf("failover: %+v", got)
	}
	in, _ = svc.Instances.Get("proj", "ha").Do()
	if in.GceZone != Region+"-b" || in.SecondaryGceZone != Region+"-a" || in.ConnectionName != "proj:"+Region+":ha" {
		t.Errorf("after failover: %s %s %s", in.GceZone, in.SecondaryGceZone, in.ConnectionName)
	}
	if sp := prov.started["ha"]; sp.HAPrimary != "b" || sp.HAEpochs[0] != 2 {
		t.Errorf("after failover the record is %+v, want primary b and server a rebuilt at epoch 2", sp)
	}
	// ZONAL again: no standby, no secondary zone; failover is refused.
	op, err = svc.Instances.Patch("proj", "ha", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{AvailabilityType: "ZONAL"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	in, _ = svc.Instances.Get("proj", "ha").Do()
	if in.Settings.AvailabilityType != "ZONAL" || in.SecondaryGceZone != "" || in.GceZone != Region+"-b" {
		t.Errorf("ZONAL: %s %q %s", in.Settings.AvailabilityType, in.SecondaryGceZone, in.GceZone)
	}
	if _, err := svc.Instances.Failover("proj", "ha", &sqladmin.InstancesFailoverRequest{}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("failover of a ZONAL instance: %v, want 400", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "rep", MasterInstanceName: "ha",
		Settings: &sqladmin.Settings{AvailabilityType: "REGIONAL"}}).Do(); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("a REGIONAL replica: %v, want 501", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "bad", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "t", AvailabilityType: "MULTI"}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a bad availabilityType: %v, want 400", err)
	}
	// The standby is not an instance.
	l, _ := svc.Instances.List("proj").Do()
	for _, i := range l.Items {
		if strings.Contains(i.Name, "standby") || strings.HasPrefix(i.Name, "sqlha-") {
			t.Errorf("listed %s", i.Name)
		}
	}
}
