package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// monitoringService serves the Cloud Monitoring API v3 (#1939) in the CLI
// process, gRPC and JSON on one port as monitoring.googleapis.com serves
// both. User-defined descriptors and series are a file in the instance's
// directory in persistent mode; the built-in metrics are read from the
// instance's Cloud Logging and its request counters.
type monitoringService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	server    *grpctransport.Server
	api       *monitoring.Server
	// logging is the instance's Cloud Logging, or nil; counters the request
	// counters.
	logging  *loggingService
	counters *metrics.Registry
	// reqs are the serviceruntime request metrics; set before Start, they
	// are sampled by whoever set them.
	reqs *monitoring.RequestBuiltin
	stop chan struct{}
	// pubsub is the instance's Pub/Sub tunnel, for Pub/Sub notification
	// channels (#1964); nil, or nil from it, when Pub/Sub is not served.
	pubsub func() *netfwd.Forwarder
	// cancel stops alert evaluation and uptime checks.
	cancel context.CancelFunc
}

func newMonitoringService(cfg config.Config) *monitoringService {
	if !monitoringEnabled(cfg) {
		return nil
	}
	return &monitoringService{cfg: cfg}
}

// monitoringEnabled is whether `up` binds the Cloud Monitoring port.
func monitoringEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceMonitoring) && cfg.Endpoints.Monitoring >= 0
}

func (s *monitoringService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *monitoringService) Name() string { return "monitoring" }

// API is the server, available after Start.
func (s *monitoringService) API() *monitoring.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *monitoringService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// requestSampleInterval is how often the request counters are sampled
// into serviceruntime.googleapis.com/api/* points, as Google samples them.
const requestSampleInterval = 60 * time.Second

func (s *monitoringService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "monitoring.json")
	}
	api, err := monitoring.NewServer(state)
	if err != nil {
		return err
	}
	s.api = api
	if s.logging != nil {
		api.AddBuiltin(monitoring.NewLogBuiltin(monitoringLogs{svc: s.logging}))
	}
	if s.logging != nil {
		api.SetLogMatcher(monitoringLogs{svc: s.logging})
	}
	if s.pubsub != nil {
		tunnel := s.pubsub
		api.SetPublisher(func(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
			return sinkPublisher{tunnel: tunnel}.Publish(ctx, topic, []logging.Message{{Data: data, Attributes: attrs}})
		})
	}
	if s.cfg.Endpoints.Console > 0 {
		api.SetConsoleURL("http://" + net.JoinHostPort("localhost", strconv.Itoa(s.cfg.Endpoints.Console)))
	}
	// Alert policies are evaluated, and uptime checks run, until Stop.
	evalCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	api.StartEvaluating(evalCtx)
	if s.reqs != nil {
		// up's request history (#1866), which it samples itself, shared
		// with the APIs & Services charts.
		api.AddBuiltin(s.reqs)
	} else if s.counters != nil {
		s.reqs = monitoring.NewRequestBuiltin(monitoringCounters{reg: s.counters}, s.cfg.DefaultProject(), time.Now)
		api.AddBuiltin(s.reqs)
		s.stop = make(chan struct{})
		go func(stop chan struct{}) {
			t := time.NewTicker(requestSampleInterval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					s.reqs.Sample()
				case <-stop:
					return
				}
			}
		}(s.stop)
	}
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Monitoring)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = monitoring.NewRESTHandler(api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *monitoringService) Stop(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// monitoringResetter deletes Cloud Monitoring's user-defined descriptors
// and series.
type monitoringResetter struct{ svc *monitoringService }

func (r *monitoringResetter) Name() string { return "monitoring" }

func (r *monitoringResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *monitoringResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Cloud Monitoring has not started")
	}
	return api.ResetProject(project)
}

// monitoringLogs is monitoring.LogSource over the instance's Cloud Logging.
type monitoringLogs struct{ svc *loggingService }

func (l monitoringLogs) api() *logging.Server {
	if l.svc == nil || l.svc.Admin() == nil {
		return nil
	}
	return l.svc.api
}

func (l monitoringLogs) LogMetrics(project string) []monitoring.LogMetric {
	api := l.api()
	if api == nil {
		return nil
	}
	var out []monitoring.LogMetric
	for _, m := range api.EnabledMetrics(project) {
		lm := monitoring.LogMetric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetMetricDescriptor().GetUnit(),
			Distribution: m.GetValueExtractor() != "", Buckets: m.GetBucketOptions(), Labels: m.GetMetricDescriptor().GetLabels()}
		out = append(out, lm)
	}
	return out
}

func (l monitoringLogs) LogMetricSamples(project, metric string, from, to time.Time) ([]monitoring.LogSample, error) {
	api := l.api()
	if api == nil {
		return nil, nil
	}
	ss, err := api.MetricSamples(project, metric, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]monitoring.LogSample, 0, len(ss))
	for _, s := range ss {
		out = append(out, monitoring.LogSample{Time: s.Entry.GetTimestamp().AsTime(), Resource: entryResource(s.Entry.GetResource()),
			Labels: s.Labels, Value: s.Value, OK: s.HasValue})
	}
	return out, nil
}

func (l monitoringLogs) LogEntries(project string, from, to time.Time) []monitoring.LogEntry {
	api := l.api()
	if api == nil {
		return nil
	}
	var out []monitoring.LogEntry
	for _, e := range api.ProjectEntries(project, from, to) {
		id := e.GetLogName()
		if i := strings.LastIndex(id, "/logs/"); i >= 0 {
			id = id[i+len("/logs/"):]
		}
		if d, err := url.PathUnescape(id); err == nil {
			id = d
		}
		out = append(out, monitoring.LogEntry{Time: e.GetTimestamp().AsTime(), Log: id, Severity: e.GetSeverity().String(),
			Resource: entryResource(e.GetResource())})
	}
	return out
}

// MatchLogEntries is monitoring.LogMatcher over the instance's Cloud
// Logging, for log-match alert conditions.
func (l monitoringLogs) MatchLogEntries(project, filter string, extractors map[string]string, after, until time.Time) ([]monitoring.LogMatch, error) {
	api := l.api()
	if api == nil {
		return nil, errors.New("Cloud Logging has not started")
	}
	ss, err := api.MatchEntries(project, filter, extractors, after, until)
	if err != nil {
		return nil, err
	}
	out := make([]monitoring.LogMatch, 0, len(ss))
	for _, s := range ss {
		e := s.Entry
		summary := e.GetTextPayload()
		if summary == "" && e.GetJsonPayload() != nil {
			if b, err := protojson.Marshal(e.GetJsonPayload()); err == nil {
				summary = string(b)
			}
		}
		if len(summary) > 200 {
			summary = summary[:200] + "..."
		}
		out = append(out, monitoring.LogMatch{Time: e.GetTimestamp().AsTime(), Log: e.GetLogName() + "/" + e.GetInsertId(),
			Type: e.GetResource().GetType(), Resource: e.GetResource().GetLabels(), Labels: s.Labels, Summary: summary})
	}
	return out, nil
}

func entryResource(r *monitoredres.MonitoredResource) *monitoredres.MonitoredResource {
	if r == nil {
		return nil
	}
	return &monitoredres.MonitoredResource{Type: r.GetType(), Labels: r.GetLabels()}
}

// monitoringCounters is monitoring.RequestSource over the request counters.
type monitoringCounters struct{ reg *metrics.Registry }

func (c monitoringCounters) Snapshot() []monitoring.RequestCount {
	snap := c.reg.ProjectSnapshot()
	out := make([]monitoring.RequestCount, len(snap))
	for i, p := range snap {
		out[i] = monitoring.RequestCount{Service: p.Service, Method: p.Method, Code: p.Code, Project: p.Project, Count: p.Count, Sum: p.Sum, Buckets: p.Buckets}
	}
	return out
}

func (monitoringCounters) Bounds() []float64 { return metrics.Buckets }

// API names a counted service's API, as Service Usage lists it.
func (monitoringCounters) API(service string) string {
	for _, a := range serviceUsageAPIs {
		if string(a.service) == service {
			return a.api.Name
		}
	}
	switch service {
	case "sqladmin":
		return "sqladmin.googleapis.com"
	case "memorystore":
		return "redis.googleapis.com"
	}
	return service + ".googleapis.com"
}

// requestSampler samples the request history every requestSampleInterval,
// so serviceruntime.googleapis.com/api/* and the APIs & Services charts
// (#1866) have one-minute points whether or not anybody reads them.
type requestSampler struct {
	history *monitoring.RequestBuiltin
	stop    chan struct{}
	done    chan struct{}
}

func (s *requestSampler) Name() string { return "request-history" }

func (s *requestSampler) Start(context.Context) error {
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		t := time.NewTicker(requestSampleInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.history.Sample()
			case <-stop:
				return
			}
		}
	}(s.stop, s.done)
	return nil
}

func (s *requestSampler) Stop(context.Context) error {
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop = nil
	}
	return nil
}
