package firestoreadmin

// Firestore with MongoDB compatibility (#1738): the IAM check of a token
// sign-in, as Google documents it ("Authenticate and connect to a
// database": PLAIN with the user access_token and an access token, and
// MONGODB-OIDC with an access token; "Identity and Access Management":
// the permissions each MongoDB command needs, and the roles that grant
// them), reusing the instance's IAM as Memorystore's IAM auth does
// (redisadmin/iamauth.go, ADR-0006's #1614 amendment):
//
//   - the token must be one this instance minted and still live, checked
//     by its SHA-256 against the instance's log of the tokens it minted
//     (internal/metadata's issued.go), never by asking Google; any other
//     string is refused AuthenticationFailed (18), as Google refuses a
//     token it did not issue;
//   - each command then needs the permissions Google's IAM page lists for
//     it, which the principal's roles on the database's project grant
//     (the project's stored IAM policy: roles/owner, roles/editor,
//     roles/viewer, roles/datastore.owner, roles/datastore.user,
//     roles/datastore.viewer and roles/datastore.indexAdmin, for
//     serviceAccount: and user: members); a command it lacks one for is
//     Unauthorized (13).
//
// The host pushes the live tokens and the principals' roles to the front
// (PUT MongoIAMPath, JSON MongoIAM) every few seconds, and as soon as a
// token is minted, before it is returned, as Memorystore's nodes are given
// theirs. Until the first push no token signs in. A connection already
// signed in keeps working after its token expires, as on Google; its
// permissions follow the policy.
//
// A SCRAM sign-in is checked against the roles of its user credentials'
// principal, principal://firestore.googleapis.com/projects/PROJECT_NUMBER/
// name/databases/DATABASE_ID/userCreds/USERNAME, as Google's "Authenticate
// and connect to a database" grants them ("A new user credential has no
// permissions by default"): the host gives that member's bindings keyed by
// UserCredsPrincipal, its project named by ID. listDatabases on a token
// connection that has not chosen its database needs
// datastore.databases.getMetadata on some project, and lists the
// Enterprise databases of the projects it holds it on.
//
// A custom role (#1743) is evaluated by the permissions it holds, as the
// host gives them. A conditional binding (#1742) grants its role only on
// the databases its condition holds for, evaluated (internal/iamcond) at
// each check with the attributes Google's "Resource attributes for IAM
// Conditions" gives a Firestore database: resource.service
// "firestore.googleapis.com", resource.type
// "firestore.googleapis.com/Database", resource.name
// "projects/PROJECT_ID/databases/DATABASE_ID", and request.time now. That
// is how Google's "Authenticate and connect to a database" scopes a user
// credentials' role to its database (--condition='expression=
// resource.name == "projects/PROJECT_NAME/databases/DATABASE_ID"').
//
// Not served: a role on the database rather than its project.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// MongoIAMPath is where the host pushes MongoIAM to the front.
const MongoIAMPath = "/cloudburrow/v1/firestoreMongoIAM"

// MongoIAM is what the token check reads: the live access tokens this
// instance minted, and each principal's roles, by email (lower case) and
// project ID.
type MongoIAM struct {
	Tokens []MongoIAMToken                `json:"tokens"`
	Roles  map[string]map[string][]string `json:"roles"`
	// CustomRoles are the custom roles (#1743) Roles names, with the
	// datastore permissions each grants (without "datastore."); a deleted
	// or DISABLED one is absent, so it grants nothing.
	CustomRoles map[string][]string `json:"customRoles,omitempty"`
	// Conditional are the roles of conditional bindings, by principal and
	// project ID, each granted where its expression holds (#1742).
	Conditional map[string]map[string][]MongoConditionalRole `json:"conditional,omitempty"`
}

// MongoConditionalRole is a role a conditional binding grants, and its
// condition's CEL expression.
type MongoConditionalRole struct {
	Role       string `json:"role"`
	Expression string `json:"expression"`
}

// rolePerms are the permissions role grants: a predefined role's from
// MongoRoles, a custom role's from the host. The caller holds a.mu.
func (a *Admin) rolePerms(role string) []string {
	if p, ok := MongoRoles[role]; ok {
		return p
	}
	return a.mongoIAM.CustomRoles[role]
}

// MongoIAMToken is a minted access token: its SHA-256 in hex, its account
// and its expiry.
type MongoIAMToken struct {
	Hash    string    `json:"hash"`
	Email   string    `json:"email"`
	Expires time.Time `json:"expires"`
}

// MongoRoles are the roles that grant any permission the MongoDB commands
// need, with the ones they grant, from Google's IAM page.
var MongoRoles = map[string][]string{
	"roles/owner":                {"*"},
	"roles/editor":               {"*"},
	"roles/datastore.owner":      {"*"},
	"roles/datastore.user":       {"databases.get", "databases.getMetadata", "entities.*", "schemas.list"},
	"roles/datastore.viewer":     {"databases.get", "databases.getMetadata", "entities.get", "entities.list", "schemas.get", "schemas.list"},
	"roles/viewer":               {"databases.get", "databases.getMetadata", "entities.get", "entities.list", "schemas.get", "schemas.list"},
	"roles/datastore.indexAdmin": {"databases.getMetadata", "schemas.*"},
}

// grants is whether one of perms grants p.
func grants(perms []string, p string) bool {
	for _, g := range perms {
		if g == "*" || g == p || (strings.HasSuffix(g, ".*") && strings.HasPrefix(p, strings.TrimSuffix(g, "*"))) {
			return true
		}
	}
	return false
}

// SetMongoIAM replaces the token check's tokens and roles.
func (a *Admin) SetMongoIAM(m *MongoIAM) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mongoIAM = m
}

func (a *Admin) serveMongoIAM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT a MongoIAM", http.StatusMethodNotAllowed)
		return
	}
	var m MongoIAM
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.SetMongoIAM(&m)
	w.WriteHeader(http.StatusNoContent)
}

// mongoTokenEmail is the account of a live token this instance minted.
func (a *Admin) mongoTokenEmail(token string) (string, bool) {
	sum := sha256.Sum256([]byte(token))
	h := hex.EncodeToString(sum[:])
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mongoIAM == nil {
		return "", false
	}
	for _, t := range a.mongoIAM.Tokens {
		if t.Hash == h && t.Email != "" && now.Before(t.Expires) {
			return strings.ToLower(t.Email), true
		}
	}
	return "", false
}

// UserCredsPrincipal is the IAM principal of the user credentials user of
// the database dbName (projects/{project ID}/databases/{id}), its project
// named by ID: the key the host gives that member's roles under.
func UserCredsPrincipal(dbName, user string) string {
	project, db, _ := strings.Cut(strings.TrimPrefix(dbName, "projects/"), "/databases/")
	return "principal://firestore.googleapis.com/projects/" + project + "/name/databases/" + db + "/userCreds/" + user
}

// mongoVisibleDatabases are the IDs of the Enterprise databases, in the
// projects where principal's roles grant databases.getMetadata, sorted.
func (a *Admin) mongoVisibleDatabases(principal string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mongoIAM == nil || principal == "" {
		return nil
	}
	var out []string
	for name, db := range a.databases {
		if db.DeleteTime != nil || db.GetDatabaseEdition() != adminpb.Database_ENTERPRISE {
			continue
		}
		project, id, ok := strings.Cut(strings.TrimPrefix(name, "projects/"), "/databases/")
		if !ok {
			continue
		}
		var perms []string
		for _, role := range a.mongoRolesLocked(principal, project, id) {
			perms = append(perms, a.rolePerms(role)...)
		}
		if grants(perms, "databases.getMetadata") {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// mongoGrantsAnywhere reports whether principal's roles grant p on any
// project.
func (a *Admin) mongoGrantsAnywhere(principal, p string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mongoIAM == nil {
		return false
	}
	for _, roles := range a.mongoIAM.Roles[principal] {
		var perms []string
		for _, role := range roles {
			perms = append(perms, a.rolePerms(role)...)
		}
		if grants(perms, p) {
			return true
		}
	}
	// A conditional grant counts where it holds for one of the project's
	// databases.
	for project := range a.mongoIAM.Conditional[principal] {
		for name, db := range a.databases {
			pid, id, ok := strings.Cut(strings.TrimPrefix(name, "projects/"), "/databases/")
			if !ok || pid != project || db.DeleteTime != nil {
				continue
			}
			var perms []string
			for _, role := range a.mongoRolesLocked(principal, project, id) {
				perms = append(perms, a.rolePerms(role)...)
			}
			if grants(perms, p) {
				return true
			}
		}
	}
	return false
}

// mongoRolesLocked are principal's roles on the database db of project:
// its unconditional roles on the project, and the conditional ones whose
// condition holds for the database now. a.mu is held.
func (a *Admin) mongoRolesLocked(principal, project, db string) []string {
	roles := slices.Clone(a.mongoIAM.Roles[principal][project])
	conds := a.mongoIAM.Conditional[principal][project]
	if len(conds) == 0 {
		return roles
	}
	attrs := iamcond.Attributes{
		ResourceName:    "projects/" + project + "/databases/" + db,
		ResourceType:    "firestore.googleapis.com/Database",
		ResourceService: "firestore.googleapis.com",
		RequestTime:     a.now(),
	}
	for _, c := range conds {
		if iamcond.Eval(c.Expression, attrs) {
			roles = append(roles, c.Role)
		}
	}
	return roles
}

// mongoPerms are the permissions email's roles on the database db of
// project grant.
func (a *Admin) mongoPerms(email, project, db string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mongoIAM == nil {
		return nil
	}
	var out []string
	for _, role := range a.mongoRolesLocked(email, project, db) {
		out = append(out, a.rolePerms(role)...)
	}
	return out
}

// mongoNeeds are the permissions a command needs, from Google's IAM page
// ("Required permissions for API methods"); the commands it does not list
// need the permission of the API method they are (createIndexes is an
// index create, dropIndexes an index delete, delete a document delete).
func mongoNeeds(lname string, cmd bson.D) []string {
	switch lname {
	case "find", "aggregate", "distinct":
		return []string{"entities.get", "entities.list"}
	case "count":
		return []string{"entities.list"}
	case "listcollections":
		return []string{"entities.list"}
	case "listdatabases":
		return []string{"databases.getMetadata"}
	case "listindexes":
		return []string{"schemas.list"}
	case "committransaction", "aborttransaction", "killcursors", "getmore":
		return []string{"databases.get"}
	case "insert", "create":
		return []string{"entities.create"}
	case "update":
		need := []string{"entities.get", "entities.list", "entities.update"}
		stmts, _ := mql.Lookup(cmd, "updates").(bson.A)
		for _, s := range stmts {
			if sd, ok := s.(bson.D); ok && mql.Truthy(mql.Lookup(sd, "upsert")) {
				return append(need, "entities.create")
			}
		}
		return need
	case "delete":
		return []string{"entities.get", "entities.list", "entities.delete"}
	case "findandmodify":
		need := []string{"entities.get", "entities.list"}
		if mql.Truthy(mql.Lookup(cmd, "remove")) {
			return append(need, "entities.delete")
		}
		need = append(need, "entities.update")
		if mql.Truthy(mql.Lookup(cmd, "upsert")) {
			need = append(need, "entities.create")
		}
		return need
	case "drop":
		return []string{"schemas.delete"}
	case "createindexes":
		return []string{"schemas.create"}
	case "dropindexes":
		return []string{"schemas.delete"}
	case "explain":
		if inner, ok := cmd[0].Value.(bson.D); ok && len(inner) > 0 {
			return []string{"entities.get", "entities.list"}
		}
	}
	return nil
}

// checkMongoIAM refuses a command of a token connection its principal's
// roles do not grant.
func (s *MongoServer) checkMongoIAM(mc *mongoConn, lname, name string, cmd bson.D) error {
	if mc.principal == "" {
		return nil
	}
	if mc.db == nil {
		// Only listDatabases runs before a token connection chooses its
		// database (dispatch); it needs the permission on some project.
		if lname == "listdatabases" && !s.a.mongoGrantsAnywhere(mc.principal, "databases.getMetadata") {
			return mql.Errorf(13, "Unauthorized", "not authorized on admin to execute command %s: %s lacks datastore.databases.getMetadata on every project",
				name, mc.principal)
		}
		return nil
	}
	project := strings.TrimPrefix(mc.db.name, "projects/")
	project, _, _ = strings.Cut(project, "/")
	var missing []string
	if s.a.iamCheck.Enabled() && !strings.HasPrefix(mc.principal, userCredsPrefix) {
		// With IAM enforcement on (#1879), a token connection's account
		// is checked by the host's evaluator, as every other Firestore
		// call is: the project's, folders' and organization's policies,
		// groups, conditions and deny policies.
		var err error
		if missing, err = s.a.missingRemote(mc.principal, mc.db.name, mongoNeeds(lname, cmd)); err != nil {
			return mql.Errorf(13, "Unauthorized", "not authorized on %s to execute command %s: the IAM check is unavailable: %v", mc.db.id, name, err)
		}
	} else {
		perms := s.a.mongoPerms(mc.principal, project, mc.db.id)
		for _, p := range mongoNeeds(lname, cmd) {
			if !grants(perms, p) {
				missing = append(missing, "datastore."+p)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return mql.Errorf(13, "Unauthorized", "not authorized on %s to execute command %s: %s lacks %s on project %s",
			mc.db.id, name, mc.principal, strings.Join(missing, ", "), project)
	}
	return nil
}

// userCredsPrefix starts a user credentials principal (UserCredsPrincipal).
const userCredsPrefix = "principal://firestore.googleapis.com/"

// UseIAMCheck makes token connections' commands checked by the host's
// evaluator (nil: by the pushed roles).
func (a *Admin) UseIAMCheck(c *iamcheck.Client) { a.iamCheck = c }

// missingRemote are the needs (without "datastore.") email lacks on the
// database, as the host's evaluator decides, with their prefix.
func (a *Admin) missingRemote(email, database string, needs []string) ([]string, error) {
	if len(needs) == 0 {
		return nil, nil
	}
	perms := make([]string, len(needs))
	for i, p := range needs {
		perms[i] = "datastore." + p
	}
	d, err := a.iamCheck.Check(context.Background(), iamcheck.Request{Signer: email, Service: "firestore.googleapis.com",
		Permissions: perms, Resource: database, Type: "firestore.googleapis.com/Database"})
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range perms {
		if !d.Holds(p) {
			missing = append(missing, p)
		}
	}
	return missing, nil
}
