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
// Not served: a binding's condition (Resource Manager refuses conditions
// here, so a binding is project-wide), custom roles, and a role on the
// database rather than its project. SCRAM user credentials are checked by
// their password, not by their principal's roles.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// MongoIAMPath is where the host pushes MongoIAM to the front.
const MongoIAMPath = "/cloudburrow/v1/firestoreMongoIAM"

// MongoIAM is what the token check reads: the live access tokens this
// instance minted, and each principal's roles, by email (lower case) and
// project ID.
type MongoIAM struct {
	Tokens []MongoIAMToken                 `json:"tokens"`
	Roles  map[string]map[string][]string `json:"roles"`
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

// mongoPerms are the permissions email's roles on project grant.
func (a *Admin) mongoPerms(email, project string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mongoIAM == nil {
		return nil
	}
	var out []string
	for _, role := range a.mongoIAM.Roles[email][project] {
		out = append(out, MongoRoles[role]...)
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
	if mc.principal == "" || mc.db == nil {
		return nil
	}
	project := strings.TrimPrefix(mc.db.name, "projects/")
	project, _, _ = strings.Cut(project, "/")
	perms := s.a.mongoPerms(mc.principal, project)
	var missing []string
	for _, p := range mongoNeeds(lname, cmd) {
		if !grants(perms, p) {
			missing = append(missing, "datastore."+p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return mql.Errorf(13, "Unauthorized", "not authorized on %s to execute command %s: %s lacks %s on project %s",
			mc.db.id, name, mc.principal, strings.Join(missing, ", "), project)
	}
	return nil
}
