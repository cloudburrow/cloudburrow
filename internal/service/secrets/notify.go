package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Event notifications (#1259): a secret's `topics` are Pub/Sub topics that
// Secret Manager publishes to when the secret or one of its versions
// changes, as docs.cloud.google.com/secret-manager/docs/event-notifications
// documents. Each message carries the attributes `eventType`, `dataFormat`
// (`JSON_API_V1`), `secretId` (the secret's full name) and `timestamp`;
// version events add `versionId` (the version's full name) and
// SECRET_DELETE adds `deleteType` (`REQUESTED` or `EXPIRATION`). The data
// is the JSON of the Secret or SecretVersion: after the change, or, for
// SECRET_DELETE, as it was before.
//
// Creating or updating a secret with topics first sends TOPIC_CONFIGURED,
// with no data and no other attribute, to every topic on it; a publish that
// fails (a topic that does not exist, say) fails the operation, as Google
// documents. A publish that fails after the change was made is logged, as
// Google logs it to the secret, and the change stands.
//
// SECRET_ROTATE and SECRET_VERSION_DESTROY_SCHEDULED are sent by the
// rotation schedule and the delayed destruction of schedule.go (#1278).

// Publisher publishes one message to a Pub/Sub topic, named in full. The
// instance supplies one that publishes to its local Pub/Sub; it is the same
// shape Cloud Storage notifications and Cloud Scheduler publish through.
type Publisher func(ctx context.Context, topic string, data []byte, attributes map[string]string) error

// MaxTopics is the documented limit: "Secrets can be configured with a list
// of up to 10 Pub/Sub topics."
const MaxTopics = 10

// The event types Google documents.
const (
	EventTopicConfigured       = "TOPIC_CONFIGURED"
	EventSecretCreate          = "SECRET_CREATE"
	EventSecretUpdate          = "SECRET_UPDATE"
	EventSecretDelete          = "SECRET_DELETE"
	EventSecretVersionAdd      = "SECRET_VERSION_ADD"
	EventSecretVersionEnable   = "SECRET_VERSION_ENABLE"
	EventSecretVersionDisable  = "SECRET_VERSION_DISABLE"
	EventSecretVersionDestroy  = "SECRET_VERSION_DESTROY"
	notificationDataFormat     = "JSON_API_V1"
	deleteRequested            = "REQUESTED"
	deleteExpiration           = "EXPIRATION"
	notificationPublishTimeout = 30 * time.Second
)

// SetPublisher sets where event notifications go. With none, a secret with
// topics is refused UNIMPLEMENTED, since nothing could be delivered.
func (s *Store) SetPublisher(p Publisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publish = p
}

// validateTopics checks a secret's topics before anything is stored.
func (s *Store) validateTopics(topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	if len(topics) > MaxTopics {
		return apierror.InvalidArgument("a secret takes at most %d topics; %d were given", MaxTopics, len(topics))
	}
	seen := map[string]bool{}
	for _, t := range topics {
		parts := strings.Split(t, "/")
		if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" || parts[2] != "topics" || parts[3] == "" {
			return apierror.InvalidArgument("topics[].name %q must be projects/{project}/topics/{topic}", t)
		}
		if seen[t] {
			return apierror.InvalidArgument("topic %s is listed twice", t)
		}
		seen[t] = true
	}
	if s.publish == nil {
		return apierror.Unimplemented("Secret.topics needs Pub/Sub to deliver to, and Pub/Sub is not enabled " +
			"on this instance; enable the pubsub service, or create the secret without topics")
	}
	return nil
}

// topicConfigured sends TOPIC_CONFIGURED to every topic, failing on the
// first publish that fails. The caller holds s.mu.
func (s *Store) topicConfigured(topics []string) error {
	for _, t := range topics {
		err := s.publishTo(t, nil, map[string]string{"eventType": EventTopicConfigured})
		if err != nil {
			if st, ok := status.FromError(err); ok && st.Code() != codes.OK && st.Code() != codes.Unknown {
				// Pub/Sub's own code, NOT_FOUND for a topic that does not
				// exist: Google documents that the operation fails "with an
				// error message indicating why the publish failed", not
				// the code (UNVERIFIED).
				// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/secret-manager/docs/event-notifications
				return &apierror.Error{Code: st.Code(),
					Message: fmt.Sprintf("publishing to topic %s failed: %s", t, st.Message())}
			}
			return apierror.FailedPrecondition("publishing to topic %s failed: %v", t, err)
		}
	}
	return nil
}

// notify publishes an event about sec to each of its topics. version is
// the version's JSON for a version event, nil otherwise. The caller holds
// s.mu, so the events of one secret arrive in the order they happened.
func (s *Store) notify(sec Secret, event string, version *Version, deleteType string) {
	if len(sec.Topics) == 0 || s.publish == nil {
		return
	}
	attrs := map[string]string{
		"eventType":  event,
		"dataFormat": notificationDataFormat,
		"secretId":   sec.Name,
		"timestamp":  s.now().Format(time.RFC3339Nano),
	}
	var data []byte
	var err error
	if version != nil {
		attrs["versionId"] = version.Name
		data, err = json.Marshal(renderVersion(*version))
	} else {
		data, err = json.Marshal(renderSecret(sec))
	}
	if err != nil {
		log.Printf("secretmanager: encode %s notification for %s: %v", event, sec.Name, err)
		return
	}
	if deleteType != "" {
		attrs["deleteType"] = deleteType
	}
	for _, t := range sec.Topics {
		if err := s.publishTo(t, data, attrs); err != nil {
			log.Printf("secretmanager: %s notification for %s to %s failed: %v", event, sec.Name, t, err)
		}
	}
}

// notifyVersion publishes a version event to the topics of v's secret.
// The caller holds s.mu.
func (s *Store) notifyVersion(project, id, event string, v Version) {
	sec, err := s.getSecret(project, id)
	if err != nil {
		return
	}
	s.notify(sec, event, &v, "")
}

func (s *Store) publishTo(topic string, data []byte, attrs map[string]string) error {
	if s.publish == nil {
		return fmt.Errorf("Pub/Sub is not enabled on this instance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), notificationPublishTimeout)
	defer cancel()
	return s.publish(ctx, topic, data, attrs)
}
