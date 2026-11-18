package firestoreadmin

import (
	"reflect"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestMongoIAMUserCreds (#1738): a SCRAM sign-in's user credentials are an
// IAM principal with no permissions until a role is granted to it, and
// listDatabases on a token connection that has not chosen its database
// needs databases.getMetadata somewhere and lists only those databases.
func TestMongoIAMUserCreds(t *testing.T) {
	a := New("")
	for _, n := range []string{"projects/p/databases/ent", "projects/q/databases/other"} {
		a.databases[n] = &adminpb.Database{Name: n, DatabaseEdition: adminpb.Database_ENTERPRISE}
	}
	a.databases["projects/p/databases/(default)"] = &adminpb.Database{Name: "projects/p/databases/(default)"}
	s := a.NewMongoServer("", t.Logf)
	user := UserCredsPrincipal("projects/p/databases/ent", "app-user")
	if want := "principal://firestore.googleapis.com/projects/p/name/databases/ent/userCreds/app-user"; user != want {
		t.Fatalf("UserCredsPrincipal = %q, want %q", user, want)
	}
	scram := &mongoConn{db: &mongoDB{name: "projects/p/databases/ent", id: "ent"}, principal: user}
	find := bson.D{{Key: "find", Value: "c"}}
	insert := bson.D{{Key: "insert", Value: "c"}}

	a.SetMongoIAM(&MongoIAM{})
	if err := s.checkMongoIAM(scram, "find", "find", find); err == nil {
		t.Error("a user credential with no role read the database")
	}
	a.SetMongoIAM(&MongoIAM{Roles: map[string]map[string][]string{user: {"p": {"roles/datastore.viewer"}}}})
	if err := s.checkMongoIAM(scram, "find", "find", find); err != nil {
		t.Errorf("datastore.viewer: find refused: %v", err)
	}
	if err := s.checkMongoIAM(scram, "insert", "insert", insert); err == nil {
		t.Error("datastore.viewer: insert allowed")
	}

	token := &mongoConn{token: true, principal: "sa@p.iam.gserviceaccount.com"}
	list := bson.D{{Key: "listDatabases", Value: 1}}
	if err := s.checkMongoIAM(token, "listdatabases", "listDatabases", list); err == nil {
		t.Error("listDatabases with no role allowed")
	}
	a.SetMongoIAM(&MongoIAM{Roles: map[string]map[string][]string{"sa@p.iam.gserviceaccount.com": {"p": {"roles/datastore.user"}}}})
	if err := s.checkMongoIAM(token, "listdatabases", "listDatabases", list); err != nil {
		t.Errorf("listDatabases with datastore.user refused: %v", err)
	}
	if got := a.mongoVisibleDatabases("sa@p.iam.gserviceaccount.com"); !reflect.DeepEqual(got, []string{"ent"}) {
		t.Errorf("visible databases = %v, want [ent]", got)
	}
}

// TestMongoIAMCustomRole (#1743): a custom role grants the datastore
// permissions the host says it holds, and nothing else.
func TestMongoIAMCustomRole(t *testing.T) {
	a := New("")
	a.databases["projects/p/databases/ent"] = &adminpb.Database{Name: "projects/p/databases/ent", DatabaseEdition: adminpb.Database_ENTERPRISE}
	s := a.NewMongoServer("", t.Logf)
	conn := &mongoConn{db: &mongoDB{name: "projects/p/databases/ent", id: "ent"}, principal: "sa@p.iam.gserviceaccount.com"}
	find := bson.D{{Key: "find", Value: "c"}}
	insert := bson.D{{Key: "insert", Value: "c"}}
	const role = "projects/p/roles/reader"
	a.SetMongoIAM(&MongoIAM{Roles: map[string]map[string][]string{"sa@p.iam.gserviceaccount.com": {"p": {role}}},
		CustomRoles: map[string][]string{role: {"entities.get", "entities.list"}}})
	if err := s.checkMongoIAM(conn, "find", "find", find); err != nil {
		t.Errorf("a custom role with entities.get and list: find refused: %v", err)
	}
	if err := s.checkMongoIAM(conn, "insert", "insert", insert); err == nil {
		t.Error("a custom role without entities.create: insert allowed")
	}
	// A role the host gives no permissions for (deleted, DISABLED) grants none.
	a.SetMongoIAM(&MongoIAM{Roles: map[string]map[string][]string{"sa@p.iam.gserviceaccount.com": {"p": {role}}}})
	if err := s.checkMongoIAM(conn, "find", "find", find); err == nil {
		t.Error("a custom role with no permissions: find allowed")
	}
}

// TestMongoIAMConditions (#1742): a conditional binding grants its role on
// the databases its condition holds for, as Google's "Authenticate and
// connect to a database" scopes a user credentials' role to its database.
func TestMongoIAMConditions(t *testing.T) {
	a := New("")
	for _, n := range []string{"projects/p/databases/ent", "projects/p/databases/ent2"} {
		a.databases[n] = &adminpb.Database{Name: n, DatabaseEdition: adminpb.Database_ENTERPRISE}
	}
	s := a.NewMongoServer("", t.Logf)
	const sa = "sa@p.iam.gserviceaccount.com"
	find := bson.D{{Key: "find", Value: "c"}}
	on := func(id string) *mongoConn {
		return &mongoConn{db: &mongoDB{name: "projects/p/databases/" + id, id: id}, principal: sa}
	}
	a.SetMongoIAM(&MongoIAM{Conditional: map[string]map[string][]MongoConditionalRole{sa: {"p": {
		{Role: "roles/datastore.user", Expression: `resource.name == "projects/p/databases/ent" && resource.type == "firestore.googleapis.com/Database" && resource.service == "firestore.googleapis.com"`},
		{Role: "roles/datastore.owner", Expression: `request.time < timestamp("2000-01-01T00:00:00Z")`},
	}}}})
	if err := s.checkMongoIAM(on("ent"), "find", "find", find); err != nil {
		t.Errorf("the condition's database: find refused: %v", err)
	}
	if err := s.checkMongoIAM(on("ent2"), "find", "find", find); err == nil {
		t.Error("another database: find allowed")
	}
	if got := a.mongoVisibleDatabases(sa); !reflect.DeepEqual(got, []string{"ent"}) {
		t.Errorf("visible databases = %v, want [ent]", got)
	}
	token := &mongoConn{token: true, principal: sa}
	if err := s.checkMongoIAM(token, "listdatabases", "listDatabases", bson.D{{Key: "listDatabases", Value: 1}}); err != nil {
		t.Errorf("listDatabases with a conditional grant refused: %v", err)
	}
}
