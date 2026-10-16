package logging

// The Logging admin resources CloudBurrow stores (#1226): log-based metrics
// (google.logging.v2.MetricsServiceV2, every method), and of
// google.logging.v2.ConfigServiceV2 the project sinks and exclusions.
//
// They are configuration, kept in a store.Store (durable in persistent mode),
// unlike the bounded in-memory entries. What each one does:
//   - A metric is stored and validated as the proto documents. It produces no
//     time series: Cloud Monitoring is not emulated, so nothing reads one.
//     CountMatching counts the entries its filter matches over a window, for
//     the console.
//   - A sink stores its destination and filter. Only a log-bucket destination
//     is delivered to (server.go routeOf, #1395); nothing else is.
//   - An enabled exclusion is applied as Google applies a project exclusion to
//     the _Default sink: a matching entry written after it is not stored, so it
//     is never listed. Only the _Default bucket is modelled, so that is every
//     entry.
//
// Filters are compiled with the ListLogEntries grammar (filter.go), so a
// filter the store could not evaluate is INVALID_ARGUMENT naming the term,
// rather than stored and silently never matching.
//
// Buckets and views are in buckets.go (#1264); the Async bucket calls, links
// and settings in links.go (#1292). Folder, organization and billing-account
// parents are in parent.go (#1395).

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/label"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Documented limits (logging_metrics.proto, logging_config.proto).
const (
	maxIDLen          = 100
	maxDescriptionLen = 8000
	maxFilterLen      = 20000
)

var (
	// metricIDRe is a metric ID's documented alphabet. A slash denotes a
	// hierarchy and cannot come first.
	metricIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-.,+!*'()%][A-Za-z0-9_\-.,+!*'()%/]*$`)
	// configIDRe is a sink's and an exclusion's: letters, digits, underscores,
	// hyphens and periods, the first alphanumeric.
	configIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-.]*$`)
	// extractorRe is EXTRACT(field) or REGEXP_EXTRACT(field, "regex").
	extractorRe = regexp.MustCompile(`^\s*(EXTRACT|REGEXP_EXTRACT)\s*\(\s*([A-Za-z_][A-Za-z0-9_.\-@"\[\]]*)\s*(?:,\s*("(?:[^"\\]|\\.)*")\s*)?\)\s*$`)
)

// Admin serves MetricsServiceV2 and the sinks and exclusions of
// ConfigServiceV2 over one store.
type Admin struct {
	mu  sync.Mutex
	h   Hierarchy // folders and the organization, or nil (#1395)
	db  store.Store
	now func() time.Time
	ops int // operations answered, for unique names (#1292)
}

// NewAdmin returns the admin services over db.
func NewAdmin(db store.Store) *Admin { return &Admin{db: db, now: time.Now} }

// Register adds MetricsServiceV2 and ConfigServiceV2 to a gRPC server or the
// JSON transcoder.
func (a *Admin) Register(g grpc.ServiceRegistrar) {
	loggingpb.RegisterMetricsServiceV2Server(g, metricsServer{a})
	loggingpb.RegisterConfigServiceV2Server(g, configServer{a: a})
}

// kind is one stored collection.
type kind struct {
	collection string // the resource name's collection: metrics, sinks, exclusions
	noun       string
	newMsg     func() proto.Message
}

var (
	kindMetric    = kind{"metrics", "metric", func() proto.Message { return &loggingpb.LogMetric{} }}
	kindSink      = kind{"sinks", "sink", func() proto.Message { return &loggingpb.LogSink{} }}
	kindExclusion = kind{"exclusions", "exclusion", func() proto.Message { return &loggingpb.LogExclusion{} }}
)

// key is the store key of one resource. IDs are hex-encoded: a metric ID may
// hold a slash or a percent sign, which a key must not.
func (k kind) key(p parent, id string) string {
	return k.prefix(p) + hex.EncodeToString([]byte(id))
}

func (k kind) prefix(p parent) string { return "logging/" + k.collection + "/" + p.seg() + "/" }

// name is the resource name. A metric's ID is escaped as Google escapes it
// (projects/my-project/metrics/nginx%2Frequests).
func (k kind) name(p parent, id string) string {
	return p.String() + "/" + k.collection + "/" + url.PathEscape(id)
}

// parseName splits {parent}/{collection}/{id}. The ID may be escaped; a
// metric's may also be given raw, slashes and all. A metric's parent is a
// project, the only one Google documents for it.
func (a *Admin) parseName(k kind, field, name string) (parent, string, error) {
	shape := "{parent}/" + k.collection + "/{id}"
	if k.collection == kindMetric.collection {
		shape = "projects/{project}/metrics/{id}"
	}
	p, rest, err := cutParent(field, name)
	if err != nil {
		return parent{}, "", err
	}
	raw, ok := strings.CutPrefix(rest, k.collection+"/")
	if !ok || raw == "" {
		return parent{}, "", apierror.InvalidArgument("%s %q is not %s", field, name, shape)
	}
	if k.collection == kindMetric.collection && !p.isProject() {
		return parent{}, "", apierror.InvalidArgument("%s %q: log-based metrics belong to projects only (projects/{project}/metrics/{id}), as Google documents", field, name)
	}
	id, uerr := url.PathUnescape(raw)
	if uerr != nil {
		return parent{}, "", apierror.InvalidArgument("%s %q: %v", field, name, uerr)
	}
	return p, id, a.check(p)
}

// parseParent checks a sink's or an exclusion's parent: any of the four.
func (a *Admin) parseParent(name string) (parent, error) { return a.resolve("parent", name) }

// parseMetricParent checks a metric's parent, which is a project only.
func (a *Admin) parseMetricParent(name string) (parent, error) {
	if p, _, err := cutParent("parent", name); err == nil && !p.isProject() {
		return parent{}, apierror.InvalidArgument("parent %q: log-based metrics belong to projects only (projects/{project}), as Google documents", name)
	}
	return a.resolve("parent", name)
}

func (a *Admin) get(k kind, project parent, id string) (proto.Message, error) {
	b, err := a.db.Get(k.key(project, id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("%s %s does not exist", k.noun, k.name(project, id))
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the %s", k.noun)
	}
	m := k.newMsg()
	if err := protojson.Unmarshal(b, m); err != nil {
		return nil, apierror.Internal(err, "decoding the %s", k.noun)
	}
	return m, nil
}

func (a *Admin) put(k kind, project parent, id string, m proto.Message) error {
	b, err := protojson.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encoding the %s", k.noun)
	}
	if err := a.db.Put(k.key(project, id), b); err != nil {
		return apierror.Internal(err, "writing the %s", k.noun)
	}
	return nil
}

func (a *Admin) delete(k kind, project parent, id string) error {
	if _, err := a.get(k, project, id); err != nil {
		return err
	}
	if err := a.db.Delete(k.key(project, id)); err != nil {
		return apierror.Internal(err, "deleting the %s", k.noun)
	}
	return nil
}

// list returns one page of a project's resources, in ID order.
func (a *Admin) list(k kind, project parent, token string, size int32) ([]proto.Message, string, error) {
	keys, err := a.db.List(k.prefix(project))
	if err != nil {
		return nil, "", apierror.Internal(err, "listing %s", k.collection)
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		raw, err := hex.DecodeString(strings.TrimPrefix(key, k.prefix(project)))
		if err == nil {
			ids = append(ids, string(raw))
		}
	}
	page, next, err := paging.Page("logging-"+k.collection+"|"+project.String(), ids, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("page_token: %v", err)
	}
	out := make([]proto.Message, 0, len(page))
	for _, id := range page {
		m, err := a.get(k, project, id)
		if err != nil {
			continue // deleted between the listing and the read
		}
		out = append(out, m)
	}
	return out, next, nil
}

func checkID(noun, id string, re *regexp.Regexp) error {
	switch {
	case id == "":
		return apierror.InvalidArgument("%s name is required", noun)
	case len(id) > maxIDLen:
		return apierror.InvalidArgument("%s name %q is longer than %d characters", noun, id, maxIDLen)
	case !re.MatchString(id):
		if noun == "metric" {
			return apierror.InvalidArgument("metric name %q may hold only A-Z, a-z, 0-9 and _-.,+!*',()%%/, and cannot start with /", id)
		}
		return apierror.InvalidArgument("%s name %q may hold only letters, digits, underscores, hyphens and periods, and must start with a letter or digit", noun, id)
	}
	return nil
}

func checkFilter(filter string, required bool) error {
	if strings.TrimSpace(filter) == "" {
		if required {
			return apierror.InvalidArgument("filter is required")
		}
		return nil
	}
	if len(filter) > maxFilterLen {
		return apierror.InvalidArgument("filter is longer than %d characters", maxFilterLen)
	}
	_, err := compile(filter)
	return err
}

func checkDescription(d string) error {
	if len(d) > maxDescriptionLen {
		return apierror.InvalidArgument("description is longer than %d characters", maxDescriptionLen)
	}
	return nil
}

// ---- Log-based metrics ----

type metricsServer struct{ a *Admin }

var _ loggingpb.MetricsServiceV2Server = metricsServer{}

// metricType is the Cloud Monitoring type of a user-defined log metric.
func metricType(id string) string { return "logging.googleapis.com/user/" + id }

// checkExtractor checks one EXTRACT or REGEXP_EXTRACT expression.
func checkExtractor(what, expr string) error {
	m := extractorRe.FindStringSubmatch(expr)
	if m == nil {
		return apierror.InvalidArgument("%s %q is not EXTRACT(field) or REGEXP_EXTRACT(field, \"regex\")", what, expr)
	}
	switch {
	case m[1] == "EXTRACT" && m[3] != "":
		return apierror.InvalidArgument("%s %q: EXTRACT takes a field and no regex", what, expr)
	case m[1] == "REGEXP_EXTRACT" && m[3] == "":
		return apierror.InvalidArgument("%s %q: REGEXP_EXTRACT takes a field and a regex", what, expr)
	case m[1] == "REGEXP_EXTRACT":
		pattern := strings.ReplaceAll(m[3][1:len(m[3])-1], `\"`, `"`)
		re, err := regexp.Compile(pattern)
		if err != nil {
			return apierror.InvalidArgument("%s %q: the regex is not RE2: %v", what, expr, err)
		}
		if re.NumSubexp() != 1 {
			return apierror.InvalidArgument("%s %q: the regex must have exactly one capture group, it has %d", what, expr, re.NumSubexp())
		}
	}
	return nil
}

// checkBuckets checks a distribution's bucket options, as Cloud Monitoring
// defines them: exactly one of linear, exponential and explicit.
func checkBuckets(b *distribution.Distribution_BucketOptions) error {
	switch o := b.GetOptions().(type) {
	case *distribution.Distribution_BucketOptions_LinearBuckets:
		if o.LinearBuckets.GetNumFiniteBuckets() <= 0 || o.LinearBuckets.GetWidth() <= 0 {
			return apierror.InvalidArgument("bucket_options.linear_buckets needs num_finite_buckets > 0 and width > 0")
		}
	case *distribution.Distribution_BucketOptions_ExponentialBuckets:
		e := o.ExponentialBuckets
		if e.GetNumFiniteBuckets() <= 0 || e.GetGrowthFactor() <= 1 || e.GetScale() <= 0 {
			return apierror.InvalidArgument("bucket_options.exponential_buckets needs num_finite_buckets > 0, growth_factor > 1 and scale > 0")
		}
	case *distribution.Distribution_BucketOptions_ExplicitBuckets:
		bounds := o.ExplicitBuckets.GetBounds()
		if len(bounds) == 0 {
			return apierror.InvalidArgument("bucket_options.explicit_buckets needs at least one bound")
		}
		for i := 1; i < len(bounds); i++ {
			if bounds[i] <= bounds[i-1] {
				return apierror.InvalidArgument("bucket_options.explicit_buckets.bounds must be strictly increasing")
			}
		}
	default:
		return apierror.InvalidArgument("bucket_options needs one of linear_buckets, exponential_buckets or explicit_buckets")
	}
	return nil
}

// prepareMetric validates a metric and fills its output-only fields. old is
// the stored metric an update replaces, or nil.
func (a *Admin) prepareMetric(p parent, in, old *loggingpb.LogMetric) (*loggingpb.LogMetric, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("metric is required")
	}
	project := p.id
	m := proto.Clone(in).(*loggingpb.LogMetric)
	id := m.GetName()
	if err := checkID("metric", id, metricIDRe); err != nil {
		return nil, err
	}
	if err := checkDescription(m.GetDescription()); err != nil {
		return nil, err
	}
	if err := checkFilter(m.GetFilter(), true); err != nil {
		return nil, err
	}
	if b := m.GetBucketName(); b != "" {
		if !strings.HasPrefix(b, "projects/"+project+"/locations/") || !strings.Contains(b, "/buckets/") {
			return nil, apierror.InvalidArgument("bucket_name %q must be a log bucket of project %s: projects/%s/locations/{location}/buckets/{bucket}", b, project, project)
		}
	}

	d := m.GetMetricDescriptor()
	if d == nil {
		d = &metric.MetricDescriptor{}
	}
	if d.GetMetricKind() == metric.MetricDescriptor_METRIC_KIND_UNSPECIFIED {
		d.MetricKind = metric.MetricDescriptor_DELTA
	}
	if d.GetValueType() == metric.MetricDescriptor_VALUE_TYPE_UNSPECIFIED {
		d.ValueType = metric.MetricDescriptor_INT64
	}
	if d.GetUnit() == "" {
		d.Unit = "1"
	}
	if d.GetMetricKind() != metric.MetricDescriptor_DELTA {
		return nil, apierror.InvalidArgument("metric_descriptor.metric_kind must be DELTA, not %s", d.GetMetricKind())
	}
	switch d.GetValueType() {
	case metric.MetricDescriptor_INT64:
		if m.GetValueExtractor() != "" {
			return nil, apierror.InvalidArgument("value_extractor is only for a DISTRIBUTION metric; this one is INT64, a counter")
		}
		if m.GetBucketOptions() != nil {
			return nil, apierror.InvalidArgument("bucket_options is only for a DISTRIBUTION metric; this one is INT64, a counter")
		}
	case metric.MetricDescriptor_DISTRIBUTION:
		if m.GetValueExtractor() == "" {
			return nil, apierror.InvalidArgument("value_extractor is required for a DISTRIBUTION metric")
		}
		if err := checkExtractor("value_extractor", m.GetValueExtractor()); err != nil {
			return nil, err
		}
		if m.GetBucketOptions() == nil {
			return nil, apierror.InvalidArgument("bucket_options is required for a DISTRIBUTION metric")
		}
		if err := checkBuckets(m.GetBucketOptions()); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("metric_descriptor.value_type must be INT64 (a counter) or DISTRIBUTION, not %s", d.GetValueType())
	}

	labels := map[string]bool{}
	for _, l := range d.GetLabels() {
		if l.GetKey() == "" {
			return nil, apierror.InvalidArgument("metric_descriptor.labels: a label has no key")
		}
		if labels[l.GetKey()] {
			return nil, apierror.InvalidArgument("metric_descriptor.labels: label %q is listed twice", l.GetKey())
		}
		labels[l.GetKey()] = true
		expr, ok := m.GetLabelExtractors()[l.GetKey()]
		if !ok {
			return nil, apierror.InvalidArgument("label %q has no extractor in label_extractors", l.GetKey())
		}
		if err := checkExtractor(fmt.Sprintf("label_extractors[%q]", l.GetKey()), expr); err != nil {
			return nil, err
		}
	}
	for key := range m.GetLabelExtractors() {
		if !labels[key] {
			return nil, apierror.InvalidArgument("label_extractors[%q] has no label in metric_descriptor.labels", key)
		}
	}

	if old != nil {
		od := old.GetMetricDescriptor()
		if od.GetMetricKind() != d.GetMetricKind() || od.GetValueType() != d.GetValueType() {
			return nil, apierror.InvalidArgument("metric_descriptor.metric_kind and value_type cannot be changed once set (%s %s)", od.GetMetricKind(), od.GetValueType())
		}
		for _, ol := range od.GetLabels() {
			var nl *label.LabelDescriptor
			for _, l := range d.GetLabels() {
				if l.GetKey() == ol.GetKey() {
					nl = l
				}
			}
			if nl == nil || nl.GetValueType() != ol.GetValueType() {
				return nil, apierror.InvalidArgument("label %q cannot be removed or changed, except its description", ol.GetKey())
			}
		}
	}

	// Output only, built from the metric's name and description.
	d.Name = "projects/" + project + "/metricDescriptors/" + metricType(id)
	d.Type = metricType(id)
	d.Description = m.GetDescription()
	m.MetricDescriptor = d
	m.Version = loggingpb.LogMetric_V2
	now := timestamppb.New(a.now())
	m.CreateTime, m.UpdateTime = now, now
	if old != nil {
		m.CreateTime = old.GetCreateTime()
	}
	return m, nil
}

func (s metricsServer) ListLogMetrics(_ context.Context, req *loggingpb.ListLogMetricsRequest) (*loggingpb.ListLogMetricsResponse, error) {
	project, err := s.a.parseMetricParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := s.a.list(kindMetric, project, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	resp := &loggingpb.ListLogMetricsResponse{NextPageToken: next}
	for _, m := range page {
		resp.Metrics = append(resp.Metrics, m.(*loggingpb.LogMetric))
	}
	return resp, nil
}

func (s metricsServer) GetLogMetric(_ context.Context, req *loggingpb.GetLogMetricRequest) (*loggingpb.LogMetric, error) {
	project, id, err := s.a.parseName(kindMetric, "metric_name", req.GetMetricName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	m, err := s.a.get(kindMetric, project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return m.(*loggingpb.LogMetric), nil
}

func (s metricsServer) CreateLogMetric(_ context.Context, req *loggingpb.CreateLogMetricRequest) (*loggingpb.LogMetric, error) {
	project, err := s.a.parseMetricParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	m, err := s.a.prepareMetric(project, req.GetMetric(), nil)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := s.a.get(kindMetric, project, m.GetName()); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("metric %s already exists", kindMetric.name(project, m.GetName())))
	}
	if err := s.a.put(kindMetric, project, m.GetName(), m); err != nil {
		return nil, apierror.Wrap(err)
	}
	return m, nil
}

// UpdateLogMetric replaces a metric, or creates it when it does not exist, as
// the proto documents. The metric's name must be the one in metric_name.
func (s metricsServer) UpdateLogMetric(_ context.Context, req *loggingpb.UpdateLogMetricRequest) (*loggingpb.LogMetric, error) {
	project, id, err := s.a.parseName(kindMetric, "metric_name", req.GetMetricName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetMetric() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("metric is required"))
	}
	if req.GetMetric().GetName() != id {
		return nil, apierror.Wrap(apierror.InvalidArgument("metric.name %q must be the [METRIC_ID] of metric_name, %q", req.GetMetric().GetName(), id))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	var old *loggingpb.LogMetric
	if o, err := s.a.get(kindMetric, project, id); err == nil {
		old = o.(*loggingpb.LogMetric)
	}
	m, err := s.a.prepareMetric(project, req.GetMetric(), old)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.a.put(kindMetric, project, id, m); err != nil {
		return nil, apierror.Wrap(err)
	}
	return m, nil
}

func (s metricsServer) DeleteLogMetric(_ context.Context, req *loggingpb.DeleteLogMetricRequest) (*emptypb.Empty, error) {
	project, id, err := s.a.parseName(kindMetric, "metric_name", req.GetMetricName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.delete(kindMetric, project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- Sinks and exclusions ----

type configServer struct {
	loggingpb.UnimplementedConfigServiceV2Server
	a *Admin
}

// checkDestination checks a sink destination against the forms the proto
// documents. The destination is stored; nothing is delivered to it.
func checkDestination(d string) error {
	parts := strings.Split(d, "/")
	ok := false
	switch {
	case d == "":
		return apierror.InvalidArgument("destination is required")
	case strings.HasPrefix(d, "storage.googleapis.com/"):
		ok = len(parts) == 2 && parts[1] != ""
	case strings.HasPrefix(d, "bigquery.googleapis.com/"):
		ok = len(parts) == 5 && parts[1] == "projects" && parts[2] != "" && parts[3] == "datasets" && parts[4] != ""
	case strings.HasPrefix(d, "pubsub.googleapis.com/"):
		ok = len(parts) == 5 && parts[1] == "projects" && parts[2] != "" && parts[3] == "topics" && parts[4] != ""
	case strings.HasPrefix(d, "logging.googleapis.com/"):
		ok = (len(parts) == 3 && parts[1] == "projects" && parts[2] != "") ||
			(len(parts) == 7 && parts[1] == "projects" && parts[2] != "" && parts[3] == "locations" && parts[4] != "" && parts[5] == "buckets" && parts[6] != "")
	}
	if !ok {
		return apierror.InvalidArgument("destination %q is not one of storage.googleapis.com/[BUCKET], "+
			"bigquery.googleapis.com/projects/[PROJECT]/datasets/[DATASET], pubsub.googleapis.com/projects/[PROJECT]/topics/[TOPIC], "+
			"logging.googleapis.com/projects/[PROJECT]/locations/[LOCATION]/buckets/[BUCKET] or logging.googleapis.com/projects/[PROJECT]", d)
	}
	return nil
}

func (a *Admin) checkExclusion(e *loggingpb.LogExclusion) error {
	if err := checkID("exclusion", e.GetName(), configIDRe); err != nil {
		return err
	}
	if err := checkDescription(e.GetDescription()); err != nil {
		return err
	}
	return checkFilter(e.GetFilter(), true)
}

// checkSink validates a sink, its embedded exclusions included.
func (a *Admin) checkSink(p parent, sk *loggingpb.LogSink) error {
	if err := checkID("sink", sk.GetName(), configIDRe); err != nil {
		return err
	}
	if err := checkDestination(sk.GetDestination()); err != nil {
		return err
	}
	if err := checkFilter(sk.GetFilter(), false); err != nil {
		return err
	}
	if err := checkDescription(sk.GetDescription()); err != nil {
		return err
	}
	if sk.GetIncludeChildren() && !p.hasChildren() {
		return apierror.InvalidArgument("include_children applies only to sinks of organizations and folders, not of %s", p)
	}
	seen := map[string]bool{}
	for i, e := range sk.GetExclusions() {
		if err := a.checkExclusion(e); err != nil {
			return apierror.InvalidArgument("exclusions[%d]: %v", i, apierror.From(err).Message)
		}
		if seen[e.GetName()] {
			return apierror.InvalidArgument("exclusions[%d]: exclusion %q is listed twice", i, e.GetName())
		}
		seen[e.GetName()] = true
	}
	return nil
}

// applyMask copies the masked top-level fields of src into dst. Output-only
// and immutable fields, and unknown paths, are INVALID_ARGUMENT.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask, allowed map[string]bool) error {
	dr, sr := dst.ProtoReflect(), src.ProtoReflect()
	for _, p := range mask.GetPaths() {
		if !allowed[p] {
			return apierror.InvalidArgument("update_mask: %q cannot be updated", p)
		}
		fd := dr.Descriptor().Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return apierror.InvalidArgument("update_mask: %q is not a field", p)
		}
		if sr.Has(fd) {
			dr.Set(fd, sr.Get(fd))
		} else {
			dr.Clear(fd)
		}
	}
	return nil
}

// sinkUpdatable are the sink fields an update_mask may name.
var sinkUpdatable = map[string]bool{
	"destination": true, "filter": true, "description": true, "disabled": true,
	"exclusions": true, "include_children": true, "bigquery_options": true, "output_version_format": true,
}

// exclusionUpdatable are the exclusion fields an update_mask may name.
var exclusionUpdatable = map[string]bool{"description": true, "filter": true, "disabled": true}

func (s configServer) ListSinks(_ context.Context, req *loggingpb.ListSinksRequest) (*loggingpb.ListSinksResponse, error) {
	project, err := s.a.parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := s.a.list(kindSink, project, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	resp := &loggingpb.ListSinksResponse{NextPageToken: next}
	for _, m := range page {
		resp.Sinks = append(resp.Sinks, m.(*loggingpb.LogSink))
	}
	return resp, nil
}

func (s configServer) GetSink(_ context.Context, req *loggingpb.GetSinkRequest) (*loggingpb.LogSink, error) {
	project, id, err := s.a.parseName(kindSink, "sink_name", req.GetSinkName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	m, err := s.a.get(kindSink, project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return m.(*loggingpb.LogSink), nil
}

// stampSink fills a sink's output-only fields. writer_identity is left
// empty: the service account Google assigns names the project number, which
// this instance does not hold, and nothing is delivered under it.
func (a *Admin) stampSink(sk *loggingpb.LogSink, old *loggingpb.LogSink) {
	now := timestamppb.New(a.now())
	sk.CreateTime, sk.UpdateTime = now, now
	sk.WriterIdentity = ""
	if old != nil {
		sk.CreateTime = old.GetCreateTime()
	}
	if bq := sk.GetBigqueryOptions(); bq != nil {
		bq.UsesTimestampColumnPartitioning = bq.GetUsePartitionedTables()
	}
	for _, e := range sk.GetExclusions() {
		if e.GetCreateTime() == nil {
			e.CreateTime = now
		}
		e.UpdateTime = now
	}
}

func (s configServer) CreateSink(_ context.Context, req *loggingpb.CreateSinkRequest) (*loggingpb.LogSink, error) {
	project, err := s.a.parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetSink() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("sink is required"))
	}
	sk := proto.Clone(req.GetSink()).(*loggingpb.LogSink)
	if err := s.a.checkSink(project, sk); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.get(kindSink, project, sk.GetName()); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("sink %s already exists", kindSink.name(project, sk.GetName())))
	}
	s.a.stampSink(sk, nil)
	if err := s.a.put(kindSink, project, sk.GetName(), sk); err != nil {
		return nil, apierror.Wrap(err)
	}
	return sk, nil
}

// UpdateSink applies the update_mask. An empty mask is
// destination,filter,include_children, as the proto documents for backwards
// compatibility.
func (s configServer) UpdateSink(_ context.Context, req *loggingpb.UpdateSinkRequest) (*loggingpb.LogSink, error) {
	project, id, err := s.a.parseName(kindSink, "sink_name", req.GetSinkName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetSink() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("sink is required"))
	}
	mask := req.GetUpdateMask()
	if len(mask.GetPaths()) == 0 {
		mask = &fieldmaskpb.FieldMask{Paths: []string{"destination", "filter", "include_children"}}
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	o, err := s.a.get(kindSink, project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	old := o.(*loggingpb.LogSink)
	sk := proto.Clone(old).(*loggingpb.LogSink)
	if err := applyMask(sk, req.GetSink(), mask, sinkUpdatable); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.a.checkSink(project, sk); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.stampSink(sk, old)
	if err := s.a.put(kindSink, project, id, sk); err != nil {
		return nil, apierror.Wrap(err)
	}
	return sk, nil
}

func (s configServer) DeleteSink(_ context.Context, req *loggingpb.DeleteSinkRequest) (*emptypb.Empty, error) {
	project, id, err := s.a.parseName(kindSink, "sink_name", req.GetSinkName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.delete(kindSink, project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (s configServer) ListExclusions(_ context.Context, req *loggingpb.ListExclusionsRequest) (*loggingpb.ListExclusionsResponse, error) {
	project, err := s.a.parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := s.a.list(kindExclusion, project, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	resp := &loggingpb.ListExclusionsResponse{NextPageToken: next}
	for _, m := range page {
		resp.Exclusions = append(resp.Exclusions, m.(*loggingpb.LogExclusion))
	}
	return resp, nil
}

func (s configServer) GetExclusion(_ context.Context, req *loggingpb.GetExclusionRequest) (*loggingpb.LogExclusion, error) {
	project, id, err := s.a.parseName(kindExclusion, "name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	m, err := s.a.get(kindExclusion, project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return m.(*loggingpb.LogExclusion), nil
}

func (s configServer) CreateExclusion(_ context.Context, req *loggingpb.CreateExclusionRequest) (*loggingpb.LogExclusion, error) {
	project, err := s.a.parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetExclusion() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("exclusion is required"))
	}
	e := proto.Clone(req.GetExclusion()).(*loggingpb.LogExclusion)
	if err := s.a.checkExclusion(e); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.get(kindExclusion, project, e.GetName()); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("exclusion %s already exists", kindExclusion.name(project, e.GetName())))
	}
	now := timestamppb.New(s.a.now())
	e.CreateTime, e.UpdateTime = now, now
	if err := s.a.put(kindExclusion, project, e.GetName(), e); err != nil {
		return nil, apierror.Wrap(err)
	}
	return e, nil
}

// UpdateExclusion applies the update_mask, which is required.
func (s configServer) UpdateExclusion(_ context.Context, req *loggingpb.UpdateExclusionRequest) (*loggingpb.LogExclusion, error) {
	project, id, err := s.a.parseName(kindExclusion, "name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetExclusion() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("exclusion is required"))
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required: name the fields to change"))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	o, err := s.a.get(kindExclusion, project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	e := proto.Clone(o).(*loggingpb.LogExclusion)
	if err := applyMask(e, req.GetExclusion(), req.GetUpdateMask(), exclusionUpdatable); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.a.checkExclusion(e); err != nil {
		return nil, apierror.Wrap(err)
	}
	e.UpdateTime = timestamppb.New(s.a.now())
	if err := s.a.put(kindExclusion, project, id, e); err != nil {
		return nil, apierror.Wrap(err)
	}
	return e, nil
}

func (s configServer) DeleteExclusion(_ context.Context, req *loggingpb.DeleteExclusionRequest) (*emptypb.Empty, error) {
	project, id, err := s.a.parseName(kindExclusion, "name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.delete(kindExclusion, project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// excluder returns a predicate matching the entries an enabled exclusion of
// project drops, or nil when there is none.
func (a *Admin) excluder(project parent) pred {
	if a == nil {
		return nil
	}
	page, _, err := a.list(kindExclusion, project, "", 1000)
	if err != nil {
		return nil
	}
	var preds []pred
	for _, m := range page {
		e := m.(*loggingpb.LogExclusion)
		if e.GetDisabled() {
			continue
		}
		if p, err := compile(e.GetFilter()); err == nil {
			preds = append(preds, p)
		}
	}
	if len(preds) == 0 {
		return nil
	}
	return func(e *loggingpb.LogEntry) bool {
		for _, p := range preds {
			if p(e) {
				return true
			}
		}
		return false
	}
}

// Metrics lists a project's metrics, for the console.
func (a *Admin) Metrics(project string) ([]*loggingpb.LogMetric, error) {
	var out []*loggingpb.LogMetric
	token := ""
	for {
		page, next, err := a.list(kindMetric, projectParent(project), token, 1000)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			out = append(out, m.(*loggingpb.LogMetric))
		}
		if next == "" {
			break
		}
		token = next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// MetricsServer is the MetricsServiceV2 the transports serve, for the
// console to call in process.
func (a *Admin) MetricsServer() loggingpb.MetricsServiceV2Server { return metricsServer{a} }

// ConfigServer is the ConfigServiceV2 the transports serve, for the console.
func (a *Admin) ConfigServer() loggingpb.ConfigServiceV2Server { return configServer{a: a} }

// MetricName is a metric's resource name.
func MetricName(project, id string) string { return kindMetric.name(projectParent(project), id) }

// sinkRoutes are p's enabled sinks whose destination is a log bucket;
// children selects the include_children ones, for an entry below p.
func (a *Admin) sinkRoutes(p parent, children bool) []sinkRoute {
	page, _, err := a.list(kindSink, p, "", 1000)
	if err != nil {
		return nil
	}
	var out []sinkRoute
	for _, m := range page {
		sk := m.(*loggingpb.LogSink)
		if sk.GetDisabled() || (children && !sk.GetIncludeChildren()) {
			continue
		}
		bucket, ok := strings.CutPrefix(sk.GetDestination(), "logging.googleapis.com/")
		if !ok || !strings.Contains(bucket, "/buckets/") {
			continue
		}
		match, err := compile(sk.GetFilter())
		if err != nil {
			continue
		}
		r := sinkRoute{match: match, bucket: bucket}
		var ex []pred
		for _, e := range sk.GetExclusions() {
			if c, err := compile(e.GetFilter()); err == nil && !e.GetDisabled() {
				ex = append(ex, c)
			}
		}
		if len(ex) > 0 {
			r.excluded = func(e *loggingpb.LogEntry) bool {
				for _, p := range ex {
					if p(e) {
						return true
					}
				}
				return false
			}
		}
		out = append(out, r)
	}
	return out
}
