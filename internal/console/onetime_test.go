package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// oneTimeProvider answers a create with a value shown once.
type oneTimeProvider struct{ fakeProvider }

func (oneTimeProvider) CreateForm() (string, []Field) {
	return "Create key", []Field{{Name: "email", Label: "Email", Type: "text"}}
}

func (p oneTimeProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name, _, err := p.CreateOneTime(ctx, project, v)
	return name, err
}

func (oneTimeProvider) CreateOneTime(context.Context, string, map[string]string) (string, *OneTime, error) {
	return "GOOGKEY", &OneTime{Title: "HMAC key created", Label: "Secret", Value: "s3cr3t-shown-once",
		Note: "You won't see this again.", Properties: []Property{{Label: "Access ID", Value: "GOOGKEY"}}}, nil
}

// A one-time value is in the create response, and in neither the operations
// ledger nor the logs, which name what was created (#792).
func TestOneTimeValueIsInTheCreateResponseOnly(t *testing.T) {
	srv := serve(t, oneTimeProvider{fakeProvider{id: "k", title: "Keys"}})
	resp, err := http.Post(srv.URL+"/api/resources/k?project=p", "application/json", strings.NewReader(`{"email":"a@b"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		Name, Operation string
		OneTime         *OneTime
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, %v", resp.StatusCode, err)
	}
	if created.Name != "GOOGKEY" || created.OneTime == nil || created.OneTime.Value != "s3cr3t-shown-once" {
		t.Fatalf("create answered %+v, want the name and the one-time value", created)
	}
	for _, path := range []string{"/api/operations?project=p", "/api/logs", "/api/resources/k?project=p"} {
		code, body := get(t, srv, path, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		if strings.Contains(body, "s3cr3t-shown-once") {
			t.Errorf("%s holds the one-time value: %s", path, body)
		}
		if path != "/api/resources/k?project=p" && !strings.Contains(body, "GOOGKEY") {
			t.Errorf("%s does not name what was created: %s", path, body)
		}
	}
}
