package sqladmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// fake is an engine over maps, for the router's tests; the real engines are
// covered by the compat tests against the instance's servers.
type fake struct {
	dbs   map[string]Database
	users map[string]User
}

func newFake() *fake {
	return &fake{dbs: map[string]Database{"cloudburrow": {Name: "cloudburrow", Charset: "UTF8", Collation: "en_US.utf8"}},
		users: map[string]User{"cloudburrow": {Name: "cloudburrow"}}}
}

func (f *fake) Instance() string                                   { return "pg" }
func (f *fake) DatabaseVersion() string                            { return "POSTGRES_17" }
func (f *fake) InstalledVersion(context.Context) (string, error)   { return "POSTGRES_17_11", nil }
func (f *fake) Addr() string                                       { return "127.0.0.1:9019" }
func (f *fake) DeleteDatabase(_ context.Context, n string) error   { delete(f.dbs, n); return nil }
func (f *fake) UpdateDatabase(_ context.Context, d Database) error { f.dbs[d.Name] = d; return nil }
func (f *fake) DeleteUser(_ context.Context, u User) error         { delete(f.users, u.Name); return nil }
func (f *fake) UpdateUser(context.Context, User, string) error     { return nil }
func (f *fake) ListDatabases(context.Context) ([]Database, error) {
	var out []Database
	for _, d := range f.dbs {
		out = append(out, d)
	}
	return out, nil
}
func (f *fake) CreateDatabase(_ context.Context, d Database) error {
	if _, ok := f.dbs[d.Name]; ok {
		return Errorf(http.StatusConflict, "exists")
	}
	f.dbs[d.Name] = d
	return nil
}
func (f *fake) ListUsers(context.Context) ([]User, error) {
	var out []User
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}
func (f *fake) CreateUser(_ context.Context, u User, _ string) error { f.users[u.Name] = u; return nil }

// TestEveryUnservedMethodIsNotImplemented sends every method of the
// discovery document and checks that exactly the ones the table marks
// unimplemented answer 501, so tools/coverage's registry cannot claim a
// method the router does not serve.
func TestEveryUnservedMethodIsNotImplemented(t *testing.T) {
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, m := range Methods {
		path := param.ReplaceAllStringFunc(m.Path, func(p string) string {
			switch p {
			case "{project}", "{projectsId}":
				return "p"
			case "{instance}":
				return "pg"
			}
			return "x"
		})
		req, _ := http.NewRequest(m.HTTPMethod, ts.URL+"/"+path+"?name=x", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := resp.StatusCode == http.StatusNotImplemented
		if got == m.Implemented {
			t.Errorf("%s (%s /%s): status %d, but the table says implemented=%v", m.ID, m.HTTPMethod, path, resp.StatusCode, m.Implemented)
		}
	}
}

func TestDatabasesAndUsersThroughTheGoClient(t *testing.T) {
	ts := httptest.NewServer(NewServer("", newFake()))
	defer ts.Close()
	ctx := context.Background()
	svc, err := sqladmin.NewService(ctx, option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	in, err := svc.Instances.Get("proj", "pg").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.ConnectionName != "proj:us-central1:pg" || in.State != "RUNNABLE" || in.DatabaseVersion != "POSTGRES_17" ||
		len(in.IpAddresses) != 1 || in.IpAddresses[0].IpAddress != "127.0.0.1" {
		t.Errorf("instance: %+v", in)
	}
	op, err := svc.Databases.Insert("proj", "pg", &sqladmin.Database{Name: "orders"}).Do()
	if err != nil || op.Status != "DONE" || op.OperationType != "CREATE_DATABASE" {
		t.Fatalf("insert: %v %+v", err, op)
	}
	got, err := svc.Operations.Get("proj", op.Name).Do()
	if err != nil || got.Status != "DONE" {
		t.Fatalf("operations.get: %v %+v", err, got)
	}
	if _, err := svc.Databases.Insert("proj", "pg", &sqladmin.Database{Name: "orders"}).Do(); err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("second insert: %v, want 409", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "new"}).Do(); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("instances.insert without instance writes: %v, want 501", err)
	}
	if _, err := svc.Users.Delete("proj", "pg").Name("cloudburrow").Do(); err != nil {
		t.Errorf("users.delete: %v", err)
	}
	if _, err := svc.Users.Get("proj", "pg", "cloudburrow").Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("users.get after delete: %v, want 404", err)
	}
}
