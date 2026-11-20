package monitoring

import (
	"context"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ServiceMonitoringService (#1964): services and their service-level
// objectives are kept in the instance's state with Google's rules
// (docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/services and
// /services.serviceLevelObjectives, read 2026-11-19). An SLO's compliance,
// budget and burn rate are read with ListTimeSeries' select_slo_* filters
// (sloseries.go).

var serviceIDRE = regexp.MustCompile(`^[a-zA-Z0-9\-_:.]+$`)

func (s *Server) CreateService(_ context.Context, req *monitoringpb.CreateServiceRequest) (*monitoringpb.Service, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	svc := clone(req.GetService())
	if svc == nil {
		return nil, apierror.InvalidArgument("Request was missing field service")
	}
	id := req.GetServiceId()
	if id == "" {
		id = newSlug("")
	}
	if !serviceIDRE.MatchString(id) || len(id) > 128 {
		return nil, apierror.InvalidArgument("Field serviceId had an invalid value of %q: use letters, digits, -, _, : and .", id)
	}
	if svc.GetIdentifier() == nil {
		svc.Identifier = &monitoringpb.Service_Custom_{Custom: &monitoringpb.Service_Custom{}}
	}
	if _, ok := svc.GetIdentifier().(*monitoringpb.Service_Custom_); !ok && svc.GetBasicService() == nil {
		// The automatic identifiers (cloud_run, gke_service, ...) are
		// accepted as given.
	}
	if err := checkUserLabels("service.userLabels", svc.GetUserLabels()); err != nil {
		return nil, err
	}
	svc.Name = "projects/" + project + "/services/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindServices, svc.Name) != nil {
		return nil, apierror.AlreadyExists("Service %s already exists", svc.Name)
	}
	s.put(kindServices, svc.Name, svc)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(svc), nil
}

func (s *Server) GetService(_ context.Context, req *monitoringpb.GetServiceRequest) (*monitoringpb.Service, error) {
	if _, err := childName("name", req.GetName(), "services"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, _ := s.get(kindServices, req.GetName()).(*monitoringpb.Service)
	if svc == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(svc), nil
}

func (s *Server) ListServices(_ context.Context, req *monitoringpb.ListServicesRequest) (*monitoringpb.ListServicesResponse, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	terms, err := parseSimpleFilter("filter", req.GetFilter())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*monitoringpb.Service
	for _, m := range s.under(kindServices, "projects/"+project+"/") {
		if matchSimple(terms, m) {
			items = append(items, m.(*monitoringpb.Service))
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("services|"+req.GetParent()+"|"+req.GetFilter(), items, (*monitoringpb.Service).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListServicesResponse{Services: page, NextPageToken: next}, nil
}

func (s *Server) UpdateService(_ context.Context, req *monitoringpb.UpdateServiceRequest) (*monitoringpb.Service, error) {
	in := req.GetService()
	if _, err := childName("service.name", in.GetName(), "services"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindServices, in.GetName()).(*monitoringpb.Service)
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
	if next.GetIdentifier() == nil {
		next.Identifier = old.Identifier
	}
	if err := checkUserLabels("service.userLabels", next.GetUserLabels()); err != nil {
		return nil, err
	}
	s.put(kindServices, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(next), nil
}

func (s *Server) DeleteService(_ context.Context, req *monitoringpb.DeleteServiceRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "services"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindServices, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(s.objects[kindServices], req.GetName())
	for n := range s.objects[kindSLOs] {
		if strings.HasPrefix(n, req.GetName()+"/") {
			delete(s.objects[kindSLOs], n)
		}
	}
	return &emptypb.Empty{}, s.save()
}

// sloParent checks projects/P/services/S/serviceLevelObjectives/ID.
func sloName(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "services" || parts[4] != "serviceLevelObjectives" || parts[5] == "" {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: expected projects/PROJECT/services/SERVICE/serviceLevelObjectives/ID", field, name)
	}
	return nil
}

func checkSLO(o *monitoringpb.ServiceLevelObjective) error {
	if g := o.GetGoal(); g <= 0 || g > 0.9999 {
		return apierror.InvalidArgument("Field serviceLevelObjective.goal had an invalid value of %g: the goal must be greater than 0 and at most 0.9999", g)
	}
	switch {
	case o.GetRollingPeriod() != nil:
		d := o.GetRollingPeriod().AsDuration()
		if d%(24*time.Hour) != 0 || d < 24*time.Hour || d > 30*24*time.Hour {
			return apierror.InvalidArgument("Field serviceLevelObjective.rollingPeriod had an invalid value of %q: a whole number of days from 1 to 30", d)
		}
	case o.GetCalendarPeriod() != 0:
	default:
		return apierror.InvalidArgument("Field serviceLevelObjective.period is required: set rollingPeriod or calendarPeriod")
	}
	sli := o.GetServiceLevelIndicator()
	if sli == nil {
		return apierror.InvalidArgument("Field serviceLevelObjective.serviceLevelIndicator is required")
	}
	if rb := sli.GetRequestBased(); rb != nil {
		if r := rb.GetGoodTotalRatio(); r != nil {
			n := 0
			for _, f := range []string{r.GetGoodServiceFilter(), r.GetBadServiceFilter(), r.GetTotalServiceFilter()} {
				if f != "" {
					n++
					if _, err := parseFilter(f, filterSeries); err != nil {
						return err
					}
				}
			}
			if n != 2 {
				return apierror.InvalidArgument("Field serviceLevelObjective.serviceLevelIndicator.requestBased.goodTotalRatio had an invalid value: set exactly two of goodServiceFilter, badServiceFilter and totalServiceFilter")
			}
		} else if rb.GetDistributionCut() == nil {
			return apierror.InvalidArgument("Field serviceLevelObjective.serviceLevelIndicator.requestBased had an invalid value: set goodTotalRatio or distributionCut")
		}
	}
	return checkUserLabels("serviceLevelObjective.userLabels", o.GetUserLabels())
}

func (s *Server) CreateServiceLevelObjective(_ context.Context, req *monitoringpb.CreateServiceLevelObjectiveRequest) (*monitoringpb.ServiceLevelObjective, error) {
	if _, err := childName("parent", req.GetParent(), "services"); err != nil {
		return nil, err
	}
	o := clone(req.GetServiceLevelObjective())
	if o == nil {
		return nil, apierror.InvalidArgument("Request was missing field serviceLevelObjective")
	}
	id := req.GetServiceLevelObjectiveId()
	if id == "" {
		id = newSlug("")
	}
	if !serviceIDRE.MatchString(id) || len(id) > 128 {
		return nil, apierror.InvalidArgument("Field serviceLevelObjectiveId had an invalid value of %q", id)
	}
	if err := checkSLO(o); err != nil {
		return nil, err
	}
	o.Name = req.GetParent() + "/serviceLevelObjectives/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindServices, req.GetParent()) == nil {
		return nil, apierror.NotFound("Service %s was not found", req.GetParent())
	}
	if s.get(kindSLOs, o.Name) != nil {
		return nil, apierror.AlreadyExists("Service level objective %s already exists", o.Name)
	}
	s.put(kindSLOs, o.Name, o)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(o), nil
}

func (s *Server) GetServiceLevelObjective(_ context.Context, req *monitoringpb.GetServiceLevelObjectiveRequest) (*monitoringpb.ServiceLevelObjective, error) {
	if err := sloName("name", req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, _ := s.get(kindSLOs, req.GetName()).(*monitoringpb.ServiceLevelObjective)
	if o == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(o), nil
}

func (s *Server) ListServiceLevelObjectives(_ context.Context, req *monitoringpb.ListServiceLevelObjectivesRequest) (*monitoringpb.ListServiceLevelObjectivesResponse, error) {
	parent := req.GetParent()
	if !strings.HasSuffix(parent, "/services/-") {
		if _, err := childName("parent", parent, "services"); err != nil {
			return nil, err
		}
	}
	terms, err := parseSimpleFilter("filter", req.GetFilter())
	if err != nil {
		return nil, err
	}
	prefix := parent + "/"
	if strings.HasSuffix(parent, "/services/-") {
		prefix = strings.TrimSuffix(parent, "-")
	}
	s.mu.Lock()
	if !strings.HasSuffix(parent, "/services/-") && s.get(kindServices, parent) == nil {
		s.mu.Unlock()
		return nil, apierror.NotFound("Service %s was not found", parent)
	}
	var items []*monitoringpb.ServiceLevelObjective
	for _, m := range s.under(kindSLOs, prefix) {
		if matchSimple(terms, m) {
			items = append(items, m.(*monitoringpb.ServiceLevelObjective))
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("slos|"+parent+"|"+req.GetFilter(), items, (*monitoringpb.ServiceLevelObjective).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListServiceLevelObjectivesResponse{ServiceLevelObjectives: page, NextPageToken: next}, nil
}

func (s *Server) UpdateServiceLevelObjective(_ context.Context, req *monitoringpb.UpdateServiceLevelObjectiveRequest) (*monitoringpb.ServiceLevelObjective, error) {
	in := req.GetServiceLevelObjective()
	if err := sloName("serviceLevelObjective.name", in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindSLOs, in.GetName()).(*monitoringpb.ServiceLevelObjective)
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
	if err := checkSLO(next); err != nil {
		return nil, err
	}
	s.put(kindSLOs, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(next), nil
}

func (s *Server) DeleteServiceLevelObjective(_ context.Context, req *monitoringpb.DeleteServiceLevelObjectiveRequest) (*emptypb.Empty, error) {
	if err := sloName("name", req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindSLOs, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(s.objects[kindSLOs], req.GetName())
	return &emptypb.Empty{}, s.save()
}

// ---- SnoozeService ----

// MaxSnoozePolicies is how many policies one snooze may name.
const MaxSnoozePolicies = 16

func (s *Server) CreateSnooze(_ context.Context, req *monitoringpb.CreateSnoozeRequest) (*monitoringpb.Snooze, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	sn := clone(req.GetSnooze())
	if sn == nil {
		return nil, apierror.InvalidArgument("Request was missing field snooze")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkSnooze(project, sn); err != nil {
		return nil, err
	}
	if !sn.GetInterval().GetEndTime().AsTime().After(s.now()) {
		return nil, apierror.InvalidArgument("Field snooze.interval.endTime had an invalid value: a snooze must end in the future")
	}
	sn.Name = "projects/" + project + "/snoozes/" + newID()
	s.put(kindSnoozes, sn.Name, sn)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(sn), nil
}

func (s *Server) checkSnooze(project string, sn *monitoringpb.Snooze) error {
	if strings.TrimSpace(sn.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field snooze.displayName is required")
	}
	ps := sn.GetCriteria().GetPolicies()
	if len(ps) == 0 || len(ps) > MaxSnoozePolicies {
		return apierror.InvalidArgument("Field snooze.criteria.policies had an invalid value: name 1 to %d alerting policies", MaxSnoozePolicies)
	}
	for i, p := range ps {
		if projectOfName(p) != project || s.get(kindPolicies, p) == nil {
			return apierror.InvalidArgument("Field snooze.criteria.policies[%d] had an invalid value of %q: no such alerting policy in project %s", i, p, project)
		}
	}
	if sn.GetCriteria().GetFilter() != "" {
		return apierror.Unimplemented("Field snooze.criteria.filter is not supported")
	}
	iv := sn.GetInterval()
	if iv.GetEndTime() == nil {
		return apierror.InvalidArgument("Field snooze.interval.endTime is required")
	}
	if iv.GetStartTime() != nil && !iv.GetEndTime().AsTime().After(iv.GetStartTime().AsTime()) {
		return apierror.InvalidArgument("Field snooze.interval had an invalid value: the end time must be after the start time")
	}
	return nil
}

func (s *Server) ListSnoozes(_ context.Context, req *monitoringpb.ListSnoozesRequest) (*monitoringpb.ListSnoozesResponse, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("Field filter is not supported for snoozes")
	}
	s.mu.Lock()
	var items []*monitoringpb.Snooze
	for _, m := range s.under(kindSnoozes, "projects/"+project+"/") {
		items = append(items, m.(*monitoringpb.Snooze))
	}
	s.mu.Unlock()
	page, next, err := pageOf("snoozes|"+req.GetParent(), items, (*monitoringpb.Snooze).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListSnoozesResponse{Snoozes: page, NextPageToken: next}, nil
}

func (s *Server) GetSnooze(_ context.Context, req *monitoringpb.GetSnoozeRequest) (*monitoringpb.Snooze, error) {
	if _, err := childName("name", req.GetName(), "snoozes"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, _ := s.get(kindSnoozes, req.GetName()).(*monitoringpb.Snooze)
	if sn == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(sn), nil
}

func (s *Server) UpdateSnooze(_ context.Context, req *monitoringpb.UpdateSnoozeRequest) (*monitoringpb.Snooze, error) {
	in := req.GetSnooze()
	project, err := childName("snooze.name", in.GetName(), "snoozes")
	if err != nil {
		return nil, err
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.InvalidArgument("Field updateMask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindSnoozes, in.GetName()).(*monitoringpb.Snooze)
	if old == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if !old.GetInterval().GetEndTime().AsTime().After(s.now()) {
		return nil, apierror.FailedPrecondition("Snooze %s has ended and cannot be changed", in.GetName())
	}
	next := clone(old)
	if err := applyMask(next, in, req.GetUpdateMask()); err != nil {
		return nil, err
	}
	if err := s.checkSnooze(project, next); err != nil {
		return nil, err
	}
	s.put(kindSnoozes, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(next), nil
}

// snoozedLocked is whether a snooze covers a policy at t; the caller
// holds mu.
func (s *Server) snoozedLocked(policy string, t time.Time) bool {
	for _, m := range s.under(kindSnoozes, "projects/"+projectOfName(policy)+"/") {
		sn := m.(*monitoringpb.Snooze)
		iv := sn.GetInterval()
		if iv.GetStartTime() != nil && t.Before(iv.GetStartTime().AsTime()) || !t.Before(iv.GetEndTime().AsTime()) {
			continue
		}
		for _, p := range sn.GetCriteria().GetPolicies() {
			if p == policy {
				return true
			}
		}
	}
	return false
}
