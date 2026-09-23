// Package grpc provides the gRPC server CloudBurrow's own services run on: one
// configured listener with the interceptors and message-size limit they share.
//
// Each service that serves gRPC constructs its own Server on its own port —
// Cloud Tasks and the Cloud Run adapter both do — because each is published at a
// distinct host endpoint that clients are pointed at separately. Services backed
// by an upstream emulator are not served through this package at all.
package grpc
