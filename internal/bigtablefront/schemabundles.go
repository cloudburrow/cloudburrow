package bigtablefront

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Schema bundles (#1499): a table's protobuf schemas, which Google's SQL
// reads PROTO columns with. The emulator has none of the five methods (it
// would panic on them), so the front keeps the bundles beside their table,
// which must exist, and drops them with it. proto_descriptors must be a
// serialized google.protobuf.FileDescriptorSet whose files resolve, as
// Google checks; nothing reads the bundles, since SQL is not served.

type schemaBundles struct {
	upstream *grpc.ClientConn
	mu       sync.Mutex
	byName   map[string]*btapb.SchemaBundle
}

func (b *schemaBundles) methods(op func(string, proto.Message, proto.Message) (proto.Message, error)) map[string]method {
	return map[string]method{
		admin + "CreateSchemaBundle": unary(func() *btapb.CreateSchemaBundleRequest { return &btapb.CreateSchemaBundleRequest{} },
			func(ctx context.Context, req *btapb.CreateSchemaBundleRequest) (proto.Message, error) {
				return b.create(ctx, req, op)
			}),
		admin + "GetSchemaBundle":   unary(func() *btapb.GetSchemaBundleRequest { return &btapb.GetSchemaBundleRequest{} }, b.get),
		admin + "ListSchemaBundles": unary(func() *btapb.ListSchemaBundlesRequest { return &btapb.ListSchemaBundlesRequest{} }, b.list),
		admin + "UpdateSchemaBundle": unary(func() *btapb.UpdateSchemaBundleRequest { return &btapb.UpdateSchemaBundleRequest{} },
			func(ctx context.Context, req *btapb.UpdateSchemaBundleRequest) (proto.Message, error) {
				return b.update(ctx, req, op)
			}),
		admin + "DeleteSchemaBundle": unary(func() *btapb.DeleteSchemaBundleRequest { return &btapb.DeleteSchemaBundleRequest{} }, b.delete),
	}
}

// checkDescriptors checks a bundle's proto_schema.
func checkDescriptors(sb *btapb.SchemaBundle) error {
	ps := sb.GetProtoSchema()
	if ps == nil {
		return status.Error(codes.InvalidArgument, "schema_bundle.proto_schema is required")
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(ps.GetProtoDescriptors(), &set); err != nil {
		return status.Errorf(codes.InvalidArgument, "proto_schema.proto_descriptors is not a FileDescriptorSet: %v", err)
	}
	if len(set.GetFile()) == 0 {
		return status.Error(codes.InvalidArgument, "proto_schema.proto_descriptors holds no file")
	}
	if _, err := protodesc.NewFiles(&set); err != nil {
		return status.Errorf(codes.InvalidArgument, "proto_schema.proto_descriptors: %v", err)
	}
	return nil
}

func (b *schemaBundles) table(ctx context.Context, name string) error {
	if !tableRe.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}/tables/{table}", name)
	}
	_, err := btapb.NewBigtableTableAdminClient(b.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_NAME_ONLY})
	return err
}

func (b *schemaBundles) create(ctx context.Context, req *btapb.CreateSchemaBundleRequest, op func(string, proto.Message, proto.Message) (proto.Message, error)) (proto.Message, error) {
	start := time.Now()
	if err := b.table(ctx, req.GetParent()); err != nil {
		return nil, err
	}
	if !backupIDRe.MatchString(req.GetSchemaBundleId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid schema_bundle_id %q", req.GetSchemaBundleId())
	}
	sb := proto.Clone(req.GetSchemaBundle()).(*btapb.SchemaBundle)
	if sb == nil {
		sb = &btapb.SchemaBundle{}
	}
	if err := checkDescriptors(sb); err != nil {
		return nil, err
	}
	sb.Name = req.GetParent() + "/schemaBundles/" + req.GetSchemaBundleId()
	sb.Etag = newEtag()
	b.mu.Lock()
	if _, ok := b.byName[sb.Name]; ok {
		b.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "SchemaBundle %s already exists.", sb.Name)
	}
	b.byName[sb.Name] = sb
	b.mu.Unlock()
	return op(sb.Name, &btapb.CreateSchemaBundleMetadata{Name: sb.Name, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, proto.Clone(sb))
}

// lookup returns the bundle name names. b.mu is held.
func (b *schemaBundles) lookup(name string) (*btapb.SchemaBundle, error) {
	i := strings.Index(name, "/schemaBundles/")
	if i < 0 || !tableRe.MatchString(name[:i]) || strings.Contains(name[i+len("/schemaBundles/"):], "/") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid schema bundle name %q: want projects/{project}/instances/{instance}/tables/{table}/schemaBundles/{schema_bundle}", name)
	}
	sb, ok := b.byName[name]
	if !ok {
		return nil, notFound("SchemaBundle", name)
	}
	return sb, nil
}

func (b *schemaBundles) get(_ context.Context, req *btapb.GetSchemaBundleRequest) (proto.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, err := b.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(sb), nil
}

func (b *schemaBundles) list(ctx context.Context, req *btapb.ListSchemaBundlesRequest) (proto.Message, error) {
	if err := b.table(ctx, req.GetParent()); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var names []string
	for n := range b.byName {
		if strings.HasPrefix(n, req.GetParent()+"/schemaBundles/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start, end, next, err := page(len(names), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	out := &btapb.ListSchemaBundlesResponse{NextPageToken: next}
	for _, n := range names[start:end] {
		out.SchemaBundles = append(out.SchemaBundles, proto.Clone(b.byName[n]).(*btapb.SchemaBundle))
	}
	return out, nil
}

func (b *schemaBundles) update(_ context.Context, req *btapb.UpdateSchemaBundleRequest, op func(string, proto.Message, proto.Message) (proto.Message, error)) (proto.Message, error) {
	start := time.Now()
	upd := req.GetSchemaBundle()
	if upd == nil {
		return nil, status.Error(codes.InvalidArgument, "schema_bundle is required")
	}
	b.mu.Lock()
	sb, err := b.lookup(upd.GetName())
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if e := upd.GetEtag(); e != "" && e != sb.GetEtag() {
		b.mu.Unlock()
		return nil, status.Error(codes.Aborted, "the etag does not match the schema bundle's current etag")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"proto_schema"}
	}
	next := proto.Clone(sb).(*btapb.SchemaBundle)
	for _, p := range paths {
		if p != "proto_schema" && p != "proto_schema.proto_descriptors" {
			b.mu.Unlock()
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not updatable: want proto_schema", p)
		}
		if err := checkDescriptors(upd); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		next.Type = proto.Clone(upd).(*btapb.SchemaBundle).Type
	}
	next.Etag = newEtag()
	b.byName[next.Name] = next
	b.mu.Unlock()
	return op(next.Name, &btapb.UpdateSchemaBundleMetadata{Name: next.Name, StartTime: timestamppb.New(start), EndTime: timestamppb.Now()}, proto.Clone(next))
}

func (b *schemaBundles) delete(_ context.Context, req *btapb.DeleteSchemaBundleRequest) (proto.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb, err := b.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	if e := req.GetEtag(); e != "" && e != sb.GetEtag() {
		return nil, status.Error(codes.Aborted, "the etag does not match the schema bundle's current etag")
	}
	delete(b.byName, req.GetName())
	return &emptypb.Empty{}, nil
}

// drop forgets the bundles of resource, a table or an instance.
func (b *schemaBundles) drop(resource string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for n := range b.byName {
		if strings.HasPrefix(n, resource+"/") {
			delete(b.byName, n)
		}
	}
}
