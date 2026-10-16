package main

// Cloud Logging's admin screens (#1226): Log-based metrics, Sinks and
// Exclusions, beside the Logs Explorer.
//
// Cloud Logging runs in this process, so every read and change is a call on
// the same MetricsServiceV2 and ConfigServiceV2 servers the gRPC and JSON
// transports serve, with the instance's fault rules applied first as the
// service's interceptor applies them. A refusal is the API's own, shown as it
// came.
//
// A metric's page counts the stored entries its filter matches over the last
// hour and day. That is the one figure the instance can give: Cloud Monitoring
// is not emulated, so a metric has no time series, and the page says so.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/label"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

const loggingNotStarted = "Cloud Logging has not started"

// loggingAdminScreen is what the three screens share: the service and the
// instance's fault rules.
type loggingAdminScreen struct {
	svc    *loggingService
	faults *admin.Faults
}

func (s loggingAdminScreen) admin() (*logging.Admin, error) {
	a := s.svc.Admin()
	if a == nil {
		return nil, errors.New(loggingNotStarted)
	}
	return a, nil
}

// fault applies the instance's fault rules for one method, as the gRPC
// interceptor does, so an injected fault fails the console as it fails an SDK.
func (s loggingAdminScreen) fault(ctx context.Context, method, resource string) error {
	return s.faults.Apply(ctx, "logging", method, resource)
}

// ofProject checks a resource name against the screen's project, so a
// hand-written request cannot reach another project's resource from it.
func ofProject(project, collection, name string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	if !strings.HasPrefix(name, "projects/"+project+"/"+collection+"/") {
		return fmt.Errorf("%s is not one of project %s's %s", name, project, collection)
	}
	return nil
}

func loggingTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return "—"
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

func enabledWord(disabled bool) string {
	if disabled {
		return "Disabled"
	}
	return "Enabled"
}

// ---- Log-based metrics ----

type loggingMetricsProvider struct{ loggingAdminScreen }

func (loggingMetricsProvider) ID() string    { return "logging-metrics" }
func (loggingMetricsProvider) Title() string { return "Log-based metrics" }

func metricKindWord(m *loggingpb.LogMetric) string {
	if m.GetMetricDescriptor().GetValueType() == metric.MetricDescriptor_DISTRIBUTION {
		return "Distribution"
	}
	return "Counter"
}

func (p loggingMetricsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Type", "Filter", "State", "Updated"}, NameColumn: "Metric", Noun: "log-based metrics"}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists log-based metrics per project. Choose one in the toolbar."
		return out, nil
	}
	if err := p.fault(ctx, loggingpb.MetricsServiceV2_ListLogMetrics_FullMethodName, projectResource(project)); err != nil {
		return console.Listing{}, err
	}
	ms, err := a.Metrics(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, m := range ms {
		out.Items = append(out.Items, console.Resource{Name: logging.MetricName(project, m.GetName()), Fields: map[string]string{
			"Type": metricKindWord(m), "Filter": m.GetFilter(), "State": enabledWord(m.GetDisabled()), "Updated": loggingTime(m.GetUpdateTime()),
		}})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

// metricFields are the create and edit form's fields. Type is shown on edit
// and cannot change, as the API refuses a changed value_type.
func metricFields(m *loggingpb.LogMetric) []console.Field {
	if m == nil {
		m = &loggingpb.LogMetric{}
	}
	bucketKind, bucketValues := bucketFormValues(m.GetBucketOptions())
	labels := ""
	if len(m.GetLabelExtractors()) > 0 {
		labels = console.FormatMap(m.GetLabelExtractors())
	}
	return []console.Field{
		{Name: "name", Label: "Metric name", Type: "text", Required: true, Default: m.GetName(), Immutable: m.GetName() != "",
			Pattern: `^[A-Za-z0-9_\-.,+!*'\(\)%][A-Za-z0-9_\-.,+!*'\(\)%\/]{0,99}$`,
			Help:    "Up to 100 of A-Z a-z 0-9 _-.,+!*',()%/, not starting with /."},
		{Name: "description", Label: "Description", Type: "text", Default: m.GetDescription()},
		{Name: "filter", Label: "Filter", Type: "textarea", Required: true, Default: m.GetFilter(),
			Help: "ANDed logName, severity, resource.type, timestamp, resource.labels.* and labels.* terms, as the Logs Explorer's query takes them."},
		{Name: "type", Label: "Type", Type: "select", Options: []string{"Counter", "Distribution"}, Default: metricKindWord(m),
			Immutable: m.GetName() != "", Help: "A counter counts matching entries; a distribution records the values its value extractor takes from them."},
		{Name: "unit", Label: "Unit", Type: "text", Default: m.GetMetricDescriptor().GetUnit(), Help: "Optional; 1 when empty."},
		{Name: "value_extractor", Label: "Value extractor", Type: "text", Default: m.GetValueExtractor(), Section: "Distribution",
			Help: `Distribution only: EXTRACT(field) or REGEXP_EXTRACT(field, "regex") with one capture group.`},
		{Name: "buckets", Label: "Buckets", Type: "select", Options: []string{"explicit", "linear", "exponential"}, Default: bucketKind, Section: "Distribution"},
		{Name: "bucket_values", Label: "Bucket values", Type: "text", Default: bucketValues, Section: "Distribution",
			Help: "explicit: the bounds, comma separated (10,100,1000); linear: count,width,offset; exponential: count,growth factor,scale."},
		{Name: "labels", Label: "Labels", Type: "map", Default: labels,
			Help: "Each label's key and its extractor, EXTRACT(field) or REGEXP_EXTRACT(field, \"regex\"). Labels are strings."},
		{Name: "disabled", Label: "Disabled", Type: "checkbox", Default: strconv.FormatBool(m.GetDisabled())},
	}
}

func bucketFormValues(b *distribution.Distribution_BucketOptions) (kind, values string) {
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	switch o := b.GetOptions().(type) {
	case *distribution.Distribution_BucketOptions_LinearBuckets:
		l := o.LinearBuckets
		return "linear", fmt.Sprintf("%d,%s,%s", l.GetNumFiniteBuckets(), f(l.GetWidth()), f(l.GetOffset()))
	case *distribution.Distribution_BucketOptions_ExponentialBuckets:
		e := o.ExponentialBuckets
		return "exponential", fmt.Sprintf("%d,%s,%s", e.GetNumFiniteBuckets(), f(e.GetGrowthFactor()), f(e.GetScale()))
	case *distribution.Distribution_BucketOptions_ExplicitBuckets:
		var s []string
		for _, v := range o.ExplicitBuckets.GetBounds() {
			s = append(s, f(v))
		}
		return "explicit", strings.Join(s, ",")
	}
	return "explicit", ""
}

func parseBuckets(kind, values string) (*distribution.Distribution_BucketOptions, error) {
	var nums []float64
	for _, part := range strings.Split(values, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, fmt.Errorf("bucket value %q is not a number", part)
		}
		nums = append(nums, v)
	}
	switch kind {
	case "", "explicit":
		return &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
			ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{Bounds: nums}}}, nil
	case "linear", "exponential":
		if len(nums) != 3 {
			return nil, fmt.Errorf("%s buckets take three values: count and two numbers", kind)
		}
		if kind == "linear" {
			return &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_LinearBuckets{
				LinearBuckets: &distribution.Distribution_BucketOptions_Linear{NumFiniteBuckets: int32(nums[0]), Width: nums[1], Offset: nums[2]}}}, nil
		}
		return &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExponentialBuckets{
			ExponentialBuckets: &distribution.Distribution_BucketOptions_Exponential{NumFiniteBuckets: int32(nums[0]), GrowthFactor: nums[1], Scale: nums[2]}}}, nil
	}
	return nil, fmt.Errorf("buckets %q is not explicit, linear or exponential", kind)
}

// metricFromForm builds the LogMetric a form describes. old, on edit, is the
// stored metric, whose labels keep their value types.
func metricFromForm(id string, values map[string]string, old *loggingpb.LogMetric) (*loggingpb.LogMetric, error) {
	m := &loggingpb.LogMetric{
		Name: id, Description: values["description"], Filter: values["filter"],
		Disabled: values["disabled"] == "true", BucketName: old.GetBucketName(),
		MetricDescriptor: &metric.MetricDescriptor{MetricKind: metric.MetricDescriptor_DELTA, ValueType: metric.MetricDescriptor_INT64,
			Unit: strings.TrimSpace(values["unit"])},
	}
	kind := values["type"]
	if old != nil {
		kind = metricKindWord(old)
	}
	if kind == "Distribution" {
		m.MetricDescriptor.ValueType = metric.MetricDescriptor_DISTRIBUTION
		m.ValueExtractor = strings.TrimSpace(values["value_extractor"])
		b, err := parseBuckets(values["buckets"], values["bucket_values"])
		if err != nil {
			return nil, err
		}
		m.BucketOptions = b
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := &label.LabelDescriptor{Key: k, ValueType: label.LabelDescriptor_STRING}
		for _, ol := range old.GetMetricDescriptor().GetLabels() {
			if ol.GetKey() == k {
				d = ol
			}
		}
		m.MetricDescriptor.Labels = append(m.MetricDescriptor.Labels, d)
	}
	if len(labels) > 0 {
		m.LabelExtractors = labels
	}
	return m, nil
}

func (loggingMetricsProvider) CreateForm() (string, []console.Field) {
	return "Create metric", metricFields(nil)
}

func (p loggingMetricsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a metric")
	}
	m, err := metricFromForm(strings.TrimSpace(values["name"]), values, nil)
	if err != nil {
		return "", err
	}
	if err := p.fault(ctx, loggingpb.MetricsServiceV2_CreateLogMetric_FullMethodName, projectResource(project)); err != nil {
		return "", err
	}
	created, err := a.MetricsServer().CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "projects/" + project, Metric: m})
	if err != nil {
		return "", err
	}
	return logging.MetricName(project, created.GetName()), nil
}

func (p loggingMetricsProvider) get(ctx context.Context, project, name string) (*loggingpb.LogMetric, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := ofProject(project, "metrics", name); err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.MetricsServiceV2_GetLogMetric_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.MetricsServer().GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: name})
}

func (p loggingMetricsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	m, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	d := m.GetMetricDescriptor()
	config := []console.Property{
		{Label: "Resource name", Value: path[0]},
		{Label: "Metric type", Value: d.GetType()},
		{Label: "Description", Value: orDash(m.GetDescription())},
		{Label: "Filter", Value: m.GetFilter()},
		{Label: "Kind", Value: d.GetMetricKind().String() + " " + d.GetValueType().String()},
		{Label: "Unit", Value: d.GetUnit()},
		{Label: "State", Value: enabledWord(m.GetDisabled())},
		{Label: "Created", Value: loggingTime(m.GetCreateTime())},
		{Label: "Updated", Value: loggingTime(m.GetUpdateTime())},
	}
	if m.GetValueExtractor() != "" {
		config = append(config, console.Property{Label: "Value extractor", Value: m.GetValueExtractor()})
	}
	if m.GetBucketOptions() != nil {
		k, v := bucketFormValues(m.GetBucketOptions())
		config = append(config, console.Property{Label: "Buckets", Value: k + " " + v})
	}
	for _, l := range d.GetLabels() {
		config = append(config, console.Property{Label: "Label " + l.GetKey(), Value: m.GetLabelExtractors()[l.GetKey()]})
	}
	counts := []console.Property{}
	for _, w := range []struct {
		label string
		d     time.Duration
	}{{"Last hour", time.Hour}, {"Last 24 hours", 24 * time.Hour}} {
		n, err := p.svc.api.CountMatching(project, m.GetFilter(), time.Now().Add(-w.d))
		v := strconv.Itoa(n) + " entries"
		if err != nil {
			v = consoleErr(err)
		}
		counts = append(counts, console.Property{Label: w.label, Value: v})
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Type", Value: metricKindWord(m)},
			{Label: "State", Value: enabledWord(m.GetDisabled())},
			{Label: "Matching entries, last hour", Value: counts[0].Value},
		},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Metric", Properties: config}}},
			{ID: "matches", Label: "Matching entries", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Stored entries the filter matches", Properties: counts}},
				Note: "Cloud Monitoring is not emulated, so this metric produces no time series. These are counts of the " +
					"entries the instance holds (at most 20,000, the oldest going first) that its filter matches, disabled or not."},
		},
		Edit: &console.EditForm{Label: "Edit metric", Fields: metricFields(m),
			Note: "The name and the type cannot change: the API refuses a changed value_type, and the name is the metric's identity."},
	}, nil
}

func (p loggingMetricsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a metric can be edited")
	}
	old, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	m, err := metricFromForm(old.GetName(), values, old)
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.MetricsServiceV2_UpdateLogMetric_FullMethodName, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	_, err = a.MetricsServer().UpdateLogMetric(ctx, &loggingpb.UpdateLogMetricRequest{MetricName: path[0], Metric: m})
	return err
}

func (p loggingMetricsProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := ofProject(project, "metrics", name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.MetricsServiceV2_DeleteLogMetric_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.MetricsServer().DeleteLogMetric(ctx, &loggingpb.DeleteLogMetricRequest{MetricName: name})
	return err
}

// ---- Sinks ----

type loggingSinksProvider struct{ loggingAdminScreen }

func (loggingSinksProvider) ID() string    { return "logging-sinks" }
func (loggingSinksProvider) Title() string { return "Log router sinks" }

const sinkNote = "A sink delivers each matching entry: to a log bucket and a Pub/Sub topic at once, to a BigQuery dataset streamed in, to a Cloud Storage bucket in hourly files, and to another project's _Default bucket and sinks. A destination that is missing is a sink error, in the project's logging.googleapis.com/sink_error log. An entry that does not fit its BigQuery table's schema is written to the dataset's export_errors table (export_errors_YYYYMMDD for date-sharded tables)."

// sinkDelivery is a sink's delivery status, for its row and its page.
func (s loggingAdminScreen) sinkDelivery(name string) (state, lastErr string) {
	if s.svc == nil || s.svc.api == nil {
		return "-", ""
	}
	st := s.svc.api.SinkStatus(name)
	if st == nil {
		return "Nothing delivered yet", ""
	}
	state = fmt.Sprintf("%d delivered", st.Delivered)
	if !st.LastDelivery.IsZero() {
		state += ", last " + st.LastDelivery.UTC().Format(time.RFC3339)
	}
	if st.Pending > 0 {
		state += fmt.Sprintf(", %d waiting for the hourly file", st.Pending)
	}
	if st.ErrorRows > 0 {
		state += fmt.Sprintf(", %d in the dataset's export_errors table", st.ErrorRows)
	}
	if st.LastError != "" {
		lastErr = st.LastErrorAt.UTC().Format(time.RFC3339) + " " + st.LastError
	}
	return state, lastErr
}

func (p loggingSinksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Destination", "Filter", "State", "Delivery", "Last error", "Updated"}, NameColumn: "Sink", Noun: "sinks", Note: sinkNote}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists sinks per project. Choose one in the toolbar."
		return out, nil
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_ListSinks_FullMethodName, projectResource(project)); err != nil {
		return console.Listing{}, err
	}
	resp, err := a.ConfigServer().ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, s := range resp.GetSinks() {
		name := "projects/" + project + "/sinks/" + s.GetName()
		delivery, lastErr := p.sinkDelivery(name)
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{
			"Destination": s.GetDestination(), "Filter": orDash(s.GetFilter()), "State": enabledWord(s.GetDisabled()),
			"Delivery": delivery, "Last error": orDash(lastErr), "Updated": loggingTime(s.GetUpdateTime()),
		}})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

func sinkFields(s *loggingpb.LogSink) []console.Field {
	if s == nil {
		s = &loggingpb.LogSink{}
	}
	return []console.Field{
		{Name: "name", Label: "Sink name", Type: "text", Required: true, Default: s.GetName(), Immutable: s.GetName() != "",
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "Up to 100 letters, digits, _, - and ., starting with a letter or digit."},
		{Name: "destination", Label: "Destination", Type: "text", Required: true, Default: s.GetDestination(),
			Help: "storage.googleapis.com/[BUCKET], bigquery.googleapis.com/projects/[P]/datasets/[D], pubsub.googleapis.com/projects/[P]/topics/[T] or logging.googleapis.com/projects/[P]/locations/[L]/buckets/[B]. " + sinkNote},
		{Name: "filter", Label: "Filter", Type: "textarea", Default: s.GetFilter(), Help: "Optional; empty matches every entry."},
		{Name: "description", Label: "Description", Type: "text", Default: s.GetDescription()},
		{Name: "disabled", Label: "Disabled", Type: "checkbox", Default: strconv.FormatBool(s.GetDisabled())},
	}
}

func (loggingSinksProvider) CreateForm() (string, []console.Field) {
	return "Create sink", sinkFields(nil)
}

func sinkFromForm(id string, v map[string]string) *loggingpb.LogSink {
	return &loggingpb.LogSink{Name: id, Destination: strings.TrimSpace(v["destination"]), Filter: v["filter"],
		Description: v["description"], Disabled: v["disabled"] == "true"}
}

func (p loggingSinksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a sink")
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateSink_FullMethodName, projectResource(project)); err != nil {
		return "", err
	}
	s, err := a.ConfigServer().CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/" + project,
		Sink: sinkFromForm(strings.TrimSpace(values["name"]), values)})
	if err != nil {
		return "", err
	}
	return "projects/" + project + "/sinks/" + s.GetName(), nil
}

func (p loggingSinksProvider) get(ctx context.Context, project, name string) (*loggingpb.LogSink, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := ofProject(project, "sinks", name); err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetSink_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
}

func (p loggingSinksProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	s, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	delivery, lastErr := p.sinkDelivery(path[0])
	props := []console.Property{
		{Label: "Resource name", Value: path[0]},
		{Label: "Destination", Value: s.GetDestination()},
		{Label: "Filter", Value: orDash(s.GetFilter())},
		{Label: "Description", Value: orDash(s.GetDescription())},
		{Label: "State", Value: enabledWord(s.GetDisabled())},
		{Label: "Writer identity", Value: orDash(s.GetWriterIdentity())},
		{Label: "Delivery", Value: delivery},
		{Label: "Last error", Value: orDash(lastErr)},
		{Label: "Created", Value: loggingTime(s.GetCreateTime())},
		{Label: "Updated", Value: loggingTime(s.GetUpdateTime())},
	}
	for _, e := range s.GetExclusions() {
		props = append(props, console.Property{Label: "Exclusion " + e.GetName(), Value: e.GetFilter()})
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Destination", Value: s.GetDestination()}, {Label: "State", Value: enabledWord(s.GetDisabled())},
			{Label: "Delivery", Value: delivery}},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Sink", Properties: props}}, Note: sinkNote}},
		Edit: &console.EditForm{Label: "Edit sink", Fields: sinkFields(s), Note: "The name is the sink's identity and cannot change."},
	}, nil
}

func (p loggingSinksProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a sink can be edited")
	}
	old, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateSink_FullMethodName, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	_, err = a.ConfigServer().UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: path[0], Sink: sinkFromForm(old.GetName(), values),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination", "filter", "description", "disabled"}}})
	return err
}

func (p loggingSinksProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := ofProject(project, "sinks", name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteSink_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.ConfigServer().DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: name})
	return err
}

// ---- Exclusions ----

type loggingExclusionsProvider struct{ loggingAdminScreen }

func (loggingExclusionsProvider) ID() string    { return "logging-exclusions" }
func (loggingExclusionsProvider) Title() string { return "Log exclusions" }

const exclusionNote = "An enabled exclusion drops the matching entries written after it, as the _Default sink does: they are accepted and never stored."

func (p loggingExclusionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Filter", "State", "Updated"}, NameColumn: "Exclusion", Noun: "exclusions", Note: exclusionNote}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists exclusions per project. Choose one in the toolbar."
		return out, nil
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_ListExclusions_FullMethodName, projectResource(project)); err != nil {
		return console.Listing{}, err
	}
	resp, err := a.ConfigServer().ListExclusions(ctx, &loggingpb.ListExclusionsRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, e := range resp.GetExclusions() {
		out.Items = append(out.Items, console.Resource{Name: "projects/" + project + "/exclusions/" + e.GetName(), Fields: map[string]string{
			"Filter": e.GetFilter(), "State": enabledWord(e.GetDisabled()), "Updated": loggingTime(e.GetUpdateTime()),
		}})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

func exclusionFields(e *loggingpb.LogExclusion) []console.Field {
	if e == nil {
		e = &loggingpb.LogExclusion{}
	}
	return []console.Field{
		{Name: "name", Label: "Exclusion name", Type: "text", Required: true, Default: e.GetName(), Immutable: e.GetName() != "",
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "Up to 100 letters, digits, _, - and ., starting with a letter or digit."},
		{Name: "filter", Label: "Filter", Type: "textarea", Required: true, Default: e.GetFilter(), Help: exclusionNote},
		{Name: "description", Label: "Description", Type: "text", Default: e.GetDescription()},
		{Name: "disabled", Label: "Disabled", Type: "checkbox", Default: strconv.FormatBool(e.GetDisabled())},
	}
}

func (loggingExclusionsProvider) CreateForm() (string, []console.Field) {
	return "Create exclusion", exclusionFields(nil)
}

func exclusionFromForm(id string, v map[string]string) *loggingpb.LogExclusion {
	return &loggingpb.LogExclusion{Name: id, Filter: v["filter"], Description: v["description"], Disabled: v["disabled"] == "true"}
}

func (p loggingExclusionsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating an exclusion")
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateExclusion_FullMethodName, projectResource(project)); err != nil {
		return "", err
	}
	e, err := a.ConfigServer().CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: "projects/" + project,
		Exclusion: exclusionFromForm(strings.TrimSpace(values["name"]), values)})
	if err != nil {
		return "", err
	}
	return "projects/" + project + "/exclusions/" + e.GetName(), nil
}

func (p loggingExclusionsProvider) get(ctx context.Context, project, name string) (*loggingpb.LogExclusion, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := ofProject(project, "exclusions", name); err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetExclusion_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: name})
}

func (p loggingExclusionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	e, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Filter", Value: e.GetFilter()}, {Label: "State", Value: enabledWord(e.GetDisabled())}},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Exclusion", Properties: []console.Property{
				{Label: "Resource name", Value: path[0]},
				{Label: "Filter", Value: e.GetFilter()},
				{Label: "Description", Value: orDash(e.GetDescription())},
				{Label: "State", Value: enabledWord(e.GetDisabled())},
				{Label: "Created", Value: loggingTime(e.GetCreateTime())},
				{Label: "Updated", Value: loggingTime(e.GetUpdateTime())},
			}}}, Note: exclusionNote}},
		Edit: &console.EditForm{Label: "Edit exclusion", Fields: exclusionFields(e), Note: "The name is the exclusion's identity and cannot change."},
	}, nil
}

func (p loggingExclusionsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only an exclusion can be edited")
	}
	old, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateExclusion_FullMethodName, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	_, err = a.ConfigServer().UpdateExclusion(ctx, &loggingpb.UpdateExclusionRequest{Name: path[0], Exclusion: exclusionFromForm(old.GetName(), values),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter", "description", "disabled"}}})
	return err
}

func (p loggingExclusionsProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := ofProject(project, "exclusions", name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteExclusion_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.ConfigServer().DeleteExclusion(ctx, &loggingpb.DeleteExclusionRequest{Name: name})
	return err
}

var (
	_ console.Driller = loggingMetricsProvider{}
	_ console.Creator = loggingMetricsProvider{}
	_ console.Editor  = loggingMetricsProvider{}
	_ console.Deleter = loggingMetricsProvider{}
	_ console.Driller = loggingSinksProvider{}
	_ console.Creator = loggingSinksProvider{}
	_ console.Editor  = loggingSinksProvider{}
	_ console.Deleter = loggingSinksProvider{}
	_ console.Driller = loggingExclusionsProvider{}
	_ console.Creator = loggingExclusionsProvider{}
	_ console.Editor  = loggingExclusionsProvider{}
	_ console.Deleter = loggingExclusionsProvider{}
)
