package main

// An instance's Databases and Users tabs on the Cloud SQL Instances page,
// through the Admin API's databases and users methods. The PostgreSQL and
// MySQL pages manage the two servers `up` runs; an instance made on the
// Instances page had no console surface for its databases or users until
// these (the console parity audit), and a user's password could be changed
// only with SQL.

import (
	"context"
	"errors"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// sqlUserRef is a user as the Users tab and its pickers name it: name, or
// name@host for a MySQL user with a host other than %.
func sqlUserRef(u *sqladminapi.User) string {
	if u.Host != "" && u.Host != "%" {
		return u.Name + "@" + u.Host
	}
	return u.Name
}

func parseSQLUserRef(ref string) (name, host string) {
	name, host, _ = strings.Cut(strings.TrimSpace(ref), "@")
	return name, host
}

// instanceDatabasesAndUsers are the Databases and Users tabs of a RUNNABLE
// instance, and the names their actions pick from.
func instanceDatabasesAndUsers(ctx context.Context, svc *sqladminapi.Service, project string, in *sqladminapi.DatabaseInstance) (
	[]console.Section, []string, []string) {
	dbs := console.Listing{Columns: []string{"Character set", "Collation"}, NameColumn: "Database", Noun: "databases",
		Note: "The instance's databases, through databases.list."}
	var dbNames, userRefs []string
	if list, err := svc.Databases.List(project, in.Name).Context(ctx).Do(); err != nil {
		dbs.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, d := range list.Items {
			dbs.Items = append(dbs.Items, console.Resource{Name: d.Name, Fields: map[string]string{
				"Character set": orDash(d.Charset), "Collation": orDash(d.Collation)}})
			dbNames = append(dbNames, d.Name)
		}
		dbs.Total = len(dbs.Items)
	}
	users := console.Listing{Columns: []string{"Host", "Type"}, NameColumn: "User", Noun: "users",
		Note: "The instance's users, through users.list. A password is never shown; Change password sets a new one."}
	if list, err := svc.Users.List(project, in.Name).Context(ctx).Do(); err != nil {
		users.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, u := range list.Items {
			users.Items = append(users.Items, console.Resource{Name: sqlUserRef(u), Fields: map[string]string{
				"Host": orDash(u.Host), "Type": orDash(u.Type)}})
			userRefs = append(userRefs, sqlUserRef(u))
		}
		users.Total = len(users.Items)
	}
	return []console.Section{{ID: "databases", Label: "Databases", Kind: console.KindListing, Listing: dbs},
		{ID: "users", Label: "Users", Kind: console.KindListing, Listing: users}}, dbNames, userRefs
}

// databaseUserActions are Create database, Delete database, Add user,
// Change password and Delete user on a RUNNABLE instance's page.
func databaseUserActions(in *sqladminapi.DatabaseInstance, dbNames, userRefs []string) []console.Action {
	mysql := strings.HasPrefix(in.DatabaseVersion, "MYSQL_")
	create := []console.Field{{Name: "dbName", Label: "Database name", Type: "text", Required: true, Pattern: "^[A-Za-z_][A-Za-z0-9_$]{0,62}$"}}
	if mysql {
		create = append(create, console.Field{Name: "charset", Label: "Character set", Type: "text", Help: "optional, such as utf8mb4"},
			console.Field{Name: "collation", Label: "Collation", Type: "text", Help: "optional, such as utf8mb4_0900_ai_ci"})
	}
	acts := []console.Action{{ID: "create-database", Label: "Create database", Fields: create}}
	if len(dbNames) > 0 {
		acts = append(acts, console.Action{ID: "drop-database", Label: "Delete database", Destructive: true,
			Confirm: "The database and every table in it are deleted.",
			Fields:  []console.Field{{Name: "dbName", Label: "Database", Type: "select", Required: true, Options: dbNames}}})
	}
	add := []console.Field{{Name: "userName", Label: "User name", Type: "text", Required: true},
		{Name: "password", Label: "Password", Type: "password", Required: true}}
	if mysql {
		add = append(add, console.Field{Name: "host", Label: "Host", Type: "text", Default: "%", Help: "the hosts the user may connect from; % is any"})
	}
	acts = append(acts, console.Action{ID: "add-user", Label: "Add user account", Fields: add})
	if len(userRefs) > 0 {
		acts = append(acts,
			console.Action{ID: "change-password", Label: "Change password", Fields: []console.Field{
				{Name: "user", Label: "User", Type: "select", Required: true, Options: userRefs},
				{Name: "password", Label: "New password", Type: "password", Required: true}}},
			console.Action{ID: "remove-user", Label: "Delete user account", Destructive: true,
				Confirm: "The user can no longer connect; what it owns stays.",
				Fields:  []console.Field{{Name: "user", Label: "User", Type: "select", Required: true, Options: userRefs}}})
	}
	return acts
}

// databaseUserAct performs databaseUserActions; ok is false for any other
// action.
func databaseUserAct(ctx context.Context, svc *sqladminapi.Service, project, instance, action string, v map[string]string) (bool, error) {
	var err error
	switch action {
	case "create-database":
		_, err = svc.Databases.Insert(project, instance, &sqladminapi.Database{Name: strings.TrimSpace(v["dbName"]),
			Charset: strings.TrimSpace(v["charset"]), Collation: strings.TrimSpace(v["collation"])}).Context(ctx).Do()
	case "drop-database":
		_, err = svc.Databases.Delete(project, instance, strings.TrimSpace(v["dbName"])).Context(ctx).Do()
	case "add-user":
		if strings.TrimSpace(v["userName"]) == "" || v["password"] == "" {
			return true, errors.New("a user name and a password are required")
		}
		_, err = svc.Users.Insert(project, instance, &sqladminapi.User{Name: strings.TrimSpace(v["userName"]),
			Host: strings.TrimSpace(v["host"]), Password: v["password"]}).Context(ctx).Do()
	case "change-password":
		if v["password"] == "" {
			return true, errors.New("a new password is required")
		}
		name, host := parseSQLUserRef(v["user"])
		call := svc.Users.Update(project, instance, &sqladminapi.User{Name: name, Host: host, Password: v["password"]}).Name(name)
		if host != "" {
			call = call.Host(host)
		}
		_, err = call.Context(ctx).Do()
	case "remove-user":
		name, host := parseSQLUserRef(v["user"])
		call := svc.Users.Delete(project, instance).Name(name)
		if host != "" {
			call = call.Host(host)
		}
		_, err = call.Context(ctx).Do()
	default:
		return false, nil
	}
	if err != nil {
		return true, sqlAPIError(err)
	}
	return true, nil
}
