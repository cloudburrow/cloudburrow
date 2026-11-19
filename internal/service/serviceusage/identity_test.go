package serviceusage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/option"
	sub "google.golang.org/api/serviceusage/v1beta1"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// generateServiceIdentity through the official v1beta1 client: the
// Certificate Authority Service agent of the project, the same at every
// call; a served service whose agent CloudBurrow does not know is 501; an
// unserved service is refused as enable refuses it.
func TestGenerateServiceIdentityThroughTheOfficialClient(t *testing.T) {
	srv := NewServer("", []API{{Name: "privateca.googleapis.com"}, {Name: "storage.googleapis.com"}},
		[]Unserved{{Name: "spanner.googleapis.com", Reason: "start the instance with --services spanner"}}, store.NewMemory(), anyProject{})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	c, err := sub.NewService(context.Background(), option.WithEndpoint(hs.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	gen := func(svc string) (map[string]any, error) {
		op, err := c.Services.GenerateServiceIdentity("projects/su-id/services/" + svc).Do()
		if err != nil {
			return nil, err
		}
		if !op.Done || op.Name != "operations/finished.DONE_OPERATION" {
			t.Errorf("%s: operation %s done=%v", svc, op.Name, op.Done)
		}
		var resp map[string]any
		return resp, json.Unmarshal(op.Response, &resp)
	}
	want := fmt.Sprintf("service-%d@gcp-sa-privateca.iam.gserviceaccount.com", resource.ProjectNumber("su-id"))
	a, err := gen("privateca.googleapis.com")
	if err != nil || a["email"] != want || len(a["uniqueId"].(string)) != 21 {
		t.Fatalf("privateca identity = %v, %v; want %s with a 21-digit uniqueId", a, err, want)
	}
	if b, _ := gen("privateca.googleapis.com"); b["uniqueId"] != a["uniqueId"] {
		t.Errorf("uniqueId changed between calls: %v, %v", a["uniqueId"], b["uniqueId"])
	}
	if _, err := gen("storage.googleapis.com"); code(err) != 501 {
		t.Errorf("storage identity: %v; want 501, its agent not guessed", err)
	}
	if _, err := gen("spanner.googleapis.com"); code(err) != 400 {
		t.Errorf("unserved service: %v; want 400 FAILED_PRECONDITION", err)
	}
	if _, err := c.Services.List("projects/su-id").Do(); code(err) != 501 {
		t.Errorf("another v1beta1 method: %v; want 501", err)
	}
}
