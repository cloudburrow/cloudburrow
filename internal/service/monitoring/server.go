// Package monitoring serves the Cloud Monitoring API v3 MetricService
// (monitoring.googleapis.com, #1939) in the CLI process, gRPC and JSON on
// one port: metric descriptors (user-defined ones created, read and
// deleted; built-in ones read), monitored resource descriptors, and time
// series, written with Google's write rules (write.go) and read with a
// filter, an interval, alignment, reduction and a view (list.go).
//
// User-defined descriptors and the points written to them are kept in the
// instance's state. Built-in metric types are produced from what the
// instance measures itself (builtin.go): its log-based metrics and log
// entries, from the local Cloud Logging, and the API request counts and
// latencies it serves.
//
// The other v3 services (#1964) are served too: alert policies, evaluated
// over these series and the local Cloud Logging (evaluate.go), with
// incidents and notifications delivered only on the machine (notify.go);
// notification channels, uptime checks run against local hosts, groups,
// services and SLOs, snoozes, and the Dashboards API v1. The Query service
// (MQL) is not registered and answers UNIMPLEMENTED.
// docs/monitoring.md is the user's guide.
package monitoring

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Limits Google documents (docs.cloud.google.com/monitoring/quotas and
// /custom-metrics/creating-metrics, read 2026-11-19).
const (
	// MaxDescriptors is "Custom metric descriptors per project: 10,000".
	MaxDescriptors = 10000
	// MaxLabels is "Labels per custom, external, and workload metric
	// descriptor: 30".
	MaxLabels = 30
	// MaxLabelKey and MaxLabelValue are the label key and value lengths.
	MaxLabelKey   = 100
	MaxLabelValue = 1024
	// MaxSeriesPerWrite is "Time series included in a write request: 200".
	MaxSeriesPerWrite = 200
	// MinWriteInterval is "one point every 5 seconds" per time series.
	MinWriteInterval = 5 * time.Second
	// MaxFuture and MaxPast bound a written point's end time.
	MaxFuture = 5 * time.Minute
	MaxPast   = 25 * time.Hour
	// MaxPointsPerSeries bounds what one series keeps locally: the oldest
	// points go first. Google keeps 24 months; a series written every 5
	// seconds reaches this in about two weeks.
	MaxPointsPerSeries = 250000
)

// userDomains are the metric domains user-defined metrics may use.
var userDomains = []string{"custom.googleapis.com/", "external.googleapis.com/", "workload.googleapis.com/"}

// isUserDefined is whether a metric type is user-defined.
func isUserDefined(t string) bool {
	for _, d := range userDomains {
		if strings.HasPrefix(t, d) && len(t) > len(d) {
			return true
		}
	}
	return false
}

var (
	projectNameRE = regexp.MustCompile(`^projects/([a-z0-9][a-z0-9-]{0,61}[a-z0-9]|[0-9]+)$`)
	labelKeyRE    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	metricTypeRE  = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z]+/[A-Za-z0-9_./-]*[A-Za-z0-9_]$`)
)

// series is one stored time series: its identity and its points, oldest
// first.
type series struct {
	TS *monitoringpb.TimeSeries
}

// Builtin produces the time series of built-in metric types, which are
// read and never written.
type Builtin interface {
	// Descriptors are the built-in metric descriptors of a project.
	Descriptors(project string) []*metricpb.MetricDescriptor
	// Series are a built-in type's series in a project, each point's
	// interval within [start, end]; points oldest first.
	Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error)
}

// Server is the Monitoring API.
type Server struct {
	monitoringpb.UnimplementedMetricServiceServer
	monitoringpb.UnimplementedAlertPolicyServiceServer
	monitoringpb.UnimplementedNotificationChannelServiceServer
	monitoringpb.UnimplementedUptimeCheckServiceServer
	monitoringpb.UnimplementedGroupServiceServer
	monitoringpb.UnimplementedServiceMonitoringServiceServer
	monitoringpb.UnimplementedSnoozeServiceServer
	dashboardpb.UnimplementedDashboardsServiceServer

	state string // the file; "" keeps everything in memory

	mu sync.Mutex
	// descriptors are user-defined metric descriptors by resource name.
	descriptors map[string]*metricpb.MetricDescriptor
	// series by project, then by series key.
	series map[string]map[string]*series

	// objects are the configuration objects by kind, then resource name.
	objects map[string]map[string]proto.Message
	// incidents are every project's, oldest first.
	incidents  []*Incident
	deliveries []Delivery
	codes      map[string]verificationCode
	lastEval   time.Time
	logs       LogMatcher
	publish    Publisher
	consoleURL string
	uptime     uptimeState

	builtins []Builtin
	// now is the clock, for tests.
	now func() time.Time
}

// NewServer returns a server whose data is kept in state ("" keeps it in
// memory only). A state file that cannot be read is an error.
func NewServer(state string) (*Server, error) {
	s := &Server{state: state, now: time.Now,
		descriptors: map[string]*metricpb.MetricDescriptor{}, series: map[string]map[string]*series{},
		objects: map[string]map[string]proto.Message{}, codes: map[string]verificationCode{},
		uptime: uptimeState{last: map[string]time.Time{}, results: map[string][]UptimeResult{}, targets: map[string]*monitoredres.MonitoredResource{}}}
	s.builtins = append(s.builtins, uptimeBuiltin{s})
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetClock replaces the clock, for tests.
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// AddBuiltin adds a producer of built-in metric types.
func (s *Server) AddBuiltin(b Builtin) {
	s.mu.Lock()
	s.builtins = append(s.builtins, b)
	s.mu.Unlock()
}

// Register adds the services to a gRPC server, or to the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	monitoringpb.RegisterMetricServiceServer(g, s)
	monitoringpb.RegisterAlertPolicyServiceServer(g, s)
	monitoringpb.RegisterNotificationChannelServiceServer(g, s)
	monitoringpb.RegisterUptimeCheckServiceServer(g, s)
	monitoringpb.RegisterGroupServiceServer(g, s)
	monitoringpb.RegisterServiceMonitoringServiceServer(g, s)
	monitoringpb.RegisterSnoozeServiceServer(g, s)
	dashboardpb.RegisterDashboardsServiceServer(g, s)
}

type disk struct {
	Descriptors map[string]json.RawMessage            `json:"descriptors"`
	Series      map[string]map[string]json.RawMessage `json:"series"`
	Objects     map[string]map[string]json.RawMessage `json:"objects,omitempty"`
	Incidents   []*Incident                           `json:"incidents,omitempty"`
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Cloud Monitoring state: %w", err)
	}
	var d disk
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("parse %s: %w", s.state, err)
	}
	for n, raw := range d.Descriptors {
		md := &metricpb.MetricDescriptor{}
		if err := protojson.Unmarshal(raw, md); err != nil {
			return fmt.Errorf("parse metric descriptor %s: %w", n, err)
		}
		s.descriptors[n] = md
	}
	for p, m := range d.Series {
		s.series[p] = map[string]*series{}
		for k, raw := range m {
			ts := &monitoringpb.TimeSeries{}
			if err := protojson.Unmarshal(raw, ts); err != nil {
				return fmt.Errorf("parse time series %s: %w", k, err)
			}
			s.series[p][k] = &series{TS: ts}
		}
	}
	for kind, m := range d.Objects {
		if newObject(kind) == nil {
			continue
		}
		for n, raw := range m {
			o := newObject(kind)
			if err := protojson.Unmarshal(raw, o); err != nil {
				return fmt.Errorf("parse %s %s: %w", kind, n, err)
			}
			s.put(kind, n, o)
		}
	}
	s.incidents = d.Incidents
	return nil
}

// save writes everything; the caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	d := disk{Descriptors: map[string]json.RawMessage{}, Series: map[string]map[string]json.RawMessage{}}
	for n, md := range s.descriptors {
		b, err := protojson.Marshal(md)
		if err != nil {
			return err
		}
		d.Descriptors[n] = b
	}
	for p, m := range s.series {
		d.Series[p] = map[string]json.RawMessage{}
		for k, sr := range m {
			b, err := protojson.Marshal(sr.TS)
			if err != nil {
				return err
			}
			d.Series[p][k] = b
		}
	}
	d.Objects = map[string]map[string]json.RawMessage{}
	for kind, m := range s.objects {
		d.Objects[kind] = map[string]json.RawMessage{}
		for n, o := range m {
			b, err := protojson.Marshal(o)
			if err != nil {
				return err
			}
			d.Objects[kind][n] = b
		}
	}
	d.Incidents = s.incidents
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// ResetProject deletes the user-defined descriptors, series, configuration
// objects and incidents of project,
// or of every project when project is "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := range s.descriptors {
		if project == "" || strings.HasPrefix(n, "projects/"+project+"/") {
			delete(s.descriptors, n)
		}
	}
	if project == "" {
		s.series = map[string]map[string]*series{}
		s.objects = map[string]map[string]proto.Message{}
		s.incidents, s.deliveries = nil, nil
	} else {
		delete(s.series, project)
		for _, m := range s.objects {
			for n := range m {
				if strings.HasPrefix(n, "projects/"+project+"/") {
					delete(m, n)
				}
			}
		}
		var keep []*Incident
		for _, in := range s.incidents {
			if in.Project != project {
				keep = append(keep, in)
			}
		}
		s.incidents = keep
	}
	return s.save()
}

// seriesKey identifies a time series: its metric and its resource.
func seriesKey(ts *monitoringpb.TimeSeries) string {
	var b strings.Builder
	b.WriteString(ts.GetMetric().GetType())
	writeLabels(&b, ts.GetMetric().GetLabels())
	b.WriteString("|")
	b.WriteString(ts.GetResource().GetType())
	writeLabels(&b, ts.GetResource().GetLabels())
	return b.String()
}

func writeLabels(b *strings.Builder, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "\x00%s=%s", k, m[k])
	}
}

func clone[M proto.Message](m M) M { return proto.Clone(m).(M) }

// descriptorName is a metric descriptor's resource name.
func descriptorName(project, metricType string) string {
	return "projects/" + project + "/metricDescriptors/" + metricType
}
