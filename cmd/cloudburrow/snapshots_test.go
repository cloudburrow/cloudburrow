package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func dump(t *testing.T, st store.Store) map[string]string {
	t.Helper()
	keys, err := st.List("")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range keys {
		v, err := st.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = string(v)
	}
	return out
}

// TestStateRoundTripsTasksSecretsAndProjects, in both modes: memory stores
// (ephemeral) and durable files (persistent). Queues, tasks, secrets with
// versions and projects are saved, changed, and loaded back identical, record
// for record, with nothing created in between surviving.
func TestStateRoundTripsTasksSecretsAndProjects(t *testing.T) {
	for _, mode := range []string{"ephemeral", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			open := func(name string) store.Store {
				if mode == "ephemeral" {
					return store.NewMemory()
				}
				st, err := store.OpenDurable(filepath.Join(t.TempDir(), name))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				return st
			}
			tdb, sdb, pdb := open("tasks"), open("secrets"), open("projects")
			ts, ss, reg := tasks.NewStore(tdb), secrets.NewStore(sdb), resourcemanager.New(pdb)

			q := "projects/snap-proj/locations/us-central1/queues/q-one"
			if _, err := ts.CreateQueue(tasks.Queue{Name: q, State: tasks.StatePaused}); err != nil {
				t.Fatal(err)
			}
			if _, err := ts.CreateTask(tasks.Task{Name: tasks.TaskName(q, "t-one"), HTTPRequest: &tasks.HTTPRequest{URL: "http://127.0.0.1:9/x", Body: []byte("payload")}}); err != nil {
				t.Fatal(err)
			}
			if _, err := ss.CreateSecret("snap-proj", "api-key", map[string]string{"env": "dev"}, nil, ""); err != nil {
				t.Fatal(err)
			}
			for _, v := range []string{"v1", "v2"} {
				if _, err := ss.AddVersion("snap-proj", "api-key", []byte(v)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ss.SetVersionState("snap-proj", "api-key", "1", secrets.StateDisabled); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.Create(resourcemanager.Project{ProjectID: "snap-proj", DisplayName: "Snap"}); err != nil {
				t.Fatal(err)
			}

			api := admin.NewAPI(admin.NewRecorder(10, nil))
			api.RegisterSnapshotter(
				&kvSnapshotter{name: "tasks", db: func() store.Store { return tdb }},
				&kvSnapshotter{name: "secretmanager", secret: true, db: func() store.Store { return sdb }},
				&kvSnapshotter{name: "projects", db: func() store.Store { return pdb }},
			)
			mux := http.NewServeMux()
			api.Routes(mux)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			before := []map[string]string{dump(t, tdb), dump(t, sdb), dump(t, pdb)}
			resp, err := http.Post(srv.URL+"/admin/state/export", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			archive, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Changes after the snapshot: all must be gone after the load.
			if _, err := ts.CreateQueue(tasks.Queue{Name: "projects/snap-proj/locations/us-central1/queues/later"}); err != nil {
				t.Fatal(err)
			}
			if err := ts.DeleteQueue(q); err != nil {
				t.Fatal(err)
			}
			if _, err := ss.AddVersion("snap-proj", "api-key", []byte("v3")); err != nil {
				t.Fatal(err)
			}
			if err := reg.Delete("snap-proj"); err != nil {
				t.Fatal(err)
			}

			resp, err = http.Post(srv.URL+"/admin/state/import", "application/gzip", bytes.NewReader(archive))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("import %d %s", resp.StatusCode, body)
			}
			after := []map[string]string{dump(t, tdb), dump(t, sdb), dump(t, pdb)}
			for i, name := range []string{"tasks", "secretmanager", "projects"} {
				if !reflect.DeepEqual(before[i], after[i]) {
					t.Errorf("%s differs after the round trip:\nbefore %v\nafter  %v", name, before[i], after[i])
				}
			}
			// And through the services' own APIs.
			if v, err := ss.AccessVersion("snap-proj", "api-key", "latest"); err != nil || string(v.Payload) != "v2" {
				t.Errorf("latest secret payload %q (%v), want v2", v.Payload, err)
			}
			if got, err := ts.GetQueue(q); err != nil || got.State != tasks.StatePaused {
				t.Errorf("queue after load: %+v (%v)", got, err)
			}
		})
	}
}
