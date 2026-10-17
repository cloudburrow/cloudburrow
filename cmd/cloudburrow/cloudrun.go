package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/registry"
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
	// storage is the Cloud Storage tunnel SubmitBuild reads its source
	// through (#1268); nil without Cloud Storage.
	storage func() *netfwd.Forwarder
	// secrets resolves secretKeyRef environment variables. It is nil when
	// Secret Manager is not enabled, and the adapter then refuses a
	// reference rather than dropping it.
	secrets runadapter.SecretResolver
	// buildSecretManager and buildKMS read a Cloud Build build's
	// availableSecrets and secrets (#1407); nil when not enabled.
	buildSecretManager runadapter.SecretAccessor
	buildKMS           runadapter.KMSDecrypter
	// env is what every revision and job task is given (#576), resolved
	// per request: the adapter starts before the CLI-hosted services are
	// published to the cluster, so their addresses are not known at Start.
	env runadapter.Environment
	// cloudSQL serves Cloud SQL volumes (#1346); nil refuses them.
	cloudSQL runadapter.CloudSQL
	// events serves functions' event triggers (#1414); nil without
	// Pub/Sub, which refuses them.
	events *eventsConfig
	// adapter is the started adapter, which the metadata server asks for a
	// workload's account (#1388); nil before Start.
	adapter atomic.Pointer[runadapter.Server]
	// functions is the started Cloud Functions server, whose functions a
	// project's reset deletes with their event triggers (#1540).
	functions atomic.Pointer[runadapter.FunctionsServer]
}

// metadataAccount is the metadata server's AccountResolver: the account a
// workload's metadata host stands for, once the adapter has started.
func (r *runService) metadataAccount(label string) (string, bool) {
	if r == nil {
		return "", false
	}
	a := r.adapter.Load()
	if a == nil {
		return "", false
	}
	return a.MetadataAccount(label)
}

// useCloudSQL enables Cloud SQL volumes.
func (r *runService) useCloudSQL(f runadapter.CloudSQL) {
	if r == nil {
		return
	}
	r.cloudSQL = f
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
	adapter.CollectVolumesInTheBackground()
	if r.secrets != nil {
		adapter = adapter.WithSecrets(r.secrets)
	}
	if r.env != nil {
		adapter = adapter.WithEnvironment(r.env)
	}
	if r.cloudSQL != nil {
		adapter = adapter.WithCloudSQL(r.cloudSQL)
	}
	r.adapter.Store(adapter)
	// Artifact Registry's Docker registry (#1383): content-addressed in
	// the instance's state directory, so pushed images and the
	// repositories that hold them survive a restart.
	regStore, err := registry.Open(filepath.Join(r.cfg.InstanceDir(), "artifactregistry"))
	if err != nil {
		return fmt.Errorf("open the Artifact Registry store: %w", err)
	}
	runadapter.UseRegistry(regStore)
	// Secure Source Manager's repositories (#1521), bare git directories
	// beside it.
	if err := runadapter.UseSourceManagerRoot(filepath.Join(r.cfg.InstanceDir(), "securesourcemanager")); err != nil {
		return fmt.Errorf("open the Secure Source Manager store: %w", err)
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
	// Builds (#1268) reads its source from this instance's Cloud Storage,
	// so without storage SubmitBuild says so rather than going unserved.
	var fetch runadapter.SourceFetcher
	if serviceEnabled(r.cfg, config.ServiceStorage) {
		sf := storageSourceFetcher{cfg: r.cfg, client: &http.Client{Timeout: 10 * time.Minute}}
		if r.storage != nil {
			sf.addr = func() string {
				if f := r.storage(); f != nil {
					return f.HostAddr()
				}
				return ""
			}
		}
		fetch = sf
		// ExportArtifact writes a version's files there (#1449).
		runadapter.UseExportWriter(sf)
		// ImportAptArtifacts and ImportYumArtifacts read from there (#1531).
		runadapter.UseImportReader(sf)
	}
	// Arbitrary steps run only with --cloudbuild-host-steps, from loopback (#1382).
	builds := adapter.Builds(fetch, newLocalImageBuilder(r.cfg)).AllowHostSteps(r.cfg.CloudBuildHostSteps)
	builds = builds.WithBuildSecrets(r.buildSecretManager, r.buildKMS)
	// Pub/Sub triggers subscribe on the instance's Pub/Sub (#1506).
	if r.events != nil {
		builds = builds.WithTriggerEvents(r.events.transport, func() string {
			if r.events.pushBase == nil {
				return ""
			}
			return r.events.pushBase()
		})
	}
	register := func(g grpc.ServiceRegistrar) {
		adapter.Register(g)
		adapter.Revisions().Register(g)
		// Jobs and Executions (#582) share the adapter's operations, and
		// Tasks (#1223) reads the executions' pods; Builds (#1268) builds
		// source locally. WorkerPools (#1300) and Instances (#1299) run
		// as Deployments.
		adapter.Jobs().Register(g)
		adapter.WorkerPools().Register(g)
		adapter.Instances().Register(g)
		adapter.Executions().Register(g)
		adapter.Tasks().Register(g)
		builds.Register(g)
		ops.Register(g)
	}
	// Artifact Registry's repositories and Cloud Build's builds (#1347),
	// which `gcloud run deploy --source` calls, on the same port: gRPC
	// beside Cloud Run's, JSON at their own v1 paths.
	sourceDeploy := func(g grpc.ServiceRegistrar) {
		adapter.ArtifactRegistry().Register(g)
		builds.CloudBuild().Register(g)
		ops.Register(g)
	}
	if r.cfg.Endpoints.Console != 0 {
		runadapter.BuildLogURLBase = "http://" + net.JoinHostPort(r.cfg.BindAddress, strconv.Itoa(r.cfg.Endpoints.Console))
	}
	// One port for gRPC and JSON, as run.googleapis.com (#591): the same
	// servers, transcoded, for REST clients and Terraform's provider.
	// The v1 (Knative-shaped) API gcloud run calls (#1293), on the same
	// port and over the same adapter.
	// Cloud Functions v2 (#1377): a function is a SubmitBuild buildpacks
	// build and a Cloud Run service, on the same port, with its source
	// upload and download URLs.
	functions := adapter.Functions(builds, func() string { return hostURL(r.server.Addr()) })
	ingress := ""
	if r.cfg.Endpoints.Ingress != 0 {
		host := r.cfg.BindAddress
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		ingress = net.JoinHostPort(host, strconv.Itoa(r.cfg.Endpoints.Ingress))
	}
	// Cloud Functions v1's call (#1428) reaches a function through the
	// ingress, as a delivery does.
	var tokens runadapter.IDTokenMinter
	if r.events != nil {
		tokens = r.events.tokens
	}
	functions.WithIngress(ingress, tokens)
	r.functions.Store(functions)
	if r.events != nil {
		functions.WithEvents(runadapter.Events{Transport: r.events.transport, Tokens: r.events.tokens,
			PushBase: r.events.pushBase, Ingress: ingress})
	}
	// The Docker Registry HTTP API v2 at /v2/ (#1383), before the JSON
	// APIs: no Google API's path starts with v2/ on this port but Cloud
	// Run's own, /v2/projects/..., which the registry passes on.
	// Secure Source Manager (#1521): its JSON API under /securesourcemanager/v1/
	// (its v1 paths are Artifact Registry's), and its repositories' git URLs,
	// whose pushes run the triggers file's builds.
	ssm := adapter.SecureSourceManager(func() string { return hostURL(r.server.Addr()) })
	// Developer Connect (#1546): connections to that Secure Source Manager
	// and links to its repositories, under /developerconnect/v1/.
	devConnect := adapter.DeveloperConnect()
	cloudBuild := builds.CloudBuild()
	var jsonAPI http.Handler = runadapter.RegistryHandlerOr(runadapter.SourceManagerHandlerOr(ssm, cloudBuild,
		func(g grpc.ServiceRegistrar) { ssm.Register(g); ops.Register(g) },
		runadapter.DeveloperConnectHandlerOr(func(g grpc.ServiceRegistrar) { devConnect.Register(g); ops.Register(g) },
			functions.FunctionsHandler(runadapter.SourceDeployHandler(sourceDeploy,
				adapter.V1Handler(runadapter.NewRESTHandler(register)))))))
	if r.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, r.requests)
	}
	r.server.ServeHTTP(jsonAPI)
	if err := r.server.Register(func(g *grpc.Server) {
		register(g)
		adapter.ArtifactRegistry().Register(g)
		builds.CloudBuild().Register(g)
		ssm.Register(g)
		devConnect.Register(g)
		functions.Register(g)
	}); err != nil {
		return err
	}
	// The gcsfuse pods, PersistentVolumes and claims no workload uses,
	// counted from the cluster: whatever was deleted while the instance was
	// down, or before they were collected at all (#1476).
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := adapter.CollectVolumes(cctx); err != nil {
			slog.Warn("Cloud Run could not remove unused volume objects", "err", err)
		}
	}()
	return r.server.Start(ctx)
}

// runVolumesResetter is Cloud Run's part of a reset (#1476): every gcsfuse
// pod, PersistentVolume and claim that no service, job, worker pool or
// instance uses. A full reset leaves Cloud Run's workloads. A project's
// reset (#1540), as at a purge, first deletes the project's services,
// jobs, worker pools, instances and functions, and forgets its Cloud Build
// state, as Google drops them with a deleted project. The volume objects
// are shared across projects by bucket or export, so a project's reset
// then removes the same unused ones a full reset does.
type runVolumesResetter struct{ svc *runService }

func (r *runVolumesResetter) Name() string { return "run" }

func (r *runVolumesResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *runVolumesResetter) ResetProject(ctx context.Context, project string) error {
	a := r.svc.adapter.Load()
	if a == nil {
		return nil
	}
	var errs []error
	if project != "" {
		errs = append(errs, a.ResetProject(ctx, project, r.svc.functions.Load()))
	}
	_, err := a.CollectVolumes(ctx)
	return errors.Join(append(errs, err)...)
}

func (r *runService) Stop(ctx context.Context) error {
	if r.server == nil {
		return nil
	}
	return r.server.Stop(ctx)
}
