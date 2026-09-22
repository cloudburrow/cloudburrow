package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"

	runadapter "github.com/identity-wael/cloudburrow/internal/adapter/run"
	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/console"
	"github.com/identity-wael/cloudburrow/internal/lifecycle"
	"github.com/identity-wael/cloudburrow/internal/localai"
	"github.com/identity-wael/cloudburrow/internal/netfwd"
	"github.com/identity-wael/cloudburrow/internal/service/resourcemanager"
	"github.com/identity-wael/cloudburrow/internal/service/vertexai"
)

// consoleDeps are the running pieces the console reads through.
//
// They are passed rather than looked up because the console must never own a
// service: it is handed the same objects the services use, so there is no way
// for it to answer from anywhere else.
type consoleDeps struct {
	cfg        config.Config
	localAI    *vertexai.Server
	projects   *resourcemanager.Registry
	coord      *lifecycle.Coordinator
	cluster    interface{ ServerVersion() string }
	tasks      *tasksService
	secrets    *secretsService
	forwarders []*netfwd.Forwarder
	metaAddr   func() string
	ingress    func() string
}

// buildConsole returns the console server, or nil when it is disabled.
func buildConsole(d consoleDeps) *console.Server {
	if d.cfg.Endpoints.Console < 0 {
		return nil
	}

	var storageAddr, pubsubAddr string
	for _, f := range d.forwarders {
		switch f.Name() {
		case "forward:storage":
			storageAddr = f.HostAddr()
		case "forward:pubsub":
			pubsubAddr = f.HostAddr()
		}
	}
	// With the notification handler in front, the address clients use is the
	// configured storage port rather than the tunnel's.
	if d.cfg.Endpoints.Storage != 0 {
		storageAddr = net.JoinHostPort(d.cfg.BindAddress, strconv.Itoa(d.cfg.Endpoints.Storage))
	}

	enabled := map[config.Service]bool{}
	for _, s := range d.cfg.EnabledServices() {
		enabled[s] = true
	}

	var providers []console.Provider
	// Resource Manager first: it is where the projects every other screen is
	// scoped to come from.
	if d.projects != nil {
		providers = append(providers, projectsProvider{registry: d.projects})
	}
	if enabled[config.ServiceStorage] && storageAddr != "" {
		providers = append(providers, storageProvider{endpoint: storageAddr})
	}
	if enabled[config.ServicePubSub] && pubsubAddr != "" {
		providers = append(providers, pubsubProvider{endpoint: pubsubAddr})
	}
	if enabled[config.ServiceTasks] && d.tasks != nil {
		providers = append(providers, tasksProvider{svc: d.tasks})
	}
	if enabled[config.ServiceRun] {
		providers = append(providers, runProvider{
			kubeconfig:     d.cfg.KubeconfigPath(),
			namespace:      runadapter.WorkloadNamespace,
			runEndpoint:    net.JoinHostPort(d.cfg.BindAddress, strconv.Itoa(d.cfg.Endpoints.Run)),
			defaultProject: d.cfg.Name,
		})
	}
	if enabled[config.ServiceSecrets] && d.secrets != nil {
		providers = append(providers, secretsProvider{svc: d.secrets})
	}
	// The opt-in databases. Each had a working backend and no screen, which
	// reads as "not implemented" to anyone looking at the console. The
	// forwarder knows their host addresses; a service that is enabled but has
	// no tunnel yet simply has no screen rather than a broken one.
	for _, db := range []struct {
		service config.Service
		build   func(addr string) console.Provider
	}{
		{config.ServiceFirestore, func(a string) console.Provider { return firestoreProvider{endpoint: a} }},
		{config.ServiceDatastore, func(a string) console.Provider { return datastoreProvider{endpoint: a} }},
		{config.ServiceBigtable, func(a string) console.Provider { return bigtableProvider{endpoint: a} }},
		{config.ServiceSpanner, func(a string) console.Provider { return spannerProvider{endpoint: a} }},
		{config.ServiceCloudSQL, func(a string) console.Provider { return cloudSQLProvider{endpoint: a} }},
	} {
		if !enabled[db.service] {
			continue
		}
		if addr := forwardedAddr(d.forwarders, string(db.service)); addr != "" {
			providers = append(providers, db.build(addr))
		}
	}

	// The cluster views are read-only and always present: CloudBurrow owns
	// this cluster, and being able to see what is actually running in it is
	// the point of running one locally.
	// The AI area is always listed and never operational, so its absence is
	// explained rather than silently missing.
	providers = append(providers, aiProvider{})

	kubeconfig := d.cfg.KubeconfigPath()
	// One source, read by the dashboard's panel and joined onto the Pods
	// listing. Two readers of one kubelet call rather than two calls.
	metrics := clusterMetrics(kubeconfig)
	providers = append(providers,
		workloadsProvider(kubeconfig),
		podsProvider(kubeconfig, metrics),
		servicesProvider(kubeconfig),
		jobsProvider(kubeconfig),
		eventsProvider(kubeconfig),
	)

	addr := net.JoinHostPort(d.cfg.BindAddress, strconv.Itoa(d.cfg.Endpoints.Console))
	srv := console.New(addr, consoleStatus(d), providers...)
	srv.SetPlayground(playgroundFor(d))
	srv.SetMetrics(metrics)
	// The history is the console's, not a browser tab's. Kept server-side so
	// it survives a reload and so the sampling rate does not depend on how
	// many people are looking.
	srv.SetSeries(console.NewSeries(console.SeriesLimit, nil))
	return srv
}

// playgroundFor returns the playground configuration, which is empty unless
// local AI is running. An empty one is not an error state: the screen is
// simply not offered, and the rest of the console is unaffected.
func playgroundFor(d consoleDeps) *console.Playground {
	if d.localAI == nil {
		return nil
	}
	p := &console.Playground{
		// Resolved per request: the console is built before the endpoint
		// binds, so capturing the address here would capture an empty one.
		Addr:      d.localAI.Addr,
		Model:     d.localAI.Model(),
		Publisher: "unknown",
	}
	if m, err := localai.Lookup(p.Model); err == nil {
		p.Publisher = string(m.Publisher)
		p.Community = m.Publisher == localai.PublisherCommunity
	}
	return p
}

// consoleStatus reports live instance state.
//
// Every field is read at request time from the thing that knows it. Nothing
// is cached, because a cached "ready" shown after a component failed is worse
// than a slow page.
func consoleStatus(d consoleDeps) console.StatusSource {
	return func(_ context.Context) console.Status {
		st := console.Status{
			Instance: d.cfg.Name,
			// The instance's own project, which is what the generated
			// credentials and the metadata server report. Selecting it is what
			// makes the console open on data instead of on "choose a project".
			DefaultProject: d.cfg.Name,
			Cluster:        d.cfg.ClusterName(),
			Namespace:      d.cfg.Cluster.Namespace,
			Mode:           string(d.cfg.Mode),
			Endpoints:      map[string]string{},
		}
		if d.coord != nil {
			st.Ready = d.coord.Ready()
			st.State = d.coord.State().String()
			names, ready := d.coord.SortedReady()
			st.Components = make(map[string]bool, len(names))
			for _, n := range names {
				st.Components[n] = ready[n]
			}
		}
		if d.cluster != nil {
			st.Kubernetes = d.cluster.ServerVersion()
		}

		for _, f := range d.forwarders {
			// Read now, not latched: a tunnel whose pod went away reports
			// not-running however well it started.
			st.Tunnels = append(st.Tunnels, console.TunnelStatus{
				Name:     trimForward(f.Name()),
				Host:     f.HostAddr(),
				Running:  f.Running(),
				Restarts: f.Restarts(),
			})
			if addr := f.HostAddr(); addr != "" {
				st.Endpoints[trimForward(f.Name())] = addr
			}
		}
		if d.cfg.Endpoints.Storage != 0 {
			st.Endpoints["storage"] = net.JoinHostPort(d.cfg.BindAddress,
				strconv.Itoa(d.cfg.Endpoints.Storage))
		}
		if d.tasks != nil {
			if addr := d.tasks.Addr(); addr != "" {
				st.Endpoints["tasks"] = addr
			}
		}
		if d.secrets != nil {
			if addr := d.secrets.Addr(); addr != "" {
				st.Endpoints["secretmanager"] = addr
			}
		}
		if d.metaAddr != nil {
			if addr := d.metaAddr(); addr != "" {
				st.Endpoints["metadata"] = addr
			}
		}
		if d.ingress != nil {
			if addr := d.ingress(); addr != "" {
				st.Endpoints["ingress"] = addr
			}
		}

		// A service is listed whether or not it is enabled, and a disabled
		// one carries its reason: a greyed-out entry with no explanation is
		// the kind of thing a developer assumes is broken.
		for _, s := range config.KnownServices() {
			entry := console.ServiceStatus{ID: string(s), Title: serviceTitle(s)}
			for _, e := range d.cfg.EnabledServices() {
				if e == s {
					entry.Enabled = true
				}
			}
			if !entry.Enabled {
				if s.IsOptional() {
					entry.Reason = "opt-in; start with --services " + string(s)
				} else {
					entry.Reason = "not selected by --services"
				}
			}
			st.Services = append(st.Services, entry)
		}
		return st
	}
}

func trimForward(name string) string {
	const prefix = "forward:"
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

func serviceTitle(s config.Service) string {
	switch s {
	case config.ServiceStorage:
		return "Cloud Storage"
	case config.ServicePubSub:
		return "Pub/Sub"
	case config.ServiceTasks:
		return "Cloud Tasks"
	case config.ServiceRun:
		return "Cloud Run"
	case config.ServiceSecrets:
		return "Secret Manager"
	default:
		return string(s)
	}
}

// printConsole reports where the console is, since a URL nobody is told about
// is a console nobody opens.
func printConsole(w io.Writer, srv *console.Server) {
	if srv == nil {
		return
	}
	if url := srv.URL(); url != "" {
		fmt.Fprintf(w, "  console:    %s\n", url)
	}
}

// forwardedAddr returns the host address of a named tunnel, or "".
func forwardedAddr(forwarders []*netfwd.Forwarder, name string) string {
	for _, f := range forwarders {
		if f.Name() == "forward:"+name {
			return f.HostAddr()
		}
	}
	return ""
}

// consoleSampler returns the component that keeps the metric history warm.
//
// Built here, where the console already is, rather than in up.go: the
// interval and the retention are console concerns and belong beside the
// server they configure.
func consoleSampler(srv *console.Server) *console.Sampler {
	if srv == nil {
		return nil
	}
	return console.NewSampler(srv, console.SampleInterval)
}
