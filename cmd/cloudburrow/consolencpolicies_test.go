package main

import (
	"context"
	"strings"
	"testing"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/networkconnectivity"
)

// TestConsoleConnectionPolicies (#2141): Private Service Connect's
// Connection Policies tab over the Network Connectivity API: Create
// connection policy ending in Create policy, the list's row, the policy's
// page with Edit (Update policy), a refused change, and Delete.
func TestConsoleConnectionPolicies(t *testing.T) {
	ctx := context.Background()
	api, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := networkconnectivity.NewServer(networkconnectivity.Options{Compute: func() networkconnectivity.Compute { return ncCompute{api: api} }})
	if err != nil {
		t.Fatal(err)
	}
	p := ncPoliciesProvider{computeScreen{svc: &computeService{api: api, nc: nc}}}
	title, fields := p.CreateForm()
	if title != "Create connection policy" || p.CreateSubmit() != "Create policy" || fields[3].Default != "google-cloud-sql" {
		t.Fatalf("Create connection policy: %q %+v", title, fields)
	}
	if _, err := p.Create(ctx, "ncp", map[string]string{"name": "bad", "serviceClass": "google-cloud-sql", "network": "default",
		"region": "us-central1", "subnetworks": "nosuch"}); err == nil {
		t.Error("a subnet the network does not have was accepted")
	}
	name, err := p.Create(ctx, "ncp", map[string]string{"name": "sql", "description": "d", "serviceClass": "google-cloud-sql", "network": "default",
		"region": "us-central1", "subnetworks": "default", "limit": "3"})
	if err != nil || name != "us-central1/sql" {
		t.Fatalf("Create policy: %q %v", name, err)
	}
	l, err := p.List(ctx, "ncp")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List: %+v %v", l, err)
	}
	if f := l.Items[0].Fields; f["Network"] != "default" || f["Service class"] != "google-cloud-sql" || f["Subnetworks"] != "default" ||
		f["Connection limit"] != "3" || f["Connections"] != "0" {
		t.Errorf("row %v", f)
	}
	d, err := p.Detail(ctx, "ncp", []string{name})
	if err != nil || d.Edit == nil || d.Edit.Submit != "Update policy" || len(d.Sections) != 2 {
		t.Fatalf("Detail: %+v %v", d, err)
	}
	if err := p.Edit(ctx, "ncp", []string{name}, map[string]string{"subnetworks": "default", "limit": "-1"}); err == nil {
		t.Error("a negative limit was accepted")
	}
	if err := p.Edit(ctx, "ncp", []string{name}, map[string]string{"subnetworks": "default", "limit": ""}); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "ncp"); l.Items[0].Fields["Connection limit"] != "None" {
		t.Errorf("after Update policy: %v", l.Items[0].Fields)
	}
	if err := p.Delete(ctx, "ncp", name); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "ncp"); len(l.Items) != 0 {
		t.Errorf("after Delete: %+v", l.Items)
	}
}

// TestSQLPoolingAndEndpointFields (#2141): Enable Managed Connection Pool
// and Advanced pooling options make connectionPoolConfig, sending only
// the flags changed from Google's defaults; Add an Endpoint's rows make
// pscAutoConnections.
func TestSQLPoolingAndEndpointFields(t *testing.T) {
	if pc := sqlPoolConfig(map[string]string{}); pc != nil {
		t.Errorf("no pooling fields: %+v", pc)
	}
	pc := sqlPoolConfig(map[string]string{"connectionPooling": "true", "poolMode": "session", "pool_max_pool_size": "20",
		"pool_query_wait_timeout": "120", "pool_ignore_startup_parameters": "extra_float_digits"})
	var got []string
	for _, f := range pc.Flags {
		got = append(got, f.Name+"="+f.Value)
	}
	if !pc.ConnectionPoolingEnabled || strings.Join(got, ",") != "pool_mode=session,max_pool_size=20,ignore_startup_parameters=extra_float_digits" {
		t.Errorf("connectionPoolConfig %+v %v", pc, got)
	}
	if pc := sqlPoolConfig(map[string]string{"connectionPooling": "false", "pool_max_pool_size": "20"}); pc.ConnectionPoolingEnabled || len(pc.Flags) != 0 {
		t.Errorf("turned off: %+v", pc)
	}
	f := sqlPoolFields("Connections", nil, &sqladminapi.ConnectionPoolConfig{ConnectionPoolingEnabled: true,
		Flags: []*sqladminapi.ConnectionPoolFlags{{Name: "max_pool_size", Value: "9"}}})
	if f[0].Label != "Enable Managed Connection Pool" || f[0].Default != "true" || f[1].Panel != "Advanced pooling options" || f[2].Default != "9" {
		t.Errorf("fields %+v", f[:3])
	}
	in := &sqladminapi.DatabaseInstance{DatabaseVersion: "POSTGRES_17", Settings: &sqladminapi.Settings{}}
	if !sqlPoolable(in) {
		t.Error("PostgreSQL 17 (Enterprise Plus by default) cannot pool")
	}
	if in.Settings.Edition = "ENTERPRISE"; sqlPoolable(in) {
		t.Error("an Enterprise instance pools")
	}
	rows, err := sqlAutoEndpointRows(`[{"project":"cons","network":"vpc-a"},{"project":"cons","network":"projects/host/global/networks/shared"}]`)
	if err != nil || len(rows) != 2 || rows[0].ConsumerNetwork != "projects/cons/global/networks/vpc-a" || rows[1].ConsumerNetwork != "projects/host/global/networks/shared" ||
		rows[1].ConsumerProject != "cons" {
		t.Errorf("pscAutoConnections %+v %v", rows, err)
	}
	if _, err := sqlAutoEndpointRows(`[{"project":"cons","network":""}]`); err == nil {
		t.Error("an endpoint with no network was accepted")
	}
}
