package main

import (
	"context"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// TestMemorystoreWords (#1862): the pages say the create pages' words for
// each API's enums, whatever the API's prefix.
func TestMemorystoreWords(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{memorystoreAuthText("AUTH_MODE_IAM_AUTH"), "IAM Auth"},
		{memorystoreAuthText("IAM_AUTH"), "IAM Auth"},
		{memorystoreAuthText("AUTH_MODE_TOKEN_AUTH"), "Token Auth"},
		{memorystoreAuthText("AUTH_MODE_DISABLED"), "Disabled"},
		{memorystoreAuthText("AUTHORIZATION_MODE_UNSPECIFIED"), "Disabled"},
		{memorystoreTLSText("TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION"), "Enabled (TLS)"},
		{memorystoreTLSText("SERVER_AUTHENTICATION"), "Enabled (TLS)"},
		{memorystoreTLSText("TRANSIT_ENCRYPTION_DISABLED"), "Disabled"},
		{memorystorePersistenceText("AOF", "Memory Snapshot"), "Append Only File (AOF)"},
		{memorystorePersistenceText("RDB", "Memory Snapshot"), "Memory Snapshot"},
		{memorystorePersistenceText("RDB", "Redis Database (RDB)"), "Redis Database (RDB)"},
		{memorystorePersistenceText("DISABLED", "Memory Snapshot"), "No persistence"},
		{memorystorePersistenceText("PERSISTENCE_MODE_UNSPECIFIED", "Memory Snapshot"), "No persistence"},
		{memorystoreEncryptionText("CUSTOMER_MANAGED_ENCRYPTION"), "Cloud KMS key"},
		{memorystoreEncryptionText("GOOGLE_DEFAULT_ENCRYPTION"), "Google-managed encryption key"},
		{memorystoreEncryptionText("TYPE_UNSPECIFIED"), "Google-managed encryption key"},
		{memorystoreConnectText("PRIVATE_SERVICE_ACCESS"), "Private services access"},
		{memorystoreConnectText("DIRECT_PEERING"), "Direct peering"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestMemorystoreRedisAdvancedNetworkAndEditPage (#1862): Create a Redis
// instance's Advanced network options reach CreateInstance's connect_mode
// and reserved_ip_range, the page shows them in Google's words, and Edit is
// Google's Edit Redis instance page, saving with Save, its choices in
// words.
func TestMemorystoreRedisAdvancedNetworkAndEditPage(t *testing.T) {
	ctx := context.Background()
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9017, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(ctx, redisFakeProv{}, func(string, string, string) redisadmin.DataPlane { return redisFakePlane{} }, nil, nil)
	p := memorystoreProvider{api: func() *redisadmin.Server { return srv }}
	_, fields := p.CreateForm()
	var section string
	for _, f := range fields {
		if f.Name == "connectMode" {
			section = f.Section
		}
	}
	if section != "Advanced network options" {
		t.Errorf("Connection is under %q; want Advanced network options", section)
	}
	id, err := p.Create(ctx, "p", map[string]string{"id": "psa", "region": redisadmin.Region, "tier": "BASIC", "memorySizeGb": "1",
		"redisVersion": redisadmin.DefaultRedisVersion, "authEnabled": "true", "authorizedNetwork": "default",
		"connectMode": "PRIVATE_SERVICE_ACCESS", "reservedIpRange": "my-allocated-range"})
	if err != nil || id != "psa" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	d, err := p.Detail(ctx, "p", []string{"psa"})
	if err != nil || d.Unavailable != "" || d.Edit == nil {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	props := map[string]string{}
	for _, pr := range memorystoreAllProps(d) {
		props[pr.Label] = pr.Value
	}
	if props["Connection mode"] != "Private services access" || props["IP range"] != "my-allocated-range" ||
		props["AUTH"] != "Enabled" || props["Persistence"] != "No persistence" || props["Tier"] != "Basic" {
		t.Errorf("the page shows %v", props)
	}
	if !d.Edit.Page || d.Edit.Title != "Edit Redis instance" || d.Edit.Submit != "Save" || d.Edit.Label != "Edit" {
		t.Errorf("Edit = %+v; want the Edit Redis instance page, saved with Save", d.Edit)
	}
	for _, f := range d.Edit.Fields {
		switch f.Name {
		case "persistence":
			if f.OptionLabels["DISABLED"] != "No persistence" || f.OptionLabels["ONE_HOUR"] != "RDB snapshot every hour" {
				t.Errorf("Persistence choices = %v", f.OptionLabels)
			}
		case "maintenanceDay":
			if f.OptionLabels["NONE"] != "No window" || f.OptionLabels["SUNDAY"] != "Sunday" {
				t.Errorf("Maintenance window day choices = %v", f.OptionLabels)
			}
		}
	}
	if err := p.Edit(ctx, "p", []string{"psa"}, map[string]string{"displayName": "renamed", "memorySizeGb": "2"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	d, _ = p.Detail(ctx, "p", []string{"psa"})
	for _, pr := range memorystoreAllProps(d) {
		props[pr.Label] = pr.Value
	}
	if props["Display name"] != "renamed" || props["Capacity"] != "2 GB" {
		t.Errorf("after Save, the page shows %v", props)
	}
}

// redisFakeProv starts no server: every instance is at one address, served
// by redisFakePlane.
type redisFakeProv struct{}

func (redisFakeProv) Provision(context.Context, redisadmin.Spec) (string, error) {
	return "127.0.0.1:6390", nil
}
func (redisFakeProv) Remove(context.Context, string) error { return nil }

// redisFakePlane is a running server that answers and keeps nothing.
type redisFakePlane struct{}

func (redisFakePlane) Info(context.Context) (redisadmin.ServerInfo, error) {
	return redisadmin.ServerInfo{}, nil
}
func (redisFakePlane) ConfigSet(context.Context, map[string]string) error { return nil }
func (redisFakePlane) Save(context.Context) (string, error)               { return "", nil }
func (redisFakePlane) Load(context.Context, string, int) error            { return nil }
