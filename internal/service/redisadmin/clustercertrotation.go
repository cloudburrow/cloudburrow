package redisadmin

import (
	"context"
	"fmt"
	"log"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Server certificate rotation of a Redis cluster and a Valkey instance
// (#1918), as Google documents it
// (https://docs.cloud.google.com/memorystore/docs/cluster/about-in-transit-encryption,
// /cluster/use-customer-managed-ca, /valkey/about-in-transit-encryption,
// re-read 2026-11-19):
//
//   - "Every week, Memorystore for Redis Cluster conducts server-side
//     certificate rotation for clusters that use the per-instance CA,
//     shared CA, and customer-managed CA modes" (and Memorystore for
//     Valkey for its instances): each node, while it runs, gets a new
//     server certificate of the same CA (for a customer-managed CA, one
//     the pool issues, listed among its certificates), a week after the
//     last rotation or the cluster's creation, by the Memorystore clock
//     (clock:advance, #1909), so advancing it a week rotates them;
//   - "New server certificates apply to new connections only, and
//     existing connections remain alive during this rotation": the node
//     is not restarted; it loads the new certificate (CONFIG SET
//     tls-cert-file), which new TLS handshakes present, while open
//     connections stay. The CA does not change, so a client keeps
//     connecting with the CA it downloaded;
//   - "for the customer-managed CA mode, you can rotate the certificates
//     on-demand": UpdateCluster with update_mask rotate_server_certificate
//     and rotate_server_certificate true (gcloud redis clusters update
//     --rotate-server-certificate), "for all machines in the cluster". Any
//     other mode is refused: "Server certificate rotation can only be
//     triggered for customer-managed CAs." Memorystore for Valkey's
//     UpdateInstance has the same field; its instances have no
//     customer-managed CA here, so it is refused the same way.
//
// Not documented, chosen here: the on-demand rotation is not taken in the
// same update as a change of shard_count or replica_count (the new nodes
// get new certificates anyway), and a weekly rotation that cannot issue a
// certificate (a CA pool deleted, or with no enabled CA) is logged and
// tried again on the next tick: the nodes keep the certificates they
// have, which Google warns "will expire".

// certRotateEvery is how often the nodes' server certificates are
// rotated.
const certRotateEvery = 7 * 24 * time.Hour

// errRotateNotCustomer is Google's refusal of an on-demand rotation.
var errRotateNotCustomer = apierror.FailedPrecondition("Server certificate rotation can only be triggered for customer-managed CAs.")

// ClusterCertRotator is a ClusterRuntime that gives a running node a new
// server certificate: StageNodeCert issues spec's node one, as when it
// started, and writes it where the node can load it, returning the files'
// paths in the node.
type ClusterCertRotator interface {
	StageNodeCert(ctx context.Context, spec Spec) (certFile, keyFile string, err error)
}

// certsIssued is when r's nodes' certificates were last rotated, or it
// was created.
func (r *clusterRecord) certsIssued() time.Time {
	if r.CertsIssued.After(r.Created) {
		return r.CertsIssued
	}
	return r.Created
}

// rotateCerts gives every node of snap a new server certificate, and
// keeps when, by the Memorystore clock.
func (c *clusters) rotateCerts(ctx context.Context, snap *clusterRecord) error {
	rot, ok := c.rt.(ClusterCertRotator)
	if !ok {
		return apierror.Unimplemented("this instance's nodes cannot load a new server certificate while they run")
	}
	for _, n := range snap.Nodes {
		if n.addr == "" {
			continue
		}
		cert, key, err := rot.StageNodeCert(ctx, snap.spec(n.Server))
		if err != nil {
			return err
		}
		if err := c.do(ctx, snap, n, "CONFIG", "SET", "tls-cert-file", cert, "tls-key-file", key); err != nil {
			return fmt.Errorf("load the new server certificate: %w", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.byName[snap.Name]; ok {
		r.CertsIssued = c.now()
		if err := c.save(r); err != nil {
			return err
		}
	}
	return nil
}

// rotateDue rotates the certificates of every ACTIVE cluster with
// in-transit encryption whose last rotation is a week old, each once at a
// time, beside its other operations.
func (c *clusters) rotateDue() {
	c.mu.Lock()
	now := c.now()
	var due []clusterRecord
	for name, r := range c.byName {
		if r.TransitCA == "" || r.state != clusterpb.Cluster_ACTIVE || r.suspended || len(r.Nodes) == 0 ||
			c.rotating[name] || now.Sub(r.certsIssued()) < certRotateEvery {
			continue
		}
		if _, pending := c.busy[name]; pending {
			continue
		}
		if c.rotating == nil {
			c.rotating = map[string]bool{}
		}
		c.rotating[name] = true
		snap := *r
		snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
		due = append(due, snap)
	}
	c.mu.Unlock()
	for i := range due {
		go func(snap *clusterRecord) {
			defer func() {
				c.mu.Lock()
				delete(c.rotating, snap.Name)
				c.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(c.bg, clusterWait)
			defer cancel()
			if err := c.rotateCerts(ctx, snap); err != nil {
				log.Printf("memorystore: %s: weekly server certificate rotation: %v", snap.Name, err)
			}
		}(&due[i])
	}
}
