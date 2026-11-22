package networkconnectivity

import (
	"strings"

	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A Google producer's automatic endpoints: Cloud SQL's pscAutoConnections
// (#2141, https://docs.cloud.google.com/sql/docs/postgres/configure-private-service-connect,
// re-read 2026-11-22). The producer keeps a service connection map of
// service class google-cloud-sql in its tenant project (the service
// attachment's), whose consumers are the instance's auto-connections; the
// consumer's service connection policy for that class decides whether,
// and in which subnet, each endpoint is made, as for any map.

// CloudSQLServiceClass is Cloud SQL's service class.
const CloudSQLServiceClass = "google-cloud-sql"

// Consumer is one requested automatic endpoint: a consumer network
// (projects/P/global/networks/N) and the project the endpoint is made in
// ("" is the network's).
type Consumer struct{ Network, Project string }

// AutoConnection is one automatic endpoint's state, as Cloud SQL answers
// it: the endpoint's address, its connection state (status: ACTIVE,
// FAILED, ...) and the consumer network's policy state
// (consumerNetworkStatus: VALID, CONNECTION_POLICY_MISSING, ...).
type AutoConnection struct {
	Network, Project, IP, Status, NetworkStatus, ForwardingRule string
}

// producerMap is the map of a service attachment
// (projects/T/regions/R/serviceAttachments/A).
func producerMap(attachment string) (string, bool) {
	p := strings.Split(attachment, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != "regions" || p[4] != "serviceAttachments" {
		return "", false
	}
	return "projects/" + p[1] + "/locations/" + p[3] + "/" + maps + "/" + p[5], true
}

// SetProducer makes the attachment's endpoints those consumers ask for:
// instanceProject is the producer instance's project, which the policies'
// service instance scope is checked against. No consumers removes them
// all.
func (s *Server) SetProducer(class, attachment, instanceProject, instance string, consumers []Consumer) error {
	name, ok := producerMap(attachment)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(consumers) == 0 {
		if _, ok := s.maps[name]; ok {
			s.dropMapLocked(name)
			return s.saveLocked()
		}
		return nil
	}
	m, ok := s.maps[name]
	if !ok {
		now := timestamppb.New(s.now())
		m = &ncpb.ServiceConnectionMap{Name: name, CreateTime: now, ServiceClass: class, Infrastructure: ncpb.Infrastructure_PSC,
			ProducerPscConfigs: []*ncpb.ServiceConnectionMap_ProducerPscConfig{{ServiceAttachmentUri: attachment}}}
		s.maps[name] = m
	}
	m.UpdateTime = timestamppb.New(s.now())
	m.Etag = newEtag()
	m.ConsumerPscConfigs = nil
	for _, c := range consumers {
		net, ok := networkRel(c.Network)
		if !ok {
			continue
		}
		project := c.Project
		if project == "" {
			project = strings.Split(net, "/")[1]
		}
		m.ConsumerPscConfigs = append(m.ConsumerPscConfigs, &ncpb.ServiceConnectionMap_ConsumerPscConfig{Project: project, Network: net,
			ConsumerInstanceProject: "projects/" + instanceProject, ProducerInstanceMetadata: map[string]string{"instance": instance}})
	}
	s.reconcileLocked()
	return s.saveLocked()
}

// Producer is the attachment's automatic endpoints, in its consumers'
// order.
func (s *Server) Producer(attachment string) []AutoConnection {
	name, ok := producerMap(attachment)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.maps[name]
	if !ok {
		return nil
	}
	var out []AutoConnection
	for _, cfg := range m.GetConsumerPscConfigs() {
		a := AutoConnection{Network: cfg.GetNetwork(), Project: cfg.GetProject(), NetworkStatus: cfg.GetState().String()}
		if cn := findConnection(m, cfg, attachment); cn != nil {
			a.IP, a.Status, a.ForwardingRule = cn.GetIp(), cn.GetState().String(), cn.GetForwardingRule()
		}
		out = append(out, a)
	}
	return out
}

// HasPolicy is whether network (projects/P/global/networks/N) has
// a policy for class in region, for the console's Confirm Network Setup.
func (s *Server) HasPolicy(network, region, class string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policyForLocked(network, region, class) != nil
}
