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
	// Google's columns: the name, then its collation type, character set
	// and database type (System for the engine's own, User for the rest).
	dbs := console.Listing{Columns: []string{"Collation type", "Character set", "Database type"}, NameColumn: "Name", Noun: "databases",
		Note: "The instance's databases, through databases.list."}
	var dbNames, userRefs []string
	if list, err := svc.Databases.List(project, in.Name).Context(ctx).Do(); err != nil {
		dbs.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, d := range list.Items {
			dbs.Items = append(dbs.Items, console.Resource{Name: d.Name, Fields: map[string]string{
				"Collation type": orDash(d.Collation), "Character set": orDash(d.Charset), "Database type": sqlDatabaseType(d.Name)}})
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

// sqlDatabaseType is a database's type as the Databases page shows it:
// System for the ones the engine or Cloud SQL makes, User for the rest.
func sqlDatabaseType(name string) string {
	switch name {
	case "postgres", "template0", "template1", "cloudsqladmin", "mysql", "information_schema", "performance_schema", "sys",
		"master", "model", "msdb", "tempdb":
		return "System"
	}
	return "User"
}

// The Add user account dialog's authentication choices, and its MySQL
// host choices.
const (
	sqlAuthBuiltIn, sqlAuthIAM  = "BUILT_IN", "CLOUD_IAM"
	sqlHostAny, sqlHostRestrict = "ANY", "RESTRICT"
)

// databaseUserActions are Create database, Delete database, Add user,
// Change password and Remove on a RUNNABLE instance's page, as Google's
// Databases and Users pages have them: Create database opens "New
// database", ending in Create; Add user account opens "Add a user account
// to instance NAME" with the authentication choice (Built-in
// authentication, or Cloud IAM), ending in Add; a user's Remove asks for
// nothing typed.
func databaseUserActions(in *sqladminapi.DatabaseInstance, dbNames, userRefs []string) []console.Action {
	mysql := strings.HasPrefix(in.DatabaseVersion, "MYSQL_")
	create := []console.Field{{Name: "dbName", Label: "Database name", Type: "text", Required: true, Pattern: "^[A-Za-z_][A-Za-z0-9_$]{0,62}$"}}
	if mysql {
		create = append(create, console.Field{Name: "charset", Label: "Character set", Type: "text", Help: "optional, such as utf8mb4"},
			console.Field{Name: "collation", Label: "Collation", Type: "text", Help: "optional, such as utf8mb4_0900_ai_ci"})
	}
	acts := []console.Action{{ID: "create-database", Label: "Create database", Title: "New database", Submit: "Create", Fields: create}}
	if len(dbNames) > 0 {
		acts = append(acts, console.Action{ID: "drop-database", Label: "Delete database", Destructive: true, ConfirmTitle: "Delete database",
			Confirm: "The database and every table in it are deleted.",
			Fields:  []console.Field{{Name: "dbName", Label: "Database", Type: "select", Required: true, Options: dbNames}}})
	}
	builtIn := map[string]string{"authentication": sqlAuthBuiltIn}
	iam := map[string]string{"authentication": sqlAuthIAM}
	add := []console.Field{
		{Name: "authentication", Label: "Authentication", Type: "radio", Default: sqlAuthBuiltIn,
			Options:      []string{sqlAuthBuiltIn, sqlAuthIAM},
			OptionLabels: map[string]string{sqlAuthBuiltIn: "Built-in authentication", sqlAuthIAM: "Cloud IAM"},
			Help:         "Built-in authentication: a user name and password of the database. Cloud IAM: a principal, signing in with an access token once the instance's IAM authentication flag is on (#1944)."},
		{Name: "userName", Label: "User name", Type: "text", Required: true, VisibleWhen: builtIn},
		{Name: "password", Label: "Password", Type: "password", VisibleWhen: builtIn, Help: "Optional."},
		{Name: "userPasswordPolicy", Label: "Set user password policy", Type: "checkbox", VisibleWhen: builtIn,
			Unavailable: "Not available locally: a user's own password policy (users.passwordPolicy) is not served; the instance's password policy applies."}}
	if mysql {
		add = append(add, console.Field{Name: "hostChoice", Label: "Host name", Type: "radio", Default: sqlHostAny, VisibleWhen: builtIn,
			Options:      []string{sqlHostAny, sqlHostRestrict},
			OptionLabels: map[string]string{sqlHostAny: "Allow any host (%)", sqlHostRestrict: "Restrict host by IP address or address range"}},
			console.Field{Name: "host", Label: "Host", Type: "text", Required: true,
				VisibleWhen: map[string]string{"authentication": sqlAuthBuiltIn, "hostChoice": sqlHostRestrict},
				Help:        "An IP address or range, such as 10.0.0.%"})
	}
	add = append(add,
		console.Field{Name: "principal", Label: "Principal", Type: "text", Required: true, VisibleWhen: iam,
			Help: "a user's or service account's email; it signs in with an IAM access token, and needs roles/cloudsql.instanceUser"},
		console.Field{Name: "type", Label: "Type", Type: "select", Required: true, Default: "CLOUD_IAM_USER", VisibleWhen: iam,
			Options:      []string{"CLOUD_IAM_USER", "CLOUD_IAM_SERVICE_ACCOUNT"},
			OptionLabels: map[string]string{"CLOUD_IAM_USER": "IAM user", "CLOUD_IAM_SERVICE_ACCOUNT": "IAM service account"}})
	acts = append(acts, console.Action{ID: "add-user", Label: "Add user account", Title: "Add a user account to instance " + in.Name,
		Submit: "Add", Fields: add})
	if len(userRefs) > 0 {
		change := []console.Field{{Name: "user", Label: "User", Type: "select", Required: true, Options: userRefs},
			{Name: "password", Label: "New password", Type: "password", Required: true}}
		if mysql {
			change = append(change, console.Field{Name: "retainCurrentPassword", Label: "Retain current password", Type: "checkbox",
				Unavailable: "Not available locally: MySQL's dual passwords (RETAIN CURRENT PASSWORD) are not set through the Admin API here."})
		}
		acts = append(acts,
			console.Action{ID: "change-password", Label: "Change password", Fields: change, Submit: "OK"},
			console.Action{ID: "remove-user", Label: "Remove", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
				ConfirmTitle: "Remove user account?",
				Confirm:      "The user can no longer connect; what it owns stays.",
				Fields:       []console.Field{{Name: "user", Label: "User", Type: "select", Required: true, Options: userRefs}}})
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
		if v["authentication"] == sqlAuthIAM {
			return databaseUserAct(ctx, svc, project, instance, "add-iam-user", v)
		}
		if strings.TrimSpace(v["userName"]) == "" {
			return true, errors.New("a user name is required")
		}
		host := strings.TrimSpace(v["host"])
		if v["hostChoice"] == sqlHostAny {
			host = "%"
		}
		_, err = svc.Users.Insert(project, instance, &sqladminapi.User{Name: strings.TrimSpace(v["userName"]),
			Host: host, Password: v["password"]}).Context(ctx).Do()
	case "add-iam-user":
		// Add user account's Cloud IAM choice; still answered under its own
		// ID for a page drawn before the two were one dialog.
		principal := strings.TrimSpace(v["principal"])
		if principal == "" {
			return true, errors.New("a principal is required")
		}
		typ := v["type"]
		if typ == "" {
			typ = "CLOUD_IAM_USER"
		}
		_, err = svc.Users.Insert(project, instance, &sqladminapi.User{Name: principal, Type: typ}).Context(ctx).Do()
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
