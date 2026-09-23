package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// mountAdmin attaches the admin API to the control server.
//
// The control server is loopback-only whatever the bind address, which is what
// makes it safe to expose reset at all: an application pod can reach the
// service APIs it needs and cannot reach the endpoint that wipes state.
func mountAdmin(control *lifecycle.ControlServer, rec *admin.Recorder, cfg config.Config, tasksSvc *tasksService) *admin.API {
	api := admin.NewAPI(rec)
	if tasksSvc != nil {
		api.RegisterResetter(&tasksResetter{svc: tasksSvc})
		api.RegisterSeeder(&tasksSeeder{svc: tasksSvc})
	}
	control.Mount(func(mux *http.ServeMux) { api.Routes(mux) })
	return api
}

// tasksResetter clears Cloud Tasks state.
type tasksResetter struct{ svc *tasksService }

func (t *tasksResetter) Name() string { return "tasks" }

// Reset deletes every queue, which removes its tasks with it.
//
// Queues are deleted rather than purged so nothing survives that a later run
// could inherit. The dispatcher observes the deletion through the store, so no
// separate cancellation is needed: a deleted task is simply never due.
func (t *tasksResetter) Reset(context.Context) error {
	st := t.svc.Store()
	if st == nil {
		return nil
	}
	queues, err := st.AllQueues()
	if err != nil {
		return err
	}
	for _, q := range queues {
		if err := st.DeleteQueue(q.Name); err != nil {
			return fmt.Errorf("delete queue %s: %w", q.Name, err)
		}
	}
	return nil
}

// tasksSeeder creates queues from a seed document.
type tasksSeeder struct{ svc *tasksService }

func (t *tasksSeeder) Name() string { return "tasks" }

type tasksSeedSpec struct {
	// Queues are full resource names.
	Queues []string `json:"queues"`
}

func (t *tasksSeeder) Seed(_ context.Context, spec json.RawMessage) error {
	var s tasksSeedSpec
	if err := json.Unmarshal(spec, &s); err != nil {
		return fmt.Errorf("parse tasks seed: %w", err)
	}
	st := t.svc.Store()
	if st == nil {
		return fmt.Errorf("Cloud Tasks is not running")
	}
	for _, name := range s.Queues {
		if _, err := st.CreateQueue(tasks.Queue{Name: name}); err != nil {
			return fmt.Errorf("create queue %s: %w", name, err)
		}
	}
	return nil
}
