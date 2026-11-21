package pubsubfront

// Changing a subscription's delivery type (#2051). Google lets an existing
// subscription change between pull, push, BigQuery and Cloud Storage: "In
// the subscription details page, click Edit. For Delivery type, select the
// new delivery type. Fill in the properties for the new subscription type
// ... Click Update", and with gcloud "Clear the configuration for the
// current delivery type and set the configuration for the new type"
// (--push-endpoint="", --clear-bigquery-config,
// --clear-cloud-storage-config, then --bigquery-table or --bucket;
// https://docs.cloud.google.com/pubsub/docs/manage-subscriptions, read
// 2026-11-20). Through the API that is one UpdateSubscription whose
// update_mask names push_config, bigquery_config and cloud_storage_config.
//
// The emulator refuses an update of bigquery_config or
// cloud_storage_config ("bigquery_config is not a known Subscription
// field"), so the front applies them, as it applies labels (#949): it
// checks the subscription the update makes as CreateSubscription is
// checked (checkUpdate: at most one of push_config, bigquery_config and
// cloud_storage_config; the table or bucket exists and fits; no
// exactly-once delivery on an export), takes the two paths out of the mask
// the emulator sees, and keeps the export configuration itself (exports).
// Every subscription it answers names the kept configuration in place of
// the emulator's, and the export deliveries (export.go) run on it: one
// switched to BigQuery or Cloud Storage starts writing its backlog there
// within exportPoll, and one switched away stops, its messages left to
// pull or push. The emulator's copy stays a pull subscription the front
// pulls from, as an export subscription it created is.

import (
	"context"
	"encoding/json"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// exportFields are the export configs the front applies on update.
var exportFields = []string{"bigquery_config", "cloud_storage_config"}

// masksExport reports whether an update mask names an export config the
// front applies.
func masksExport(paths []string) bool {
	for _, p := range exportFields {
		if masks(paths, p) {
			return true
		}
	}
	return false
}

// withoutExports is paths less the export configs the front applies.
func withoutExports(paths []string) []string {
	for _, p := range exportFields {
		paths = without(paths, p)
	}
	return paths
}

// keptExport is the export configuration an update makes of cur: cur's,
// with each export config the mask names replaced by u's, and Google's
// default max_duration on a new Cloud Storage one. Only BigqueryConfig and
// CloudStorageConfig are set.
func keptExport(cur, u *pubsubpb.Subscription, paths []string) *pubsubpb.Subscription {
	k := &pubsubpb.Subscription{BigqueryConfig: cur.GetBigqueryConfig(), CloudStorageConfig: cur.GetCloudStorageConfig()}
	if masks(paths, "bigquery_config") {
		k.BigqueryConfig = u.GetBigqueryConfig()
	}
	if masks(paths, "cloud_storage_config") {
		k.CloudStorageConfig = u.GetCloudStorageConfig()
	}
	k = proto.Clone(k).(*pubsubpb.Subscription)
	if k.BigqueryConfig != nil {
		k.BigqueryConfig.State = 0
	}
	if k.CloudStorageConfig != nil {
		k.CloudStorageConfig.State = 0
	}
	withStorageDefaults(k)
	return k
}

// exportUpdate is the export configuration an UpdateSubscription sets, and
// whether its mask names one; k is nil when the update leaves the
// configuration as it is (Terraform's provider names bigqueryConfig in
// every update, #928). checkUpdate has accepted the update.
func (f *Front) exportUpdate(ctx context.Context, r *pubsubpb.UpdateSubscriptionRequest) (k *pubsubpb.Subscription, masked bool) {
	paths := r.GetUpdateMask().GetPaths()
	if !masksExport(paths) {
		return nil, false
	}
	cur, err := f.subscription(ctx, r.GetSubscription().GetName())
	if err != nil {
		cur = &pubsubpb.Subscription{}
	}
	k = keptExport(cur, r.GetSubscription(), paths)
	if was := keptExport(cur, nil, nil); proto.Equal(k, was) {
		return nil, true
	}
	return k, true
}

// setExport keeps the export configuration an update set, and writes it to
// the state file before the update is answered.
func (f *Front) setExport(name string, k *pubsubpb.Subscription) {
	f.mu.Lock()
	f.exports[name] = proto.Clone(k).(*pubsubpb.Subscription)
	f.mu.Unlock()
	f.persist()
}

// withExport puts the export configuration an update set, if any, in place
// of the emulator's; it reports whether it changed anything.
func (f *Front) withExport(s *pubsubpb.Subscription) bool {
	if s == nil {
		return false
	}
	f.mu.Lock()
	k, ok := f.exports[s.GetName()]
	f.mu.Unlock()
	if !ok {
		return false
	}
	if proto.Equal(k.GetBigqueryConfig(), s.GetBigqueryConfig()) &&
		proto.Equal(k.GetCloudStorageConfig(), s.GetCloudStorageConfig()) {
		return false
	}
	k = proto.Clone(k).(*pubsubpb.Subscription)
	s.BigqueryConfig, s.CloudStorageConfig = k.BigqueryConfig, k.CloudStorageConfig
	return true
}

// withExportJSON is a Subscription's JSON with the export configuration the
// front keeps for it, and whether it changed.
func (f *Front) withExportJSON(sub json.RawMessage) (json.RawMessage, bool) {
	var o jsonObject
	if json.Unmarshal(sub, &o) != nil || o == nil {
		return sub, false
	}
	var name string
	if raw, key := o.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return sub, false
	}
	f.mu.Lock()
	k, ok := f.exports[name]
	f.mu.Unlock()
	if !ok {
		return sub, false
	}
	for _, key := range []string{"bigqueryConfig", "bigquery_config", "cloudStorageConfig", "cloud_storage_config"} {
		delete(o, key)
	}
	if k.GetBigqueryConfig() != nil {
		b, err := protojson.Marshal(k.GetBigqueryConfig())
		if err != nil {
			return sub, false
		}
		o["bigqueryConfig"] = b
	}
	if k.GetCloudStorageConfig() != nil {
		b, err := protojson.Marshal(k.GetCloudStorageConfig())
		if err != nil {
			return sub, false
		}
		o["cloudStorageConfig"] = b
	}
	out, err := encodeJSON(o)
	if err != nil {
		return sub, false
	}
	return out, true
}
