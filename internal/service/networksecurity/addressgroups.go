package networksecurity

import (
	"context"
	"net"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Address groups (#2160), as https://docs.cloud.google.com/armor/docs/address-groups-overview,
// .../armor/quotas and the AddressGroupService and
// OrganizationAddressGroupService references describe them (read
// 2026-11-22):
//
//   - a group is project-scoped (projects/P/locations/L/addressGroups/N)
//     or organization-scoped (organizations/O/...); its type, IPV4 or
//     IPV6, and its capacity are set at creation and cannot change;
//   - each item is an IP address or CIDR range of the group's type, and
//     the items never exceed the capacity;
//   - a group whose purposes do not include CLOUD_ARMOR has a capacity of
//     at most 1,000; one for Cloud Armor at most 150,000 IPv4 or 50,000
//     IPv6 ranges, and one call adds or removes at most 50,000 IPv4 or
//     20,000 IPv6 ranges;
//   - Cloud Armor rules match it with evaluateAddressGroup (project
//     groups, in the policy's own project) and
//     evaluateOrganizationAddressGroup; ListAddressGroupReferences names
//     the security policies and rules that do, and a group in use cannot
//     be deleted.

// The documented limits (armor/quotas, address-groups-overview).
const (
	maxDefaultCapacity = 1000
	maxArmorIPv4       = 150000
	maxArmorIPv6       = 50000
	maxChangeIPv4      = 50000
	maxChangeIPv6      = 20000
)

// AddressGroupReference is a security policy rule that matches an
// address group.
type AddressGroupReference struct {
	SecurityPolicy string
	RulePriority   int32
}

// addressGroups is the AddressGroupService over the Server.
type addressGroups struct {
	nspb.UnimplementedAddressGroupServiceServer
	s *Server
}

// orgAddressGroups is the OrganizationAddressGroupService: the same
// methods, on organizations/O/locations/L.
type orgAddressGroups struct {
	orgUnimplemented
	*addressGroups
}

// orgUnimplemented holds the generated base a level down, so the
// addressGroups methods win.
type orgUnimplemented struct {
	nspb.UnimplementedOrganizationAddressGroupServiceServer
}

var _ nspb.OrganizationAddressGroupServiceServer = (*orgAddressGroups)(nil)

// normItem is an item as Google keeps it: a CIDR range with its prefix
// length, the address canonical; ok is false for anything else or the
// wrong family.
func normItem(item string, typ nspb.AddressGroup_Type) (string, bool) {
	item = strings.TrimSpace(item)
	var ip net.IP
	bits := -1
	if strings.Contains(item, "/") {
		a, n, err := net.ParseCIDR(item)
		if err != nil {
			return "", false
		}
		ip = a
		bits, _ = n.Mask.Size()
		if !a.Equal(n.IP) {
			return "", false
		}
	} else {
		ip = net.ParseIP(item)
	}
	if ip == nil {
		return "", false
	}
	v4 := ip.To4() != nil && !strings.Contains(item, ":")
	if typ == nspb.AddressGroup_IPV4 && !v4 || typ == nspb.AddressGroup_IPV6 && v4 {
		return "", false
	}
	if bits < 0 {
		return ip.String(), true
	}
	return ip.String() + "/" + itoa(bits), true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func hasArmorPurpose(g *nspb.AddressGroup) bool {
	for _, p := range g.GetPurpose() {
		if p == nspb.AddressGroup_CLOUD_ARMOR {
			return true
		}
	}
	return false
}

// maxCapacity is a group's largest capacity.
func maxCapacity(g *nspb.AddressGroup) int32 {
	if !hasArmorPurpose(g) {
		return maxDefaultCapacity
	}
	if g.GetType() == nspb.AddressGroup_IPV6 {
		return maxArmorIPv6
	}
	return maxArmorIPv4
}

func maxChange(g *nspb.AddressGroup) int {
	if g.GetType() == nspb.AddressGroup_IPV6 {
		return maxChangeIPv6
	}
	return maxChangeIPv4
}

// normItems checks and normalizes items for g, without duplicates.
func normItems(field string, items []string, g *nspb.AddressGroup) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for i, it := range items {
		n, ok := normItem(it, g.GetType())
		if !ok {
			return nil, apierror.InvalidArgument("Invalid value for field '%s[%d]': '%s'. It must be an %s address or range in CIDR notation, matching the address group's type.", field, i, it, g.GetType())
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// checkAddressGroup checks a new or updated group.
func checkAddressGroup(old *nspb.AddressGroup) func(m proto.Message, _ string) error {
	return func(m proto.Message, _ string) error {
		g := m.(*nspb.AddressGroup)
		if g.GetType() == nspb.AddressGroup_TYPE_UNSPECIFIED {
			return apierror.InvalidArgument("Invalid argument: type is required: IPV4 or IPV6.")
		}
		if g.GetCapacity() <= 0 {
			return apierror.InvalidArgument("Invalid argument: capacity is required and must be positive.")
		}
		if old != nil {
			if g.GetType() != old.GetType() {
				return apierror.InvalidArgument("Invalid argument: the type of an address group cannot be changed.")
			}
			if g.GetCapacity() != old.GetCapacity() {
				return apierror.InvalidArgument("Invalid argument: the capacity of an address group cannot be changed.")
			}
		}
		for _, p := range g.GetPurpose() {
			if p == nspb.AddressGroup_PURPOSE_UNSPECIFIED {
				return apierror.InvalidArgument("Invalid argument: purpose must be DEFAULT or CLOUD_ARMOR.")
			}
		}
		if mc := maxCapacity(g); g.GetCapacity() > mc {
			return apierror.InvalidArgument("Invalid argument: capacity %d exceeds the maximum of %d for this address group's type and purpose.", g.GetCapacity(), mc)
		}
		items, err := normItems("address_group.items", g.GetItems(), g)
		if err != nil {
			return err
		}
		if len(items) > int(g.GetCapacity()) {
			return apierror.InvalidArgument("Invalid argument: the address group has %d items, more than its capacity of %d.", len(items), g.GetCapacity())
		}
		g.Items = items
		return nil
	}
}

func addressGroupSelfLink(name string) string {
	return "https://networksecurity.googleapis.com/v1/" + name
}

// CreateAddressGroup creates a group under a project's or an
// organization's location.
func (a *addressGroups) CreateAddressGroup(_ context.Context, req *nspb.CreateAddressGroupRequest) (*longrunningpb.Operation, error) {
	g, _ := proto.Clone(req.GetAddressGroup()).(*nspb.AddressGroup)
	if g == nil {
		g = &nspb.AddressGroup{}
	}
	g.SelfLink = addressGroupSelfLink(req.GetParent() + "/" + collAddrGroup + "/" + req.GetAddressGroupId())
	return a.s.create(req.GetParent(), collAddrGroup, "address_group_id", req.GetAddressGroupId(), g, checkAddressGroup(nil))
}

func (a *addressGroups) GetAddressGroup(_ context.Context, req *nspb.GetAddressGroupRequest) (*nspb.AddressGroup, error) {
	m, err := a.s.get(collAddrGroup, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.AddressGroup), nil
}

func (a *addressGroups) ListAddressGroups(_ context.Context, req *nspb.ListAddressGroupsRequest) (*nspb.ListAddressGroupsResponse, error) {
	l, err := a.s.list(req.GetParent(), collAddrGroup)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.AddressGroup, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.AddressGroup)
	}
	page, next, err := aipfilter.ListPage("ag:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListAddressGroupsResponse{AddressGroups: page, NextPageToken: next}, nil
}

func (a *addressGroups) UpdateAddressGroup(_ context.Context, req *nspb.UpdateAddressGroupRequest) (*longrunningpb.Operation, error) {
	name := req.GetAddressGroup().GetName()
	a.s.mu.Lock()
	old, _ := a.s.res[name].(*nspb.AddressGroup)
	a.s.mu.Unlock()
	if old != nil {
		old = proto.Clone(old).(*nspb.AddressGroup)
	}
	return a.s.update(collAddrGroup, req.GetAddressGroup(), req.GetUpdateMask(),
		[]string{"description", "labels", "type", "items", "capacity", "purpose"}, checkAddressGroup(old))
}

// changeItems applies f to a group's items under the lock, checks the
// result, and answers the operation.
func (a *addressGroups) changeItems(name string, n int, f func(g *nspb.AddressGroup) error) (*longrunningpb.Operation, error) {
	if err := checkName(name, collAddrGroup); err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	cur, ok := a.s.res[name].(*nspb.AddressGroup)
	if !ok {
		return nil, notFound(collAddrGroup, name)
	}
	if n == 0 {
		return nil, apierror.InvalidArgument("Invalid argument: items is required.")
	}
	if n > maxChange(cur) {
		return nil, apierror.InvalidArgument("Invalid argument: at most %d %s ranges can be changed in one call.", maxChange(cur), cur.GetType())
	}
	next := proto.Clone(cur).(*nspb.AddressGroup)
	if err := f(next); err != nil {
		return nil, err
	}
	if len(next.GetItems()) > int(next.GetCapacity()) {
		return nil, apierror.InvalidArgument("Invalid argument: the address group would have %d items, more than its capacity of %d.", len(next.GetItems()), next.GetCapacity())
	}
	stamp(next, a.s.opts.Now(), false)
	a.s.res[name] = next
	return a.s.opLocked(name, "update", next)
}

func (a *addressGroups) AddAddressGroupItems(_ context.Context, req *nspb.AddAddressGroupItemsRequest) (*longrunningpb.Operation, error) {
	return a.changeItems(req.GetAddressGroup(), len(req.GetItems()), func(g *nspb.AddressGroup) error {
		add, err := normItems("items", req.GetItems(), g)
		if err != nil {
			return err
		}
		g.Items = mergeItems(g.GetItems(), add)
		return nil
	})
}

func (a *addressGroups) RemoveAddressGroupItems(_ context.Context, req *nspb.RemoveAddressGroupItemsRequest) (*longrunningpb.Operation, error) {
	return a.changeItems(req.GetAddressGroup(), len(req.GetItems()), func(g *nspb.AddressGroup) error {
		rm, err := normItems("items", req.GetItems(), g)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, it := range g.GetItems() {
			have[it] = true
		}
		drop := map[string]bool{}
		for _, it := range rm {
			if !have[it] {
				return apierror.InvalidArgument("Invalid argument: item '%s' is not in the address group.", it)
			}
			drop[it] = true
		}
		var keep []string
		for _, it := range g.GetItems() {
			if !drop[it] {
				keep = append(keep, it)
			}
		}
		g.Items = keep
		return nil
	})
}

// CloneAddressGroupItems adds every item of the source group, which must
// be of the same type.
func (a *addressGroups) CloneAddressGroupItems(_ context.Context, req *nspb.CloneAddressGroupItemsRequest) (*longrunningpb.Operation, error) {
	src := req.GetSourceAddressGroup()
	if err := checkName(src, collAddrGroup); err != nil {
		return nil, apierror.InvalidArgument("Invalid argument: source_address_group %q is not an address group name.", src)
	}
	a.s.mu.Lock()
	sg, ok := a.s.res[src].(*nspb.AddressGroup)
	var items []string
	var typ nspb.AddressGroup_Type
	if ok {
		items, typ = append([]string(nil), sg.GetItems()...), sg.GetType()
	}
	a.s.mu.Unlock()
	if !ok {
		return nil, notFound(collAddrGroup, src)
	}
	return a.changeItems(req.GetAddressGroup(), max(len(items), 1), func(g *nspb.AddressGroup) error {
		if typ != g.GetType() {
			return apierror.InvalidArgument("Invalid argument: the source address group's type %s is not the address group's %s.", typ, g.GetType())
		}
		g.Items = mergeItems(g.GetItems(), items)
		return nil
	})
}

func mergeItems(cur, add []string) []string {
	seen := map[string]bool{}
	for _, it := range cur {
		seen[it] = true
	}
	out := append([]string(nil), cur...)
	for _, it := range add {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out
}

func (a *addressGroups) DeleteAddressGroup(_ context.Context, req *nspb.DeleteAddressGroupRequest) (*longrunningpb.Operation, error) {
	if refs := a.s.references(req.GetName()); len(refs) > 0 {
		if err := checkName(req.GetName(), collAddrGroup); err != nil {
			return nil, err
		}
		return nil, apierror.FailedPrecondition("AddressGroup '%s' is in use by security policy '%s'.", req.GetName(), refs[0].SecurityPolicy)
	}
	return a.s.delete(collAddrGroup, req.GetName())
}

// ListAddressGroupReferences names the Cloud Armor rules that match the
// group.
func (a *addressGroups) ListAddressGroupReferences(_ context.Context, req *nspb.ListAddressGroupReferencesRequest) (*nspb.ListAddressGroupReferencesResponse, error) {
	name := req.GetAddressGroup()
	if _, err := a.s.get(collAddrGroup, name); err != nil {
		return nil, err
	}
	refs := a.s.references(name)
	keys := make([]string, len(refs))
	for i := range refs {
		keys[i] = itoa(i)
	}
	page, next, err := paging.Page("agrefs:"+name, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &nspb.ListAddressGroupReferencesResponse{NextPageToken: next}
	idx := map[string]int{}
	for i := range refs {
		idx[itoa(i)] = i
	}
	for _, k := range page {
		r := refs[idx[k]]
		out.AddressGroupReferences = append(out.AddressGroupReferences,
			&nspb.ListAddressGroupReferencesResponse_AddressGroupReference{SecurityPolicy: r.SecurityPolicy, RulePriority: r.RulePriority})
	}
	return out, nil
}

// references are the security policy rules that match a group, sorted.
func (s *Server) references(name string) []AddressGroupReference {
	h := s.hooksNow()
	if h.AddressGroupReferences == nil {
		return nil
	}
	refs := h.AddressGroupReferences(name)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].SecurityPolicy != refs[j].SecurityPolicy {
			return refs[i].SecurityPolicy < refs[j].SecurityPolicy
		}
		return refs[i].RulePriority < refs[j].RulePriority
	})
	return refs
}

// AddressGroupItems are a group's items, for Cloud Armor's
// evaluateAddressGroup; ok is false when there is no such group, or it is
// not for Cloud Armor.
func (s *Server) AddressGroupItems(name string) (items []string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, found := s.res[name].(*nspb.AddressGroup)
	if !found || !hasArmorPurpose(g) {
		return nil, false
	}
	return append([]string(nil), g.GetItems()...), true
}
