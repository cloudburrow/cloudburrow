package main

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	iampb "cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// Firestore with MongoDB compatibility's console pages (#1845, round 3),
// from Google's "Authenticate and connect to a database"
// (https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/connect,
// read 2026-11-20): an Enterprise database's Security page lists its users,
// and Add User takes a Username and a role, then Add user shows the
// generated password once in a confirmation dialog. The role is granted to
// the user's principal on the project, with the condition Google's gcloud
// steps write, resource.name == the database, so it holds on that database
// alone; the MongoDB front enforces it (firestoremongoiam.go). The
// Explorer panel shows the database's base connection string, with no
// credentials, under "Connect using MongoDB tools".

// credsRoles are the roles Add User offers: the Firestore roles that grant
// the MongoDB commands something (firestoreadmin.MongoRoles), named by
// their titles, in least to most access.
var credsRoles = []string{"roles/datastore.viewer", "roles/datastore.user", "roles/datastore.owner"}

var credsRoleTitles = map[string]string{
	"roles/datastore.viewer": "Cloud Datastore Viewer (read)",
	"roles/datastore.user":   "Cloud Datastore User (read and write)",
	"roles/datastore.owner":  "Cloud Datastore Owner (full access)",
}

// addUserAction is the Security page's Add User.
func addUserAction() console.Action {
	return console.Action{ID: "createcreds", Label: "Add User", Title: "Add user", Submit: "Add user", Fields: []console.Field{
		{Name: "userCredsId", Label: "Username", Type: "text", Required: true, Pattern: databaseIDPattern,
			Help: "4 to 63 lowercase letters, numbers and hyphens, starting with a letter. The password is generated and shown once."},
		{Name: "role", Label: "Role", Type: "select", Required: true, Options: credsRoles, OptionLabels: credsRoleTitles,
			Default: "roles/datastore.user", Help: "Granted to the user on this database only, as an IAM binding with the condition resource.name == the database."},
	}}
}

// credsCondition is the condition that limits a role to one database, as
// Google's per-database access steps write it.
func credsCondition(project, dbID string) string {
	return `resource.name == "projects/` + project + `/databases/` + dbID + `"`
}

// projectIAM is Resource Manager's Projects IAM, or nil before it serves.
func (p firestoreDatabasesProvider) projectIAM() iamServer {
	if p.projects == nil {
		return nil
	}
	return resourcemanager.NewProjectsServer(p.projects)
}

// grantCredsRole grants role to a user's principal on one database.
func (p firestoreDatabasesProvider) grantCredsRole(ctx context.Context, project, dbID, user, principal, role string) error {
	if _, ok := credsRoleTitles[role]; !ok {
		return errors.New("choose one of the roles offered")
	}
	api := p.projectIAM()
	if api == nil {
		return errors.New("the user was added, but its role was not granted: Resource Manager is not serving IAM policies")
	}
	res := resourcemanager.V3Name(project)
	pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		return err
	}
	pol.Version = 3
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{principal},
		Condition: &expr.Expr{Title: "Firestore user " + user + " on " + dbID, Expression: credsCondition(project, dbID)}})
	_, err = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: pol})
	return err
}

// credsRolesOn is each principal's roles on the database: the project's
// bindings that are unconditional or limited to this database.
func (p firestoreDatabasesProvider) credsRolesOn(ctx context.Context, project, dbID string) map[string][]string {
	api := p.projectIAM()
	if api == nil {
		return nil
	}
	pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resourcemanager.V3Name(project),
		Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		return nil
	}
	cond := credsCondition(project, dbID)
	out := map[string][]string{}
	for _, b := range pol.GetBindings() {
		if c := b.GetCondition(); c != nil && strings.TrimSpace(c.GetExpression()) != cond {
			continue
		}
		for _, m := range b.GetMembers() {
			out[m] = append(out[m], b.GetRole())
		}
	}
	for m := range out {
		sort.Strings(out[m])
	}
	return out
}

// roleTitles names roles by their titles where Add User offers them.
func roleTitles(roles []string) string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		if t, ok := credsRoleTitles[r]; ok {
			r = strings.TrimSpace(t[:strings.Index(t, " (")])
		}
		names = append(names, r)
	}
	if len(names) == 0 {
		return "None"
	}
	return strings.Join(names, ", ")
}

// baseConnectionString is Google's base connection string, with no
// credentials, for this instance's endpoint and CA
// (https://firebase.google.com/docs/firestore/enterprise/connect).
func (p firestoreDatabasesProvider) baseConnectionString(dbID string) string {
	if p.mongo == "" {
		return ""
	}
	cs := "mongodb://" + p.mongo + "/" + dbID + "?loadBalanced=true&tls=true&retryWrites=false"
	if p.mongoCA != "" {
		cs += "&tlsCAFile=" + url.QueryEscape(p.mongoCA)
	}
	return cs
}
