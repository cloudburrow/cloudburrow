package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"google.golang.org/grpc"
)

// runService serves the Cloud Run v2 API by translating to Knative.
//
// Like Cloud Tasks it runs in the CLI process, because the adapter is ours
// even though the execution engine is the cluster's.
type runService struct {
	// tracing, when on, spans calls (#313).
	tracing *telemetry.Tracing
	// calls reports each completed API call to the admin event log.
	calls grpctransport.Observer
	// interpose run inside the call observer, in order: the request log
	// (#314), then fault injection (#306), so the log sees injected faults.
	interpose []grpc.UnaryServerInterceptor
	// requests reports each JSON request to the admin event log (#591).
	requests func(rest.Request)
	cfg      config.Config
	server   *grpctransport.Server
	// secrets resolves secretKeyRef environment variables. It is nil when
	// Secret Manager is not enabled, and the adapter then refuses a
	// reference rather than dropping it.
	secrets runadapter.SecretResolver
	// env is what every revision and job task is given (#576), resolved
	// per request: the adapter starts before the CLI-hosted services are
	// published to the cluster, so their addresses are not known at Start.
	env runadapter.Environment
}

// useEnvironment sets the environment revisions and job tasks are given.
func (r *runService) useEnvironment(env runadapter.Environment) {
	if r == nil {
		return
	}
	r.env = env
}

// useSecrets attaches the Secret Manager resolver.
//
// The two services are wired together in the CLI rather than importing each
// other, so neither depends on the other's implementation.
func (r *runService) useSecrets(res runadapter.SecretResolver) {
	if r == nil {
		return
	}
	r.secrets = res
}

// newRunService returns the Cloud Run adapter, or nil when not enabled.
func newRunService(cfg config.Config) *runService {
	for _, s := range cfg.EnabledServices() {
		if s == config.ServiceRun {
			return &runService{cfg: cfg}
		}
	}
	return nil
}

func (r *runService) register(coord *lifecycle.Coordinator) {
	if r == nil {
		return
	}
	coord.Register(r)
}

func (r *runService) Name() string { return "run" }

// Addr returns the host address the Cloud Run API listens on.
func (r *runService) Addr() string {
	if r == nil || r.server == nil {
		return ""
	}
	return r.server.Addr()
}

// Start binds the adapter. Resources are acquired here rather than in the
// constructor so failures unwind through the coordinator.
func (r *runService) Start(ctx context.Context) error {
	kn := &runadapter.Knative{Kube: k8s.New(r.cfg.KubeconfigPath(), "", runadapter.WorkloadNamespace)}
	adapter := runadapter.NewServer(kn, r.cfg.Name, time.Duration(r.cfg.ReadyTimeout))
	if r.secrets != nil {
		adapter = adapter.WithSecrets(r.secrets)
	}
	if r.env != nil {
		adapter = adapter.WithEnvironment(r.env)
	}

	addr := net.JoinHostPort(r.cfg.BindAddress, strconv.Itoa(r.cfg.Endpoints.Run))
	r.server = grpctransport.New(addr)
	r.server.Observe(r.calls)
	if r.tracing != nil {
		r.server.ServerOptions(r.tracing.ServerOptions()...)
	}
	for _, i := range r.interpose {
		r.server.Interpose(i)
	}
	// The Operations service must be registered too: the official SDK polls a
	// create through google.longrunning.Operations, and without it every
	// deployment appears to hang.
	ops := runadapter.NewOperationsServer(adapter)
	register := func(g grpc.ServiceRegistrar) {
		adapter.Register(g)
		adapter.Revisions().Register(g)
		// Jobs and Executions (#582) share the adapter's operations.
		// Instances, Tasks, Builds and WorkerPools stay unregistered.
		adapter.Jobs().Register(g)
		adapter.Executions().Register(g)
		ops.Register(g)
	}
	// One port for gRPC and JSON, as run.googleapis.com (#591): the same
	// servers, transcoded, for REST clients and Terraform's provider.
	var jsonAPI http.Handler = runadapter.NewRESTHandler(register)
	if r.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, r.requests)
	}
	r.server.ServeHTTP(jsonAPI)
	if err := r.server.Register(func(g *grpc.Server) { register(g) }); err != nil {
		return err
	}
	return r.server.Start(ctx)
}

func (r *runService) Stop(ctx context.Context) error {
	if r.server == nil {
		return nil
	}
	return r.server.Stop(ctx)
}
