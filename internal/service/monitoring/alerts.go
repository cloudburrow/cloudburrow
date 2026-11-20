package monitoring

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AlertPolicyService (#1964): policies are kept in the instance's state
// and evaluated by evaluate.go over the served time series and the local
// Cloud Logging. Limits and rules from
// docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.alertPolicies,
// docs.cloud.google.com/monitoring/quotas and alert.proto (read 2026-11-19).
const (
	// MaxConditions is "Conditions per alerting policy: 6".
	MaxConditions = 6
	// MaxPolicies is "Alerting policies per metrics scope: 2,000".
	MaxPolicies = 2000
	// MaxPolicyChannels is "Notification channels per alerting policy: 16".
	MaxPolicyChannels = 16
	// MaxFilter is a condition filter's length.
	MaxFilter = 2048
)

var (
	userLabelKeyRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	userLabelValueRE = regexp.MustCompile(`^[a-z0-9_-]*$`)
)

func (s *Server) ListAlertPolicies(_ context.Context, req *monitoringpb.ListAlertPoliciesRequest) (*monitoringpb.ListAlertPoliciesResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	terms, err := parseSimpleFilter("filter", req.GetFilter())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*monitoringpb.AlertPolicy
	for _, m := range s.under(kindPolicies, "projects/"+project+"/") {
		if matchSimple(terms, m) {
			items = append(items, m.(*monitoringpb.AlertPolicy))
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("alertPolicies|"+req.GetName()+"|"+req.GetFilter(), items, (*monitoringpb.AlertPolicy).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListAlertPoliciesResponse{AlertPolicies: page, NextPageToken: next, TotalSize: int32(len(items))}, nil
}

func (s *Server) GetAlertPolicy(_ context.Context, req *monitoringpb.GetAlertPolicyRequest) (*monitoringpb.AlertPolicy, error) {
	if _, err := childName("name", req.GetName(), "alertPolicies"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.get(kindPolicies, req.GetName()).(*monitoringpb.AlertPolicy)
	if p == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(p), nil
}

func (s *Server) CreateAlertPolicy(_ context.Context, req *monitoringpb.CreateAlertPolicyRequest) (*monitoringpb.AlertPolicy, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetAlertPolicy() == nil {
		return nil, apierror.InvalidArgument("Request was missing field alertPolicy")
	}
	p := clone(req.GetAlertPolicy())
	p.Name = "projects/" + project + "/alertPolicies/" + newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.under(kindPolicies, "projects/"+project+"/")) >= MaxPolicies {
		return nil, apierror.FailedPrecondition("The project %s has reached its limit of %d alerting policies", project, MaxPolicies)
	}
	if err := s.checkPolicy(project, p, nil); err != nil {
		return nil, err
	}
	p.CreationRecord, p.MutationRecord = s.record(), s.record()
	s.put(kindPolicies, p.Name, p)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(p), nil
}

func (s *Server) UpdateAlertPolicy(_ context.Context, req *monitoringpb.UpdateAlertPolicyRequest) (*monitoringpb.AlertPolicy, error) {
	in := req.GetAlertPolicy()
	project, err := childName("alertPolicy.name", in.GetName(), "alertPolicies")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindPolicies, in.GetName()).(*monitoringpb.AlertPolicy)
	if old == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	next := clone(in)
	if len(req.GetUpdateMask().GetPaths()) > 0 {
		next = clone(old)
		if err := applyMask(next, in, req.GetUpdateMask()); err != nil {
			return nil, err
		}
	}
	next.Name, next.CreationRecord = old.GetName(), old.GetCreationRecord()
	if err := s.checkPolicy(project, next, old); err != nil {
		return nil, err
	}
	next.MutationRecord = s.record()
	s.put(kindPolicies, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(next), nil
}

func (s *Server) DeleteAlertPolicy(_ context.Context, req *monitoringpb.DeleteAlertPolicyRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "alertPolicies"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindPolicies, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(s.objects[kindPolicies], req.GetName())
	// Its open incidents close with it, as Google closes them.
	for _, in := range s.incidents {
		if in.Policy == req.GetName() && in.State == IncidentOpen {
			in.State, in.Ended = IncidentClosed, s.now()
			in.Summary += " (the policy was deleted)"
		}
	}
	return &emptypb.Empty{}, s.save()
}

// checkPolicy validates a policy and names its conditions; the caller
// holds mu.
func (s *Server) checkPolicy(project string, p, old *monitoringpb.AlertPolicy) error {
	if strings.TrimSpace(p.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field alertPolicy.displayName is required")
	}
	if len(p.GetDisplayName()) > 512 {
		return apierror.InvalidArgument("Field alertPolicy.displayName had an invalid value: at most 512 characters")
	}
	n := len(p.GetConditions())
	if n == 0 {
		return apierror.InvalidArgument("Field alertPolicy.conditions had an invalid value: an alerting policy must have at least one condition")
	}
	if n > MaxConditions {
		return apierror.InvalidArgument("Field alertPolicy.conditions had an invalid value: an alerting policy can have at most %d conditions", MaxConditions)
	}
	if n > 1 && p.GetCombiner() == monitoringpb.AlertPolicy_COMBINE_UNSPECIFIED {
		return apierror.InvalidArgument("Field alertPolicy.combiner is required when an alerting policy has more than one condition")
	}
	if err := checkUserLabels("alertPolicy.userLabels", p.GetUserLabels()); err != nil {
		return err
	}
	if p.Enabled == nil {
		p.Enabled = wrapperspb.Bool(true)
	}
	if len(p.GetNotificationChannels()) > MaxPolicyChannels {
		return apierror.InvalidArgument("Field alertPolicy.notificationChannels had an invalid value: at most %d notification channels", MaxPolicyChannels)
	}
	for i, c := range p.GetNotificationChannels() {
		if projectOfName(c) != project || s.get(kindChannels, c) == nil {
			return apierror.InvalidArgument("Field alertPolicy.notificationChannels[%d] had an invalid value of %q: the notification channel does not exist in project %s", i, c, project)
		}
	}
	if d := p.GetDocumentation(); d != nil {
		if len(d.GetContent()) > 10240 {
			return apierror.InvalidArgument("Field alertPolicy.documentation.content had an invalid value: at most 10,240 bytes")
		}
		if d.GetMimeType() == "" && d.GetContent() != "" {
			d.MimeType = "text/markdown"
		}
		if d.GetMimeType() != "" && d.GetMimeType() != "text/markdown" {
			return apierror.InvalidArgument("Field alertPolicy.documentation.mimeType had an invalid value of %q: only text/markdown is supported", d.GetMimeType())
		}
	}
	oldNames := map[string]bool{}
	for _, c := range old.GetConditions() {
		oldNames[c.GetName()] = true
	}
	hasLog := false
	for i, c := range p.GetConditions() {
		if err := checkCondition(i, c); err != nil {
			return err
		}
		if pq := c.GetConditionPrometheusQueryLanguage(); pq != nil {
			if err := s.checkPromMetrics(project, "alertPolicy.conditions["+itoa(i)+"].conditionPrometheusQueryLanguage.query", pq); err != nil {
				return err
			}
		}
		if c.GetConditionMatchedLog() != nil {
			hasLog = true
		}
		if c.GetName() == "" || !oldNames[c.GetName()] {
			c.Name = p.GetName() + "/conditions/" + newID()
		}
	}
	if hasLog {
		if n > 1 {
			return apierror.InvalidArgument("Field alertPolicy.conditions had an invalid value: a log-based alerting policy can have only one condition")
		}
		if p.GetAlertStrategy().GetNotificationRateLimit().GetPeriod() == nil {
			return apierror.InvalidArgument("Field alertPolicy.alertStrategy.notificationRateLimit is required for log-based alerting policies")
		}
	}
	policyChannels := map[string]bool{}
	for _, c := range p.GetNotificationChannels() {
		policyChannels[c] = true
	}
	for i, st := range p.GetAlertStrategy().GetNotificationChannelStrategy() {
		f := "alertPolicy.alertStrategy.notificationChannelStrategy[" + itoa(i) + "]"
		for _, n := range st.GetNotificationChannelNames() {
			if !policyChannels[n] {
				return apierror.InvalidArgument("Field %s.notificationChannelNames had an invalid value of %q: name one of the policy's notificationChannels", f, n)
			}
		}
		if r := st.GetRenotifyInterval(); r != nil {
			if d := r.AsDuration(); d < 30*time.Minute || d > 24*time.Hour || d%time.Minute != 0 {
				return apierror.InvalidArgument("Field %s.renotifyInterval had an invalid value of %q: it must be from 30 minutes to 24 hours, a whole number of minutes", f, d)
			}
			if hasLog {
				return apierror.InvalidArgument("Field %s.renotifyInterval had an invalid value: log-based alerting policies repeat notifications by notificationRateLimit", f)
			}
		}
	}
	if ac := p.GetAlertStrategy().GetAutoClose(); ac != nil {
		if d := ac.AsDuration(); d < 30*time.Minute || d > 7*24*time.Hour {
			return apierror.InvalidArgument("Field alertPolicy.alertStrategy.autoClose had an invalid value of %q: it must be from 30 minutes to 7 days", d)
		}
	}
	return nil
}

func checkCondition(i int, c *monitoringpb.AlertPolicy_Condition) error {
	field := func(f string) string { return "alertPolicy.conditions[" + itoa(i) + "]." + f }
	if strings.TrimSpace(c.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field %s is required", field("displayName"))
	}
	checkFilter := func(f, filter string) error {
		if filter == "" {
			return apierror.InvalidArgument("Field %s is required", field(f))
		}
		if len([]rune(filter)) > MaxFilter {
			return apierror.InvalidArgument("Field %s had an invalid value: at most %d characters", field(f), MaxFilter)
		}
		if isSLOSelector(filter) {
			if _, err := parseSLOSelector(filter); err != nil {
				return apierror.InvalidArgument("Field %s had an invalid value of %q: %v", field(f), filter, apierror.From(err).Message)
			}
			return nil
		}
		pf, err := parseFilter(filter, filterSeries)
		if err != nil {
			return apierror.InvalidArgument("Field %s had an invalid value of %q: %v", field(f), filter, apierror.From(err).Message)
		}
		if pf.metricType == "" {
			return apierror.InvalidArgument("Field %s had an invalid value of %q: the filter must specify a metric type, as metric.type = \"TYPE\"", field(f), filter)
		}
		return nil
	}
	checkAggs := func(f string, aggs []*monitoringpb.Aggregation) error {
		if len(aggs) > 2 {
			return apierror.Unimplemented("%s: more than two aggregations are not supported", field(f))
		}
		for j, a := range aggs {
			if _, err := checkAggregation(field(f)+"["+itoa(j)+"]", a); err != nil {
				if j == 1 && a.GetPerSeriesAligner() == monitoringpb.Aggregation_ALIGN_NONE {
					continue
				}
				return err
			}
		}
		return nil
	}
	checkTrigger := func(f string, t *monitoringpb.AlertPolicy_Condition_Trigger) error {
		if t.GetCount() < 0 || t.GetPercent() < 0 || t.GetPercent() > 100 {
			return apierror.InvalidArgument("Field %s had an invalid value", field(f))
		}
		return nil
	}
	switch {
	case c.GetConditionThreshold() != nil:
		t := c.GetConditionThreshold()
		if err := checkFilter("conditionThreshold.filter", t.GetFilter()); err != nil {
			return err
		}
		if t.GetDenominatorFilter() != "" {
			if err := checkFilter("conditionThreshold.denominatorFilter", t.GetDenominatorFilter()); err != nil {
				return err
			}
		}
		if err := checkAggs("conditionThreshold.aggregations", t.GetAggregations()); err != nil {
			return err
		}
		if err := checkAggs("conditionThreshold.denominatorAggregations", t.GetDenominatorAggregations()); err != nil {
			return err
		}
		if t.GetComparison() == monitoringpb.ComparisonType_COMPARISON_UNSPECIFIED {
			return apierror.InvalidArgument("Field %s is required", field("conditionThreshold.comparison"))
		}
		if d := t.GetDuration().AsDuration(); d < 0 || d%time.Minute != 0 || d > 24*time.Hour {
			return apierror.InvalidArgument("Field %s had an invalid value of %q: the duration must be a whole number of minutes, at most 24 hours", field("conditionThreshold.duration"), d)
		}
		if t.GetForecastOptions() != nil {
			return apierror.Unimplemented("%s: forecast conditions are not supported", field("conditionThreshold.forecastOptions"))
		}
		return checkTrigger("conditionThreshold.trigger", t.GetTrigger())
	case c.GetConditionAbsent() != nil:
		a := c.GetConditionAbsent()
		if err := checkFilter("conditionAbsent.filter", a.GetFilter()); err != nil {
			return err
		}
		if err := checkAggs("conditionAbsent.aggregations", a.GetAggregations()); err != nil {
			return err
		}
		if a.GetDuration() == nil {
			return apierror.InvalidArgument("Field %s is required", field("conditionAbsent.duration"))
		}
		if d := a.GetDuration().AsDuration().Truncate(time.Second); d < 120*time.Second || d%time.Minute != 0 || d > 24*time.Hour {
			return apierror.InvalidArgument("Field %s had an invalid value of %q: the minimum is 120 seconds, and longer durations must be a multiple of a minute", field("conditionAbsent.duration"), d)
		}
		return checkTrigger("conditionAbsent.trigger", a.GetTrigger())
	case c.GetConditionMatchedLog() != nil:
		l := c.GetConditionMatchedLog()
		if strings.TrimSpace(l.GetFilter()) == "" {
			return apierror.InvalidArgument("Field %s is required", field("conditionMatchedLog.filter"))
		}
		return nil
	case c.GetConditionMonitoringQueryLanguage() != nil:
		return checkMQLCondition(field, c.GetConditionMonitoringQueryLanguage())
	case c.GetConditionPrometheusQueryLanguage() != nil:
		return checkPromCondition(field, c.GetConditionPrometheusQueryLanguage())
	case c.GetConditionSql() != nil:
		return apierror.Unimplemented("%s: SQL conditions are not supported", field("conditionSql"))
	}
	return apierror.InvalidArgument("Field %s had an invalid value: a condition must set one of conditionThreshold, conditionAbsent or conditionMatchedLog", field("condition"))
}

func itoa(i int) string { return strconv.Itoa(i) }
