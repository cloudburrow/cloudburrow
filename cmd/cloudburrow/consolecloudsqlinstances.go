package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cloudSQLInstancesProvider is Cloud SQL's Instances page (#1330): every
// instance the Admin API serves, the servers `up` runs and the ones created
// through instances.insert, with Create instance and Delete. It reads and
// writes through the Admin API itself, so the page shows what the API and
// gcloud and Terraform see.
type cloudSQLInstancesProvider struct {
	// endpoint is the Admin API's host:port.
	endpoint string
	// versions are the databaseVersion values the instance's images provide.
	versions []string
}

func (cloudSQLInstancesProvider) ID() string    { return "cloudsql-instances" }
func (cloudSQLInstancesProvider) Title() string { return "Cloud SQL instances" }

const cloudSQLInstancesNote = "Each created instance is a server of its own, from the image `up` runs " +
	"(PostgreSQL 17 or MySQL 8.4), reached through the connectors on port 3307. The instances `up` runs " +
	"(postgres, mysql) cannot be deleted here. Backups, replicas, IAM authentication, clone and failover do not exist here."

func (p cloudSQLInstancesProvider) api(ctx context.Context) (*sqladminapi.Service, error) {
	return sqladminapi.NewService(ctx, option.WithEndpoint("http://"+p.endpoint+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(http.DefaultClient))
}

func (p cloudSQLInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Connection name", "Version", "Tier", "Address", "Deletion protection"},
		NameColumn: "Instance ID",
		Noun:       "instances",
		Note:       cloudSQLInstancesNote,
	}
	svc, err := p.api(ctx)
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	resp, err := svc.Instances.List(project).Context(ctx).Do()
	if err != nil {
		l.Unavailable = "cannot reach the Cloud SQL Admin API: " + err.Error()
		return l, nil
	}
	for _, in := range resp.Items {
		addr := ""
		if len(in.IpAddresses) > 0 {
			addr = in.IpAddresses[0].IpAddress
		}
		tier, protect := "", "Off"
		if in.Settings != nil {
			tier = in.Settings.Tier
			if in.Settings.DeletionProtectionEnabled {
				protect = "On"
			}
		}
		l.Items = append(l.Items, console.Resource{Name: in.Name, Status: in.State, Fields: map[string]string{
			"Connection name": in.ConnectionName, "Version": in.DatabaseVersion, "Tier": tier,
			"Address": addr, "Deletion protection": protect,
		}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm implements console.Creator: what instances.insert takes here.
func (p cloudSQLInstancesProvider) CreateForm() (string, []console.Field) {
	def := ""
	if len(p.versions) > 0 {
		def = p.versions[0]
	}
	return "Create instance", []console.Field{
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
			Help: "Lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "databaseVersion", Label: "Database version", Type: "select", Required: true, Options: p.versions, Default: def,
			Help: "The versions of the images this instance runs."},
		{Name: "tier", Label: "Machine type (tier)", Type: "text", Required: true, Default: "db-custom-1-3840",
			Help: "Recorded and reported; the server is not sized by it."},
		{Name: "rootPassword", Label: "Root password (MySQL)", Type: "password",
			Help: "MySQL's root password. Empty leaves root without one, as Cloud SQL does. PostgreSQL's postgres user is trusted locally."},
		{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox"},
	}
}

// Create implements console.Creator. It returns once the API has accepted
// the instance; the page shows it PENDING_CREATE until its server serves.
func (p cloudSQLInstancesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	svc, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(v["name"])
	_, err = svc.Instances.Insert(project, &sqladminapi.DatabaseInstance{
		Name: name, DatabaseVersion: v["databaseVersion"], RootPassword: v["rootPassword"], Region: "us-central1",
		Settings: &sqladminapi.Settings{Tier: strings.TrimSpace(v["tier"]), DeletionProtectionEnabled: v["deletionProtection"] == "true"},
	}).Context(ctx).Do()
	if err != nil {
		return "", sqlAPIError(err)
	}
	return name, nil
}

// Delete implements console.Deleter: deletion protection and the instances
// `up` runs refuse it with the API's own words.
func (p cloudSQLInstancesProvider) Delete(ctx context.Context, project, name string) error {
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	if _, err := svc.Instances.Delete(project, name).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

// sqlAPIError is the Admin API's own message for a failure.
func sqlAPIError(err error) error {
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Message != "" {
		return fmt.Errorf("%s", ge.Message)
	}
	return err
}

// Actions implements console.Actor: Restart, on a created instance.
func (p cloudSQLInstancesProvider) Actions(r console.Resource) []console.Action {
	if r.Name == sqlAdminPostgresInstance || r.Name == sqlAdminMySQLInstance {
		return nil
	}
	return []console.Action{{ID: "restart", Label: "Restart", Confirm: "Restarting the instance closes every connection to it."}}
}

// Act implements console.Actor.
func (p cloudSQLInstancesProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "restart" {
		return fmt.Errorf("unknown action %q", action)
	}
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	if _, err := svc.Instances.Restart(project, name).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

var (
	_ console.Actor   = cloudSQLInstancesProvider{}
	_ console.Creator = cloudSQLInstancesProvider{}
	_ console.Deleter = cloudSQLInstancesProvider{}
)
