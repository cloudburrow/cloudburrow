package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/version"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
)

// adminDeps are what the resetters reach through. Each is resolved when a reset
// runs, not when the routes are mounted, so a service that starts after the
// admin API is still covered.
type adminDeps struct {
	tasks     *tasksService
	secrets   *secretsService
	kms       *kmsService
	scheduler *schedulerService
	logging   *loggingService
	notify    *notifyService
	// mysql is Cloud SQL for MySQL's credentials, for its resetter.
	mysql      components.MySQLCredentials
	forwarders []*netfwd.Forwarder
	// projects lists the registered projects; it is read at reset time because
	// the registry is opened after the admin routes are mounted.
	projects func() []string
	// projectStore is the registry's store, for state snapshots; read at
	// snapshot time for the same reason.
	projectStore func() store.Store
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
		if cfg.Storage.Backend == config.StorageBuiltin {
			// The builtin server clears its own state, per project too (#510).
			api.RegisterResetter(&builtinStorageResetter{tunnel: f})
		} else {
			api.RegisterResetter(&storageResetter{tunnel: f, notify: d.notify})
		}
		api.RegisterSeeder(&storageSeeder{project: cfg.DefaultProject(), builtin: cfg.Storage.Backend == config.StorageBuiltin, front: func() string {
			// Through the notification front when it is up, as a client's
			// upload is; the bare backend otherwise.
			if a := d.notify.Addr(); a != "" {
				return a
			}
			return f.HostAddr()
		}})
	}
	if f := forwarderFor(d.forwarders, "pubsub"); f != nil {
		api.RegisterResetter(&pubsubResetter{tunnel: f, projects: func() []string {
			return knownProjects(cfg.DefaultProject(), d.projects)
		}})
		api.RegisterSeeder(&pubsubSeeder{tunnel: f})
	}
	if f := forwarderFor(d.forwarders, string(config.ServiceCloudSQLMySQL)); f != nil {
		api.RegisterResetter(&mysqlResetter{tunnel: f, creds: d.mysql})
	}
	if d.scheduler != nil {
		api.RegisterResetter(&schedulerResetter{svc: d.scheduler})
	}
	if d.logging != nil {
		api.RegisterResetter(&loggingResetter{svc: d.logging})
	}
	if d.kms != nil {
		api.RegisterResetter(&kmsResetter{svc: d.kms})
	}
	if d.secrets != nil {
		api.RegisterResetter(&secretsResetter{svc: d.secrets})
		api.RegisterSeeder(&secretsSeeder{svc: d.secrets})
	}

	// State snapshots (#289): the services whose state is theirs to keep,
	// and, for every other enabled service, the reason it is left out.
	api.SetStateProducer(version.Get().Version, cfg.Name)
	if d.tasks != nil {
		api.RegisterSnapshotter(&kvSnapshotter{name: "tasks", db: func() store.Store { return d.tasks.db }})
	}
	if d.secrets != nil {
		api.RegisterSnapshotter(&kvSnapshotter{name: "secretmanager", secret: true, db: func() store.Store { return d.secrets.db }})
	}
	if d.projectStore != nil {
		api.RegisterSnapshotter(&kvSnapshotter{name: "projects", db: d.projectStore})
	}
	if f := forwarderFor(d.forwarders, "storage"); f != nil {
		api.RegisterSnapshotter(&storageSnapshotter{tunnel: f, notify: d.notify, project: cfg.DefaultProject()})
	}
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceTasks, config.ServiceSecrets, config.ServiceStorage:
		case config.ServiceCloudSQL:
			// pg_dump and pg_restore in the server's pod (#311).
			api.RegisterSnapshotter(newPostgresSnapshotter(cfg))
		default:
			api.RegisterNotCaptured(string(s), notCapturedReasons(s))
		}
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
	IfNotExists bool `json:"ifNotExists"`
	// Queues are full resource names.
	Queues []string `json:"queues"`
}

func (t *tasksSeeder) Validate(spec json.RawMessage) error {
	var s tasksSeedSpec
	if err := strictDecode(spec, &s); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, name := range s.Queues {
		// The store's own parser, so validation cannot disagree with creation.
		if n, err := resource.Parse(name); err != nil || n.Collection != "queues" {
			return fmt.Errorf("queues[%d] %q is not projects/{project}/locations/{location}/queues/{queue}", i, name)
		}
		if seen[name] {
			return fmt.Errorf("queues[%d] %q appears twice", i, name)
		}
		seen[name] = true
	}
	return nil
}

func (t *tasksSeeder) Seed(_ context.Context, spec json.RawMessage) error {
	var s tasksSeedSpec
	if err := strictDecode(spec, &s); err != nil {
		return apierror.InvalidArgument("parse tasks seed: %v", err)
	}
	st := t.svc.Store()
	if st == nil {
		return fmt.Errorf("Cloud Tasks is not running")
	}
	for _, name := range s.Queues {
		_, err := st.CreateQueue(tasks.Queue{Name: name})
		if status.Code(err) == codes.AlreadyExists {
			if s.IfNotExists {
				continue
			}
			return apierror.AlreadyExists("queue %s already exists; set ifNotExists to skip it", name)
		}
		if err != nil {
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
