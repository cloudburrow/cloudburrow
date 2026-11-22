package monitoring

import (
	"context"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// GroupService (#1964): groups are kept in the instance's state; a
// group's members are the monitored resources of the project's time series
// (user-written and built-in) its filter matches, and its ancestors'
// filters too, as Google defines group membership
// (docs.cloud.google.com/monitoring/groups and
// /api/ref_v3/rest/v3/projects.groups, read 2026-11-19). The products'
// own resources (Compute Engine VMs, Cloud SQL instances, GKE clusters,
// buckets, topics: AddResourceInventory) are members too, with their
// metadata (#2146). Filters take the selectors of the time series filter
// and the resource metadata's (inventory.go).

// MaxGroups is "Groups per metrics scope: 500"; MaxGroupDepth is "Nesting
// depth: 6".
const (
	MaxGroups     = 500
	MaxGroupDepth = 6
)

func (s *Server) ListGroups(_ context.Context, req *monitoringpb.ListGroupsRequest) (*monitoringpb.ListGroupsResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	all := map[string]*monitoringpb.Group{}
	for _, m := range s.under(kindGroups, "projects/"+project+"/") {
		g := m.(*monitoringpb.Group)
		all[g.GetName()] = g
	}
	s.mu.Unlock()
	var items []*monitoringpb.Group
	switch {
	case req.GetChildrenOfGroup() != "":
		for _, g := range all {
			if g.GetParentName() == req.GetChildrenOfGroup() {
				items = append(items, g)
			}
		}
	case req.GetAncestorsOfGroup() != "":
		g := all[req.GetAncestorsOfGroup()]
		if g == nil {
			return nil, apierror.NotFound("Group %s was not found", req.GetAncestorsOfGroup())
		}
		for p := all[g.GetParentName()]; p != nil; p = all[p.GetParentName()] {
			items = append(items, p)
		}
	case req.GetDescendantsOfGroup() != "":
		var walk func(string)
		walk = func(n string) {
			for _, g := range all {
				if g.GetParentName() == n {
					items = append(items, g)
					walk(g.GetName())
				}
			}
		}
		walk(req.GetDescendantsOfGroup())
	default:
		for _, g := range all {
			items = append(items, g)
		}
	}
	if req.GetAncestorsOfGroup() == "" {
		sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	}
	page, next, err := pageOf("groups|"+req.GetName()+"|"+req.GetChildrenOfGroup()+req.GetAncestorsOfGroup()+req.GetDescendantsOfGroup(),
		items, (*monitoringpb.Group).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListGroupsResponse{Group: page, NextPageToken: next}, nil
}

func (s *Server) GetGroup(_ context.Context, req *monitoringpb.GetGroupRequest) (*monitoringpb.Group, error) {
	if _, err := childName("name", req.GetName(), "groups"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, _ := s.get(kindGroups, req.GetName()).(*monitoringpb.Group)
	if g == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(g), nil
}

// checkGroup validates a group; the caller holds mu.
func (s *Server) checkGroup(project string, g *monitoringpb.Group) error {
	if strings.TrimSpace(g.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field group.displayName is required")
	}
	if strings.TrimSpace(g.GetFilter()) == "" {
		return apierror.InvalidArgument("Field group.filter is required")
	}
	if _, err := parseFilter(g.GetFilter(), filterGroups); err != nil {
		return err
	}
	depth := 1
	for p := g.GetParentName(); p != ""; depth++ {
		if p == g.GetName() {
			return apierror.InvalidArgument("Field group.parentName had an invalid value of %q: a group cannot be its own ancestor", g.GetParentName())
		}
		pg, _ := s.get(kindGroups, p).(*monitoringpb.Group)
		if pg == nil || projectOfName(p) != project {
			return apierror.InvalidArgument("Field group.parentName had an invalid value of %q: no such group in project %s", g.GetParentName(), project)
		}
		p = pg.GetParentName()
	}
	if depth > MaxGroupDepth {
		return apierror.InvalidArgument("Field group.parentName had an invalid value: groups nest at most %d deep", MaxGroupDepth)
	}
	return nil
}

func (s *Server) CreateGroup(_ context.Context, req *monitoringpb.CreateGroupRequest) (*monitoringpb.Group, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	g := clone(req.GetGroup())
	if g == nil {
		return nil, apierror.InvalidArgument("Request was missing field group")
	}
	g.Name = "projects/" + project + "/groups/" + newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkGroup(project, g); err != nil {
		return nil, err
	}
	if len(s.under(kindGroups, "projects/"+project+"/")) >= MaxGroups {
		return nil, apierror.FailedPrecondition("The project %s has reached its limit of %d groups", project, MaxGroups)
	}
	if req.GetValidateOnly() {
		return g, nil
	}
	s.put(kindGroups, g.Name, g)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(g), nil
}

func (s *Server) UpdateGroup(_ context.Context, req *monitoringpb.UpdateGroupRequest) (*monitoringpb.Group, error) {
	g := clone(req.GetGroup())
	project, err := childName("group.name", g.GetName(), "groups")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindGroups, g.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if err := s.checkGroup(project, g); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return g, nil
	}
	s.put(kindGroups, g.Name, g)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(g), nil
}

func (s *Server) DeleteGroup(_ context.Context, req *monitoringpb.DeleteGroupRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "groups"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindGroups, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	var del []string
	var walk func(string)
	walk = func(n string) {
		del = append(del, n)
		for _, m := range s.under(kindGroups, "") {
			if g := m.(*monitoringpb.Group); g.GetParentName() == n {
				walk(g.GetName())
			}
		}
	}
	walk(req.GetName())
	if len(del) > 1 && !req.GetRecursive() {
		return nil, apierror.FailedPrecondition("Group %s has child groups; delete it with recursive to delete them too", req.GetName())
	}
	for _, n := range del {
		delete(s.objects[kindGroups], n)
	}
	return &emptypb.Empty{}, s.save()
}

func (s *Server) ListGroupMembers(ctx context.Context, req *monitoringpb.ListGroupMembersRequest) (*monitoringpb.ListGroupMembersResponse, error) {
	project, err := childName("name", req.GetName(), "groups")
	if err != nil {
		return nil, err
	}
	var extra *Filter
	if strings.TrimSpace(req.GetFilter()) != "" {
		if extra, err = parseFilter(req.GetFilter(), filterGroups); err != nil {
			return nil, err
		}
	}
	end := s.now()
	if req.GetInterval().GetEndTime() != nil {
		end = req.GetInterval().GetEndTime().AsTime()
	}
	inv := s.inventory(ctx, project)
	s.mu.Lock()
	if s.get(kindGroups, req.GetName()) == nil {
		s.mu.Unlock()
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	id := req.GetName()[strings.LastIndex(req.GetName(), "/")+1:]
	members := s.groupMembersLocked(project, id, end, inv)
	s.mu.Unlock()
	var keep []*monitoredres.MonitoredResource
	for _, r := range members {
		if extra == nil || extra.match(subject{project: project, resourceType: r.GetType(), resourceLabels: r.GetLabels(), meta: metaOf(inv, r)}) {
			keep = append(keep, r)
		}
	}
	page, next, err := pageOf("members|"+req.GetName()+"|"+req.GetFilter(), keep, monResKey, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListGroupMembersResponse{Members: page, NextPageToken: next, TotalSize: int32(len(keep))}, nil
}

// metaOf is a resource's metadata in an inventory, or nil.
func metaOf(inv map[string]InventoryResource, r *monitoredres.MonitoredResource) *ResourceMetadata {
	if ir, ok := inv[monResKey(r)]; ok {
		return &ir.Metadata
	}
	return nil
}

// groupMembersLocked are the resources a group's filter, and every
// ancestor's, match among the project's series of the 24 hours before end
// and its products' resources (inv); the caller holds mu.
func (s *Server) groupMembersLocked(project, id string, end time.Time, inv map[string]InventoryResource) []*monitoredres.MonitoredResource {
	var filters []*Filter
	for n := "projects/" + project + "/groups/" + id; n != ""; {
		g, _ := s.get(kindGroups, n).(*monitoringpb.Group)
		if g == nil {
			break
		}
		f, err := parseFilter(g.GetFilter(), filterGroups)
		if err != nil {
			return nil
		}
		filters = append(filters, f)
		n = g.GetParentName()
	}
	seen := map[string]*monitoredres.MonitoredResource{}
	consider := func(r *monitoredres.MonitoredResource, metric *metricpb.Metric) {
		sub := subject{project: project, resourceType: r.GetType(), resourceLabels: r.GetLabels(), metricType: metric.GetType(),
			metricLabels: metric.GetLabels(), meta: metaOf(inv, r)}
		for _, f := range filters {
			if !f.match(sub) {
				return
			}
		}
		seen[monResKey(r)] = clone(r)
	}
	for _, k := range inventoryKeys(inv) {
		consider(inv[k].Resource, nil)
	}
	start := end.Add(-24 * time.Hour)
	for _, sr := range s.series[project] {
		consider(sr.TS.GetResource(), sr.TS.GetMetric())
	}
	for _, b := range s.builtins {
		for _, md := range b.Descriptors(project) {
			ts, _ := b.Series(project, md.GetType(), start, end)
			for _, t := range ts {
				consider(t.GetResource(), t.GetMetric())
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*monitoredres.MonitoredResource, len(keys))
	for i, k := range keys {
		out[i] = seen[k]
	}
	return out
}
