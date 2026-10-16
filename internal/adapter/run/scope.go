package run

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
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

// A service's Knative Service is scoped too (#1400), as <id>-<scope>: its
// name is the first label of its URL (<id>-<scope>.<namespace>.<domain>)
// and the prefix of its revisions (<id>-<scope>-00001), so it is kept short
// enough that a revision's name, and the Kubernetes Services Knative names
// after it, fit in 63 characters. The Cloud Run API never shows it: a
// revision reads back as Google names one, <id>-<generation>-<suffix>
// (RevisionID), and is found again from that name (kRevisionName). A
// service an older version created is a Knative Service named by its ID; it
// keeps that name, its URL and its revisions' names, and is still found by
// them.

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

// maxKServiceName bounds a Knative Service's name so its revisions'
// (<name>-00001) fit in a 63-character DNS label, as do the Kubernetes
// Services Knative names after a revision.
const maxKServiceName = 57

// serviceKName is the Knative Service a new service, by full name, is
// created as: <id>-<scope>, or for a long ID its first characters and a
// hash of the full name, so two long IDs in one scope stay distinct.
func serviceKName(name string) string {
	id := name[strings.LastIndex(name, "/")+1:]
	base := id + "-" + nameScope(parentOf(name))
	if len(base) <= maxKServiceName {
		return base
	}
	h := sha256.Sum256([]byte(name))
	return strings.TrimRight(id[:maxKServiceName-9], "-") + "-" + hex.EncodeToString(h[:4])
}

// generationRE is the part of a Knative revision's name after its
// service's: the configuration generation Knative numbers it with.
var generationRE = regexp.MustCompile(`^[0-9]{5,}$`)

// revisionIDRE is the part of a Cloud Run revision ID after its service's:
// the generation and a three-character suffix.
var revisionIDRE = regexp.MustCompile(`^([0-9]{5,})-([a-z0-9]{3})$`)

// RevisionID is the Cloud Run revision ID of the Knative revision krev of
// the service named serviceName, whose Knative Service is kname. For a
// scoped service it is Google's form, <id>-<generation>-<suffix>, the
// suffix a hash of the Knative name, so it is the same on every read; for
// a service an older version created (kname is its ID) it is Knative's
// name, as it always read back.
func RevisionID(serviceName, kname, krev string) string {
	id := serviceName[strings.LastIndex(serviceName, "/")+1:]
	if kname == "" || kname == id {
		return krev
	}
	rest, ok := strings.CutPrefix(krev, kname+"-")
	if !ok {
		return krev
	}
	if generationRE.MatchString(rest) {
		return id + "-" + rest + "-" + revisionHash(krev)
	}
	return id + "-" + rest
}

// kRevisionName is RevisionID reversed: the Knative revision a Cloud Run
// revision ID of the service names. An ID that is not one of the
// service's is returned as it is, so it is not found.
func kRevisionName(serviceName, kname, rev string) string {
	id := serviceName[strings.LastIndex(serviceName, "/")+1:]
	if kname == "" || kname == id {
		return rev
	}
	rest, ok := strings.CutPrefix(rev, id+"-")
	if !ok {
		return rev
	}
	if m := revisionIDRE.FindStringSubmatch(rest); m != nil {
		if revisionHash(kname+"-"+m[1]) == m[2] {
			return kname + "-" + m[1]
		}
		return rev
	}
	if generationRE.MatchString(rest) {
		return rev // Knative's form of the ID, which is not the API's
	}
	return kname + "-" + rest
}

// revisionHash is a Knative revision's three-character suffix: lowercase
// letters and digits, as Cloud Run's random ones are.
func revisionHash(krev string) string {
	h := sha256.Sum256([]byte(krev))
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	n := uint32(h[0])<<16 | uint32(h[1])<<8 | uint32(h[2])
	out := make([]byte, 3)
	for i := range out {
		out[i] = alphabet[n%36]
		n /= 36
	}
	return string(out)
}
