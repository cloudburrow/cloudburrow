package main

import (
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

// consoleRequests adapts the admin recorder for the console's Request Log
// (#291). It copies named fields only, so nothing else a recorded event
// might carry can reach the console.
type consoleRequests struct {
	rec *admin.Recorder
	// unmeasured names the enabled services with no call observer, asked on
	// each request: it is the metrics registry's list, which is marked by the
	// same callEvents/requestEvents/storageEvents that feed rec. A hand-kept
	// switch here once labelled KMS, Scheduler and Logging unobservable while
	// their calls were in the log (#683).
	unmeasured func() []string
}

func newConsoleRequests(rec *admin.Recorder, reg *metrics.Registry) *consoleRequests {
	return &consoleRequests{rec: rec, unmeasured: reg.Unmeasured}
}

func toRequestEvent(e admin.Event) console.RequestEvent {
	d, _ := strconv.ParseInt(e.Detail["duration_ms"], 10, 64)
	return console.RequestEvent{
		Time: e.Time, Service: e.Service, Method: e.Target,
		Resource: e.Detail["resource"], Project: e.Detail["project"],
		Code: e.Detail["code"], DurationMS: d, Transport: e.Detail["transport"],
	}
}

func (c *consoleRequests) Requests() []console.RequestEvent {
	events := c.rec.EventsWhere(admin.Filter{Kind: requestKind}, 1000)
	out := make([]console.RequestEvent, 0, len(events))
	for _, e := range events {
		out = append(out, toRequestEvent(e))
	}
	return out
}

func (c *consoleRequests) WatchRequests(buf int) (<-chan console.RequestEvent, func()) {
	in, stop := c.rec.Subscribe(buf)
	out := make(chan console.RequestEvent, buf)
	go func() {
		defer close(out)
		for e := range in {
			if e.Kind != requestKind {
				continue
			}
			select {
			case out <- toRequestEvent(e):
			default:
			}
		}
	}()
	return out, stop
}

func (c *consoleRequests) Unobserved() []string { return c.unmeasured() }
