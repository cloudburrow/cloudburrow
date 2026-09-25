package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	rpccode "google.golang.org/genproto/googleapis/rpc/code"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// requestKind is the Event.Kind every observed call is recorded under.
const requestKind = "request"

// callEvents records each completed gRPC call against a service.
//
// This is what /admin/events always promised and never did: the recorder was
// bounded, served and empty, because nothing called it. Only the method, the
// resource the call addressed, the status and the duration are kept — the
// Call type carries no payload, so there is nothing else to leak.
func callEvents(rec *admin.Recorder, reg *metrics.Registry, service string) grpctransport.Observer {
	if rec == nil && reg == nil {
		return nil
	}
	return func(c grpctransport.Call) {
		// The canonical name — NOT_FOUND, not gRPC's Go spelling NotFound — so
		// an event reads the same as the error gcloud or a REST client shows.
		code := rpccode.Code(c.Code).String()
		// Counted with the same code the event records, so /metrics and
		// /admin/events can never disagree about a call.
		reg.Observe(service, c.Method, code, c.Duration)
		detail := map[string]string{
			"code":        code,
			"duration_ms": strconv.FormatInt(c.Duration.Milliseconds(), 10),
			"transport":   "grpc",
		}
		if c.Resource != "" {
			detail["resource"] = c.Resource
			if p := projectOf(c.Resource); p != "" {
				detail["project"] = p
			}
		}
		if c.Stream {
			detail["stream"] = "true"
		}
		rec.Record(service, requestKind, c.Method, detail)
	}
}

// requestEvents records each completed JSON request against a service.
func requestEvents(rec *admin.Recorder, reg *metrics.Registry, service string) func(rest.Request) {
	if rec == nil && reg == nil {
		return nil
	}
	return func(r rest.Request) {
		// The verb, not the path: a path names resources, and a label per
		// secret would grow without bound.
		reg.Observe(service, "JSON "+r.Method, strconv.Itoa(r.Status), r.Duration)
		detail := map[string]string{
			"code":        strconv.Itoa(r.Status),
			"duration_ms": strconv.FormatInt(r.Duration.Milliseconds(), 10),
			"transport":   "http",
		}
		if p := projectOf(strings.TrimPrefix(r.Path, "/v1/")); p != "" {
			detail["project"] = p
		}
		rec.Record(service, requestKind, fmt.Sprintf("%s %s", r.Method, r.Path), detail)
	}
}

// storageEvents records each request the builtin Cloud Storage server
// served (#513): its API method, bucket, object, status and duration. A
// storage Call carries no query string or body, so no upload_id, token,
// signature or secret can reach an event or a metric label; the object name
// goes to the event only, since a label per object would grow without
// bound.
func storageEvents(rec *admin.Recorder, reg *metrics.Registry) func(gcsbuiltin.Call) {
	if rec == nil && reg == nil {
		return nil
	}
	return func(c gcsbuiltin.Call) {
		code := strconv.Itoa(c.Status)
		reg.Observe("storage", c.Method, code, c.Duration)
		detail := map[string]string{
			"code":        code,
			"duration_ms": strconv.FormatInt(c.Duration.Milliseconds(), 10),
			"transport":   "http",
		}
		if c.Bucket != "" {
			detail["bucket"] = c.Bucket
		}
		if c.Object != "" {
			detail["object"] = c.Object
		}
		rec.Record("storage", requestKind, c.Method, detail)
	}
}

// storageFaults applies /admin/faults rules with service=storage to the
// builtin server (#513), as HTTP statuses in storage's own error bodies.
type storageFaults struct{ f *admin.Faults }

func (s storageFaults) Fault(method, resource string) (int, time.Duration, bool) {
	return s.f.DecideHTTP("storage", method, resource)
}

// projectOf reads the project from a resource name, "projects/{p}/...".
func projectOf(resource string) string {
	rest, ok := strings.CutPrefix(resource, "projects/")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, "/")
	return p
}
