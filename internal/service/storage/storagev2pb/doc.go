// Package storagev2pb is google.storage.v2, copied from the Go storage
// client's generated code (cloud.google.com/go/storage v1.69.0,
// internal/apiv2/storagepb, Apache-2.0), for the gRPC API the builtin Cloud
// Storage server serves (#1321). The client keeps it internal, so it cannot
// be imported.
//
// tools/genstoragev2pb rewrites its file descriptor to register as
// cloudburrow.storage.v2 (cloudburrow/storage/v2/storage.proto): the
// cloudburrow CLI links the client too, and two registrations of one proto
// name panic at init. The wire format carries no package name, and the gRPC
// service and method names are still google.storage.v2.Storage, so clients
// cannot tell. To refresh it, copy storage.pb.go and storage_grpc.pb.go from
// the client, rename their package storagev2pb, and run
// `go run ./tools/genstoragev2pb`.
package storagev2pb
