package iameval

import (
	"regexp"
	"strings"
)

// The thirteenth round's checks (#1879): Logging's saved queries and log
// scopes, Memorystore's Operations and Locations mixins, Eventarc's
// operations on the Cloud Run port, Kubernetes Engine's cluster
// credentials, Compute Engine's internal addresses, natIP and patches,
// and deny policies' Cloud Identity customer principals.

const (
	logBase   = `^/v2/((?:projects|folders|organizations|billingAccounts)/[^/]+/locations/[^/:]+`
	logParent = logBase + `)`
	logQuery  = logBase + `/savedQueries/[^/:]+)$`
	logScope  = logBase + `/logScopes/[^/:]+)$`
)

// LoggingExtraJSON are Logging's saved queries and log scopes, served over
// JSON alone. Their REST pages
// (https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.locations.savedQueries
// and .logScopes, read 2026-11-20) name logging.queries.create, .list,
// .get and .delete and logging.logScopes.create, .list, .get and .delete,
// on the parent for a create or list and on the resource otherwise. The
// patch pages name none: each is checked with the permission of its name
// in Google's permission index, logging.queries.update and
// logging.logScopes.update (Logging Admin's),
// UNVERIFIED (re-read 2026-11-20: the patch pages above).
var LoggingExtraJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(logParent + `/savedQueries$`), Permission: "logging.queries.list"},
	{Method: "POST", Pattern: regexp.MustCompile(logParent + `/savedQueries$`), Permission: "logging.queries.create"},
	{Method: "GET", Pattern: regexp.MustCompile(logQuery), Permission: "logging.queries.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(logQuery), Permission: "logging.queries.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(logQuery), Permission: "logging.queries.delete"},
	{Method: "GET", Pattern: regexp.MustCompile(logParent + `/logScopes$`), Permission: "logging.logScopes.list"},
	{Method: "POST", Pattern: regexp.MustCompile(logParent + `/logScopes$`), Permission: "logging.logScopes.create"},
	{Method: "GET", Pattern: regexp.MustCompile(logScope), Permission: "logging.logScopes.get"},
	{Method: "PATCH", Pattern: regexp.MustCompile(logScope), Permission: "logging.logScopes.update"},
	{Method: "DELETE", Pattern: regexp.MustCompile(logScope), Permission: "logging.logScopes.delete"},
}

// MemorystoreOperation is the table, and the permissions to get, list,
// delete and cancel operations, of an operation Memorystore's one port
// serves, by its metadata's type URL (or valkey, a call under Valkey's
// JSON prefix): Memorystore for Redis' and Redis Cluster's are
// redis.operations.get and .list ("Memorystore for Redis access control"
// maps them to operations.get and .list,
// https://docs.cloud.google.com/memorystore/docs/redis/access-control, read
// 2026-11-20; it names redis.operations.delete and .cancel without a
// method, so they guard delete and cancel, UNVERIFIED, re-read 2026-11-20); Memorystore for
// Valkey's are memorystore.operations.get, .list, .delete and .cancel, as
// its REST pages name them
// (https://docs.cloud.google.com/memorystore/docs/valkey/reference/rest/v1/projects.locations.operations).
// svc is nil for an operation of none of them.
func MemorystoreOperation(typeURL string, valkey bool) (svc *Service, get, list, del, cancel string) {
	name := typeURL[strings.LastIndex(typeURL, "/")+1:]
	switch {
	case valkey || strings.HasPrefix(name, "google.cloud.memorystore."):
		return Valkey, "memorystore.operations.get", "memorystore.operations.list", "memorystore.operations.delete", "memorystore.operations.cancel"
	case strings.HasPrefix(name, "google.cloud.redis.cluster."):
		return RedisCluster, "redis.operations.get", "redis.operations.list", "redis.operations.delete", "redis.operations.cancel"
	case strings.HasPrefix(name, "google.cloud.redis."):
		return Redis, "redis.operations.get", "redis.operations.list", "redis.operations.delete", "redis.operations.cancel"
	}
	return nil, "", "", "", ""
}

// MemorystoreListOperations and MemorystoreLocations are the permissions,
// any one of which lets a caller list operations, or list or get
// locations, on Memorystore's port over gRPC, where the three APIs' one
// Locations and Operations services cannot tell which API is called
// (each listed operation is then kept only if the caller holds its own
// API's list permission). Over JSON, Valkey's prefix names its own:
// memorystore.locations.list and .get (its REST pages); Redis' are
// redis.locations.list and .get (the access-control page above).
var (
	MemorystoreListOperations = []string{"redis.operations.list", "memorystore.operations.list"}
	MemorystoreListLocations  = []string{"redis.locations.list", "memorystore.locations.list"}
	MemorystoreGetLocation    = []string{"redis.locations.get", "memorystore.locations.get"}
)

// GKEGetCredentials is the permission "GKE API permissions"
// (https://docs.cloud.google.com/kubernetes-engine/docs/reference/api-permissions,
// read 2026-11-20) names for projects.locations.clusters.get and .list
// "to see the cluster's credentials": without it a cluster's masterAuth
// comes back without its password and client key and certificate (the
// cluster CA certificate, which `gcloud container clusters
// get-credentials` needs with container.clusters.get alone, stays).
const GKEGetCredentials = "container.clusters.getCredentials"

// ComputeInternalAddress is addresses.insert's permission for an INTERNAL
// address: the page lists compute.addresses.create,
// compute.addresses.createInternal and compute.instances.update
// (https://docs.cloud.google.com/compute/docs/reference/rest/v1/addresses/insert,
// read 2026-11-20); "Reserve a static internal IP address"
// (https://docs.cloud.google.com/vpc/docs/reserve-static-internal-ip-address)
// needs compute.addresses.createInternal, which Compute Network User
// holds without compute.addresses.create. A global address's is
// compute.globalAddresses.createInternal.
func ComputeInternalAddress(global bool) string {
	if global {
		return "compute.globalAddresses.createInternal"
	}
	return "compute.addresses.createInternal"
}

// DomainCustomers are the Cloud Identity customers, by their primary
// domain, whose users a deny policy's
// principalSet://goog/cloudIdentityCustomerId/CUSTOMER_ID names ("All
// principals in a Cloud Identity account (domain)", "Principal
// identifiers", https://docs.cloud.google.com/iam/docs/principal-identifiers,
// read 2026-11-20). Set by the instance: its one organization's.
var DomainCustomers = map[string]string{}
