package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
