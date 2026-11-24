package main

// Cloud SQL's private IP on a VPC network (#2141) is reachable from the
// network's VMs: Compute Engine's VPC model puts each instance's private
// address and port in the VMs' rulesets, sending a connection to it on to
// the instance's server, its cluster Service, which the VMs' pods reach.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// serviceIPFresh is how long a Service's cluster IP is used before it is
// read again.
const serviceIPFresh = 30 * time.Second

// serviceIPs caches cluster Services' IPs, read in the background so the
// VPC configuration, built under Compute Engine's lock, never waits on
// the cluster.
type serviceIPs struct {
	kubeconfig, namespace string

	mu      sync.Mutex
	ips     map[string]string
	read    map[string]time.Time
	reading map[string]bool
}

func (c *serviceIPs) get(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ips == nil {
		c.ips, c.read, c.reading = map[string]string{}, map[string]time.Time{}, map[string]bool{}
	}
	if time.Since(c.read[name]) > serviceIPFresh && !c.reading[name] {
		c.reading[name] = true
		go c.refresh(name)
	}
	return c.ips[name]
}

func (c *serviceIPs) refresh(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ip := ""
	if raw, err := kubectlRaw(ctx, c.kubeconfig, "/api/v1/namespaces/"+c.namespace+"/services/"+name); err == nil {
		var svc struct {
			Spec struct {
				ClusterIP string `json:"clusterIP"`
			} `json:"spec"`
		}
		if json.Unmarshal(raw, &svc) == nil && svc.Spec.ClusterIP != "None" {
			ip = svc.Spec.ClusterIP
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ips[name], c.read[name], c.reading[name] = ip, time.Now(), false
}

// sqlEnginePort is the port an engine serves on, its private IP's and its
// Service's.
func sqlEnginePort(version string) int {
	switch {
	case strings.HasPrefix(version, "MYSQL_"):
		return components.CloudSQLMySQLPort
	case strings.HasPrefix(version, "SQLSERVER_"):
		return components.CloudSQLServerPort
	}
	return components.CloudSQLPort
}

// privateServices are network's Cloud SQL private IPs.
func (s *computeService) privateServices(network string) []compute.PrivateService {
	if s.sql == nil {
		return nil
	}
	srv := s.sql()
	if srv == nil {
		return nil
	}
	var out []compute.PrivateService
	for _, ep := range srv.PrivateEndpoints() {
		if ep.Network != network {
			continue
		}
		ip := s.svcIPs.get(components.SQLInstancePrefix + ep.Server)
		if ip == "" {
			continue
		}
		port := sqlEnginePort(ep.DatabaseVersion)
		out = append(out, compute.PrivateService{Name: "cloudsql " + ep.Instance, IP: ep.IP, Port: port, TargetIP: ip, TargetPort: port,
			ExtraPorts: poolerPorts(ep)})
	}
	return out
}

// sqlNetworkUsers are the Cloud SQL instances with a private IP on a
// network, which keep it from being deleted while their private services
// access connection is on it.
func (s *computeService) sqlNetworkUsers(rel string) []string {
	if s.sql == nil {
		return nil
	}
	srv := s.sql()
	if srv == nil {
		return nil
	}
	var out []string
	for _, ep := range srv.PrivateEndpoints() {
		if ep.Network == rel {
			out = append(out, "Cloud SQL instance "+ep.Instance)
		}
	}
	return out
}

// serviceAttachment resolves a Private Service Connect endpoint's target,
// a Cloud SQL instance's service attachment, to the instance's server.
func (s *computeService) serviceAttachment(link, consumer string) (compute.PrivateService, bool, bool) {
	if s.sql == nil {
		return compute.PrivateService{}, false, false
	}
	srv := s.sql()
	if srv == nil {
		return compute.PrivateService{}, false, false
	}
	ep, accepted, ok := srv.PSCAttachment(link, consumer)
	if !ok {
		return compute.PrivateService{}, false, false
	}
	port := sqlEnginePort(ep.DatabaseVersion)
	return compute.PrivateService{Name: "cloudsql " + ep.Instance, Port: port, TargetPort: port,
		TargetIP: s.svcIPs.get(components.SQLInstancePrefix + ep.Server), ExtraPorts: poolerPorts(ep)}, accepted, true
}

// poolerPorts are the ports an instance's private endpoints serve beside
// its server's: its managed connection pooler's, 6432, as Google's
// private IP and Private Service Connect endpoints do (#2141).
func poolerPorts(ep sqladmin.PrivateEndpoint) []int {
	if ep.PoolerPort == 0 {
		return nil
	}
	return []int{ep.PoolerPort}
}
