package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleListsFollowTheServicesFaultRules (#594): the Cloud Tasks,
// Secret Manager and Cloud Scheduler screens read the store the in-process
// service serves, as that service's List call, so a fault rule on the method
// fails the screen with the message an SDK would receive, is recorded as a
// fault, stays inside its project scope and stops when its count runs out.
func TestConsoleListsFollowTheServicesFaultRules(t *testing.T) {
	rec := admin.NewRecorder(100, nil)
	api := admin.NewAPI(rec)
	faults := api.Faults()
	for _, s := range []string{"tasks", "secretmanager", "scheduler"} {
		faults.Interpose(s)
	}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addRule := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/admin/faults", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add rule %s = %d: %s", body, resp.StatusCode, b)
		}
	}

	schedSvc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := schedSvc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = schedSvc.Stop(context.Background()) })
	for _, c := range []struct {
		service, method string
		provider        console.Provider
	}{
		{"tasks", "ListQueues", tasksProvider{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}, faults: faults}},
		{"secretmanager", "ListSecrets", secretsProvider{svc: &secretsService{store: secrets.NewStore(store.NewMemory())}, faults: faults}},
		{"scheduler", "ListJobs", schedulerProvider{svc: schedSvc, faults: faults}},
	} {
		t.Run(c.service, func(t *testing.T) {
			ctx := context.Background()
			addRule(`{"service":"` + c.service + `","method":"` + c.method + `","project":"faulted","code":"UNAVAILABLE","count":1}`)

			if _, err := c.provider.List(ctx, "other"); err != nil {
				t.Fatalf("a project outside the rule's scope was faulted: %v", err)
			}
			_, err := c.provider.List(ctx, "faulted")
			if err == nil {
				t.Fatalf("%s under an UNAVAILABLE rule listed without error", c.method)
			}
			// The status the service's interceptor returns, which the
			// console's list handler renders as "Unavailable: <message>".
			if st := status.Convert(err); st.Code() != codes.Unavailable || !strings.HasPrefix(st.Message(), "injected fault (rule ") {
				t.Errorf("the list failed with %v; want the service's own UNAVAILABLE injected-fault status", err)
			}
			if _, err := c.provider.List(ctx, "faulted"); err != nil {
				t.Errorf("a count:1 rule faulted a second list: %v", err)
			}
			found := false
			for _, e := range rec.Events(c.service, 100) {
				if e.Kind == "fault" && strings.HasSuffix(e.Target, "/"+c.method) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s fault was recorded for /admin/events", c.method)
			}
		})
	}
}

// faultConsole is a console attached to an admin API that requires token,
// as up attaches it (#800), with tasks and secretmanager interposed and
// pubsub enabled but not.
func faultConsole(t *testing.T, token string) (*admin.API, *httptest.Server) {
	t.Helper()
	api := admin.NewAPI(admin.NewRecorder(100, nil))
	api.RequireToken(token)
	api.Faults().Interpose("tasks")
	api.Faults().Interpose("secretmanager")
	api.Faults().Enabled("tasks", "secretmanager", "pubsub")
	c := console.New("127.0.0.1:0", nil)
	c.SetFaults(adminHandler(api), "/state/demo/admin-token")
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return api, srv
}

// faultRequest sends one request to the console's fault endpoints. token is
// sent as the page sends it, when set; headers are set after it.
func faultRequest(t *testing.T, srv *httptest.Server, method, path, body, token string, headers map[string]string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ruleCount is how many rules the admin API holds, read with the token.
func ruleCount(t *testing.T, api *admin.API, token string) int {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/faults", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct{ Faults []admin.FaultRule }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/faults = %d: %v", resp.StatusCode, err)
	}
	return len(list.Faults)
}

const faultTestToken = "console-fault-test-token"

// TestConsoleFaultsRefuseARequestWithoutTheAdminToken (#800, #553): the
// console's fault endpoints add no token of their own, so a request from
// outside the browser without the instance's admin token is refused with the
// admin API's 401 and changes nothing, even one that passes the console's
// own guards as a workload's would (Host host.docker.internal, no Origin, no
// fetch metadata) or carries same-origin headers; a wrong token is refused
// too.
func TestConsoleFaultsRefuseARequestWithoutTheAdminToken(t *testing.T) {
	api, srv := faultConsole(t, faultTestToken)
	rule := `{"service":"tasks","code":"UNAVAILABLE"}`
	if code, body := faultRequest(t, srv, http.MethodPost, "/api/faults", rule, faultTestToken, nil); code != http.StatusCreated {
		t.Fatalf("POST /api/faults with the token = %d: %s", code, body)
	}
	for _, c := range []struct {
		name    string
		token   string
		headers map[string]string
	}{
		{"no token", "", nil},
		{"a workload on Docker Desktop", "", map[string]string{"Host": "host.docker.internal:9090"}},
		{"same-origin headers", "", map[string]string{"Host": "127.0.0.1:9090", "Origin": "http://127.0.0.1:9090", "Sec-Fetch-Site": "same-origin"}},
		{"a wrong token", "not-the-token", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/faults", ""},
				{http.MethodPost, "/api/faults", rule},
				{http.MethodDelete, "/api/faults?all=true", ""},
				{http.MethodDelete, "/api/faults?id=fault-1", ""},
			} {
				code, body := faultRequest(t, srv, r.method, r.path, r.body, c.token, c.headers)
				if code != http.StatusUnauthorized || !strings.Contains(body, "admin token") ||
					!strings.Contains(body, `"token_file":"/state/demo/admin-token"`) {
					t.Errorf("%s %s = %d %s; want the admin API's 401, naming the token file", r.method, r.path, code, body)
				}
				if strings.Contains(body, faultTestToken) {
					t.Errorf("%s %s: the refusal carries the token", r.method, r.path)
				}
			}
			if n := ruleCount(t, api, faultTestToken); n != 1 {
				t.Errorf("after refused requests the admin API holds %d rules, want the 1 added with the token", n)
			}
		})
	}
}

// TestConsoleFaultsRefuseACrossOriginRequest: the console's same-origin
// and Host guards apply to the fault endpoints as to the rest of /api, so a
// page on another site cannot add or delete a rule even with the token.
func TestConsoleFaultsRefuseACrossOriginRequest(t *testing.T) {
	api, srv := faultConsole(t, faultTestToken)
	rule := `{"service":"tasks","code":"UNAVAILABLE"}`
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"another origin", map[string]string{"Origin": "http://attacker.example"}, http.StatusForbidden},
		{"a rebound host", map[string]string{"Host": "attacker.example:9090", "Origin": "http://attacker.example:9090",
			"Sec-Fetch-Site": "same-origin"}, http.StatusMisdirectedRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/faults", ""},
				{http.MethodPost, "/api/faults", rule},
				{http.MethodDelete, "/api/faults?all=true", ""},
			} {
				if code, body := faultRequest(t, srv, r.method, r.path, r.body, faultTestToken, c.headers); code != c.want {
					t.Errorf("%s %s = %d %s, want %d", r.method, r.path, code, body, c.want)
				}
			}
		})
	}
	if n := ruleCount(t, api, faultTestToken); n != 0 {
		t.Errorf("cross-origin requests left %d rules", n)
	}
}

// TestConsoleFaultsActThroughTheAdminAPI (#800): with the token, the
// console lists what a rule may name (and why pubsub is refused, in the admin
// API's words), adds a rule the admin API then holds and applies, shows the
// fault it injects under Recent faults, relays a refusal's own message,
// refuses a DELETE that names nothing, and deletes one rule or all of them.
func TestConsoleFaultsActThroughTheAdminAPI(t *testing.T) {
	api, srv := faultConsole(t, faultTestToken)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		return faultRequest(t, srv, method, path, body, faultTestToken, nil)
	}
	type page struct {
		Faults       []admin.FaultRule
		Interposed   []string
		Refused      []admin.Refusal
		Codes        []string
		HTTPStatuses []int `json:"httpStatuses"`
		Recent       []console.FaultEvent
	}
	get := func() page {
		t.Helper()
		code, body := do(http.MethodGet, "/api/faults", "")
		var p page
		if err := json.Unmarshal([]byte(body), &p); err != nil || code != http.StatusOK {
			t.Fatalf("GET /api/faults = %d %s: %v", code, body, err)
		}
		return p
	}

	p := get()
	if strings.Join(p.Interposed, ",") != "secretmanager,tasks" || len(p.Faults) != 0 || len(p.Recent) != 0 {
		t.Errorf("an empty instance's page = %+v", p)
	}
	if len(p.Refused) != 1 || p.Refused[0].Service != "pubsub" || !strings.Contains(p.Refused[0].Reason, "port-forward") {
		t.Errorf("refused = %+v, want pubsub with the admin API's reason", p.Refused)
	}
	if !slices.Contains(p.Codes, "UNAVAILABLE") || !slices.Contains(p.HTTPStatuses, 503) {
		t.Errorf("codes %q, statuses %v", p.Codes, p.HTTPStatuses)
	}

	code, body := do(http.MethodPost, "/api/faults", `{"service":"pubsub"}`)
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if code != http.StatusBadRequest || refusal.Error != p.Refused[0].Reason {
		t.Errorf("a pubsub rule = %d %s; want 400 with the reason the list gives", code, body)
	}

	code, body = do(http.MethodPost, "/api/faults",
		`{"service":"tasks","method":"List*","project":"faulted","code":"PERMISSION_DENIED","count":2,"seed":7,"probability":1}`)
	var created admin.FaultRule
	if err := json.Unmarshal([]byte(body), &created); err != nil || code != http.StatusCreated || created.ID == "" {
		t.Fatalf("POST a tasks rule = %d %s", code, body)
	}
	if n := ruleCount(t, api, faultTestToken); n != 1 {
		t.Fatalf("the admin API holds %d rules after the console's add, want 1", n)
	}
	err := api.Faults().Apply(context.Background(), "tasks", "/google.cloud.tasks.v2.CloudTasks/ListQueues", "projects/faulted/locations/us-central1")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a matching call under the console's rule = %v, want PERMISSION_DENIED", err)
	}
	p = get()
	if len(p.Faults) != 1 || p.Faults[0].ID != created.ID || p.Faults[0].Injected != 1 || p.Faults[0].Remaining != 1 {
		t.Errorf("rules after one fault = %+v", p.Faults)
	}
	if len(p.Recent) != 1 || p.Recent[0].Rule != created.ID || p.Recent[0].Code != "PERMISSION_DENIED" ||
		p.Recent[0].Method != "/google.cloud.tasks.v2.CloudTasks/ListQueues" {
		t.Errorf("recent faults = %+v", p.Recent)
	}

	if code, body := do(http.MethodDelete, "/api/faults", ""); code != http.StatusBadRequest || !strings.Contains(body, "?id=") {
		t.Errorf("DELETE naming nothing = %d %s; want 400", code, body)
	}
	if n := ruleCount(t, api, faultTestToken); n != 1 {
		t.Errorf("a DELETE naming nothing left %d rules, want 1", n)
	}
	if code, body := do(http.MethodDelete, "/api/faults?id=fault-99", ""); code != http.StatusNotFound || !strings.Contains(body, "no fault rule fault-99") {
		t.Errorf("DELETE an absent rule = %d %s", code, body)
	}
	if code, body := do(http.MethodDelete, "/api/faults?id="+created.ID, ""); code != http.StatusOK || !strings.Contains(body, `"deleted":1`) {
		t.Errorf("DELETE %s = %d %s", created.ID, code, body)
	}
	if err := api.Faults().Apply(context.Background(), "tasks", "/google.cloud.tasks.v2.CloudTasks/ListQueues", "projects/faulted"); err != nil {
		t.Errorf("a call after the console's delete = %v, want no fault", err)
	}

	for _, r := range []string{`{"service":"tasks"}`, `{"service":"secretmanager","latencyMs":5}`} {
		if code, body := do(http.MethodPost, "/api/faults", r); code != http.StatusCreated {
			t.Fatalf("POST %s = %d %s", r, code, body)
		}
	}
	if code, body := do(http.MethodDelete, "/api/faults?all=true", ""); code != http.StatusOK || !strings.Contains(body, `"deleted":2`) {
		t.Errorf("DELETE ?all=true = %d %s", code, body)
	}
	if n := ruleCount(t, api, faultTestToken); n != 0 {
		t.Errorf("Clear all left %d rules", n)
	}
}

// TestConsoleWithoutAdminOffersNoFaults: a console not attached to an admin
// API says so, rather than showing an empty rule list.
func TestConsoleWithoutAdminOffersNoFaults(t *testing.T) {
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil).Handler())
	defer srv.Close()
	code, body := faultRequest(t, srv, http.MethodGet, "/api/faults", "", "", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "not available") {
		t.Errorf("GET /api/faults on a console with no admin API = %d %s", code, body)
	}
}
