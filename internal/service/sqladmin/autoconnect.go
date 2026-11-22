package sqladmin

import (
	"net/http"
	"strings"
)

// Private Service Connect's automatic endpoints (#2141): an instance with
// Private Service Connect may ask for endpoints in consumer networks,
// settings.ipConfiguration.pscConfig.pscAutoConnections, each a
// consumerNetwork (projects/P/global/networks/N) and an optional
// consumerProject. Cloud SQL makes each endpoint through the consumer
// network's service connection policy for the google-cloud-sql service
// class (the Network Connectivity API): the endpoint's address is
// answered as the entry's ipAddress, its connection state as status
// (ACTIVE once made) and the policy's as consumerNetworkStatus (VALID, or
// CONNECTION_POLICY_MISSING while the network has no policy, when "the
// endpoint won't be created" until one is). The endpoint's project is
// accepted by the instance's service attachment, as the consumer the
// instance asked for.
// (https://docs.cloud.google.com/sql/docs/postgres/configure-private-service-connect,
// re-read 2026-11-22.)

// AutoConsumer is one pscAutoConnections entry.
type AutoConsumer struct{ Network, Project string }

// AutoConnection is one automatic endpoint's state.
type AutoConnection struct {
	Network, Project, IP, Status, NetworkStatus, ForwardingRule string
}

// AutoConnector makes the automatic endpoints: the Network Connectivity
// API's service connectivity automation. It is never called with the
// instances' lock held.
type AutoConnector interface {
	// SetAutoConnections makes the endpoints of attachment those
	// consumers ask for; none removes them.
	SetAutoConnections(attachment, project, instance string, consumers []AutoConsumer) error
	// AutoConnections are the attachment's endpoints, in consumer order.
	AutoConnections(attachment string) []AutoConnection
}

// SetAutoConnector makes pscAutoConnections real through a; nil (no
// Compute Engine) leaves every entry without an endpoint.
func (s *Server) SetAutoConnector(a AutoConnector) {
	s.emu.Lock()
	s.autoConn = a
	s.emu.Unlock()
}

// autoConsumers are spec's pscAutoConnections.
func (s Spec) autoConsumers() []AutoConsumer {
	pc, _ := s.ipConfig()["pscConfig"].(map[string]any)
	list, _ := pc["pscAutoConnections"].([]any)
	var out []AutoConsumer
	for _, e := range list {
		m, _ := e.(map[string]any)
		n, _ := m["consumerNetwork"].(string)
		p, _ := m["consumerProject"].(string)
		out = append(out, AutoConsumer{Network: strings.TrimSpace(n), Project: strings.TrimSpace(p)})
	}
	return out
}

// consumerProjects are the projects spec's automatic endpoints are made
// in, which its service attachment accepts.
func (s Spec) autoConsumerProjects() []string {
	var out []string
	for _, c := range s.autoConsumers() {
		if c.Project != "" {
			out = append(out, c.Project)
		} else if p := strings.Split(c.Network, "/"); len(p) > 1 {
			out = append(out, p[1])
		}
	}
	return out
}

// checkAutoConnections refuses pscAutoConnections without Private
// Service Connect, and an entry whose consumerNetwork is not a network's
// path ("This must be a resource path that includes both the host project
// and the network name", the Admin API reference). The wording is
// UNVERIFIED.
func checkAutoConnections(spec Spec) error {
	cs := spec.autoConsumers()
	if len(cs) == 0 {
		return nil
	}
	if !spec.pscEnabled() {
		return Errorf(http.StatusBadRequest, "Invalid request: settings.ipConfiguration.pscConfig.pscAutoConnections requires Private Service Connect: set pscConfig.pscEnabled.")
	}
	seen := map[string]bool{}
	for i, c := range cs {
		p := strings.Split(c.Network, "/")
		if len(p) != 5 || p[0] != "projects" || p[2] != "global" || p[3] != "networks" || p[1] == "" || p[4] == "" {
			return Errorf(http.StatusBadRequest, "Invalid request: pscAutoConnections[%d].consumerNetwork %q must be projects/{project}/global/networks/{network}.", i, c.Network)
		}
		k := c.Network + "|" + c.Project
		if seen[k] {
			return Errorf(http.StatusBadRequest, "Invalid request: pscAutoConnections[%d] repeats the consumer network %s.", i, c.Network)
		}
		seen[k] = true
	}
	return nil
}

// syncAutoConnections makes spec's automatic endpoints, or removes them
// when gone is set (the instance deleted) or none is asked for.
func (s *Server) syncAutoConnections(spec Spec, gone bool) {
	s.emu.RLock()
	a := s.autoConn
	s.emu.RUnlock()
	if a == nil {
		return
	}
	var cs []AutoConsumer
	if !gone && spec.pscEnabled() {
		cs = spec.autoConsumers()
	}
	_ = a.SetAutoConnections(pscServiceAttachment(spec), spec.Project, spec.Name, cs)
}

// autoConnectionsJSON is pscConfig.pscAutoConnections with each entry's
// endpoint state, or nil when there are none.
func (s *Server) autoConnectionsJSON(spec Spec) []any {
	cs := spec.autoConsumers()
	if len(cs) == 0 {
		return nil
	}
	s.emu.RLock()
	a := s.autoConn
	s.emu.RUnlock()
	var got []AutoConnection
	if a != nil && spec.pscEnabled() {
		got = a.AutoConnections(pscServiceAttachment(spec))
	}
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		e := map[string]any{"consumerNetwork": c.Network}
		if c.Project != "" {
			e["consumerProject"] = c.Project
		}
		for _, g := range got {
			if g.Network != c.Network || (c.Project != "" && g.Project != c.Project) {
				continue
			}
			if g.NetworkStatus != "" && g.NetworkStatus != "STATE_UNSPECIFIED" {
				e["consumerNetworkStatus"] = g.NetworkStatus
			}
			if g.Status != "" {
				e["status"] = g.Status
			}
			if g.IP != "" {
				e["ipAddress"] = g.IP
			}
		}
		out = append(out, e)
	}
	return out
}

// withAutoConnections is settings with ipConfiguration.pscConfig's
// pscAutoConnections answered with their state, copying what it changes.
func (s *Server) withAutoConnections(st map[string]any, spec Spec) {
	list := s.autoConnectionsJSON(spec)
	if list == nil {
		return
	}
	ic, _ := st["ipConfiguration"].(map[string]any)
	pc, _ := ic["pscConfig"].(map[string]any)
	if pc == nil {
		return
	}
	nic := make(map[string]any, len(ic))
	for k, v := range ic {
		nic[k] = v
	}
	npc := make(map[string]any, len(pc))
	for k, v := range pc {
		npc[k] = v
	}
	npc["pscAutoConnections"] = list
	nic["pscConfig"] = npc
	st["ipConfiguration"] = nic
}
