package bigtablefront

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Logical and materialized views (#1499): the instance admin's
// CreateLogicalView, GetLogicalView, ListLogicalViews, UpdateLogicalView
// and DeleteLogicalView, and the same five for materialized views, kept
// with the instance they belong to (instances.go), which must exist. A
// view's query is stored as given: neither the emulator nor the front runs
// SQL, so a view is never read (ExecuteQuery, and ReadRows naming a
// materialized view, stay refused), and the query is not checked against
// the tables beyond being non-empty. A materialized view's query cannot be
// changed, as Google documents; a logical view's can. Deleting the instance
// deletes its views.

var sqlViewIDRe = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]{0,127}$`)

// sqlView is a logical or a materialized view.
type sqlView struct {
	name, query, etag string
	protected         bool
}

// sqlKind is what differs between the two kinds.
type sqlKind struct {
	segment, noun string // "logicalViews", "LogicalView"
	queryMutable  bool
}

var (
	logicalKind      = sqlKind{segment: "logicalViews", noun: "LogicalView", queryMutable: true}
	materializedKind = sqlKind{segment: "materializedViews", noun: "MaterializedView"}
)

// sqlViews is the views of both kinds, by name.
type sqlViews struct {
	instances *instances
	// checkQuery compiles a view's query against the instance (#1511), as
	// Google does when a view is created or its query changed.
	checkQuery func(instance, query string) error
	mu         sync.Mutex
	byName     map[string]*sqlView
}

func (s *sqlViews) check(instance, query string) error {
	if s.checkQuery == nil {
		return nil
	}
	return s.checkQuery(instance, query)
}

func (s *sqlViews) instanceExists(parent string) error {
	s.instances.mu.Lock()
	defer s.instances.mu.Unlock()
	_, err := s.instances.lookup(parent)
	return err
}

func (s *sqlViews) create(k sqlKind, parent, id string, query string, protected bool) (*sqlView, error) {
	if err := s.instanceExists(parent); err != nil {
		return nil, err
	}
	if !sqlViewIDRe.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s ID %q", k.noun, id)
	}
	if strings.TrimSpace(query) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "%s.query is required", k.noun)
	}
	if err := s.check(parent, query); err != nil {
		return nil, err
	}
	v := &sqlView{name: parent + "/" + k.segment + "/" + id, query: query, etag: newEtag(), protected: protected}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[v.name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "%s %s already exists.", k.noun, v.name)
	}
	s.byName[v.name] = v
	c := *v
	return &c, nil
}

// lookup returns the view name names. s.mu is held.
func (s *sqlViews) lookup(k sqlKind, name string) (*sqlView, error) {
	i := strings.Index(name, "/"+k.segment+"/")
	if i < 0 || !instanceRe.MatchString(name[:i]) || strings.Contains(name[i+len(k.segment)+2:], "/") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s name %q: want projects/{project}/instances/{instance}/%s/{id}", k.noun, name, k.segment)
	}
	v, ok := s.byName[name]
	if !ok {
		return nil, notFound(k.noun, name)
	}
	return v, nil
}

func (s *sqlViews) get(k sqlKind, name string) (*sqlView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lookup(k, name)
	if err != nil {
		return nil, err
	}
	c := *v
	return &c, nil
}

func (s *sqlViews) list(k sqlKind, parent, token string, size int32) ([]sqlView, string, error) {
	if err := s.instanceExists(parent); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.byName {
		if strings.HasPrefix(n, parent+"/"+k.segment+"/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start, end, next, err := page(len(names), token, size)
	if err != nil {
		return nil, "", err
	}
	out := make([]sqlView, 0, end-start)
	for _, n := range names[start:end] {
		out = append(out, *s.byName[n])
	}
	return out, next, nil
}

func (s *sqlViews) update(k sqlKind, upd sqlView, mask *fieldmaskpb.FieldMask) (*sqlView, error) {
	for _, p := range mask.GetPaths() {
		if i := strings.Index(upd.name, "/"+k.segment+"/"); p == "query" && k.queryMutable && i > 0 && strings.TrimSpace(upd.query) != "" {
			if err := s.check(upd.name[:i], upd.query); err != nil {
				return nil, err
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lookup(k, upd.name)
	if err != nil {
		return nil, err
	}
	if upd.etag != "" && upd.etag != v.etag {
		return nil, status.Errorf(codes.Aborted, "the etag does not match the %s's current etag", k.noun)
	}
	paths := mask.GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	next := *v
	for _, p := range paths {
		switch {
		case p == "query" && k.queryMutable:
			if strings.TrimSpace(upd.query) == "" {
				return nil, status.Errorf(codes.InvalidArgument, "%s.query is required", k.noun)
			}
			next.query = upd.query
		case p == "deletion_protection":
			next.protected = upd.protected
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not updatable on a %s", p, k.noun)
		}
	}
	next.etag = newEtag()
	s.byName[next.name] = &next
	c := next
	return &c, nil
}

func (s *sqlViews) delete(k sqlKind, name, etag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lookup(k, name)
	if err != nil {
		return err
	}
	if etag != "" && etag != v.etag {
		return status.Errorf(codes.Aborted, "the etag does not match the %s's current etag", k.noun)
	}
	if v.protected {
		return status.Errorf(codes.FailedPrecondition, "%s %s is protected against deletion", k.noun, name)
	}
	delete(s.byName, name)
	return nil
}

// drop forgets the views of instance.
func (s *sqlViews) drop(instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := range s.byName {
		if strings.HasPrefix(n, instance+"/") {
			delete(s.byName, n)
		}
	}
}

func (v *sqlView) logical() *btapb.LogicalView {
	return &btapb.LogicalView{Name: v.name, Query: v.query, Etag: v.etag, DeletionProtection: v.protected}
}

func (v *sqlView) materialized() *btapb.MaterializedView {
	return &btapb.MaterializedView{Name: v.name, Query: v.query, Etag: v.etag, DeletionProtection: v.protected}
}

// methods are the ten view methods; op records a done operation.
func (s *sqlViews) methods(op func(string, proto.Message, proto.Message) (proto.Message, error)) map[string]method {
	return map[string]method{
		instanceAdmin + "CreateLogicalView": unary(func() *btapb.CreateLogicalViewRequest { return &btapb.CreateLogicalViewRequest{} },
			func(_ context.Context, req *btapb.CreateLogicalViewRequest) (proto.Message, error) {
				start := time.Now()
				lv := req.GetLogicalView()
				v, err := s.create(logicalKind, req.GetParent(), req.GetLogicalViewId(), lv.GetQuery(), lv.GetDeletionProtection())
				if err != nil {
					return nil, err
				}
				return op(v.name, &btapb.CreateLogicalViewMetadata{OriginalRequest: req, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, v.logical())
			}),
		instanceAdmin + "GetLogicalView": unary(func() *btapb.GetLogicalViewRequest { return &btapb.GetLogicalViewRequest{} },
			func(_ context.Context, req *btapb.GetLogicalViewRequest) (proto.Message, error) {
				v, err := s.get(logicalKind, req.GetName())
				if err != nil {
					return nil, err
				}
				return v.logical(), nil
			}),
		instanceAdmin + "ListLogicalViews": unary(func() *btapb.ListLogicalViewsRequest { return &btapb.ListLogicalViewsRequest{} },
			func(_ context.Context, req *btapb.ListLogicalViewsRequest) (proto.Message, error) {
				vs, next, err := s.list(logicalKind, req.GetParent(), req.GetPageToken(), req.GetPageSize())
				if err != nil {
					return nil, err
				}
				out := &btapb.ListLogicalViewsResponse{NextPageToken: next}
				for i := range vs {
					out.LogicalViews = append(out.LogicalViews, vs[i].logical())
				}
				return out, nil
			}),
		instanceAdmin + "UpdateLogicalView": unary(func() *btapb.UpdateLogicalViewRequest { return &btapb.UpdateLogicalViewRequest{} },
			func(_ context.Context, req *btapb.UpdateLogicalViewRequest) (proto.Message, error) {
				start := time.Now()
				lv := req.GetLogicalView()
				v, err := s.update(logicalKind, sqlView{name: lv.GetName(), query: lv.GetQuery(), etag: lv.GetEtag(), protected: lv.GetDeletionProtection()}, req.GetUpdateMask())
				if err != nil {
					return nil, err
				}
				return op(v.name, &btapb.UpdateLogicalViewMetadata{OriginalRequest: req, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, v.logical())
			}),
		instanceAdmin + "DeleteLogicalView": unary(func() *btapb.DeleteLogicalViewRequest { return &btapb.DeleteLogicalViewRequest{} },
			func(_ context.Context, req *btapb.DeleteLogicalViewRequest) (proto.Message, error) {
				return &emptypb.Empty{}, s.delete(logicalKind, req.GetName(), req.GetEtag())
			}),
		instanceAdmin + "CreateMaterializedView": unary(func() *btapb.CreateMaterializedViewRequest { return &btapb.CreateMaterializedViewRequest{} },
			func(_ context.Context, req *btapb.CreateMaterializedViewRequest) (proto.Message, error) {
				start := time.Now()
				mv := req.GetMaterializedView()
				v, err := s.create(materializedKind, req.GetParent(), req.GetMaterializedViewId(), mv.GetQuery(), mv.GetDeletionProtection())
				if err != nil {
					return nil, err
				}
				return op(v.name, &btapb.CreateMaterializedViewMetadata{OriginalRequest: req, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, v.materialized())
			}),
		instanceAdmin + "GetMaterializedView": unary(func() *btapb.GetMaterializedViewRequest { return &btapb.GetMaterializedViewRequest{} },
			func(_ context.Context, req *btapb.GetMaterializedViewRequest) (proto.Message, error) {
				v, err := s.get(materializedKind, req.GetName())
				if err != nil {
					return nil, err
				}
				return v.materialized(), nil
			}),
		instanceAdmin + "ListMaterializedViews": unary(func() *btapb.ListMaterializedViewsRequest { return &btapb.ListMaterializedViewsRequest{} },
			func(_ context.Context, req *btapb.ListMaterializedViewsRequest) (proto.Message, error) {
				vs, next, err := s.list(materializedKind, req.GetParent(), req.GetPageToken(), req.GetPageSize())
				if err != nil {
					return nil, err
				}
				out := &btapb.ListMaterializedViewsResponse{NextPageToken: next}
				for i := range vs {
					out.MaterializedViews = append(out.MaterializedViews, vs[i].materialized())
				}
				return out, nil
			}),
		instanceAdmin + "UpdateMaterializedView": unary(func() *btapb.UpdateMaterializedViewRequest { return &btapb.UpdateMaterializedViewRequest{} },
			func(_ context.Context, req *btapb.UpdateMaterializedViewRequest) (proto.Message, error) {
				start := time.Now()
				mv := req.GetMaterializedView()
				v, err := s.update(materializedKind, sqlView{name: mv.GetName(), query: mv.GetQuery(), etag: mv.GetEtag(), protected: mv.GetDeletionProtection()}, req.GetUpdateMask())
				if err != nil {
					return nil, err
				}
				return op(v.name, &btapb.UpdateMaterializedViewMetadata{OriginalRequest: req, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, v.materialized())
			}),
		instanceAdmin + "DeleteMaterializedView": unary(func() *btapb.DeleteMaterializedViewRequest { return &btapb.DeleteMaterializedViewRequest{} },
			func(_ context.Context, req *btapb.DeleteMaterializedViewRequest) (proto.Message, error) {
				return &emptypb.Empty{}, s.delete(materializedKind, req.GetName(), req.GetEtag())
			}),
	}
}
