package monitoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// DashboardsService (google.monitoring.dashboard.v1, #1964): dashboards
// are kept in the instance's state with an etag that changes on each
// write, as docs.cloud.google.com/monitoring/api/ref_v3/rest/v1/projects.dashboards
// describes (read 2026-11-19), and drawn on the console's Dashboards page.

// MaxDashboards is "Dashboards per metrics scope: 1,000"; MaxWidgets is
// "Charts per dashboard: 40" counted over every widget
// (docs.cloud.google.com/monitoring/quotas, read 2026-11-19).
const (
	MaxDashboards = 1000
	MaxWidgets    = 100
)

var dashboardIDRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func dashboardEtag(d *dashboardpb.Dashboard) string {
	c := clone(d)
	c.Etag = ""
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

func checkDashboard(d *dashboardpb.Dashboard) error {
	if strings.TrimSpace(d.GetDisplayName()) == "" {
		return apierror.InvalidArgument("Field dashboard.displayName is required")
	}
	if err := checkUserLabels("dashboard.labels", d.GetLabels()); err != nil {
		return err
	}
	n := 0
	switch l := d.GetLayout().(type) {
	case *dashboardpb.Dashboard_GridLayout:
		n = len(l.GridLayout.GetWidgets())
	case *dashboardpb.Dashboard_MosaicLayout:
		n = len(l.MosaicLayout.GetTiles())
		for i, t := range l.MosaicLayout.GetTiles() {
			if t.GetWidth() < 0 || t.GetHeight() < 0 || t.GetXPos() < 0 || t.GetYPos() < 0 {
				return apierror.InvalidArgument("Field dashboard.mosaicLayout.tiles[%d] had an invalid value: positions and sizes are not negative", i)
			}
			if cols := l.MosaicLayout.GetColumns(); cols > 0 && t.GetXPos()+t.GetWidth() > cols {
				return apierror.InvalidArgument("Field dashboard.mosaicLayout.tiles[%d] had an invalid value: the tile is wider than the layout's %d columns", i, cols)
			}
		}
	case *dashboardpb.Dashboard_RowLayout:
		for _, r := range l.RowLayout.GetRows() {
			n += len(r.GetWidgets())
		}
	case *dashboardpb.Dashboard_ColumnLayout:
		for _, c := range l.ColumnLayout.GetColumns() {
			n += len(c.GetWidgets())
		}
	}
	if n > MaxWidgets {
		return apierror.InvalidArgument("Field dashboard had an invalid value: a dashboard can have at most %d widgets", MaxWidgets)
	}
	return nil
}

func (s *Server) CreateDashboard(_ context.Context, req *dashboardpb.CreateDashboardRequest) (*dashboardpb.Dashboard, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	d := clone(req.GetDashboard())
	if d == nil {
		return nil, apierror.InvalidArgument("Request was missing field dashboard")
	}
	id := uuid.NewString()
	if d.GetName() != "" {
		p, err := childName("dashboard.name", d.GetName(), "dashboards")
		if err != nil {
			return nil, err
		}
		if p != project {
			return nil, apierror.InvalidArgument("Field dashboard.name had an invalid value of %q: it is not in %s", d.GetName(), req.GetParent())
		}
		id = d.GetName()[strings.LastIndex(d.GetName(), "/")+1:]
		if !dashboardIDRE.MatchString(id) {
			return nil, apierror.InvalidArgument("Field dashboard.name had an invalid value of %q", d.GetName())
		}
	}
	if err := checkDashboard(d); err != nil {
		return nil, err
	}
	d.Name = "projects/" + project + "/dashboards/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindDashboards, d.Name) != nil {
		return nil, apierror.AlreadyExists("Dashboard %s already exists", d.Name)
	}
	if len(s.under(kindDashboards, "projects/"+project+"/")) >= MaxDashboards {
		return nil, apierror.FailedPrecondition("The project %s has reached its limit of %d dashboards", project, MaxDashboards)
	}
	d.Etag = dashboardEtag(d)
	if req.GetValidateOnly() {
		return d, nil
	}
	s.put(kindDashboards, d.Name, d)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(d), nil
}

func (s *Server) ListDashboards(_ context.Context, req *dashboardpb.ListDashboardsRequest) (*dashboardpb.ListDashboardsResponse, error) {
	project, err := parseProject("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*dashboardpb.Dashboard
	for _, m := range s.under(kindDashboards, "projects/"+project+"/") {
		items = append(items, m.(*dashboardpb.Dashboard))
	}
	s.mu.Unlock()
	page, next, err := pageOf("dashboards|"+req.GetParent(), items, (*dashboardpb.Dashboard).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &dashboardpb.ListDashboardsResponse{Dashboards: page, NextPageToken: next}, nil
}

func (s *Server) GetDashboard(_ context.Context, req *dashboardpb.GetDashboardRequest) (*dashboardpb.Dashboard, error) {
	if _, err := childName("name", req.GetName(), "dashboards"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, _ := s.get(kindDashboards, req.GetName()).(*dashboardpb.Dashboard)
	if d == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return clone(d), nil
}

func (s *Server) DeleteDashboard(_ context.Context, req *dashboardpb.DeleteDashboardRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "dashboards"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindDashboards, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(s.objects[kindDashboards], req.GetName())
	return &emptypb.Empty{}, s.save()
}

func (s *Server) UpdateDashboard(_ context.Context, req *dashboardpb.UpdateDashboardRequest) (*dashboardpb.Dashboard, error) {
	d := clone(req.GetDashboard())
	if _, err := childName("dashboard.name", d.GetName(), "dashboards"); err != nil {
		return nil, err
	}
	if err := checkDashboard(d); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindDashboards, d.GetName()).(*dashboardpb.Dashboard)
	if old == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if d.GetEtag() != "" && d.GetEtag() != old.GetEtag() {
		return nil, apierror.Aborted("The dashboard %s was changed since etag %q was read: get it again and reapply the change", d.GetName(), d.GetEtag())
	}
	d.Etag = dashboardEtag(d)
	if req.GetValidateOnly() {
		return d, nil
	}
	s.put(kindDashboards, d.Name, d)
	if err := s.save(); err != nil {
		return nil, err
	}
	return clone(d), nil
}
