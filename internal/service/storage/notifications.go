package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/storagenotify"
)

// Pub/Sub notifications (#506), to docs.cloud.google.com/storage/docs/pubsub-notifications,
// served by the storage server itself rather than routed from
// fake-gcs-server's single topic. notificationConfigs are kept in the
// server's store, so they follow its persistence. Events are written to an
// outbox in the same transaction as the mutation that caused them, and a
// dispatcher publishes each after commit, retrying until the Pub/Sub
// emulator takes it: delivery is at least once, and an event is never lost
// to a crash between the mutation and the publish.
//
//   - OBJECT_FINALIZE: a new live version (upload, copy, rewrite, compose,
//     move's destination, restore), with overwroteGeneration when it
//     replaced one.
//   - OBJECT_METADATA_UPDATE: a patch or update of an object's metadata.
//   - OBJECT_DELETE: a version leaves the bucket (a delete, an overwrite on
//     an unversioned bucket, a lifecycle delete), with
//     overwrittenByGeneration when a new version replaced it.
//   - OBJECT_ARCHIVE: a live version became noncurrent, with
//     overwrittenByGeneration when a new version replaced it.
//
// A bucket has at most 100 configurations, and a configuration at most 10
// custom attributes.

const (
	notificationPrefix     = "notification/"
	outboxPrefix           = "outbox/"
	maxNotificationConfigs = 100
	maxCustomAttributes    = 10
	// maxPublishAttempts bounds retries of an event whose topic keeps
	// refusing it (a topic that was never created, say), so one bad
	// configuration cannot keep the dispatcher busy forever.
	maxPublishAttempts = 20
)

// Publisher delivers an event to a Pub/Sub topic, named in full
// (projects/{p}/topics/{t}).
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attributes map[string]string) error
}

type notificationConfig struct {
	ID         string            `json:"id"`
	Bucket     string            `json:"bucket"`
	Topic      string            `json:"topic"` // bare: projects/{p}/topics/{t}
	EventTypes []string          `json:"eventTypes,omitempty"`
	Prefix     string            `json:"prefix,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Format     string            `json:"format"`
}

func (c notificationConfig) matches(eventType, name string) bool {
	if c.Prefix != "" && !strings.HasPrefix(name, c.Prefix) {
		return false
	}
	if len(c.EventTypes) == 0 {
		return true
	}
	for _, t := range c.EventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

func notificationKey(bucket, id string) string { return notificationPrefix + bucket + "/" + id }

func notificationsOf(tx Tx, bucket string) ([]notificationConfig, error) {
	var out []notificationConfig
	for _, k := range tx.List(notificationPrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var c notificationConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID)
		b, _ := strconv.Atoi(out[j].ID)
		return a < b
	})
	return out, nil
}

func (s *Server) notificationJSON(r *http.Request, c notificationConfig) map[string]any {
	out := map[string]any{
		"kind": "storage#notification", "id": c.ID, "etag": c.ID,
		"topic": storagenotify.QualifiedTopic(c.Topic), "payload_format": c.Format,
		"selfLink": baseURL(r) + jsonPrefix + "b/" + escape(c.Bucket) + "/notificationConfigs/" + c.ID,
	}
	if len(c.EventTypes) > 0 {
		out["event_types"] = c.EventTypes
	}
	if c.Prefix != "" {
		out["object_name_prefix"] = c.Prefix
	}
	if len(c.Attributes) > 0 {
		out["custom_attributes"] = c.Attributes
	}
	return out
}

func (s *Server) notificationsInsert(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	if s.publisher == nil {
		writeError(w, errorf(http.StatusNotImplemented, "notImplemented",
			"notifications need a Pub/Sub emulator to deliver to; start the server with one (storage-server --pubsub-emulator)"))
		return
	}
	var in struct {
		Topic      string            `json:"topic"`
		EventTypes []string          `json:"event_types"`
		Prefix     string            `json:"object_name_prefix"`
		Attributes map[string]string `json:"custom_attributes"`
		Format     string            `json:"payload_format"`
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	for k := range body {
		switch k {
		case "topic", "event_types", "object_name_prefix", "custom_attributes", "payload_format", "kind", "id", "etag", "selfLink":
		default:
			writeError(w, badRequest("Invalid argument: %s is not a Notification field", k))
			return
		}
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError(w, badRequest("Invalid argument: %v", err))
		return
	}
	topic, err := storagenotify.NormaliseTopic(in.Topic)
	if err != nil {
		writeError(w, badRequest("Invalid argument: topic %q must be projects/{project}/topics/{topic}", in.Topic))
		return
	}
	if in.Format == "" {
		in.Format = string(storagenotify.PayloadJSONAPIV1)
	}
	if in.Format != string(storagenotify.PayloadJSONAPIV1) && in.Format != string(storagenotify.PayloadNone) {
		writeError(w, badRequest("Invalid argument: payload_format %q must be JSON_API_V1 or NONE", in.Format))
		return
	}
	for _, t := range in.EventTypes {
		if !knownEventType(t) {
			writeError(w, badRequest("Invalid argument: %q is not a Cloud Storage event type", t))
			return
		}
	}
	if len(in.Attributes) > maxCustomAttributes {
		writeError(w, badRequest("Invalid argument: a notification configuration has at most %d custom attributes, not %d", maxCustomAttributes, len(in.Attributes)))
		return
	}
	c := notificationConfig{Bucket: bucket, Topic: topic, EventTypes: in.EventTypes, Prefix: in.Prefix, Attributes: in.Attributes, Format: in.Format}
	err = s.meta.Update(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		existing, err := notificationsOf(tx, bucket)
		if err != nil {
			return err
		}
		if len(existing) >= maxNotificationConfigs {
			// UNVERIFIED: the docs state the limit of 100, not the code.
			return badRequest("The bucket %s already has %d notification configurations, the maximum.", bucket, maxNotificationConfigs)
		}
		// IDs are per bucket, increasing and never reused.
		next := 1
		if raw, ok := tx.Get(notificationPrefix + bucket + ".seq"); ok {
			next, _ = strconv.Atoi(string(raw))
			next++
		}
		tx.Put(notificationPrefix+bucket+".seq", []byte(strconv.Itoa(next)))
		c.ID = strconv.Itoa(next)
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		tx.Put(notificationKey(bucket, c.ID), raw)
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.notificationJSON(r, c))
}

func knownEventType(t string) bool {
	for _, k := range storagenotify.KnownEventTypes() {
		if string(k) == t {
			return true
		}
	}
	return false
}

func (s *Server) notificationsGet(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	var c notificationConfig
	err := s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		raw, ok := tx.Get(notificationKey(bucket, id))
		if !ok {
			return notFound("Notification %s not found on bucket %s.", id, bucket)
		}
		return json.Unmarshal(raw, &c)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.notificationJSON(r, c))
}

func (s *Server) notificationsList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	var cs []notificationConfig
	err := s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		var err error
		cs, err = notificationsOf(tx, bucket)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#notifications"}
	if len(cs) > 0 {
		items := make([]any, 0, len(cs))
		for _, c := range cs {
			items = append(items, s.notificationJSON(r, c))
		}
		resp["items"] = items
	}
	writeResponse(w, r, http.StatusOK, resp)
}

func (s *Server) notificationsDelete(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	err := s.meta.Update(func(tx Tx) error {
		if _, ok := tx.Get(notificationKey(bucket, id)); !ok {
			return notFound("Notification %s not found on bucket %s.", id, bucket)
		}
		tx.Delete(notificationKey(bucket, id))
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteNotifications removes a deleted bucket's configurations.
func deleteNotifications(tx Tx, bucket string) {
	for _, k := range tx.List(notificationPrefix + bucket + "/") {
		tx.Delete(k)
	}
}

// objectEvent is one change to an object version.
type objectEvent struct {
	Type          storagenotify.EventType
	Object        objectRecord
	Overwrote     int64 // the live generation a finalize replaced
	OverwrittenBy int64 // the generation that replaced a deleted or archived one
	Time          time.Time
}

// outboxEntry is one event bound for one configuration's topic.
type outboxEntry struct {
	Topic      string            `json:"topic"`
	Attributes map[string]string `json:"attributes"`
	Object     *objectRecord     `json:"object,omitempty"` // nil for payload_format NONE
	Attempts   int               `json:"attempts,omitempty"`
}

// emit writes ev to the outbox for every configuration of its bucket that
// matches, in the caller's transaction.
func emit(tx Tx, ev objectEvent) error {
	cs, err := notificationsOf(tx, ev.Object.Bucket)
	if err != nil || len(cs) == 0 {
		return err
	}
	for _, c := range cs {
		if !c.matches(string(ev.Type), ev.Object.Name) {
			continue
		}
		attrs := map[string]string{}
		for k, v := range c.Attributes {
			attrs[k] = v
		}
		// The standard attributes are set last, so a custom attribute can
		// never make a message lie about what happened.
		for k, v := range map[string]string{
			"notificationConfig": fmt.Sprintf("projects/_/buckets/%s/notificationConfigs/%s", c.Bucket, c.ID),
			"eventType":          string(ev.Type),
			"payloadFormat":      c.Format,
			"bucketId":           ev.Object.Bucket,
			"objectId":           ev.Object.Name,
			"objectGeneration":   strconv.FormatInt(ev.Object.Generation, 10),
			"eventTime":          ev.Time.UTC().Format(time.RFC3339Nano),
		} {
			attrs[k] = v
		}
		if ev.Overwrote != 0 {
			attrs["overwroteGeneration"] = strconv.FormatInt(ev.Overwrote, 10)
		}
		if ev.OverwrittenBy != 0 {
			attrs["overwrittenByGeneration"] = strconv.FormatInt(ev.OverwrittenBy, 10)
		}
		e := outboxEntry{Topic: c.Topic, Attributes: attrs}
		if c.Format == string(storagenotify.PayloadJSONAPIV1) {
			o := ev.Object
			e.Object = &o
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		tx.Put(outboxKey(ev.Time), raw)
	}
	return nil
}

// outboxSeq orders the events of one process in the order they were made,
// several of which can share an instant (an overwrite's delete and finalize).
var outboxSeq atomic.Uint64

// outboxKey orders entries by time, then by outboxSeq; the random suffix
// keeps keys from two processes on one store apart.
func outboxKey(t time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%020d-%012d-%s", outboxPrefix, t.UnixNano(), outboxSeq.Add(1), hex.EncodeToString(b))
}

// notifier publishes the outbox.
type notifier struct {
	wake chan struct{}
	mu   sync.Mutex
	// published and dropped count what the dispatcher did, for tests and
	// diagnostics.
	published, dropped int
}

func (s *Server) wakeNotifier() {
	if s.notify == nil {
		return
	}
	select {
	case s.notify.wake <- struct{}{}:
	default:
	}
}

// NotificationStats reports how many events were published and how many
// were given up on after maxPublishAttempts.
func (s *Server) NotificationStats() (published, dropped int) {
	if s.notify == nil {
		return 0, 0
	}
	s.notify.mu.Lock()
	defer s.notify.mu.Unlock()
	return s.notify.published, s.notify.dropped
}

// DispatchNotifications publishes every outbox entry once, deleting each
// that was published, and returns how many remain.
func (s *Server) DispatchNotifications(ctx context.Context) (int, error) {
	if s.publisher == nil {
		return 0, nil
	}
	var keys []string
	_ = s.meta.View(func(tx Tx) error { keys = tx.List(outboxPrefix); return nil })
	remaining := 0
	for _, k := range keys {
		var e outboxEntry
		err := s.meta.View(func(tx Tx) error {
			raw, ok := tx.Get(k)
			if !ok {
				return errGone
			}
			return json.Unmarshal(raw, &e)
		})
		if err != nil {
			continue
		}
		var data []byte
		if e.Object != nil {
			data, _ = json.Marshal(s.objectJSONWith(nil, *e.Object, 0))
		}
		perr := s.publisher.Publish(ctx, e.Topic, data, e.Attributes)
		_ = s.meta.Update(func(tx Tx) error {
			if perr == nil {
				tx.Delete(k)
				return nil
			}
			e.Attempts++
			if e.Attempts >= maxPublishAttempts {
				tx.Delete(k)
				return nil
			}
			raw, _ := json.Marshal(e)
			tx.Put(k, raw)
			return nil
		})
		s.notify.mu.Lock()
		switch {
		case perr == nil:
			s.notify.published++
		case e.Attempts >= maxPublishAttempts:
			s.notify.dropped++
		default:
			remaining++
		}
		s.notify.mu.Unlock()
	}
	return remaining, nil
}

var errGone = fmt.Errorf("gone")

// runNotifier dispatches when woken, and every second while entries are
// left over from a failed publish.
func (s *Server) runNotifier(ctx context.Context) {
	backoff := time.Second
	for {
		left, _ := s.DispatchNotifications(ctx)
		wait := time.Minute
		if left > 0 {
			wait = backoff
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notify.wake:
		case <-time.After(wait):
		}
	}
}
