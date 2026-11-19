package orgpolicy

// TypeARRepository is Artifact Registry's custom constraint resource type.
// "Artifact Registry supports custom constraints, on all fields except
// labels, for create and update operations on the repository resource"
// ("Use custom organization policies", Artifact Registry,
// https://docs.cloud.google.com/artifact-registry/docs/custom-constraints,
// read 2026-11-19): no field list is registered, and the caller leaves
// labels out of the resource it hands over.
const TypeARRepository = "artifactregistry.googleapis.com/Repository"
