package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// An Enterprise edition database's pipelines and user credentials (#1500),
// on its page on the Firestore databases screen: Run pipeline, through the
// official client's pipeline API, shows the results; the User credentials
// tab (Google's Security page, consolefirestoresecurity.go) lists them,
// with Add User on the tab and Disable,
// Enable, Reset password and Delete on a row, through the official Admin
// client. A created or reset password is shown once, in the action's
// result, as Google's console shows it, and is not in the operations log.
// A Standard edition database is offered neither: Google serves both on
// Enterprise databases alone.

// userCredsNameRe is a user's credentials name: its database ID and user.
var userCredsNameRe = regexp.MustCompile(`/databases/([^/]+)/userCreds/([^/]+)$`)

// firestoreCredsSegment is the second segment of a user's action path.
const firestoreCredsSegment = "userCreds"

var pipelineOps = []string{"==", "!=", "<", "<=", ">", ">=", "array-contains", "array-contains-any", "in", "not-in"}

func runPipelineFields() []console.Field {
	return []console.Field{
		{Name: "source", Label: "Source", Type: "select", Options: []string{"collection", "collection_group", "database"}, Default: "collection",
			Help: "The input stage: a collection, every collection with an ID (collection group), or every document of the database."},
		{Name: "path", Label: "Collection path or ID", Type: "text",
			Help: "A collection path (users, or users/alice/orders) or, for a collection group, its ID; empty for the database."},
		{Name: "field", Label: "Where: field (optional)", Type: "text"},
		{Name: "op", Label: "Where: operator", Type: "select", Options: pipelineOps, Default: "=="},
		{Name: "value", Label: "Where: value", Type: "text", Help: "Typed as in the query builder: a number, true, false, null, or a string; comma-separated for in and not-in."},
		{Name: "orderBy", Label: "Sort by (optional)", Type: "text"},
		{Name: "direction", Label: "Direction", Type: "select", Options: []string{"ascending", "descending"}, Default: "ascending"},
		{Name: "limit", Label: "Limit", Type: "text", Pattern: `^[0-9]*$`, Default: "100"},
		{Name: "readTime", Label: "Read time (optional)", Type: "text",
			Help: "An RFC 3339 time, such as 2026-10-17T14:00:00Z: the documents are read as they were then. Not with update or delete."},
		{Name: "output", Label: "Output", Type: "select", Options: []string{"results", "update", "delete"}, Default: "results",
			Help: "results shows the documents; update writes each one back, with Update: set field set to its value (the update stage); delete deletes each one (the delete stage). Limit applies first."},
		{Name: "setField", Label: "Update: set field (optional)", Type: "text"},
		{Name: "setValue", Label: "Update: set value", Type: "text", Help: "Typed as the Where value is."},
		{Name: "explain", Label: "Explain (analyze)", Type: "checkbox", Help: "Also show the explain stats the pipeline returns."},
	}
}

func credsActions(uc *adminpb.UserCreds) []console.Action {
	toggle := console.Action{ID: "disablecreds", Label: "Disable"}
	if uc.GetState() == adminpb.UserCreds_DISABLED {
		toggle = console.Action{ID: "enablecreds", Label: "Enable"}
	}
	return []console.Action{toggle,
		{ID: "resetpassword", Label: "Reset password", Confirm: "The current password stops being the user's; the new one is shown once."},
		{ID: "deletecreds", Label: "Delete", Destructive: true},
	}
}

// userCreds is an Enterprise database's User credentials tab.
func (p firestoreDatabasesProvider) userCreds(ctx context.Context, c *admin.FirestoreAdminClient, project, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Role", "State", "Principal", "Created", "Updated"}, NameColumn: "Username", Noun: "users"}
	resp, err := c.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: db})
	if err != nil {
		list.Unavailable = "listing users: " + err.Error()
		return list
	}
	roles := p.credsRolesOn(ctx, project, id)
	for _, uc := range resp.GetUserCreds() {
		list.Items = append(list.Items, console.Resource{Name: lastSegment(uc.GetName()), Fields: map[string]string{
			"Role":      roleTitles(roles[uc.GetResourceIdentity().GetPrincipal()]),
			"State":     enumWord(uc.GetState(), ""),
			"Principal": uc.GetResourceIdentity().GetPrincipal(),
			"Created":   uc.GetCreateTime().AsTime().Format("2006-01-02 15:04:05 MST"),
			"Updated":   uc.GetUpdateTime().AsTime().Format("2006-01-02 15:04:05 MST"),
		}, Actions: credsActions(uc), ActsOn: []string{id, firestoreCredsSegment, lastSegment(uc.GetName())}})
	}
	list.Total = len(list.Items)
	list.Note = "Users authenticate MongoDB drivers, by SCRAM-SHA-256, to Firestore with MongoDB compatibility: " +
		"use the database's connection string with the user's name and password. A user's role holds on this database alone " +
		"(an IAM binding on the project, conditioned on resource.name); Role lists the project's bindings that hold here."
	return list
}

func (p firestoreDatabasesProvider) passwordListing(uc *adminpb.UserCreds) *console.Listing {
	cols := []string{"Password", "State"}
	fields := map[string]string{"Password": uc.GetSecurePassword(), "State": enumWord(uc.GetState(), "")}
	if m := userCredsNameRe.FindStringSubmatch(uc.GetName()); m != nil {
		if cs := p.mongoConnectionString(m[2], m[1]); cs != "" {
			cols = append(cols, "Connection string")
			fields["Connection string"] = cs
		}
	}
	return &console.Listing{Columns: cols, NameColumn: "User", Noun: "user credentials",
		Items: []console.Resource{{Name: lastSegment(uc.GetName()), Fields: fields}}, Total: 1,
		Note: "Copy the password now: it is shown once, and cannot be read again (Reset password makes a new one)."}
}

// ActAtResult implements console.ResultActor: Run pipeline and the user
// credentials actions that return a password show their result; every
// other action is ActAt's.
func (p firestoreDatabasesProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	switch {
	case action == "runpipeline" && len(path) == 1,
		action == "explainquery" && len(path) == 1,
		action == "createcreds" && len(path) == 1,
		len(path) == 3 && path[1] == firestoreCredsSegment:
	default:
		return nil, p.ActAt(ctx, project, path, action, values)
	}
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	db := p.dbName(project, path[0])
	if action == "runpipeline" {
		return p.runPipeline(ctx, project, path[0], values)
	}
	if action == "explainquery" {
		return p.explainQuery(ctx, db, values)
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	switch action {
	case "createcreds":
		role := values["role"]
		if role != "" {
			if _, ok := credsRoleTitles[role]; !ok {
				return nil, fmt.Errorf("%q is not a role Add User offers", role)
			}
		}
		user := strings.TrimSpace(values["userCredsId"])
		uc, err := c.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: user, UserCreds: &adminpb.UserCreds{}})
		if err != nil {
			return nil, err
		}
		res := p.passwordListing(uc)
		if role != "" {
			// The password is shown even when the grant fails: it cannot
			// be read again.
			if err := p.grantCredsRole(ctx, project, path[0], user, uc.GetResourceIdentity().GetPrincipal(), role); err != nil {
				res.Note += " The role was not granted: " + err.Error()
			} else {
				res.Columns = append(res.Columns, "Role")
				res.Items[0].Fields["Role"] = roleTitles([]string{role})
			}
		}
		return res, nil
	case "resetpassword":
		uc, err := c.ResetUserPassword(ctx, &adminpb.ResetUserPasswordRequest{Name: db + "/userCreds/" + path[2]})
		if err != nil {
			return nil, err
		}
		return p.passwordListing(uc), nil
	case "disablecreds":
		_, err := c.DisableUserCreds(ctx, &adminpb.DisableUserCredsRequest{Name: db + "/userCreds/" + path[2]})
		return nil, err
	case "enablecreds":
		_, err := c.EnableUserCreds(ctx, &adminpb.EnableUserCredsRequest{Name: db + "/userCreds/" + path[2]})
		return nil, err
	case "deletecreds":
		return nil, c.DeleteUserCreds(ctx, &adminpb.DeleteUserCredsRequest{Name: db + "/userCreds/" + path[2]})
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// pipelineCondition is the where stage's condition the form describes.
func pipelineCondition(field, op string, value any) (firestore.BooleanExpression, error) {
	f := firestore.FieldOf(field)
	switch op {
	case "==":
		return firestore.Equal(f, value), nil
	case "!=":
		return firestore.NotEqual(f, value), nil
	case "<":
		return firestore.LessThan(f, value), nil
	case "<=":
		return firestore.LessThanOrEqual(f, value), nil
	case ">":
		return firestore.GreaterThan(f, value), nil
	case ">=":
		return firestore.GreaterThanOrEqual(f, value), nil
	case "array-contains":
		return firestore.ArrayContains(f, value), nil
	case "array-contains-any":
		return firestore.ArrayContainsAny(f, value), nil
	case "in":
		return firestore.EqualAny(f, value), nil
	case "not-in":
		return firestore.NotEqualAny(f, value), nil
	}
	return nil, fmt.Errorf("operator %q is not one of %s", op, strings.Join(pipelineOps, ", "))
}

// runPipeline runs the pipeline the form describes on the database id.
func (p firestoreDatabasesProvider) runPipeline(ctx context.Context, project, id string, values map[string]string) (*console.Listing, error) {
	c, err := firestore.NewClientWithDatabase(ctx, project, id, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Firestore: %w", err)
	}
	defer c.Close()
	path := strings.Trim(strings.TrimSpace(values["path"]), "/")
	var pl *firestore.Pipeline
	switch values["source"] {
	case "collection", "":
		if path == "" {
			return nil, errors.New("a collection path is required")
		}
		pl = c.Pipeline().Collection(path)
	case "collection_group":
		if path == "" || strings.Contains(path, "/") {
			return nil, fmt.Errorf("collection ID %q is not a collection ID", path)
		}
		pl = c.Pipeline().CollectionGroup(path)
	case "database":
		pl = c.Pipeline().Database()
	default:
		return nil, fmt.Errorf("source %q is not collection, collection_group or database", values["source"])
	}
	if field := strings.TrimSpace(values["field"]); field != "" {
		op := strings.TrimSpace(values["op"])
		if op == "" {
			op = "=="
		}
		tvOp := op
		if op == "not-in" || op == "array-contains-any" {
			tvOp = "in"
		}
		v, err := typedValue(values["value"], tvOp)
		if err != nil {
			return nil, err
		}
		cond, err := pipelineCondition(field, op, v)
		if err != nil {
			return nil, err
		}
		pl = pl.Where(cond)
	}
	if ob := strings.TrimSpace(values["orderBy"]); ob != "" {
		o := firestore.Ascending(firestore.FieldOf(ob))
		if values["direction"] == "descending" {
			o = firestore.Descending(firestore.FieldOf(ob))
		}
		pl = pl.Sort(firestore.Orders(o))
	}
	limit := 100
	if l := strings.TrimSpace(values["limit"]); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("limit %q is not a number", l)
		}
		limit = n
	}
	pl = pl.Limit(limit)
	if rt := strings.TrimSpace(values["readTime"]); rt != "" {
		t, err := time.Parse(time.RFC3339Nano, rt)
		if err != nil {
			return nil, fmt.Errorf("read time %q is not an RFC 3339 time", rt)
		}
		pl = pl.WithReadOptions(firestore.ReadTime(t))
	}
	switch values["output"] {
	case "results", "":
	case "update":
		var opts []firestore.UpdateOption
		if f := strings.TrimSpace(values["setField"]); f != "" {
			v, err := typedValue(values["setValue"], "==")
			if err != nil {
				return nil, err
			}
			opts = append(opts, firestore.WithUpdateTransformations(firestore.ConstantOf(v).As(f)))
		}
		pl = pl.Update(opts...)
	case "delete":
		pl = pl.Delete()
	default:
		return nil, fmt.Errorf("output %q is not results, update or delete", values["output"])
	}
	var opts []firestore.ExecuteOption
	if values["explain"] == "true" {
		opts = append(opts, firestore.WithExplainMode(firestore.ExplainModeAnalyze))
	}
	snap := pl.Execute(ctx, opts...)
	rs, err := snap.Results().GetAll()
	if err != nil {
		return nil, err
	}
	out := &console.Listing{Columns: []string{"Fields"}, NameColumn: "Document", Noun: "results"}
	for i, r := range rs {
		name := fmt.Sprintf("result %d", i+1)
		if r.Ref() != nil {
			name = r.Ref().Path[strings.Index(r.Ref().Path, "/documents/")+len("/documents/"):]
		}
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{"Fields": flatten(r.Data())}})
	}
	out.Total = len(out.Items)
	out.Note = "Run by the instance's pipeline engine over the emulator's documents (Firestore Enterprise edition pipelines)."
	if values["explain"] == "true" {
		if text, err := snap.ExplainStats().Text(); err == nil && text != "" {
			out.Note += " Explain stats: " + strings.Join(strings.Fields(text), " ")
		}
	}
	return out, nil
}
