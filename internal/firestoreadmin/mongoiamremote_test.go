package firestoreadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// TestMongoRemoteCheck (#1879): with enforcement on, a token connection's
// commands are decided by the host's evaluator for the token's account on
// the database, not by the pushed roles; a SCRAM principal keeps the
// pushed roles.
func TestMongoRemoteCheck(t *testing.T) {
	var seen []iamcheck.Request
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in iamcheck.Request
		_ = json.NewDecoder(r.Body).Decode(&in)
		seen = append(seen, in)
		held := []string{}
		for _, p := range in.Permissions {
			if p == "datastore.entities.get" || p == "datastore.entities.list" {
				held = append(held, p)
			}
		}
		_ = json.NewEncoder(w).Encode(iamcheck.Decision{Held: held})
	}))
	defer host.Close()
	a := New("")
	a.databases["projects/p/databases/ent"] = &adminpb.Database{Name: "projects/p/databases/ent", DatabaseEdition: adminpb.Database_ENTERPRISE}
	a.UseIAMCheck(&iamcheck.Client{Base: host.URL})
	// The pushed roles grant nothing: only the evaluator's answer counts.
	a.SetMongoIAM(&MongoIAM{})
	s := a.NewMongoServer("", t.Logf)
	conn := &mongoConn{token: true, db: &mongoDB{name: "projects/p/databases/ent", id: "ent"}, principal: "sa@p.iam.gserviceaccount.com"}
	if err := s.checkMongoIAM(conn, "find", "find", bson.D{{Key: "find", Value: "c"}}); err != nil {
		t.Errorf("find with entities.get and list from the evaluator: %v", err)
	}
	err := s.checkMongoIAM(conn, "insert", "insert", bson.D{{Key: "insert", Value: "c"}})
	if err == nil || !strings.Contains(err.Error(), "datastore.entities.create") {
		t.Errorf("insert without entities.create = %v", err)
	}
	if len(seen) == 0 || seen[0].Signer != "sa@p.iam.gserviceaccount.com" || seen[0].Resource != "projects/p/databases/ent" ||
		seen[0].Type != "firestore.googleapis.com/Database" {
		t.Errorf("check requests = %+v", seen)
	}
	n := len(seen)
	user := UserCredsPrincipal("projects/p/databases/ent", "u")
	scram := &mongoConn{db: &mongoDB{name: "projects/p/databases/ent", id: "ent"}, principal: user}
	if err := s.checkMongoIAM(scram, "find", "find", bson.D{{Key: "find", Value: "c"}}); err == nil {
		t.Error("a SCRAM principal with no pushed role read the database")
	}
	if len(seen) != n {
		t.Error("a SCRAM principal was sent to the evaluator")
	}
}
