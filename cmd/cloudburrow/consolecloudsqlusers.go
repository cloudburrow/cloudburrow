package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// Users on the Cloud SQL pages (#1301): a database's page offers Add user
// and Delete user, which run the same statements the Cloud SQL Admin API's
// users.insert and users.delete do, through the same engine, so a user made
// on either is the other's.

func (p cloudSQLProvider) engine() *sqladmin.Postgres {
	return &sqladmin.Postgres{Name: sqlAdminPostgresInstance, Version: "POSTGRES_17",
		User: components.CloudSQLUser, Database: components.CloudSQLDatabase,
		HostAddr: func() string { return p.endpoint }}
}

func (p cloudSQLMySQLProvider) engine() *sqladmin.MySQL {
	return &sqladmin.MySQL{Name: sqlAdminMySQLInstance, Version: "MYSQL_8_4",
		AppUser: components.CloudSQLUser, Database: components.CloudSQLDatabase,
		RootPassword: p.creds.RootPassword, HostAddr: func() string { return p.endpoint }}
}

func userActions(withHost bool, note string) []console.Action {
	name := console.Field{Name: "name", Label: "User name", Type: "text", Required: true,
		Help: "Letters, digits, _, $ and -.", Pattern: `^[A-Za-z_][A-Za-z0-9_$-]{0,62}$`}
	add := []console.Field{name, {Name: "password", Label: "Password", Type: "password", Help: note}}
	del := []console.Field{name}
	if withHost {
		host := console.Field{Name: "host", Label: "Host", Type: "text", Help: "MySQL's host part; % when empty, as Cloud SQL's default."}
		add = append(add, host)
		del = append(del, host)
	}
	return []console.Action{
		{ID: "add-user", Label: "Add user", Fields: add},
		{ID: "delete-user", Label: "Delete user", Destructive: true, Fields: del,
			Confirm: "the user is dropped from the server, as users.delete does"},
	}
}

// userAct performs a users action on engine e.
func userAct(ctx context.Context, e sqladmin.Engine, action string, values map[string]string) error {
	u := sqladmin.User{Name: strings.TrimSpace(values["name"]), Host: strings.TrimSpace(values["host"])}
	if u.Name == "" {
		return fmt.Errorf("a user name is required")
	}
	switch action {
	case "add-user":
		return e.CreateUser(ctx, u, values["password"])
	case "delete-user":
		return e.DeleteUser(ctx, u)
	}
	return fmt.Errorf("unknown action %q", action)
}

// DetailActions implements console.PathActor: users on a database's page.
func (p cloudSQLProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return userActions(false, "Stored, not checked: the server trusts local connections (docs/cloudsql.md).")
}

// ActAt implements console.PathActor.
func (p cloudSQLProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("users are managed from a database's page")
	}
	return userAct(ctx, p.engine(), action, values)
}

// DetailActions implements console.PathActor: users on a database's page.
func (p cloudSQLMySQLProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return userActions(true, "MySQL checks it. The user is granted every privilege, as Cloud SQL's built-in users are.")
}

// ActAt implements console.PathActor.
func (p cloudSQLMySQLProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("users are managed from a database's page")
	}
	return userAct(ctx, p.engine(), action, values)
}

var (
	_ console.PathActor = cloudSQLProvider{}
	_ console.PathActor = cloudSQLMySQLProvider{}
)
