package networkconnectivity

import (
	"context"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Service connection maps, a producer's side: "The service producer
// configures a service connection map in their own project"
// (about-service-connection-policies). serviceClass is required and
// serviceClassUri is output; the service class is the producer project's
// ServiceClass of that name in the map's location, made with the map's
// first use of it (the API has no ServiceClasses.Create: how a class comes
// to exist is UNVERIFIED, re-read 2026-11-22: the v1 reference), and a class a map still names is not deleted.

const (
	maps    = "serviceConnectionMaps"
	classes = "serviceClasses"
	tokens  = "serviceConnectionTokens"
)

func (s *Server) CreateServiceConnectionMap(_ context.Context, req *ncpb.CreateServiceConnectionMapRequest) (*longrunningpb.Operation, error) {
	_, region, err := parent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	id := req.GetServiceConnectionMapId()
	if id == "" {
		id = "scm-" + randomHex(6)
	}
	if err := checkID("service_connection_map_id", id); err != nil {
		return nil, err
	}
	in := req.GetServiceConnectionMap()
	if in == nil {
		return nil, apierror.InvalidArgument("service_connection_map is required")
	}
	m := clone(in)
	m.Name = req.GetParent() + "/" + maps + "/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.maps[m.Name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", m.Name)
	}
	if err := s.checkMapLocked(region, m); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	m.CreateTime, m.UpdateTime = now, now
	s.useClassLocked(req.GetParent(), m)
	m.Infrastructure = ncpb.Infrastructure_PSC
	m.ConsumerPscConnections = nil
	m.Etag = newEtag()
	s.maps[m.Name] = m
	s.reconcileLocked()
	return s.opLocked(req.GetParent(), m.Name, "create", s.maps[m.Name])
}

// checkMapLocked checks a map's service class, attachments (each in the
// map's region) and consumer configurations.
func (s *Server) checkMapLocked(region string, m *ncpb.ServiceConnectionMap) error {
	if m.GetServiceClass() == "" {
		return apierror.InvalidArgument("service_connection_map.service_class is required")
	}
	if !idRE.MatchString(m.GetServiceClass()) {
		return apierror.InvalidArgument("service_connection_map.service_class %q is not a valid service class name", m.GetServiceClass())
	}
	for i, p := range m.GetProducerPscConfigs() {
		rel := computeRel(p.GetServiceAttachmentUri())
		parts := strings.Split(rel, "/")
		if len(parts) != 6 || parts[0] != "projects" || parts[2] != "regions" || parts[4] != "serviceAttachments" {
			return apierror.InvalidArgument("producer_psc_configs[%d].service_attachment_uri %q must be projects/{project}/regions/{region}/serviceAttachments/{name}", i, p.GetServiceAttachmentUri())
		}
		if parts[3] != region {
			return apierror.InvalidArgument("producer_psc_configs[%d].service_attachment_uri %s is not in the map's region, %s", i, rel, region)
		}
		p.ServiceAttachmentUri = rel
	}
	for i, c := range m.GetConsumerPscConfigs() {
		net, ok := networkRel(c.GetNetwork())
		if !ok {
			return apierror.InvalidArgument("consumer_psc_configs[%d].network %q must be projects/{project}/global/networks/{network}", i, c.GetNetwork())
		}
		c.Network = net
		if c.GetProject() == "" {
			c.Project = strings.Split(net, "/")[1]
		}
		if cip := c.GetConsumerInstanceProject(); cip != "" && !strings.HasPrefix(cip, "projects/") {
			c.ConsumerInstanceProject = "projects/" + cip
		}
		if c.GetConsumerInstanceProject() == "" {
			return apierror.InvalidArgument("consumer_psc_configs[%d].consumer_instance_project is required", i)
		}
		c.State = ncpb.ServiceConnectionMap_ConsumerPscConfig_STATE_UNSPECIFIED
		c.ServiceAttachmentIpAddressMap = nil
	}
	return nil
}

// useClassLocked makes the map's service class, in its project and
// location, if it is not there yet, and sets serviceClassUri.
func (s *Server) useClassLocked(parentName string, m *ncpb.ServiceConnectionMap) {
	name := parentName + "/" + classes + "/" + m.GetServiceClass()
	if _, ok := s.classes[name]; !ok {
		now := timestamppb.New(s.now())
		s.classes[name] = &ncpb.ServiceClass{Name: name, ServiceClass: m.GetServiceClass(), CreateTime: now, UpdateTime: now, Etag: newEtag()}
	}
	m.ServiceClassUri = name
}

func (s *Server) GetServiceConnectionMap(_ context.Context, req *ncpb.GetServiceConnectionMapRequest) (*ncpb.ServiceConnectionMap, error) {
	if _, _, err := child(req.GetName(), maps); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.maps[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(m), nil
}

func (s *Server) ListServiceConnectionMaps(_ context.Context, req *ncpb.ListServiceConnectionMapsRequest) (*ncpb.ListServiceConnectionMapsResponse, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	s.mu.Lock()
	all := []*ncpb.ServiceConnectionMap{}
	for _, k := range s.listKeysLocked(s.maps, req.GetParent(), maps) {
		all = append(all, clone(s.maps[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("maps:"+req.GetParent(), all, req.GetFilter(), req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(),
		func(m *ncpb.ServiceConnectionMap) string { return m.GetName() })
	if err != nil {
		return nil, err
	}
	return &ncpb.ListServiceConnectionMapsResponse{ServiceConnectionMaps: out, NextPageToken: next}, nil
}

func (s *Server) UpdateServiceConnectionMap(_ context.Context, req *ncpb.UpdateServiceConnectionMapRequest) (*longrunningpb.Operation, error) {
	in := req.GetServiceConnectionMap()
	if in == nil {
		return nil, apierror.InvalidArgument("service_connection_map is required")
	}
	parentName, _, err := child(in.GetName(), maps)
	if err != nil {
		return nil, err
	}
	_, region, _ := parent("parent", parentName)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.maps[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	if err := checkEtag(in.Etag, cur.Etag); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description", "labels", "producer_psc_configs", "consumer_psc_configs", "token"}
	}
	next := clone(cur)
	for _, path := range paths {
		switch path {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			next.Labels = in.GetLabels()
		case "producer_psc_configs", "producerPscConfigs":
			next.ProducerPscConfigs = in.GetProducerPscConfigs()
		case "consumer_psc_configs", "consumerPscConfigs":
			next.ConsumerPscConfigs = in.GetConsumerPscConfigs()
		case "token":
			next.Token = in.GetToken()
		case "service_class", "serviceClass":
			return nil, apierror.InvalidArgument("service_class cannot be updated: delete the map and create a new one")
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of ServiceConnectionMap that can be updated", path)
		}
	}
	if err := s.checkMapLocked(region, next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = newEtag()
	s.maps[next.Name] = next
	// A consumer or attachment no longer listed loses its endpoints; a new
	// one gets them.
	s.reconcileLocked()
	return s.opLocked(parentName, next.Name, "update", s.maps[next.Name])
}

func (s *Server) DeleteServiceConnectionMap(_ context.Context, req *ncpb.DeleteServiceConnectionMapRequest) (*longrunningpb.Operation, error) {
	parentName, _, err := child(req.GetName(), maps)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.maps[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if err := checkEtag(req.Etag, cur.Etag); err != nil {
		return nil, err
	}
	s.dropMapLocked(req.GetName())
	return s.opLocked(parentName, req.GetName(), "delete", nil)
}

// dropMapLocked removes a map and the endpoints it made.
func (s *Server) dropMapLocked(name string) {
	m, ok := s.maps[name]
	if !ok {
		return
	}
	m.ConsumerPscConfigs, m.ProducerPscConfigs = nil, nil
	s.reconcileLocked()
	delete(s.maps, name)
	s.reconcileLocked()
}

// ---- Service classes ----

func (s *Server) GetServiceClass(_ context.Context, req *ncpb.GetServiceClassRequest) (*ncpb.ServiceClass, error) {
	if _, _, err := child(req.GetName(), classes); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.classes[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(c), nil
}

func (s *Server) ListServiceClasses(_ context.Context, req *ncpb.ListServiceClassesRequest) (*ncpb.ListServiceClassesResponse, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	s.mu.Lock()
	all := []*ncpb.ServiceClass{}
	for _, k := range s.listKeysLocked(s.classes, req.GetParent(), classes) {
		all = append(all, clone(s.classes[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("classes:"+req.GetParent(), all, req.GetFilter(), req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(),
		func(c *ncpb.ServiceClass) string { return c.GetName() })
	if err != nil {
		return nil, err
	}
	return &ncpb.ListServiceClassesResponse{ServiceClasses: out, NextPageToken: next}, nil
}

func (s *Server) UpdateServiceClass(_ context.Context, req *ncpb.UpdateServiceClassRequest) (*longrunningpb.Operation, error) {
	in := req.GetServiceClass()
	if in == nil {
		return nil, apierror.InvalidArgument("service_class is required")
	}
	parentName, _, err := child(in.GetName(), classes)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.classes[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	if err := checkEtag(in.Etag, cur.Etag); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"description", "labels"}
	}
	next := clone(cur)
	for _, path := range paths {
		switch path {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			next.Labels = in.GetLabels()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of ServiceClass that can be updated", path)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = newEtag()
	s.classes[next.Name] = next
	return s.opLocked(parentName, next.Name, "update", next)
}

func (s *Server) DeleteServiceClass(_ context.Context, req *ncpb.DeleteServiceClassRequest) (*longrunningpb.Operation, error) {
	parentName, _, err := child(req.GetName(), classes)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.classes[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if err := checkEtag(req.Etag, cur.Etag); err != nil {
		return nil, err
	}
	for k, m := range s.maps {
		if m.GetServiceClassUri() == req.GetName() {
			return nil, apierror.FailedPrecondition("The service class %s is used by the service connection map %s: delete the map first", req.GetName(), k)
		}
	}
	delete(s.classes, req.GetName())
	return s.opLocked(parentName, req.GetName(), "delete", nil)
}

// ---- Service connection tokens ----

// tokenLifetime is how long a token is valid. UNVERIFIED (re-read 2026-11-22: the v1 reference
// documents expireTime as output only, with no lifetime).
const tokenLifetime = 7 * 24 * time.Hour

func (s *Server) CreateServiceConnectionToken(_ context.Context, req *ncpb.CreateServiceConnectionTokenRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	id := req.GetServiceConnectionTokenId()
	if id == "" {
		id = "sct-" + randomHex(6)
	}
	if err := checkID("service_connection_token_id", id); err != nil {
		return nil, err
	}
	in := req.GetServiceConnectionToken()
	if in == nil {
		in = &ncpb.ServiceConnectionToken{}
	}
	t := clone(in)
	t.Name = req.GetParent() + "/" + tokens + "/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[t.Name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", t.Name)
	}
	if t.GetNetwork() != "" {
		net, ok := networkRel(t.GetNetwork())
		if !ok {
			return nil, apierror.InvalidArgument("service_connection_token.network %q must be projects/{project}/global/networks/{network}", t.GetNetwork())
		}
		if c := s.computeAPI(); c != nil && !c.HasNetwork(net, "") {
			return nil, apierror.InvalidArgument("The network %s was not found", net)
		}
		t.Network = net
	}
	now := s.now()
	t.CreateTime, t.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	t.ExpireTime = timestamppb.New(now.Add(tokenLifetime))
	t.Token = randomHex(24)
	t.Etag = newEtag()
	s.tokens[t.Name] = t
	return s.opLocked(req.GetParent(), t.Name, "create", t)
}

func (s *Server) GetServiceConnectionToken(_ context.Context, req *ncpb.GetServiceConnectionTokenRequest) (*ncpb.ServiceConnectionToken, error) {
	if _, _, err := child(req.GetName(), tokens); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(t), nil
}

func (s *Server) ListServiceConnectionTokens(_ context.Context, req *ncpb.ListServiceConnectionTokensRequest) (*ncpb.ListServiceConnectionTokensResponse, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent())
	}
	s.mu.Lock()
	all := []*ncpb.ServiceConnectionToken{}
	for _, k := range s.listKeysLocked(s.tokens, req.GetParent(), tokens) {
		all = append(all, clone(s.tokens[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("tokens:"+req.GetParent(), all, req.GetFilter(), req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(),
		func(t *ncpb.ServiceConnectionToken) string { return t.GetName() })
	if err != nil {
		return nil, err
	}
	return &ncpb.ListServiceConnectionTokensResponse{ServiceConnectionTokens: out, NextPageToken: next}, nil
}

func (s *Server) DeleteServiceConnectionToken(_ context.Context, req *ncpb.DeleteServiceConnectionTokenRequest) (*longrunningpb.Operation, error) {
	parentName, _, err := child(req.GetName(), tokens)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tokens[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if err := checkEtag(req.Etag, cur.Etag); err != nil {
		return nil, err
	}
	delete(s.tokens, req.GetName())
	return s.opLocked(parentName, req.GetName(), "delete", nil)
}
