package sqladmin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

func TestPasswordValidationPolicy(t *testing.T) {
	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for what, p := range map[string]*sqladmin.PasswordValidationPolicy{
		"reuseInterval 101":         {EnablePasswordPolicy: true, ReuseInterval: 101},
		"complexity":                {EnablePasswordPolicy: true, Complexity: "COMPLEXITY_HIGH"},
		"passwordChangeInterval":    {EnablePasswordPolicy: true, PasswordChangeInterval: "soon"},
		"MySQL passwordChangeInter": {EnablePasswordPolicy: true, PasswordChangeInterval: "60s"},
	} {
		version := "POSTGRES_17"
		if strings.HasPrefix(what, "MySQL") {
			version = "MYSQL_8_4"
		}
		_, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "bad", DatabaseVersion: version,
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", PasswordValidationPolicy: p}}).Do()
		if err == nil || (!strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "412")) {
			t.Errorf("%s: %v, want 400", what, err)
		}
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pw", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{
			MinLength: 10, Complexity: "COMPLEXITY_DEFAULT", ReuseInterval: 2, DisallowUsernameSubstring: true}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	// Not enabled: not checked.
	if _, err := svc.Users.Insert("proj", "pw", &sqladmin.User{Name: "early", Password: "x"}).Do(); err != nil {
		t.Fatalf("a password before the policy is enabled: %v", err)
	}
	// A patch of one field keeps the others.
	op, err = svc.Instances.Patch("proj", "pw", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{EnablePasswordPolicy: true, PasswordChangeInterval: "3600s"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	in, _ := svc.Instances.Get("proj", "pw").Do()
	if p := in.Settings.PasswordValidationPolicy; p == nil || !p.EnablePasswordPolicy || p.MinLength != 10 || p.ReuseInterval != 2 ||
		p.PasswordChangeInterval != "3600s" || p.Complexity != "COMPLEXITY_DEFAULT" {
		t.Fatalf("policy after patch: %+v", p)
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "password validation policy") {
			t.Errorf("%s: %v, want 400 from the policy", what, err)
		}
	}
	_, err = svc.Users.Insert("proj", "pw", &sqladmin.User{Name: "alice", Password: "Sh0rt!"}).Do()
	refused("too short", err)
	_, err = svc.Users.Insert("proj", "pw", &sqladmin.User{Name: "alice", Password: "alllowercase1!"}).Do()
	refused("not complex", err)
	_, err = svc.Users.Insert("proj", "pw", &sqladmin.User{Name: "alice", Password: "Xalice-pass1"}).Do()
	refused("the username", err)
	if _, err := svc.Users.Insert("proj", "pw", &sqladmin.User{Name: "alice", Password: "Good-pass-1"}).Do(); err != nil {
		t.Fatalf("a password that meets the policy: %v", err)
	}
	// The change interval: the password was set just now.
	_, err = svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Other-pass-2"}).Name("alice").Do()
	refused("a change within the interval", err)
	op, err = svc.Instances.Patch("proj", "pw", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		PasswordValidationPolicy: &sqladmin.PasswordValidationPolicy{PasswordChangeInterval: "0s"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if _, err := svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Other-pass-2"}).Name("alice").Do(); err != nil {
		t.Fatalf("a change after the interval: %v", err)
	}
	// Reuse of one of the last two.
	_, err = svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Good-pass-1"}).Name("alice").Do()
	refused("a reused password", err)
	if _, err := svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Third-pass-3"}).Name("alice").Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Fourth-pass-4"}).Name("alice").Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Users.Update("proj", "pw", &sqladmin.User{Name: "alice", Password: "Good-pass-1"}).Name("alice").Do(); err != nil {
		t.Errorf("a password older than the last two: %v", err)
	}
}
