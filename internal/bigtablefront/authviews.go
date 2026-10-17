package bigtablefront

import (
	"bytes"
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Authorized views (#1499). The emulator answers their admin methods
// UNIMPLEMENTED and ignores authorized_view_name on the data calls, so the
// front keeps the views, in memory beside their tables, and serves the data
// calls that name one on the view's table, limited to the view's subset:
//
//   - a read (ReadRows, and CheckAndMutateRow's predicate) is given a filter,
//     chained before the caller's, that passes only the cells of the view's
//     row prefixes, families and qualifiers, so it sees nothing else;
//   - a write (MutateRow, MutateRows, CheckAndMutateRow,
//     ReadModifyWriteRow) outside the subset is refused PERMISSION_DENIED,
//     as Google refuses it; deleting a whole row, or a family the view does
//     not wholly include, is outside it;
//   - SampleRowKeys is served on the table.
//
// Deleting a table deletes its views.

var authorizedViewIDRe = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]{0,49}$`)

type authViews struct {
	upstream *grpc.ClientConn
	mu       sync.Mutex
	byName   map[string]*btapb.AuthorizedView
}

func checkSubset(av *btapb.AuthorizedView) error {
	if av.GetSubsetView() == nil {
		return status.Error(codes.InvalidArgument, "authorized_view.subset_view is required")
	}
	return nil
}

func (v *authViews) create(ctx context.Context, req *btapb.CreateAuthorizedViewRequest, op func(string, proto.Message, proto.Message) (proto.Message, error)) (proto.Message, error) {
	start := time.Now().UTC()
	if !tableRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}/tables/{table}", req.GetParent())
	}
	if !authorizedViewIDRe.MatchString(req.GetAuthorizedViewId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid authorized_view_id %q", req.GetAuthorizedViewId())
	}
	av := proto.Clone(req.GetAuthorizedView()).(*btapb.AuthorizedView)
	if av == nil {
		av = &btapb.AuthorizedView{}
	}
	if err := checkSubset(av); err != nil {
		return nil, err
	}
	if _, err := btapb.NewBigtableTableAdminClient(v.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: req.GetParent(), View: btapb.Table_NAME_ONLY}); err != nil {
		return nil, err
	}
	av.Name = req.GetParent() + "/authorizedViews/" + req.GetAuthorizedViewId()
	av.Etag = newEtag()
	v.mu.Lock()
	if _, ok := v.byName[av.Name]; ok {
		v.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "AuthorizedView %s already exists.", av.Name)
	}
	v.byName[av.Name] = av
	v.mu.Unlock()
	return op(av.Name, &btapb.CreateAuthorizedViewMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start), FinishTime: timestamppb.Now()}, proto.Clone(av))
}

// lookup returns the view name names. v.mu is held.
func (v *authViews) lookup(name string) (*btapb.AuthorizedView, error) {
	if !authorizedViewRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid authorized view name %q: want projects/{project}/instances/{instance}/tables/{table}/authorizedViews/{authorized_view}", name)
	}
	av, ok := v.byName[name]
	if !ok {
		return nil, notFound("AuthorizedView", name)
	}
	return av, nil
}

// shaped is av as view asks for it.
func shaped(av *btapb.AuthorizedView, view btapb.AuthorizedView_ResponseView) *btapb.AuthorizedView {
	out := proto.Clone(av).(*btapb.AuthorizedView)
	if view == btapb.AuthorizedView_NAME_ONLY {
		return &btapb.AuthorizedView{Name: av.GetName()}
	}
	return out
}

func (v *authViews) get(_ context.Context, req *btapb.GetAuthorizedViewRequest) (proto.Message, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	av, err := v.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	return shaped(av, req.GetView()), nil
}

func (v *authViews) list(ctx context.Context, req *btapb.ListAuthorizedViewsRequest) (proto.Message, error) {
	if !tableRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}/tables/{table}", req.GetParent())
	}
	if _, err := btapb.NewBigtableTableAdminClient(v.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: req.GetParent(), View: btapb.Table_NAME_ONLY}); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var names []string
	for n := range v.byName {
		if strings.HasPrefix(n, req.GetParent()+"/authorizedViews/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start, end, next, err := page(len(names), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	view := req.GetView()
	if view == btapb.AuthorizedView_RESPONSE_VIEW_UNSPECIFIED {
		view = btapb.AuthorizedView_NAME_ONLY
	}
	out := &btapb.ListAuthorizedViewsResponse{NextPageToken: next}
	for _, n := range names[start:end] {
		out.AuthorizedViews = append(out.AuthorizedViews, shaped(v.byName[n], view))
	}
	return out, nil
}

func (v *authViews) update(_ context.Context, req *btapb.UpdateAuthorizedViewRequest, op func(string, proto.Message, proto.Message) (proto.Message, error)) (proto.Message, error) {
	start := time.Now().UTC()
	upd := req.GetAuthorizedView()
	if upd == nil {
		return nil, status.Error(codes.InvalidArgument, "authorized_view is required")
	}
	v.mu.Lock()
	av, err := v.lookup(upd.GetName())
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}
	if e := upd.GetEtag(); e != "" && e != av.GetEtag() {
		v.mu.Unlock()
		return nil, status.Error(codes.Aborted, "the etag does not match the authorized view's current etag")
	}
	next := proto.Clone(av).(*btapb.AuthorizedView)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"subset_view", "deletion_protection"}
	}
	for _, p := range paths {
		switch p {
		case "subset_view":
			if err := checkSubset(upd); err != nil {
				v.mu.Unlock()
				return nil, err
			}
			next.AuthorizedView = proto.Clone(upd).(*btapb.AuthorizedView).AuthorizedView
		case "deletion_protection":
			next.DeletionProtection = upd.GetDeletionProtection()
		default:
			v.mu.Unlock()
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not updatable: want subset_view or deletion_protection", p)
		}
	}
	next.Etag = newEtag()
	v.byName[next.Name] = next
	v.mu.Unlock()
	return op(next.Name, &btapb.UpdateAuthorizedViewMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start), FinishTime: timestamppb.Now()}, proto.Clone(next))
}

func (v *authViews) delete(_ context.Context, req *btapb.DeleteAuthorizedViewRequest) (proto.Message, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	av, err := v.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	if e := req.GetEtag(); e != "" && e != av.GetEtag() {
		return nil, status.Error(codes.Aborted, "the etag does not match the authorized view's current etag")
	}
	if av.GetDeletionProtection() {
		return nil, status.Errorf(codes.FailedPrecondition, "AuthorizedView %s is protected against deletion", req.GetName())
	}
	delete(v.byName, req.GetName())
	return &emptypb.Empty{}, nil
}

// exists reports whether the view name exists.
func (v *authViews) exists(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.byName[name]
	return ok
}

// drop forgets the views of resource, a table or an instance.
func (v *authViews) drop(resource string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for n := range v.byName {
		if strings.HasPrefix(n, resource+"/") {
			delete(v.byName, n)
		}
	}
}

// --- the data plane ---

// viewMethods are the data methods that can name an authorized view.
var viewMethods = map[string]bool{
	data + "ReadRows": true, data + "SampleRowKeys": true, data + "MutateRow": true, data + "MutateRows": true,
	data + "CheckAndMutateRow": true, data + "ReadModifyWriteRow": true,
}

// subset is a view's subset, as the data plane checks it.
type subset struct {
	sv *btapb.AuthorizedView_SubsetView
}

func (s subset) row(key []byte) bool {
	for _, p := range s.sv.GetRowPrefixes() {
		if bytes.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func (s subset) column(family string, qualifier []byte) bool {
	f, ok := s.sv.GetFamilySubsets()[family]
	if !ok {
		return false
	}
	for _, q := range f.GetQualifiers() {
		if bytes.Equal(q, qualifier) {
			return true
		}
	}
	for _, p := range f.GetQualifierPrefixes() {
		if bytes.HasPrefix(qualifier, p) {
			return true
		}
	}
	return false
}

// family reports whether the view holds every qualifier of family.
func (s subset) family(family string) bool {
	for _, p := range s.sv.GetFamilySubsets()[family].GetQualifierPrefixes() {
		if len(p) == 0 {
			return true
		}
	}
	return false
}

func quote(b []byte) []byte { return []byte(regexp.QuoteMeta(string(b))) }

func alternatives(exact, prefixes [][]byte) []byte {
	var alts [][]byte
	for _, q := range exact {
		alts = append(alts, quote(q))
	}
	for _, p := range prefixes {
		alts = append(alts, append(quote(p), []byte(".*")...))
	}
	return append(append([]byte("(?s)^(?:"), bytes.Join(alts, []byte("|"))...), []byte(")$")...)
}

// filter is the RowFilter that passes the subset's cells alone.
func (s subset) filter() *btpb.RowFilter {
	block := &btpb.RowFilter{Filter: &btpb.RowFilter_BlockAllFilter{BlockAllFilter: true}}
	if len(s.sv.GetRowPrefixes()) == 0 || len(s.sv.GetFamilySubsets()) == 0 {
		return block
	}
	var fams []*btpb.RowFilter
	ids := make([]string, 0, len(s.sv.GetFamilySubsets()))
	for id := range s.sv.GetFamilySubsets() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := s.sv.GetFamilySubsets()[id]
		if len(f.GetQualifiers())+len(f.GetQualifierPrefixes()) == 0 {
			continue
		}
		fams = append(fams, &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: []*btpb.RowFilter{
			{Filter: &btpb.RowFilter_FamilyNameRegexFilter{FamilyNameRegexFilter: "^" + regexp.QuoteMeta(id) + "$"}},
			{Filter: &btpb.RowFilter_ColumnQualifierRegexFilter{ColumnQualifierRegexFilter: alternatives(f.GetQualifiers(), f.GetQualifierPrefixes())}},
		}}}})
	}
	if len(fams) == 0 {
		return block
	}
	columns := fams[0]
	if len(fams) > 1 {
		// Google's Interleave, and the emulator's, takes two or more.
		columns = &btpb.RowFilter{Filter: &btpb.RowFilter_Interleave_{Interleave: &btpb.RowFilter_Interleave{Filters: fams}}}
	}
	return &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: []*btpb.RowFilter{
		{Filter: &btpb.RowFilter_RowKeyRegexFilter{RowKeyRegexFilter: alternatives(nil, s.sv.GetRowPrefixes())}},
		columns,
	}}}}
}

// limit chains the subset's filter before user's.
func (s subset) limit(user *btpb.RowFilter) *btpb.RowFilter {
	f := s.filter()
	if user == nil {
		return f
	}
	return &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: []*btpb.RowFilter{f, user}}}}
}

func denied(name string) error {
	return status.Errorf(codes.PermissionDenied, "the request is outside the authorized view %s", name)
}

// mutations checks that ms on row key are within the subset.
func (s subset) mutations(name string, key []byte, ms []*btpb.Mutation) error {
	if !s.row(key) {
		return denied(name)
	}
	for _, m := range ms {
		ok := true
		switch x := m.GetMutation().(type) {
		case *btpb.Mutation_SetCell_:
			ok = s.column(x.SetCell.GetFamilyName(), x.SetCell.GetColumnQualifier())
		case *btpb.Mutation_DeleteFromColumn_:
			ok = s.column(x.DeleteFromColumn.GetFamilyName(), x.DeleteFromColumn.GetColumnQualifier())
		case *btpb.Mutation_AddToCell_:
			ok = s.column(x.AddToCell.GetFamilyName(), x.AddToCell.GetColumnQualifier().GetRawValue())
		case *btpb.Mutation_MergeToCell_:
			ok = s.column(x.MergeToCell.GetFamilyName(), x.MergeToCell.GetColumnQualifier().GetRawValue())
		case *btpb.Mutation_DeleteFromFamily_:
			ok = s.family(x.DeleteFromFamily.GetFamilyName())
		default:
			ok = false
		}
		if !ok {
			return denied(name)
		}
	}
	return nil
}

// viewTable is the table of an authorized view's name.
func viewTable(name string) string { return name[:strings.Index(name, "/authorizedViews/")] }

// subsetOf returns the subset of the view name names.
func (v *authViews) subsetOf(name string) (subset, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	av, err := v.lookup(name)
	if err != nil {
		return subset{}, err
	}
	return subset{sv: proto.Clone(av.GetSubsetView()).(*btapb.AuthorizedView_SubsetView)}, nil
}

// rewrite turns a data request naming an authorized view into one on its
// table limited to the view, and returns any other unchanged.
func (v *authViews) rewrite(method string, in frame) (frame, error) {
	var req interface {
		proto.Message
		GetAuthorizedViewName() string
	}
	switch method {
	case data + "ReadRows":
		req = &btpb.ReadRowsRequest{}
	case data + "SampleRowKeys":
		req = &btpb.SampleRowKeysRequest{}
	case data + "MutateRow":
		req = &btpb.MutateRowRequest{}
	case data + "MutateRows":
		req = &btpb.MutateRowsRequest{}
	case data + "CheckAndMutateRow":
		req = &btpb.CheckAndMutateRowRequest{}
	case data + "ReadModifyWriteRow":
		req = &btpb.ReadModifyWriteRowRequest{}
	default:
		return in, nil
	}
	if err := proto.Unmarshal(in, req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
	}
	name := req.GetAuthorizedViewName()
	if name == "" {
		return in, nil
	}
	s, err := v.subsetOf(name)
	if err != nil {
		return nil, err
	}
	table := viewTable(name)
	switch r := req.(type) {
	case *btpb.ReadRowsRequest:
		r.AuthorizedViewName, r.TableName = "", table
		r.Filter = s.limit(r.GetFilter())
	case *btpb.SampleRowKeysRequest:
		r.AuthorizedViewName, r.TableName = "", table
	case *btpb.MutateRowRequest:
		if err := s.mutations(name, r.GetRowKey(), r.GetMutations()); err != nil {
			return nil, err
		}
		r.AuthorizedViewName, r.TableName = "", table
	case *btpb.MutateRowsRequest:
		for _, e := range r.GetEntries() {
			if err := s.mutations(name, e.GetRowKey(), e.GetMutations()); err != nil {
				return nil, err
			}
		}
		r.AuthorizedViewName, r.TableName = "", table
	case *btpb.CheckAndMutateRowRequest:
		if err := s.mutations(name, r.GetRowKey(), r.GetTrueMutations()); err != nil {
			return nil, err
		}
		if err := s.mutations(name, r.GetRowKey(), r.GetFalseMutations()); err != nil {
			return nil, err
		}
		r.AuthorizedViewName, r.TableName = "", table
		r.PredicateFilter = s.limit(r.GetPredicateFilter())
	case *btpb.ReadModifyWriteRowRequest:
		if !s.row(r.GetRowKey()) {
			return nil, denied(name)
		}
		for _, rule := range r.GetRules() {
			if !s.column(rule.GetFamilyName(), rule.GetColumnQualifier()) {
				return nil, denied(name)
			}
		}
		r.AuthorizedViewName, r.TableName = "", table
	}
	return proto.Marshal(req)
}
