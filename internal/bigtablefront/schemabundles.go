package bigtablefront

import (
	"context"
	"fmt"
	"regexp"
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
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Schema bundles (#1499): a table's protobuf schemas, which Google's SQL
// reads PROTO columns with. The emulator has none of the five methods (it
// would panic on them), so the front keeps the bundles beside their table,
// which must exist, and drops them with it. proto_descriptors must be a
// serialized google.protobuf.FileDescriptorSet whose files resolve, as
// Google checks. SQL reads them: a CAST to bundle_id.package.Message
// (sqlproto.go, #2113).
//
// "Create and manage protobuf schemas" (docs read 2026-11-21:
// https://docs.cloud.google.com/bigtable/docs/create-manage-protobuf-schemas)
// sets the rules kept here: an ID of 1 to 50 letters, numbers, underscores
// and hyphens, not starting with a hyphen and without a period; at most
// 4 MB of serialized descriptors; up to 10 bundles per table; an update
// whose descriptors are not backward compatible fails FAILED_PRECONDITION
// unless ignore_warnings is set, and never when a logical or materialized
// view uses the bundle; and a bundle a view uses cannot be deleted.

var schemaBundleIDRe = regexp.MustCompile(`^[_a-zA-Z0-9][-_a-zA-Z0-9]{0,49}$`)

const (
	maxBundleBytes     = 4 << 20
	maxBundlesPerTable = 10
)

type schemaBundles struct {
	upstream *grpc.ClientConn
	mu       sync.Mutex
	byName   map[string]*btapb.SchemaBundle
	// usedBy is the logical or materialized view whose query reads the
	// bundle, if any (sqlviews.go).
	usedBy func(bundle string) string
}

// descriptors is the serialized descriptor set of each of table's
// bundles, by bundle ID.
func (b *schemaBundles) descriptors(table string) map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string][]byte{}
	for n, sb := range b.byName {
		if id, ok := strings.CutPrefix(n, table+"/schemaBundles/"); ok {
			out[id] = sb.GetProtoSchema().GetProtoDescriptors()
		}
	}
	return out
}

// incompatible is why next's descriptors are not backward compatible with
// prev's, or "": a message or enum went away, or a field number changed
// its name, type or cardinality, or went away without being reserved, or
// an enum number went away.
func incompatible(prev, next []byte) string {
	files := func(raw []byte) *protoregistry.Files {
		var set descriptorpb.FileDescriptorSet
		if proto.Unmarshal(raw, &set) != nil {
			return nil
		}
		f, _ := protodesc.NewFiles(&set)
		return f
	}
	pf, nf := files(prev), files(next)
	if pf == nil || nf == nil {
		return ""
	}
	why := ""
	pf.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		var walkMsgs func(ms protoreflect.MessageDescriptors) bool
		var walkEnums func(es protoreflect.EnumDescriptors) bool
		walkEnums = func(es protoreflect.EnumDescriptors) bool {
			for i := 0; i < es.Len(); i++ {
				e := es.Get(i)
				d, err := nf.FindDescriptorByName(e.FullName())
				ne, ok := d.(protoreflect.EnumDescriptor)
				if err != nil || !ok {
					why = "enum " + string(e.FullName()) + " was removed"
					return false
				}
				for j := 0; j < e.Values().Len(); j++ {
					v := e.Values().Get(j)
					if ne.Values().ByNumber(v.Number()) == nil && !ne.ReservedRanges().Has(v.Number()) {
						why = fmt.Sprintf("enum %s value %d was removed", e.FullName(), v.Number())
						return false
					}
				}
			}
			return true
		}
		walkMsgs = func(ms protoreflect.MessageDescriptors) bool {
			for i := 0; i < ms.Len(); i++ {
				m := ms.Get(i)
				if m.IsMapEntry() {
					continue
				}
				d, err := nf.FindDescriptorByName(m.FullName())
				nm, ok := d.(protoreflect.MessageDescriptor)
				if err != nil || !ok {
					why = "message " + string(m.FullName()) + " was removed"
					return false
				}
				for j := 0; j < m.Fields().Len(); j++ {
					f := m.Fields().Get(j)
					g := nm.Fields().ByNumber(f.Number())
					switch {
					case g == nil && nm.ReservedRanges().Has(f.Number()):
					case g == nil:
						why = fmt.Sprintf("field %d (%s) of %s was removed without being reserved", f.Number(), f.Name(), m.FullName())
					case g.Name() != f.Name():
						why = fmt.Sprintf("field %d of %s was renamed from %s to %s", f.Number(), m.FullName(), f.Name(), g.Name())
					case g.Kind() != f.Kind() || g.Cardinality() != f.Cardinality() || g.IsMap() != f.IsMap():
						why = fmt.Sprintf("field %d (%s) of %s changed its type", f.Number(), f.Name(), m.FullName())
					case f.Message() != nil && g.Message().FullName() != f.Message().FullName(),
						f.Enum() != nil && g.Enum().FullName() != f.Enum().FullName():
						why = fmt.Sprintf("field %d (%s) of %s changed its type", f.Number(), f.Name(), m.FullName())
					}
					if why != "" {
						return false
					}
				}
				if !walkMsgs(m.Messages()) || !walkEnums(m.Enums()) {
					return false
				}
			}
			return true
		}
		return walkMsgs(fd.Messages()) && walkEnums(fd.Enums())
	})
	return why
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
	if len(ps.GetProtoDescriptors()) > maxBundleBytes {
		return status.Errorf(codes.InvalidArgument, "proto_schema.proto_descriptors is %d bytes: the serialized descriptors of a schema bundle can't exceed 4 MB", len(ps.GetProtoDescriptors()))
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
	if !schemaBundleIDRe.MatchString(req.GetSchemaBundleId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid schema_bundle_id %q: it must be 1 to 50 letters, numbers, underscores and hyphens, not starting with a hyphen, and cannot contain a period", req.GetSchemaBundleId())
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
	n := 0
	for name := range b.byName {
		if strings.HasPrefix(name, req.GetParent()+"/schemaBundles/") {
			n++
		}
	}
	if n >= maxBundlesPerTable {
		b.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "table %s already has %d schema bundles, the most a table can have", req.GetParent(), maxBundlesPerTable)
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
	if why := incompatible(sb.GetProtoSchema().GetProtoDescriptors(), next.GetProtoSchema().GetProtoDescriptors()); why != "" {
		view := ""
		if b.usedBy != nil {
			view = b.usedBy(next.Name)
		}
		switch {
		case view != "":
			b.mu.Unlock()
			return nil, status.Errorf(codes.FailedPrecondition, "the new schema bundle is not backward compatible (%s), and %s uses the schema bundle: update or delete the view first", why, view)
		case !req.GetIgnoreWarnings():
			b.mu.Unlock()
			return nil, status.Errorf(codes.FailedPrecondition, "the new schema bundle is not backward compatible with the existing one: %s; set ignore_warnings to update it anyway", why)
		}
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
	if b.usedBy != nil {
		if view := b.usedBy(req.GetName()); view != "" {
			return nil, status.Errorf(codes.FailedPrecondition, "schema bundle %s is in use by %s: delete the view, or update it to remove the reference, first", req.GetName(), view)
		}
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
