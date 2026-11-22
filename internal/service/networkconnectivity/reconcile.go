package networkconnectivity

import (
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// reconcileLocked brings every map's endpoints to what the maps and the
// policies say, after any change to either: a consumer configuration of a
// map whose network has a policy for the map's service class in the map's
// region is VALID, and gets an endpoint to each of the map's service
// attachments, unless the policy's connection limit is reached
// (POLICY_LIMIT_REACHED) or the producer is outside the policy's service
// instance scope (CONSUMER_INSTANCE_PROJECT_NOT_ALLOWLISTED); without a
// policy it is CONNECTION_POLICY_MISSING, and connects once one is made
// ("Creation or deletion blocked by an issue is retried periodically until
// the issue is resolved", about-service-connection-policies). An endpoint
// that failed (no free address) is tried again here. An endpoint whose
// consumer or attachment the map no longer names is removed, and with it
// its connection on the policy.
func (s *Server) reconcileLocked() {
	c := s.computeAPI()
	type want struct {
		m   *ncpb.ServiceConnectionMap
		cfg *ncpb.ServiceConnectionMap_ConsumerPscConfig
		sa  string
	}
	key := func(mapName, project, network, sa string) string {
		return mapName + "|" + project + "|" + network + "|" + sa
	}
	desired := map[string]want{}
	names := sortedKeys(s.maps, "")
	for _, n := range names {
		m := s.maps[n]
		for _, cfg := range m.GetConsumerPscConfigs() {
			for _, pc := range m.GetProducerPscConfigs() {
				desired[key(n, cfg.GetProject(), cfg.GetNetwork(), pc.GetServiceAttachmentUri())] = want{m, cfg, pc.GetServiceAttachmentUri()}
			}
		}
	}
	// Remove what is no longer wanted, and every map's connections from the
	// maps that are gone.
	for _, n := range names {
		m := s.maps[n]
		kept := m.ConsumerPscConnections[:0]
		for _, cn := range m.GetConsumerPscConnections() {
			if _, ok := desired[key(n, cn.GetProject(), cn.GetNetwork(), cn.GetServiceAttachmentUri())]; ok {
				kept = append(kept, cn)
				continue
			}
			s.dropConnectionLocked(c, cn.GetForwardingRule())
		}
		m.ConsumerPscConnections = kept
	}
	for _, p := range s.policies {
		kept := p.PscConnections[:0]
		for _, pc := range p.GetPscConnections() {
			if s.connectionOwnedLocked(pc.GetConsumerForwardingRule()) {
				kept = append(kept, pc)
			} else if c != nil && pc.GetConsumerForwardingRule() != "" {
				_ = c.DeleteAutoEndpoint(pc.GetConsumerForwardingRule())
			}
		}
		p.PscConnections = kept
	}
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, n := range names {
		for _, cfg := range s.maps[n].GetConsumerPscConfigs() {
			cfg.State = ncpb.ServiceConnectionMap_ConsumerPscConfig_STATE_UNSPECIFIED
		}
	}
	for _, k := range keys {
		w := desired[k]
		region := mapRegion(w.m.GetName())
		pol := s.policyForLocked(w.cfg.GetNetwork(), region, w.m.GetServiceClass())
		existing := findConnection(w.m, w.cfg, w.sa)
		switch {
		case pol == nil:
			setState(w.cfg, ncpb.ServiceConnectionMap_ConsumerPscConfig_CONNECTION_POLICY_MISSING)
			continue
		case !inScope(pol, w.cfg.GetConsumerInstanceProject()):
			setState(w.cfg, ncpb.ServiceConnectionMap_ConsumerPscConfig_CONSUMER_INSTANCE_PROJECT_NOT_ALLOWLISTED)
			continue
		}
		if existing != nil && existing.GetState() == ncpb.ServiceConnectionMap_ConsumerPscConnection_ACTIVE {
			setState(w.cfg, ncpb.ServiceConnectionMap_ConsumerPscConfig_VALID)
			continue
		}
		if lim := pol.GetPscConfig().Limit; lim != nil && int64(activeConnections(pol)) >= *lim {
			setState(w.cfg, ncpb.ServiceConnectionMap_ConsumerPscConfig_POLICY_LIMIT_REACHED)
			continue
		}
		setState(w.cfg, ncpb.ServiceConnectionMap_ConsumerPscConfig_VALID)
		if existing == nil {
			existing = &ncpb.ServiceConnectionMap_ConsumerPscConnection{ServiceAttachmentUri: w.sa, Project: w.cfg.GetProject(),
				Network: w.cfg.GetNetwork(), ProducerInstanceMetadata: w.cfg.GetProducerInstanceMetadata(), ProducerInstanceId: w.cfg.GetProducerInstanceId(),
				IpVersion: w.cfg.IpVersion}
			w.m.ConsumerPscConnections = append(w.m.ConsumerPscConnections, existing)
		}
		if c == nil {
			existing.State = ncpb.ServiceConnectionMap_ConsumerPscConnection_FAILED
			existing.ErrorInfo = &errdetails.ErrorInfo{Reason: "COMPUTE_ENGINE_NOT_SERVED", Domain: "networkconnectivity.googleapis.com",
				Metadata: map[string]string{"detail": "this instance does not serve Compute Engine, which the endpoint is made in"}}
			continue
		}
		res, err := c.CreateAutoEndpoint(Endpoint{Project: w.cfg.GetProject(), Region: region, Name: endpointName(k), Network: w.cfg.GetNetwork(),
			Subnetworks: pol.GetPscConfig().GetSubnetworks(), Target: w.sa, GlobalAccess: !w.cfg.GetDisableGlobalAccess(),
			Description: "Created by service connectivity automation for " + w.m.GetServiceClass() + "."})
		if err != nil {
			existing.State = ncpb.ServiceConnectionMap_ConsumerPscConnection_FAILED
			reason := "ERROR_CREATING_FORWARDING_RULE"
			if errors.Is(err, ErrNoAddress) {
				reason = "NO_IP_ADDRESS_AVAILABLE"
			}
			existing.ErrorType = ncpb.ConnectionErrorType_ERROR_CONSUMER_SIDE
			existing.ErrorInfo = &errdetails.ErrorInfo{Reason: reason, Domain: "networkconnectivity.googleapis.com",
				Metadata: map[string]string{"detail": err.Error()}}
			continue
		}
		existing.State = ncpb.ServiceConnectionMap_ConsumerPscConnection_ACTIVE
		existing.ErrorInfo, existing.ErrorType, existing.Error = nil, ncpb.ConnectionErrorType_CONNECTION_ERROR_TYPE_UNSPECIFIED, nil
		existing.ForwardingRule, existing.Ip, existing.PscConnectionId = res.ForwardingRule, res.IP, res.PSCConnectionID
		existing.SelectedSubnetwork, existing.GceOperation = res.Subnetwork, res.Operation
		pol.PscConnections = append(pol.PscConnections, &ncpb.ServiceConnectionPolicy_PscConnection{
			State: ncpb.ServiceConnectionPolicy_ACTIVE, ConsumerForwardingRule: res.ForwardingRule, ConsumerAddress: res.IP,
			GceOperation: res.Operation, ConsumerTargetProject: w.cfg.GetProject(), PscConnectionId: res.PSCConnectionID,
			SelectedSubnetwork: res.Subnetwork, ProducerInstanceId: w.cfg.GetProducerInstanceId(),
			ProducerInstanceMetadata: w.cfg.GetProducerInstanceMetadata(), ServiceClass: w.m.GetServiceClass(), IpVersion: w.cfg.IpVersion})
	}
}

// setState keeps the worst state a consumer configuration's attachments
// give it: any failure over VALID.
func setState(cfg *ncpb.ServiceConnectionMap_ConsumerPscConfig, st ncpb.ServiceConnectionMap_ConsumerPscConfig_State) {
	if cfg.State == ncpb.ServiceConnectionMap_ConsumerPscConfig_STATE_UNSPECIFIED || cfg.State == ncpb.ServiceConnectionMap_ConsumerPscConfig_VALID {
		cfg.State = st
	}
}

func (s *Server) dropConnectionLocked(c Compute, fr string) {
	if fr == "" {
		return
	}
	if c != nil {
		_ = c.DeleteAutoEndpoint(fr)
	}
	for _, p := range s.policies {
		kept := p.PscConnections[:0]
		for _, pc := range p.GetPscConnections() {
			if pc.GetConsumerForwardingRule() != fr {
				kept = append(kept, pc)
			}
		}
		p.PscConnections = kept
	}
}

// connectionOwnedLocked is whether a map still has the endpoint fr.
func (s *Server) connectionOwnedLocked(fr string) bool {
	for _, m := range s.maps {
		for _, cn := range m.GetConsumerPscConnections() {
			if cn.GetForwardingRule() == fr {
				return true
			}
		}
	}
	return false
}

func findConnection(m *ncpb.ServiceConnectionMap, cfg *ncpb.ServiceConnectionMap_ConsumerPscConfig, sa string) *ncpb.ServiceConnectionMap_ConsumerPscConnection {
	for _, cn := range m.GetConsumerPscConnections() {
		if cn.GetProject() == cfg.GetProject() && cn.GetNetwork() == cfg.GetNetwork() && cn.GetServiceAttachmentUri() == sa {
			return cn
		}
	}
	return nil
}

// policyForLocked is the policy for network, region and service class:
// in the network's (host) project.
func (s *Server) policyForLocked(network, region, class string) *ncpb.ServiceConnectionPolicy {
	p := strings.Split(network, "/")
	if len(p) < 2 {
		return nil
	}
	prefix := "projects/" + p[1] + "/locations/" + region + "/" + policies + "/"
	for _, k := range sortedKeys(s.policies, prefix) {
		if pol := s.policies[k]; pol.GetNetwork() == network && pol.GetServiceClass() == class {
			return pol
		}
	}
	return nil
}

// inScope is whether a producer instance in project (projects/P) may
// connect through pol: by default only one in the policy's own project
// ("Connections must be in the same project as the producer instance"),
// else one in a listed project. A folder or organization in the list is
// not resolved to its projects here, so it admits none: UNVERIFIED gap,
// named in docs/gaps.md.
func inScope(pol *ncpb.ServiceConnectionPolicy, project string) bool {
	pc := pol.GetPscConfig()
	if pc.GetProducerInstanceLocation() != ncpb.ServiceConnectionPolicy_PscConfig_CUSTOM_RESOURCE_HIERARCHY_LEVELS {
		return project == "projects/"+strings.Split(pol.GetName(), "/")[1]
	}
	for _, l := range pc.GetAllowedGoogleProducersResourceHierarchyLevel() {
		if l == project {
			return true
		}
	}
	return false
}

func activeConnections(pol *ncpb.ServiceConnectionPolicy) int {
	n := 0
	for _, pc := range pol.GetPscConnections() {
		if pc.GetState() == ncpb.ServiceConnectionPolicy_ACTIVE {
			n++
		}
	}
	return n
}

// mapRegion is a map's location.
func mapRegion(name string) string {
	_, rest, _ := strings.Cut(name, "/locations/")
	l, _, _ := strings.Cut(rest, "/")
	return l
}

// endpointName is an automated endpoint's forwarding rule name, stable
// for its map, consumer and attachment. The form is UNVERIFIED: Google's
// documentation does not name the forwarding rules automation makes.
func endpointName(k string) string {
	return fmt.Sprintf("sca-auto-fr-%08x", crc32.ChecksumIEEE([]byte(k)))
}
