package pubsubfront

// CMEK topics (#1874), to https://docs.cloud.google.com/pubsub/docs/encryption:
// a topic's kms_key_name names a Cloud KMS key,
// projects/PROJECT_ID/locations/LOCATION/keyRings/KEY_RING/cryptoKeys/CRYPTO_KEY,
// which the instance's Cloud KMS must serve, with purpose ENCRYPT_DECRYPT
// and its primary version enabled. A regional key must be in one of the
// regions of the topic's message storage policy, when it has one. While the
// key is not enabled, "Publishing and message delivery can fail with
// FAILED_PRECONDITION errors", and here they do: Publish to the topic and
// Pull and StreamingPull of its subscriptions are refused, and its export
// subscriptions deliver nothing.
//
// Messages at rest: the front keeps no message, except a Cloud Storage
// subscription's batch in memory until it is written; the emulator keeps
// them in its own memory, not on disk. So there is nothing at rest to
// encrypt.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var kmsKeyName = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+$`)

// keyCache is how long a key's state is trusted.
var keyCache = 2 * time.Second

type keyState struct {
	at      time.Time
	found   bool
	purpose string
	state   string
	err     error
}

// key looks a Cloud KMS key up, through a short cache.
func (e *exporter) key(ctx context.Context, name string) keyState {
	e.init()
	e.mu.Lock()
	k, ok := e.keys[name]
	e.mu.Unlock()
	if ok && time.Since(k.at) < keyCache {
		return k
	}
	k = keyState{at: time.Now()}
	if e.kms == "" {
		k.err = apierror.FailedPrecondition("Cloud KMS is not served by this instance, so the key %s cannot be used.", name)
		return k
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.kms+"/v1/"+name, nil)
	if err != nil {
		k.err = err
		return k
	}
	resp, err := e.client.Do(req)
	if err != nil {
		k.err = apierror.Internal(err, "Cannot reach Cloud KMS.")
		return k
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
	case resp.StatusCode/100 != 2:
		k.err = apierror.Internal(nil, "Cloud KMS answered %s for %s.", resp.Status, name)
		return k
	default:
		var ck struct {
			Purpose string `json:"purpose"`
			Primary struct {
				State string `json:"state"`
			} `json:"primary"`
		}
		_ = json.Unmarshal(body, &ck)
		k.found, k.purpose, k.state = true, ck.Purpose, ck.Primary.State
	}
	e.mu.Lock()
	e.keys[name] = k
	e.mu.Unlock()
	return k
}

func errKeyNotEnabled(name string) error {
	return apierror.FailedPrecondition("Cloud KMS key %s is not enabled or does not have an enabled primary version.", name)
}

// checkTopicKey is CreateTopic's and UpdateTopic's check of kms_key_name.
func (f *Front) checkTopicKey(ctx context.Context, t *pubsubpb.Topic) error {
	name := t.GetKmsKeyName()
	if name == "" {
		return nil
	}
	m := kmsKeyName.FindStringSubmatch(name)
	if m == nil {
		return apierror.InvalidArgument("Invalid kms_key_name: %s. It must be of the form "+
			"projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}.", name)
	}
	if loc := m[1]; loc != "global" {
		if regions := t.GetMessageStoragePolicy().GetAllowedPersistenceRegions(); len(regions) > 0 {
			in := false
			for _, r := range regions {
				if r == loc {
					in = true
				}
			}
			if !in {
				return apierror.InvalidArgument("The Cloud KMS key %s is in %s, which is not one of the topic's allowed persistence regions (%s).",
					name, loc, strings.Join(regions, ", "))
			}
		}
	}
	k := f.exp.key(ctx, name)
	if k.err != nil {
		return k.err
	}
	if !k.found {
		return apierror.NotFound("Cloud KMS key not found: %s.", name)
	}
	if k.purpose != "" && k.purpose != "ENCRYPT_DECRYPT" {
		return apierror.InvalidArgument("The Cloud KMS key %s has purpose %s; a topic needs a key with purpose ENCRYPT_DECRYPT.", name, k.purpose)
	}
	if k.state != "ENABLED" {
		return errKeyNotEnabled(name)
	}
	return nil
}

// topicKey is the kms_key_name of a topic the emulator holds.
func (f *Front) topicKey(topic string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.defs.topics[topic].GetKmsKeyName()
}

// subscriptionKey is the kms_key_name of a subscription's topic.
func (f *Front) subscriptionKey(sub string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.defs.topics[f.defs.subs[sub].GetTopic()].GetKmsKeyName()
}

// checkKeyEnabled refuses a call on a CMEK topic whose key is not enabled.
func (f *Front) checkKeyEnabled(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	k := f.exp.key(ctx, key)
	if k.err != nil || !k.found || k.state != "ENABLED" {
		return errKeyNotEnabled(key)
	}
	return nil
}
