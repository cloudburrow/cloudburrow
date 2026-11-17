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
