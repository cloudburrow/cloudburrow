package bigtablefront

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1895) on the Bigtable admin resource types Google
// documents, bigtableadmin.googleapis.com/Instance, Cluster, AppProfile,
// Table, Backup, LogicalView, MaterializedView and SchemaBundle, CREATE
// and UPDATE, over gRPC and REST (which calls the same served methods)
// ("Use custom organization policies", Bigtable, read 2026-11-19). The
// condition reads the resource by its REST field names, the documented
// ones alone (internal/orgpolicy/fields_bigtable.go), as the request
// would leave it: a create's resource with its name, an update's the
// current one with the request's fields laid over it. An instance created
// with clusters is checked as an instance and each cluster as a cluster.
// Unset scalars read as their defaults (resource.deletionProtection is
// false on a table created without it).
//
// A violation is FAILED_PRECONDITION with the page's message, "Operation
// denied by custom org policy: [...]".

// customCheck is one resource a request would leave, of resourceType.
type customCheck struct {
	resourceType, method string
	m                    proto.Message
}

// customOf is the resources a request (its encoded frame) would leave.
type customOf func(ctx context.Context, in []byte) ([]customCheck, error)

// asResource is m in its REST JSON, unset scalars at their defaults and
// unset messages absent.
func asResource(m proto.Message) map[string]any {
	raw, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	dropNulls(out)
	return out
}

func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			dropNulls(x)
		}
	case []any:
		for _, x := range t {
			dropNulls(x)
		}
	}
}

// applyMask lays the fields paths name (by their first segment, proto or
// JSON name) from req over cur; an empty mask lays every field req sets.
func applyMask(cur, req proto.Message, paths []string) {
	c, r := cur.ProtoReflect(), req.ProtoReflect()
	fields := r.Descriptor().Fields()
	if len(paths) == 0 {
		r.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			c.Set(fd, v)
			return true
		})
		return
	}
	for _, p := range paths {
		top, _, _ := strings.Cut(p, ".")
		fd := fields.ByName(protoreflect.Name(top))
		if fd == nil {
			fd = fields.ByJSONName(top)
		}
		if fd == nil {
			continue
		}
		if r.Has(fd) {
			c.Set(fd, r.Get(fd))
		} else {
			c.Clear(fd)
		}
	}
}

// current is the resource Get method full answers for req.
func current[T proto.Message](ctx context.Context, f *Front, full string, req proto.Message) (T, error) {
	var zero T
	resp, err := f.call(ctx, full, req, zero.ProtoReflect().Descriptor())
	if err != nil {
		return zero, err
	}
	t, ok := resp.(T)
	if !ok {
		return zero, status.Errorf(codes.Internal, "%s answered %T", full, resp)
	}
	return t, nil
}

// updated is cur (from get) with the request's fields laid over it.
func updated[T proto.Message](ctx context.Context, f *Front, resourceType, get string, getReq proto.Message, in T, paths []string) ([]customCheck, error) {
	cur, err := current[T](ctx, f, get, getReq)
	if err != nil {
		return nil, nil // the update answers it
	}
	next := proto.Clone(cur)
	applyMask(next, in, paths)
	return []customCheck{{resourceType, orgpolicy.MethodUpdate, next}}, nil
}

func customDecode[Req proto.Message](mk func() Req, fn func(context.Context, Req) ([]customCheck, error)) customOf {
	return func(ctx context.Context, in []byte) ([]customCheck, error) {
		req := mk()
		if err := proto.Unmarshal(in, req); err != nil {
			return nil, nil // the method answers it
		}
		return fn(ctx, req)
	}
}

func created(resourceType string, m proto.Message) []customCheck {
	return []customCheck{{resourceType, orgpolicy.MethodCreate, m}}
}

// clone is a copy of m, or a new T when m is nil.
func clone[T proto.Message](m T, mk func() T) T {
	if m.ProtoReflect().IsValid() {
		return proto.Clone(m).(T)
	}
	return mk()
}

// customMethods are the create and update methods, and the resources each
// would leave.
func (f *Front) customMethods() map[string]customOf {
	ins, tbl := instanceAdmin, admin
	return map[string]customOf{
		ins + "CreateInstance": customDecode(func() *btapb.CreateInstanceRequest { return &btapb.CreateInstanceRequest{} },
			func(_ context.Context, req *btapb.CreateInstanceRequest) ([]customCheck, error) {
				name := req.GetParent() + "/instances/" + req.GetInstanceId()
				inst := clone(req.GetInstance(), func() *btapb.Instance { return &btapb.Instance{} })
				inst.Name = name
				out := created(orgpolicy.TypeBigtableInstance, inst)
				for _, id := range sortedKeys(req.GetClusters()) {
					c := proto.Clone(req.GetClusters()[id]).(*btapb.Cluster)
					c.Name = name + "/clusters/" + id
					out = append(out, created(orgpolicy.TypeBigtableCluster, c)...)
				}
				return out, nil
			}),
		ins + "UpdateInstance": customDecode(func() *btapb.Instance { return &btapb.Instance{} },
			func(ctx context.Context, in *btapb.Instance) ([]customCheck, error) {
				return updated(ctx, f, orgpolicy.TypeBigtableInstance, ins+"GetInstance", &btapb.GetInstanceRequest{Name: in.GetName()},
					in, []string{"display_name", "type", "labels"})
			}),
		ins + "PartialUpdateInstance": customDecode(func() *btapb.PartialUpdateInstanceRequest { return &btapb.PartialUpdateInstanceRequest{} },
			func(ctx context.Context, req *btapb.PartialUpdateInstanceRequest) ([]customCheck, error) {
				in := clone(req.GetInstance(), func() *btapb.Instance { return &btapb.Instance{} })
				return updated(ctx, f, orgpolicy.TypeBigtableInstance, ins+"GetInstance", &btapb.GetInstanceRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
		ins + "CreateCluster": customDecode(func() *btapb.CreateClusterRequest { return &btapb.CreateClusterRequest{} },
			func(_ context.Context, req *btapb.CreateClusterRequest) ([]customCheck, error) {
				c := clone(req.GetCluster(), func() *btapb.Cluster { return &btapb.Cluster{} })
				c.Name = req.GetParent() + "/clusters/" + req.GetClusterId()
				return created(orgpolicy.TypeBigtableCluster, c), nil
			}),
		ins + "UpdateCluster": customDecode(func() *btapb.Cluster { return &btapb.Cluster{} },
			func(ctx context.Context, in *btapb.Cluster) ([]customCheck, error) {
				return updated(ctx, f, orgpolicy.TypeBigtableCluster, ins+"GetCluster", &btapb.GetClusterRequest{Name: in.GetName()},
					in, []string{"serve_nodes"})
			}),
		ins + "PartialUpdateCluster": customDecode(func() *btapb.PartialUpdateClusterRequest { return &btapb.PartialUpdateClusterRequest{} },
			func(ctx context.Context, req *btapb.PartialUpdateClusterRequest) ([]customCheck, error) {
				in := clone(req.GetCluster(), func() *btapb.Cluster { return &btapb.Cluster{} })
				return updated(ctx, f, orgpolicy.TypeBigtableCluster, ins+"GetCluster", &btapb.GetClusterRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
		ins + "CreateAppProfile": customDecode(func() *btapb.CreateAppProfileRequest { return &btapb.CreateAppProfileRequest{} },
			func(_ context.Context, req *btapb.CreateAppProfileRequest) ([]customCheck, error) {
				p := clone(req.GetAppProfile(), func() *btapb.AppProfile { return &btapb.AppProfile{} })
				p.Name = req.GetParent() + "/appProfiles/" + req.GetAppProfileId()
				return created(orgpolicy.TypeBigtableAppProfile, p), nil
			}),
		ins + "UpdateAppProfile": customDecode(func() *btapb.UpdateAppProfileRequest { return &btapb.UpdateAppProfileRequest{} },
			func(ctx context.Context, req *btapb.UpdateAppProfileRequest) ([]customCheck, error) {
				in := clone(req.GetAppProfile(), func() *btapb.AppProfile { return &btapb.AppProfile{} })
				return updated(ctx, f, orgpolicy.TypeBigtableAppProfile, ins+"GetAppProfile", &btapb.GetAppProfileRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
		ins + "CreateLogicalView": customDecode(func() *btapb.CreateLogicalViewRequest { return &btapb.CreateLogicalViewRequest{} },
			func(_ context.Context, req *btapb.CreateLogicalViewRequest) ([]customCheck, error) {
				v := clone(req.GetLogicalView(), func() *btapb.LogicalView { return &btapb.LogicalView{} })
				v.Name = req.GetParent() + "/logicalViews/" + req.GetLogicalViewId()
				return created(orgpolicy.TypeBigtableLogicalView, v), nil
			}),
		ins + "UpdateLogicalView": customDecode(func() *btapb.UpdateLogicalViewRequest { return &btapb.UpdateLogicalViewRequest{} },
			func(ctx context.Context, req *btapb.UpdateLogicalViewRequest) ([]customCheck, error) {
				in := clone(req.GetLogicalView(), func() *btapb.LogicalView { return &btapb.LogicalView{} })
				return updated(ctx, f, orgpolicy.TypeBigtableLogicalView, ins+"GetLogicalView", &btapb.GetLogicalViewRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
		ins + "CreateMaterializedView": customDecode(func() *btapb.CreateMaterializedViewRequest { return &btapb.CreateMaterializedViewRequest{} },
			func(_ context.Context, req *btapb.CreateMaterializedViewRequest) ([]customCheck, error) {
				v := clone(req.GetMaterializedView(), func() *btapb.MaterializedView { return &btapb.MaterializedView{} })
				v.Name = req.GetParent() + "/materializedViews/" + req.GetMaterializedViewId()
				return created(orgpolicy.TypeBigtableMaterializedView, v), nil
			}),
		ins + "UpdateMaterializedView": customDecode(func() *btapb.UpdateMaterializedViewRequest { return &btapb.UpdateMaterializedViewRequest{} },
			func(ctx context.Context, req *btapb.UpdateMaterializedViewRequest) ([]customCheck, error) {
				in := clone(req.GetMaterializedView(), func() *btapb.MaterializedView { return &btapb.MaterializedView{} })
				return updated(ctx, f, orgpolicy.TypeBigtableMaterializedView, ins+"GetMaterializedView",
					&btapb.GetMaterializedViewRequest{Name: in.GetName()}, in, req.GetUpdateMask().GetPaths())
			}),
		tbl + "CreateTable": customDecode(func() *btapb.CreateTableRequest { return &btapb.CreateTableRequest{} },
			func(_ context.Context, req *btapb.CreateTableRequest) ([]customCheck, error) {
				t := clone(req.GetTable(), func() *btapb.Table { return &btapb.Table{} })
				t.Name = req.GetParent() + "/tables/" + req.GetTableId()
				return created(orgpolicy.TypeBigtableTable, t), nil
			}),
		tbl + "CreateTableFromSnapshot": customDecode(func() *btapb.CreateTableFromSnapshotRequest { return &btapb.CreateTableFromSnapshotRequest{} },
			func(_ context.Context, req *btapb.CreateTableFromSnapshotRequest) ([]customCheck, error) {
				return created(orgpolicy.TypeBigtableTable, &btapb.Table{Name: req.GetParent() + "/tables/" + req.GetTableId()}), nil
			}),
		tbl + "RestoreTable": customDecode(func() *btapb.RestoreTableRequest { return &btapb.RestoreTableRequest{} },
			func(_ context.Context, req *btapb.RestoreTableRequest) ([]customCheck, error) {
				return created(orgpolicy.TypeBigtableTable, &btapb.Table{Name: req.GetParent() + "/tables/" + req.GetTableId()}), nil
			}),
		tbl + "UpdateTable": customDecode(func() *btapb.UpdateTableRequest { return &btapb.UpdateTableRequest{} },
			func(ctx context.Context, req *btapb.UpdateTableRequest) ([]customCheck, error) {
				in := clone(req.GetTable(), func() *btapb.Table { return &btapb.Table{} })
				return updated(ctx, f, orgpolicy.TypeBigtableTable, tbl+"GetTable", &btapb.GetTableRequest{Name: in.GetName(), View: btapb.Table_FULL},
					in, req.GetUpdateMask().GetPaths())
			}),
		tbl + "CreateBackup": customDecode(func() *btapb.CreateBackupRequest { return &btapb.CreateBackupRequest{} },
			func(_ context.Context, req *btapb.CreateBackupRequest) ([]customCheck, error) {
				b := clone(req.GetBackup(), func() *btapb.Backup { return &btapb.Backup{} })
				b.Name = req.GetParent() + "/backups/" + req.GetBackupId()
				return created(orgpolicy.TypeBigtableBackup, b), nil
			}),
		tbl + "CopyBackup": customDecode(func() *btapb.CopyBackupRequest { return &btapb.CopyBackupRequest{} },
			func(ctx context.Context, req *btapb.CopyBackupRequest) ([]customCheck, error) {
				b, err := current[*btapb.Backup](ctx, f, tbl+"GetBackup", &btapb.GetBackupRequest{Name: req.GetSourceBackup()})
				if err != nil {
					return nil, nil // the copy answers it
				}
				b = proto.Clone(b).(*btapb.Backup)
				b.Name, b.ExpireTime = req.GetParent()+"/backups/"+req.GetBackupId(), req.GetExpireTime()
				return created(orgpolicy.TypeBigtableBackup, b), nil
			}),
		tbl + "UpdateBackup": customDecode(func() *btapb.UpdateBackupRequest { return &btapb.UpdateBackupRequest{} },
			func(ctx context.Context, req *btapb.UpdateBackupRequest) ([]customCheck, error) {
				in := clone(req.GetBackup(), func() *btapb.Backup { return &btapb.Backup{} })
				return updated(ctx, f, orgpolicy.TypeBigtableBackup, tbl+"GetBackup", &btapb.GetBackupRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
		tbl + "CreateSchemaBundle": customDecode(func() *btapb.CreateSchemaBundleRequest { return &btapb.CreateSchemaBundleRequest{} },
			func(_ context.Context, req *btapb.CreateSchemaBundleRequest) ([]customCheck, error) {
				sb := clone(req.GetSchemaBundle(), func() *btapb.SchemaBundle { return &btapb.SchemaBundle{} })
				sb.Name = req.GetParent() + "/schemaBundles/" + req.GetSchemaBundleId()
				return created(orgpolicy.TypeBigtableSchemaBundle, sb), nil
			}),
		tbl + "UpdateSchemaBundle": customDecode(func() *btapb.UpdateSchemaBundleRequest { return &btapb.UpdateSchemaBundleRequest{} },
			func(ctx context.Context, req *btapb.UpdateSchemaBundleRequest) ([]customCheck, error) {
				in := clone(req.GetSchemaBundle(), func() *btapb.SchemaBundle { return &btapb.SchemaBundle{} })
				return updated(ctx, f, orgpolicy.TypeBigtableSchemaBundle, tbl+"GetSchemaBundle", &btapb.GetSchemaBundleRequest{Name: in.GetName()},
					in, req.GetUpdateMask().GetPaths())
			}),
	}
}

// checkCustoms refuses the first resource of checks a custom constraint
// enforced on its project refuses.
func checkCustoms(ctx context.Context, checks []customCheck) error {
	for _, c := range checks {
		raw := c.m.ProtoReflect()
		name, _ := raw.Get(raw.Descriptor().Fields().ByName("name")).Interface().(string)
		project := orgpolicy.ProjectOf(name)
		if project == "" {
			continue
		}
		var v *orgpolicy.CustomViolation
		if errors.As(orgpolicy.CheckCustom(ctx, project, c.resourceType, c.method, asResource(c.m)), &v) {
			return status.Error(codes.FailedPrecondition, v.Singular())
		}
	}
	return nil
}

// wrapCustom checks each create and update the front serves against the
// custom constraints before it is served.
func (f *Front) wrapCustom() {
	for full, of := range f.customMethods() {
		serve, ok := f.served[full]
		if !ok {
			continue
		}
		f.served[full] = func(ctx context.Context, in []byte) (proto.Message, error) {
			if orgpolicy.SourceSet() {
				checks, _ := of(ctx, in)
				if err := checkCustoms(ctx, checks); err != nil {
					return nil, err
				}
			}
			return serve(ctx, in)
		}
	}
}
