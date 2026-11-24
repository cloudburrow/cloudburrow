package monitoring

import (
	"context"
	"sort"
	"strings"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// parseProject reads projects/P.
func parseProject(field, name string) (string, error) {
	m := projectNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", apierror.InvalidArgument("Field %s had an invalid value of %q: must be of the form projects/[PROJECT_ID_OR_NUMBER]", field, name)
	}
	return m[1], nil
}

// parseDescriptorName reads projects/P/metricDescriptors/TYPE.
func parseDescriptorName(name string) (project, metricType string, err error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		if p, t, ok2 := strings.Cut(rest, "/metricDescriptors/"); ok2 && projectNameRE.MatchString("projects/"+p) && t != "" {
			return p, t, nil
		}
	}
	return "", "", apierror.InvalidArgument("Field name had an invalid value of %q: must be of the form projects/[PROJECT_ID_OR_NUMBER]/metricDescriptors/[METRIC_ID]", name)
}

// lookup finds a descriptor by type: user-defined first, then built-in.
// The caller holds mu.
func (s *Server) lookup(project, metricType string) (*metricpb.MetricDescriptor, bool) {
	if md, ok := s.descriptors[descriptorName(project, metricType)]; ok {
		return md, false
	}
	for _, b := range s.builtins {
		for _, md := range b.Descriptors(project) {
			if md.GetType() == metricType {
				return md, true
			}
		}
	}
	return nil, false
}

// allDescriptors is every descriptor of a project, by type. The caller
// holds mu.
func (s *Server) allDescriptors(project string) []*metricpb.MetricDescriptor {
	var out []*metricpb.MetricDescriptor
	seen := map[string]bool{}
	for _, b := range s.builtins {
		for _, md := range b.Descriptors(project) {
			if !seen[md.GetType()] {
				seen[md.GetType()] = true
				out = append(out, md)
			}
		}
	}
	prefix := "projects/" + project + "/metricDescriptors/"
	for n, md := range s.descriptors {
		if strings.HasPrefix(n, prefix) && !seen[md.GetType()] {
			out = append(out, md)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetType() < out[j].GetType() })
	return out
}

// ListMetricDescriptors lists a project's descriptors that match a filter
// on metric.type (and project).
func (s *Server) ListMetricDescriptors(_ context.Context, req *monitoringpb.ListMetricDescriptorsRequest) (*monitoringpb.ListMetricDescriptorsResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), filterDescriptors)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	all := s.allDescriptors(project)
	s.mu.Unlock()
	byKey := map[string]*metricpb.MetricDescriptor{}
	var keys []string
	for _, md := range all {
		if f.match(subject{project: project, metricType: md.GetType()}) {
			byKey[md.GetType()] = md
			keys = append(keys, md.GetType())
		}
	}
	page, next, err := paging.Page("metricDescriptors|"+req.GetName()+"|"+req.GetFilter(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("Field pageToken had an invalid value: %v", err)
	}
	resp := &monitoringpb.ListMetricDescriptorsResponse{NextPageToken: next}
	for _, k := range page {
		resp.MetricDescriptors = append(resp.MetricDescriptors, clone(byKey[k]))
	}
	return resp, nil
}

// GetMetricDescriptor reads one descriptor.
func (s *Server) GetMetricDescriptor(_ context.Context, req *monitoringpb.GetMetricDescriptorRequest) (*metricpb.MetricDescriptor, error) {
	project, t, err := parseDescriptorName(req.GetName())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	md, _ := s.lookup(project, t)
	if md == nil {
		return nil, apierror.NotFound("Could not find descriptor for metric '%s'.", t)
	}
	return clone(md), nil
}

// checkDescriptor checks a user-defined descriptor as Google does.
func checkDescriptor(md *metricpb.MetricDescriptor) error {
	t := md.GetType()
	if t == "" || !metricTypeRE.MatchString(t) {
		return apierror.InvalidArgument("The metric type must be a URL-formatted string with a domain and non-empty path")
	}
	if !isUserDefined(t) {
		domain, _, _ := strings.Cut(t, "/")
		return apierror.InvalidArgument("user defined metrics are not supported on the metric domain %q", domain)
	}
	switch md.GetMetricKind() {
	case metricpb.MetricDescriptor_GAUGE, metricpb.MetricDescriptor_CUMULATIVE:
	case metricpb.MetricDescriptor_DELTA:
		return apierror.InvalidArgument("Field metricDescriptor.metricKind had an invalid value of \"DELTA\": DELTA metric kind is not supported for user-defined metric %s", t)
	default:
		return apierror.InvalidArgument("Field metricDescriptor.metricKind had an invalid value: the metric kind must be GAUGE or CUMULATIVE")
	}
	switch md.GetValueType() {
	case metricpb.MetricDescriptor_INT64, metricpb.MetricDescriptor_DOUBLE, metricpb.MetricDescriptor_DISTRIBUTION:
	case metricpb.MetricDescriptor_BOOL:
		if md.GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
			return apierror.InvalidArgument("Field metricDescriptor.valueType had an invalid value of \"BOOL\": a BOOL metric must be a GAUGE")
		}
	case metricpb.MetricDescriptor_STRING:
		return apierror.InvalidArgument("Field metricDescriptor.valueType had an invalid value of \"STRING\": String values in custom metrics are not supported")
	default:
		return apierror.InvalidArgument("Field metricDescriptor.valueType had an invalid value: the value type must be BOOL, INT64, DOUBLE or DISTRIBUTION")
	}
	if len(md.GetLabels()) > MaxLabels {
		return apierror.InvalidArgument("Field metric.labels had an invalid value: The metric %s has more than %d labels", t, MaxLabels)
	}
	seen := map[string]bool{}
	for _, l := range md.GetLabels() {
		if err := checkLabelKey(l.GetKey()); err != nil {
			return err
		}
		if seen[l.GetKey()] {
			return apierror.InvalidArgument("Field metricDescriptor.labels had an invalid value of %q: duplicate label key", l.GetKey())
		}
		seen[l.GetKey()] = true
	}
	return nil
}

func checkLabelKey(k string) error {
	if len(k) > MaxLabelKey || !labelKeyRE.MatchString(k) {
		return apierror.InvalidArgument("Field metric.labels had an invalid value of %q: Label key contains invalid characters", k)
	}
	return nil
}

// CreateMetricDescriptor creates a user-defined descriptor. An existing one
// is updated, except that its labels are never removed; its kind and value
// type cannot change.
func (s *Server) CreateMetricDescriptor(_ context.Context, req *monitoringpb.CreateMetricDescriptorRequest) (*metricpb.MetricDescriptor, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	in := req.GetMetricDescriptor()
	if in == nil {
		return nil, apierror.InvalidArgument("Request was missing field metricDescriptor")
	}
	md := clone(in)
	if md.GetMetricKind() == metricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED {
		md.MetricKind = metricpb.MetricDescriptor_GAUGE
	}
	if err := checkDescriptor(md); err != nil {
		return nil, err
	}
	md.Name = descriptorName(project, md.GetType())
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.descriptors[md.Name]; ok {
		if old.GetMetricKind() != md.GetMetricKind() || old.GetValueType() != md.GetValueType() {
			return nil, apierror.InvalidArgument("Metric descriptor %s already exists with metric kind %s and value type %s; delete it to change them",
				md.GetType(), old.GetMetricKind(), old.GetValueType())
		}
		md.Labels = mergeLabels(old.GetLabels(), md.GetLabels())
		if len(md.Labels) > MaxLabels {
			return nil, apierror.InvalidArgument("Field metric.labels had an invalid value: The metric %s has more than %d labels", md.GetType(), MaxLabels)
		}
	} else if n := s.countDescriptors(project); n >= MaxDescriptors {
		return nil, &apierror.Error{Code: codes.ResourceExhausted, Message: "Your Metric Descriptors quota has been exhausted"}
	}
	s.descriptors[md.Name] = md
	if err := s.save(); err != nil {
		return nil, apierror.Internal(err, "save Cloud Monitoring state")
	}
	return clone(md), nil
}

// mergeLabels is old's labels with new's added; a label in both keeps the
// new definition.
func mergeLabels(old, add []*labelpb.LabelDescriptor) []*labelpb.LabelDescriptor {
	out := []*labelpb.LabelDescriptor{}
	idx := map[string]int{}
	for _, l := range old {
		idx[l.GetKey()] = len(out)
		out = append(out, clone(l))
	}
	for _, l := range add {
		if i, ok := idx[l.GetKey()]; ok {
			out[i] = clone(l)
			continue
		}
		idx[l.GetKey()] = len(out)
		out = append(out, clone(l))
	}
	return out
}

func (s *Server) countDescriptors(project string) int {
	n := 0
	for k := range s.descriptors {
		// Agent metrics' descriptors are Google's, not the project's quota.
		if strings.HasPrefix(k, "projects/"+project+"/") && !isAgentMetric(s.descriptors[k].GetType()) {
			n++
		}
	}
	return n
}

// DeleteMetricDescriptor deletes a user-defined descriptor. Its series stop
// being readable, as on Google, where the data is made inaccessible.
func (s *Server) DeleteMetricDescriptor(_ context.Context, req *monitoringpb.DeleteMetricDescriptorRequest) (*emptypb.Empty, error) {
	project, t, err := parseDescriptorName(req.GetName())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if isAgentMetric(t) {
		return nil, apierror.InvalidArgument("Built-in metric descriptor %s cannot be deleted; only user-defined metrics can be", t)
	}
	if _, ok := s.descriptors[descriptorName(project, t)]; !ok {
		if md, builtin := s.lookup(project, t); md != nil && builtin {
			return nil, apierror.InvalidArgument("Built-in metric descriptor %s cannot be deleted; only user-defined metrics can be", t)
		}
		return nil, apierror.NotFound("Could not find descriptor for metric '%s'.", t)
	}
	delete(s.descriptors, descriptorName(project, t))
	for k, sr := range s.series[project] {
		if sr.TS.GetMetric().GetType() == t {
			delete(s.series[project], k)
		}
	}
	if err := s.save(); err != nil {
		return nil, apierror.Internal(err, "save Cloud Monitoring state")
	}
	return &emptypb.Empty{}, nil
}

// ListMonitoredResourceDescriptors lists the monitored resource types.
func (s *Server) ListMonitoredResourceDescriptors(_ context.Context, req *monitoringpb.ListMonitoredResourceDescriptorsRequest) (*monitoringpb.ListMonitoredResourceDescriptorsResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), filterResources)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*monitoredres.MonitoredResourceDescriptor{}
	var keys []string
	for _, d := range resourceDescriptors {
		if f.match(subject{project: project, resourceType: d.GetType()}) {
			byKey[d.GetType()] = d
			keys = append(keys, d.GetType())
		}
	}
	page, next, err := paging.Page("monitoredResourceDescriptors|"+req.GetName()+"|"+req.GetFilter(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("Field pageToken had an invalid value: %v", err)
	}
	resp := &monitoringpb.ListMonitoredResourceDescriptorsResponse{NextPageToken: next}
	for _, k := range page {
		d := clone(byKey[k])
		d.Name = "projects/" + project + "/monitoredResourceDescriptors/" + d.GetType()
		resp.ResourceDescriptors = append(resp.ResourceDescriptors, d)
	}
	return resp, nil
}

// GetMonitoredResourceDescriptor reads one monitored resource type.
func (s *Server) GetMonitoredResourceDescriptor(_ context.Context, req *monitoringpb.GetMonitoredResourceDescriptorRequest) (*monitoredres.MonitoredResourceDescriptor, error) {
	p, t, ok := strings.Cut(strings.TrimPrefix(req.GetName(), "projects/"), "/monitoredResourceDescriptors/")
	if !strings.HasPrefix(req.GetName(), "projects/") || !ok || !projectNameRE.MatchString("projects/"+p) || t == "" {
		return nil, apierror.InvalidArgument("Field name had an invalid value of %q: must be of the form projects/[PROJECT_ID_OR_NUMBER]/monitoredResourceDescriptors/[RESOURCE_TYPE]", req.GetName())
	}
	d := resourceDescriptor(t)
	if d == nil {
		return nil, apierror.NotFound("The monitored resource descriptor %s does not exist.", t)
	}
	d = clone(d)
	d.Name = req.GetName()
	return d, nil
}
