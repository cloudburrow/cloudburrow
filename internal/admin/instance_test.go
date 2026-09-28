package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestInstanceInfoSaysWhatResetSeedAndStateAccept (#801): GET /admin/instance
// lists the reset components in reset order with whether each can be scoped
// to a project, the startup seed's components (none until up records one),
// the seedable components, and the manifest an export would carry; and what
// it says is what the handlers do: a project reset of a component it marks
// unscoped is refused, one it marks scoped succeeds, and reseed is refused
// while it lists no startup seed.
func TestInstanceInfoSaysWhatResetSeedAndStateAccept(t *testing.T) {
	a, _, _ := stateAPI()
	calls := []string{}
	a.RegisterResetter(projectResetter{fakeResetter{name: "tasks", calls: &calls}}, fakeResetter{name: "logging"})
	a.RegisterSeeder(fakeSeeder{name: "tasks"})
	a.RegisterSeeder(fakeSeeder{name: "secretmanager"})
	srv := serve(a)
	defer srv.Close()

	info := func() InstanceInfo {
		t.Helper()
		resp, err := http.Get(srv.URL + "/admin/instance")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got InstanceInfo
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/instance = %d: %v", resp.StatusCode, err)
		}
		return got
	}
	got := info()
	if len(got.Reset) != 2 || got.Reset[0] != (ResetTarget{"tasks", true}) || got.Reset[1] != (ResetTarget{"logging", false}) {
		t.Errorf("reset = %+v, want tasks (by project) then logging", got.Reset)
	}
	if strings.Join(got.Seed, ",") != "secretmanager,tasks" || len(got.Reseed) != 0 {
		t.Errorf("seed = %q, reseed = %q", got.Seed, got.Reseed)
	}
	var names []string
	for _, s := range got.State.Services {
		names = append(names, s.Name)
		if s.Name == "pubsub" && (s.Captured || s.Reason == "") {
			t.Errorf("pubsub = %+v, want not captured with its reason", s)
		}
	}
	if strings.Join(names, ",") != "pubsub,secretmanager,tasks" || !got.State.ContainsSecretValues || got.State.Format != StateFormat {
		t.Errorf("state = %+v", got.State)
	}

	if code, body := post(t, srv.URL+"/admin/reset?reseed=true", ""); code != http.StatusBadRequest {
		t.Errorf("reseed with no startup seed listed = %d %s, want 400", code, body)
	}
	if code, body := post(t, srv.URL+"/admin/reset?service=logging&project=p", ""); code != http.StatusBadRequest {
		t.Errorf("a project reset of logging, listed as unscoped = %d %s, want 400", code, body)
	}
	if code, body := post(t, srv.URL+"/admin/reset?service=tasks&project=p", ""); code != http.StatusOK || strings.Join(calls, ",") != "tasks@p" {
		t.Errorf("a project reset of tasks, listed as scoped = %d %s, calls %q", code, body, calls)
	}

	plan, err := a.PlanSeed([]byte(`{"components":{"tasks":{}}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	a.SetStartupSeed(plan)
	if got := info(); strings.Join(got.Reseed, ",") != "tasks" {
		t.Errorf("reseed after up recorded a startup seed = %q, want tasks", got.Reseed)
	}
}

// TestInstanceInfoNeedsTheToken: the route is behind the admin token like
// every other (#553).
func TestInstanceInfoNeedsTheToken(t *testing.T) {
	a := NewAPI(NewRecorder(10, nil))
	a.RequireToken("tok")
	srv := serve(a)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/instance")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /admin/instance without the token = %d, want 401", resp.StatusCode)
	}
}
