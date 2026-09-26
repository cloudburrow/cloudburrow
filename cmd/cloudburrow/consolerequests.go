package main

import (
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// consoleRequests adapts the admin recorder for the console's Request Log
// (#291). It copies named fields only, so nothing else a recorded event
// might carry can reach the console.
type consoleRequests struct {
	rec        *admin.Recorder
	unobserved []string
}

func newConsoleRequests(rec *admin.Recorder, cfg config.Config) *consoleRequests {
	c := &consoleRequests{rec: rec}
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceTasks, config.ServiceRun, config.ServiceSecrets:
			// Served by CloudBurrow itself, so every call is recorded.
		case config.ServiceStorage:
			// The storage server's calls are scraped into the recorder (#513).
		default:
			c.unobserved = append(c.unobserved, string(s))
		}
	}
	return c
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

func (c *consoleRequests) Unobserved() []string { return c.unobserved }
