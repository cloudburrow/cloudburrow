package monitoring

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/egress"
)

// UptimeCheckService (#1964): checks are kept in the instance's state and
// run from one local checker against local hosts only (IsLocalHost); a
// check of any other host fails without being sent. Each run is a point of
// monitoring.googleapis.com/uptime_check/check_passed and
// /request_latency on uptime_url, as Google's checkers write them
// (docs.cloud.google.com/monitoring/uptime-checks, read 2026-11-19).

const (
	// MaxUptimeChecks is "Uptime checks per metrics scope: 100".
	MaxUptimeChecks = 100
	// checkerLocation is the local checker's checker_location label.
	checkerLocation = "local"
	uptimePassed    = "monitoring.googleapis.com/uptime_check/check_passed"
	uptimeLatency   = "monitoring.googleapis.com/uptime_check/request_latency"
	// uptimeRing bounds each check's kept results.
	uptimeRing = 10000
)

var uptimePeriods = map[time.Duration]bool{time.Minute: true, 5 * time.Minute: true, 10 * time.Minute: true, 15 * time.Minute: true}

// UptimeResult is one run of a check.
type UptimeResult struct {
	Time    time.Time     `json:"time"`
	Passed  bool          `json:"passed"`
	Latency time.Duration `json:"latency"`
	Status  int           `json:"status,omitempty"`
	Error   string        `json:"error,omitempty"`
}

type uptimeState struct {
	mu      sync.Mutex
	last    map[string]time.Time
	results map[string][]UptimeResult
	// targets is each check's uptime_url resource, for its series.
	targets map[string]*monitoredres.MonitoredResource
}

func (s *Server) ListUptimeCheckConfigs(_ context.Context, req *monitoringpb.ListUptimeCheckConfigsRequest) (*monitoringpb.ListUptimeCheckConfigsResponse, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	terms, err := parseSimpleFilter("filter", req.GetFilter())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*monitoringpb.UptimeCheckConfig
	for _, m := range s.under(kindUptime, "projects/"+project+"/") {
		if matchSimple(terms, m) {
			items = append(items, maskedUptime(m.(*monitoringpb.UptimeCheckConfig)))
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("uptime|"+req.GetParent()+"|"+req.GetFilter(), items, (*monitoringpb.UptimeCheckConfig).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListUptimeCheckConfigsResponse{UptimeCheckConfigs: page, NextPageToken: next, TotalSize: int32(len(items))}, nil
}

// maskedUptime hides a check's basic auth password, as Google does.
func maskedUptime(c *monitoringpb.UptimeCheckConfig) *monitoringpb.UptimeCheckConfig {
	out := clone(c)
	if a := out.GetHttpCheck().GetAuthInfo(); a != nil && a.Password != "" {
		a.Password = "******"
	}
	return out
}

func (s *Server) GetUptimeCheckConfig(_ context.Context, req *monitoringpb.GetUptimeCheckConfigRequest) (*monitoringpb.UptimeCheckConfig, error) {
	if _, err := childName("name", req.GetName(), "uptimeCheckConfigs"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.get(kindUptime, req.GetName()).(*monitoringpb.UptimeCheckConfig)
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return maskedUptime(c), nil
}

func (s *Server) CreateUptimeCheckConfig(_ context.Context, req *monitoringpb.CreateUptimeCheckConfigRequest) (*monitoringpb.UptimeCheckConfig, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetUptimeCheckConfig() == nil {
		return nil, apierror.InvalidArgument("Request was missing field uptimeCheckConfig")
	}
	c := clone(req.GetUptimeCheckConfig())
	if err := checkUptime(project, c); err != nil {
		return nil, err
	}
	c.Name = "projects/" + project + "/uptimeCheckConfigs/" + newSlug(c.GetDisplayName())
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.under(kindUptime, "projects/"+project+"/")) >= MaxUptimeChecks {
		return nil, apierror.FailedPrecondition("The project %s has reached its limit of %d uptime checks", project, MaxUptimeChecks)
	}
	if g := c.GetResourceGroup(); g != nil && s.get(kindGroups, "projects/"+project+"/groups/"+g.GetGroupId()) == nil {
		return nil, apierror.InvalidArgument("Field uptimeCheckConfig.resourceGroup.groupId had an invalid value of %q: no such group", g.GetGroupId())
	}
	s.put(kindUptime, c.Name, c)
	if err := s.save(); err != nil {
		return nil, err
	}
	return maskedUptime(c), nil
}

func checkUptime(project string, c *monitoringpb.UptimeCheckConfig) error {
	if strings.TrimSpace(c.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field uptimeCheckConfig.displayName is required")
	}
	switch {
	case c.GetMonitoredResource() != nil:
		r := c.GetMonitoredResource()
		if r.GetType() != "uptime_url" {
			return apierror.Unimplemented("Uptime checks of monitored resource type %q are not supported; use uptime_url", r.GetType())
		}
		if r.GetLabels()["host"] == "" {
			return apierror.InvalidArgument("Field uptimeCheckConfig.monitoredResource.labels.host is required")
		}
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
		if r.Labels["project_id"] == "" {
			r.Labels["project_id"] = project
		}
	case c.GetResourceGroup() != nil:
		if c.GetResourceGroup().GetGroupId() == "" {
			return apierror.InvalidArgument("Field uptimeCheckConfig.resourceGroup.groupId is required")
		}
	case c.GetSyntheticMonitor() != nil:
		return apierror.Unimplemented("Synthetic monitors are not supported")
	default:
		return apierror.InvalidArgument("Field uptimeCheckConfig.resource is required: set monitoredResource or resourceGroup")
	}
	if c.GetHttpCheck() == nil && c.GetTcpCheck() == nil {
		return apierror.InvalidArgument("Field uptimeCheckConfig.checkRequestType is required: set httpCheck or tcpCheck")
	}
	if c.GetIsInternal() || len(c.GetInternalCheckers()) > 0 || c.GetCheckerType() == monitoringpb.UptimeCheckConfig_VPC_CHECKERS {
		return apierror.Unimplemented("Private (VPC) uptime checks are not supported")
	}
	if c.Period == nil {
		c.Period = durationpb.New(time.Minute)
	}
	if !uptimePeriods[c.GetPeriod().AsDuration()] {
		return apierror.InvalidArgument("Field uptimeCheckConfig.period had an invalid value of %q: the period must be 60s, 300s, 600s or 900s", c.GetPeriod().AsDuration())
	}
	if c.Timeout == nil {
		return apierror.InvalidArgument("Field uptimeCheckConfig.timeout is required")
	}
	if d := c.GetTimeout().AsDuration(); d < time.Second || d > time.Minute {
		return apierror.InvalidArgument("Field uptimeCheckConfig.timeout had an invalid value of %q: the timeout must be from 1 to 60 seconds", d)
	}
	if n := len(c.GetSelectedRegions()); n > 0 && n < 3 {
		return apierror.InvalidArgument("Field uptimeCheckConfig.selectedRegions had an invalid value: select at least 3 regions, or none for all")
	}
	if h := c.GetHttpCheck(); h != nil {
		if h.GetPort() == 0 {
			h.Port = 80
			if h.GetUseSsl() {
				h.Port = 443
			}
		}
		if h.GetPath() == "" {
			h.Path = "/"
		}
		if !strings.HasPrefix(h.GetPath(), "/") {
			h.Path = "/" + h.GetPath()
		}
		if h.GetRequestMethod() == monitoringpb.UptimeCheckConfig_HttpCheck_METHOD_UNSPECIFIED {
			h.RequestMethod = monitoringpb.UptimeCheckConfig_HttpCheck_GET
		}
		if len(h.GetBody()) > 0 && h.GetRequestMethod() != monitoringpb.UptimeCheckConfig_HttpCheck_POST {
			return apierror.InvalidArgument("Field uptimeCheckConfig.httpCheck.body had an invalid value: a body is only sent with POST")
		}
		if h.GetServiceAgentAuthentication() != nil {
			return apierror.Unimplemented("Service agent authentication for uptime checks is not supported")
		}
	}
	if t := c.GetTcpCheck(); t != nil && (t.GetPort() <= 0 || t.GetPort() > 65535) {
		return apierror.InvalidArgument("Field uptimeCheckConfig.tcpCheck.port is required")
	}
	if len(c.GetContentMatchers()) > 1 {
		return apierror.InvalidArgument("Field uptimeCheckConfig.contentMatchers had an invalid value: at most one content matcher")
	}
	for _, m := range c.GetContentMatchers() {
		switch m.GetMatcher() {
		case monitoringpb.UptimeCheckConfig_ContentMatcher_MATCHES_REGEX, monitoringpb.UptimeCheckConfig_ContentMatcher_NOT_MATCHES_REGEX:
			if _, err := regexp.Compile(m.GetContent()); err != nil {
				return apierror.InvalidArgument("Field uptimeCheckConfig.contentMatchers[0].content had an invalid value: %v", err)
			}
		case monitoringpb.UptimeCheckConfig_ContentMatcher_MATCHES_JSON_PATH, monitoringpb.UptimeCheckConfig_ContentMatcher_NOT_MATCHES_JSON_PATH:
			return apierror.Unimplemented("JSONPath content matchers are not supported")
		}
	}
	return checkUserLabels("uptimeCheckConfig.userLabels", c.GetUserLabels())
}

func (s *Server) UpdateUptimeCheckConfig(_ context.Context, req *monitoringpb.UpdateUptimeCheckConfigRequest) (*monitoringpb.UptimeCheckConfig, error) {
	in := req.GetUptimeCheckConfig()
	project, err := childName("uptimeCheckConfig.name", in.GetName(), "uptimeCheckConfigs")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindUptime, in.GetName()).(*monitoringpb.UptimeCheckConfig)
	if old == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	next := clone(in)
	if len(req.GetUpdateMask().GetPaths()) > 0 {
		next = clone(old)
		mask, err := overlayMask(next, in, req.GetUpdateMask())
		if err != nil {
			return nil, err
		}
		if err := applyMask(next, in, mask); err != nil {
			return nil, err
		}
	}
	if next.GetResource() == nil {
		next.Resource = old.Resource
	}
	if next.GetHttpCheck().GetAuthInfo().GetPassword() == "******" {
		next.GetHttpCheck().GetAuthInfo().Password = old.GetHttpCheck().GetAuthInfo().GetPassword()
	}
	if err := checkUptime(project, next); err != nil {
		return nil, err
	}
	if fmt.Sprint(next.GetResource()) != fmt.Sprint(old.GetResource()) {
		return nil, apierror.InvalidArgument("Field uptimeCheckConfig.resource had an invalid value: an uptime check's resource cannot change")
	}
	if (next.GetHttpCheck() == nil) != (old.GetHttpCheck() == nil) {
		return nil, apierror.InvalidArgument("Field uptimeCheckConfig.checkRequestType had an invalid value: an uptime check's protocol cannot change")
	}
	next.Name = old.GetName()
	s.put(kindUptime, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return maskedUptime(next), nil
}

func (s *Server) DeleteUptimeCheckConfig(_ context.Context, req *monitoringpb.DeleteUptimeCheckConfigRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "uptimeCheckConfigs"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindUptime, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	id := req.GetName()[strings.LastIndex(req.GetName(), "/")+1:]
	for _, m := range s.under(kindPolicies, "projects/"+projectOfName(req.GetName())+"/") {
		for _, c := range m.(*monitoringpb.AlertPolicy).GetConditions() {
			if f := c.GetConditionThreshold().GetFilter() + c.GetConditionAbsent().GetFilter(); strings.Contains(f, `"`+id+`"`) {
				return nil, apierror.FailedPrecondition("Uptime check %s is used by alerting policy %s; delete or change the policy first", id, m.(*monitoringpb.AlertPolicy).GetName())
			}
		}
	}
	delete(s.objects[kindUptime], req.GetName())
	return &emptypb.Empty{}, s.save()
}

// ListUptimeCheckIps is the one local checker's address.
func (s *Server) ListUptimeCheckIps(context.Context, *monitoringpb.ListUptimeCheckIpsRequest) (*monitoringpb.ListUptimeCheckIpsResponse, error) {
	return &monitoringpb.ListUptimeCheckIpsResponse{UptimeCheckIps: []*monitoringpb.UptimeCheckIp{
		{Region: monitoringpb.UptimeCheckRegion_REGION_UNSPECIFIED, Location: checkerLocation, IpAddress: "127.0.0.1"}}}, nil
}

// UptimeResults are a check's runs, newest first.
func (s *Server) UptimeResults(name string) []UptimeResult {
	s.uptime.mu.Lock()
	defer s.uptime.mu.Unlock()
	rs := s.uptime.results[name]
	out := make([]UptimeResult, len(rs))
	for i := range rs {
		out[len(rs)-1-i] = rs[i]
	}
	return out
}

// RunUptimeChecks runs each check whose period has elapsed.
func (s *Server) RunUptimeChecks(ctx context.Context) {
	now := s.now()
	// The group checks' projects' products' resources, read before mu.
	s.mu.Lock()
	groupProjects := map[string]bool{}
	for _, m := range s.under(kindUptime, "") {
		if c := m.(*monitoringpb.UptimeCheckConfig); c.GetResourceGroup() != nil {
			groupProjects[projectOfName(c.GetName())] = true
		}
	}
	s.mu.Unlock()
	invs := map[string]map[string]InventoryResource{}
	for p := range groupProjects {
		invs[p] = s.inventory(ctx, p)
	}
	s.mu.Lock()
	var due []*monitoringpb.UptimeCheckConfig
	targets := map[string][]*monitoredres.MonitoredResource{}
	for _, m := range s.under(kindUptime, "") {
		c := m.(*monitoringpb.UptimeCheckConfig)
		s.uptime.mu.Lock()
		last := s.uptime.last[c.GetName()]
		s.uptime.mu.Unlock()
		if UptimeDisabled(c) || (!last.IsZero() && now.Sub(last) < c.GetPeriod().AsDuration()) {
			continue
		}
		due = append(due, clone(c))
		if c.GetResourceGroup() != nil {
			targets[c.GetName()] = s.groupMembersLocked(projectOfName(c.GetName()), c.GetResourceGroup().GetGroupId(), now, invs[projectOfName(c.GetName())])
		}
	}
	s.mu.Unlock()
	for _, c := range due {
		s.uptime.mu.Lock()
		s.uptime.last[c.GetName()] = now
		s.uptime.mu.Unlock()
		hosts := []*monitoredres.MonitoredResource{c.GetMonitoredResource()}
		if c.GetResourceGroup() != nil {
			hosts = nil
			for _, r := range targets[c.GetName()] {
				h := r.GetLabels()["host"]
				if h == "" {
					h = r.GetLabels()["instance_id"]
				}
				if h != "" {
					hosts = append(hosts, &monitoredres.MonitoredResource{Type: "uptime_url", Labels: map[string]string{"host": h, "project_id": projectOfName(c.GetName())}})
				}
			}
		}
		for _, h := range hosts {
			r := runCheck(ctx, c, h.GetLabels()["host"])
			r.Time = now
			s.logUptimeFailure(ctx, c, h.GetLabels()["host"], r)
			s.uptime.mu.Lock()
			key := c.GetName() + "|" + h.GetLabels()["host"]
			s.uptime.results[key] = append(s.uptime.results[key], r)
			if len(s.uptime.results[key]) > uptimeRing {
				s.uptime.results[key] = s.uptime.results[key][1:]
			}
			s.uptime.results[c.GetName()] = append(s.uptime.results[c.GetName()], r)
			if len(s.uptime.results[c.GetName()]) > uptimeRing {
				s.uptime.results[c.GetName()] = s.uptime.results[c.GetName()][1:]
			}
			s.uptime.targets[key] = &monitoredres.MonitoredResource{Type: "uptime_url", Labels: map[string]string{
				"host": h.GetLabels()["host"], "project_id": projectOfName(c.GetName())}}
			s.uptime.mu.Unlock()
		}
	}
}

func runCheck(ctx context.Context, c *monitoringpb.UptimeCheckConfig, host string) UptimeResult {
	if !IsLocalHost(host) {
		return UptimeResult{Error: "not checked: " + host + " is not a local host, and CloudBurrow checks only hosts on the machine"}
	}
	if egress.GoogleHost(host) {
		// The metadata server's link-local address is Google's (#2015).
		return UptimeResult{Error: "not checked: " + host + " is Google's, and CloudBurrow sends no request to Google"}
	}
	timeout := c.GetTimeout().AsDuration()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	if t := c.GetTcpCheck(); t != nil {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(t.GetPort()))))
		if err != nil {
			return UptimeResult{Latency: time.Since(start), Error: err.Error()}
		}
		_ = conn.Close()
		return UptimeResult{Passed: true, Latency: time.Since(start)}
	}
	h := c.GetHttpCheck()
	scheme := "http"
	if h.GetUseSsl() {
		scheme = "https"
	}
	u := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(int(h.GetPort()))) + h.GetPath()
	method := http.MethodGet
	var body io.Reader
	if h.GetRequestMethod() == monitoringpb.UptimeCheckConfig_HttpCheck_POST {
		method, body = http.MethodPost, strings.NewReader(string(h.GetBody()))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return UptimeResult{Error: err.Error()}
	}
	req.Header.Set("User-Agent", "GoogleStackdriverMonitoring-UptimeChecks(https://cloud.google.com/monitoring)")
	for k, v := range h.GetHeaders() {
		req.Header.Set(k, v)
	}
	switch h.GetContentType() {
	case monitoringpb.UptimeCheckConfig_HttpCheck_URL_ENCODED:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case monitoringpb.UptimeCheckConfig_HttpCheck_USER_PROVIDED:
		req.Header.Set("Content-Type", h.GetCustomContentType())
	}
	if a := h.GetAuthInfo(); a != nil {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.GetUsername()+":"+a.GetPassword())))
	}
	client := egress.Client(&http.Client{
		// Redirects are not followed, as Google's checkers do not follow them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     uptimeTransport(h.GetValidateSsl()),
	})
	resp, err := client.Do(req)
	if err != nil {
		return UptimeResult{Latency: time.Since(start), Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	// Google reads the first 1 MB of the response for content matching.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r := UptimeResult{Latency: time.Since(start), Status: resp.StatusCode}
	if !statusAccepted(h.GetAcceptedResponseStatusCodes(), resp.StatusCode) {
		r.Error = "the response status " + resp.Status + " is not accepted"
		return r
	}
	for _, m := range c.GetContentMatchers() {
		if !contentMatches(m, string(b)) {
			r.Error = "the response content does not match"
			return r
		}
	}
	r.Passed = true
	return r
}

func statusAccepted(codes []*monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode, status int) bool {
	if len(codes) == 0 {
		return status/100 == 2
	}
	for _, c := range codes {
		if v := c.GetStatusValue(); v != 0 && int(v) == status {
			return true
		}
		switch c.GetStatusClass() {
		case monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_STATUS_CLASS_ANY:
			return true
		case monitoringpb.UptimeCheckConfig_HttpCheck_ResponseStatusCode_STATUS_CLASS_UNSPECIFIED:
		default:
			if int(c.GetStatusClass()) == status/100*100 {
				return true
			}
		}
	}
	return false
}

func contentMatches(m *monitoringpb.UptimeCheckConfig_ContentMatcher, body string) bool {
	switch m.GetMatcher() {
	case monitoringpb.UptimeCheckConfig_ContentMatcher_NOT_CONTAINS_STRING:
		return !strings.Contains(body, m.GetContent())
	case monitoringpb.UptimeCheckConfig_ContentMatcher_MATCHES_REGEX:
		ok, _ := regexp.MatchString(m.GetContent(), body)
		return ok
	case monitoringpb.UptimeCheckConfig_ContentMatcher_NOT_MATCHES_REGEX:
		ok, _ := regexp.MatchString(m.GetContent(), body)
		return !ok
	}
	return strings.Contains(body, m.GetContent())
}

// uptimeBuiltin serves the uptime check metrics from the runs kept.
type uptimeBuiltin struct{ s *Server }

func (uptimeBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	labels := []*labelpb.LabelDescriptor{
		{Key: "check_id", ValueType: labelpb.LabelDescriptor_STRING, Description: "The ID of the uptime check."},
		{Key: "checker_location", ValueType: labelpb.LabelDescriptor_STRING, Description: "The location of the checker."},
		{Key: "checked_resource_id", ValueType: labelpb.LabelDescriptor_STRING, Description: "The ID of the checked resource."},
	}
	return []*metricpb.MetricDescriptor{
		{Name: descriptorName(project, uptimePassed), Type: uptimePassed, MetricKind: metricpb.MetricDescriptor_GAUGE, ValueType: metricpb.MetricDescriptor_BOOL,
			Unit: "1", DisplayName: "Check passed", Description: "Whether the resource passed the check.", Labels: labels, MonitoredResourceTypes: []string{"uptime_url"}},
		{Name: descriptorName(project, uptimeLatency), Type: uptimeLatency, MetricKind: metricpb.MetricDescriptor_GAUGE, ValueType: metricpb.MetricDescriptor_DOUBLE,
			Unit: "ms", DisplayName: "Request latency", Description: "The request latency of the check.", Labels: labels, MonitoredResourceTypes: []string{"uptime_url"}},
	}
}

func (b uptimeBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if metricType != uptimePassed && metricType != uptimeLatency {
		return nil, nil
	}
	u := &b.s.uptime
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for key, res := range u.targets {
		name, host, _ := strings.Cut(key, "|")
		if projectOfName(name) != project {
			continue
		}
		ts := &monitoringpb.TimeSeries{
			Metric: &metricpb.Metric{Type: metricType, Labels: map[string]string{"check_id": name[strings.LastIndex(name, "/")+1:],
				"checker_location": checkerLocation, "checked_resource_id": host}},
			Resource: clone(res), MetricKind: metricpb.MetricDescriptor_GAUGE, ValueType: metricpb.MetricDescriptor_BOOL, Unit: "1"}
		if metricType == uptimeLatency {
			ts.ValueType, ts.Unit = metricpb.MetricDescriptor_DOUBLE, "ms"
		}
		for _, r := range u.results[key] {
			if r.Time.Before(start) || r.Time.After(end) {
				continue
			}
			v := &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_BoolValue{BoolValue: r.Passed}}
			if metricType == uptimeLatency {
				v = typed(tDouble, float64(r.Latency.Microseconds())/1000)
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(r.Time), EndTime: timestamppb.New(r.Time)}, Value: v})
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	return out, nil
}

// uptimeTransport is what an uptime check dials with: SSL validated unless
// validate_ssl is off, as asked. Tests replace it with a loopback-only sink.
var uptimeTransport = func(validate bool) http.RoundTripper {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: !validate}} //nolint:gosec // validate_ssl off, as asked
}
