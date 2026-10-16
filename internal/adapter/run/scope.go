package run

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Per-project Kubernetes names (#1399).
//
// On Google a job, worker pool or instance ID is unique within its project
// and region, so projects/a/locations/l/workerPools/p and
// projects/b/locations/l/workerPools/p are two pools. Every object behind
// one lives in the adapter's one namespace, so its Kubernetes name carries
// the scope: a short hash of projects/{p}/locations/{l}, as
// run-worker-pool-<scope>-<id>. The scope is also a label,
// cloudburrow.dev/run-scope, on the objects and in every selector, so two
// pools of one ID never select each other's pods or ReplicaSets, and the
// full resource name is the cloudburrow.dev/cloud-run-name annotation, which
// is what reads back.
//
// Objects an older version created have the unscoped name
// (run-worker-pool-<id>) and no scope label. They are still found, by their
// unscoped name and their cloud-run-name annotation, and are updated and
// deleted in place under the names they have; only a create uses the
// scoped name.

// Services are the exception: a Knative Service's name is its URL's first
// label and its revisions' prefix, so it stays the service ID (#1400). A
// service is read, changed and deleted only under the full name it was
// created with (its cloud-run-name annotation), and a second project's
// service of an ID in use is refused as unsupported, not given another's.

// labelScope is the scope a scoped object belongs to.
const labelScope = "cloudburrow.dev/run-scope"

// nameScope is the scope of a parent, projects/{p}/locations/{l}: eight hex
// characters, short enough that the names it is part of stay readable.
func nameScope(parent string) string {
	h := sha256.Sum256([]byte(parent))
	return hex.EncodeToString(h[:4])
}

// parentOf is the projects/{p}/locations/{l} a resource name is under.
func parentOf(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[:4], "/")
}

// dnsLabel bounds a name to the 63 characters a DNS label (a Service's name,
// a host label) allows: a longer one keeps its first 54 characters and ends
// in a hash of the whole, so two long names stay distinct.
func dnsLabel(base string) string {
	if len(base) <= 63 {
		return base
	}
	h := sha256.Sum256([]byte(base))
	return strings.TrimRight(base[:54], "-") + "-" + hex.EncodeToString(h[:4])
}
