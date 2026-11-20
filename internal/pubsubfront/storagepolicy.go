package pubsubfront

// Message storage policy updates (#2000). The emulator refuses an
// UpdateTopic whose mask names message_storage_policy ("message_storage_policy
// is not a known Topic field"), over gRPC and REST alike. Google lets a
// topic's policy be changed, `gcloud pubsub topics update
// --message-storage-policy-allowed-regions` and a PATCH of
// messageStoragePolicy (https://docs.cloud.google.com/pubsub/docs/resource-location-restriction,
// read 2026-11-20), so the front applies it, as it applies labels (#949,
// labels.go): the path is taken out of the mask the emulator sees, the
// policy is kept in the front once the rest of the update succeeds, and
// every topic read back through the front (Get, List and Update, over gRPC
// and REST) names it in place of the emulator's. The front's definitions
// (restore.go) carry it too, so a topic created again after an emulator
// restart, or by `cloudburrow state load` from a topic read back, has it;
// with a state file it survives a restart of the front alone. A create or
// delete of the topic drops it.
//
// Google's validation: the policy names "a list of allowed Google Cloud
// region names", and zones and multi-region locations are not supported;
// gcp.resourceLocations is checked as on CreateTopic (orgpolicy.go); and a
// CMEK topic's regional key must stay in one of the regions (cmek.go). The
// export subscriptions read the topic's policy each time they check their
// destination, so an IN_TRANSIT_LOCATION_RESTRICTION subscription whose
// destination the new policy allows is ACTIVE again within exportCheck,
// and one the new policy excludes stops.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// regionName is a Google Cloud region's form, "us-central1" or
// "northamerica-northeast1": not a zone ("us-central1-a") and not a
// multi-region ("us", "eu", "nam4").
var regionName = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)

// checkStoragePolicy refuses a message storage policy Google refuses.
func checkStoragePolicy(p *pubsubpb.MessageStoragePolicy) error {
	for _, r := range p.GetAllowedPersistenceRegions() {
		if !regionName.MatchString(r) {
			return apierror.InvalidArgument("Invalid message_storage_policy: %q is not a Google Cloud region. "+
				"allowed_persistence_regions must name regions; zones and multi-region locations are not supported.", r)
		}
	}
	return nil
}

// emptyPolicy reports whether p constrains nothing: a topic with it reads
// back with no message_storage_policy.
func emptyPolicy(p *pubsubpb.MessageStoragePolicy) bool {
	return len(p.GetAllowedPersistenceRegions()) == 0 && !p.GetEnforceInTransit()
}

// checkStoragePolicyUpdate checks the policy an UpdateTopic of
// message_storage_policy sets on name: its regions, and the topic's Cloud
// KMS key (the update's own when it changes kms_key_name too) against
// them.
func (f *Front) checkStoragePolicyUpdate(ctx context.Context, name string, t *pubsubpb.Topic, paths []string) error {
	p := t.GetMessageStoragePolicy()
	if err := checkStoragePolicy(p); err != nil {
		return err
	}
	key := t.GetKmsKeyName()
	if !masks(paths, "kms_key_name") {
		key = f.topicKey(name)
	}
	if key == "" {
		return nil
	}
	m := kmsKeyName.FindStringSubmatch(key)
	if m == nil || m[1] == "global" || len(p.GetAllowedPersistenceRegions()) == 0 {
		return nil
	}
	for _, r := range p.GetAllowedPersistenceRegions() {
		if r == m[1] {
			return nil
		}
	}
	return apierror.InvalidArgument("The Cloud KMS key %s is in %s, which is not one of the topic's allowed persistence regions (%s).",
		key, m[1], strings.Join(p.GetAllowedPersistenceRegions(), ", "))
}

// updatedStoragePolicy is the policy an update of message_storage_policy
// sets: never nil, so that none is kept as none.
func updatedStoragePolicy(p *pubsubpb.MessageStoragePolicy) *pubsubpb.MessageStoragePolicy {
	if p == nil {
		return &pubsubpb.MessageStoragePolicy{}
	}
	return proto.Clone(p).(*pubsubpb.MessageStoragePolicy)
}

// setStoragePolicy keeps the policy an update set on a topic, in the
// front's definitions too, and writes the state before the update is
// answered.
func (f *Front) setStoragePolicy(name string, p *pubsubpb.MessageStoragePolicy) {
	f.mu.Lock()
	f.storagePolicies[name] = updatedStoragePolicy(p)
	if t := f.defs.topics[name]; t != nil {
		t = proto.Clone(t).(*pubsubpb.Topic)
		f.withStoragePolicyLocked(t)
		f.defs.topics[name] = t
	}
	f.mu.Unlock()
	f.persist()
}

// withStoragePolicyLocked puts the kept policy, if any, in t; f.mu is held.
func (f *Front) withStoragePolicyLocked(t *pubsubpb.Topic) bool {
	p, ok := f.storagePolicies[t.GetName()]
	if !ok {
		return false
	}
	var want *pubsubpb.MessageStoragePolicy
	if !emptyPolicy(p) {
		want = proto.Clone(p).(*pubsubpb.MessageStoragePolicy)
	}
	if proto.Equal(t.GetMessageStoragePolicy(), want) || (want == nil && emptyPolicy(t.GetMessageStoragePolicy())) {
		return false
	}
	t.MessageStoragePolicy = want
	return true
}

// withTopicStoragePolicy puts the policy an update set, if any, in place of
// the emulator's; it reports whether it changed anything.
func (f *Front) withTopicStoragePolicy(t *pubsubpb.Topic) bool {
	if t == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.withStoragePolicyLocked(t)
}

// withStoragePolicyJSON is a Topic's JSON with the policy the front keeps
// for it in place of the emulator's, and whether it changed.
func (f *Front) withStoragePolicyJSON(res json.RawMessage) (json.RawMessage, bool) {
	var o jsonObject
	if json.Unmarshal(res, &o) != nil || o == nil {
		return res, false
	}
	var name string
	if raw, key := o.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return res, false
	}
	f.mu.Lock()
	p, ok := f.storagePolicies[name]
	if ok {
		p = proto.Clone(p).(*pubsubpb.MessageStoragePolicy)
	}
	f.mu.Unlock()
	if !ok {
		return res, false
	}
	delete(o, "message_storage_policy")
	delete(o, "messageStoragePolicy")
	if !emptyPolicy(p) {
		b, err := protojson.Marshal(p)
		if err != nil {
			return res, false
		}
		o["messageStoragePolicy"] = b
	}
	out, err := encodeJSON(o)
	if err != nil {
		return res, false
	}
	return out, true
}

// topicStoragePolicy is the message storage policy a topic has now, as it
// reads back: the kept one, or the emulator's.
func (f *Front) topicStoragePolicy(name string) *pubsubpb.MessageStoragePolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.storagePolicies[name]; ok {
		return proto.Clone(p).(*pubsubpb.MessageStoragePolicy)
	}
	if p := f.defs.topics[name].GetMessageStoragePolicy(); p != nil {
		return proto.Clone(p).(*pubsubpb.MessageStoragePolicy)
	}
	return nil
}
