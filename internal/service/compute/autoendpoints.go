package compute

import (
	"errors"
	"net/http"
	"strings"
)

// Private Service Connect endpoints made by service connectivity
// automation (#2141): the Network Connectivity API's service connection
// policies and maps make a consumer's endpoint to a producer's service
// attachment in one of the policy's subnets, as
// https://docs.cloud.google.com/vpc/docs/about-service-connection-policies
// (re-read 2026-11-22) describes: "Endpoint IP addresses come from the
// policy's subnets", and "You cannot directly delete Private Service
// Connect endpoints that are created through service connectivity
// automation." Such an endpoint is an ordinary forwarding rule otherwise:
// listed, read and reached from the network's VMs like any other.

// AutoEndpoint is what service connectivity automation asks for.
type AutoEndpoint struct {
	// Project is where the forwarding rule is made, Region its region,
	// Name its name.
	Project, Region, Name string
	// Network is projects/P/global/networks/N.
	Network string
	// Subnetworks are the policy's subnets, tried in order for a free
	// address.
	Subnetworks []string
	// Target is the producer's service attachment.
	Target string
	// GlobalAccess is the endpoint's allowPscGlobalAccess.
	GlobalAccess bool
	Description  string
	Labels       map[string]string
}

// AutoEndpointResult is the endpoint made.
type AutoEndpointResult struct {
	// ForwardingRule is projects/P/regions/R/forwardingRules/N.
	ForwardingRule, IP, Subnetwork, PSCConnectionID, Operation string
}

// ErrNoSubnetAddress is an automated endpoint none of whose subnets has
// an address free, or that has no subnet in its region.
var ErrNoSubnetAddress = errors.New("no subnetwork of the policy has a free address")

// CreateAutoEndpoint makes e, taking its address from the first of its
// subnetworks with one free. It must not be called with the server's lock
// held.
func (s *Server) CreateAutoEndpoint(e AutoEndpoint) (AutoEndpointResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureProjectLocked(e.Project)
	if p := strings.Split(e.Network, "/"); len(p) > 1 {
		_ = s.ensureProjectLocked(p[1])
	}
	rel := "projects/" + e.Project + "/regions/" + e.Region + "/forwardingRules/" + e.Name
	if o, ok := s.res[rel]; ok && s.automated[rel] {
		return AutoEndpointResult{ForwardingRule: rel, IP: str(o, "IPAddress"), Subnetwork: str(o, "subnetwork"),
			PSCConnectionID: str(o, "pscConnectionId")}, nil
	}
	if len(e.Subnetworks) == 0 {
		return AutoEndpointResult{}, ErrNoSubnetAddress
	}
	var last error = ErrNoSubnetAddress
	for _, sub := range e.Subnetworks {
		sub = Rel(sub, e.Project)
		if p := strings.Split(sub, "/"); len(p) != 6 || p[3] != e.Region {
			continue
		}
		body := obj{"name": e.Name, "target": e.Target, "network": e.Network, "subnetwork": sub, "description": e.Description}
		if e.GlobalAccess {
			body["allowPscGlobalAccess"] = true
		}
		if len(e.Labels) > 0 {
			l := obj{}
			for k, v := range e.Labels {
				l[k] = v
			}
			body["labels"] = l
		}
		op, err := s.insertForwardingRule(e.Project, e.Region, body)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && (ae.reason == "ipSpaceExhausted" || ae.reason == "invalid") {
				last = err
				continue
			}
			return AutoEndpointResult{}, err
		}
		s.automated[rel] = true
		o := s.res[rel]
		if err := s.saveLocked(); err != nil {
			return AutoEndpointResult{}, err
		}
		return AutoEndpointResult{ForwardingRule: rel, IP: str(o, "IPAddress"), Subnetwork: str(o, "subnetwork"),
			PSCConnectionID: str(o, "pscConnectionId"), Operation: str(op, "selfLink")}, nil
	}
	return AutoEndpointResult{}, last
}

// DeleteAutoEndpoint removes an endpoint CreateAutoEndpoint made; one
// already gone is not an error.
func (s *Server) DeleteAutoEndpoint(rel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.Split(rel, "/")
	if len(p) != 6 || p[4] != "forwardingRules" {
		return errf(http.StatusBadRequest, "invalid", "%s is not a forwarding rule", rel)
	}
	delete(s.automated, rel)
	if _, ok := s.res[rel]; !ok {
		return s.saveLocked()
	}
	_, err := s.deleteForwardingRule(p[1], p[3], p[5])
	return err
}

// ForwardingRuleStatus is an endpoint's pscConnectionStatus now, and
// false when it is gone.
func (s *Server) ForwardingRuleStatus(rel string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.res[rel]
	if !ok {
		return "", false
	}
	s.pscStatusLocked(o)
	return str(o, "pscConnectionStatus"), true
}
