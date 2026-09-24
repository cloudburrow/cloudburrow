package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// adminDeps are what the resetters reach through. Each is resolved when a reset
// runs, not when the routes are mounted, so a service that starts after the
// admin API is still covered.
type adminDeps struct {
	tasks      *tasksService
	secrets    *secretsService
	notify     *notifyService
	forwarders []*netfwd.Forwarder
	// projects lists the registered projects; it is read at reset time because
	// the registry is opened after the admin routes are mounted.
	projects func() []string
}

// mountAdmin attaches the admin API to the control server.
//
// The control server is loopback-only whatever the bind address, which is what
// makes it safe to expose reset at all: an application pod can reach the
// service APIs it needs and cannot reach the endpoint that wipes state.
func mountAdmin(control *lifecycle.ControlServer, rec *admin.Recorder, cfg config.Config, d adminDeps) *admin.API {
	api := admin.NewAPI(rec)
	// Registration order is reset order. Storage goes before Pub/Sub so its
	// notification configurations are gone before topics are removed; see
	// storageResetter.
	if d.tasks != nil {
		api.RegisterResetter(&tasksResetter{svc: d.tasks})
		api.RegisterSeeder(&tasksSeeder{svc: d.tasks})
	}
	if f := forwarderFor(d.forwarders, "storage"); f != nil {
		api.RegisterResetter(&storageResetter{tunnel: f, notify: d.notify})
	}
	if f := forwarderFor(d.forwarders, "pubsub"); f != nil {
		api.RegisterResetter(&pubsubResetter{tunnel: f, projects: func() []string {
			return knownProjects(cfg.DefaultProject(), d.projects)
		}})
	}
	if d.secrets != nil {
		api.RegisterResetter(&secretsResetter{svc: d.secrets})
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
func (t *tasksResetter) Reset(context.Context) error { return t.deleteQueues("") }

// ResetProject deletes one project's queues. A queue's name begins with its
// project, so this is exact.
func (t *tasksResetter) ResetProject(_ context.Context, project string) error {
	return t.deleteQueues("projects/" + project + "/")
}

func (t *tasksResetter) deleteQueues(prefix string) error {
	st := t.svc.Store()
	if st == nil {
		return nil
	}
	queues, err := st.AllQueues()
	if err != nil {
		return err
	}
	for _, q := range queues {
		if !strings.HasPrefix(q.Name, prefix) {
			continue
		}
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

// forwarderFor returns the tunnel for a service, or nil when it is not enabled.
func forwarderFor(fwds []*netfwd.Forwarder, service string) *netfwd.Forwarder {
	for _, f := range fwds {
		if f.Name() == "forward:"+service {
			return f
		}
	}
	return nil
}
