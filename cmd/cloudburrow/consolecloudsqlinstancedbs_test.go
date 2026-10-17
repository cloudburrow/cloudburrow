package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCloudSQLConsoleInstanceDatabasesAndUsers: a RUNNABLE instance's page
// has Databases and Users tabs, from databases.list and users.list, and
// Create database, Delete database, Add user account, Change password and
// Delete user account send databases.insert and delete and users.insert,
// update and delete, a MySQL user named by its host as well.
func TestCloudSQLConsoleInstanceDatabasesAndUsers(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/my"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "my", "state": "RUNNABLE", "databaseVersion": "MYSQL_8_0",
				"settings": map[string]any{"tier": "db-custom-1-3840"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/my/databases"):
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"name": "shop", "charset": "utf8mb4", "collation": "utf8mb4_0900_ai_ci"}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/my/users"):
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"name": "root", "host": "%"},
				map[string]any{"name": "app", "host": "10.0.0.%"}}})
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"MYSQL_8_0"}}
	ctx := context.Background()

	d, err := p.Detail(ctx, "proj", []string{"my"})
	if err != nil {
		t.Fatal(err)
	}
	tabs := map[string][]string{}
	for _, s := range d.Sections {
		for _, it := range s.Listing.Items {
			tabs[s.ID] = append(tabs[s.ID], it.Name)
		}
	}
	if strings.Join(tabs["databases"], ",") != "shop" || strings.Join(tabs["users"], ",") != "root,app@10.0.0.%" {
		t.Errorf("Databases and Users tabs: %v", tabs)
	}
	ids := map[string]bool{}
	for _, a := range d.Actions {
		ids[a.ID] = true
	}
	for _, want := range []string{"create-database", "drop-database", "add-user", "change-password", "remove-user"} {
		if !ids[want] {
			t.Errorf("the instance's page has no %s", want)
		}
	}

	for _, step := range []struct {
		action string
		values map[string]string
		want   string
	}{
		{"create-database", map[string]string{"dbName": "orders", "charset": "utf8mb4"}, `POST /v1/projects/proj/instances/my/databases?alt=json&prettyPrint=false {"charset":"utf8mb4","name":"orders"}`},
		{"drop-database", map[string]string{"dbName": "shop"}, "DELETE /v1/projects/proj/instances/my/databases/shop?"},
		{"add-user", map[string]string{"userName": "ro", "password": "Pw-1", "host": "%"}, `POST /v1/projects/proj/instances/my/users?alt=json&prettyPrint=false {"host":"%","name":"ro","password":"Pw-1"}`},
		{"change-password", map[string]string{"user": "app@10.0.0.%", "password": "Pw-2"}, `PUT /v1/projects/proj/instances/my/users?alt=json&host=10.0.0.%25&name=app&prettyPrint=false {"host":"10.0.0.%","name":"app","password":"Pw-2"}`},
		{"remove-user", map[string]string{"user": "root"}, "DELETE /v1/projects/proj/instances/my/users?alt=json&name=root&prettyPrint=false"},
	} {
		mu.Lock()
		calls = nil
		mu.Unlock()
		if err := p.ActAt(ctx, "proj", []string{"my"}, step.action, step.values); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		mu.Lock()
		got := strings.Join(calls, "\n")
		mu.Unlock()
		if !strings.Contains(got, step.want) {
			t.Errorf("%s sent %q; want %q", step.action, got, step.want)
		}
	}
	if err := p.ActAt(ctx, "proj", []string{"my"}, "change-password", map[string]string{"user": "root"}); err == nil {
		t.Error("Change password took no password")
	}
}
