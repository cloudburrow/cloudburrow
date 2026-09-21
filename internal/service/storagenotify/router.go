package storagenotify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Message is one event as it arrived from the storage backend.
type Message struct {
	Attributes map[string]string
	Data       []byte
}

// Bucket, ObjectName and EventType read the standard attributes.
func (m Message) Bucket() string     { return m.Attributes["bucketId"] }
func (m Message) ObjectName() string { return m.Attributes["objectId"] }
func (m Message) EventType() EventType {
	return EventType(m.Attributes["eventType"])
}

// Source delivers events from the internal topic.
//
// It is an interface so the router can be driven without Pub/Sub: the routing
// rules are the part worth testing exhaustively, and a real subscription
// would make that slow and flaky.
type Source interface {
	// Receive calls fn for each message until ctx is cancelled.
	Receive(ctx context.Context, fn func(Message)) error
}

// Publisher delivers a routed event to a caller's topic.
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attributes map[string]string) error
}

// Router fans events from the backend's single topic out to the topics
// registered notificationConfigs name.
//
// This exists because fake-gcs-server publishes every mutation to one topic,
// while the Cloud Storage API lets each bucket register several
// configurations with different topics and filters. Without it, a caller's
// notificationConfig would be recorded and never deliver anything.
type Router struct {
	configs *Store
	source  Source
	pub     Publisher
	onError func(error)

	mu       sync.Mutex
	routed   int
	dropped  int
	failures int
	done     chan struct{}
	cancel   context.CancelFunc
}

// NewRouter returns a router. onError may be nil.
func NewRouter(configs *Store, src Source, pub Publisher, onError func(error)) *Router {
	return &Router{configs: configs, source: src, pub: pub, onError: onError}
}

func (r *Router) Name() string { return "storage-notify" }

// Stats reports what the router has done, so a test or an operator can tell
// "no configuration matched" from "delivery failed".
func (r *Router) Stats() (routed, dropped, failures int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.routed, r.dropped, r.failures
}

// Start begins routing. It does not block.
func (r *Router) Start(ctx context.Context) error {
	if r.source == nil || r.pub == nil {
		return errors.New("storage notification router needs a source and a publisher")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})

	r.mu.Lock()
	r.cancel, r.done = cancel, done
	r.mu.Unlock()

	go func() {
		defer close(done)
		if err := r.source.Receive(runCtx, func(m Message) { r.route(runCtx, m) }); err != nil &&
			!errors.Is(err, context.Canceled) {
			r.report(fmt.Errorf("storage notification source: %w", err))
		}
	}()
	return nil
}

// Stop ends routing.
func (r *Router) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel = nil
	r.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

// route delivers one event to every matching configuration.
func (r *Router) route(ctx context.Context, m Message) {
	bucket := m.Bucket()
	if bucket == "" {
		// A message with no bucket cannot be routed. Counting it as dropped
		// rather than ignoring it keeps the statistics honest.
		r.count(0, 1, 0)
		r.report(fmt.Errorf("storage event has no bucketId attribute: %v", m.Attributes))
		return
	}

	matches, err := r.configs.Matching(bucket, m.EventType(), m.ObjectName())
	if err != nil {
		r.count(0, 1, 1)
		r.report(fmt.Errorf("match notification configurations for %s: %w", bucket, err))
		return
	}
	if len(matches) == 0 {
		// Perfectly normal: most buckets have no notification configured.
		r.count(0, 1, 0)
		return
	}

	for _, c := range matches {
		attrs := make(map[string]string, len(m.Attributes)+len(c.CustomAttributes)+1)
		for k, v := range m.Attributes {
			attrs[k] = v
		}
		// Custom attributes are applied last but must not overwrite the
		// standard ones: a configuration that set eventType would make the
		// message lie about what happened.
		for k, v := range c.CustomAttributes {
			if isStandardAttribute(k) {
				continue
			}
			attrs[k] = v
		}
		attrs["notificationConfig"] = fmt.Sprintf("projects/_/buckets/%s/notificationConfigs/%s",
			c.Bucket, c.ID)
		attrs["payloadFormat"] = string(c.PayloadFormat)

		data := m.Data
		if c.PayloadFormat == PayloadNone {
			// NONE means attributes only. Sending the body anyway would make
			// the format setting meaningless.
			data = nil
		}

		if err := r.pub.Publish(ctx, c.Topic, data, attrs); err != nil {
			r.count(0, 0, 1)
			r.report(fmt.Errorf("publish to %s for bucket %s: %w", c.Topic, bucket, err))
			continue
		}
		r.count(1, 0, 0)
	}
}

// standardAttributes are the attributes Cloud Storage itself sets.
var standardAttributes = map[string]bool{
	"eventType": true, "bucketId": true, "objectId": true,
	"objectGeneration": true, "payloadFormat": true, "eventTime": true,
	"notificationConfig": true, "overwroteGeneration": true,
	"overwrittenByGeneration": true,
}

func isStandardAttribute(k string) bool { return standardAttributes[k] }

func (r *Router) count(routed, dropped, failures int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routed += routed
	r.dropped += dropped
	r.failures += failures
}

func (r *Router) report(err error) {
	if r.onError != nil {
		r.onError(err)
	}
}

// WaitRouted blocks until at least n events have been routed, or the deadline
// passes. It exists for tests and for a caller that needs to know delivery has
// started.
func (r *Router) WaitRouted(n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if routed, _, _ := r.Stats(); routed >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// InternalTopicName renders the topic the storage backend publishes to.
func InternalTopicName(project, topic string) string {
	return fmt.Sprintf("projects/%s/topics/%s", project, topic)
}

// TopicID extracts the bare topic ID from a full topic name.
func TopicID(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// TopicProject extracts the project from a full topic name.
func TopicProject(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) == 4 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}
