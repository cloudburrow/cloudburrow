package networkservices

import (
	"context"
	"regexp"
	"sort"
	"strconv"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Endpoint policies (#2160), as the EndpointPolicy reference and
// service-mesh/docs/service-routing/... describe them (read 2026-11-22):
// global, a type (SIDECAR_PROXY or GRPC_SERVER), an endpoint matcher by
// xDS node metadata labels (MATCH_ANY or MATCH_ALL, at most 64 labels),
// an optional traffic port selector, and the Network Security server TLS,
// client TLS and authorization policies the matched endpoints apply. The
// xDS API gives an xDS-enabled gRPC server whose node matches its policy's
// serverTlsPolicy (EndpointPolicies).

const collEndpointPolicies = "endpointPolicies"

const maxMetadataLabels = 64

var (
	netsecRefRE = map[string]*regexp.Regexp{
		"server_tls_policy":    regexp.MustCompile(`^projects/[^/]+/locations/global/serverTlsPolicies/[^/]+$`),
		"client_tls_policy":    regexp.MustCompile(`^projects/[^/]+/locations/global/clientTlsPolicies/[^/]+$`),
		"authorization_policy": regexp.MustCompile(`^projects/[^/]+/locations/global/authorizationPolicies/[^/]+$`),
	}
	portRangeRE = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)
)

func validPortSpec(p string) bool {
	if p == "*" {
		return true
	}
	m := portRangeRE.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	lo, _ := strconv.Atoi(m[1])
	hi := lo
	if m[2] != "" {
		hi, _ = strconv.Atoi(m[2])
	}
	return lo >= 1 && hi <= 65535 && lo <= hi
}

func (s *Server) checkEndpointPolicy(m proto.Message, name string) error {
	e := m.(*nsvpb.EndpointPolicy)
	if locationOf(name) != "global" {
		return apierror.InvalidArgument("EndpointPolicy resources are global: location '%s' is not supported.", locationOf(name))
	}
	if e.GetType() == nsvpb.EndpointPolicy_ENDPOINT_POLICY_TYPE_UNSPECIFIED {
		return apierror.InvalidArgument("Invalid argument: type is required: SIDECAR_PROXY or GRPC_SERVER.")
	}
	ml := e.GetEndpointMatcher().GetMetadataLabelMatcher()
	if ml == nil {
		return apierror.InvalidArgument("Invalid argument: endpoint_matcher.metadata_label_matcher is required.")
	}
	if ml.GetMetadataLabelMatchCriteria() == nsvpb.EndpointMatcher_MetadataLabelMatcher_METADATA_LABEL_MATCH_CRITERIA_UNSPECIFIED {
		return apierror.InvalidArgument("Invalid argument: endpoint_matcher.metadata_label_matcher.metadata_label_match_criteria is required: MATCH_ANY or MATCH_ALL.")
	}
	if len(ml.GetMetadataLabels()) > maxMetadataLabels {
		return apierror.InvalidArgument("Invalid value for field 'endpoint_matcher.metadata_label_matcher.metadata_labels': at most %d labels.", maxMetadataLabels)
	}
	for i, l := range ml.GetMetadataLabels() {
		if l.GetLabelName() == "" || l.GetLabelValue() == "" {
			return apierror.InvalidArgument("Invalid value for field 'endpoint_matcher.metadata_label_matcher.metadata_labels[%d]': label_name and label_value are required.", i)
		}
	}
	for i, p := range e.GetTrafficPortSelector().GetPorts() {
		if !validPortSpec(p) {
			return apierror.InvalidArgument("Invalid value for field 'traffic_port_selector.ports[%d]': '%s'. A port is a number (1-65535), a range ('8080-8090') or '*'.", i, p)
		}
	}
	h := s.hooksNow()
	for field, ref := range map[string]string{"server_tls_policy": e.GetServerTlsPolicy(), "client_tls_policy": e.GetClientTlsPolicy(), "authorization_policy": e.GetAuthorizationPolicy()} {
		if ref == "" {
			continue
		}
		rel := relRef(ref)
		if !netsecRefRE[field].MatchString(rel) {
			return apierror.InvalidArgument("Invalid value for field '%s': '%s' is not a global Network Security policy name.", field, ref)
		}
		if h.NetSecPolicyExists != nil && !h.NetSecPolicyExists(rel) {
			return apierror.NotFound("The policy '%s' was not found.", rel)
		}
	}
	return nil
}

func (s *Server) CreateEndpointPolicy(_ context.Context, req *nsvpb.CreateEndpointPolicyRequest) (*longrunningpb.Operation, error) {
	e, _ := proto.Clone(req.GetEndpointPolicy()).(*nsvpb.EndpointPolicy)
	if e == nil {
		e = &nsvpb.EndpointPolicy{}
	}
	return s.create(req.GetParent(), collEndpointPolicies, "endpoint_policy_id", req.GetEndpointPolicyId(), e, s.checkEndpointPolicy)
}

func (s *Server) GetEndpointPolicy(_ context.Context, req *nsvpb.GetEndpointPolicyRequest) (*nsvpb.EndpointPolicy, error) {
	m, err := s.get(collEndpointPolicies, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nsvpb.EndpointPolicy), nil
}

func (s *Server) ListEndpointPolicies(_ context.Context, req *nsvpb.ListEndpointPoliciesRequest) (*nsvpb.ListEndpointPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collEndpointPolicies)
	if err != nil {
		return nil, err
	}
	items := make([]*nsvpb.EndpointPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nsvpb.EndpointPolicy)
	}
	page, next, err := aipfilter.ListPage("ep:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nsvpb.ListEndpointPoliciesResponse{EndpointPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateEndpointPolicy(_ context.Context, req *nsvpb.UpdateEndpointPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collEndpointPolicies, req.GetEndpointPolicy(), req.GetUpdateMask(),
		[]string{"labels", "description", "authorization_policy", "endpoint_matcher", "traffic_port_selector", "server_tls_policy", "client_tls_policy"},
		s.checkEndpointPolicy)
}

func (s *Server) DeleteEndpointPolicy(_ context.Context, req *nsvpb.DeleteEndpointPolicyRequest) (*longrunningpb.Operation, error) {
	return s.delete(collEndpointPolicies, req.GetName())
}

// EndpointPolicies are a project's endpoint policies, copies, by name. It
// reads only this API's records, so it may be called with Compute
// Engine's lock held.
func (s *Server) EndpointPolicies(project string) []*nsvpb.EndpointPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*nsvpb.EndpointPolicy
	for k, m := range s.res {
		if e, ok := m.(*nsvpb.EndpointPolicy); ok && projectOf(k) == project {
			out = append(out, proto.Clone(e).(*nsvpb.EndpointPolicy))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// EndpointPolicyMatches is whether a policy's endpoint matcher matches an
// xDS node's metadata labels: MATCH_ANY needs one of its labels, MATCH_ALL
// every one; a policy with no labels matches every node.
func EndpointPolicyMatches(e *nsvpb.EndpointPolicy, node map[string]string) bool {
	ml := e.GetEndpointMatcher().GetMetadataLabelMatcher()
	labels := ml.GetMetadataLabels()
	if len(labels) == 0 {
		return true
	}
	n := 0
	for _, l := range labels {
		if v, ok := node[l.GetLabelName()]; ok && v == l.GetLabelValue() {
			n++
		}
	}
	if ml.GetMetadataLabelMatchCriteria() == nsvpb.EndpointMatcher_MetadataLabelMatcher_MATCH_ALL {
		return n == len(labels)
	}
	return n > 0
}
