package redisadmin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A Redis cluster's server CA mode (#1907): server_ca_mode and
// server_ca_pool, as Google documents them
// (https://docs.cloud.google.com/memorystore/docs/cluster/about-in-transit-encryption,
// /use-customer-managed-ca, re-read 2026-11-19):
//
//   - SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA, the default: the
//     cluster's own CA (tls.go), as before;
//   - SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA: "a managed, regionalized CA
//     infrastructure. For each region, you can download a single CA
//     certificate bundle", valid for every cluster of the region that uses
//     it. Here one CA per project and region, made with the first such
//     cluster and kept, issues every such cluster's node certificates;
//     GetSharedRegionalCertificateAuthority returns it, as does
//     GetClusterCertificateAuthority of each of them;
//   - SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA: the customer's CA pool in
//     Certificate Authority Service, this instance's own (--services
//     privateca, in process), issues each node's certificate when the node
//     starts (a configuration-based request, P-256, the node's names and
//     addresses in the SANs), so it is listed among the pool's
//     certificates and chains to the pool's CAs, which the nodes and the
//     admin plane trust. Checked as Google documents: "server_ca_pool must
//     be provided when server_ca_mode is SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA";
//     the pool is projects/{project}/locations/{region}/caPools/{pool}, in
//     the cluster's region; it exists ("Resource {pool} not found."), and
//     has an enabled CA ("There are no enabled CAs in the CaPool");
//     GetClusterCertificateAuthority is "not supported for customer-managed
//     CAs". Without Certificate Authority Service the mode is UNIMPLEMENTED.
//     Not enforced: the service agent
//     service-PROJECT_NUMBER@cloud-redis.iam.gserviceaccount.com and its
//     roles/privateca.certificateRequester on the pool, as no call here is
//     authorized.
//
// Every mode other than the default needs in-transit encryption, which
// is what it issues certificates for. The mode and pool are immutable
// ("after you configure a customer-managed CA for a cluster, you can't
// modify the configuration"; UpdateCluster has no such path). The weekly
// certificate rotation and rotate_server_certificate are not served: a
// node's certificate is issued again each time it starts.

// ClusterCAS is Certificate Authority Service as a Redis cluster's
// customer-managed CA uses it. Its errors are gRPC statuses.
type ClusterCAS interface {
	// EnabledCAs is how many of the pool's CAs are ENABLED; a pool that
	// does not exist is NotFound.
	EnabledCAs(ctx context.Context, pool string) (int, error)
	// CACerts are the pool's CA certificates, PEM, each chain's.
	CACerts(ctx context.Context, pool string) ([]string, error)
}

// UseClusterCAS makes SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA work over cas.
func (s *Server) UseClusterCAS(cas ClusterCAS) {
	if s.clusters != nil {
		s.clusters.mu.Lock()
		s.clusters.cas = cas
		s.clusters.mu.Unlock()
	}
}

var clusterCAPoolRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/caPools/([^/]+)$`)

// serverCA sets r's CA for in's server CA mode, in project and region,
// with in-transit encryption tlsOn.
func (c *clusters) serverCA(ctx context.Context, r *clusterRecord, in *clusterpb.Cluster, project, region string, tlsOn bool) error {
	mode := in.GetServerCaMode()
	pool := in.GetServerCaPool()
	switch mode {
	case clusterpb.ServerCaMode_SERVER_CA_MODE_UNSPECIFIED, clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA,
		clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA, clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA:
	default:
		return apierror.InvalidArgument("server_ca_mode %v is not a server CA mode", mode)
	}
	if mode == clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA && pool == "" {
		return apierror.InvalidArgument("server_ca_pool must be provided when server_ca_mode is SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.")
	}
	if pool != "" && mode != clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA {
		return apierror.InvalidArgument("server_ca_pool can only be provided when server_ca_mode is SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.")
	}
	if in.ServerCaMode != nil && mode != clusterpb.ServerCaMode_SERVER_CA_MODE_UNSPECIFIED {
		r.ServerCAMode = mode.String()
	}
	if !tlsOn {
		if mode == clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA || mode == clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA {
			return apierror.InvalidArgument("server_ca_mode %v requires transit_encryption_mode TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION", mode)
		}
		return nil
	}
	switch mode {
	case clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA:
		ca, key, err := c.sharedCA(project, region)
		if err != nil {
			return err
		}
		r.TransitCA, r.TransitCAKey = ca, key
	case clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA:
		m := clusterCAPoolRE.FindStringSubmatch(pool)
		if m == nil {
			return apierror.InvalidArgument("server_ca_pool %q must be projects/{project}/locations/{region}/caPools/{ca_pool}", pool)
		}
		if m[2] != region {
			return apierror.InvalidArgument("server_ca_pool %s must be in the cluster's region, %s", pool, region)
		}
		c.mu.Lock()
		cas := c.cas
		c.mu.Unlock()
		if cas == nil {
			return apierror.Unimplemented("server_ca_mode SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA needs Certificate Authority Service, which this instance does not serve (--services privateca)")
		}
		n, err := cas.EnabledCAs(ctx, pool)
		if status.Code(err) == codes.NotFound {
			return apierror.NotFound("Resource %s not found.", pool)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return apierror.FailedPrecondition("There are no enabled CAs in the CaPool %s.", pool)
		}
		certs, err := cas.CACerts(ctx, pool)
		if err != nil {
			return err
		}
		r.ServerCAPool = pool
		r.TransitCA, r.TransitCAKey = strings.Join(certs, ""), ""
	default:
		var err error
		if r.TransitCA, r.TransitCAKey, err = newCA(r.Name[strings.LastIndex(r.Name, "/")+1:]); err != nil {
			return apierror.Internal(err, "create the CA of cluster %s", r.Name)
		}
	}
	return nil
}

// sharedRecord is a project's and region's shared CA, as kept.
type sharedRecord struct {
	CA  string `json:"ca"`
	Key string `json:"key"`
}

const sharedCAPrefix = "sharedca/"

// sharedCA is project's and region's shared CA, made the first time.
func (c *clusters) sharedCA(project, region string) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := project + "/" + region
	if s, ok := c.shared[k]; ok {
		return s.CA, s.Key, nil
	}
	if c.db != nil {
		if b, err := c.db.Get(sharedCAPrefix + k); err == nil && len(b) > 0 {
			var s sharedRecord
			if json.Unmarshal(b, &s) == nil && s.CA != "" {
				c.sharedSet(k, s)
				return s.CA, s.Key, nil
			}
		}
	}
	ca, key, err := newCA("shared-" + region)
	if err != nil {
		return "", "", apierror.Internal(err, "create the shared CA of %s", region)
	}
	s := sharedRecord{CA: ca, Key: key}
	if c.db != nil {
		b, _ := json.Marshal(s)
		if err := c.db.Put(sharedCAPrefix+k, b); err != nil {
			return "", "", apierror.Internal(err, "keep the shared CA of %s", region)
		}
	}
	c.sharedSet(k, s)
	return ca, key, nil
}

func (c *clusters) sharedSet(k string, s sharedRecord) {
	if c.shared == nil {
		c.shared = map[string]sharedRecord{}
	}
	c.shared[k] = s
}

var sharedCANameRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/sharedRegionalCertificateAuthority$`)

// GetSharedRegionalCertificateAuthority returns the shared CA of a project
// and region, made on first use as Google's exists in every region.
func (s *ClusterServer) GetSharedRegionalCertificateAuthority(ctx context.Context, req *clusterpb.GetSharedRegionalCertificateAuthorityRequest) (*clusterpb.SharedRegionalCertificateAuthority, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	m := sharedCANameRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/sharedRegionalCertificateAuthority, not %q", req.GetName())
	}
	ca, _, err := c.sharedCA(m[1], m[2])
	if err != nil {
		return nil, err
	}
	return &clusterpb.SharedRegionalCertificateAuthority{Name: req.GetName(),
		ServerCa: &clusterpb.SharedRegionalCertificateAuthority_ManagedServerCa{ManagedServerCa: &clusterpb.SharedRegionalCertificateAuthority_RegionalManagedCertificateAuthority{
			CaCerts: []*clusterpb.SharedRegionalCertificateAuthority_RegionalManagedCertificateAuthority_RegionalCertChain{{Certificates: []string{ca}}}}}}, nil
}

// renderServerCA sets out's server CA fields from r's.
func (r *clusterRecord) renderServerCA(out *clusterpb.Cluster) {
	if r.ServerCAMode != "" {
		out.ServerCaMode = clusterpb.ServerCaMode(clusterpb.ServerCaMode_value[r.ServerCAMode]).Enum()
	} else if r.TransitCA != "" {
		out.ServerCaMode = clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA.Enum()
	}
	if r.ServerCAPool != "" {
		out.ServerCaPool = proto.String(r.ServerCAPool)
	}
}
