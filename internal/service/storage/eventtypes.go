package storage

import (
	"errors"
	"strings"
)

// The notification vocabulary (#506): the event types a notificationConfig
// may name and the payload formats it may ask for, as Cloud Storage names
// them, and the two spellings of its topic.

// EventType is one object mutation, as Cloud Storage names it.
type EventType string

const (
	EventFinalize       EventType = "OBJECT_FINALIZE"
	EventDelete         EventType = "OBJECT_DELETE"
	EventMetadataUpdate EventType = "OBJECT_METADATA_UPDATE"
	// EventArchive fires when a live version becomes noncurrent (#498).
	EventArchive EventType = "OBJECT_ARCHIVE"
)

// KnownEventTypes lists every type a configuration may name.
func KnownEventTypes() []EventType {
	return []EventType{EventFinalize, EventDelete, EventMetadataUpdate, EventArchive}
}

// PayloadFormat is how the message body is rendered.
type PayloadFormat string

const (
	// PayloadJSONAPIV1 sends the Storage Object resource as JSON.
	PayloadJSONAPIV1 PayloadFormat = "JSON_API_V1"
	// PayloadNone sends attributes only, with an empty body.
	PayloadNone PayloadFormat = "NONE"
)

// topicServicePrefix is the service-qualified form Cloud Storage uses for a
// notification's topic. The official Go client sends
// //pubsub.googleapis.com/projects/{p}/topics/{t} and expects it back, while
// the reference documents the bare form, so both are accepted and the bare
// one is stored.
const topicServicePrefix = "//pubsub.googleapis.com/"

var errTopicForm = errors.New("a topic is projects/{project}/topics/{topic}")

// normaliseTopic accepts either spelling and returns the bare resource name.
func normaliseTopic(topic string) (string, error) {
	bare := strings.TrimPrefix(topic, topicServicePrefix)
	parts := strings.Split(bare, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "topics" || parts[1] == "" || parts[3] == "" {
		return "", errTopicForm
	}
	return bare, nil
}

// qualifiedTopic renders a topic the way Cloud Storage reports it.
func qualifiedTopic(bare string) string { return topicServicePrefix + bare }
