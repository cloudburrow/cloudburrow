// Package rest provides the HTTP plumbing CloudBurrow's own REST surfaces share:
// a router, request-size limits, JSON decoding and encoding, and Google-style
// error bodies. It converts wire formats to and from service calls and holds no
// service behaviour of its own.
//
// It does not implement the Cloud Storage upload and download protocols. Cloud
// Storage is served by fake-gcs-server; see docs/upstream-evaluation.md.
package rest
