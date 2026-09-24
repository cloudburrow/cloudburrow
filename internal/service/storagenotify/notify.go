// Package storagenotify implements Cloud Storage Pub/Sub notifications.
//
// The event *detection* is not implemented here. fake-gcs-server already
// publishes object mutations to Pub/Sub with the official message shape —
// `eventType`, `bucketId`, `objectId`, `objectGeneration`, `payloadFormat`
// and the Storage Object JSON payload — so reimplementing it would be work
// spent producing something less faithful than what upstream already emits.
// The upstream audit's premise that it has no notification dispatcher does
// not hold for 1.56.1; see docs/upstream-evaluation.md.
//
// What is missing upstream, and is implemented here, is *routing*.
// fake-gcs-server takes one topic for the whole server, while the Cloud
// Storage API lets each bucket register several notificationConfigs, each
// with its own topic, event-type filter, prefix and custom attributes. So the
// backend publishes everything to one internal topic and this package fans it
// out to the topics callers actually registered.
package storagenotify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// EventType is one object mutation, as Cloud Storage names them.
type EventType string

const (
	EventFinalize       EventType = "OBJECT_FINALIZE"
	EventDelete         EventType = "OBJECT_DELETE"
	EventMetadataUpdate EventType = "OBJECT_METADATA_UPDATE"
	// EventArchive is part of the API and is **not** emitted: the backend
	// does not support object versioning, so nothing can be archived. It is
	// accepted in a configuration and never fires, which is reported rather
	// than hidden.
	EventArchive EventType = "OBJECT_ARCHIVE"
)

// KnownEventTypes lists every type a configuration may name.
func KnownEventTypes() []EventType {
	return []EventType{EventFinalize, EventDelete, EventMetadataUpdate, EventArchive}
}

// EmittedEventTypes lists the types that actually fire.
func EmittedEventTypes() []EventType {
	return []EventType{EventFinalize, EventDelete, EventMetadataUpdate}
}

// PayloadFormat is how the message body is rendered.
type PayloadFormat string

const (
	// PayloadJSONAPIV1 sends the Storage Object resource as JSON.
	PayloadJSONAPIV1 PayloadFormat = "JSON_API_V1"
	// PayloadNone sends attributes only, with an empty body.
	PayloadNone PayloadFormat = "NONE"
)

// Config is a bucket's notification configuration.
type Config struct {
	ID     string `json:"id"`
	Bucket string `json:"bucket"`
	// Topic is the full Pub/Sub topic name events are published to.
	Topic string `json:"topic"`
	// EventTypes filters which mutations fire. Empty means all of them, as
	// the API defines it.
	EventTypes []EventType `json:"event_types,omitempty"`
	// ObjectNamePrefix filters by object name.
	ObjectNamePrefix string `json:"object_name_prefix,omitempty"`
	// CustomAttributes are added to every message this configuration sends.
	CustomAttributes map[string]string `json:"custom_attributes,omitempty"`
	PayloadFormat    PayloadFormat     `json:"payload_format"`
	Etag             string            `json:"etag"`
	Created          time.Time         `json:"created"`
}

// SelfLink renders the API's self link for this configuration.
func (c Config) SelfLink() string {
	return fmt.Sprintf("https://www.googleapis.com/storage/v1/b/%s/notificationConfigs/%s", c.Bucket, c.ID)
}

// Matches reports whether an event should be delivered to this configuration.
func (c Config) Matches(eventType EventType, objectName string) bool {
	if c.ObjectNamePrefix != "" && !strings.HasPrefix(objectName, c.ObjectNamePrefix) {
		return false
	}
	// An empty event_types list means every type, which is what the API
	// documents. Treating it as "none" would make a configuration created
	// with defaults silently deliver nothing.
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

// ServicePrefix is the service-qualified form Cloud Storage uses for a
// notification's topic.
//
// The official Go client sends `//pubsub.googleapis.com/projects/{p}/topics/{t}`
// and expects it back. Accepting only the bare form rejected every
// notification the real SDK created — found by driving the SDK rather than
// by reading the reference, which documents the bare form.
const ServicePrefix = "//pubsub.googleapis.com/"

// NormaliseTopic accepts either spelling and returns the bare resource name.
//
// One canonical form is stored so that matching, publishing and comparison
// never have to care which spelling a caller used.
func NormaliseTopic(topic string) (string, error) {
	bare := strings.TrimPrefix(topic, ServicePrefix)
	parts := strings.Split(bare, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "topics" ||
		parts[1] == "" || parts[3] == "" {
		return "", apierror.InvalidArgument(
			"topic %q must be projects/{project}/topics/{topic}, optionally prefixed with %s",
			topic, ServicePrefix)
	}
	return bare, nil
}

// QualifiedTopic renders a topic the way Cloud Storage reports it.
func QualifiedTopic(bare string) string { return ServicePrefix + bare }

// ValidateTopic checks the target topic name.
//
// It is validated here rather than at publish time because a configuration
// with an unusable topic would otherwise be accepted and then drop every
// event it matched, with nothing to point at.
func ValidateTopic(topic string) error {
	_, err := NormaliseTopic(topic)
	return err
}

// ValidatePayloadFormat checks the requested payload format.
func ValidatePayloadFormat(f PayloadFormat) error {
	switch f {
	case PayloadJSONAPIV1, PayloadNone:
		return nil
	default:
		return apierror.InvalidArgument(
			"payload_format %q is not supported; use JSON_API_V1 or NONE", f)
	}
}

// ValidateEventTypes checks that every named type exists.
func ValidateEventTypes(types []EventType) error {
	for _, t := range types {
		known := false
		for _, k := range KnownEventTypes() {
			if t == k {
				known = true
				break
			}
		}
		if !known {
			return apierror.InvalidArgument("event type %q is not a Cloud Storage event type", t)
		}
	}
	return nil
}

// Store holds notification configurations.
type Store struct {
	mu  sync.Mutex
	db  store.Store
	now func() time.Time
	// nextID assigns the numeric IDs Cloud Storage uses. They are per bucket
	// and never reused, so a deleted configuration's ID cannot come back
	// attached to a different topic.
	nextID map[string]int
}

// NewStore returns a store backed by db.
// Backing is the store the configurations are kept in, for state
// snapshots, which capture it record for record (#290).
func (s *Store) Backing() store.Store { return s.db }

func NewStore(db store.Store) *Store {
	return &Store{db: db, now: time.Now, nextID: map[string]int{}}
}

// NewStoreWithClock returns a store with an injected clock.
func NewStoreWithClock(db store.Store, now func() time.Time) *Store {
	return &Store{db: db, now: now, nextID: map[string]int{}}
}

func configKey(bucket, id string) string { return "notify/" + bucket + "/" + id }

// Create registers a configuration and returns it.
func (s *Store) Create(bucket string, c Config) (Config, error) {
	if bucket == "" {
		return Config{}, apierror.InvalidArgument("bucket must not be empty")
	}
	bare, err := NormaliseTopic(c.Topic)
	if err != nil {
		return Config{}, err
	}
	c.Topic = bare
	if c.PayloadFormat == "" {
		c.PayloadFormat = PayloadJSONAPIV1
	}
	if err := ValidatePayloadFormat(c.PayloadFormat); err != nil {
		return Config{}, err
	}
	if err := ValidateEventTypes(c.EventTypes); err != nil {
		return Config{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A duplicate configuration is refused. Two identical registrations would
	// deliver every event twice, which looks like the backend emitting
	// duplicates rather than the caller registering twice.
	existing, listErr := s.list(bucket)
	if listErr != nil {
		return Config{}, listErr
	}
	for _, e := range existing {
		if e.Topic == c.Topic && e.ObjectNamePrefix == c.ObjectNamePrefix &&
			sameTypes(e.EventTypes, c.EventTypes) {
			return Config{}, apierror.AlreadyExists(
				"an identical notification configuration already exists on bucket %s (id %s)",
				bucket, e.ID)
		}
	}

	s.nextID[bucket] = s.nextID[bucket] + 1
	// IDs continue past anything already stored, so a restart cannot reissue
	// an ID that a caller still holds.
	for _, e := range existing {
		if n, convErr := strconv.Atoi(e.ID); convErr == nil && n >= s.nextID[bucket] {
			s.nextID[bucket] = n + 1
		}
	}

	c.ID = strconv.Itoa(s.nextID[bucket])
	c.Bucket = bucket
	c.Created = s.now().UTC()
	c.Etag = c.ID

	if err := s.put(configKey(bucket, c.ID), c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func sameTypes(a, b []EventType) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]EventType(nil), a...)
	bs := append([]EventType(nil), b...)
	sort.Slice(as, func(i, j int) bool { return as[i] < as[j] })
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// Get returns one configuration.
func (s *Store) Get(bucket, id string) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.db.Get(configKey(bucket, id))
	if err != nil {
		return Config{}, apierror.NotFound(
			"notification configuration %s not found on bucket %s", id, bucket)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, apierror.Internal(err, "decode notification configuration")
	}
	return c, nil
}

// List returns a bucket's configurations, ordered by ID.
func (s *Store) List(bucket string) ([]Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(bucket)
}

func (s *Store) list(bucket string) ([]Config, error) {
	keys, err := s.db.List("notify/" + bucket + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list notification configurations")
	}
	out := make([]Config, 0, len(keys))
	for _, k := range keys {
		raw, err := s.db.Get(k)
		if err != nil {
			continue
		}
		var c Config
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	// Numeric order, not lexical: "10" must come after "9".
	sort.Slice(out, func(i, j int) bool {
		a, aerr := strconv.Atoi(out[i].ID)
		b, berr := strconv.Atoi(out[j].ID)
		if aerr == nil && berr == nil {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete removes a configuration.
func (s *Store) Delete(bucket, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := configKey(bucket, id)
	if _, err := s.db.Get(key); err != nil {
		return apierror.NotFound(
			"notification configuration %s not found on bucket %s", id, bucket)
	}
	if err := s.db.Delete(key); err != nil {
		return apierror.Internal(err, "delete notification configuration")
	}
	return nil
}

// Matching returns the configurations an event should be delivered to.
func (s *Store) Matching(bucket string, eventType EventType, objectName string) ([]Config, error) {
	all, err := s.List(bucket)
	if err != nil {
		return nil, err
	}
	var out []Config
	for _, c := range all {
		if c.Matches(eventType, objectName) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Reset removes every configuration.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys, err := s.db.List("notify/")
	if err != nil {
		return apierror.Internal(err, "list notification configurations")
	}
	for _, k := range keys {
		if err := s.db.Delete(k); err != nil {
			return apierror.Internal(err, "delete %s", k)
		}
	}
	s.nextID = map[string]int{}
	return nil
}

func (s *Store) put(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return apierror.Internal(err, "encode %s", key)
	}
	if err := s.db.Put(key, raw); err != nil {
		return apierror.Internal(err, "store %s", key)
	}
	return nil
}
