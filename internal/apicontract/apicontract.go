// Package apicontract pins the Google API contracts CloudBurrow implements
// against, and records where each one comes from.
//
// The contract is the published API definition, never another emulator's
// behaviour (ADR-0005). Two kinds of surface exist here:
//
//   - Services CloudBurrow *implements* (Cloud Tasks) need the generated
//     message and service types so our handlers speak the real wire format.
//   - Services CloudBurrow *adapts onto upstream* (Cloud Run v2 -> Knative)
//     need the same types so the adapter accepts what a real client sends.
//
// Services satisfied entirely by an upstream emulator (Pub/Sub, Cloud Storage)
// need no generated server surface: the upstream already implements them, and
// generating one would imply we serve it.
//
// Reuse applies to code generation too. Google publishes maintained, generated
// Go packages for these APIs, so CloudBurrow consumes them rather than running
// protoc itself. That removes an entire generation pipeline, and the pinning
// that would otherwise be ours is handled by go.mod and go.sum.
package apicontract

import (
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// GoogleapisCommit is the googleapis revision the contracts correspond to.
//
// googleapis publishes no release tags, so an explicit commit is pinned
// instead, as required by ADR-0005's rule against mutable references.
const GoogleapisCommit = "9f99764bb7841a50f0e46c41fd958a296b108e60"

// Surface describes one API contract CloudBurrow depends on.
type Surface struct {
	// Service is the proto package, e.g. "google.cloud.tasks.v2".
	Service string
	// Source is where the definition comes from.
	Source string
	// GoPackage is the generated Go package consumed.
	GoPackage string
	// Role says what CloudBurrow does with it.
	Role Role
	// RESTSurface records whether the HTTP surface is generated from
	// annotations or written by hand.
	RESTSurface string
}

// Role is what CloudBurrow does with a contract.
type Role string

const (
	// RoleImplement means CloudBurrow serves this API itself.
	RoleImplement Role = "implement"
	// RoleAdapt means CloudBurrow translates this API onto an upstream engine.
	RoleAdapt Role = "adapt"
	// RoleUpstream means an upstream component serves it and CloudBurrow only
	// integrates it.
	RoleUpstream Role = "upstream"
)

// Surfaces lists every contract, in a stable order.
func Surfaces() []Surface {
	return []Surface{
		{
			Service:     "google.cloud.tasks.v2",
			Source:      "googleapis/googleapis @ " + GoogleapisCommit,
			GoPackage:   "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb",
			Role:        RoleImplement,
			RESTSurface: "handwritten, from the proto's google.api.http annotations",
		},
		{
			Service:     "google.cloud.run.v2",
			Source:      "googleapis/googleapis @ " + GoogleapisCommit,
			GoPackage:   "cloud.google.com/go/run/apiv2/runpb",
			Role:        RoleAdapt,
			RESTSurface: "handwritten; requests are translated to Knative Serving resources",
		},
		{
			Service:     "google.pubsub.v1",
			Source:      "Google Pub/Sub emulator 0.8.35",
			GoPackage:   "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb (tests only)",
			Role:        RoleUpstream,
			RESTSurface: "not served by CloudBurrow",
		},
		{
			Service:     "Cloud Storage JSON API v1",
			Source:      "fake-gcs-server v1.56.1",
			GoPackage:   "cloud.google.com/go/storage (tests only)",
			Role:        RoleUpstream,
			RESTSurface: "not served by CloudBurrow",
		},
	}
}

// The compile-time references below are the drift check.
//
// If an upgrade removes or renames any of these, the build fails here rather
// than somewhere subtle at runtime. They are deliberately the exact types the
// Cloud Tasks implementation and the Cloud Run adapter are built on.
var (
	_ = (*taskspb.Queue)(nil)
	_ = (*taskspb.Task)(nil)
	_ = (*taskspb.HttpRequest)(nil)
	_ = (*taskspb.CreateQueueRequest)(nil)
	_ = (*taskspb.CreateTaskRequest)(nil)
	_ = (*taskspb.ListTasksRequest)(nil)
	_ = (*taskspb.RetryConfig)(nil)
	_ = (*taskspb.RateLimits)(nil)

	_ = (*runpb.Service)(nil)
	_ = (*runpb.Revision)(nil)
	_ = (*runpb.RevisionTemplate)(nil)
	_ = (*runpb.Container)(nil)
	_ = (*runpb.TrafficTarget)(nil)
	_ = (*runpb.CreateServiceRequest)(nil)
	_ = (*runpb.ListServicesRequest)(nil)
)
